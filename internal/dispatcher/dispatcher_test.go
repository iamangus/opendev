package dispatcher

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/iamangus/code-mcp/internal/agentfoundry"
	"github.com/iamangus/code-mcp/internal/pipeline"
)

type memoryRunStore struct {
	mu   sync.Mutex
	runs map[string]DispatchRun
}

func (s *memoryRunStore) Get(_ context.Context, taskID string) (*DispatchRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[taskID]
	if !ok {
		return nil, nil
	}
	return &run, nil
}
func (s *memoryRunStore) Save(_ context.Context, run DispatchRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[run.TaskID] = run
	return nil
}
func (s *memoryRunStore) ListUnapplied(_ context.Context) ([]DispatchRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var runs []DispatchRun
	for _, run := range s.runs {
		if !terminal(run.Status) || !run.OutcomeApplied {
			runs = append(runs, run)
		}
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].TaskID < runs[j].TaskID })
	return runs, nil
}

type fakeRunner struct {
	mu        sync.Mutex
	starts    int
	agentID   string
	options   agentfoundry.RunOptions
	getStatus string
	startErr  error
}

func (f *fakeRunner) StartRun(_ context.Context, agentID string, options agentfoundry.RunOptions) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	f.agentID = agentID
	f.options = options
	if f.startErr != nil {
		return "", f.startErr
	}
	return "run-1", nil
}
func (f *fakeRunner) GetRun(_ context.Context, _ string) (*agentfoundry.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &agentfoundry.Run{ID: "run-1", Status: f.getStatus}, nil
}
func (*fakeRunner) GetRunByTaskID(context.Context, string) (*agentfoundry.Run, error) {
	return nil, nil
}

func TestStartWriterPersistsDeterministicRunAndObservesTerminalState(t *testing.T) {
	store := &memoryRunStore{runs: make(map[string]DispatchRun)}
	runner := &fakeRunner{getStatus: "completed"}
	dispatcher, err := New(runner, store, Config{PollInterval: time.Millisecond, MCPServers: []agentfoundry.MCPServer{{Name: "jobs", URL: "http://mcp.test", Transport: "streamable-http"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer dispatcher.Close()
	job := &pipeline.Job{ID: "job-7", WriterAgentID: "writer"}
	task := &pipeline.Task{Key: "api", Description: "Add API", WriterAttempts: 1}
	run, err := dispatcher.StartWriter(context.Background(), job, task)
	if err != nil {
		t.Fatal(err)
	}
	if run.TaskID != "opendev:job-7:writer:api:2" || run.RunID != "run-1" {
		t.Fatalf("unexpected dispatch run: %+v", run)
	}
	runner.mu.Lock()
	options := runner.options
	runner.mu.Unlock()
	if options.TaskID != run.TaskID || len(options.MCPServers) != 1 {
		t.Fatalf("unexpected run options: %+v", options)
	}
	deadline := time.Now().Add(time.Second)
	for {
		persisted, _ := store.Get(context.Background(), run.TaskID)
		if persisted.Status == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminal state was not persisted: %+v", persisted)
		}
		time.Sleep(time.Millisecond)
	}
	if job.Status != "" || task.Status != "" {
		t.Fatal("dispatcher must not change pipeline state")
	}
}

func TestReconcileRetriesPersistedStartIntent(t *testing.T) {
	store := &memoryRunStore{runs: map[string]DispatchRun{
		"opendev:job-1:planner:job:1": {TaskID: "opendev:job-1:planner:job:1", JobID: "job-1", Role: RolePlanner, TaskKey: "job", Attempt: 1, AgentID: "planner", Message: "plan", Status: "starting"},
	}}
	runner := &fakeRunner{getStatus: "running"}
	dispatcher, err := New(runner, store, Config{PollInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer dispatcher.Close()
	if err := dispatcher.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	starts := runner.starts
	runner.mu.Unlock()
	if starts != 1 {
		t.Fatalf("starts = %d, want 1", starts)
	}
	persisted, _ := store.Get(context.Background(), "opendev:job-1:planner:job:1")
	if persisted.RunID != "run-1" || persisted.Status != "running" {
		t.Fatalf("reconciliation did not persist run ID: %+v", persisted)
	}
}

func TestReconcileContinuesAfterFailedStartIntent(t *testing.T) {
	store := &memoryRunStore{runs: map[string]DispatchRun{
		"a-stale": {TaskID: "a-stale", JobID: "job-1", Role: RoleHolistic, TaskKey: "job", AgentID: "missing", Status: "starting"},
		"z-terminal": {TaskID: "z-terminal", JobID: "job-1", Role: RoleWriter, TaskKey: "task", AgentID: "writer", RunID: "run-1", Status: "completed", Response: `{"status":"completed"}`},
	}}
	var applied int
	d, err := New(&fakeRunner{startErr: fmt.Errorf("agent not found")}, store, Config{OutcomeHandler: func(_ context.Context, run DispatchRun) error {
		if run.TaskID == "z-terminal" {
			applied++
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if applied != 1 {
		t.Fatalf("terminal outcomes applied = %d, want 1", applied)
	}
}

func TestStartHolisticRefreshesUnacceptedIntent(t *testing.T) {
	job := &pipeline.Job{ID: "job-1", ReviewerAgentID: "reviewer", IntegrationSHA: "integrated"}
	taskID := TaskID(job.ID, RoleHolistic, "job", 1)
	store := &memoryRunStore{runs: map[string]DispatchRun{
		taskID: {TaskID: taskID, JobID: job.ID, Role: RoleHolistic, TaskKey: "job", Attempt: 1, AgentID: "TODO", Message: "stale", Status: "starting"},
	}}
	runner := &fakeRunner{getStatus: "running"}
	d, err := New(runner, store, Config{PollInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, err := d.StartHolistic(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	persisted, _ := store.Get(context.Background(), taskID)
	if persisted.AgentID != "reviewer" || persisted.RunID != "run-1" {
		t.Fatalf("unaccepted intent was not refreshed: %+v", persisted)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.agentID != "reviewer" {
		t.Fatalf("started agent = %q, want reviewer", runner.agentID)
	}
}

func TestReconcileAppliesTerminalResponseOnce(t *testing.T) {
	store := &memoryRunStore{runs: map[string]DispatchRun{
		"opendev:job-1:planner:job:1": {TaskID: "opendev:job-1:planner:job:1", JobID: "job-1", Role: RolePlanner, TaskKey: "job", Attempt: 1, AgentID: "planner", RunID: "run-1", Status: "completed", Response: `{"summary":"plan"}`},
	}}
	var calls int
	d, err := New(&fakeRunner{}, store, Config{OutcomeHandler: func(_ context.Context, run DispatchRun) error {
		calls++
		if run.Response == "" {
			t.Fatal("terminal response was not persisted")
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := d.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("outcome applications = %d, want 1", calls)
	}
	run, _ := store.Get(context.Background(), "opendev:job-1:planner:job:1")
	if !run.OutcomeApplied {
		t.Fatal("outcome-applied marker was not persisted")
	}
}

func TestInvalidTerminalOutcomeIsNotMarkedApplied(t *testing.T) {
	store := &memoryRunStore{runs: map[string]DispatchRun{
		"opendev:job-1:writer:one:1": {TaskID: "opendev:job-1:writer:one:1", JobID: "job-1", Role: RoleWriter, TaskKey: "one", Attempt: 1, AgentID: "writer", RunID: "run-1", Status: "completed", Response: "not-json"},
	}}
	d, err := New(&fakeRunner{}, store, Config{OutcomeHandler: func(_ context.Context, _ DispatchRun) error {
		return fmt.Errorf("invalid response")
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	run, _ := store.Get(context.Background(), "opendev:job-1:writer:one:1")
	if run.OutcomeApplied {
		t.Fatal("invalid terminal response was marked applied")
	}
}
