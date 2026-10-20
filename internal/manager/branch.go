package manager

import (
	"fmt"
	"os"
	"strings"

	"github.com/iamangus/code-mcp/internal/identity"
	"github.com/iamangus/code-mcp/internal/worktree"
)

// PushBranch pushes the branch worktree to origin.
func (m *Manager) PushBranch(repo, branch string) error {
	if err := identity.Repository(repo); err != nil {
		return err
	}
	if err := identity.Branch(branch); err != nil {
		return err
	}
	mu := m.pathMutex(m.RepoDir(repo))
	mu.Lock()
	defer mu.Unlock()
	wtDir := m.BranchWorktreeDir(repo, branch)
	return m.git.Push(m.ctx, wtDir, branch)
}

// MergeBranch merges sourceBranch into targetBranch within the given repo.
func (m *Manager) MergeBranch(repo, sourceBranch, targetBranch string) error {
	if err := identity.Repository(repo); err != nil {
		return err
	}
	if err := identity.Branch(sourceBranch); err != nil {
		return err
	}
	if err := identity.Branch(targetBranch); err != nil {
		return err
	}
	mu := m.pathMutex(m.RepoDir(repo))
	mu.Lock()
	defer mu.Unlock()

	ctx := m.ctx
	repoDir := m.RepoDir(repo)

	defaultBranch, err := m.git.DefaultBranch(ctx, repoDir)
	if err != nil {
		return fmt.Errorf("cannot determine default branch: %w", err)
	}

	var targetDir string
	if targetBranch == defaultBranch {
		targetDir = m.BranchWorktreeDir(repo, targetBranch)
		if _, err := os.Stat(targetDir); os.IsNotExist(err) {
			if err := m.git.WorktreeAdd(ctx, repoDir, targetDir, targetBranch); err != nil {
				return fmt.Errorf("create worktree for merge target: %w", err)
			}
		}
	} else {
		targetDir = m.BranchWorktreeDir(repo, targetBranch)
		if _, err := os.Stat(targetDir); os.IsNotExist(err) {
			return fmt.Errorf("target branch worktree %q not found", targetBranch)
		}
	}

	release, err := worktree.Coordinate(ctx, targetDir, true)
	if err != nil {
		return err
	}
	defer release()
	if err := m.git.Merge(ctx, targetDir, sourceBranch); err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "CONFLICT") || strings.Contains(errStr, "conflict") || strings.Contains(errStr, "pre-existing merge") || strings.Contains(errStr, "must be clean") {
			return &MergeConflictError{Output: errStr}
		}
		return err
	}

	if pushErr := m.git.Push(ctx, targetDir, targetBranch); pushErr != nil {
		m.logger.Warn("merge: push after merge (non-fatal)", "repo", repo, "branch", targetBranch, "error", pushErr)
	}

	return nil
}

// GetCommits returns commits unique to the branch (not in default branch).
func (m *Manager) GetCommits(repo, branch string) ([]CommitInfo, error) {
	mu := m.pathMutex(m.RepoDir(repo))
	mu.RLock()
	defer mu.RUnlock()

	ctx := m.ctx
	repoDir := m.RepoDir(repo)

	defaultBranch, err := m.git.DefaultBranch(ctx, repoDir)
	if err != nil {
		return nil, err
	}

	wtDir, err := m.worktreeDirLocked(repo, branch)
	if err != nil {
		return nil, err
	}

	rangeSpec := fmt.Sprintf("%s..%s", defaultBranch, branch)
	out, err := m.git.CommitLog(ctx, wtDir, "--oneline", rangeSpec)
	if err != nil {
		return nil, err
	}

	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}

	var commits []CommitInfo
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		ci := CommitInfo{Hash: parts[0]}
		if len(parts) > 1 {
			ci.Subject = parts[1]
		}
		commits = append(commits, ci)
	}
	return commits, nil
}
