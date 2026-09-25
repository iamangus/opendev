package outbox

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iamangus/code-mcp/internal/pipeline"
)

func TestEnqueueIsIdempotentAndPersistent(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	event := Event{Type: EventStarted, JobID: "job-1", Status: "planning", Summary: "Plan work"}
	if err := store.Enqueue(event); err != nil {
		t.Fatal(err)
	}
	if err := store.Enqueue(event); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if events := reopened.due(time.Now().UTC()); len(events) != 1 || events[0].ID != eventID("job-1", EventStarted) {
		t.Fatalf("unexpected persisted events: %#v", events)
	}
}

func TestFailedOutboxSaveCanBeRetried(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	original := store.path
	store.path = filepath.Join(dir, "missing", "outbox.json")
	event := Event{Type: EventStarted, JobID: "job-1"}
	if err := store.Enqueue(event); err == nil {
		t.Fatal("enqueue acknowledged without persistence")
	}
	if len(store.events) != 0 {
		t.Fatalf("failed event remained in memory: %+v", store.events)
	}
	store.path = original
	if err := store.Enqueue(event); err != nil {
		t.Fatal(err)
	}
	id := eventID(event.JobID, event.Type)
	store.path = filepath.Join(dir, "missing", "outbox.json")
	if err := store.delivered(id); err == nil || store.events[id].DeliveredAt != nil {
		t.Fatalf("failed delivery receipt was retained: %+v %v", store.events[id], err)
	}
	store.path = original
	if err := store.delivered(id); err != nil {
		t.Fatal(err)
	}
	reloaded, err := New(dir)
	if err != nil || reloaded.events[id].DeliveredAt == nil {
		t.Fatalf("delivery receipt did not survive restart: %+v %v", reloaded.events[id], err)
	}
}

func TestReconcileRecoversNotificationAfterWriteOutage(t *testing.T) {
	jobs, err := pipeline.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	job, err := jobs.Create("repo", "directive", "main", "", "planner", "writer", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.StartPlanning(job.ID); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	original := store.path
	store.path = filepath.Join(dir, "missing", "outbox.json")
	d := NewDeliverer(store, "http://example.invalid/events", "token", nil, jobs)
	d.reconcile()
	if len(store.events) != 0 {
		t.Fatalf("unpersisted notification was retained: %+v", store.events)
	}
	store.path = original
	d.reconcile()
	if events := store.due(time.Now().UTC()); len(events) != 1 || events[0].Type != EventStarted {
		t.Fatalf("persisted transition not recovered: %+v", events)
	}
}

func TestReconcileRestoresMeaningfulTransitions(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	job := &pipeline.Job{ID: "job-1", Directive: "Fix the bug", Status: pipeline.JobFailed, Failure: "planner failed", PullRequestURL: "https://example.test/pr/1", MergeState: pipeline.MergeMerged, Plan: &pipeline.Plan{Tasks: []pipeline.Task{{Key: "task", Status: pipeline.TaskBlocked, BlockReason: "blocked"}}}}
	if err := store.Reconcile([]*pipeline.Job{job}); err != nil {
		t.Fatal(err)
	}
	if events := store.due(time.Now().UTC()); len(events) != 5 {
		t.Fatalf("got %d events, want 5: %#v", len(events), events)
	}
}

func TestDeliveryRetriesAndUsesBearerAuthentication(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Enqueue(Event{Type: EventStarted, JobID: "job-1", Status: "planning", Summary: "Plan work"}); err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Errorf("authorization = %q", got)
		}
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"state":"done"}`))
			return
		}
		if got := r.Header.Get("X-OpenDev-Event-ID"); got == "" {
			t.Error("missing event ID header")
		}
		if attempts.Add(1) == 1 {
			http.Error(w, "retry", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	deliverer := NewDeliverer(store, server.URL, "token", nil)
	deliverer.flush(context.Background())
	events := store.due(time.Now().UTC())
	if len(events) != 0 { // The failed event is delayed, but should still be persisted.
		if events[0].Attempts != 1 {
			t.Fatalf("attempts = %d, want 1", events[0].Attempts)
		}
	}
	store.mu.Lock()
	for id, event := range store.events {
		event.NextAttemptAt = time.Now().UTC()
		store.events[id] = event
	}
	if err := store.saveLocked(); err != nil {
		store.mu.Unlock()
		t.Fatal(err)
	}
	store.mu.Unlock()
	deliverer.flush(context.Background())
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, event := range store.events {
		if event.DeliveredAt == nil || event.Attempts != 1 {
			t.Fatalf("unexpected delivery state: %#v", event)
		}
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2", attempts.Load())
	}
}

func TestDeliveryWaitsForProcessedEventAndRecordsTerminalFailure(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Enqueue(Event{Type: EventStarted, JobID: "job-1"}); err != nil {
		t.Fatal(err)
	}
	state := "pending"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("missing auth for %s", r.Method)
		}
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"state":%q,"last_error":"agent failed"}`, state)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	d := NewDeliverer(store, server.URL, "token", nil)
	d.flush(context.Background())
	id := eventID("job-1", EventStarted)
	if store.events[id].DeliveredAt != nil || store.events[id].Attempts != 1 {
		t.Fatalf("event was acknowledged before processing: %+v", store.events[id])
	}
	store.mu.Lock()
	event := store.events[id]
	event.NextAttemptAt = time.Now().UTC()
	store.events[id] = event
	store.mu.Unlock()
	state = "done"
	d.flush(context.Background())
	if store.events[id].DeliveredAt == nil {
		t.Fatal("processed event was not acknowledged")
	}
	if err := store.Enqueue(Event{Type: EventFailed, JobID: "job-2"}); err != nil {
		t.Fatal(err)
	}
	state = "failed"
	d.flush(context.Background())
	failedID := eventID("job-2", EventFailed)
	if !store.events[failedID].TerminalFailure || store.events[failedID].LastError == "" {
		t.Fatalf("failed event lost its terminal reason: %+v", store.events[failedID])
	}
	restarted, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !restarted.events[failedID].TerminalFailure || len(restarted.due(time.Now().Add(time.Hour))) != 0 {
		t.Fatalf("failed event redelivered after restart: %+v", restarted.events[failedID])
	}
}
