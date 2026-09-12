// Package pipeline persists the deterministic state of a coding job. Agent
// output proposes state; this package validates and records it.
package pipeline

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type JobStatus string

const (
	JobCreated           JobStatus = "created"
	JobPlanning          JobStatus = "planning"
	JobPlanned           JobStatus = "planned"
	JobWorking           JobStatus = "working"
	JobReviewing         JobStatus = "reviewing"
	JobIntegrating       JobStatus = "integrating"
	JobHolisticReviewing JobStatus = "holistic_reviewing"
	JobReadyToPublish    JobStatus = "ready_to_publish"
	JobPublished         JobStatus = "published"
	JobNoChanges         JobStatus = "no_changes"
	JobFailed            JobStatus = "failed"
)

type TaskStatus string

const (
	TaskPlanned             TaskStatus = "planned"
	TaskWorking             TaskStatus = "working"
	TaskReviewing           TaskStatus = "reviewing"
	TaskChangesRequested    TaskStatus = "changes_requested"
	TaskApproved            TaskStatus = "approved"
	TaskIntegrationEligible TaskStatus = "integration_eligible"
	TaskIntegrating         TaskStatus = "integrating"
	TaskIntegrated          TaskStatus = "integrated"
	TaskNoChanges           TaskStatus = "no_changes"
	TaskBlocked             TaskStatus = "blocked"
)

type ReviewVerdict string

const (
	ReviewPending          ReviewVerdict = "pending"
	ReviewApproved         ReviewVerdict = "approved"
	ReviewChangesRequested ReviewVerdict = "changes_requested"
	ReviewBlocked          ReviewVerdict = "blocked"
)

type MergeState string

const (
	MergeNotRequested MergeState = "not_requested"
	MergeOpen         MergeState = "open"
	MergeMerged       MergeState = "merged"
)

// ValidationEvidence is an immutable record of validation reported for a task.
type ValidationEvidence struct {
	Command string `json:"command"`
	Result  string `json:"result"`
	RunID   string `json:"run_id,omitempty"`
}

type Task struct {
	Key                string               `json:"key"`
	Title              string               `json:"title"`
	Description        string               `json:"description"`
	AcceptanceCriteria []string             `json:"acceptance_criteria"`
	DependsOn          []string             `json:"depends_on"`
	Validation         []string             `json:"validation"`
	Status             TaskStatus           `json:"status"`
	Branch             string               `json:"branch,omitempty"`
	WorktreePath       string               `json:"worktree_path,omitempty"`
	WriterAttempts     int                  `json:"writer_attempts"`
	ReviewerAttempts   int                  `json:"reviewer_attempts"`
	WriterRunID        string               `json:"writer_run_id,omitempty"`
	ReviewerRunID      string               `json:"reviewer_run_id,omitempty"`
	CommitSHA          string               `json:"commit_sha,omitempty"`
	IntegrationSHA     string               `json:"integration_sha,omitempty"`
	ReviewVerdict      ReviewVerdict        `json:"review_verdict"`
	ValidationEvidence []ValidationEvidence `json:"validation_evidence,omitempty"`
	NoChangeReason     string               `json:"no_change_reason,omitempty"`
	BlockReason        string               `json:"block_reason,omitempty"`
}

// ReferenceRepository supplies context for planning without becoming a task worktree.
type ReferenceRepository struct {
	Repository string `json:"repository"`
	Branch     string `json:"branch,omitempty"`
	Revision   string `json:"revision,omitempty"`
	Purpose    string `json:"purpose"`
}

type Plan struct {
	Summary               string                `json:"summary"`
	AcceptanceCriteria    []string              `json:"acceptance_criteria"`
	Tasks                 []Task                `json:"tasks"`
	IntegrationOrder      []string              `json:"integration_order"`
	Risks                 []string              `json:"risks"`
	ReferenceRepositories []ReferenceRepository `json:"reference_repositories"`
}

// ReferenceSnapshot is an immutable, detached local view of an affiliated repo.
type ReferenceSnapshot struct {
	Repository string `json:"repository"`
	SHA        string `json:"sha"`
	Path       string `json:"path"`
	Purpose    string `json:"purpose"`
}

type Job struct {
	ID                    string              `json:"id"`
	Repository            string              `json:"repository"`
	Directive             string              `json:"directive"`
	TargetBranch          string              `json:"target_branch"`
	IntegrationBranch     string              `json:"integration_branch"`
	OrchestratorAgentID   string              `json:"orchestrator_agent_id"`
	PlannerAgentID        string              `json:"planner_agent_id"`
	WriterAgentID         string              `json:"writer_agent_id"`
	ReviewerAgentID       string              `json:"reviewer_agent_id"`
	HolisticAgentID       string              `json:"holistic_agent_id"`
	Status                JobStatus           `json:"status"`
	Plan                  *Plan               `json:"plan,omitempty"`
	ReferenceSnapshots    []ReferenceSnapshot `json:"reference_snapshots,omitempty"`
	IntegrationSHA        string              `json:"integration_sha,omitempty"`
	HolisticReviewVerdict ReviewVerdict       `json:"holistic_review_verdict"`
	HolisticReviewRunID   string              `json:"holistic_review_run_id,omitempty"`
	HolisticReviewSHA     string              `json:"holistic_review_sha,omitempty"`
	PullRequestURL        string              `json:"pull_request_url,omitempty"`
	PullRequestNumber     int                 `json:"pull_request_number,omitempty"`
	MergeState            MergeState          `json:"merge_state"`
	Failure               string              `json:"failure,omitempty"`
	CreatedAt             time.Time           `json:"created_at"`
	UpdatedAt             time.Time           `json:"updated_at"`
}

// BlockTask records a terminal agent failure without executing another pipeline side effect.
func (s *Store) BlockTask(jobID, taskKey, reason string) (*Job, error) {
	if strings.TrimSpace(reason) == "" {
		reason = "agent reported a blocked outcome"
	}
	return s.updateTask(jobID, taskKey, func(_ *Job, task *Task) error {
		if task.Status == TaskBlocked {
			if task.BlockReason == "" {
				task.BlockReason = reason
			}
			return nil
		}
		task.Status, task.BlockReason = TaskBlocked, reason
		return nil
	})
}

// FailJob records a terminal planner or holistic failure.
func (s *Store) FailJob(jobID, reason string) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok {
		return nil, ErrNotFound
	}
	if job.Status == JobFailed {
		return cloneJob(job), nil
	}
	job.Status, job.Failure = JobFailed, reason
	return s.saveAndCloneLocked(job)
}

type persisted struct {
	Jobs []*Job `json:"jobs"`
}

type Store struct {
	path string
	mu   sync.RWMutex
	jobs map[string]*Job
}

var (
	ErrNotFound          = errors.New("coding job not found")
	ErrInvalidPlan       = errors.New("invalid coding plan")
	ErrPlanExists        = errors.New("coding job already has a plan")
	ErrInvalidJob        = errors.New("invalid coding job")
	ErrInvalidTransition = errors.New("invalid pipeline transition")
	ErrDependencyGate    = errors.New("integration prerequisites are not integrated")
)

func NewStore(dataDir string) (*Store, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("pipeline data directory is required")
	}
	if err := os.MkdirAll(dataDir, 0750); err != nil {
		return nil, fmt.Errorf("create pipeline data directory: %w", err)
	}
	s := &Store{path: filepath.Join(dataDir, "jobs.json"), jobs: make(map[string]*Job)}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Create(repository, directive, targetBranch, orchestratorID, plannerID, writerID, reviewerID string, holisticAgentID ...string) (*Job, error) {
	if strings.TrimSpace(repository) == "" || strings.TrimSpace(directive) == "" {
		return nil, fmt.Errorf("%w: repository and directive are required", ErrInvalidJob)
	}
	if len(holisticAgentID) > 1 {
		return nil, fmt.Errorf("%w: at most one holistic agent ID is allowed", ErrInvalidJob)
	}
	if targetBranch == "" {
		targetBranch = "main"
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	job := &Job{
		ID: id, Repository: repository, Directive: directive, TargetBranch: targetBranch,
		IntegrationBranch: "opendev/job-" + id, OrchestratorAgentID: orchestratorID,
		PlannerAgentID: plannerID, WriterAgentID: writerID, ReviewerAgentID: reviewerID,
		Status: JobCreated, CreatedAt: now, UpdatedAt: now,
		HolisticReviewVerdict: ReviewPending, MergeState: MergeNotRequested,
	}
	if len(holisticAgentID) == 1 {
		job.HolisticAgentID = holisticAgentID[0]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[id] = job
	if err := s.saveLocked(); err != nil {
		delete(s.jobs, id)
		return nil, err
	}
	return cloneJob(job), nil
}

func (s *Store) Get(id string) (*Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	job, ok := s.jobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneJob(job), nil
}

func (s *Store) List() []*Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	jobs := make([]*Job, 0, len(s.jobs))
	for _, job := range s.jobs {
		jobs = append(jobs, cloneJob(job))
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].CreatedAt.After(jobs[j].CreatedAt) })
	return jobs
}

func (s *Store) StartPlanning(id string) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	if job.Status != JobCreated {
		return nil, fmt.Errorf("%w: job is %s, expected %s", ErrInvalidJob, job.Status, JobCreated)
	}
	job.Status, job.UpdatedAt = JobPlanning, time.Now().UTC()
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return cloneJob(job), nil
}

func (s *Store) SubmitPlan(id string, plan Plan) (*Job, error) {
	if err := validatePlan(plan); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	if job.Plan != nil {
		return nil, ErrPlanExists
	}
	if job.Status != JobPlanning {
		return nil, fmt.Errorf("%w: job is %s, expected %s", ErrInvalidJob, job.Status, JobPlanning)
	}
	for i := range plan.Tasks {
		plan.Tasks[i].Status = TaskPlanned
		plan.Tasks[i].ReviewVerdict = ReviewPending
	}
	job.Plan, job.Status, job.UpdatedAt = &plan, JobPlanned, time.Now().UTC()
	if err := s.saveLocked(); err != nil {
		job.Plan, job.Status = nil, JobPlanning
		return nil, err
	}
	return cloneJob(job), nil
}

// RecordReferenceSnapshots persists the exact read-only repositories a plan may use.
func (s *Store) RecordReferenceSnapshots(id string, snapshots []ReferenceSnapshot) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	if job.Plan == nil {
		return nil, fmt.Errorf("%w: job has no plan", ErrInvalidTransition)
	}
	for _, snapshot := range snapshots {
		if strings.TrimSpace(snapshot.Repository) == "" || strings.TrimSpace(snapshot.SHA) == "" || strings.TrimSpace(snapshot.Path) == "" || strings.TrimSpace(snapshot.Purpose) == "" {
			return nil, fmt.Errorf("%w: invalid reference snapshot", ErrInvalidTransition)
		}
	}
	job.ReferenceSnapshots = append([]ReferenceSnapshot(nil), snapshots...)
	job.UpdatedAt = time.Now().UTC()
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return cloneJob(job), nil
}

func validatePlan(plan Plan) error {
	if strings.TrimSpace(plan.Summary) == "" || len(plan.Tasks) == 0 {
		return fmt.Errorf("%w: summary and at least one task are required", ErrInvalidPlan)
	}
	keys := make(map[string]struct{}, len(plan.Tasks))
	for _, task := range plan.Tasks {
		if task.Key == "" || task.Title == "" || task.Description == "" || len(task.AcceptanceCriteria) == 0 {
			return fmt.Errorf("%w: each task needs key, title, description, and acceptance criteria", ErrInvalidPlan)
		}
		if _, exists := keys[task.Key]; exists {
			return fmt.Errorf("%w: duplicate task key %q", ErrInvalidPlan, task.Key)
		}
		keys[task.Key] = struct{}{}
	}
	references := make(map[string]struct{}, len(plan.ReferenceRepositories))
	for _, reference := range plan.ReferenceRepositories {
		repository := strings.TrimSpace(reference.Repository)
		if repository == "" || strings.TrimSpace(reference.Purpose) == "" {
			return fmt.Errorf("%w: each reference repository needs repository and purpose", ErrInvalidPlan)
		}
		if _, exists := references[repository]; exists {
			return fmt.Errorf("%w: duplicate reference repository %q", ErrInvalidPlan, repository)
		}
		references[repository] = struct{}{}
	}
	for _, task := range plan.Tasks {
		for _, dependency := range task.DependsOn {
			if dependency == task.Key {
				return fmt.Errorf("%w: task %q depends on itself", ErrInvalidPlan, task.Key)
			}
			if _, exists := keys[dependency]; !exists {
				return fmt.Errorf("%w: task %q depends on unknown task %q", ErrInvalidPlan, task.Key, dependency)
			}
		}
	}
	if len(plan.IntegrationOrder) != len(plan.Tasks) {
		return fmt.Errorf("%w: integration order must contain every task exactly once", ErrInvalidPlan)
	}
	seen := make(map[string]struct{}, len(plan.IntegrationOrder))
	order := make(map[string]int, len(plan.IntegrationOrder))
	for index, key := range plan.IntegrationOrder {
		if _, exists := keys[key]; !exists {
			return fmt.Errorf("%w: integration order references unknown task %q", ErrInvalidPlan, key)
		}
		if _, exists := seen[key]; exists {
			return fmt.Errorf("%w: integration order repeats task %q", ErrInvalidPlan, key)
		}
		seen[key] = struct{}{}
		order[key] = index
	}
	for _, task := range plan.Tasks {
		for _, dependency := range task.DependsOn {
			if order[dependency] >= order[task.Key] {
				return fmt.Errorf("%w: task %q must follow dependency %q in integration order", ErrInvalidPlan, task.Key, dependency)
			}
		}
	}
	return nil
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read pipeline state: %w", err)
	}
	var saved persisted
	if err := json.Unmarshal(data, &saved); err != nil {
		return fmt.Errorf("decode pipeline state: %w", err)
	}
	for _, job := range saved.Jobs {
		if job == nil || job.ID == "" {
			return fmt.Errorf("decode pipeline state: invalid job")
		}
		s.jobs[job.ID] = job
	}
	return nil
}

func (s *Store) saveLocked() error {
	jobs := make([]*Job, 0, len(s.jobs))
	for _, job := range s.jobs {
		jobs = append(jobs, job)
	}
	data, err := json.MarshalIndent(persisted{Jobs: jobs}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode pipeline state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".jobs-*.tmp")
	if err != nil {
		return fmt.Errorf("create pipeline state temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write pipeline state: %w", err)
	}
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return fmt.Errorf("set pipeline state permissions: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync pipeline state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close pipeline state: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("replace pipeline state: %w", err)
	}
	return nil
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate job id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func cloneJob(job *Job) *Job {
	data, _ := json.Marshal(job)
	var clone Job
	_ = json.Unmarshal(data, &clone)
	return &clone
}
