package gitops

import (
	"context"
	"sync"
)

type FakeCall struct {
	Method string
	Args   []string
}

type Fake struct {
	mu    sync.Mutex
	Calls []FakeCall

	Errors        map[string]error
	StringReturns map[string]string
	BoolReturns   map[string]bool
	Worktrees     []Worktree
}

func NewFake() *Fake {
	return &Fake{
		Errors:        make(map[string]error),
		StringReturns: make(map[string]string),
		BoolReturns:   make(map[string]bool),
	}
}

func (f *Fake) record(method string, args ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, FakeCall{Method: method, Args: args})
}

func (f *Fake) err(method string) error {
	if e, ok := f.Errors[method]; ok {
		return e
	}
	return nil
}

func (f *Fake) str(method string) string {
	if s, ok := f.StringReturns[method]; ok {
		return s
	}
	return ""
}

func (f *Fake) Clone(_ context.Context, url, dir string) error {
	f.record("Clone", url, dir)
	return f.err("Clone")
}

func (f *Fake) Fetch(_ context.Context, dir string) error {
	f.record("Fetch", dir)
	return f.err("Fetch")
}

func (f *Fake) FastForward(_ context.Context, dir, branch string) error {
	f.record("FastForward", dir, branch)
	return f.err("FastForward")
}

func (f *Fake) Checkout(_ context.Context, dir, branch string) error {
	f.record("Checkout", dir, branch)
	return f.err("Checkout")
}

func (f *Fake) WorktreeAdd(_ context.Context, repoDir, wtDir, branch string) error {
	f.record("WorktreeAdd", repoDir, wtDir, branch)
	return f.err("WorktreeAdd")
}

func (f *Fake) WorktreeRemove(_ context.Context, repoDir, wtDir string) error {
	f.record("WorktreeRemove", repoDir, wtDir)
	return f.err("WorktreeRemove")
}

func (f *Fake) WorktreeList(_ context.Context, repoDir string) ([]Worktree, error) {
	f.record("WorktreeList", repoDir)
	return f.Worktrees, f.err("WorktreeList")
}

func (f *Fake) EnsureLocalExcludes(_ context.Context, dir string, patterns []string) error {
	f.record("EnsureLocalExcludes", append([]string{dir}, patterns...)...)
	return f.err("EnsureLocalExcludes")
}

func (f *Fake) Merge(_ context.Context, dir, branch string) error {
	f.record("Merge", dir, branch)
	return f.err("Merge")
}

func (f *Fake) Push(_ context.Context, dir, branch string) error {
	f.record("Push", dir, branch)
	return f.err("Push")
}

func (f *Fake) Commit(_ context.Context, dir, message string) error {
	f.record("Commit", dir, message)
	return f.err("Commit")
}

func (f *Fake) HeadCommit(_ context.Context, dir string) (string, error) {
	f.record("HeadCommit", dir)
	return f.str("HeadCommit"), f.err("HeadCommit")
}

func (f *Fake) OriginURL(_ context.Context, dir string) (string, error) {
	f.record("OriginURL", dir)
	return f.str("OriginURL"), f.err("OriginURL")
}

func (f *Fake) CherryPick(_ context.Context, dir, commit string) error {
	f.record("CherryPick", dir, commit)
	return f.err("CherryPick")
}

func (f *Fake) Diff(_ context.Context, dir string) (string, error) {
	f.record("Diff", dir)
	return f.str("Diff"), f.err("Diff")
}

func (f *Fake) CommitLog(_ context.Context, dir string, args ...string) (string, error) {
	allArgs := append([]string{dir}, args...)
	f.record("CommitLog", allArgs...)
	return f.str("CommitLog"), f.err("CommitLog")
}

func (f *Fake) DefaultBranch(_ context.Context, dir string) (string, error) {
	f.record("DefaultBranch", dir)
	s := f.str("DefaultBranch")
	if s == "" {
		s = "main"
	}
	return s, f.err("DefaultBranch")
}

func (f *Fake) BranchExists(_ context.Context, dir, branch string) (bool, error) {
	f.record("BranchExists", dir, branch)
	return f.BoolReturns["BranchExists"], f.err("BranchExists")
}

func (f *Fake) RemoteBranchExists(_ context.Context, dir, branch string) (bool, error) {
	f.record("RemoteBranchExists", dir, branch)
	return f.BoolReturns["RemoteBranchExists"], f.err("RemoteBranchExists")
}

func (f *Fake) CreateBranch(_ context.Context, dir, branch, startPoint string) error {
	f.record("CreateBranch", dir, branch, startPoint)
	return f.err("CreateBranch")
}

func (f *Fake) Status(_ context.Context, dir string) (string, error) {
	f.record("Status", dir)
	return f.str("Status"), f.err("Status")
}

func (f *Fake) ResolveRevision(_ context.Context, dir, revision string) (string, error) {
	f.record("ResolveRevision", dir, revision)
	return f.str("ResolveRevision"), f.err("ResolveRevision")
}

func (f *Fake) WorktreeAddDetached(_ context.Context, repoDir, wtDir, revision string) error {
	f.record("WorktreeAddDetached", repoDir, wtDir, revision)
	return f.err("WorktreeAddDetached")
}

func (f *Fake) HasCall(method string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.Calls {
		if c.Method == method {
			return true
		}
	}
	return false
}

func (f *Fake) CallCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.Calls {
		if c.Method == method {
			n++
		}
	}
	return n
}

// Compile-time check that Fake implements GitOps.
var _ GitOps = (*Fake)(nil)
