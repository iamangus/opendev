package manager

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/iamangus/code-mcp/internal/gitops"
)

func newTestManager(t *testing.T) (*Manager, *gitops.Fake) {
	t.Helper()
	dir := t.TempDir()
	fake := gitops.NewFake()
	mgr, err := New(dir, fake, slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return mgr, fake
}

// createFakeRepo creates a normal clone layout on disk so the manager finds it.
func createFakeRepo(t *testing.T, mgr *Manager, name string) string {
	t.Helper()
	repoDir := mgr.RepoDir(name)
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	return repoDir
}

func TestNew_CreatesMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sub", "repos")
	fake := gitops.NewFake()
	_, err := New(dir, fake, slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir not created: %v", err)
	}
}

func TestRepoDir(t *testing.T) {
	mgr, _ := newTestManager(t)
	got := mgr.RepoDir("myapp")
	want := filepath.Join(mgr.ReposDir(), "myapp")
	if got != want {
		t.Errorf("RepoDir = %q, want %q", got, want)
	}
}

func TestBranchWorktreeDir(t *testing.T) {
	mgr, _ := newTestManager(t)
	got := mgr.BranchWorktreeDir("myapp", "feature")
	want := filepath.Join(mgr.ReposDir(), "myapp", ".opendev", "worktrees", "feature")
	if got != want {
		t.Errorf("BranchWorktreeDir = %q, want %q", got, want)
	}
}

func TestBranchWorktreeDirKeepsSlashBranchesSingleSegment(t *testing.T) {
	mgr, _ := newTestManager(t)
	got := mgr.BranchWorktreeDir("myapp", "opendev/job-1")
	want := filepath.Join(mgr.ReposDir(), "myapp", ".opendev", "worktrees", "opendev-job-1")
	if got != want {
		t.Errorf("BranchWorktreeDir = %q, want %q", got, want)
	}
}

func TestCommitReturnsHeadSHA(t *testing.T) {
	mgr, fake := newTestManager(t)
	fake.StringReturns["HeadCommit"] = "abc123"
	sha, err := mgr.Commit("/work/task")
	if err != nil {
		t.Fatal(err)
	}
	if sha != "abc123" || !fake.HasCall("Commit") || !fake.HasCall("HeadCommit") {
		t.Fatalf("Commit = %q, calls=%+v", sha, fake.Calls)
	}
}

func TestRepositoryMetadata(t *testing.T) {
	mgr, fake := newTestManager(t)
	repoDir := createFakeRepo(t, mgr, "repo")
	fake.StringReturns["OriginURL"] = "https://github.com/test/repo.git"
	fake.StringReturns["HeadCommit"] = "abc123"

	metadata, err := mgr.RepositoryMetadata("repo")
	if err != nil {
		t.Fatal(err)
	}
	if metadata.OriginURL != "https://github.com/test/repo.git" || metadata.HeadSHA != "abc123" {
		t.Fatalf("RepositoryMetadata = %+v", metadata)
	}
	for _, method := range []string{"OriginURL", "HeadCommit"} {
		if !fake.HasCall(method) {
			t.Errorf("expected %s to be called", method)
		}
	}
	for _, call := range fake.Calls {
		if (call.Method == "OriginURL" || call.Method == "HeadCommit") && call.Args[0] != repoDir {
			t.Errorf("%s path = %q, want %q", call.Method, call.Args[0], repoDir)
		}
	}
}

func TestRepositoryMetadataMissingRepo(t *testing.T) {
	mgr, _ := newTestManager(t)
	if _, err := mgr.RepositoryMetadata("missing"); err == nil {
		t.Fatal("expected error for missing repo")
	}
}

func TestReferenceGitOperations(t *testing.T) {
	mgr, fake := newTestManager(t)
	sourceDir := createFakeRepo(t, mgr, "source")
	snapshotDir := filepath.Join(t.TempDir(), "snapshot")
	if err := os.MkdirAll(filepath.Join(snapshotDir, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	fake.StringReturns["Status"] = ""
	fake.StringReturns["ResolveRevision"] = "abc123"
	fake.Worktrees = []gitops.Worktree{{Path: snapshotDir, Detached: true}}

	if status, err := mgr.Status(sourceDir); err != nil || status != "" {
		t.Fatalf("Status = %q, %v", status, err)
	}
	if sha, err := mgr.ResolveRevision(sourceDir, "main"); err != nil || sha != "abc123" {
		t.Fatalf("ResolveRevision = %q, %v", sha, err)
	}
	if err := mgr.AddDetachedWorktree(sourceDir, snapshotDir, "abc123"); err != nil {
		t.Fatal(err)
	}
	if detached, err := mgr.IsDetachedWorktree(sourceDir, snapshotDir); err != nil || !detached {
		t.Fatalf("IsDetachedWorktree = %t, %v", detached, err)
	}
	for _, method := range []string{"Status", "ResolveRevision", "WorktreeAddDetached", "WorktreeList"} {
		if !fake.HasCall(method) {
			t.Errorf("expected %s to be called", method)
		}
	}
}

func TestSyncRepo_Clone(t *testing.T) {
	mgr, fake := newTestManager(t)
	if err := mgr.SyncRepo("https://github.com/test/repo.git", "repo"); err != nil {
		t.Fatalf("SyncRepo: %v", err)
	}
	if !fake.HasCall("Clone") {
		t.Error("expected Clone to be called")
	}
	if !fake.HasCall("EnsureLocalExcludes") {
		t.Error("expected local excludes to be configured")
	}
}

func TestSyncRepo_FetchExisting(t *testing.T) {
	mgr, fake := newTestManager(t)
	createFakeRepo(t, mgr, "repo")
	fake.StringReturns["DefaultBranch"] = "main"
	if err := mgr.SyncRepo("https://github.com/test/repo.git", "repo"); err != nil {
		t.Fatalf("SyncRepo: %v", err)
	}
	if !fake.HasCall("Fetch") {
		t.Error("expected Fetch to be called")
	}
	if !fake.HasCall("FastForward") {
		t.Error("expected existing repository to fast-forward")
	}
	if fake.HasCall("Clone") {
		t.Error("should not Clone existing repo")
	}
}

func TestSyncRepo_RefusesDirtyPrimaryMirror(t *testing.T) {
	mgr, fake := newTestManager(t)
	createFakeRepo(t, mgr, "repo")
	fake.StringReturns["Status"] = " M changed.txt"
	if err := mgr.SyncRepo("https://github.com/test/repo.git", "repo"); err == nil {
		t.Fatal("SyncRepo succeeded for a dirty primary mirror")
	}
	if fake.HasCall("FastForward") {
		t.Fatal("dirty primary mirror was fast-forwarded")
	}
}

func TestRemoveRepo(t *testing.T) {
	mgr, fake := newTestManager(t)
	repoDir := createFakeRepo(t, mgr, "repo")
	wtDir := mgr.BranchWorktreeDir("repo", "feat")
	os.MkdirAll(wtDir, 0755)
	fake.Worktrees = []gitops.Worktree{
		{Path: repoDir, Branch: "main"},
		{Path: wtDir, Branch: "feat"},
		{Path: filepath.Join(mgr.ReposDir(), "external-worktree"), Branch: "external"},
	}

	if err := mgr.RemoveRepo("repo"); err != nil {
		t.Fatalf("RemoveRepo: %v", err)
	}
	if _, err := os.Stat(mgr.RepoDir("repo")); !os.IsNotExist(err) {
		t.Error("repo dir should be removed")
	}
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Error("worktree dir should be removed")
	}
	for _, call := range fake.Calls {
		if call.Method == "WorktreeRemove" && call.Args[1] != wtDir {
			t.Errorf("only managed worktrees may be removed, got %q", call.Args[1])
		}
	}
}

func TestRemoveWorktreeDoesNotRemovePrimaryClone(t *testing.T) {
	mgr, fake := newTestManager(t)
	createFakeRepo(t, mgr, "repo")

	if err := mgr.RemoveWorktree("repo", "main"); err == nil {
		t.Fatal("expected primary clone not to be treated as a worktree")
	}
	if fake.HasCall("WorktreeRemove") {
		t.Fatal("primary clone must never be removed as a worktree")
	}
}

func TestRemoveRepo_NotFound(t *testing.T) {
	mgr, _ := newTestManager(t)
	if err := mgr.RemoveRepo("nope"); err == nil {
		t.Fatal("expected error")
	}
}

func TestCreateWorktree_NewBranch(t *testing.T) {
	mgr, fake := newTestManager(t)
	createFakeRepo(t, mgr, "repo")
	fake.BoolReturns["BranchExists"] = false

	dir, err := mgr.CreateWorktree("repo", "feature", "")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	if dir == "" {
		t.Error("expected non-empty dir")
	}
	if !fake.HasCall("CreateBranch") {
		t.Error("expected CreateBranch for new branch")
	}
	if !fake.HasCall("WorktreeAdd") {
		t.Error("expected WorktreeAdd")
	}
}

func TestCreateWorktree_DefaultBranch(t *testing.T) {
	mgr, fake := newTestManager(t)
	createFakeRepo(t, mgr, "repo")
	fake.StringReturns["DefaultBranch"] = "main"
	fake.BoolReturns["BranchExists"] = true

	dir, err := mgr.CreateWorktree("repo", "main", "")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	want := mgr.BranchWorktreeDir("repo", "main")
	if dir != want {
		t.Errorf("expected worktree dir %q for default branch, got %q", want, dir)
	}
	if !fake.HasCall("WorktreeAdd") {
		t.Error("expected WorktreeAdd for default branch")
	}
}

func TestCreateWorktree_InvalidBranch(t *testing.T) {
	mgr, _ := newTestManager(t)
	createFakeRepo(t, mgr, "repo")
	if _, err := mgr.CreateWorktree("repo", "bad branch!", ""); err == nil {
		t.Fatal("expected error for invalid branch name")
	}
}

func TestCreateWorktree_AlreadyExists(t *testing.T) {
	mgr, _ := newTestManager(t)
	createFakeRepo(t, mgr, "repo")
	wtDir := mgr.BranchWorktreeDir("repo", "feat")
	os.MkdirAll(wtDir, 0755)

	dir, err := mgr.CreateWorktree("repo", "feat", "")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	if dir == "" {
		t.Error("expected non-empty dir for existing worktree")
	}
}

func TestMergeBranch_Conflict(t *testing.T) {
	mgr, fake := newTestManager(t)
	createFakeRepo(t, mgr, "repo")
	fake.StringReturns["DefaultBranch"] = "main"
	fake.Errors["Merge"] = &MergeConflictError{Output: "CONFLICT (content): Merge conflict in file.go"}

	targetDir := mgr.BranchWorktreeDir("repo", "main")
	os.MkdirAll(targetDir, 0755)

	err := mgr.MergeBranch("repo", "feature", "main")
	if err == nil {
		t.Fatal("expected merge conflict error")
	}
	if _, ok := err.(*MergeConflictError); !ok {
		t.Errorf("expected MergeConflictError, got %T: %v", err, err)
	}
}

func TestGetCommits(t *testing.T) {
	mgr, fake := newTestManager(t)
	createFakeRepo(t, mgr, "repo")
	fake.StringReturns["DefaultBranch"] = "main"
	fake.StringReturns["CommitLog"] = "abc1234 first commit\ndef5678 second commit"

	wtDir := mgr.BranchWorktreeDir("repo", "feature")
	os.MkdirAll(wtDir, 0755)

	commits, err := mgr.GetCommits("repo", "feature")
	if err != nil {
		t.Fatalf("GetCommits: %v", err)
	}
	if len(commits) != 2 {
		t.Fatalf("expected 2 commits, got %d", len(commits))
	}
	if commits[0].Hash != "abc1234" {
		t.Errorf("commit[0].Hash = %q, want abc1234", commits[0].Hash)
	}
	if commits[0].Subject != "first commit" {
		t.Errorf("commit[0].Subject = %q, want 'first commit'", commits[0].Subject)
	}
}

func TestScan_EmptyDir(t *testing.T) {
	mgr, _ := newTestManager(t)
	repos, err := mgr.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(repos) != 0 {
		t.Errorf("expected 0 repos, got %d", len(repos))
	}
}

func TestScan_DiscoverRepo(t *testing.T) {
	mgr, fake := newTestManager(t)
	repoDir := createFakeRepo(t, mgr, "myapp")
	fake.StringReturns["DefaultBranch"] = "main"
	fake.Worktrees = []gitops.Worktree{
		{Path: repoDir, Branch: "main"},
		{Path: mgr.BranchWorktreeDir("myapp", "task"), Branch: "task"},
	}

	repos, err := mgr.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("expected 1 repo, got %d", len(repos))
	}
	if repos[0].Name != "myapp" {
		t.Errorf("repo name = %q, want myapp", repos[0].Name)
	}
	if len(repos[0].Branches) != 2 {
		t.Errorf("branches = %+v, want primary clone and managed worktree", repos[0].Branches)
	}
}

func TestScan_DiscoversGitFileAndIgnoresOpenDev(t *testing.T) {
	mgr, _ := newTestManager(t)
	linkedDir := filepath.Join(mgr.ReposDir(), "linked")
	if err := os.MkdirAll(linkedDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linkedDir, ".git"), []byte("gitdir: elsewhere\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(mgr.ReposDir(), ".opendev", ".git"), 0755); err != nil {
		t.Fatal(err)
	}

	repos, err := mgr.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(repos) != 1 || repos[0].Name != "linked" {
		t.Fatalf("repos = %+v, want only linked", repos)
	}
}

func TestListBranchesUsesGitWorktreeList(t *testing.T) {
	mgr, fake := newTestManager(t)
	repoDir := createFakeRepo(t, mgr, "repo")
	worktreeDir := mgr.BranchWorktreeDir("repo", "feature")
	fake.Worktrees = []gitops.Worktree{
		{Path: repoDir, Branch: "main"},
		{Path: worktreeDir, Branch: "feature"},
	}

	branches, err := mgr.ListBranches("repo")
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	if len(branches) != 2 || branches[0].Dir != repoDir || branches[1].Dir != worktreeDir {
		t.Fatalf("branches = %+v", branches)
	}
	if !fake.HasCall("WorktreeList") {
		t.Error("expected Git worktree list")
	}
}
