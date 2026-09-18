package manager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var runtimeExcludePatterns = []string{
	".opendev/state",
	".opendev/worktrees",
	".opendev/references",
}

// SyncRepo clones the repo if it doesn't exist. Existing primary mirrors are
// fetched and fast-forwarded only, leaving active task worktrees untouched.
func (m *Manager) SyncRepo(repoURL, name string, expectedDefaultBranch ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	ctx := context.Background()
	repoDir := m.RepoDir(name)

	if isGitClone(repoDir) {
		m.logger.Info("repo sync: fetching", "repo", name)
		if err := m.git.Fetch(ctx, repoDir); err != nil {
			return fmt.Errorf("fetch repository %q: %w", name, err)
		}
		status, err := m.git.Status(ctx, repoDir)
		if err != nil {
			return fmt.Errorf("check primary mirror status for %q: %w", name, err)
		}
		if strings.TrimSpace(status) != "" {
			return fmt.Errorf("primary mirror for %q is dirty; refusing to fast-forward", name)
		}
		branch := ""
		if len(expectedDefaultBranch) > 0 {
			branch = strings.TrimSpace(expectedDefaultBranch[0])
		}
		if branch == "" {
			var err error
			branch, err = m.git.DefaultBranch(ctx, repoDir)
			if err != nil {
				return fmt.Errorf("determine primary mirror branch for %q: %w", name, err)
			}
		} else {
			remote, err := m.git.RemoteBranchExists(ctx, repoDir, branch)
			if err != nil {
				return fmt.Errorf("verify remote default branch for %q: %w", name, err)
			}
			if !remote {
				return fmt.Errorf("remote default branch %q for %q does not exist", branch, name)
			}
			local, err := m.git.BranchExists(ctx, repoDir, branch)
			if err != nil {
				return fmt.Errorf("verify local default branch for %q: %w", name, err)
			}
			if !local {
				if err := m.git.CreateBranch(ctx, repoDir, branch, "origin/"+branch); err != nil {
					return fmt.Errorf("create local default branch for %q: %w", name, err)
				}
			}
			if err := m.git.Checkout(ctx, repoDir, branch); err != nil {
				return fmt.Errorf("checkout default branch for %q: %w", name, err)
			}
		}
		if err := m.git.FastForward(ctx, repoDir, branch); err != nil {
			return fmt.Errorf("fast-forward primary mirror for %q: %w", name, err)
		}
		return m.ensureRuntimeExcludes(ctx, repoDir)
	}
	if _, err := os.Stat(repoDir); err == nil {
		return fmt.Errorf("repo %q is not a normal Git clone", name)
	}

	m.logger.Info("repo sync: cloning", "repo", name, "url", repoURL)
	if err := m.git.Clone(ctx, repoURL, repoDir); err != nil {
		return err
	}
	return m.ensureRuntimeExcludes(ctx, repoDir)
}

func (m *Manager) ensureRuntimeExcludes(ctx context.Context, repoDir string) error {
	if err := m.git.EnsureLocalExcludes(ctx, repoDir, runtimeExcludePatterns); err != nil {
		return fmt.Errorf("configure local excludes: %w", err)
	}
	return nil
}

// RemoveRepo deletes the main clone and its managed worktrees.
func (m *Manager) RemoveRepo(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	repoDir := m.RepoDir(name)
	if _, err := os.Stat(repoDir); os.IsNotExist(err) {
		return fmt.Errorf("repo %q not found", name)
	}

	worktrees, err := m.git.WorktreeList(context.Background(), repoDir)
	if err == nil {
		for _, worktree := range worktrees {
			if !isManagedWorktree(repoDir, worktree.Path) {
				continue
			}
			if err := m.git.WorktreeRemove(context.Background(), repoDir, worktree.Path); err != nil {
				m.logger.Warn("removing worktree failed", "repo", name, "path", worktree.Path, "error", err)
			}
		}
	}

	m.logger.Info("removing repo", "repo", name)
	return os.RemoveAll(repoDir)
}

// Scan discovers all repos and worktrees on disk.
func (m *Manager) Scan() ([]RepoInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.scan()
}

func (m *Manager) scan() ([]RepoInfo, error) {
	entries, err := os.ReadDir(m.reposDir)
	if err != nil {
		return nil, err
	}

	ctx := context.Background()
	var repos []RepoInfo

	for _, e := range entries {
		if !e.IsDir() || e.Name() == ".opendev" {
			continue
		}
		name := e.Name()
		repoDir := filepath.Join(m.reposDir, name)
		if !isGitClone(repoDir) {
			continue
		}
		repoName := name

		defaultBranch, err := m.git.DefaultBranch(ctx, repoDir)
		if err != nil {
			m.logger.Warn("scan: could not determine default branch", "repo", repoName, "error", err)
			defaultBranch = "main"
		}

		branches := m.listWorktrees(repoDir)

		repos = append(repos, RepoInfo{
			Name:          repoName,
			Dir:           repoDir,
			DefaultBranch: defaultBranch,
			Branches:      branches,
		})
	}
	return repos, nil
}

func isGitClone(repoDir string) bool {
	gitDir, err := os.Stat(filepath.Join(repoDir, ".git"))
	return err == nil && (gitDir.IsDir() || gitDir.Mode().IsRegular())
}

func isManagedWorktree(repoDir, worktreeDir string) bool {
	managedRoot := filepath.Join(repoDir, ".opendev", "worktrees")
	rel, err := filepath.Rel(managedRoot, worktreeDir)
	return err == nil && rel != "." && rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (m *Manager) listWorktrees(repoDir string) []BranchInfo {
	worktrees, err := m.git.WorktreeList(context.Background(), repoDir)
	if err != nil {
		m.logger.Warn("list worktrees failed", "repo", repoDir, "error", err)
		return nil
	}
	var branches []BranchInfo
	for _, worktree := range worktrees {
		branches = append(branches, BranchInfo{
			Name: worktree.Branch,
			Dir:  worktree.Path,
		})
	}
	return branches
}
