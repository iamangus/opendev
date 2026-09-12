// Package repositories provisions and reconciles managed GitHub repositories.
package repositories

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/iamangus/code-mcp/internal/github"
	"github.com/iamangus/code-mcp/internal/manager"
	"github.com/iamangus/code-mcp/internal/repositorycatalog"
)

type repositoryManager interface {
	SyncRepo(repoURL, name string) error
	RepoDir(repo string) string
}

// Service reconciles GitHub-owned repositories with local clones and the catalog.
type Service struct {
	manager repositoryManager
	catalog *repositorycatalog.Catalog
	github  github.Client
}

// New creates a repository provisioning service.
func New(manager repositoryManager, catalog *repositorycatalog.Catalog, githubClient github.Client) (*Service, error) {
	if manager == nil {
		return nil, fmt.Errorf("repository manager is required")
	}
	if catalog == nil {
		return nil, fmt.Errorf("repository catalog is required")
	}
	if githubClient == nil {
		return nil, fmt.Errorf("GitHub client is required")
	}
	return &Service{manager: manager, catalog: catalog, github: githubClient}, nil
}

// Lookup reconciles an existing GitHub repository and returns its catalog record.
// It returns nil when the repository is not owned by the configured GitHub client.
func (s *Service) Lookup(ctx context.Context, name string) (*repositorycatalog.Record, error) {
	name, err := repositoryName(name)
	if err != nil {
		return nil, err
	}
	if _, err := s.catalog.Get(name); err != nil {
		return nil, fmt.Errorf("lookup repository catalog entry: %w", err)
	}
	repo, err := s.github.GetRepository(ctx, name)
	if errors.Is(err, github.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup GitHub repository %q: %w", name, err)
	}
	return s.syncAndRefresh(ctx, name, repo)
}

// ProvisionPrivate creates a private repository when absent, then syncs and catalogs it.
func (s *Service) ProvisionPrivate(ctx context.Context, name, description string) (*repositorycatalog.Record, error) {
	name, err := repositoryName(name)
	if err != nil {
		return nil, err
	}
	if record, err := s.Lookup(ctx, name); err != nil || record != nil {
		return record, err
	}

	repo, err := s.github.CreateRepository(ctx, name, description, true)
	if err != nil {
		createErr := err
		repo, recheckErr := s.github.GetRepository(ctx, name)
		if recheckErr == nil {
			return s.syncAndRefresh(ctx, name, repo)
		}
		if errors.Is(recheckErr, github.ErrNotFound) {
			return nil, fmt.Errorf("create private GitHub repository %q: %w", name, createErr)
		}
		return nil, fmt.Errorf("recheck GitHub repository %q after create: %w", name, recheckErr)
	}
	return s.syncAndRefresh(ctx, name, repo)
}

// ForkPublic forks the named public upstream only when the target repository is absent.
func (s *Service) ForkPublic(ctx context.Context, upstreamOwner, upstreamRepo, name string) (*repositorycatalog.Record, error) {
	name, err := repositoryName(name)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(upstreamOwner) == "" || strings.TrimSpace(upstreamRepo) == "" {
		return nil, fmt.Errorf("public upstream owner and repository are required")
	}
	if record, err := s.Lookup(ctx, name); err != nil || record != nil {
		return record, err
	}

	repo, err := s.github.ForkPublicRepository(ctx, strings.TrimSpace(upstreamOwner), strings.TrimSpace(upstreamRepo), name, false)
	if err != nil {
		forkErr := err
		repo, recheckErr := s.github.GetRepository(ctx, name)
		if recheckErr == nil {
			return s.syncAndRefresh(ctx, name, repo)
		}
		if errors.Is(recheckErr, github.ErrNotFound) {
			return nil, fmt.Errorf("fork public GitHub repository %q: %w", name, forkErr)
		}
		return nil, fmt.Errorf("recheck GitHub repository %q after fork: %w", name, recheckErr)
	}
	return s.syncAndRefresh(ctx, name, repo)
}

func (s *Service) syncAndRefresh(ctx context.Context, name string, repo *github.Repository) (*repositorycatalog.Record, error) {
	if repo == nil || strings.TrimSpace(repo.CloneURL) == "" {
		return nil, fmt.Errorf("GitHub repository %q has no clone URL", name)
	}
	if err := s.manager.SyncRepo(repo.CloneURL, name); err != nil {
		return nil, fmt.Errorf("sync repository %q: %w", name, err)
	}
	record, err := s.catalog.Refresh(ctx, manager.RepoInfo{
		Name:          name,
		Dir:           s.manager.RepoDir(name),
		DefaultBranch: repo.DefaultBranch,
	})
	if err != nil {
		return nil, fmt.Errorf("refresh repository catalog for %q: %w", name, err)
	}
	return record, nil
}

func repositoryName(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return "", fmt.Errorf("repository name is required")
	}
	return name, nil
}
