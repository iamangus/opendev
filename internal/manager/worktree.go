package manager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

var validBranchName = regexp.MustCompile(`^[a-zA-Z0-9._/-]+$`)

// WorktreeDir returns the directory for a repo/branch pair.
func (m *Manager) WorktreeDir(repo, branch string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.worktreeDirLocked(repo, branch)
}

func (m *Manager) worktreeDirLocked(repo, branch string) (string, error) {
	repoDir := m.RepoDir(repo)
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
	m.mu.Lock()
	defer m.mu.Unlock()

	if !validBranchName.MatchString(branch) {
		return "", fmt.Errorf("invalid branch name: %q", branch)
	}

	ctx := context.Background()
	repoDir := m.RepoDir(repo)
	if _, err := os.Stat(repoDir); os.IsNotExist(err) {
		return "", fmt.Errorf("repo %q not found", repo)
	}

	wtDir := m.BranchWorktreeDir(repo, branch)

	if _, err := os.Stat(wtDir); err == nil {
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
	m.mu.Lock()
	defer m.mu.Unlock()

	ctx := context.Background()
	repoDir := m.RepoDir(repo)
	wtDir := m.BranchWorktreeDir(repo, branch)
	if filepath.Clean(wtDir) == filepath.Clean(repoDir) {
		return fmt.Errorf("cannot remove primary worktree for repo %q", repo)
	}

	if _, err := os.Stat(wtDir); os.IsNotExist(err) {
		return fmt.Errorf("worktree %q not found for repo %q", branch, repo)
	}

	if err := m.git.WorktreeRemove(ctx, repoDir, wtDir); err != nil {
		m.logger.Warn("worktree: git worktree remove (non-fatal)", "repo", repo, "branch", branch, "error", err)
	}

	m.logger.Info("worktree removed", "repo", repo, "branch", branch)
	return os.RemoveAll(wtDir)
}

// ListBranches returns all branches (default + worktrees) for a repo.
func (m *Manager) ListBranches(repo string) ([]BranchInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	repoDir := m.RepoDir(repo)
	if _, err := os.Stat(repoDir); os.IsNotExist(err) {
		return nil, fmt.Errorf("repo %q not found", repo)
	}

	return m.listWorktrees(repoDir), nil
}
