// Package dispatcher starts and observes AgentFoundry runs for durable coding jobs.
package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/iamangus/code-mcp/internal/agentfoundry"
	"github.com/iamangus/code-mcp/internal/pipeline"
)

const (
	RolePlanner  = "planner"
	RoleWriter   = "writer"
	RoleReviewer = "reviewer"
	RoleHolistic = "holistic"
)

// RunStore must durably retain records before a request is submitted. An
// implementation supplied by the application makes reconciliation restart-safe.
type RunStore interface {
	Get(ctx context.Context, taskID string) (*DispatchRun, error)
	Save(ctx context.Context, run DispatchRun) error
	ListUnapplied(ctx context.Context) ([]DispatchRun, error)
}

// RunInspector is optional so existing durable stores retain the minimal
// dispatch contract while stores that support it can power operator inspection.
type RunInspector interface {
	ListByJob(context.Context, string) ([]DispatchRun, error)
}

// Supersede marks a terminal dispatch as deliberately replaced by an operator retry.
func (d *Dispatcher) Supersede(ctx context.Context, taskID, reason string) error {
	run, err := d.store.Get(ctx, taskID)
	if err != nil {
		return fmt.Errorf("load dispatch run: %w", err)
	}
	if run == nil || !terminal(run.Status) || run.OutcomeApplied {
		return fmt.Errorf("dispatch run %q is not an unapplied terminal result", taskID)
	}
	run.OutcomeApplied = true
	run.Error = strings.TrimSpace(run.Error + "; superseded: " + reason)
	return d.store.Save(ctx, *run)
}

// DispatchRun is the persisted dispatch intent and remote run identifier.
type DispatchRun struct {
	TaskID         string
	JobID          string
	Role           string
	TaskKey        string
	Attempt        int
	AgentID        string
	Message        string
	RunID          string
	WorkflowID     string
	Status         string
	Response       string
	Error          string
	OutcomeApplied bool
	StartedAt      time.Time
	CompletedAt    time.Time
}

// OutcomeHandler applies a terminal run's durable response to the coding pipeline.
type OutcomeHandler func(context.Context, DispatchRun) error

// Runner is the AgentFoundry subset used by Dispatcher.
type Runner interface {
	StartRun(context.Context, string, agentfoundry.RunOptions) (string, error)
	GetRun(context.Context, string) (*agentfoundry.Run, error)
	GetRunByTaskID(context.Context, string) (*agentfoundry.Run, error)
}

// DetailedRunner is implemented by AgentFoundry clients that expose the
// durable Temporal workflow identity at submission time.
type DetailedRunner interface {
	StartRunDetail(context.Context, string, agentfoundry.RunOptions) (*agentfoundry.Run, error)
}

// TraceRunner returns a durable normalized Temporal trace for a workflow.
type TraceRunner interface {
	GetExecutionTrace(context.Context, string) (json.RawMessage, error)
}

type Config struct {
	MCPServers     []agentfoundry.MCPServer
	PollInterval   time.Duration
	Logger         *slog.Logger
	OutcomeHandler OutcomeHandler
}

// Dispatcher launches asynchronous runs, durably observes terminal responses,
// then delegates their application to the configured outcome handler.
type Dispatcher struct {
	runner         Runner
	store          RunStore
	mcpServers     []agentfoundry.MCPServer
	pollInterval   time.Duration
	logger         *slog.Logger
	outcomeHandler OutcomeHandler
	ctx            context.Context
	cancel         context.CancelFunc
	mu             sync.Mutex
	watching       map[string]struct{}
}

func New(runner Runner, store RunStore, config Config) (*Dispatcher, error) {
	if runner == nil || store == nil {
		return nil, fmt.Errorf("AgentFoundry runner and dispatch run store are required")
	}
	if config.PollInterval <= 0 {
		config.PollInterval = 5 * time.Second
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Dispatcher{runner: runner, store: store, mcpServers: config.MCPServers, pollInterval: config.PollInterval, logger: config.Logger, outcomeHandler: config.OutcomeHandler, ctx: ctx, cancel: cancel, watching: make(map[string]struct{})}, nil
}

func (d *Dispatcher) Close() { d.cancel() }

// InspectJob returns the immutable dispatch records for an operator view.
func (d *Dispatcher) InspectJob(ctx context.Context, jobID string) ([]DispatchRun, error) {
	store, ok := d.store.(RunInspector)
	if !ok {
		return nil, fmt.Errorf("dispatch store does not support inspection")
	}
	return store.ListByJob(ctx, jobID)
}

// ExecutionTrace resolves the durable per-turn AgentFoundry trace recorded for
// a dispatch. Legacy records without workflow identity remain inspectable but
// cannot be traced.
func (d *Dispatcher) ExecutionTrace(ctx context.Context, run DispatchRun) (json.RawMessage, error) {
	if run.WorkflowID == "" {
		return nil, fmt.Errorf("dispatch run %q has no workflow ID", run.TaskID)
	}
	tracer, ok := d.runner.(TraceRunner)
	if !ok {
		return nil, fmt.Errorf("AgentFoundry runner does not support execution traces")
	}
	return tracer.GetExecutionTrace(ctx, run.WorkflowID)
}

func (d *Dispatcher) StartPlanner(ctx context.Context, job *pipeline.Job) (*DispatchRun, error) {
	if job == nil {
		return nil, fmt.Errorf("job is required")
	}
	return d.start(ctx, job, RolePlanner, "job", 1, job.PlannerAgentID, fmt.Sprintf("Plan coding job %s: %s. Your final structured response is authoritative; do not use a reporting or completion MCP tool.", job.ID, job.Directive))
}

func (d *Dispatcher) StartWriter(ctx context.Context, job *pipeline.Job, task *pipeline.Task) (*DispatchRun, error) {
	if job == nil {
		return nil, fmt.Errorf("job is required")
	}
	if task == nil {
		return nil, fmt.Errorf("task is required")
	}
	action := "Implement"
	if task.Kind == pipeline.TaskValidation {
		action = "Validate"
	}
	return d.start(ctx, job, RoleWriter, task.Key, task.WriterAttempts+1, job.WriterAgentID, taskMessage(action, job, task))
}

func (d *Dispatcher) StartReviewer(ctx context.Context, job *pipeline.Job, task *pipeline.Task) (*DispatchRun, error) {
	if job == nil {
		return nil, fmt.Errorf("job is required")
	}
	if task == nil {
		return nil, fmt.Errorf("task is required")
	}
	return d.start(ctx, job, RoleReviewer, task.Key, task.ReviewerAttempts+1, job.ReviewerAgentID, taskMessage("Review", job, task))
}

func (d *Dispatcher) StartHolistic(ctx context.Context, job *pipeline.Job) (*DispatchRun, error) {
	if job == nil {
		return nil, fmt.Errorf("job is required")
	}
	agentID := job.HolisticAgentID
	if agentID == "" {
		agentID = job.ReviewerAgentID
	}
	return d.start(ctx, job, RoleHolistic, "job", 1, agentID, fmt.Sprintf("Perform the holistic review for coding job %s at integration SHA %s. Your final structured response is authoritative; do not use a reporting or completion MCP tool.", job.ID, job.IntegrationSHA))
}

func taskMessage(action string, job *pipeline.Job, task *pipeline.Task) string {
	message := fmt.Sprintf("%s task %s for coding job %s: %s. Your final structured response is authoritative; do not use a reporting or completion MCP tool.", action, task.Key, job.ID, task.Description)
	if action == "Implement" && task.LatestReview != nil && task.LatestReview.Verdict == pipeline.ReviewChangesRequested {
		review := task.LatestReview
		message += fmt.Sprintf(" This is revision work for writer attempt %d. Address this immutable reviewer feedback from reviewer attempt %d on commit %s: %s", review.WriterAttempt, review.ReviewerAttempt, review.ReviewedCommitSHA, review.Summary)
		if len(review.Findings) > 0 {
			message += " Findings: " + strings.Join(review.Findings, " | ")
		}
	}
	return message
}

func (d *Dispatcher) start(ctx context.Context, job *pipeline.Job, role, taskKey string, attempt int, agentID, message string) (*DispatchRun, error) {
	if job == nil || strings.TrimSpace(job.ID) == "" || strings.TrimSpace(agentID) == "" || strings.TrimSpace(taskKey) == "" || attempt < 1 {
		return nil, fmt.Errorf("valid job, task, attempt, and agent ID are required")
	}
	taskID := TaskID(job.ID, role, taskKey, attempt)
	existing, err := d.store.Get(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("load dispatch run: %w", err)
	}
	if existing != nil {
		if existing.RunID == "" {
			if err := d.launch(ctx, existing); err != nil {
				return nil, err
			}
			return existing, nil
		}
		if existing.RunID != "" && (!terminal(existing.Status) || !existing.OutcomeApplied) {
			d.watch(*existing)
		}
		return existing, nil
	}
	run := DispatchRun{TaskID: taskID, JobID: job.ID, Role: role, TaskKey: taskKey, Attempt: attempt, AgentID: agentID, Message: message, Status: "starting", StartedAt: time.Now().UTC()}
	if err := d.store.Save(ctx, run); err != nil {
		return nil, fmt.Errorf("persist dispatch intent: %w", err)
	}
	if err := d.launch(ctx, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// Reconcile resumes polling persisted runs. For an intent without a recorded
// run ID, launch first asks AgentFoundry whether the submission already arrived.
func (d *Dispatcher) Reconcile(ctx context.Context) error {
	runs, err := d.store.ListUnapplied(ctx)
	if err != nil {
		return fmt.Errorf("list pending dispatch runs: %w", err)
	}
	for i := range runs {
		run := runs[i]
		if run.RunID == "" {
			if err := d.launch(ctx, &run); err != nil {
				return err
			}
			continue
		}
		if terminal(run.Status) {
			d.apply(run)
		} else {
			d.watch(run)
		}
	}
	return nil
}

func (d *Dispatcher) launch(ctx context.Context, run *DispatchRun) error {
	if recovered, err := d.runner.GetRunByTaskID(ctx, run.TaskID); err != nil {
		return fmt.Errorf("find existing %s run: %w", run.Role, err)
	} else if recovered != nil {
		run.RunID, run.WorkflowID, run.Status, run.Response, run.Error = recovered.ID, recovered.WorkflowID, recovered.Status, recovered.Response, recovered.Error
		if err := d.store.Save(ctx, *run); err != nil {
			return fmt.Errorf("persist recovered AgentFoundry run: %w", err)
		}
		if terminal(run.Status) {
			d.apply(*run)
		} else {
			d.watch(*run)
		}
		return nil
	}
	options := agentfoundry.RunOptions{Message: run.Message, TaskID: run.TaskID, MCPServers: d.mcpServers}
	if detailed, ok := d.runner.(DetailedRunner); ok {
		started, err := detailed.StartRunDetail(ctx, run.AgentID, options)
		if err != nil {
			return fmt.Errorf("start %s run: %w", run.Role, err)
		}
		run.RunID, run.WorkflowID, run.Status = started.ID, started.WorkflowID, "running"
	} else {
		runID, err := d.runner.StartRun(ctx, run.AgentID, options)
		if err != nil {
			return fmt.Errorf("start %s run: %w", run.Role, err)
		}
		run.RunID, run.Status = runID, "running"
	}
	if err := d.store.Save(ctx, *run); err != nil {
		return fmt.Errorf("persist AgentFoundry run ID: %w", err)
	}
	d.watch(*run)
	return nil
}

func (d *Dispatcher) watch(run DispatchRun) {
	d.mu.Lock()
	if _, exists := d.watching[run.TaskID]; exists {
		d.mu.Unlock()
		return
	}
	d.watching[run.TaskID] = struct{}{}
	d.mu.Unlock()
	go func() {
		defer func() {
			d.mu.Lock()
			delete(d.watching, run.TaskID)
			d.mu.Unlock()
		}()
		ticker := time.NewTicker(d.pollInterval)
		defer ticker.Stop()
		for {
			observed, err := d.runner.GetRun(d.ctx, run.RunID)
			if err == nil && observed != nil && terminal(observed.Status) {
				run.Status = observed.Status
				run.Response = observed.Response
				run.Error = observed.Error
				run.CompletedAt = time.Now().UTC()
				if err := d.store.Save(d.ctx, run); err == nil {
					d.logger.Info("AgentFoundry run reached terminal state", "job_id", run.JobID, "role", run.Role, "task", run.TaskKey, "run_id", run.RunID, "status", observed.Status)
					d.apply(run)
				}
				return
			}
			select {
			case <-d.ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (d *Dispatcher) apply(run DispatchRun) {
	if run.OutcomeApplied || d.outcomeHandler == nil {
		return
	}
	if err := d.outcomeHandler(d.ctx, run); err != nil {
		d.logger.Error("apply AgentFoundry terminal outcome", "job_id", run.JobID, "role", run.Role, "task", run.TaskKey, "run_id", run.RunID, "error", err)
		return
	}
	run.OutcomeApplied = true
	if err := d.store.Save(d.ctx, run); err != nil {
		d.logger.Error("persist AgentFoundry outcome marker", "run_id", run.RunID, "error", err)
	}
}

func TaskID(jobID, role, taskKey string, attempt int) string {
	return fmt.Sprintf("opendev:%s:%s:%s:%d", jobID, role, taskKey, attempt)
}

func terminal(status string) bool {
	switch status {
	case "completed", "failed", "canceled", "cancelled":
		return true
	default:
		return false
	}
}
