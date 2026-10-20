package outbox

import (
	"testing"

	"github.com/iamangus/code-mcp/internal/pipeline"
)

func TestTransitionJournalRepairsMissingAndRepeatedEvents(t *testing.T) {
	dir := t.TempDir()
	jobs, _ := pipeline.NewStore(dir)
	job, _ := jobs.Create("repo", "directive", "main", "", "p", "w", "r")
	jobs.StartPlanning(job.ID)
	jobs.Cancel(job.ID, "operator")
	// No Notify calls: both state and their event identities must survive restart.
	jobs, err := pipeline.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	box, _ := New(dir)
	if err := box.Reconcile(jobs.List()); err != nil {
		t.Fatal(err)
	}
	if len(box.List()) != 2 {
		t.Fatalf("missing transition events: %+v", box.List())
	}
	if err := box.Reconcile(jobs.List()); err != nil {
		t.Fatal(err)
	}
	if len(box.List()) != 2 {
		t.Fatal("journal replay duplicated notifications")
	}
}
