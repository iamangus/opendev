//go:build integration

package gitops

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecGitOps_CloneAndWorktree(t *testing.T) {
	srcDir := t.TempDir()
	runCmd(t, srcDir, "git", "init")
	runCmd(t, srcDir, "git", "config", "user.email", "test@test.com")
	runCmd(t, srcDir, "git", "config", "user.name", "Test")
	writeFile(t, filepath.Join(srcDir, "hello.txt"), "hello")
	runCmd(t, srcDir, "git", "add", ".")
	runCmd(t, srcDir, "git", "commit", "-m", "initial")

	g := NewExec(slog.Default(), "")
	ctx := context.Background()

	cloneDir := filepath.Join(t.TempDir(), "clone")
	if err := g.Clone(ctx, srcDir, cloneDir); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cloneDir, ".git")); err != nil {
		t.Fatalf("Clone did not create a normal clone: %v", err)
	}
	originURL, err := g.OriginURL(ctx, cloneDir)
	if err != nil {
		t.Fatalf("OriginURL: %v", err)
	}
	if originURL != srcDir {
		t.Errorf("OriginURL = %q, want %q", originURL, srcDir)
	}
	headSHA, err := g.HeadCommit(ctx, cloneDir)
	if err != nil {
		t.Fatalf("HeadCommit: %v", err)
	}
	if headSHA == "" {
		t.Fatal("HeadCommit returned an empty SHA")
	}
	if err := g.EnsureLocalExcludes(ctx, cloneDir, []string{".opendev/state", ".opendev/worktrees", ".opendev/references"}); err != nil {
		t.Fatalf("EnsureLocalExcludes: %v", err)
	}
	excludes, err := os.ReadFile(filepath.Join(cloneDir, ".git", "info", "exclude"))
	if err != nil {
		t.Fatalf("read local excludes: %v", err)
	}
	for _, pattern := range []string{".opendev/state", ".opendev/worktrees", ".opendev/references"} {
		if !strings.Contains(string(excludes), pattern) {
			t.Errorf("exclude file missing %q: %s", pattern, excludes)
		}
	}

	branch, err := g.DefaultBranch(ctx, cloneDir)
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}
	if branch == "" {
		t.Fatal("DefaultBranch returned empty string")
	}

	if err := g.Fetch(ctx, cloneDir); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	wtDir := filepath.Join(t.TempDir(), "wt-feature")
	if err := g.CreateBranch(ctx, cloneDir, "feature", branch); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := g.WorktreeAdd(ctx, cloneDir, wtDir, "feature"); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	worktrees, err := g.WorktreeList(ctx, cloneDir)
	if err != nil {
		t.Fatalf("WorktreeList: %v", err)
	}
	if len(worktrees) != 2 || worktrees[0].Path != cloneDir || worktrees[1].Path != wtDir {
		t.Fatalf("WorktreeList = %+v, want primary clone and feature worktree", worktrees)
	}

	if _, err := os.Stat(filepath.Join(wtDir, "hello.txt")); err != nil {
		t.Fatalf("worktree missing hello.txt: %v", err)
	}

	diff, err := g.Diff(ctx, wtDir)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if diff != "" {
		t.Fatalf("expected empty diff, got: %s", diff)
	}

	status, err := g.Status(ctx, wtDir)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status != "" {
		t.Fatalf("expected clean status, got: %s", status)
	}

	runCmd(t, wtDir, "git", "config", "user.email", "test@test.com")
	runCmd(t, wtDir, "git", "config", "user.name", "Test")
	writeFile(t, filepath.Join(wtDir, "committed.txt"), "committed")
	if err := g.Commit(ctx, wtDir, "add committed file"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	commit, err := g.HeadCommit(ctx, wtDir)
	if err != nil {
		t.Fatalf("HeadCommit: %v", err)
	}
	if commit == "" {
		t.Fatal("HeadCommit returned an empty commit")
	}

	releaseDir := filepath.Join(t.TempDir(), "wt-release")
	if err := g.CreateBranch(ctx, cloneDir, "release", branch); err != nil {
		t.Fatalf("CreateBranch release: %v", err)
	}
	if err := g.WorktreeAdd(ctx, cloneDir, releaseDir, "release"); err != nil {
		t.Fatalf("WorktreeAdd release: %v", err)
	}
	if err := g.CherryPick(ctx, releaseDir, commit); err != nil {
		t.Fatalf("CherryPick: %v", err)
	}
	if _, err := os.Stat(filepath.Join(releaseDir, "committed.txt")); err != nil {
		t.Fatalf("cherry-picked file missing: %v", err)
	}
	if err := g.WorktreeRemove(ctx, cloneDir, releaseDir); err != nil {
		t.Fatalf("WorktreeRemove release: %v", err)
	}

	if err := g.WorktreeRemove(ctx, cloneDir, wtDir); err != nil {
		t.Fatalf("WorktreeRemove: %v", err)
	}
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Fatal("worktree directory should have been removed")
	}

	sha, err := g.ResolveRevision(ctx, cloneDir, branch)
	if err != nil || sha == "" {
		t.Fatalf("ResolveRevision: %q, %v", sha, err)
	}
	snapshotDir := filepath.Join(cloneDir, ".opendev", "references", "job-1", "reference")
	if err := os.MkdirAll(filepath.Dir(snapshotDir), 0755); err != nil {
		t.Fatal(err)
	}
	if err := g.WorktreeAddDetached(ctx, cloneDir, snapshotDir, sha); err != nil {
		t.Fatalf("WorktreeAddDetached: %v", err)
	}
	worktrees, err = g.WorktreeList(ctx, cloneDir)
	if err != nil || len(worktrees) != 2 || !worktrees[1].Detached {
		t.Fatalf("WorktreeList after detached add = %+v, %v", worktrees, err)
	}
	snapshotSHA, err := g.HeadCommit(ctx, snapshotDir)
	if err != nil || snapshotSHA != sha {
		t.Fatalf("snapshot HEAD = %q, %v; want %q", snapshotSHA, err, sha)
	}
	if err := g.WorktreeRemove(ctx, cloneDir, snapshotDir); err != nil {
		t.Fatalf("WorktreeRemove snapshot: %v", err)
	}
}

func runCmd(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, out)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
