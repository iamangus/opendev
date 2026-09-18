package jobmcp

import (
	"context"
	"fmt"

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
	for _, job := range config.Store.List() {
		if job.Status == pipeline.JobReadyToPublish && job.HolisticReviewVerdict == pipeline.ReviewApproved {
			if _, err := publishApprovedJob(ctx, job.ID, config); err != nil {
				return fmt.Errorf("resume approved publication for job %s: %w", job.ID, err)
			}
			continue
		}
		if job.Status != pipeline.JobAwaitingCI || job.CI == nil {
			continue
		}
		pr, err := config.GitHub.GetPR(ctx, job.Repository, job.PullRequestNumber)
		if err != nil {
			return fmt.Errorf("get CI pull request for job %s: %w", job.ID, err)
		}
		if pr.Head.SHA != job.CI.HeadSHA {
			// The next remediation or explicit re-review owns a new head. Never
			// apply results from an unrecorded SHA.
			continue
		}
		checks, err := config.GitHub.GetPRChecks(ctx, job.Repository, pr.Head.SHA)
		if err != nil {
			return fmt.Errorf("get CI checks for job %s: %w", job.ID, err)
		}
		state, results, fingerprint := ciobserver.Evaluate(job.CI.RequiredChecks, checks)
		updated, err := config.Controller.RecordCI(job.ID, pr.Head.SHA, state, results, fingerprint)
		if err != nil {
			return fmt.Errorf("record CI state for job %s: %w", job.ID, err)
		}
		switch state {
		case pipeline.CIPending:
			notify(config, updated, outbox.EventCIPending, "Required GitHub Actions checks are pending", "")
		case pipeline.CIPassed:
			notify(config, updated, outbox.EventCIPassed, "Required GitHub Actions checks passed", "")
			run, err := config.Dispatcher.StartHolistic(ctx, updated)
			if err != nil {
				return fmt.Errorf("start holistic review after CI for job %s: %w", job.ID, err)
			}
			if _, err := config.Controller.StartHolisticReviewer(updated.ID, run.RunID, updated.IntegrationSHA); err != nil {
				return fmt.Errorf("record holistic review after CI for job %s: %w", job.ID, err)
			}
		case pipeline.CIFailed:
			notify(config, updated, outbox.EventCIFailed, "Required GitHub Actions check failed", fingerprint)
			var task *pipeline.Task
			updated, task, err = config.Controller.StartCIRemediation(updated.ID, fingerprint, results)
			if err != nil {
				return fmt.Errorf("start CI remediation for job %s: %w", job.ID, err)
			}
			if task == nil {
				notify(config, updated, outbox.EventCIBlocked, "Repeated identical CI failure", fingerprint)
				continue
			}
			notify(config, updated, outbox.EventCIRemediating, "Started remediation for failed required CI", fingerprint)
			if _, err := startReadyWriters(ctx, updated, config); err != nil {
				return fmt.Errorf("start CI remediation writer for job %s: %w", job.ID, err)
			}
		}
	}
	return nil
}
