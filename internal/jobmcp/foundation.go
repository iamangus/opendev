package jobmcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/iamangus/code-mcp/internal/outbox"
	"github.com/iamangus/code-mcp/internal/pipeline"
	"github.com/iamangus/code-mcp/internal/repositories"
)

// ResumeFoundationPlanning starts jobs that were durably held while their
// one-time repository foundation PR was awaiting CI and merge.
func ResumeFoundationPlanning(ctx context.Context, config Config) error {
	if config.Store == nil || config.Dispatcher == nil {
		return nil
	}
	var firstErr error
	for _, job := range config.Store.List() {
		if job.FoundationPending && job.Status == pipeline.JobCreated {
			if config.Repositories == nil {
				continue
			}
			if _, err := config.Repositories.EnsureFoundation(ctx, job.Repository); err != nil {
				if errors.Is(err, repositories.ErrFoundationPending) || errors.Is(err, repositories.ErrFoundationBlocked) {
					continue
				}
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if job.PlannerAgentID == "" {
				if firstErr == nil {
					firstErr = fmt.Errorf("job %s has no planner agent", job.ID)
				}
				continue
			}
			planned, err := config.Store.StartPlanning(job.ID)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			notify(config, planned, outbox.EventStarted, "", "")
			job = planned
		}
		if job.Status != pipeline.JobPlanning || job.Plan != nil {
			continue
		}
		if job.PlannerAgentID == "" {
			if firstErr == nil {
				firstErr = fmt.Errorf("job %s has no planner agent", job.ID)
			}
			continue
		}
		if _, err := config.Dispatcher.StartPlanner(ctx, job); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
