package dispatchstore

import (
	"context"
	"testing"

	"github.com/iamangus/code-mcp/internal/dispatcher"
)

func TestArchivedRunsRemainInspectableAndCannotBeResurrected(t *testing.T) {
	dir := t.TempDir()
	store, _ := New(dir)
	ctx := context.Background()
	run := dispatcher.DispatchRun{TaskID: "opendev:job:writer:one:1", JobID: "job", Status: "completed", OutcomeApplied: true, Response: "durable result"}
	if err := store.Save(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := store.ArchiveJob("job"); err != nil {
		t.Fatal(err)
	}
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, run.TaskID)
	if err != nil || got == nil || got.Response != run.Response {
		t.Fatalf("archive lost: %+v %v", got, err)
	}
	run.Status, run.OutcomeApplied = "running", false
	if err := store.Save(ctx, run); err != nil {
		t.Fatal(err)
	}
	pending, _ := store.ListUnapplied(ctx)
	if len(pending) != 0 {
		t.Fatal("stale watcher resurrected archived run")
	}
	runs, err := store.ListByJob(ctx, "job")
	if err != nil || len(runs) != 1 || !runs[0].OutcomeApplied {
		t.Fatalf("inspection: %+v %v", runs, err)
	}
}
