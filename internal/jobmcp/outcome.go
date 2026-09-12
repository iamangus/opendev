package jobmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/iamangus/code-mcp/internal/dispatcher"
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
	if response.Status == "blocked" {
		_, err := config.Controller.BlockTask(run.JobID, run.TaskKey, response.Reason)
		return err
	}
	job, err := config.Store.Get(run.JobID)
	if err != nil {
		return err
	}
	task := findTask(job, run.TaskKey)
	if task == nil || task.WriterRunID != run.RunID {
		return fmt.Errorf("writer run does not own task %q", run.TaskKey)
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

type reviewResponse struct {
	Verdict            string   `json:"verdict"`
	Summary            string   `json:"summary"`
	Findings           []string `json:"findings"`
	AcceptanceCriteria []struct {
		Criterion string `json:"criterion"`
		Satisfied bool   `json:"satisfied"`
		Evidence  string `json:"evidence"`
	} `json:"acceptance_criteria"`
	Reason string `json:"reason"`
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
	job, err := config.Controller.RecordReview(run.JobID, run.TaskKey, run.RunID, verdict)
	if err != nil {
		return err
	}
	if verdict == pipeline.ReviewBlocked {
		_, err = config.Controller.BlockTask(run.JobID, run.TaskKey, response.Reason)
		return err
	}
	if verdict == pipeline.ReviewChangesRequested {
		_, err := startWriter(ctx, job, findTask(job, run.TaskKey), config)
		return err
	}
	_, _, err = integrateEligible(job.ID, config, ctx)
	return err
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
		_, err = config.Controller.FailJob(job.ID, response.Reason)
		return err
	}
	if verdict == pipeline.ReviewApproved {
		_, err = publishApprovedJob(ctx, updated.ID, config)
	}
	return err
}

func recordTerminalFailure(run dispatcher.DispatchRun, config Config) error {
	reason := strings.TrimSpace(run.Error)
	if reason == "" {
		reason = "AgentFoundry run ended with status " + run.Status
	}
	if run.Role == dispatcher.RolePlanner || run.Role == dispatcher.RoleHolistic {
		_, err := config.Controller.FailJob(run.JobID, reason)
		return err
	}
	_, err := config.Controller.BlockTask(run.JobID, run.TaskKey, reason)
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
