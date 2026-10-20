package dispatcher

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iamangus/code-mcp/internal/agentfoundry"
)

func TestTerminalApplicationRetriesWithoutRestart(t *testing.T) {
	run := DispatchRun{TaskID: "task", JobID: "job", RunID: "run", Status: "completed"}
	store := &memoryRunStore{runs: map[string]DispatchRun{"task": run}}
	var calls atomic.Int32
	d, err := New(&fakeRunner{}, store, Config{PollInterval: time.Millisecond, OutcomeHandler: func(context.Context, DispatchRun) error {
		if calls.Add(1) == 1 {
			return fmt.Errorf("temporary failure")
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
	deadline := time.After(time.Second)
	for {
		current, _ := store.Get(context.Background(), "task")
		if current.OutcomeApplied {
			break
		}
		select {
		case <-deadline:
			t.Fatal("outcome never retried")
		case <-time.After(time.Millisecond):
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("applications: %d", calls.Load())
	}
}

func TestRecoveredLaunchReconstructsMCPAttachments(t *testing.T) {
	run := DispatchRun{TaskID: "task", JobID: "job", Role: RoleWriter, TaskKey: "a", Attempt: 1, AgentID: "writer", Message: "work", Status: "starting"}
	store := &memoryRunStore{runs: map[string]DispatchRun{"task": run}}
	runner := &fakeRunner{getStatus: "running"}
	d, err := New(runner, store, Config{MCPServersForRun: func(_ context.Context, recovered DispatchRun) ([]agentfoundry.MCPServer, error) {
		if recovered.TaskID != "task" {
			t.Errorf("wrong recovered intent")
		}
		return []agentfoundry.MCPServer{{Name: "restored-tools", URL: "http://tools.test"}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.options.MCPServers) != 1 || runner.options.MCPServers[0].Name != "restored-tools" {
		t.Fatalf("tools not restored: %+v", runner.options)
	}
}
