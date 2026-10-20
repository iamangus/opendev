package pipeline

import (
	"fmt"
	"time"
)

func Terminal(status JobStatus) bool {
	return status == JobPublished || status == JobNoChanges || status == JobFailed || status == JobCancelled
}

func (s *Store) Pause(id string) (*Job, error) {
	return s.updateJob(id, func(job *Job) error {
		if job.Status == JobPaused {
			return nil
		}
		if Terminal(job.Status) {
			return transition(job.Status, "pause")
		}
		job.PausedFrom, job.Status, job.PausedAt = job.Status, JobPaused, time.Now().UTC()
		return nil
	})
}

func (s *Store) Resume(id string) (*Job, error) {
	return s.updateJob(id, func(job *Job) error {
		if job.Status != JobPaused || job.PausedFrom == "" {
			return transition(job.Status, "resume")
		}
		if job.PausedFrom == JobAwaitingCI && job.CI != nil && job.CI.State == CIPending && !job.PausedAt.IsZero() && !job.CI.StartedAt.IsZero() {
			job.CI.StartedAt = job.CI.StartedAt.Add(time.Since(job.PausedAt))
		}
		job.Status, job.PausedFrom, job.PausedAt = job.PausedFrom, "", time.Time{}
		return nil
	})
}

func (s *Store) Cancel(id, reason string) (*Job, error) {
	return s.updateJob(id, func(job *Job) error {
		if job.Status == JobCancelled {
			return nil
		}
		if Terminal(job.Status) {
			return transition(job.Status, "cancel")
		}
		job.Status, job.Failure = JobCancelled, reason
		return nil
	})
}

func (s *Store) SetMergePolicy(id, policy string) (*Job, error) {
	if policy != "auto" && policy != "manual" {
		return nil, fmt.Errorf("merge_policy must be auto or manual")
	}
	return s.updateJob(id, func(job *Job) error {
		if Terminal(job.Status) {
			return transition(job.Status, "change merge policy")
		}
		job.MergePolicy = policy
		return nil
	})
}

func (s *Store) RetryPlanning(id string) (*Job, error) {
	return s.updateJob(id, func(job *Job) error {
		if job.Status != JobPlanning && job.Status != JobCIReplanning {
			return transition(job.Status, "retry planning")
		}
		job.PlannerRetry++
		return nil
	})
}
