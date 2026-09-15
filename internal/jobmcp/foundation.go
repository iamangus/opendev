package jobmcp

import (
	"context"
	"errors"

	"github.com/iamangus/code-mcp/internal/outbox"
	"github.com/iamangus/code-mcp/internal/repositories"
)

// ResumeFoundationPlanning starts jobs that were durably held while their
// one-time repository foundation PR was awaiting CI and merge.
func ResumeFoundationPlanning(ctx context.Context, config Config) error {
	if config.Repositories == nil {
		return nil
	}
	for _, job := range config.Store.List() {
		if !job.FoundationPending {
			continue
		}
		if _, err := config.Repositories.EnsureFoundation(ctx, job.Repository); err != nil {
			if errors.Is(err, repositories.ErrFoundationPending) {
				continue
			}
			return err
		}
		planned, err := config.Store.StartPlanning(job.ID)
		if err != nil {
			return err
		}
		notify(config, planned, outbox.EventStarted, "", "")
		if _, err := config.Dispatcher.StartPlanner(ctx, planned); err != nil {
			return err
		}
	}
	return nil
}
