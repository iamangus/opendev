// Package repositories provisions and reconciles managed GitHub repositories.
package repositories

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/iamangus/code-mcp/internal/foundation"
	"github.com/iamangus/code-mcp/internal/github"
	"github.com/iamangus/code-mcp/internal/manager"
	"github.com/iamangus/code-mcp/internal/repositorycatalog"
)

type repositoryManager interface {
	SyncRepo(repoURL, name string, expectedDefaultBranch ...string) error
	RepoDir(repo string) string
	CreateFoundationBranch(repo, branch, base string) (string, string, error)
}
type requiredCheckConfigurer interface {
	EnsureRequiredCheck(context.Context, string, string, string) error
}

// ErrFoundationPending indicates that a repository foundation PR is awaiting CI.
var ErrFoundationPending = errors.New("repository foundation is pending")

// Service reconciles GitHub-owned repositories with local clones and the catalog.
type Service struct {
	manager repositoryManager
	catalog *repositorycatalog.Catalog
	github  github.Client
	owners  map[string]struct{}
}

// New creates a repository provisioning service.
func New(manager repositoryManager, catalog *repositorycatalog.Catalog, githubClient github.Client, ownedAccounts ...string) (*Service, error) {
	if manager == nil {
		return nil, fmt.Errorf("repository manager is required")
	}
	if catalog == nil {
		return nil, fmt.Errorf("repository catalog is required")
	}
	if githubClient == nil {
		return nil, fmt.Errorf("GitHub client is required")
	}
	owners := make(map[string]struct{}, len(ownedAccounts))
	for _, owner := range ownedAccounts {
		if owner = strings.ToLower(strings.TrimSpace(owner)); owner != "" {
			owners[owner] = struct{}{}
		}
	}
	return &Service{manager: manager, catalog: catalog, github: githubClient, owners: owners}, nil
}

// EnsureFoundation lazily starts or completes the fixed OpenDev foundation for
// an owned, non-fork repository. Lookup and catalog refresh remain read-only.
func (s *Service) EnsureFoundation(ctx context.Context, name string) (*repositorycatalog.Record, error) {
	name, err := repositoryName(name)
	if err != nil {
		return nil, err
	}
	repo, err := s.github.GetRepository(ctx, name)
	if err != nil {
		return nil, err
	}
	if !s.managed(repo) {
		return nil, fmt.Errorf("repository %q is not an owned non-fork repository", name)
	}
	record, err := s.syncAndRefresh(ctx, name, repo)
	if err != nil {
		return nil, err
	}
	if ready(record) {
		return record, nil
	}
	if record.Foundation != nil && record.Foundation.PRNumber > 0 {
		return s.reconcileFoundation(ctx, name, repo, record)
	}
	branch := "opendev/foundation-" + foundation.Version
	_, sha, err := s.manager.CreateFoundationBranch(name, branch, repo.DefaultBranch)
	if err != nil {
		return nil, err
	}
	pr, err := s.github.CreatePR(ctx, github.CreatePROptions{
		Repo: name, Title: "chore: add OpenDev repository foundation", Head: branch, Base: repo.DefaultBranch,
		Body: "Adds the versioned OpenDev CI and repository readiness contract.", Draft: true,
	})
	if err != nil {
		return nil, fmt.Errorf("create foundation pull request: %w", err)
	}
	if rules, ok := s.github.(requiredCheckConfigurer); ok {
		if err := rules.EnsureRequiredCheck(ctx, name, repo.DefaultBranch, "ci / OpenDev CI"); err != nil {
			return nil, fmt.Errorf("configure foundation required check: %w", err)
		}
	}
	record.Foundation = &repositorycatalog.Foundation{Version: foundation.Version, Branch: branch, PRNumber: pr.Number, PRURL: pr.HTMLURL, SHA: sha, Status: "pending", UpdatedAt: time.Now().UTC()}
	if _, err := s.catalog.Save(*record); err != nil {
		return nil, err
	}
	return record, ErrFoundationPending
}

func (s *Service) reconcileFoundation(ctx context.Context, name string, repo *github.Repository, record *repositorycatalog.Record) (*repositorycatalog.Record, error) {
	pr, err := s.github.GetPR(ctx, name, record.Foundation.PRNumber)
	if err != nil {
		return nil, err
	}
	if pr.Merged {
		refreshed, err := s.syncAndRefresh(ctx, name, repo)
		if err != nil {
			return nil, err
		}
		refreshed.Foundation = &repositorycatalog.Foundation{Version: foundation.Version, Branch: record.Foundation.Branch, PRNumber: pr.Number, PRURL: pr.HTMLURL, SHA: pr.Head.SHA, Status: "ready", UpdatedAt: time.Now().UTC()}
		return s.catalog.Save(*refreshed)
	}
	checks, err := s.github.GetPRChecks(ctx, name, pr.Head.SHA)
	if err != nil {
		return nil, err
	}
	if foundationChecksFailed(checks) {
		record.Foundation.Status = "blocked"
		record.Foundation.UpdatedAt = time.Now().UTC()
		_, saveErr := s.catalog.Save(*record)
		return record, saveErr
	}
	if !foundationChecksSucceeded(checks) {
		return record, ErrFoundationPending
	}
	if err := s.github.PromotePR(ctx, name, pr.Number); err != nil {
		return nil, err
	}
	if err := s.github.MergePR(ctx, name, pr.Number); err != nil {
		return nil, err
	}
	return s.reconcileFoundation(ctx, name, repo, record)
}

func (s *Service) managed(repo *github.Repository) bool {
	if repo == nil || repo.Fork {
		return false
	}
	if len(s.owners) == 0 {
		return true
	}
	owner, _, found := strings.Cut(strings.ToLower(repo.FullName), "/")
	if !found {
		return false
	}
	_, ok := s.owners[owner]
	return ok
}

func ready(record *repositorycatalog.Record) bool {
	return record != nil && record.Foundation != nil && record.Foundation.Version == foundation.Version && record.Foundation.Status == "ready"
}

func foundationChecksSucceeded(checks *github.PRChecks) bool {
	if checks == nil {
		return false
	}
	for _, check := range checks.CheckRuns {
		if check.Name == "ci / OpenDev CI" && strings.EqualFold(check.Status, "completed") && strings.EqualFold(check.Conclusion, "success") {
			return true
		}
	}
	return false
}

func foundationChecksFailed(checks *github.PRChecks) bool {
	if checks == nil {
		return false
	}
	for _, check := range checks.CheckRuns {
		if check.Name == "ci / OpenDev CI" && strings.EqualFold(check.Status, "completed") && !strings.EqualFold(check.Conclusion, "success") {
			return true
		}
	}
	return false
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
	if err := s.manager.SyncRepo(repo.CloneURL, name, repo.DefaultBranch); err != nil {
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
