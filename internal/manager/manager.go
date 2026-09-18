// Package manager handles discovery, cloning, and worktree management for
// multiple Git repositories stored under a single root directory.
//
// Directory layout
//
//	/repos/<name>                                  ← primary clone
//	/repos/<name>/.opendev/worktrees/<safe branch> ← managed worktree
package manager

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/iamangus/code-mcp/internal/foundation"
	"github.com/iamangus/code-mcp/internal/gitops"
)

// RepoInfo describes a synced repository and its worktrees.
type RepoInfo struct {
	Name          string       `json:"name"`
	Dir           string       `json:"dir"`
	DefaultBranch string       `json:"default_branch"`
	Branches      []BranchInfo `json:"branches"`
}

// CreateFoundationBranch writes the fixed repository foundation to an isolated
// branch, commits it, and pushes it for review. It never accepts agent-provided
// paths or content.
func (m *Manager) CreateFoundationBranch(repo, branch, base string) (string, string, error) {
	dir, err := m.CreateWorktree(repo, branch, base)
	if err != nil {
		return "", "", err
	}
	for path, content := range foundation.Files() {
		fullPath := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			return "", "", fmt.Errorf("create foundation directory: %w", err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
			return "", "", fmt.Errorf("write foundation file %s: %w", path, err)
		}
	}
	ctx := context.Background()
	status, err := m.git.Status(ctx, dir)
	if err != nil {
		return "", "", fmt.Errorf("read foundation status: %w", err)
	}
	// A retry can find the branch already carrying the exact foundation
	// content. Treat a clean tree as already-committed instead of failing on
	// an empty commit.
	if strings.TrimSpace(status) != "" {
		if err := m.git.Commit(ctx, dir, "chore: add OpenDev repository foundation"); err != nil {
			return "", "", fmt.Errorf("commit foundation: %w", err)
		}
	}
	if err := m.git.Push(ctx, dir, branch); err != nil {
		return "", "", fmt.Errorf("push foundation branch: %w", err)
	}
	sha, err := m.git.HeadCommit(ctx, dir)
	if err != nil {
		return "", "", fmt.Errorf("read foundation commit: %w", err)
	}
	return dir, sha, nil
}

// RepositoryMetadata identifies the origin and current revision of a clone.
type RepositoryMetadata struct {
	OriginURL string
	HeadSHA   string
}

// BranchInfo describes a worktree branch.
type BranchInfo struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`
}

// CommitInfo describes a single commit.
type CommitInfo struct {
	Hash    string `json:"hash"`
	Subject string `json:"subject"`
}

// MergeConflictError is returned when a merge produces conflicts.
type MergeConflictError struct {
	Output string
}

func (e *MergeConflictError) Error() string {
	return fmt.Sprintf("merge conflict:\n%s", e.Output)
}

// Manager manages repositories and worktrees on disk.
type Manager struct {
	reposDir string
	git      gitops.GitOps
	logger   *slog.Logger
	mu       sync.RWMutex
}

// New creates a Manager, creating reposDir if it doesn't exist.
func New(reposDir string, git gitops.GitOps, logger *slog.Logger) (*Manager, error) {
	if err := os.MkdirAll(reposDir, 0755); err != nil {
		return nil, err
	}
	return &Manager{
		reposDir: reposDir,
		git:      git,
		logger:   logger,
	}, nil
}

// ReposDir returns the root directory for all repos.
func (m *Manager) ReposDir() string {
	return m.reposDir
}

// RepositoryMetadata returns the origin URL and current HEAD SHA for a clone.
func (m *Manager) RepositoryMetadata(repo string) (RepositoryMetadata, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	repoDir := m.RepoDir(repo)
	if !isGitClone(repoDir) {
		return RepositoryMetadata{}, fmt.Errorf("repo %q not found", repo)
	}

	ctx := context.Background()
	originURL, err := m.git.OriginURL(ctx, repoDir)
	if err != nil {
		return RepositoryMetadata{}, fmt.Errorf("read origin URL for repo %q: %w", repo, err)
	}
	headSHA, err := m.git.HeadCommit(ctx, repoDir)
	if err != nil {
		return RepositoryMetadata{}, fmt.Errorf("read HEAD SHA for repo %q: %w", repo, err)
	}
	return RepositoryMetadata{OriginURL: originURL, HeadSHA: headSHA}, nil
}

// Status returns the porcelain status of an existing local clone or worktree.
func (m *Manager) Status(dir string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !isGitClone(dir) {
		return "", fmt.Errorf("Git worktree %q not found", dir)
	}
	status, err := m.git.Status(context.Background(), dir)
	if err != nil {
		return "", fmt.Errorf("read Git status for %q: %w", dir, err)
	}
	return strings.TrimSpace(status), nil
}

// ResolveRevision resolves a revision in an existing local clone or worktree to a commit SHA.
func (m *Manager) ResolveRevision(dir, revision string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !isGitClone(dir) {
		return "", fmt.Errorf("Git worktree %q not found", dir)
	}
	revision = strings.TrimSpace(revision)
	if revision == "" {
		return "", fmt.Errorf("Git revision is required")
	}
	sha, err := m.git.ResolveRevision(context.Background(), dir, revision)
	if err != nil {
		return "", fmt.Errorf("resolve Git revision %q in %q: %w", revision, dir, err)
	}
	return strings.TrimSpace(sha), nil
}

// AddDetachedWorktree creates a detached worktree at an immutable revision.
func (m *Manager) AddDetachedWorktree(repoDir, worktreeDir, revision string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !isGitClone(repoDir) {
		return fmt.Errorf("Git source clone %q not found", repoDir)
	}
	if strings.TrimSpace(revision) == "" {
		return fmt.Errorf("Git revision is required")
	}
	if err := m.git.WorktreeAddDetached(context.Background(), repoDir, worktreeDir, revision); err != nil {
		return fmt.Errorf("add detached Git worktree: %w", err)
	}
	return nil
}

// IsDetachedWorktree reports whether worktreeDir is a detached worktree of repoDir.
func (m *Manager) IsDetachedWorktree(repoDir, worktreeDir string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !isGitClone(repoDir) {
		return false, fmt.Errorf("Git source clone %q not found", repoDir)
	}
	worktrees, err := m.git.WorktreeList(context.Background(), repoDir)
	if err != nil {
		return false, fmt.Errorf("list Git worktrees: %w", err)
	}
	for _, worktree := range worktrees {
		if filepath.Clean(worktree.Path) == filepath.Clean(worktreeDir) {
			return worktree.Detached, nil
		}
	}
	return false, nil
}

// RepoDir returns the filesystem path for a primary repo clone.
func (m *Manager) RepoDir(repo string) string {
	return fmt.Sprintf("%s/%s", m.reposDir, repo)
}

// BranchWorktreeDir returns the filesystem path for a branch worktree.
func (m *Manager) BranchWorktreeDir(repo, branch string) string {
	return fmt.Sprintf("%s/%s/.opendev/worktrees/%s", m.reposDir, repo, filesystemBranch(branch))
}

// filesystemBranch preserves the Git branch name while keeping worktree paths
// and legacy MCP route fields single-segment.
func filesystemBranch(branch string) string {
	safe := strings.NewReplacer("/", "-", "\\", "-", "..", "-").Replace(branch)
	if safe == "." || safe == "" {
		return "-"
	}
	return safe
}

// Commit records all changes in a prepared task worktree and returns its SHA.
func (m *Manager) Commit(worktreePath string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx := context.Background()
	if err := m.git.Commit(ctx, worktreePath, "opendev: complete task"); err != nil {
		return "", fmt.Errorf("commit task worktree: %w", err)
	}
	sha, err := m.git.HeadCommit(ctx, worktreePath)
	if err != nil {
		return "", fmt.Errorf("read committed task SHA: %w", err)
	}
	return sha, nil
}

// HasChanges reports whether a task worktree contains changes to commit.
func (m *Manager) HasChanges(worktreePath string) (bool, error) {
	status, err := m.git.Status(context.Background(), worktreePath)
	if err != nil {
		return false, fmt.Errorf("read task worktree status: %w", err)
	}
	return strings.TrimSpace(status) != "", nil
}

func (m *Manager) HeadCommit(worktreePath string) (string, error) {
	return m.git.HeadCommit(context.Background(), worktreePath)
}

func (m *Manager) DiffRange(worktreePath, base, target string) (string, error) {
	return m.git.DiffRange(context.Background(), worktreePath, base, target)
}

// MergeTask merges an approved task branch and returns the integration HEAD SHA.
func (m *Manager) MergeTask(repository, sourceBranch, targetBranch string) (string, error) {
	if err := m.MergeBranch(repository, sourceBranch, targetBranch); err != nil {
		return "", err
	}
	sha, err := m.git.HeadCommit(context.Background(), m.BranchWorktreeDir(repository, targetBranch))
	if err != nil {
		return "", fmt.Errorf("read integration SHA: %w", err)
	}
	return sha, nil
}
