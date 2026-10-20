package pipeline

import "testing"

func TestPauseResumeAndCancellationPersist(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)
	job, _ := s.Create("repo", "directive", "main", "", "p", "w", "r")
	s.StartPlanning(job.ID)
	paused, err := s.Pause(job.ID)
	if err != nil || paused.Status != JobPaused || paused.PausedFrom != JobPlanning {
		t.Fatalf("pause: %+v %v", paused, err)
	}
	s, err = NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := s.Resume(job.ID)
	if err != nil || resumed.Status != JobPlanning {
		t.Fatalf("resume: %+v %v", resumed, err)
	}
	cancelled, err := s.Cancel(job.ID, "operator requested")
	if err != nil || cancelled.Status != JobCancelled {
		t.Fatalf("cancel: %+v %v", cancelled, err)
	}
	if _, err := s.Resume(job.ID); err == nil {
		t.Fatal("cancelled job resumed")
	}
	if len(cancelled.Events) == 0 || cancelled.Events[len(cancelled.Events)-1].Type != "cancelled" {
		t.Fatal("cancellation was not journaled")
	}
}

func TestArchivePreservesInspectionAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir)
	job, _ := store.Create("Repo", "directive", "main", "", "p", "w", "r")
	if job.Repository != "repo" {
		t.Fatal("repository identity was not normalized")
	}
	if _, err := store.Archive(job.ID); err == nil {
		t.Fatal("active job archived")
	}
	store.Cancel(job.ID, "operator")
	if _, err := store.Archive(job.ID); err != nil {
		t.Fatal(err)
	}
	if len(store.List()) != 0 {
		t.Fatal("archived job still in active snapshots")
	}
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	archived, err := store.Get(job.ID)
	if err != nil || archived.Status != JobCancelled {
		t.Fatalf("archive inspection: %+v %v", archived, err)
	}
	if _, err := store.Get("../../jobs"); err == nil {
		t.Fatal("archive traversal accepted")
	}
}
