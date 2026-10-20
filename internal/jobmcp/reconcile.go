package jobmcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/iamangus/code-mcp/internal/pipeline"
)

// ReconcileJobs resumes side effects from durable intent, including transitions
// committed just before a failed dispatch, merge, push, or PR creation.
func ReconcileJobs(ctx context.Context, config Config) error {
	var failures []error
	for _, job := range config.Store.List() {
		if err := reconcileJob(ctx, job.ID, config); err != nil {
			failures = append(failures, fmt.Errorf("job %s: %w", job.ID, err))
		}
	}
	return errors.Join(failures...)
}

func reconcileJob(ctx context.Context, id string, config Config) error {
	unlock := config.Store.LockJob(id)
	defer unlock()
	job, err := config.Store.Get(id)
	if err != nil {
		return err
	}
	switch job.Status {
	case pipeline.JobCancelled:
		if canceller, ok := config.Dispatcher.(jobCanceller); ok {
			return canceller.CancelJob(ctx, id)
		}
		return nil
	case pipeline.JobCIReplanning:
		_, err := config.Dispatcher.StartPlannerRevision(ctx, job)
		return err
	case pipeline.JobPlanned, pipeline.JobWorking, pipeline.JobReviewing, pipeline.JobIntegrating, pipeline.JobCIRemediating:
		if job.Plan == nil {
			return fmt.Errorf("active job has no plan")
		}
		if err := ensureIntegrationWorktree(job, config); err != nil {
			return err
		}
		for _, task := range job.Plan.Tasks {
			if task.Status != pipeline.TaskReviewing || task.ReviewerRunID != "" {
				continue
			}
			run, err := config.Dispatcher.StartReviewer(ctx, job, &task)
			if err != nil {
				return err
			}
			if _, err := config.Controller.StartReviewer(id, task.Key, run.RunID); err != nil {
				return err
			}
		}
		_, _, err := integrateEligible(id, config, ctx)
		return err
	case pipeline.JobHolisticReviewing:
		if job.HolisticReviewVerdict != pipeline.ReviewPending {
			return nil
		} // durable outcome replay owns remediation
		if job.CI == nil || job.CI.HeadSHA != job.IntegrationSHA || job.CI.State != pipeline.CIPassed {
			_, err := startDraftPRCI(ctx, job, config)
			return err
		}
		if job.HolisticReviewRunID == "" {
			run, err := config.Dispatcher.StartHolistic(ctx, job)
			if err != nil {
				return err
			}
			_, err = config.Controller.StartHolisticReviewer(id, run.RunID, job.IntegrationSHA)
			return err
		}
	}
	return nil
}
