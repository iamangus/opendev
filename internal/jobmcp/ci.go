package jobmcp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/iamangus/code-mcp/internal/ciobserver"
	"github.com/iamangus/code-mcp/internal/outbox"
	"github.com/iamangus/code-mcp/internal/pipeline"
)

// ObservePendingCI advances draft-PR jobs from their durable GitHub check state.
// It is safe to call repeatedly and is used by startup reconciliation and polling.
func ObservePendingCI(ctx context.Context, config Config) error {
	if config.GitHub == nil {
		return nil
	}
	var failures []error
	for _, job := range config.Store.List() {
		if err := observeCIJob(ctx, job.ID, config); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func observeCIJob(ctx context.Context, jobID string, config Config) error {
	unlock := config.Store.LockJob(jobID)
	defer unlock()
	job, err := config.Store.Get(jobID)
	if err != nil {
		return err
	}
	if job.Status == pipeline.JobReadyToPublish && job.HolisticReviewVerdict == pipeline.ReviewApproved {
		if _, err := publishApprovedJob(ctx, job.ID, config); err != nil {
			return fmt.Errorf("resume approved publication for job %s: %w", job.ID, err)
		}
		return nil
	}
	if job.Status == pipeline.JobHolisticReviewing && job.CI != nil && job.CI.State == pipeline.CIPassed &&
		job.HolisticReviewRunID == "" && job.HolisticReviewVerdict == pipeline.ReviewPending {
		// A restart, a failed dispatch, or a pre-fix stale verdict stranded the
		// holistic round: CI has passed and the round must run again.
		run, err := config.Dispatcher.StartHolistic(ctx, job)
		if err != nil {
			return fmt.Errorf("resume holistic review for job %s: %w", job.ID, err)
		}
		if _, err := config.Controller.StartHolisticReviewer(job.ID, run.RunID, job.IntegrationSHA); err != nil {
			return fmt.Errorf("record resumed holistic review for job %s: %w", job.ID, err)
		}
		return nil
	}
	if job.Status != pipeline.JobAwaitingCI || job.CI == nil {
		return nil
	}
	pr, err := config.GitHub.GetPR(ctx, job.Repository, job.PullRequestNumber)
	if err != nil {
		return fmt.Errorf("get CI pull request for job %s: %w", job.ID, err)
	}
	if pr.Head.SHA != job.CI.HeadSHA {
		// The next remediation or explicit re-review owns a new head. Never
		// apply results from an unrecorded SHA.
		_, err := config.Controller.FailJob(job.ID, "draft PR head changed outside the recorded integration; create a new job or explicitly review the new revision")
		return err
	}
	checks, err := config.GitHub.GetPRChecks(ctx, job.Repository, pr.Head.SHA)
	if err != nil {
		return fmt.Errorf("get CI checks for job %s: %w", job.ID, err)
	}
	state, results, fingerprint := ciobserver.Evaluate(job.CI.RequiredChecks, checks)
	started := job.CI.StartedAt
	if started.IsZero() {
		started = job.CI.UpdatedAt
	}
	if state == pipeline.CIPending && time.Since(started) > 30*time.Minute {
		state = pipeline.CIBlocked
	}
	updated, err := config.Controller.RecordCI(job.ID, pr.Head.SHA, state, results, fingerprint)
	if err != nil {
		return fmt.Errorf("record CI state for job %s: %w", job.ID, err)
	}
	switch state {
	case pipeline.CIPending:
		notify(config, updated, outbox.EventCIPending, "Required GitHub Actions checks are pending", "")
	case pipeline.CIPassed:
		notify(config, updated, outbox.EventCIPassed, "Required GitHub Actions checks passed", "")
		// A new head invalidates the previous holistic round. Clear stale verdict
		// and run ownership so the fresh review can be dispatched and recorded.
		updated, err = config.Controller.ResetHolisticRound(updated.ID)
		if err != nil {
			return fmt.Errorf("reset holistic round after CI for job %s: %w", job.ID, err)
		}
		run, err := config.Dispatcher.StartHolistic(ctx, updated)
		if err != nil {
			return fmt.Errorf("start holistic review after CI for job %s: %w", job.ID, err)
		}
		if _, err := config.Controller.StartHolisticReviewer(updated.ID, run.RunID, updated.IntegrationSHA); err != nil {
			return fmt.Errorf("record holistic review after CI for job %s: %w", job.ID, err)
		}
	case pipeline.CIFailed:
		notify(config, updated, outbox.EventCIFailed, "Required GitHub Actions check failed", fingerprint)
		identical := 0
		for _, prior := range updated.CIFailureFingerprints {
			if prior == fingerprint {
				identical++
			}
		}
		if identical >= 2 {
			// The same failure survived a remediation Writer: escalate to the
			// Planner for a plan revision instead of another patch attempt.
			updated, err = config.Controller.BeginCIReplan(updated.ID, fingerprint)
			if err != nil {
				return fmt.Errorf("begin CI replan for job %s: %w", job.ID, err)
			}
			notify(config, updated, outbox.EventCIRemediating, "Repeated identical CI failure; escalating to planner revision", fingerprint)
			if updated.Status == pipeline.JobFailed {
				notify(config, updated, outbox.EventFailed, updated.Failure, fingerprint)
				return nil
			}
			if _, err := config.Dispatcher.StartPlannerRevision(ctx, updated); err != nil {
				return fmt.Errorf("start planner revision for job %s: %w", job.ID, err)
			}
			return nil
		}
		var task *pipeline.Task
		updated, task, err = config.Controller.StartCIRemediation(updated.ID, fingerprint, results)
		if err != nil {
			return fmt.Errorf("start CI remediation for job %s: %w", job.ID, err)
		}
		if task == nil {
			notify(config, updated, outbox.EventCIBlocked, "Repeated identical CI failure", fingerprint)
			return nil
		}
		notify(config, updated, outbox.EventCIRemediating, "Started remediation for failed required CI", fingerprint)
		if _, err := startReadyWriters(ctx, updated, config); err != nil {
			return fmt.Errorf("start CI remediation writer for job %s: %w", job.ID, err)
		}
	case pipeline.CIBlocked:
		notify(config, updated, outbox.EventFailed, "Required CI did not complete within its deadline", updated.Failure)
	}
	return nil
}
