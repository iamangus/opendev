package gitops

import "context"

// Worktree describes a Git worktree reported by Git.
type Worktree struct {
	Path     string
	Branch   string
	Detached bool
}

type GitOps interface {
	Clone(ctx context.Context, url, dir string) error
	Fetch(ctx context.Context, dir string) error
	FastForward(ctx context.Context, dir, branch string) error
	Checkout(ctx context.Context, dir, branch string) error
	WorktreeAdd(ctx context.Context, repoDir, wtDir, branch string) error
	WorktreeRemove(ctx context.Context, repoDir, wtDir string) error
	WorktreeList(ctx context.Context, repoDir string) ([]Worktree, error)
	EnsureLocalExcludes(ctx context.Context, dir string, patterns []string) error
	Merge(ctx context.Context, dir, branch string) error
	Push(ctx context.Context, dir, branch string) error
	Commit(ctx context.Context, dir, message string) error
	HeadCommit(ctx context.Context, dir string) (string, error)
	OriginURL(ctx context.Context, dir string) (string, error)
	CherryPick(ctx context.Context, dir, commit string) error
	Diff(ctx context.Context, dir string) (string, error)
	CommitLog(ctx context.Context, dir string, args ...string) (string, error)
	DefaultBranch(ctx context.Context, dir string) (string, error)
	BranchExists(ctx context.Context, dir, branch string) (bool, error)
	RemoteBranchExists(ctx context.Context, dir, branch string) (bool, error)
	CreateBranch(ctx context.Context, dir, branch, startPoint string) error
	Status(ctx context.Context, dir string) (string, error)
	ResolveRevision(ctx context.Context, dir, revision string) (string, error)
	WorktreeAddDetached(ctx context.Context, repoDir, wtDir, revision string) error
}
