package manager

import (
	"fmt"
	"github.com/iamangus/code-mcp/internal/identity"
	"github.com/iamangus/code-mcp/internal/worktree"
	"os"
	"path/filepath"
)

// WorktreeDir returns the directory for a repo/branch pair.
func (m *Manager) WorktreeDir(repo, branch string) (string, error) {
	mu := m.pathMutex(m.RepoDir(repo))
	mu.RLock()
	defer mu.RUnlock()
	return m.worktreeDirLocked(repo, branch)
}

func (m *Manager) worktreeDirLocked(repo, branch string) (string, error) {
	repoDir := m.RepoDir(repo)
	if _, err := worktree.Resolve(m.reposDir, repo); err != nil {
		return "", err
	}
	if _, err := os.Stat(repoDir); os.IsNotExist(err) {
		return "", fmt.Errorf("repo %q not found", repo)
	}

	wtDir := m.BranchWorktreeDir(repo, branch)
	if filepath.Clean(wtDir) == filepath.Clean(repoDir) {
		return "", fmt.Errorf("cannot remove primary worktree for repo %q", repo)
	}
	if _, err := os.Stat(wtDir); os.IsNotExist(err) {
		return "", fmt.Errorf("worktree %q not found for repo %q", branch, repo)
	}
	return wtDir, nil
}

// CreateWorktree creates a worktree for the given branch, creating the branch if needed.
func (m *Manager) CreateWorktree(repo, branch, base string) (string, error) {
	mu := m.pathMutex(m.RepoDir(repo))
	mu.Lock()
	defer mu.Unlock()

	if err := identity.Repository(repo); err != nil {
		return "", err
	}
	if err := identity.Branch(branch); err != nil {
		return "", err
	}
	if base != "" && base[0] == '-' {
		return "", fmt.Errorf("invalid base revision")
	}

	ctx := m.ctx
	repoDir := m.RepoDir(repo)
	if _, err := worktree.Resolve(m.reposDir, repo); err != nil {
		return "", err
	}
	if _, err := os.Stat(repoDir); os.IsNotExist(err) {
		return "", fmt.Errorf("repo %q not found", repo)
	}

	wtDir := m.BranchWorktreeDir(repo, branch)
	if _, err := worktree.ResolveManaged(repoDir, wtDir); err != nil {
		return "", err
	}

	if _, err := os.Stat(wtDir); err == nil {
		listed, listErr := m.git.WorktreeList(ctx, repoDir)
		if listErr != nil {
			return "", listErr
		}
		valid := false
		for _, wt := range listed {
			if filepath.Clean(wt.Path) == filepath.Clean(wtDir) && wt.Branch == branch {
				valid = true
			}
		}
		if !valid {
			return "", fmt.Errorf("existing worktree path is not registered for branch %q", branch)
		}
		m.logger.Info("worktree: already exists", "repo", repo, "branch", branch)
		resolved, err := filepath.EvalSymlinks(wtDir)
		if err != nil {
			return wtDir, nil
		}
		return resolved, nil
	}

	if fetchErr := m.git.Fetch(ctx, repoDir); fetchErr != nil {
		m.logger.Warn("worktree: fetch before create (non-fatal)", "repo", repo, "error", fetchErr)
	}

	localExists, err := m.git.BranchExists(ctx, repoDir, branch)
	if err != nil {
		return "", err
	}

	remoteExists := false
	if !localExists {
		remoteExists, _ = m.git.RemoteBranchExists(ctx, repoDir, branch)

		var startPoint string
		if remoteExists {
			startPoint = "origin/" + branch
		} else {
			defaultBranch, err := m.git.DefaultBranch(ctx, repoDir)
			if err != nil {
				return "", fmt.Errorf("cannot determine default branch: %w", err)
			}
			startPoint = defaultBranch
			if base != "" {
				startPoint = base
			}
		}

		if err := m.git.CreateBranch(ctx, repoDir, branch, startPoint); err != nil {
			return "", fmt.Errorf("create branch %q: %w", branch, err)
		}
	}

	if err := m.git.WorktreeAdd(ctx, repoDir, wtDir, branch); err != nil {
		return "", fmt.Errorf("worktree add: %w", err)
	}

	if !localExists && !remoteExists {
		if pushErr := m.git.Push(ctx, wtDir, branch); pushErr != nil {
			m.logger.Warn("worktree: push after create (non-fatal)", "repo", repo, "branch", branch, "error", pushErr)
		}
	}

	resolved, err := filepath.EvalSymlinks(wtDir)
	if err != nil {
		return wtDir, nil
	}

	m.logger.Info("worktree created", "repo", repo, "branch", branch, "dir", resolved)
	return resolved, nil
}

// RemoveWorktree removes a branch worktree.
func (m *Manager) RemoveWorktree(repo, branch string) error {
	if err := identity.Repository(repo); err != nil {
		return err
	}
	if err := identity.Branch(branch); err != nil {
		return err
	}
	mu := m.pathMutex(m.RepoDir(repo))
	mu.Lock()
	defer mu.Unlock()

	ctx := m.ctx
	repoDir := m.RepoDir(repo)
	wtDir := m.BranchWorktreeDir(repo, branch)
	if filepath.Clean(wtDir) == filepath.Clean(repoDir) {
		return fmt.Errorf("cannot remove primary worktree for repo %q", repo)
	}

	if _, err := os.Stat(wtDir); os.IsNotExist(err) {
		return fmt.Errorf("worktree %q not found for repo %q", branch, repo)
	}

	if err := m.git.WorktreeRemove(ctx, repoDir, wtDir); err != nil {
		return err
	}

	m.logger.Info("worktree removed", "repo", repo, "branch", branch)
	return nil
}

// ListBranches returns all branches (default + worktrees) for a repo.
func (m *Manager) ListBranches(repo string) ([]BranchInfo, error) {
	if err := identity.Repository(repo); err != nil {
		return nil, err
	}
	if _, err := worktree.Resolve(m.reposDir, repo); err != nil {
		return nil, err
	}
	mu := m.pathMutex(m.RepoDir(repo))
	mu.RLock()
	defer mu.RUnlock()

	repoDir := m.RepoDir(repo)
	if _, err := os.Stat(repoDir); os.IsNotExist(err) {
		return nil, fmt.Errorf("repo %q not found", repo)
	}

	return m.listWorktrees(repoDir), nil
}
