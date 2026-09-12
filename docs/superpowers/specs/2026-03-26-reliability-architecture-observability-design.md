# Reliability, Architecture & Observability Improvements

**Date:** 2026-03-26
**Status:** Draft
**Scope:** Structured logging, interface extraction, manager split, test improvements, concurrency improvements

## Context

opendev is a Go MCP server that gives AI agents coding tools scoped to git worktrees. The project has grown organically — adding multi-repo support, GitHub PR management, and test infrastructure. This design addresses three areas that need attention:

1. **Observability is minimal** — scattered `log.Printf` calls with no structure, no request tracing, no way to correlate logs across repos/branches
2. **Architecture is tightly coupled** — `manager.go` handles four jobs, git operations are done via direct `exec.Command` with no abstraction, components are hard to test in isolation
3. **Test coverage has gaps** — 9 broken manager tests (regression from `CreateWorktree` signature change), no GitHub client tests, no concurrency tests

## Constraints

- Zero new dependencies — everything uses Go stdlib (`log/slog`, `net/http/httptest`, `sync`, `context`)
- Go 1.24 (already in use)
- No behavior changes to existing MCP tools or API endpoints
- No changes to Docker/deployment

---

## 1. Structured Logging with `log/slog`

### Setup

A single `slog.Logger` initialized at server startup. Output format and level controlled by flags:

- `--log-format json|text` (default: `text` for development, `json` for production)
- `--log-level debug|info|warn|error` (default: `info`)

The logger is injected into all components via constructor parameters — no global logger.

### Request-Scoped Context

Every MCP tool call and API request gets a `context.Context` enriched with structured attributes:

| Field | Source | Example |
|-------|--------|---------|
| `request_id` | Generated per-request (UUID or short random) | `abc123` |
| `repo` | From URL path (multi-server) or startup config (single) | `myapp` |
| `branch` | From URL path or worktree config | `feature-x` |
| `tool` | MCP tool name | `grep_search` |

A helper creates the enriched context:

```go
func WithRequestContext(ctx context.Context, repo, branch, tool string) context.Context
```

Any `slog` call using this context automatically includes these fields via `slog.Handler` integration.

### What Gets Logged

**INFO level:**
- Tool call start and completion with duration
- Git operations (clone, fetch, worktree create/delete, merge) with duration
- GitHub API calls with HTTP status and duration
- Server startup with configuration summary
- Repository sync events

**WARN level:**
- Lock contention exceeding threshold (see Section 5)
- Fuzzy match fallback in search_and_replace (with similarity score)
- Slow tool calls exceeding a threshold (e.g., >5s)

**ERROR level:**
- Git command failures with stderr
- GitHub API errors with response body
- File operation failures
- Lock acquisition timeouts

**DEBUG level:**
- Full command arguments for git operations
- Lock acquisition/release events
- File read/write paths and sizes
- Request/response details for GitHub API

### What Does NOT Get Logged

- File contents (security — could contain secrets)
- Full command stdout (too noisy — only on error or at debug level)
- GitHub tokens or auth headers
- Request bodies containing file content

### Example Output

```json
{"time":"2026-03-26T10:15:30Z","level":"INFO","msg":"tool call completed","request_id":"abc123","repo":"myapp","branch":"feature-x","tool":"grep_search","duration_ms":42,"matches":7}
```

```json
{"time":"2026-03-26T10:15:31Z","level":"WARN","msg":"lock contention","request_id":"def456","repo":"myapp","branch":"feature-x","path":"src/main.go","wait_ms":250}
```

---

## 2. Interface Extraction

Three interfaces to decouple components and enable testing with fakes.

### 2.1 `GitOps` Interface

Lives in `internal/gitops/gitops.go`. Abstracts all git shell commands.

```go
package gitops

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

**Real implementation:** `internal/gitops/exec.go` — wraps `exec.Command` with:
- Structured logging (command, duration, exit code)
- Token injection for push/fetch (from constructor param)
- Context cancellation support (via `CommandContext`)
- Consistent error wrapping with stderr content

**Fake implementation:** `internal/gitops/fake.go` — records calls, returns configured responses. Used by manager tests.

### 2.2 `GitHubClient` Interface

Lives in `internal/github/github.go`. Abstracts the GitHub REST API.

```go
package github

type Client interface {
    CreatePR(ctx context.Context, opts CreatePROptions) (*PR, error)
    UpdatePR(ctx context.Context, owner, repo string, number int, body string) error
    PromotePR(ctx context.Context, owner, repo string, number int) error
}

type CreatePROptions struct {
    Owner, Repo       string
    Title, Body       string
    Head, Base        string
    Draft             bool
}

type PR struct {
    Number  int
    HTMLURL string
}
```

**Real implementation:** the existing `client.go` logic, refactored to implement this interface. Constructor takes `*slog.Logger` and logs all API calls.

**Fake implementation:** `internal/github/fake.go` — records calls for test assertions.

### 2.3 `FileSystem` Interface

Lives in `internal/tools/fs.go`. Abstracts file operations used by the tools layer.

```go
type FileSystem interface {
    ReadFile(ctx context.Context, path string) ([]byte, error)
    WriteFile(ctx context.Context, path string, data []byte, perm os.FileMode) error
    Stat(ctx context.Context, path string) (os.FileInfo, error)
    ReadDir(ctx context.Context, path string) ([]os.DirEntry, error)
    MkdirAll(ctx context.Context, path string, perm os.FileMode) error
}
```

**Real implementation:** `internal/tools/osfs.go` — thin wrapper around `os` package. Provides a natural place to inject per-file locking and log file access.

**Fake implementation:** in-memory filesystem for tests.

Lower priority than GitOps and GitHubClient — existing tool tests work well against real disk. Migrate as time permits.

---

## 3. Splitting `manager.go`

Currently `manager.go` handles repo sync, worktree lifecycle, branch operations, and coordination. Split into four files within `internal/manager/`:

| File | Responsibility | Key Functions |
|------|---------------|---------------|
| `manager.go` | Struct definition, constructor, shared state, `GitOps` injection | `New()`, fields: repos map, mutex, logger, gitops |
| `repo.go` | Getting bare repos on disk and keeping them current | `SyncRepo`, `RemoveRepo`, `ScanRepos`, `ListRepos` |
| `worktree.go` | Worktree lifecycle within a repo | `CreateWorktree`, `RemoveWorktree`, `ListBranches` |
| `branch.go` | Branch-level operations within a worktree | `MergeBranch`, `GetCommits`, `Push` |

All four files share the `Manager` receiver. The `Manager` constructor takes a `GitOps` interface:

```go
func New(reposDir string, gitops gitops.GitOps, logger *slog.Logger, opts ...Option) *Manager
```

No behavior changes — purely organizational.

---

## 4. Test Improvements

### 4.1 Fix Broken Manager Tests

The 9 broken tests in `manager_test.go` fail because `CreateWorktree` now returns `(string, error)` instead of `error`. These are fixed as part of the rewrite against the `GitOps` interface.

### 4.2 Manager Tests with Fake GitOps

Replace the current tests that shell out to real git with tests that inject `FakeGitOps`. Coverage targets:

- Repo sync idempotency (sync existing repo is a no-op fetch)
- Worktree create for new branch
- Worktree create for existing branch
- Worktree create no-op for default branch
- Worktree remove
- Merge branch success
- Merge conflict detection (fake returns error)
- GetCommits returns parsed output
- Push delegates to GitOps with correct args

These tests are fast (no disk, no git) and reliable.

### 4.3 GitHub Client Tests

New file: `internal/github/client_test.go`. Uses `httptest.Server` to fake the GitHub API.

Coverage:
- Successful PR creation (verify request body, auth header, response parsing)
- PR creation with draft flag
- Update PR body (verify PATCH request)
- Promote PR to ready (verify PUT request)
- Error responses (404, 422) produce meaningful errors
- Missing token returns error at construction time

### 4.4 GitOps Integration Test

One integration test in `internal/gitops/exec_test.go` that uses real git:

- Init a temp repo, add a commit, create a branch, verify worktree operations work end-to-end
- Tagged with `//go:build integration` so it doesn't run in normal `go test ./...`
- Verifies the real implementation actually works (the fakes only verify the consumer logic)

### 4.5 Concurrency Tests

- Lock manager tests under parallel goroutine access
- Run with `-race` flag to detect data races
- Test context cancellation during lock wait
- Test reference counting cleanup (locks are removed when no longer held)

---

## 5. Concurrency Improvements

### Current State

`internal/locks/locks.go` uses `sync.Map` of `*sync.RWMutex` per file path. Problems:
- No visibility into contention
- No context cancellation (blocks forever)
- Map grows unboundedly

### New `LockManager`

```go
type LockManager struct {
    mu        sync.Mutex
    locks     map[string]*lockEntry
    logger    *slog.Logger
    warnAfter time.Duration  // log warning if acquisition takes longer (default 100ms)
}

type lockEntry struct {
    rwmu    sync.RWMutex
    refs    int  // reference count of goroutines using this entry
}
```

**API:**

```go
func NewLockManager(logger *slog.Logger, opts ...LockOption) *LockManager
func (lm *LockManager) RLock(ctx context.Context, path string) error
func (lm *LockManager) RUnlock(path string)
func (lm *LockManager) Lock(ctx context.Context, path string) error
func (lm *LockManager) Unlock(path string)
```

**Improvements:**

| Feature | Behavior |
|---------|----------|
| Contention logging | When lock acquisition takes longer than `warnAfter`, log a WARN with path, wait duration, and request context |
| Reference counting | Track holders per lock entry. When refs drops to 0, remove from map. Prevents unbounded growth. |
| Context cancellation | Lock/RLock accept `context.Context`. If context is cancelled while waiting, return `context.Err()` instead of blocking forever. |
| Read vs write visibility | Log whether each acquisition is read or write at DEBUG level, so contention patterns are visible. |

**Implementation note on context-aware locking:** Go's `sync.RWMutex` doesn't natively support context cancellation. Implementation uses a goroutine-based approach: attempt lock in a goroutine, select on lock completion vs context cancellation. If context wins, the goroutine still completes the lock then immediately unlocks. This is the standard Go pattern for context-aware mutex acquisition.

**Drop-in replacement:** The tools layer calls `LockManager.RLock(ctx, path)` / `LockManager.Lock(ctx, path)` in the same places it currently calls the existing lock functions. The `ctx` parameter is the only API change.

---

## File Layout Summary

```
internal/
├── gitops/
│   ├── gitops.go        # GitOps interface definition
│   ├── exec.go          # Real implementation (exec.Command)
│   ├── exec_test.go     # Integration test (//go:build integration)
│   └── fake.go          # Fake for tests
├── github/
│   ├── github.go        # Client interface + types
│   ├── client.go        # Real implementation (HTTP)
│   ├── client_test.go   # Tests with httptest.Server
│   └── fake.go          # Fake for tests
├── manager/
│   ├── manager.go       # Struct, constructor, shared state
│   ├── repo.go          # SyncRepo, RemoveRepo, ScanRepos, ListRepos
│   ├── worktree.go      # CreateWorktree, RemoveWorktree, ListBranches
│   ├── branch.go        # MergeBranch, GetCommits, Push
│   └── manager_test.go  # Tests with FakeGitOps
├── locks/
│   ├── locks.go         # LockManager (replaces current impl)
│   └── locks_test.go    # Concurrency tests
├── tools/
│   ├── fs.go            # FileSystem interface
│   ├── osfs.go          # Real implementation
│   ├── filesystem.go    # (existing, updated to use FileSystem)
│   ├── cli.go           # (existing, updated to accept logger)
│   ├── testing.go       # (existing, updated to accept logger)
│   ├── filesystem_test.go
│   └── cli_test.go
├── worktree/
│   └── (unchanged)
└── config/
    └── (unchanged)

cmd/opendev/
├── main.go              # Updated: logger init, flag parsing, DI wiring
├── register.go          # Updated: pass logger to tools
├── api.go               # Updated: request context enrichment
└── main_test.go         # (existing, updated as needed)
```

## Out of Scope

- New MCP tools or API endpoints
- Docker/deployment changes
- Configuration file format changes
- Frontend/UI work
- Performance benchmarking (can follow later)
