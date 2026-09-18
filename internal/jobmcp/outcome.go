package jobmcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/iamangus/code-mcp/internal/dispatcher"
	"github.com/iamangus/code-mcp/internal/outbox"
	"github.com/iamangus/code-mcp/internal/pipeline"
)

// OutcomeHandler turns a durably observed AgentFoundry terminal response into
// deterministic pipeline transitions. Agent definitions, not this package,
// supply the StructuredOutput schema presented to the model.
func OutcomeHandler(config Config) dispatcher.OutcomeHandler {
	return func(ctx context.Context, run dispatcher.DispatchRun) error {
		if run.Status != "completed" {
			return recordTerminalFailure(run, config)
		}
		switch run.Role {
		case dispatcher.RolePlanner:
			return applyPlan(ctx, run, config)
		case dispatcher.RoleWriter:
			return applyWriter(ctx, run, config)
		case dispatcher.RoleReviewer:
			return applyReview(ctx, run, config)
		case dispatcher.RoleHolistic:
			return applyHolistic(ctx, run, config)
		default:
			return fmt.Errorf("unknown dispatch role %q", run.Role)
		}
	}
}

func applyPlan(ctx context.Context, run dispatcher.DispatchRun, config Config) error {
	var plan pipeline.Plan
	if err := decodeResponse(run.Response, &plan); err != nil {
		return fmt.Errorf("invalid planner response: %w", err)
	}
	job, err := config.Store.SubmitPlan(run.JobID, plan)
	if err == pipeline.ErrPlanExists {
		job, err = config.Store.Get(run.JobID)
	}
	if err != nil {
		return err
	}
	if len(job.Plan.ReferenceRepositories) > 0 {
		if config.References == nil {
			return fmt.Errorf("reference repository snapshots are not configured")
		}
		snapshots, err := config.References.ValidateAndSnapshot(job.ID, job.Plan.ReferenceRepositories)
		if err != nil {
			return err
		}
		persisted := make([]pipeline.ReferenceSnapshot, 0, len(snapshots))
		for _, snapshot := range snapshots {
			config.Registrar.RegisterReference(job.ID, snapshot.Repository, snapshot.Path)
			persisted = append(persisted, pipeline.ReferenceSnapshot{Repository: snapshot.Repository, SHA: snapshot.SHA, Path: snapshot.Path, Purpose: snapshot.Purpose})
		}
		job, err = config.Controller.RecordReferenceSnapshots(job.ID, persisted)
		if err != nil {
			return err
		}
	}
	if err := ensureIntegrationWorktree(job, config); err != nil {
		return err
	}
	_, err = startReadyWriters(ctx, job, config)
	return err
}

type writerResponse struct {
	Status             string                         `json:"status"`
	Summary            string                         `json:"summary"`
	ChangedFiles       []string                       `json:"changed_files"`
	ValidationEvidence *[]pipeline.ValidationEvidence `json:"validation_evidence"`
	RemainingRisks     []string                       `json:"remaining_risks"`
	Reason             string                         `json:"reason"`
}

func applyWriter(ctx context.Context, run dispatcher.DispatchRun, config Config) error {
	var response writerResponse
	if err := decodeResponse(run.Response, &response); err != nil {
		return fmt.Errorf("invalid writer response: %w", err)
	}
	if response.Status != "completed" && response.Status != "no_changes" && response.Status != "blocked" {
		return fmt.Errorf("invalid writer status %q", response.Status)
	}
	if response.ValidationEvidence == nil {
		return fmt.Errorf("writer response omitted validation_evidence")
	}
	job, err := config.Store.Get(run.JobID)
	if err != nil {
		return err
	}
	task := findTask(job, run.TaskKey)
	if task == nil || task.WriterRunID != run.RunID {
		return fmt.Errorf("writer run does not own task %q", run.TaskKey)
	}
	if response.Status == "blocked" {
		return applyWriterBlocked(ctx, run, config, job, task, response)
	}
	if task.Status == pipeline.TaskReviewing {
		if task.ReviewerRunID != "" {
			return nil
		}
		review, err := config.Dispatcher.StartReviewer(ctx, job, task)
		if err != nil {
			return err
		}
		_, err = config.Controller.StartReviewer(job.ID, task.Key, review.RunID)
		return err
	}
	if task.Status != pipeline.TaskWorking {
		// A previous application already advanced this writer's owned task.
		return nil
	}
	hasChanges, err := config.Worktrees.HasChanges(task.WorktreePath)
	if err != nil {
		return err
	}
	if response.Status == "no_changes" && hasChanges {
		return fmt.Errorf("writer reported no_changes but the task worktree is dirty")
	}
	if !hasChanges {
		if len(response.ChangedFiles) > 0 {
			return fmt.Errorf("writer reported changed_files but the task worktree is clean")
		}
		reason := strings.TrimSpace(response.Reason)
		if reason == "" {
			reason = strings.TrimSpace(response.Summary)
		}
		if reason == "" {
			return fmt.Errorf("writer no-change result omitted summary and reason")
		}
		job, err = config.Controller.RecordNoChanges(job.ID, task.Key, run.RunID, reason, *response.ValidationEvidence)
		if err != nil {
			return err
		}
		_, _, err = integrateEligible(job.ID, config, ctx)
		return err
	}
	if response.Status == "no_changes" {
		return fmt.Errorf("writer reported no_changes but the task worktree is dirty")
	}
	sha, err := config.Worktrees.Commit(task.WorktreePath)
	if err != nil {
		return err
	}
	job, err = config.Controller.RecordWriterCompletion(job.ID, task.Key, run.RunID, sha, *response.ValidationEvidence)
	if err != nil {
		return err
	}
	task = findTask(job, task.Key)
	review, err := config.Dispatcher.StartReviewer(ctx, job, task)
	if err != nil {
		return err
	}
	_, err = config.Controller.StartReviewer(job.ID, task.Key, review.RunID)
	return err
}

// applyWriterBlocked verifies a Writer's blocker claim against recorded
// workspace tool failures before the claim may block the task. A claim with no
// recorded tool failures in the run window is treated as fabricated: real work
// is committed and submitted to review, while an empty worktree fails honestly.
func applyWriterBlocked(ctx context.Context, run dispatcher.DispatchRun, config Config, job *pipeline.Job, task *pipeline.Task, response writerResponse) error {
	if task.Status != pipeline.TaskWorking {
		return nil
	}
	var failures []ToolFailureInfo
	if config.WorktreeToolFailures != nil && task.WorktreePath != "" {
		failures = config.WorktreeToolFailures(task.WorktreePath, run.StartedAt.Add(-time.Minute))
	}
	// A verified blocker needs a meaningful wall of failures, not one or two
	// recoverable misses the Writer should have retried past.
	if len(failures) >= 3 {
		if config.Logger != nil {
			config.Logger.Info("writer blocker verified by recorded tool failures", "job_id", run.JobID, "task", run.TaskKey, "failures", len(failures), "first", failures[0], "window_since", run.StartedAt.Add(-time.Minute))
		}
		blocked, err := config.Controller.BlockTask(run.JobID, run.TaskKey, response.Reason)
		if err != nil {
			return err
		}
		notify(config, blocked, outbox.EventBlocked+":"+run.TaskKey, response.Summary, response.Reason)
		return nil
	}
	if config.Logger != nil {
		config.Logger.Warn("writer blocker claim unverified; no tool failures recorded", "job_id", run.JobID, "task", run.TaskKey, "claim", response.Reason)
	}
	hasChanges, err := config.Worktrees.HasChanges(task.WorktreePath)
	if err != nil {
		return err
	}
	if !hasChanges {
		reason := fmt.Sprintf("writer reported blocked with no recorded tool failures and no worktree changes: %s", response.Reason)
		blocked, err := config.Controller.BlockTask(run.JobID, run.TaskKey, reason)
		if err != nil {
			return err
		}
		notify(config, blocked, outbox.EventBlocked+":"+run.TaskKey, response.Summary, reason)
		return nil
	}
	sha, err := config.Worktrees.Commit(task.WorktreePath)
	if err != nil {
		return err
	}
	job, err = config.Controller.RecordWriterCompletion(job.ID, task.Key, run.RunID, sha, append([]pipeline.ValidationEvidence(nil), *response.ValidationEvidence...))
	if err != nil {
		return err
	}
	task = findTask(job, task.Key)
	review, err := config.Dispatcher.StartReviewer(ctx, job, task)
	if err != nil {
		return err
	}
	_, err = config.Controller.StartReviewer(job.ID, task.Key, review.RunID)
	return err
}

type reviewResponse struct {
	Verdict            string   `json:"verdict"`
	Summary            string   `json:"summary"`
	Findings           []string `json:"findings"`
	AcceptanceCriteria []struct {
		Criterion string `json:"criterion"`
		Satisfied bool   `json:"satisfied"`
		Evidence  string `json:"evidence"`
	} `json:"acceptance_criteria"`
	Reason      string                       `json:"reason"`
	PullRequest *pipeline.PullRequestContent `json:"pull_request,omitempty"`
}

func applyReview(ctx context.Context, run dispatcher.DispatchRun, config Config) error {
	var response reviewResponse
	if err := decodeResponse(run.Response, &response); err != nil {
		return fmt.Errorf("invalid task review response: %w", err)
	}
	verdict := pipeline.ReviewVerdict(response.Verdict)
	if verdict != pipeline.ReviewApproved && verdict != pipeline.ReviewChangesRequested && verdict != pipeline.ReviewBlocked {
		return fmt.Errorf("invalid task review verdict %q", response.Verdict)
	}
	before, err := config.Store.Get(run.JobID)
	if err != nil {
		return err
	}
	prior := findTask(before, run.TaskKey)
	if prior == nil {
		return fmt.Errorf("task %q not found", run.TaskKey)
	}
	if verdict == pipeline.ReviewChangesRequested && prior.ReviewerRunID == run.RunID && prior.Status == pipeline.TaskWorking {
		return nil
	}
	report := pipeline.ReviewReport{
		Verdict:  verdict,
		Summary:  response.Summary,
		Findings: append([]string(nil), response.Findings...),
		Reason:   response.Reason,
	}
	for _, criterion := range response.AcceptanceCriteria {
		report.AcceptanceCriteria = append(report.AcceptanceCriteria, pipeline.ReviewCriterion{
			Criterion: criterion.Criterion,
			Satisfied: criterion.Satisfied,
			Evidence:  criterion.Evidence,
		})
	}
	if _, err := config.Controller.RecordReviewReport(run.JobID, run.TaskKey, run.RunID, report); err != nil {
		return err
	}
	job, err := config.Controller.RecordReview(run.JobID, run.TaskKey, run.RunID, verdict)
	if err != nil {
		return err
	}
	if verdict == pipeline.ReviewBlocked {
		job, blockErr := config.Controller.BlockTask(run.JobID, run.TaskKey, response.Reason)
		notify(config, job, outbox.EventBlocked+":"+run.TaskKey, response.Summary, response.Reason)
		err = blockErr
		return err
	}
	if verdict == pipeline.ReviewChangesRequested {
		task := findTask(job, run.TaskKey)
		if repeatedReviewFeedback(task) >= 3 {
			reason := "review feedback repeated across three attempts; blocked to prevent an unproductive revision loop"
			job, err = config.Controller.BlockTask(run.JobID, run.TaskKey, reason)
			notify(config, job, outbox.EventBlocked+":"+run.TaskKey, "OpenDev stopped repeated review feedback", reason)
			return err
		}
		if stop, err := enforceWriterAttemptLimit(job, task, config); stop || err != nil {
			return err
		}
		_, err := startWriter(ctx, job, task, config)
		return err
	}
	_, _, err = integrateEligible(job.ID, config, ctx)
	return err
}

func repeatedReviewFeedback(task *pipeline.Task) int {
	if task == nil || len(task.ReviewHistory) == 0 {
		return 0
	}
	latest := task.ReviewHistory[len(task.ReviewHistory)-1]
	if latest.Verdict != pipeline.ReviewChangesRequested {
		return 0
	}
	count := 0
	for i := len(task.ReviewHistory) - 1; i >= 0; i-- {
		report := task.ReviewHistory[i]
		if report.Verdict != pipeline.ReviewChangesRequested || report.Summary != latest.Summary || !sameStrings(report.Findings, latest.Findings) {
			break
		}
		count++
	}
	return count
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func applyHolistic(ctx context.Context, run dispatcher.DispatchRun, config Config) error {
	var response reviewResponse
	if err := decodeResponse(run.Response, &response); err != nil {
		return fmt.Errorf("invalid holistic review response: %w", err)
	}
	verdict := pipeline.ReviewVerdict(response.Verdict)
	if verdict != pipeline.ReviewApproved && verdict != pipeline.ReviewChangesRequested && verdict != pipeline.ReviewBlocked {
		return fmt.Errorf("invalid holistic review verdict %q", response.Verdict)
	}
	job, err := config.Store.Get(run.JobID)
	if err != nil {
		return err
	}
	content := pipeline.PullRequestContent{}
	if response.PullRequest != nil {
		content = *response.PullRequest
	}
	updated, err := config.Controller.RecordHolisticReview(job.ID, run.RunID, job.IntegrationSHA, verdict, content)
	if err != nil {
		return err
	}
	if verdict == pipeline.ReviewBlocked {
		failed, failErr := config.Controller.FailJob(job.ID, response.Reason)
		notify(config, failed, outbox.EventFailed, response.Summary, response.Reason)
		err = failErr
		return err
	}
	if verdict == pipeline.ReviewChangesRequested {
		if job.HolisticRounds > maxHolisticRemediationRounds {
			reason := fmt.Sprintf("holistic review requested changes across %d rounds; stopping", job.HolisticRounds)
			failed, failErr := config.Controller.FailJob(job.ID, reason)
			notify(config, failed, outbox.EventFailed, response.Summary, reason)
			return failErr
		}
		reopened, err := config.Store.ReopenIncompleteTasks(job.ID)
		if err != nil {
			// Everything was already integrated and approved; the request must
			// target the integrated diff itself, so route it through PR-branch
			// remediation via the CI path's synthetic task machinery.
			return remediateIntegratedDiff(ctx, updated, config, response)
		}
		notify(config, reopened, outbox.EventCIRemediating, response.Summary, strings.Join(response.Findings, "\n"))
		_, err = startReadyWriters(ctx, reopened, config)
		return err
	}
	if verdict == pipeline.ReviewApproved {
		_, err = publishApprovedJob(ctx, updated.ID, config)
	}
	return err
}

// maxHolisticRemediationRounds bounds holistic changes-requested cycles.
const maxHolisticRemediationRounds = 3

// remediateIntegratedDiff hands a holistic changes-requested verdict about an
// already fully integrated diff to the PR-branch remediation machinery by
// creating a synthetic remediation task keyed to the integration SHA.
func remediateIntegratedDiff(ctx context.Context, job *pipeline.Job, config Config, response reviewResponse) error {
	fingerprint := fmt.Sprintf("holistic:%x", sha256.Sum256([]byte(strings.Join(response.Findings, "\n"))))
	checks := []pipeline.CheckResult(nil)
	if job.CI != nil {
		checks = job.CI.Checks
	}
	controller := pipeline.NewController(config.Store)
	_, task, err := controller.StartCIRemediation(job.ID, fingerprint, checks)
	if err != nil {
		return err
	}
	if config.Logger != nil {
		config.Logger.Info("holistic remediation dispatched", "job_id", job.ID, "task", task.Key)
	}
	return nil
}

func recordTerminalFailure(run dispatcher.DispatchRun, config Config) error {
	reason := strings.TrimSpace(run.Error)
	if reason == "" {
		reason = "AgentFoundry run ended with status " + run.Status
	}
	if run.Role == dispatcher.RolePlanner || run.Role == dispatcher.RoleHolistic {
		job, err := config.Controller.FailJob(run.JobID, reason)
		eventType := outbox.EventFailed
		if run.Status == "canceled" || run.Status == "cancelled" {
			eventType = outbox.EventCancelled
		}
		notify(config, job, eventType, "", reason)
		return err
	}
	job, err := config.Controller.BlockTask(run.JobID, run.TaskKey, reason)
	eventType := outbox.EventBlocked + ":" + run.TaskKey
	if run.Status == "canceled" || run.Status == "cancelled" {
		eventType = outbox.EventCancelled + ":" + run.TaskKey
	}
	notify(config, job, eventType, "", reason)
	return err
}

func decodeResponse(raw string, output any) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}
