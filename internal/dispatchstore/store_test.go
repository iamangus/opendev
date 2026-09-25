package dispatchstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/iamangus/code-mcp/internal/dispatcher"
)

func TestStorePersistsTerminalUnappliedRuns(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), dispatcher.DispatchRun{TaskID: "pending", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), dispatcher.DispatchRun{TaskID: "done", Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(dir, "dispatch-runs.json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state file permissions: %v, %v", info, err)
	}
	reopened, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := reopened.ListUnapplied(context.Background())
	if err != nil || len(pending) != 2 {
		t.Fatalf("ListUnapplied = %+v, %v", pending, err)
	}
}

func TestFailedDispatchSaveDoesNotAppearAccepted(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	original := store.path
	store.path = filepath.Join(dir, "missing", "dispatch-runs.json")
	run := dispatcher.DispatchRun{TaskID: "attempt-1", Status: "starting"}
	if err := store.Save(context.Background(), run); err == nil {
		t.Fatal("save succeeded without durable file")
	}
	if stored, err := store.Get(context.Background(), run.TaskID); err != nil || stored != nil {
		t.Fatalf("failed dispatch remained in memory: %+v %v", stored, err)
	}
	store.path = original
	if err := store.Save(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	reloaded, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if stored, err := reloaded.Get(context.Background(), run.TaskID); err != nil || stored == nil {
		t.Fatalf("retried dispatch not persisted: %+v %v", stored, err)
	}
}
