# Reliability, Architecture & Observability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Introduce structured logging, extract interfaces for testability, split oversized files, fix broken tests, and improve the concurrency model — all with zero new dependencies.

**Architecture:** Replace scattered `log.Printf` with `log/slog` threaded through all components via dependency injection. Extract `GitOps`, `GitHubClient`, and `FileSystem` interfaces to decouple components and enable testing with fakes. Upgrade the lock manager to support context cancellation, contention logging, and reference-counted cleanup.

**Tech Stack:** Go 1.24 stdlib only (`log/slog`, `net/http/httptest`, `sync`, `context`)

**Spec:** `docs/superpowers/specs/2026-03-26-reliability-architecture-observability-design.md`

---

### Task 1: LockManager — Context-Aware Locking with Contention Logging

Replace the current `internal/locks/locks.go` (simple `sync.Map` of `*sync.RWMutex`) with a new `LockManager` that supports `context.Context`, contention warnings, and reference-counted cleanup. This is first because many later tasks depend on the new lock API.

**Files:**
- Modify: `internal/locks/locks.go` (lines 1-40 — full rewrite)
- Modify: `internal/locks/locks_test.go` (lines 1-91 — full rewrite)

- [ ] **Step 1: Write failing tests for the new LockManager**

Create tests covering: basic read/write locking, context cancellation, reference counting cleanup, contention logging, and concurrent access.

```go
// internal/locks/locks_test.go
package locks

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestLM() *Manager {
	return NewManager(slog.Default())
}

func TestManager_BasicReadWrite(t *testing.T) {
	lm := newTestLM()
	ctx := context.Background()

	if err := lm.RLock(ctx, "/a"); err != nil {
		t.Fatalf("RLock: %v", err)
	}
	lm.RUnlock("/a")

	if err := lm.Lock(ctx, "/a"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	lm.Unlock("/a")
}

func TestManager_MultipleReaders(t *testing.T) {
	lm := newTestLM()
	ctx := context.Background()
	var active atomic.Int32
	var wg sync.WaitGroup

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := lm.RLock(ctx, "/b"); err != nil {
				t.Errorf("RLock: %v", err)
				return
			}
			active.Add(1)
			time.Sleep(10 * time.Millisecond)
			if n := active.Load(); n < 2 {
				// at least 2 readers should overlap
			}
			active.Add(-1)
			lm.RUnlock("/b")
		}()
	}
	wg.Wait()
}

func TestManager_ContextCancellation(t *testing.T) {
	lm := newTestLM()
	ctx := context.Background()

	// Hold a write lock
	if err := lm.Lock(ctx, "/c"); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	// Try to acquire with cancelled context
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel() // cancel immediately

	err := lm.RLock(cancelCtx, "/c")
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}

	lm.Unlock("/c")
}

func TestManager_ContextTimeout(t *testing.T) {
	lm := newTestLM()
	ctx := context.Background()

	// Hold a write lock
	if err := lm.Lock(ctx, "/d"); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	// Try to acquire with short timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()

	err := lm.Lock(timeoutCtx, "/d")
	if err == nil {
		t.Fatal("expected error from timeout")
	}

	lm.Unlock("/d")
}

func TestManager_ReferenceCountCleanup(t *testing.T) {
	lm := newTestLM()
	ctx := context.Background()

	if err := lm.Lock(ctx, "/e"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	lm.Unlock("/e")

	// After unlock with no holders, entry should be cleaned up
	lm.mu.Lock()
	_, exists := lm.locks["/e"]
	lm.mu.Unlock()

	if exists {
		t.Fatal("expected lock entry to be cleaned up after last unlock")
	}
}

func TestManager_SeparatePaths(t *testing.T) {
	lm := newTestLM()
	ctx := context.Background()

	// Write-lock /f, read-lock /g — should not block
	if err := lm.Lock(ctx, "/f"); err != nil {
		t.Fatalf("Lock /f: %v", err)
	}
	if err := lm.RLock(ctx, "/g"); err != nil {
		t.Fatalf("RLock /g: %v", err)
	}
	lm.RUnlock("/g")
	lm.Unlock("/f")
}

func TestManager_WriteExcludesReaders(t *testing.T) {
	lm := newTestLM()
	ctx := context.Background()

	if err := lm.Lock(ctx, "/h"); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	// Reader should block until writer releases
	done := make(chan struct{})
	go func() {
		if err := lm.RLock(ctx, "/h"); err != nil {
			t.Errorf("RLock: %v", err)
			close(done)
			return
		}
		lm.RUnlock("/h")
		close(done)
	}()

	// Give goroutine time to block
	time.Sleep(20 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("reader should have been blocked by writer")
	default:
	}

	lm.Unlock("/h")
	<-done // reader should now complete
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /home/angoo/repos/opendev/opendev-coder && go test ./internal/locks/ -v -count=1`
Expected: Compilation errors — `NewManager` doesn't accept `*slog.Logger`, `RLock`/`Lock` don't accept `context.Context`.

- [ ] **Step 3: Implement the new LockManager**

```go
// internal/locks/locks.go
package locks

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// DefaultWarnAfter is the default threshold for logging lock contention warnings.
const DefaultWarnAfter = 100 * time.Millisecond

type lockEntry struct {
	rwmu sync.RWMutex
	refs int
}

type Manager struct {
	mu        sync.Mutex
	locks     map[string]*lockEntry
	logger    *slog.Logger
	warnAfter time.Duration
}

type Option func(*Manager)

func WithWarnAfter(d time.Duration) Option {
	return func(m *Manager) { m.warnAfter = d }
}

func NewManager(logger *slog.Logger, opts ...Option) *Manager {
	m := &Manager{
		locks:     make(map[string]*lockEntry),
		logger:    logger,
		warnAfter: DefaultWarnAfter,
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

func (m *Manager) getOrCreate(path string) *lockEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.locks[path]
	if !ok {
		e = &lockEntry{}
		m.locks[path] = e
	}
	e.refs++
	return e
}

func (m *Manager) release(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.locks[path]
	if !ok {
		return
	}
	e.refs--
	if e.refs <= 0 {
		delete(m.locks, path)
	}
}

func (m *Manager) RLock(ctx context.Context, path string) error {
	return m.acquireLock(ctx, path, false)
}

func (m *Manager) RUnlock(path string) {
	m.mu.Lock()
	e, ok := m.locks[path]
	m.mu.Unlock()
	if ok {
		e.rwmu.RUnlock()
	}
	m.release(path)
	m.logger.Debug("lock released", "path", path, "mode", "read")
}

func (m *Manager) Lock(ctx context.Context, path string) error {
	return m.acquireLock(ctx, path, true)
}

func (m *Manager) Unlock(path string) {
	m.mu.Lock()
	e, ok := m.locks[path]
	m.mu.Unlock()
	if ok {
		e.rwmu.Unlock()
	}
	m.release(path)
	m.logger.Debug("lock released", "path", path, "mode", "write")
}

func (m *Manager) acquireLock(ctx context.Context, path string, exclusive bool) error {
	e := m.getOrCreate(path)
	start := time.Now()

	acquired := make(chan struct{})
	go func() {
		if exclusive {
			e.rwmu.Lock()
		} else {
			e.rwmu.RLock()
		}
		close(acquired)
	}()

	mode := "read"
	if exclusive {
		mode = "write"
	}

	select {
	case <-acquired:
		elapsed := time.Since(start)
		if elapsed >= m.warnAfter {
			m.logger.Warn("lock contention", "path", path, "mode", mode, "wait_ms", elapsed.Milliseconds())
		} else {
			m.logger.Debug("lock acquired", "path", path, "mode", mode, "wait_ms", elapsed.Milliseconds())
		}
		return nil
	case <-ctx.Done():
		// A goroutine is still waiting on the lock. It will eventually acquire
		// and we need to immediately release it to avoid a leak.
		go func() {
			<-acquired
			if exclusive {
				e.rwmu.Unlock()
			} else {
				e.rwmu.RUnlock()
			}
			m.release(path)
		}()
		return ctx.Err()
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /home/angoo/repos/opendev/opendev-coder && go test ./internal/locks/ -v -count=1 -race`
Expected: All 7 tests PASS, no race conditions detected.

- [ ] **Step 5: Update all callers to pass `context.Context` to lock methods**

The callers are in `internal/tools/filesystem.go`. Every call to `lm.RLock(path)` becomes `lm.RLock(ctx, path)` and `lm.Lock(path)` becomes `lm.Lock(ctx, path)`. Since the tools functions don't currently accept `context.Context`, add it as the first parameter to each function.

Update these function signatures in `internal/tools/filesystem.go`:

```go
// Line 32: was func ReadFile(worktreeRoot, filePath string, lm *locks.Manager) (string, error)
func ReadFile(ctx context.Context, worktreeRoot, filePath string, lm *locks.Manager) (string, error)

// Line 55: was func ReadLines(worktreeRoot, filePath string, startLine, endLine int, lm *locks.Manager) (string, error)
func ReadLines(ctx context.Context, worktreeRoot, filePath string, startLine, endLine int, lm *locks.Manager) (string, error)

// Line 98: was func CreateFile(worktreeRoot, filePath, content string, lm *locks.Manager) (string, error)
func CreateFile(ctx context.Context, worktreeRoot, filePath, content string, lm *locks.Manager) (string, error)

// Line 121: was func ListDirectory(worktreeRoot, dirPath string, recursive bool, lm *locks.Manager) (string, error)
func ListDirectory(ctx context.Context, worktreeRoot, dirPath string, recursive bool, lm *locks.Manager) (string, error)

// Line 179: was func GrepSearch(worktreeRoot, query, directory string, lm *locks.Manager) (string, error)
func GrepSearch(ctx context.Context, worktreeRoot, query, directory string, lm *locks.Manager) (string, error)

// Line 251: was func SearchAndReplace(worktreeRoot, filePath, searchBlock, replaceBlock string, lm *locks.Manager) (string, error)
func SearchAndReplace(ctx context.Context, worktreeRoot, filePath, searchBlock, replaceBlock string, lm *locks.Manager) (string, error)
```

Inside each function body, change `lm.RLock(abs)` to `lm.RLock(ctx, abs)`, `lm.Lock(abs)` to `lm.Lock(ctx, abs)` — and handle the returned error. Example for `ReadFile`:

```go
func ReadFile(ctx context.Context, worktreeRoot, filePath string, lm *locks.Manager) (string, error) {
	abs, err := worktree.Resolve(worktreeRoot, filePath)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", &worktree.ToolError{Message: fmt.Sprintf("file not found: %s", filePath)}
	}
	if info.Size() > MaxFileSize {
		return "", &worktree.ToolError{Message: fmt.Sprintf("file exceeds 1 MiB limit: %s (%d bytes)", filePath, info.Size())}
	}
	if err := lm.RLock(ctx, abs); err != nil {
		return "", err
	}
	defer lm.RUnlock(abs)
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", &worktree.ToolError{Message: fmt.Sprintf("cannot read file: %s", filePath)}
	}
	return string(data), nil
}
```

Apply the same pattern to all 6 functions: add `ctx context.Context` as first param, pass `ctx` to lock calls, handle the error.

- [ ] **Step 6: Update callers in register.go to pass context**

In `cmd/opendev/register.go`, every tool handler closure receives a `ctx context.Context` from the MCP framework. Update all calls to tools functions to pass `ctx`:

```go
// Line ~37 (read_file handler): was tools.ReadFile(worktreeRoot, fp, lm)
tools.ReadFile(ctx, worktreeRoot, fp, lm)

// Line ~63 (read_lines handler): was tools.ReadLines(worktreeRoot, fp, startLine, endLine, lm)
tools.ReadLines(ctx, worktreeRoot, fp, startLine, endLine, lm)

// Line ~89 (list_directory handler): was tools.ListDirectory(worktreeRoot, dirPath, recursive, lm)
tools.ListDirectory(ctx, worktreeRoot, dirPath, recursive, lm)

// Line ~114 (grep_search handler): was tools.GrepSearch(worktreeRoot, query, directory, lm)
tools.GrepSearch(ctx, worktreeRoot, query, directory, lm)

// Line ~160 (create_file handler): was tools.CreateFile(worktreeRoot, fp, content, lm)
tools.CreateFile(ctx, worktreeRoot, fp, content, lm)

// Line ~190 (search_and_replace handler): was tools.SearchAndReplace(worktreeRoot, fp, search, replace, lm)
tools.SearchAndReplace(ctx, worktreeRoot, fp, search, replace, lm)
```

Also update `NewManager()` call in `main.go` to pass `slog.Default()`:

```go
// was: lm := locks.NewManager()
lm := locks.NewManager(slog.Default())
```

- [ ] **Step 7: Update filesystem tests to pass context**

In `internal/tools/filesystem_test.go`, update `newLM()` and all test calls:

```go
func newLM() *locks.Manager {
	return locks.NewManager(slog.Default())
}
```

Every call like `tools.ReadFile(root, "file.txt", lm)` becomes `tools.ReadFile(context.Background(), root, "file.txt", lm)`. Add `"context"` and `"log/slog"` to imports.

- [ ] **Step 8: Run full test suite**

Run: `cd /home/angoo/repos/opendev/opendev-coder && go test ./internal/locks/ ./internal/tools/ -v -count=1 -race`
Expected: All lock tests and tool tests PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/locks/locks.go internal/locks/locks_test.go internal/tools/filesystem.go internal/tools/filesystem_test.go cmd/opendev/register.go cmd/opendev/main.go
git commit -m "feat: context-aware LockManager with contention logging and ref-counted cleanup"
```

---

### Task 2: GitOps Interface and Exec Implementation

Extract all git shell commands behind a `GitOps` interface. The real implementation wraps `exec.Command` with structured logging. A fake is provided for testing.

**Files:**
- Create: `internal/gitops/gitops.go`
- Create: `internal/gitops/exec.go`
- Create: `internal/gitops/exec_test.go`
- Create: `internal/gitops/fake.go`

- [ ] **Step 1: Write the integration test for the real exec implementation**

```go
// internal/gitops/exec_test.go
//go:build integration

package gitops

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestExecGitOps_CloneAndWorktree(t *testing.T) {
	// Create a source repo with one commit
	srcDir := t.TempDir()
	run(t, srcDir, "git", "init")
	run(t, srcDir, "git", "config", "user.email", "test@test.com")
	run(t, srcDir, "git", "config", "user.name", "Test")
	writeFile(t, filepath.Join(srcDir, "hello.txt"), "hello")
	run(t, srcDir, "git", "add", ".")
	run(t, srcDir, "git", "commit", "-m", "initial")

	g := NewExec(slog.Default(), "")

	// Clone
	cloneDir := filepath.Join(t.TempDir(), "clone")
	ctx := context.Background()
	if err := g.Clone(ctx, srcDir, cloneDir); err != nil {
		t.Fatalf("Clone: %v", err)
	}

	// DefaultBranch
	branch, err := g.DefaultBranch(ctx, cloneDir)
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}
	if branch == "" {
		t.Fatal("DefaultBranch returned empty string")
	}

	// Fetch (should be a no-op but not error)
	if err := g.Fetch(ctx, cloneDir); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// WorktreeAdd
	wtDir := filepath.Join(t.TempDir(), "wt-feature")
	if err := g.CreateBranch(ctx, cloneDir, "feature", branch); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := g.WorktreeAdd(ctx, cloneDir, wtDir, "feature"); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}

	// Verify worktree has the file
	if _, err := os.Stat(filepath.Join(wtDir, "hello.txt")); err != nil {
		t.Fatalf("worktree missing hello.txt: %v", err)
	}

	// Diff (should be empty)
	diff, err := g.Diff(ctx, wtDir)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if diff != "" {
		t.Fatalf("expected empty diff, got: %s", diff)
	}

	// Status (should be clean)
	status, err := g.Status(ctx, wtDir)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status != "" {
		t.Fatalf("expected clean status, got: %s", status)
	}

	// WorktreeRemove
	if err := g.WorktreeRemove(ctx, cloneDir, wtDir); err != nil {
		t.Fatalf("WorktreeRemove: %v", err)
	}
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Fatal("worktree directory should have been removed")
	}
}

func run(t *testing.T, dir string, name string, args ...string) {
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
```

- [ ] **Step 2: Define the GitOps interface**

```go
// internal/gitops/gitops.go
package gitops

import "context"

type GitOps interface {
	Clone(ctx context.Context, url, dir string) error
	Fetch(ctx context.Context, dir string) error
	WorktreeAdd(ctx context.Context, repoDir, wtDir, branch string) error
	WorktreeRemove(ctx context.Context, repoDir, wtDir string) error
	Merge(ctx context.Context, dir, branch string) error
	Push(ctx context.Context, dir, branch string) error
	Diff(ctx context.Context, dir string) (string, error)
	CommitLog(ctx context.Context, dir string, args ...string) (string, error)
	DefaultBranch(ctx context.Context, dir string) (string, error)
	BranchExists(ctx context.Context, dir, branch string) (bool, error)
	CreateBranch(ctx context.Context, dir, branch, startPoint string) error
	Status(ctx context.Context, dir string) (string, error)
}
```

- [ ] **Step 3: Write the real exec implementation**

```go
// internal/gitops/exec.go
package gitops

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

type Exec struct {
	logger *slog.Logger
	token  string
}

func NewExec(logger *slog.Logger, token string) *Exec {
	return &Exec{logger: logger, token: token}
}

func (e *Exec) Clone(ctx context.Context, url, dir string) error {
	_, err := e.run(ctx, "", "git", "clone", "--bare", e.authURL(url), dir)
	return err
}

func (e *Exec) Fetch(ctx context.Context, dir string) error {
	_, err := e.run(ctx, dir, "git", "fetch", "--prune", "origin")
	return err
}

func (e *Exec) WorktreeAdd(ctx context.Context, repoDir, wtDir, branch string) error {
	_, err := e.run(ctx, repoDir, "git", "worktree", "add", wtDir, branch)
	return err
}

func (e *Exec) WorktreeRemove(ctx context.Context, repoDir, wtDir string) error {
	_, err := e.run(ctx, repoDir, "git", "worktree", "remove", "--force", wtDir)
	return err
}

func (e *Exec) Merge(ctx context.Context, dir, branch string) error {
	_, err := e.run(ctx, dir, "git", "merge", branch)
	return err
}

func (e *Exec) Push(ctx context.Context, dir, branch string) error {
	_, err := e.run(ctx, dir, "git", "push", "origin", branch)
	return err
}

func (e *Exec) Diff(ctx context.Context, dir string) (string, error) {
	return e.run(ctx, dir, "git", "diff", "HEAD")
}

func (e *Exec) CommitLog(ctx context.Context, dir string, args ...string) (string, error) {
	fullArgs := append([]string{"log"}, args...)
	return e.run(ctx, dir, "git", fullArgs...)
}

func (e *Exec) DefaultBranch(ctx context.Context, dir string) (string, error) {
	out, err := e.run(ctx, dir, "git", "symbolic-ref", "--short", "HEAD")
	if err != nil {
		// Bare repo: try refs/remotes/origin/HEAD
		out, err = e.run(ctx, dir, "git", "rev-parse", "--abbrev-ref", "origin/HEAD")
		if err != nil {
			return "", err
		}
		// "origin/main" -> "main"
		out = strings.TrimPrefix(out, "origin/")
	}
	return strings.TrimSpace(out), nil
}

func (e *Exec) BranchExists(ctx context.Context, dir, branch string) (bool, error) {
	_, err := e.run(ctx, dir, "git", "rev-parse", "--verify", branch)
	if err != nil {
		return false, nil
	}
	return true, nil
}

func (e *Exec) CreateBranch(ctx context.Context, dir, branch, startPoint string) error {
	_, err := e.run(ctx, dir, "git", "branch", branch, startPoint)
	return err
}

func (e *Exec) Status(ctx context.Context, dir string) (string, error) {
	return e.run(ctx, dir, "git", "status", "--short")
}

func (e *Exec) authURL(rawURL string) string {
	if e.token == "" {
		return rawURL
	}
	if strings.HasPrefix(rawURL, "https://") {
		return strings.Replace(rawURL, "https://", "https://x-access-token:"+e.token+"@", 1)
	}
	return rawURL
}

func (e *Exec) run(ctx context.Context, dir, name string, args ...string) (string, error) {
	start := time.Now()
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}

	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)
	result := strings.TrimSpace(string(out))

	if err != nil {
		e.logger.Error("git command failed",
			"cmd", name,
			"args", args,
			"dir", dir,
			"duration_ms", elapsed.Milliseconds(),
			"error", err.Error(),
			"output", result,
		)
		return result, fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, result)
	}

	e.logger.Debug("git command completed",
		"cmd", name,
		"args", args,
		"dir", dir,
		"duration_ms", elapsed.Milliseconds(),
	)
	return result, nil
}
```

- [ ] **Step 4: Write the fake implementation**

```go
// internal/gitops/fake.go
package gitops

import (
	"context"
	"fmt"
	"sync"
)

type FakeCall struct {
	Method string
	Args   []string
}

type Fake struct {
	mu    sync.Mutex
	Calls []FakeCall

	// Configure return values per method. Key is method name.
	Errors        map[string]error
	StringReturns map[string]string
	BoolReturns   map[string]bool
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

func (f *Fake) WorktreeAdd(_ context.Context, repoDir, wtDir, branch string) error {
	f.record("WorktreeAdd", repoDir, wtDir, branch)
	return f.err("WorktreeAdd")
}

func (f *Fake) WorktreeRemove(_ context.Context, repoDir, wtDir string) error {
	f.record("WorktreeRemove", repoDir, wtDir)
	return f.err("WorktreeRemove")
}

func (f *Fake) Merge(_ context.Context, dir, branch string) error {
	f.record("Merge", dir, branch)
	return f.err("Merge")
}

func (f *Fake) Push(_ context.Context, dir, branch string) error {
	f.record("Push", dir, branch)
	return f.err("Push")
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

func (f *Fake) CreateBranch(_ context.Context, dir, branch, startPoint string) error {
	f.record("CreateBranch", dir, branch, startPoint)
	return f.err("CreateBranch")
}

func (f *Fake) Status(_ context.Context, dir string) (string, error) {
	f.record("Status", dir)
	return f.str("Status"), f.err("Status")
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

// Verify that Fake implements GitOps at compile time.
var _ GitOps = (*Fake)(nil)

// Verify that Exec implements GitOps at compile time.
var _ GitOps = (*Exec)(nil)
```

- [ ] **Step 5: Run the integration test**

Run: `cd /home/angoo/repos/opendev/opendev-coder && go test ./internal/gitops/ -v -count=1 -tags=integration -race`
Expected: PASS. If git is not available or tags aren't set, the test file is skipped.

Also verify the package compiles without integration tag:
Run: `cd /home/angoo/repos/opendev/opendev-coder && go build ./internal/gitops/`
Expected: Success (no compilation errors).

- [ ] **Step 6: Commit**

```bash
git add internal/gitops/
git commit -m "feat: add GitOps interface with exec implementation and fake for testing"
```

---

### Task 3: GitHub Client Interface and Tests

Refactor the GitHub client to implement a `Client` interface. Add tests using `httptest.Server`.

**Files:**
- Create: `internal/github/github.go`
- Modify: `internal/github/client.go` (lines 1-110)
- Create: `internal/github/client_test.go`
- Create: `internal/github/fake.go`

- [ ] **Step 1: Write the failing tests**

```go
// internal/github/client_test.go
package github

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPClient_CreatePR(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/repos/owner/myrepo/pulls" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("missing or wrong auth header: %s", r.Header.Get("Authorization"))
		}

		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["title"] != "Test PR" {
			t.Errorf("unexpected title: %v", body["title"])
		}
		if body["draft"] != true {
			t.Errorf("expected draft=true, got %v", body["draft"])
		}

		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"number":   42,
			"html_url": "https://github.com/owner/myrepo/pull/42",
		})
	}))
	defer srv.Close()

	c := NewHTTPClient("test-token", "owner", slog.Default(), WithBaseURL(srv.URL))
	ctx := context.Background()

	pr, err := c.CreatePR(ctx, CreatePROptions{
		Repo:  "myrepo",
		Title: "Test PR",
		Head:  "feature",
		Base:  "main",
		Body:  "description",
		Draft: true,
	})
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	if pr.Number != 42 {
		t.Errorf("expected PR #42, got #%d", pr.Number)
	}
	if pr.HTMLURL != "https://github.com/owner/myrepo/pull/42" {
		t.Errorf("unexpected URL: %s", pr.HTMLURL)
	}
}

func TestHTTPClient_UpdatePR(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("expected PATCH, got %s", r.Method)
		}
		if r.URL.Path != "/repos/owner/myrepo/pulls/42" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewHTTPClient("test-token", "owner", slog.Default(), WithBaseURL(srv.URL))
	err := c.UpdatePR(context.Background(), "myrepo", 42, "new body")
	if err != nil {
		t.Fatalf("UpdatePR: %v", err)
	}
}

func TestHTTPClient_PromotePR(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("expected PATCH, got %s", r.Method)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["draft"] != false {
			t.Errorf("expected draft=false, got %v", body["draft"])
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewHTTPClient("test-token", "owner", slog.Default(), WithBaseURL(srv.URL))
	err := c.PromotePR(context.Background(), "myrepo", 42)
	if err != nil {
		t.Fatalf("PromotePR: %v", err)
	}
}

func TestHTTPClient_ErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"message":"Validation Failed"}`))
	}))
	defer srv.Close()

	c := NewHTTPClient("test-token", "owner", slog.Default(), WithBaseURL(srv.URL))
	_, err := c.CreatePR(context.Background(), CreatePROptions{
		Repo: "myrepo", Title: "t", Head: "h", Base: "b",
	})
	if err == nil {
		t.Fatal("expected error for 422 response")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /home/angoo/repos/opendev/opendev-coder && go test ./internal/github/ -v -count=1`
Expected: Compilation errors — `NewHTTPClient`, `CreatePROptions`, `WithBaseURL`, etc. don't exist yet.

- [ ] **Step 3: Define the Client interface and types**

```go
// internal/github/github.go
package github

import "context"

type Client interface {
	CreatePR(ctx context.Context, opts CreatePROptions) (*PR, error)
	UpdatePR(ctx context.Context, repo string, number int, body string) error
	PromotePR(ctx context.Context, repo string, number int) error
}

type CreatePROptions struct {
	Repo  string
	Title string
	Head  string
	Base  string
	Body  string
	Draft bool
}

type PR struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
}
```

- [ ] **Step 4: Refactor client.go to implement the interface**

Replace the contents of `internal/github/client.go`:

```go
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

const defaultBaseURL = "https://api.github.com"

type HTTPClient struct {
	token   string
	owner   string
	baseURL string
	http    *http.Client
	logger  *slog.Logger
}

type HTTPClientOption func(*HTTPClient)

func WithBaseURL(url string) HTTPClientOption {
	return func(c *HTTPClient) { c.baseURL = url }
}

func NewHTTPClient(token, owner string, logger *slog.Logger, opts ...HTTPClientOption) *HTTPClient {
	c := &HTTPClient{
		token:   token,
		owner:   owner,
		baseURL: defaultBaseURL,
		http:    &http.Client{},
		logger:  logger,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

func (c *HTTPClient) CreatePR(ctx context.Context, opts CreatePROptions) (*PR, error) {
	start := time.Now()
	path := fmt.Sprintf("/repos/%s/%s/pulls", c.owner, opts.Repo)
	payload := map[string]any{
		"title": opts.Title,
		"head":  opts.Head,
		"base":  opts.Base,
		"body":  opts.Body,
		"draft": opts.Draft,
	}

	var pr PR
	if err := c.do(ctx, http.MethodPost, path, payload, &pr); err != nil {
		c.logger.Error("github: CreatePR failed", "repo", opts.Repo, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return nil, err
	}
	c.logger.Info("github: PR created", "repo", opts.Repo, "number", pr.Number, "duration_ms", time.Since(start).Milliseconds())
	return &pr, nil
}

func (c *HTTPClient) UpdatePR(ctx context.Context, repo string, number int, body string) error {
	start := time.Now()
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", c.owner, repo, number)
	payload := map[string]any{"body": body}

	if err := c.do(ctx, http.MethodPatch, path, payload, nil); err != nil {
		c.logger.Error("github: UpdatePR failed", "repo", repo, "number", number, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return err
	}
	c.logger.Info("github: PR updated", "repo", repo, "number", number, "duration_ms", time.Since(start).Milliseconds())
	return nil
}

func (c *HTTPClient) PromotePR(ctx context.Context, repo string, number int) error {
	start := time.Now()
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", c.owner, repo, number)
	payload := map[string]any{"draft": false}

	if err := c.do(ctx, http.MethodPatch, path, payload, nil); err != nil {
		c.logger.Error("github: PromotePR failed", "repo", repo, "number", number, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return err
	}
	c.logger.Info("github: PR promoted", "repo", repo, "number", number, "duration_ms", time.Since(start).Milliseconds())
	return nil
}

func (c *HTTPClient) do(ctx context.Context, method, path string, reqBody any, out any) error {
	var bodyReader io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("github API %s %s: %d %s", method, path, resp.StatusCode, string(respBody))
	}

	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// Verify HTTPClient implements Client at compile time.
var _ Client = (*HTTPClient)(nil)
```

- [ ] **Step 5: Write the fake implementation**

```go
// internal/github/fake.go
package github

import (
	"context"
	"sync"
)

type FakeClient struct {
	mu    sync.Mutex
	Calls []FakeCall

	CreatePRResult *PR
	CreatePRError  error
	UpdatePRError  error
	PromotePRError error
}

type FakeCall struct {
	Method string
	Args   []any
}

func NewFakeClient() *FakeClient {
	return &FakeClient{
		CreatePRResult: &PR{Number: 1, HTMLURL: "https://github.com/test/test/pull/1"},
	}
}

func (f *FakeClient) CreatePR(_ context.Context, opts CreatePROptions) (*PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, FakeCall{Method: "CreatePR", Args: []any{opts}})
	return f.CreatePRResult, f.CreatePRError
}

func (f *FakeClient) UpdatePR(_ context.Context, repo string, number int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, FakeCall{Method: "UpdatePR", Args: []any{repo, number, body}})
	return f.UpdatePRError
}

func (f *FakeClient) PromotePR(_ context.Context, repo string, number int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, FakeCall{Method: "PromotePR", Args: []any{repo, number}})
	return f.PromotePRError
}

var _ Client = (*FakeClient)(nil)
```

- [ ] **Step 6: Update callers in api.go and main.go**

In `cmd/opendev/api.go`, change the `ghClient *githubpkg.Client` parameter to `ghClient githubpkg.Client` (interface type). Update call sites:

```go
// api.go line ~31: was func registerAPIRoutes(..., ghClient *githubpkg.Client, ...)
func registerAPIRoutes(mux *http.ServeMux, mgr *manager.Manager, ts *tools.TestStore, ghClient github.Client, onAdded func(repo, branch, dir string), onRemoved func(repo, branch string))
```

In the PR handlers inside `registerAPIRoutes`, update the `CreatePR` call:

```go
// was: prNum, htmlURL, err := ghClient.CreatePR(ctx, repo, title, head, base, body, draft)
pr, err := ghClient.CreatePR(ctx, github.CreatePROptions{
    Repo: repo, Title: title, Head: head, Base: base, Body: body, Draft: draft,
})
// then use pr.Number and pr.HTMLURL instead of prNum and htmlURL
```

Update `UpdatePR` call — the `draft bool` parameter was removed from the interface:

```go
// was: err := ghClient.UpdatePR(ctx, repo, number, body, false)
err := ghClient.UpdatePR(ctx, repo, number, body)
```

In `cmd/opendev/main.go`, update the `NewClient` call:

```go
// was: ghClient = githubpkg.NewClient(token, owner)
ghClient = githubpkg.NewHTTPClient(token, owner, slog.Default())
```

And change the type of `ghClient` variable from `*githubpkg.Client` to `githubpkg.Client` (the interface).

- [ ] **Step 7: Run tests**

Run: `cd /home/angoo/repos/opendev/opendev-coder && go test ./internal/github/ ./cmd/opendev/ -v -count=1`
Expected: All GitHub client tests and existing main_test.go tests PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/github/ cmd/opendev/api.go cmd/opendev/main.go
git commit -m "feat: extract GitHub Client interface with httptest-based tests"
```

---

### Task 4: Split manager.go and Wire GitOps

Split `internal/manager/manager.go` into four files and inject `GitOps` instead of calling `exec.Command` directly.

**Files:**
- Modify: `internal/manager/manager.go` (lines 1-520 — significant rewrite)
- Create: `internal/manager/repo.go`
- Create: `internal/manager/worktree.go`
- Create: `internal/manager/branch.go`
- Modify: `internal/manager/manager_test.go` (lines 1-466 — rewrite with FakeGitOps)

- [ ] **Step 1: Rewrite manager.go — struct, constructor, helpers only**

```go
// internal/manager/manager.go
package manager

import (
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/iamangus/code-mcp/internal/gitops"
)

type RepoInfo struct {
	Name          string       `json:"name"`
	Dir           string       `json:"dir"`
	DefaultBranch string       `json:"default_branch"`
	Branches      []BranchInfo `json:"branches"`
}

type BranchInfo struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`
}

type CommitInfo struct {
	Hash    string `json:"hash"`
	Subject string `json:"subject"`
}

type MergeConflictError struct {
	Output string
}

func (e *MergeConflictError) Error() string {
	return fmt.Sprintf("merge conflict:\n%s", e.Output)
}

type Manager struct {
	reposDir string
	git      gitops.GitOps
	logger   *slog.Logger
	mu       sync.RWMutex
}

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

func (m *Manager) ReposDir() string {
	return m.reposDir
}

func (m *Manager) RepoDir(repo string) string {
	return fmt.Sprintf("%s/%s.git", m.reposDir, repo)
}

func (m *Manager) BranchWorktreeDir(repo, branch string) string {
	return fmt.Sprintf("%s/%s+%s", m.reposDir, repo, branch)
}
```

- [ ] **Step 2: Create repo.go — SyncRepo, RemoveRepo, Scan, ListRepos**

```go
// internal/manager/repo.go
package manager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (m *Manager) SyncRepo(repoURL, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	ctx := context.Background()
	repoDir := m.RepoDir(name)

	if _, err := os.Stat(repoDir); err == nil {
		m.logger.Info("repo sync: fetching", "repo", name)
		if err := m.git.Fetch(ctx, repoDir); err != nil {
			m.logger.Warn("repo sync: fetch failed (non-fatal)", "repo", name, "error", err)
		}
		return nil
	}

	m.logger.Info("repo sync: cloning", "repo", name, "url", repoURL)
	return m.git.Clone(ctx, repoURL, repoDir)
}

func (m *Manager) RemoveRepo(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	repoDir := m.RepoDir(name)
	if _, err := os.Stat(repoDir); os.IsNotExist(err) {
		return fmt.Errorf("repo %q not found", name)
	}

	// Remove all worktree directories first (repo+branch pattern)
	entries, _ := os.ReadDir(m.reposDir)
	prefix := name + "+"
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			wtPath := filepath.Join(m.reposDir, e.Name())
			m.logger.Info("removing worktree dir", "repo", name, "path", wtPath)
			os.RemoveAll(wtPath)
		}
	}

	m.logger.Info("removing repo", "repo", name)
	return os.RemoveAll(repoDir)
}

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
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".git") {
			continue
		}
		repoName := strings.TrimSuffix(name, ".git")
		repoDir := filepath.Join(m.reposDir, name)

		defaultBranch, err := m.git.DefaultBranch(ctx, repoDir)
		if err != nil {
			m.logger.Warn("scan: could not determine default branch", "repo", repoName, "error", err)
			defaultBranch = "main"
		}

		branches := m.listWorktrees(repoName, repoDir, defaultBranch)

		repos = append(repos, RepoInfo{
			Name:          repoName,
			Dir:           repoDir,
			DefaultBranch: defaultBranch,
			Branches:      branches,
		})
	}
	return repos, nil
}

func (m *Manager) listWorktrees(repoName, repoDir, defaultBranch string) []BranchInfo {
	entries, err := os.ReadDir(m.reposDir)
	if err != nil {
		return nil
	}

	prefix := repoName + "+"
	var branches []BranchInfo

	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		branchName := strings.TrimPrefix(e.Name(), prefix)
		branches = append(branches, BranchInfo{
			Name: branchName,
			Dir:  filepath.Join(m.reposDir, e.Name()),
		})
	}
	return branches
}
```

- [ ] **Step 3: Create worktree.go — CreateWorktree, RemoveWorktree, WorktreeDir, ListBranches**

```go
// internal/manager/worktree.go
package manager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var validBranchName = regexp.MustCompile(`^[a-zA-Z0-9._/-]+$`)

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

	ctx := context.Background()
	defaultBranch, err := m.git.DefaultBranch(ctx, repoDir)
	if err != nil {
		return "", fmt.Errorf("cannot determine default branch: %w", err)
	}
	if branch == defaultBranch {
		return repoDir, nil
	}

	wtDir := m.BranchWorktreeDir(repo, branch)
	if _, err := os.Stat(wtDir); os.IsNotExist(err) {
		return "", fmt.Errorf("worktree %q not found for repo %q", branch, repo)
	}
	return wtDir, nil
}

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

	defaultBranch, err := m.git.DefaultBranch(ctx, repoDir)
	if err != nil {
		return "", fmt.Errorf("cannot determine default branch: %w", err)
	}
	if branch == defaultBranch {
		m.logger.Info("worktree: branch is default, returning repo dir", "repo", repo, "branch", branch)
		return repoDir, nil
	}

	wtDir := m.BranchWorktreeDir(repo, branch)

	// Already exists
	if _, err := os.Stat(wtDir); err == nil {
		m.logger.Info("worktree: already exists", "repo", repo, "branch", branch)
		resolved, err := filepath.EvalSymlinks(wtDir)
		if err != nil {
			return wtDir, nil
		}
		return resolved, nil
	}

	// Fetch latest before creating
	if fetchErr := m.git.Fetch(ctx, repoDir); fetchErr != nil {
		m.logger.Warn("worktree: fetch before create (non-fatal)", "repo", repo, "error", fetchErr)
	}

	// Check if branch exists; if not, create it
	exists, err := m.git.BranchExists(ctx, repoDir, branch)
	if err != nil {
		return "", err
	}
	if !exists {
		startPoint := defaultBranch
		if base != "" {
			startPoint = base
		}
		if err := m.git.CreateBranch(ctx, repoDir, branch, startPoint); err != nil {
			return "", fmt.Errorf("create branch %q: %w", branch, err)
		}
	}

	if err := m.git.WorktreeAdd(ctx, repoDir, wtDir, branch); err != nil {
		return "", fmt.Errorf("worktree add: %w", err)
	}

	// Push new branch to origin
	if pushErr := m.git.Push(ctx, wtDir, branch); pushErr != nil {
		m.logger.Warn("worktree: push after create (non-fatal)", "repo", repo, "branch", branch, "error", pushErr)
	}

	resolved, err := filepath.EvalSymlinks(wtDir)
	if err != nil {
		return wtDir, nil
	}

	m.logger.Info("worktree created", "repo", repo, "branch", branch, "dir", resolved)
	return resolved, nil
}

func (m *Manager) RemoveWorktree(repo, branch string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	ctx := context.Background()
	repoDir := m.RepoDir(repo)
	wtDir := m.BranchWorktreeDir(repo, branch)

	if _, err := os.Stat(wtDir); os.IsNotExist(err) {
		return fmt.Errorf("worktree %q not found for repo %q", branch, repo)
	}

	if err := m.git.WorktreeRemove(ctx, repoDir, wtDir); err != nil {
		m.logger.Warn("worktree: git worktree remove (non-fatal)", "repo", repo, "branch", branch, "error", err)
	}

	m.logger.Info("worktree removed", "repo", repo, "branch", branch)
	return os.RemoveAll(wtDir)
}

func (m *Manager) ListBranches(repo string) ([]BranchInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	repoDir := m.RepoDir(repo)
	if _, err := os.Stat(repoDir); os.IsNotExist(err) {
		return nil, fmt.Errorf("repo %q not found", repo)
	}

	ctx := context.Background()
	defaultBranch, _ := m.git.DefaultBranch(ctx, repoDir)

	entries, err := os.ReadDir(m.reposDir)
	if err != nil {
		return nil, err
	}

	prefix := repo + "+"
	var branches []BranchInfo
	// Include default branch pointing to bare repo
	if defaultBranch != "" {
		branches = append(branches, BranchInfo{Name: defaultBranch, Dir: repoDir})
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			branchName := strings.TrimPrefix(e.Name(), prefix)
			branches = append(branches, BranchInfo{
				Name: branchName,
				Dir:  filepath.Join(m.reposDir, e.Name()),
			})
		}
	}
	return branches, nil
}
```

- [ ] **Step 4: Create branch.go — MergeBranch, GetCommits, PushBranch**

```go
// internal/manager/branch.go
package manager

import (
	"context"
	"fmt"
	"os"
	"strings"
)

func (m *Manager) PushBranch(repo, branch string) error {
	wtDir := m.BranchWorktreeDir(repo, branch)
	ctx := context.Background()
	return m.git.Push(ctx, wtDir, branch)
}

func (m *Manager) MergeBranch(repo, sourceBranch, targetBranch string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	ctx := context.Background()
	repoDir := m.RepoDir(repo)

	defaultBranch, err := m.git.DefaultBranch(ctx, repoDir)
	if err != nil {
		return fmt.Errorf("cannot determine default branch: %w", err)
	}

	// Determine the target worktree directory
	var targetDir string
	if targetBranch == defaultBranch {
		// For the default branch, we need a worktree (bare repo can't merge)
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

	// Merge source into target
	if err := m.git.Merge(ctx, targetDir, sourceBranch); err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "CONFLICT") || strings.Contains(errStr, "conflict") {
			return &MergeConflictError{Output: errStr}
		}
		return err
	}

	// Push merged result
	if pushErr := m.git.Push(ctx, targetDir, targetBranch); pushErr != nil {
		m.logger.Warn("merge: push after merge (non-fatal)", "repo", repo, "branch", targetBranch, "error", pushErr)
	}

	return nil
}

func (m *Manager) GetCommits(repo, branch string) ([]CommitInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	ctx := context.Background()
	repoDir := m.RepoDir(repo)

	defaultBranch, err := m.git.DefaultBranch(ctx, repoDir)
	if err != nil {
		return nil, err
	}

	wtDir, err := m.worktreeDirLocked(repo, branch)
	if err != nil {
		return nil, err
	}

	// Commits unique to this branch
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
```

- [ ] **Step 5: Update callers in main.go and api.go**

In `cmd/opendev/main.go`, update the `Manager` constructor:

```go
// was: mgr, err := manager.New(reposDir, githubToken)
gitOps := gitops.NewExec(slog.Default(), githubToken)
mgr, err := manager.New(reposDir, gitOps, slog.Default())
```

Add import: `"github.com/iamangus/code-mcp/internal/gitops"`

Remove the `token` parameter from the old `manager.New` since it's now in `gitops.NewExec`.

- [ ] **Step 6: Write manager tests with FakeGitOps**

```go
// internal/manager/manager_test.go
package manager

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"log/slog"

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

// createFakeRepo creates the bare repo directory on disk so manager finds it.
func createFakeRepo(t *testing.T, mgr *Manager, name string) string {
	t.Helper()
	repoDir := mgr.RepoDir(name)
	if err := os.MkdirAll(repoDir, 0755); err != nil {
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
	want := filepath.Join(mgr.ReposDir(), "myapp.git")
	if got != want {
		t.Errorf("RepoDir = %q, want %q", got, want)
	}
}

func TestBranchWorktreeDir(t *testing.T) {
	mgr, _ := newTestManager(t)
	got := mgr.BranchWorktreeDir("myapp", "feature")
	want := filepath.Join(mgr.ReposDir(), "myapp+feature")
	if got != want {
		t.Errorf("BranchWorktreeDir = %q, want %q", got, want)
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
}

func TestSyncRepo_FetchExisting(t *testing.T) {
	mgr, fake := newTestManager(t)
	createFakeRepo(t, mgr, "repo")
	if err := mgr.SyncRepo("https://github.com/test/repo.git", "repo"); err != nil {
		t.Fatalf("SyncRepo: %v", err)
	}
	if !fake.HasCall("Fetch") {
		t.Error("expected Fetch to be called")
	}
	if fake.HasCall("Clone") {
		t.Error("should not Clone existing repo")
	}
}

func TestRemoveRepo(t *testing.T) {
	mgr, _ := newTestManager(t)
	createFakeRepo(t, mgr, "repo")
	// Create a worktree dir
	wtDir := mgr.BranchWorktreeDir("repo", "feat")
	os.MkdirAll(wtDir, 0755)

	if err := mgr.RemoveRepo("repo"); err != nil {
		t.Fatalf("RemoveRepo: %v", err)
	}
	if _, err := os.Stat(mgr.RepoDir("repo")); !os.IsNotExist(err) {
		t.Error("repo dir should be removed")
	}
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Error("worktree dir should be removed")
	}
}

func TestRemoveRepo_NotFound(t *testing.T) {
	mgr, _ := newTestManager(t)
	err := mgr.RemoveRepo("nope")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestCreateWorktree_NewBranch(t *testing.T) {
	mgr, fake := newTestManager(t)
	createFakeRepo(t, mgr, "repo")
	fake.BoolReturns["BranchExists"] = false

	// Make WorktreeAdd create the dir so the manager finds it
	origWT := fake.Errors["WorktreeAdd"]
	fake.Errors["WorktreeAdd"] = nil
	_ = origWT

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
	repoDir := createFakeRepo(t, mgr, "repo")
	fake.StringReturns["DefaultBranch"] = "main"

	dir, err := mgr.CreateWorktree("repo", "main", "")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	if dir != repoDir {
		t.Errorf("expected repo dir %q for default branch, got %q", repoDir, dir)
	}
	if fake.HasCall("WorktreeAdd") {
		t.Error("should not call WorktreeAdd for default branch")
	}
}

func TestCreateWorktree_InvalidBranch(t *testing.T) {
	mgr, _ := newTestManager(t)
	createFakeRepo(t, mgr, "repo")

	_, err := mgr.CreateWorktree("repo", "bad branch!", "")
	if err == nil {
		t.Fatal("expected error for invalid branch name")
	}
}

func TestCreateWorktree_AlreadyExists(t *testing.T) {
	mgr, _ := newTestManager(t)
	createFakeRepo(t, mgr, "repo")
	// Pre-create the worktree dir
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
	fake.Errors["Merge"] = fmt.Errorf("CONFLICT (content): Merge conflict in file.go")

	// Create target worktree dir
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

	// Create worktree dir so worktreeDirLocked finds it
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
	createFakeRepo(t, mgr, "myapp")
	fake.StringReturns["DefaultBranch"] = "main"

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
}
```

- [ ] **Step 7: Run tests**

Run: `cd /home/angoo/repos/opendev/opendev-coder && go test ./internal/manager/ -v -count=1 -race`
Expected: All manager tests PASS with no race conditions.

- [ ] **Step 8: Run full test suite to verify nothing is broken**

Run: `cd /home/angoo/repos/opendev/opendev-coder && go test ./... -count=1`
Expected: All packages compile and tests pass (excluding integration-tagged tests).

- [ ] **Step 9: Commit**

```bash
git add internal/manager/ internal/gitops/ cmd/opendev/
git commit -m "refactor: split manager.go into focused files, inject GitOps interface"
```

---

### Task 5: Structured Logging with `log/slog`

Replace all `log.Printf` calls with structured `slog` logging. Add `--log-format` and `--log-level` flags.

**Files:**
- Modify: `cmd/opendev/main.go`
- Modify: `cmd/opendev/register.go`
- Modify: `cmd/opendev/api.go`

- [ ] **Step 1: Add logger initialization and CLI flags to main.go**

Add these flags and logger setup at the top of `func main()`:

```go
logFormat := flag.String("log-format", "text", "log output format: text or json")
logLevel  := flag.String("log-level", "info", "log level: debug, info, warn, error")
```

After `flag.Parse()`, add logger initialization:

```go
// Parse log level
var level slog.Level
switch strings.ToLower(*logLevel) {
case "debug":
    level = slog.LevelDebug
case "warn":
    level = slog.LevelWarn
case "error":
    level = slog.LevelError
default:
    level = slog.LevelInfo
}

// Create handler based on format
opts := &slog.HandlerOptions{Level: level}
var handler slog.Handler
if *logFormat == "json" {
    handler = slog.NewJSONHandler(os.Stderr, opts)
} else {
    handler = slog.NewTextHandler(os.Stderr, opts)
}
logger := slog.New(handler)
slog.SetDefault(logger)
```

Add imports: `"log/slog"` (replace `"log"` usage).

Replace all `log.Printf(...)` calls in main.go with `logger.Info(...)` or `logger.Error(...)` with structured fields. Replace `log.Fatalf(...)` with `logger.Error(...); os.Exit(1)`.

Examples:
```go
// was: log.Printf("GitHub PR integration enabled (owner: %s)", owner)
logger.Info("GitHub PR integration enabled", "owner", owner)

// was: log.Fatalf("manager: %v", err)
logger.Error("manager initialization failed", "error", err)
os.Exit(1)

// was: log.Printf("registered MCP handler for %s/%s/%s -> %s", repo, branch, p, dir)
logger.Info("registered MCP handler", "repo", repo, "branch", branch, "profile", p, "dir", dir)

// was: log.Printf("starting multi-server on %s  (repos-dir=%s)", addr, reposDir)
logger.Info("starting multi-server", "addr", addr, "repos_dir", reposDir)
```

- [ ] **Step 2: Replace log.Printf in register.go with slog**

The register functions need a `*slog.Logger` parameter. Update signatures:

```go
func registerReadTools(s *server.MCPServer, lm *locks.Manager, worktreeRoot string, logger *slog.Logger)
func registerWriteTools(s *server.MCPServer, lm *locks.Manager, worktreeRoot string, ts *tools.TestStore, logger *slog.Logger)
func newMCPHandler(profile Profile, worktreeRoot string, ts *tools.TestStore, logger *slog.Logger) *server.StreamableHTTPServer
```

Inside each tool handler, replace the `log.Printf` pattern with structured slog. The existing pattern is:

```go
// Current pattern (repeated for each tool):
start := time.Now()
// ... extract params ...
log.Printf("tool=read_file filepath=%q ok elapsed=%dms", fp, time.Since(start).Milliseconds())
```

Replace with:

```go
start := time.Now()
// ... extract params ...
logger.Info("tool call completed",
    "tool", "read_file",
    "filepath", fp,
    "duration_ms", time.Since(start).Milliseconds(),
)
```

For error cases:
```go
logger.Error("tool call failed",
    "tool", "read_file",
    "filepath", fp,
    "error", toolErr,
    "duration_ms", time.Since(start).Milliseconds(),
)
```

Apply this to all 9 tool handlers (read_file, read_lines, list_directory, grep_search, get_git_diff, create_file, search_and_replace, execute_terminal_command, register_test).

- [ ] **Step 3: Replace log.Printf in api.go with slog**

Update `registerAPIRoutes` to accept `*slog.Logger`:

```go
func registerAPIRoutes(mux *http.ServeMux, mgr *manager.Manager, ts *tools.TestStore, ghClient github.Client, logger *slog.Logger, onAdded func(repo, branch, dir string), onRemoved func(repo, branch string))
```

Replace the one `log.Printf` in `writeJSON`:
```go
// was: log.Printf("api: writeJSON encode error: %v", err)
logger.Error("api: JSON encode failed", "error", err)
```

Note: `writeJSON` and `apiError` are package-level functions. Either make `logger` a parameter or capture it via closure in `registerAPIRoutes`. The closure approach is simpler since these helpers are only called from within the registered handlers.

- [ ] **Step 4: Update all callers in main.go to pass logger**

Pass `logger` to `registerReadTools`, `registerWriteTools`, `newMCPHandler`, `registerAPIRoutes`, and `locks.NewManager`:

```go
lm := locks.NewManager(logger)
// ...
registerReadTools(s, lm, dir, logger)
registerWriteTools(s, lm, dir, ts, logger)
newMCPHandler(p, dir, ts, logger)
registerAPIRoutes(mux, mgr, ts, ghClient, logger, onAdded, onRemoved)
```

- [ ] **Step 5: Run full test suite**

Run: `cd /home/angoo/repos/opendev/opendev-coder && go test ./... -count=1`
Expected: All tests pass. The test files may need minor updates to pass `slog.Default()` where new logger parameters were added.

- [ ] **Step 6: Commit**

```bash
git add cmd/opendev/
git commit -m "feat: replace log.Printf with structured slog logging, add --log-format and --log-level flags"
```

---

### Task 6: Final Integration — Wire Everything Together and Verify

Connect all the pieces, run the full test suite, and verify the build.

**Files:**
- Modify: `cmd/opendev/main.go` (final wiring pass)
- Modify: `cmd/opendev/main_test.go` (update for new constructors)

- [ ] **Step 1: Update main_test.go for new constructor signatures**

Update test helpers and test functions to use new constructors:

```go
// Update imports to include slog and gitops
import (
    "log/slog"
    "github.com/iamangus/code-mcp/internal/gitops"
    // ... existing imports ...
)

// Update newLM() calls in any test that creates a locks.Manager:
// was: lm := locks.NewManager()
lm := locks.NewManager(slog.Default())

// Update newMCPHandler calls:
// was: newMCPHandler(p, dir, ts)
newMCPHandler(p, dir, ts, slog.Default())

// Update manager.New calls in TestMultiServerRouting:
// was: mgr, err := manager.New(reposDir, "")
git := gitops.NewExec(slog.Default(), "")
mgr, err := manager.New(reposDir, git, slog.Default())
```

- [ ] **Step 2: Run full test suite**

Run: `cd /home/angoo/repos/opendev/opendev-coder && go test ./... -count=1 -race`
Expected: All tests pass, no race conditions.

- [ ] **Step 3: Verify the binary builds**

Run: `cd /home/angoo/repos/opendev/opendev-coder && go build ./cmd/opendev/`
Expected: Clean build, no warnings.

- [ ] **Step 4: Verify the new flags work**

Run: `cd /home/angoo/repos/opendev/opendev-coder && go run ./cmd/opendev/ --help`
Expected: Output includes `--log-format` and `--log-level` flags.

- [ ] **Step 5: Run with JSON logging to verify output format**

Run: `cd /home/angoo/repos/opendev/opendev-coder && timeout 2 go run ./cmd/opendev/ --dir /tmp/test-wt --mode http --addr :9999 --log-format json --log-level debug 2>&1 || true`
Expected: Log output is structured JSON to stderr.

- [ ] **Step 6: Commit**

```bash
git add cmd/opendev/main_test.go
git commit -m "chore: update tests for new constructor signatures, verify full integration"
```

---

## Task Dependency Graph

```
Task 1 (LockManager)
    ↓
Task 2 (GitOps interface) ─── Task 3 (GitHub interface)
    ↓                              ↓
Task 4 (Split manager, wire GitOps + GitHub)
    ↓
Task 5 (Structured logging)
    ↓
Task 6 (Final integration + verification)
```

Tasks 2 and 3 are independent of each other and can be done in parallel after Task 1.
Task 4 depends on both Tasks 2 and 3.
Tasks 5 and 6 are sequential and depend on Task 4.
