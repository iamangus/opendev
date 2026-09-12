package outbox

import (
	"context"
	"net/http"
	"net/http/httptest"
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
