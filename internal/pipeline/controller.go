package pipeline

import (
	"fmt"
	"strings"
	"time"
)

// WorktreeProvisioner performs only filesystem and Git setup.
type WorktreeProvisioner interface {
	CreateWorktree(repository, branch, base string) (string, error)
}

// Committer records a commit for already prepared work; it makes no content decisions.
type Committer interface {
	Commit(worktreePath string) (string, error)
}

// Integrator performs a requested merge and returns the resulting integration SHA.
type Integrator interface {
	MergeTask(repository, sourceBranch, targetBranch string) (string, error)
}

// Controller exposes deterministic state transitions. Dispatchers may use its
// persisted intent and the narrow execution interfaces above to perform side effects.
type Controller struct{ store *Store }

func NewController(store *Store) *Controller { return &Controller{store: store} }

func (c *Controller) StartTaskWork(jobID, taskKey, branch, worktreePath, runID string) (*Job, error) {
	return c.store.startTaskWork(jobID, taskKey, branch, worktreePath, runID)
}

func (c *Controller) RecordTaskBase(jobID, taskKey, baseSHA string) (*Job, error) {
	return c.store.updateTask(jobID, taskKey, func(_ *Job, task *Task) error {
		if strings.TrimSpace(baseSHA) == "" {
			return fmt.Errorf("%w: base SHA is required", ErrInvalidTransition)
		}
		if task.BaseSHA != "" && task.BaseSHA != baseSHA {
			return fmt.Errorf("%w: task base SHA is immutable", ErrInvalidTransition)
		}
		task.BaseSHA = baseSHA
		return nil
	})
}

func (c *Controller) RecordWriterCompletion(jobID, taskKey, runID, commitSHA string, evidence []ValidationEvidence) (*Job, error) {
	return c.store.recordWriterCompletion(jobID, taskKey, runID, commitSHA, evidence)
}

func (c *Controller) RecordNoChanges(jobID, taskKey, runID, reason string, evidence []ValidationEvidence) (*Job, error) {
	return c.store.recordNoChanges(jobID, taskKey, runID, reason, evidence)
}

func (c *Controller) RecordReview(jobID, taskKey, runID string, verdict ReviewVerdict) (*Job, error) {
	return c.store.recordReview(jobID, taskKey, runID, verdict)
}

// RecordReviewReport saves the complete reviewer decision before advancing the task.
func (c *Controller) RecordReviewReport(jobID, taskKey, runID string, report ReviewReport) (*Job, error) {
	return c.store.recordReviewReport(jobID, taskKey, runID, report)
}

// StartReviewer records the exact reviewer run before it can submit a verdict.
func (c *Controller) StartReviewer(jobID, taskKey, runID string) (*Job, error) {
	return c.store.startReviewer(jobID, taskKey, runID)
}

func (c *Controller) StartIntegration(jobID, taskKey string) (*Job, error) {
	return c.store.startIntegration(jobID, taskKey)
}

func (c *Controller) RecordIntegration(jobID, taskKey, integrationSHA string) (*Job, error) {
	return c.store.recordIntegration(jobID, taskKey, integrationSHA)
}

func (c *Controller) RecordHolisticReview(jobID, runID, integrationSHA string, verdict ReviewVerdict, content PullRequestContent) (*Job, error) {
	return c.store.recordHolisticReview(jobID, runID, integrationSHA, verdict, content)
}

// StartHolisticReviewer records the exact reviewer run and reviewed SHA.
func (c *Controller) StartHolisticReviewer(jobID, runID, integrationSHA string) (*Job, error) {
	return c.store.startHolisticReviewer(jobID, runID, integrationSHA)
}

func (c *Controller) StartHolisticRereview(jobID, integrationSHA string) (*Job, error) {
	return c.store.startHolisticRereview(jobID, integrationSHA)
}

func (c *Controller) RecordPullRequest(jobID string, number int, url string) (*Job, error) {
	return c.store.recordPullRequest(jobID, number, url)
}

// StartCI records the exact draft-PR head that GitHub Actions must validate.
func (c *Controller) StartCI(jobID, sha string, required []string) (*Job, error) {
	return c.store.startCI(jobID, sha, required)
}

// RecordCI records check results only for the currently awaited PR head.
func (c *Controller) RecordCI(jobID, sha string, state CIState, checks []CheckResult, fingerprint string) (*Job, error) {
	return c.store.recordCI(jobID, sha, state, checks, fingerprint)
}

func (c *Controller) StartCIRemediation(jobID, fingerprint string, checks []CheckResult) (*Job, *Task, error) {
	return c.store.startCIRemediation(jobID, fingerprint, checks)
}

func (c *Controller) RecordMerge(jobID string) (*Job, error) { return c.store.recordMerge(jobID) }

func (c *Controller) BlockTask(jobID, taskKey, reason string) (*Job, error) {
	return c.store.BlockTask(jobID, taskKey, reason)
}

// RetryWriter releases a task whose Writer result could not be applied.
func (c *Controller) RetryWriter(jobID, taskKey, runID string) (*Job, error) {
	return c.store.updateTask(jobID, taskKey, func(job *Job, task *Task) error {
		if task.Status != TaskWorking || task.WriterRunID != runID {
			return transition(task.Status, "retry writer")
		}
		task.WriterRunID = ""
		task.Status = TaskPlanned
		job.Status = JobPlanned
		return nil
	})
}

// ResumeBlockedWriter re-opens a task its Writer reported blocked. The block
// reason stays in durable history; a fresh Writer attempt decides anew.
func (c *Controller) ResumeBlockedWriter(jobID, taskKey string) (*Job, error) {
	return c.store.updateTask(jobID, taskKey, func(job *Job, task *Task) error {
		if task.Status != TaskBlocked || task.WriterRunID == "" {
			return transition(task.Status, "resume blocked writer")
		}
		task.WriterRunID = ""
		task.BlockReason = ""
		task.Status = TaskPlanned
		job.Status = JobPlanned
		return nil
	})
}

// ReopenNoChangesTask re-opens a task its Writer reported as needing no
// changes, for example after a holistic review found the work incomplete.
func (c *Controller) ReopenNoChangesTask(jobID, taskKey string) (*Job, error) {
	return c.store.updateTask(jobID, taskKey, func(job *Job, task *Task) error {
		if task.Status != TaskNoChanges {
			return transition(task.Status, "reopen no-changes task")
		}
		task.WriterRunID = ""
		task.BlockReason = ""
		task.Status = TaskPlanned
		job.Status = JobPlanned
		return nil
	})
}

func (c *Controller) FailJob(jobID, reason string) (*Job, error) {
	return c.store.FailJob(jobID, reason)
}

func (c *Controller) RecordReferenceSnapshots(jobID string, snapshots []ReferenceSnapshot) (*Job, error) {
	return c.store.RecordReferenceSnapshots(jobID, snapshots)
}

func (s *Store) startTaskWork(jobID, taskKey, branch, worktreePath, runID string) (*Job, error) {
	if strings.TrimSpace(worktreePath) == "" || strings.TrimSpace(runID) == "" {
		return nil, fmt.Errorf("%w: worktree path and writer run ID are required", ErrInvalidTransition)
	}
	return s.updateTask(jobID, taskKey, func(job *Job, task *Task) error {
		if task.Status == TaskWorking && task.WriterRunID == runID {
			return nil
		}
		if task.Status != TaskPlanned && task.Status != TaskChangesRequested && task.Status != TaskBlocked {
			return transition(task.Status, "start work")
		}
		if strings.TrimSpace(branch) == "" {
			branch = job.IntegrationBranch + "/" + task.Key
		}
		task.Status, task.Branch, task.WorktreePath, task.WriterRunID = TaskWorking, branch, worktreePath, runID
		// A revision replaces the prior review decision and its run ownership.
		task.ReviewerRunID, task.ReviewVerdict = "", ReviewPending
		task.WriterAttempts++
		job.Status = JobWorking
		return nil
	})
}

func (s *Store) recordWriterCompletion(jobID, taskKey, runID, commitSHA string, evidence []ValidationEvidence) (*Job, error) {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(commitSHA) == "" {
		return nil, fmt.Errorf("%w: writer run ID and commit SHA are required", ErrInvalidTransition)
	}
	return s.updateTask(jobID, taskKey, func(job *Job, task *Task) error {
		if task.Status == TaskReviewing && task.WriterRunID == runID && task.CommitSHA == commitSHA {
			return nil
		}
		if task.Status != TaskWorking || task.WriterRunID != runID {
			return transition(task.Status, "record writer completion")
		}
		task.Status, task.CommitSHA, task.ValidationEvidence = TaskReviewing, commitSHA, append([]ValidationEvidence(nil), evidence...)
		task.ReviewVerdict = ReviewPending
		job.Status = JobReviewing
		return nil
	})
}

func (s *Store) recordNoChanges(jobID, taskKey, runID, reason string, evidence []ValidationEvidence) (*Job, error) {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("%w: writer run ID and no-change reason are required", ErrInvalidTransition)
	}
	return s.updateTask(jobID, taskKey, func(job *Job, task *Task) error {
		if task.Status == TaskNoChanges && task.WriterRunID == runID {
			return nil
		}
		if task.Status != TaskWorking || task.WriterRunID != runID {
			return transition(task.Status, "record no-change writer completion")
		}
		task.Status = TaskNoChanges
		task.NoChangeReason = reason
		task.ValidationEvidence = append([]ValidationEvidence(nil), evidence...)
		if allTasksIntegrated(job.Plan) {
			if hasIntegratedTask(job.Plan) {
				job.Status = JobHolisticReviewing
			} else {
				job.Status = JobNoChanges
			}
		}
		return nil
	})
}

func (s *Store) recordReview(jobID, taskKey, runID string, verdict ReviewVerdict) (*Job, error) {
	if strings.TrimSpace(runID) == "" || (verdict != ReviewApproved && verdict != ReviewChangesRequested && verdict != ReviewBlocked) {
		return nil, fmt.Errorf("%w: valid reviewer run ID and verdict are required", ErrInvalidTransition)
	}
	return s.updateTask(jobID, taskKey, func(job *Job, task *Task) error {
		if task.ReviewerRunID == runID && task.ReviewVerdict == verdict && task.Status != TaskReviewing {
			return nil
		}
		// Recover jobs written before revision work cleared the superseded reviewer.
		// A later writer attempt proves this terminal reviewer belongs to the next review.
		if task.Status == TaskReviewing && task.ReviewerRunID != runID && task.ReviewerAttempts < task.WriterAttempts {
			task.ReviewerRunID = runID
			task.ReviewerAttempts++
		}
		if task.Status != TaskReviewing || (task.ReviewerRunID != "" && task.ReviewerRunID != runID) {
			return transition(task.Status, "record review")
		}
		if task.ReviewerRunID == "" {
			task.ReviewerRunID = runID
			task.ReviewerAttempts++
		}
		task.ReviewVerdict = verdict
		switch verdict {
		case ReviewApproved:
			task.Status = TaskApproved
			if integrationPrerequisitesMet(job.Plan, task) {
				task.Status = TaskIntegrationEligible
			}
		case ReviewChangesRequested:
			task.Status = TaskChangesRequested
		case ReviewBlocked:
			task.Status = TaskBlocked
		}
		return nil
	})
}

func (s *Store) recordReviewReport(jobID, taskKey, runID string, report ReviewReport) (*Job, error) {
	if strings.TrimSpace(runID) == "" || report.Verdict != ReviewApproved && report.Verdict != ReviewChangesRequested && report.Verdict != ReviewBlocked {
		return nil, fmt.Errorf("%w: valid reviewer run ID and verdict are required", ErrInvalidTransition)
	}
	return s.updateTask(jobID, taskKey, func(job *Job, task *Task) error {
		// The report is recorded before transition validation so rejected outcomes
		// remain inspectable rather than surviving only in a dispatch log.
		if task.Status == TaskReviewing && task.ReviewerRunID != runID && task.ReviewerAttempts < task.WriterAttempts {
			task.ReviewerRunID = runID
			task.ReviewerAttempts++
		}
		report.RunID = runID
		report.WriterAttempt = task.WriterAttempts
		report.ReviewerAttempt = task.ReviewerAttempts
		report.ReviewedCommitSHA = task.CommitSHA
		report.CreatedAt = time.Now().UTC()
		if task.ReviewerRunID == runID {
			for _, existing := range task.ReviewHistory {
				if existing.RunID == runID {
					return nil
				}
			}
			task.LatestReview = &report
			task.ReviewHistory = append(task.ReviewHistory, report)
		}
		return nil
	})
}

func (s *Store) startReviewer(jobID, taskKey, runID string) (*Job, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, fmt.Errorf("%w: reviewer run ID is required", ErrInvalidTransition)
	}
	return s.updateTask(jobID, taskKey, func(_ *Job, task *Task) error {
		if task.Status == TaskReviewing && task.ReviewerRunID == runID {
			return nil
		}
		if task.Status != TaskReviewing || (task.ReviewerRunID != "" && task.ReviewerRunID != runID) {
			return transition(task.Status, "start reviewer")
		}
		task.ReviewerRunID = runID
		task.ReviewerAttempts++
		return nil
	})
}

func (s *Store) startIntegration(jobID, taskKey string) (*Job, error) {
	return s.updateTask(jobID, taskKey, func(job *Job, task *Task) error {
		if task.Status != TaskIntegrationEligible {
			return transition(task.Status, "start integration")
		}
		if !integrationPrerequisitesMet(job.Plan, task) {
			return ErrDependencyGate
		}
		task.Status, job.Status = TaskIntegrating, JobIntegrating
		return nil
	})
}

func (s *Store) recordIntegration(jobID, taskKey, integrationSHA string) (*Job, error) {
	if strings.TrimSpace(integrationSHA) == "" {
		return nil, fmt.Errorf("%w: integration SHA is required", ErrInvalidTransition)
	}
	return s.updateTask(jobID, taskKey, func(job *Job, task *Task) error {
		if task.Status != TaskIntegrating {
			return transition(task.Status, "record integration")
		}
		task.Status, task.IntegrationSHA = TaskIntegrated, integrationSHA
		for i := range job.Plan.Tasks {
			candidate := &job.Plan.Tasks[i]
			if candidate.Status == TaskApproved && integrationPrerequisitesMet(job.Plan, candidate) {
				candidate.Status = TaskIntegrationEligible
			}
		}
		job.IntegrationSHA = integrationSHA
		if allTasksIntegrated(job.Plan) {
			job.Status = JobHolisticReviewing
		}
		return nil
	})
}

func (s *Store) recordHolisticReview(jobID, runID, integrationSHA string, verdict ReviewVerdict, content PullRequestContent) (*Job, error) {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(integrationSHA) == "" || (verdict != ReviewApproved && verdict != ReviewChangesRequested && verdict != ReviewBlocked) {
		return nil, fmt.Errorf("%w: valid holistic review is required", ErrInvalidTransition)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok {
		return nil, ErrNotFound
	}
	if (job.Status == JobReadyToPublish || job.Status == JobPublished) && job.HolisticReviewRunID == runID && job.HolisticReviewSHA == integrationSHA && job.HolisticReviewVerdict == verdict {
		return cloneJob(job), nil
	}
	if job.Status != JobHolisticReviewing || !allTasksIntegrated(job.Plan) || (job.HolisticReviewRunID != "" && job.HolisticReviewRunID != runID) {
		return nil, transition(job.Status, "record holistic review")
	}
	if job.IntegrationSHA != integrationSHA {
		return nil, fmt.Errorf("%w: holistic review SHA does not match integration", ErrInvalidTransition)
	}
	if job.HolisticReviewRunID == "" {
		job.HolisticReviewRunID = runID
		job.HolisticReviewSHA = integrationSHA
	}
	job.HolisticReviewVerdict = verdict
	if verdict == ReviewApproved {
		if strings.TrimSpace(content.Title) == "" || strings.TrimSpace(content.Body) == "" {
			return nil, fmt.Errorf("%w: approved holistic review requires pull request title and body", ErrInvalidTransition)
		}
		job.PullRequestTitle, job.PullRequestBody = strings.TrimSpace(content.Title), strings.TrimSpace(content.Body)
		job.Status = JobReadyToPublish
	}
	return s.saveAndCloneLocked(job)
}

// ResetHolisticRound clears a stale holistic verdict and run ownership after
// CI passed for a new integration head, so a fresh holistic round can be
// dispatched and recorded. The round count increments so each round owns a
// unique dispatch identity.
func (c *Controller) ResetHolisticRound(jobID string) (*Job, error) {
	return c.store.updateJob(jobID, func(job *Job) error {
		if job.Status != JobHolisticReviewing {
			return transition(job.Status, "reset holistic round")
		}
		job.HolisticRounds++
		job.HolisticReviewVerdict = ReviewPending
		job.HolisticReviewRunID = ""
		job.HolisticReviewSHA = ""
		return nil
	})
}

func (s *Store) startHolisticReviewer(jobID, runID, integrationSHA string) (*Job, error) {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(integrationSHA) == "" {
		return nil, fmt.Errorf("%w: holistic reviewer run ID and integration SHA are required", ErrInvalidTransition)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok {
		return nil, ErrNotFound
	}
	if job.Status != JobHolisticReviewing || !allTasksIntegrated(job.Plan) || job.IntegrationSHA != integrationSHA || (job.HolisticReviewRunID != "" && job.HolisticReviewRunID != runID) {
		return nil, transition(job.Status, "start holistic reviewer")
	}
	job.HolisticReviewRunID, job.HolisticReviewSHA = runID, integrationSHA
	return s.saveAndCloneLocked(job)
}

func (s *Store) startHolisticRereview(jobID, integrationSHA string) (*Job, error) {
	if strings.TrimSpace(integrationSHA) == "" {
		return nil, fmt.Errorf("%w: integration SHA is required", ErrInvalidTransition)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok {
		return nil, ErrNotFound
	}
	if job.Status == JobHolisticReviewing && job.IntegrationSHA == integrationSHA {
		return cloneJob(job), nil
	}
	if job.Status != JobReadyToPublish || job.MergeState != MergeOpen || job.PullRequestNumber == 0 {
		return nil, transition(job.Status, "start holistic re-review")
	}
	job.Status = JobHolisticReviewing
	job.IntegrationSHA = integrationSHA
	job.HolisticReviewRunID = ""
	job.HolisticReviewSHA = ""
	job.HolisticReviewVerdict = ReviewPending
	return s.saveAndCloneLocked(job)
}

func (s *Store) recordPullRequest(jobID string, number int, url string) (*Job, error) {
	if number <= 0 || strings.TrimSpace(url) == "" {
		return nil, fmt.Errorf("%w: pull request number and URL are required", ErrInvalidTransition)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok {
		return nil, ErrNotFound
	}
	if job.PullRequestNumber == number && job.PullRequestURL == url && job.MergeState != MergeNotRequested {
		return cloneJob(job), nil
	}
	if (job.Status != JobReadyToPublish && job.Status != JobHolisticReviewing) || job.PullRequestNumber != 0 || job.MergeState != MergeNotRequested {
		return nil, transition(job.Status, "record pull request")
	}
	job.PullRequestNumber, job.PullRequestURL, job.MergeState = number, url, MergeOpen
	return s.saveAndCloneLocked(job)
}

func (s *Store) recordMerge(jobID string) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok {
		return nil, ErrNotFound
	}
	if job.MergeState == MergeMerged && job.Status == JobPublished {
		return cloneJob(job), nil
	}
	if job.MergeState != MergeOpen || job.HolisticReviewVerdict != ReviewApproved {
		return nil, transition(job.Status, "record merge")
	}
	job.MergeState, job.Status = MergeMerged, JobPublished
	return s.saveAndCloneLocked(job)
}

// updateJob mutates job-level state under the store lock.
func (s *Store) updateJob(jobID string, update func(*Job) error) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok {
		return nil, ErrNotFound
	}
	if err := update(job); err != nil {
		return nil, err
	}
	return s.saveAndCloneLocked(job)
}

func (s *Store) updateTask(jobID, taskKey string, update func(*Job, *Task) error) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok {
		return nil, ErrNotFound
	}
	if job.Plan == nil {
		return nil, fmt.Errorf("%w: job has no plan", ErrInvalidTransition)
	}
	for i := range job.Plan.Tasks {
		if job.Plan.Tasks[i].Key == taskKey {
			if err := update(job, &job.Plan.Tasks[i]); err != nil {
				return nil, err
			}
			return s.saveAndCloneLocked(job)
		}
	}
	return nil, fmt.Errorf("%w: task %q", ErrNotFound, taskKey)
}

// ReopenIncompleteTasks returns tasks whose work the holistic review found
// insufficient (no_changes or writer-blocked) to planned for another attempt.
func (s *Store) ReopenIncompleteTasks(jobID string) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok {
		return nil, ErrNotFound
	}
	if job.Plan == nil {
		return nil, fmt.Errorf("%w: job has no plan", ErrInvalidTransition)
	}
	reopened := 0
	for i := range job.Plan.Tasks {
		task := &job.Plan.Tasks[i]
		switch task.Status {
		case TaskNoChanges:
			task.Status = TaskPlanned
			task.NoChangeReason = ""
			task.WriterRunID = ""
			reopened++
		case TaskBlocked:
			if task.WriterRunID != "" {
				task.Status = TaskPlanned
				task.WriterRunID = ""
				task.BlockReason = ""
				reopened++
			}
		}
	}
	if reopened == 0 {
		return nil, fmt.Errorf("%w: no incomplete tasks to reopen", ErrInvalidTransition)
	}
	job.Status = JobPlanned
	return s.saveAndCloneLocked(job)
}

func (s *Store) saveAndCloneLocked(job *Job) (*Job, error) {
	job.UpdatedAt = time.Now().UTC()
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return cloneJob(job), nil
}
func transition(status interface{}, action string) error {
	return fmt.Errorf("%w: cannot %s while state is %s", ErrInvalidTransition, action, status)
}
func integrationPrerequisitesMet(plan *Plan, task *Task) bool {
	if plan == nil {
		return false
	}
	position := -1
	for i, key := range plan.IntegrationOrder {
		if key == task.Key {
			position = i
			break
		}
	}
	if position < 0 {
		return false
	}
	for _, key := range plan.IntegrationOrder[:position] {
		for i := range plan.Tasks {
			if plan.Tasks[i].Key == key && plan.Tasks[i].Status != TaskIntegrated && plan.Tasks[i].Status != TaskNoChanges {
				return false
			}
		}
	}
	return true
}
func allTasksIntegrated(plan *Plan) bool {
	return plan != nil && len(plan.Tasks) > 0 && func() bool {
		for i := range plan.Tasks {
			if plan.Tasks[i].Status != TaskIntegrated && plan.Tasks[i].Status != TaskNoChanges {
				return false
			}
		}
		return true
	}()
}

func hasIntegratedTask(plan *Plan) bool {
	for i := range plan.Tasks {
		if plan.Tasks[i].Status == TaskIntegrated {
			return true
		}
	}
	return false
}
