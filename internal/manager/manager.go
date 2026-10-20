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
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/iamangus/code-mcp/internal/foundation"
	"github.com/iamangus/code-mcp/internal/gitops"
	"github.com/iamangus/code-mcp/internal/identity"
	"github.com/iamangus/code-mcp/internal/worktree"
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
		r, rel, err := worktree.OpenRoot(dir, path)
		if err != nil {
			return "", "", err
		}
		if err := r.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
			r.Close()
			return "", "", fmt.Errorf("create foundation directory: %w", err)
		}
		if err := r.WriteFile(rel, []byte(content), 0o644); err != nil {
			r.Close()
			return "", "", fmt.Errorf("write foundation file %s: %w", path, err)
		}
		r.Close()
	}
	ctx := m.ctx
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

func (e *MergeConflictError) NeedsIntervention() bool { return true }

// Manager manages repositories and worktrees on disk.
type Manager struct {
	reposDir  string
	git       gitops.GitOps
	logger    *slog.Logger
	repoLocks sync.Map
	ctx       context.Context
}

// New creates a Manager, creating reposDir if it doesn't exist.
func New(reposDir string, git gitops.GitOps, logger *slog.Logger, contexts ...context.Context) (*Manager, error) {
	absolute, err := filepath.Abs(reposDir)
	if err != nil {
		return nil, err
	}
	reposDir = absolute
	if err := os.MkdirAll(reposDir, 0755); err != nil {
		return nil, err
	}
	ctx := context.Background()
	if len(contexts) > 0 && contexts[0] != nil {
		ctx = contexts[0]
	}
	return &Manager{
		reposDir: reposDir,
		git:      git,
		logger:   logger,
		ctx:      ctx,
	}, nil
}

func (m *Manager) pathMutex(path string) *sync.RWMutex {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if rel, err := filepath.Rel(m.reposDir, abs); err == nil && filepath.IsLocal(rel) {
		abs = filepath.Join(m.reposDir, strings.Split(rel, string(filepath.Separator))[0])
	}
	value, _ := m.repoLocks.LoadOrStore(abs, &sync.RWMutex{})
	return value.(*sync.RWMutex)
}

// ReposDir returns the root directory for all repos.
func (m *Manager) ReposDir() string {
	return m.reposDir
}

// RepositoryMetadata returns the origin URL and current HEAD SHA for a clone.
func (m *Manager) RepositoryMetadata(repo string) (RepositoryMetadata, error) {
	if err := identity.Repository(repo); err != nil {
		return RepositoryMetadata{}, err
	}
	if _, err := worktree.Resolve(m.reposDir, repo); err != nil {
		return RepositoryMetadata{}, err
	}
	mu := m.pathMutex(m.RepoDir(repo))
	mu.RLock()
	defer mu.RUnlock()

	repoDir := m.RepoDir(repo)
	if !isGitClone(repoDir) {
		return RepositoryMetadata{}, fmt.Errorf("repo %q not found", repo)
	}

	ctx := m.ctx
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
	mu := m.pathMutex(dir)
	mu.RLock()
	defer mu.RUnlock()
	if !isGitClone(dir) {
		return "", fmt.Errorf("git worktree %q not found", dir)
	}
	status, err := m.git.Status(m.ctx, dir)
	if err != nil {
		return "", fmt.Errorf("read Git status for %q: %w", dir, err)
	}
	return strings.TrimSpace(status), nil
}

// ResolveRevision resolves a revision in an existing local clone or worktree to a commit SHA.
func (m *Manager) ResolveRevision(dir, revision string) (string, error) {
	mu := m.pathMutex(dir)
	mu.RLock()
	defer mu.RUnlock()
	if !isGitClone(dir) {
		return "", fmt.Errorf("git worktree %q not found", dir)
	}
	revision = strings.TrimSpace(revision)
	if revision == "" {
		return "", fmt.Errorf("git revision is required")
	}
	sha, err := m.git.ResolveRevision(m.ctx, dir, revision)
	if err != nil {
		return "", fmt.Errorf("resolve Git revision %q in %q: %w", revision, dir, err)
	}
	return strings.TrimSpace(sha), nil
}

// AddDetachedWorktree creates a detached worktree at an immutable revision.
func (m *Manager) AddDetachedWorktree(repoDir, worktreeDir, revision string) error {
	mu := m.pathMutex(repoDir)
	mu.Lock()
	defer mu.Unlock()
	if !isGitClone(repoDir) {
		return fmt.Errorf("git source clone %q not found", repoDir)
	}
	if strings.TrimSpace(revision) == "" {
		return fmt.Errorf("git revision is required")
	}
	if err := m.git.WorktreeAddDetached(m.ctx, repoDir, worktreeDir, revision); err != nil {
		return fmt.Errorf("add detached Git worktree: %w", err)
	}
	return nil
}

// IsDetachedWorktree reports whether worktreeDir is a detached worktree of repoDir.
func (m *Manager) IsDetachedWorktree(repoDir, worktreeDir string) (bool, error) {
	mu := m.pathMutex(repoDir)
	mu.RLock()
	defer mu.RUnlock()
	if !isGitClone(repoDir) {
		return false, fmt.Errorf("git source clone %q not found", repoDir)
	}
	worktrees, err := m.git.WorktreeList(m.ctx, repoDir)
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
	repo = strings.ToLower(strings.TrimSpace(repo))
	canonical := filepath.Join(m.reposDir, repo)
	if _, err := os.Lstat(canonical); err == nil {
		return canonical
	}
	// Adopt pre-normalization clones without renaming or deleting user files.
	if entries, err := os.ReadDir(m.reposDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() && strings.EqualFold(entry.Name(), repo) {
				return filepath.Join(m.reposDir, entry.Name())
			}
		}
	}
	return canonical
}

// BranchWorktreeDir returns the filesystem path for a branch worktree.
func (m *Manager) BranchWorktreeDir(repo, branch string) string {
	legacy := strings.NewReplacer("/", "-", "\\", "-", "..", "-").Replace(branch)
	legacyPath := filepath.Join(m.RepoDir(repo), ".opendev", "worktrees", legacy)
	if legacy != filesystemBranch(branch) {
		if _, err := os.Lstat(legacyPath); err == nil {
			if worktrees, err := m.git.WorktreeList(m.ctx, m.RepoDir(repo)); err == nil {
				for _, wt := range worktrees {
					if wt.Branch == branch && filepath.Clean(wt.Path) == filepath.Clean(legacyPath) {
						return legacyPath
					}
				}
			}
		}
	}
	return filepath.Join(m.RepoDir(repo), ".opendev", "worktrees", filesystemBranch(branch))
}

// filesystemBranch preserves the Git branch name while keeping worktree paths
// and legacy MCP route fields single-segment.
func filesystemBranch(branch string) string {
	if strings.ContainsAny(branch, "/\\") || strings.Contains(branch, "..") || strings.HasPrefix(branch, "ref-") {
		return "ref-" + base64.RawURLEncoding.EncodeToString([]byte(branch))
	}
	safe := strings.NewReplacer("/", "-", "\\", "-", "..", "-").Replace(branch)
	if safe == "." || safe == "" {
		return "-"
	}
	return safe
}

// Commit records all changes in a prepared task worktree and returns its SHA.
func (m *Manager) Commit(worktreePath string) (string, error) {
	resolved, err := worktree.ResolveManaged(m.reposDir, worktreePath)
	if err != nil {
		return "", err
	}
	rel, _ := filepath.Rel(m.reposDir, resolved)
	repo := strings.Split(rel, string(filepath.Separator))[0]
	if !isManagedWorktree(m.RepoDir(repo), resolved) {
		return "", fmt.Errorf("only managed task worktrees may be committed")
	}
	mu := m.pathMutex(worktreePath)
	mu.Lock()
	defer mu.Unlock()
	release, err := worktree.Coordinate(m.ctx, worktreePath, true)
	if err != nil {
		return "", err
	}
	defer release()
	ctx := m.ctx
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
	mu := m.pathMutex(worktreePath)
	mu.RLock()
	defer mu.RUnlock()
	status, err := m.git.Status(m.ctx, worktreePath)
	if err != nil {
		return false, fmt.Errorf("read task worktree status: %w", err)
	}
	return strings.TrimSpace(status) != "", nil
}

func (m *Manager) HeadCommit(worktreePath string) (string, error) {
	mu := m.pathMutex(worktreePath)
	mu.RLock()
	defer mu.RUnlock()
	return m.git.HeadCommit(m.ctx, worktreePath)
}

func (m *Manager) DiffRange(worktreePath, base, target string) (string, error) {
	mu := m.pathMutex(worktreePath)
	mu.RLock()
	defer mu.RUnlock()
	return m.git.DiffRange(m.ctx, worktreePath, base, target)
}

func (m *Manager) ReadFileAt(ctx context.Context, dir, sha, path string) (string, error) {
	reader, ok := m.git.(interface {
		ReadFileAt(context.Context, string, string, string) (string, error)
	})
	if !ok {
		return "", fmt.Errorf("committed evidence reads are unavailable")
	}
	return reader.ReadFileAt(ctx, dir, sha, path)
}

// MergeTask merges an approved task branch and returns the integration HEAD SHA.
func (m *Manager) MergeTask(repository, sourceBranch, targetBranch string) (string, error) {
	if err := m.MergeBranch(repository, sourceBranch, targetBranch); err != nil {
		return "", err
	}
	sha, err := m.git.HeadCommit(m.ctx, m.BranchWorktreeDir(repository, targetBranch))
	if err != nil {
		return "", fmt.Errorf("read integration SHA: %w", err)
	}
	return sha, nil
}
