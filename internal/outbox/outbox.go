// Package outbox durably records OpenDev job notifications before attempting delivery.
package outbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/iamangus/code-mcp/internal/pipeline"
)

const (
	EventStarted       = "started"
	EventBlocked       = "blocked"
	EventPROpened      = "pr_opened"
	EventMerged        = "merged"
	EventFailed        = "failed"
	EventCancelled     = "cancelled"
	EventCIPending     = "ci_pending"
	EventCIFailed      = "ci_failed"
	EventCIRemediating = "ci_remediating"
	EventCIPassed      = "ci_passed"
	EventCIBlocked     = "ci_blocked"
)

type Event struct {
	ID              string     `json:"id"`
	Type            string     `json:"type"`
	JobID           string     `json:"job_id"`
	Status          string     `json:"status"`
	Summary         string     `json:"summary"`
	PullRequestURL  string     `json:"pull_request_url,omitempty"`
	Errors          string     `json:"errors,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	Attempts        int        `json:"attempts"`
	NextAttemptAt   time.Time  `json:"next_attempt_at"`
	DeliveredAt     *time.Time `json:"delivered_at,omitempty"`
	LastError       string     `json:"last_error,omitempty"`
	TerminalFailure bool       `json:"terminal_failure,omitempty"`
}

var errEventFailed = errors.New("Eve agent processing failed")

type persisted struct {
	Events []Event `json:"events"`
}

type Store struct {
	path   string
	mu     sync.Mutex
	events map[string]Event
}

func New(stateDir string) (*Store, error) {
	if stateDir == "" {
		return nil, fmt.Errorf("outbox state directory is required")
	}
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		return nil, fmt.Errorf("create outbox state directory: %w", err)
	}
	s := &Store{path: filepath.Join(stateDir, "notification-outbox.json"), events: make(map[string]Event)}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Enqueue records an event exactly once. A stable key makes repeated pipeline application safe.
func (s *Store) Enqueue(event Event) error {
	if strings.TrimSpace(event.JobID) == "" || strings.TrimSpace(event.Type) == "" {
		return fmt.Errorf("outbox event job ID and type are required")
	}
	if event.ID == "" {
		event.ID = eventID(event.JobID, event.Type)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.events[event.ID]; ok {
		return nil
	}
	now := time.Now().UTC()
	event.CreatedAt, event.NextAttemptAt = now, now
	s.events[event.ID] = event
	if err := s.saveLocked(); err != nil {
		delete(s.events, event.ID)
		return err
	}
	return nil
}

func (s *Store) due(now time.Time) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var events []Event
	for _, event := range s.events {
		if event.DeliveredAt == nil && !event.TerminalFailure && !event.NextAttemptAt.After(now) {
			events = append(events, event)
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].CreatedAt.Before(events[j].CreatedAt) })
	return events
}

func (s *Store) delivered(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	event, ok := s.events[id]
	if !ok {
		return nil
	}
	previous := event
	now := time.Now().UTC()
	event.DeliveredAt = &now
	event.LastError = ""
	s.events[id] = event
	if err := s.saveLocked(); err != nil {
		s.events[id] = previous
		return err
	}
	return nil
}

func (s *Store) failed(id string, err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	event, ok := s.events[id]
	if !ok {
		return nil
	}
	previous := event
	event.Attempts++
	event.LastError = err.Error()
	delay := time.Second * time.Duration(1<<min(event.Attempts-1, 8))
	event.NextAttemptAt = time.Now().UTC().Add(delay)
	s.events[id] = event
	if err := s.saveLocked(); err != nil {
		s.events[id] = previous
		return err
	}
	return nil
}

func (s *Store) failedTerminal(id string, reason error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	event, ok := s.events[id]
	if !ok {
		return nil
	}
	previous := event
	event.Attempts++
	event.TerminalFailure = true
	event.LastError = reason.Error()
	s.events[id] = event
	if err := s.saveLocked(); err != nil {
		s.events[id] = previous
		return err
	}
	return nil
}

// Reconcile restores notifications whose state transition was persisted before a process crash.
func (s *Store) Reconcile(jobs []*pipeline.Job) error {
	for _, job := range jobs {
		if job == nil {
			continue
		}
		if job.Status != pipeline.JobCreated {
			if err := s.Enqueue(eventFor(job, EventStarted, "", "")); err != nil {
				return err
			}
		}
		if job.PullRequestURL != "" {
			if err := s.Enqueue(eventFor(job, EventPROpened, "", "")); err != nil {
				return err
			}
		}
		if job.MergeState == pipeline.MergeMerged {
			if err := s.Enqueue(eventFor(job, EventMerged, "", "")); err != nil {
				return err
			}
		}
		if job.Status == pipeline.JobFailed {
			if err := s.Enqueue(eventFor(job, EventFailed, "", job.Failure)); err != nil {
				return err
			}
		}
		if job.Plan != nil {
			for _, task := range job.Plan.Tasks {
				if task.Status == pipeline.TaskBlocked {
					if err := s.Enqueue(eventFor(job, EventBlocked+":"+task.Key, task.BlockReason, "")); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// Notify is intentionally best-effort: a notification persistence failure must not affect a job.
func Notify(store *Store, job *pipeline.Job, eventType, summary, eventError string, logger *slog.Logger) {
	if store == nil || job == nil {
		return
	}
	if err := store.Enqueue(eventFor(job, eventType, summary, eventError)); err != nil && logger != nil {
		logger.Warn("enqueue notification failed", "job_id", job.ID, "event", eventType, "error", err)
	}
}

func eventFor(job *pipeline.Job, eventType, summary, eventError string) Event {
	if summary == "" {
		summary = job.Directive
	}
	return Event{ID: eventID(job.ID, eventType), Type: strings.Split(eventType, ":")[0], JobID: job.ID, Status: string(job.Status), Summary: summary, PullRequestURL: job.PullRequestURL, Errors: eventError}
}

func eventID(jobID, eventType string) string {
	sum := sha256.Sum256([]byte(jobID + "\x00" + eventType))
	return hex.EncodeToString(sum[:])
}

type Deliverer struct {
	store      *Store
	jobs       *pipeline.Store
	url, token string
	client     *http.Client
	logger     *slog.Logger
}

func NewDeliverer(store *Store, url, token string, logger *slog.Logger, jobs ...*pipeline.Store) *Deliverer {
	d := &Deliverer{store: store, url: strings.TrimSpace(url), token: strings.TrimSpace(token), client: &http.Client{Timeout: 10 * time.Second}, logger: logger}
	if len(jobs) > 0 {
		d.jobs = jobs[0]
	}
	return d
}

// Start keeps delivery independent of request and agent execution paths.
func (d *Deliverer) Start(ctx context.Context) {
	if d.store == nil || d.url == "" || d.token == "" {
		return
	}
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		reconcileTicker := time.NewTicker(30 * time.Second)
		defer reconcileTicker.Stop()
		d.reconcile()
		d.flush(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				d.flush(ctx)
			case <-reconcileTicker.C:
				d.reconcile()
			}
		}
	}()
}

func (d *Deliverer) reconcile() {
	if d.store == nil || d.jobs == nil {
		return
	}
	if err := d.store.Reconcile(d.jobs.List()); err != nil && d.logger != nil {
		d.logger.Warn("reconcile notification outbox", "error", err)
	}
}

func (d *Deliverer) flush(ctx context.Context) {
	for _, event := range d.store.due(time.Now().UTC()) {
		if err := d.deliver(ctx, event); err != nil {
			if errors.Is(err, errEventFailed) {
				if saveErr := d.store.failedTerminal(event.ID, err); saveErr != nil && d.logger != nil {
					d.logger.Warn("record terminal notification failure failed", "event_id", event.ID, "error", saveErr)
				}
				continue
			}
			if saveErr := d.store.failed(event.ID, err); saveErr != nil && d.logger != nil {
				d.logger.Warn("record notification delivery failure failed", "event_id", event.ID, "error", saveErr)
			}
			continue
		}
		if err := d.store.delivered(event.ID); err != nil && d.logger != nil {
			d.logger.Warn("record notification delivery failed", "event_id", event.ID, "error", err)
		}
	}
}

func (d *Deliverer) deliver(ctx context.Context, event Event) error {
	body, err := json.Marshal(struct {
		ID       string `json:"id"`
		Category string `json:"category"`
		Payload  Event  `json:"payload"`
	}{ID: event.ID, Category: "backend", Payload: event})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+d.token)
	req.Header.Set("X-OpenDev-Event-ID", event.ID)
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned %s", resp.Status)
	}
	statusURL := strings.TrimSuffix(d.url, "/") + "/events/" + url.PathEscape(event.ID)
	statusRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, statusURL, nil)
	if err != nil {
		return err
	}
	statusRequest.Header.Set("Authorization", "Bearer "+d.token)
	statusResponse, err := d.client.Do(statusRequest)
	if err != nil {
		return err
	}
	defer statusResponse.Body.Close()
	if statusResponse.StatusCode != http.StatusOK {
		return fmt.Errorf("Eve event status returned %s", statusResponse.Status)
	}
	var status struct {
		State     string `json:"state"`
		LastError string `json:"last_error"`
	}
	if err := json.NewDecoder(statusResponse.Body).Decode(&status); err != nil {
		return fmt.Errorf("decode Eve event status: %w", err)
	}
	switch status.State {
	case "done":
		return nil
	case "failed":
		return fmt.Errorf("%w: %s", errEventFailed, status.LastError)
	case "pending", "leased":
		return fmt.Errorf("Eve event is %s", status.State)
	default:
		return fmt.Errorf("unknown Eve event state %q", status.State)
	}
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read outbox state: %w", err)
	}
	var saved persisted
	if err := json.Unmarshal(data, &saved); err != nil {
		return fmt.Errorf("decode outbox state: %w", err)
	}
	for _, event := range saved.Events {
		if event.ID == "" {
			return fmt.Errorf("decode outbox state: event without ID")
		}
		s.events[event.ID] = event
	}
	return nil
}
func (s *Store) saveLocked() error {
	events := make([]Event, 0, len(s.events))
	for _, event := range s.events {
		events = append(events, event)
	}
	sort.Slice(events, func(i, j int) bool { return events[i].ID < events[j].ID })
	data, err := json.MarshalIndent(persisted{Events: events}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode outbox state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".notification-outbox-*.tmp")
	if err != nil {
		return fmt.Errorf("create outbox state temp file: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write outbox state: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, s.path); err != nil {
		return fmt.Errorf("replace outbox state: %w", err)
	}
	return nil
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
