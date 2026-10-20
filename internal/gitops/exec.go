package gitops

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/iamangus/code-mcp/internal/limits"
)

type Exec struct {
	logger *slog.Logger
	token  string
}

func NewExec(logger *slog.Logger, token string) *Exec {
	return &Exec{logger: logger, token: token}
}

func (e *Exec) Clone(ctx context.Context, url, dir string) error {
	_, err := e.run(ctx, "", "git", "clone", "--", cleanURL(url), dir)
	return err
}

func (e *Exec) Fetch(ctx context.Context, dir string) error {
	if err := e.cleanOrigin(ctx, dir); err != nil {
		return err
	}
	_, err := e.run(ctx, dir, "git", "fetch", "--prune", "origin")
	return err
}

// FastForward advances the checked-out branch only when origin has a direct
// descendant. It never rewrites local history or touches other worktrees.
func (e *Exec) FastForward(ctx context.Context, dir, branch string) error {
	_, err := e.run(ctx, dir, "git", "merge", "--ff-only", "origin/"+branch)
	return err
}

func (e *Exec) Checkout(ctx context.Context, dir, branch string) error {
	_, err := e.run(ctx, dir, "git", "checkout", branch)
	return err
}

func (e *Exec) WorktreeAdd(ctx context.Context, repoDir, wtDir, branch string) error {
	_, err := e.run(ctx, repoDir, "git", "worktree", "add", wtDir, branch)
	return err
}

func (e *Exec) WorktreeRemove(ctx context.Context, repoDir, wtDir string) error {
	_, err := e.run(ctx, repoDir, "git", "worktree", "remove", wtDir)
	return err
}

func (e *Exec) WorktreeList(ctx context.Context, repoDir string) ([]Worktree, error) {
	out, err := e.run(ctx, repoDir, "git", "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}

	var worktrees []Worktree
	var current *Worktree
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			worktrees = append(worktrees, Worktree{Path: strings.TrimPrefix(line, "worktree ")})
			current = &worktrees[len(worktrees)-1]
		case current != nil && strings.HasPrefix(line, "branch refs/heads/"):
			current.Branch = strings.TrimPrefix(line, "branch refs/heads/")
		case current != nil && line == "detached":
			current.Detached = true
		}
	}
	return worktrees, nil
}

func (e *Exec) EnsureLocalExcludes(ctx context.Context, dir string, patterns []string) error {
	excludePath, err := e.run(ctx, dir, "git", "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return err
	}
	excludePath = strings.TrimSpace(excludePath)
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(dir, excludePath)
	}

	contents, err := os.ReadFile(excludePath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, pattern := range patterns {
		if !containsLine(string(contents), pattern) {
			contents = append(contents, []byte(pattern+"\n")...)
		}
	}
	return os.WriteFile(excludePath, contents, 0600)
}

func containsLine(contents, line string) bool {
	for _, existing := range strings.Split(contents, "\n") {
		if existing == line {
			return true
		}
	}
	return false
}

func (e *Exec) Merge(ctx context.Context, dir, branch string) error {
	if _, err := e.runProbe(ctx, dir, "git", "rev-parse", "--verify", "MERGE_HEAD"); err == nil {
		return fmt.Errorf("target has a pre-existing merge in progress; manual inspection is required")
	}
	status, err := e.Status(ctx, dir)
	if err != nil {
		return err
	}
	if status != "" {
		return fmt.Errorf("integration worktree must be clean before merging")
	}
	_, err = e.run(ctx, dir, "git", "merge", "--no-edit", "--", branch)
	if err != nil && strings.Contains(err.Error(), "CONFLICT") {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, abortErr := e.run(cleanup, dir, "git", "merge", "--abort"); abortErr != nil {
			return fmt.Errorf("%w; abort failed: %v", err, abortErr)
		}
	}
	return err
}

func (e *Exec) Push(ctx context.Context, dir, branch string) error {
	if err := e.cleanOrigin(ctx, dir); err != nil {
		return err
	}
	_, err := e.run(ctx, dir, "git", "push", "origin", branch)
	return err
}

func (e *Exec) Commit(ctx context.Context, dir, message string) error {
	if _, err := e.run(ctx, dir, "git", "add", "--all"); err != nil {
		return err
	}
	_, err := e.run(ctx, dir, "git", "commit", "-m", message)
	return err
}

func (e *Exec) HeadCommit(ctx context.Context, dir string) (string, error) {
	return e.run(ctx, dir, "git", "rev-parse", "HEAD")
}

func (e *Exec) OriginURL(ctx context.Context, dir string) (string, error) {
	value, err := e.run(ctx, dir, "git", "remote", "get-url", "origin")
	return cleanURL(value), err
}

func (e *Exec) CherryPick(ctx context.Context, dir, commit string) error {
	_, err := e.run(ctx, dir, "git", "cherry-pick", commit)
	return err
}

func (e *Exec) Diff(ctx context.Context, dir string) (string, error) {
	return e.run(ctx, dir, "git", "diff", "HEAD")
}

func (e *Exec) DiffRange(ctx context.Context, dir, base, target string) (string, error) {
	return e.run(ctx, dir, "git", "diff", base+".."+target)
}

func (e *Exec) ReadFileAt(ctx context.Context, dir, sha, path string) (string, error) {
	if !filepath.IsLocal(path) || strings.ContainsAny(sha, ": \t\r\n") || strings.HasPrefix(sha, "-") {
		return "", fmt.Errorf("invalid committed evidence path or revision")
	}
	return e.runProbe(ctx, dir, "git", "show", "--no-ext-diff", "--no-textconv", sha+":"+filepath.ToSlash(path))
}

func (e *Exec) CommitLog(ctx context.Context, dir string, args ...string) (string, error) {
	fullArgs := append([]string{"log"}, args...)
	return e.run(ctx, dir, "git", fullArgs...)
}

func (e *Exec) DefaultBranch(ctx context.Context, dir string) (string, error) {
	out, err := e.run(ctx, dir, "git", "symbolic-ref", "--short", "HEAD")
	if err != nil {
		out, err = e.run(ctx, dir, "git", "rev-parse", "--abbrev-ref", "origin/HEAD")
		if err != nil {
			return "", err
		}
		out = strings.TrimPrefix(out, "origin/")
	}
	return strings.TrimSpace(out), nil
}

func (e *Exec) BranchExists(ctx context.Context, dir, branch string) (bool, error) {
	_, err := e.runProbe(ctx, dir, "git", "rev-parse", "--verify", branch)
	if err != nil {
		return false, nil
	}
	return true, nil
}

func (e *Exec) RemoteBranchExists(ctx context.Context, dir, branch string) (bool, error) {
	_, err := e.runProbe(ctx, dir, "git", "rev-parse", "--verify", "origin/"+branch)
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

func (e *Exec) ResolveRevision(ctx context.Context, dir, revision string) (string, error) {
	return e.run(ctx, dir, "git", "rev-parse", "--verify", "--end-of-options", revision+"^{commit}")
}

func (e *Exec) WorktreeAddDetached(ctx context.Context, repoDir, wtDir, revision string) error {
	_, err := e.run(ctx, repoDir, "git", "worktree", "add", "--detach", wtDir, revision)
	return err
}

func cleanURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err == nil && u.User != nil && (u.Scheme == "https" || u.Scheme == "http") {
		u.User = nil
		return u.String()
	}
	return rawURL
}

func (e *Exec) cleanOrigin(ctx context.Context, dir string) error {
	value, err := e.run(ctx, dir, "git", "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	if cleaned := cleanURL(value); cleaned != value {
		_, err = e.run(ctx, dir, "git", "remote", "set-url", "origin", cleaned)
	}
	return err
}

func (e *Exec) command(ctx context.Context, dir, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	// Credentials never enter argv, remotes, or logs. Limit the authorization
	// header to GitHub HTTPS and disable repository-controlled Git hooks.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=/dev/null", "GIT_CONFIG_KEY_1=http.https://github.com/.extraHeader", "GIT_CONFIG_VALUE_1=Authorization: Basic "+base64.StdEncoding.EncodeToString([]byte("x-access-token:"+e.token)))
	if e.token == "" {
		cmd.Env = append(cmd.Env, "GIT_CONFIG_VALUE_1=")
	}
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

func (e *Exec) run(ctx context.Context, dir, name string, args ...string) (string, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := e.command(ctx, dir, name, args...)

	output := &limits.Buffer{Limit: 128 << 10}
	cmd.Stdout, cmd.Stderr = output, output
	err := cmd.Run()
	elapsed := time.Since(start)
	result := strings.TrimSpace(output.String())
	if e.token != "" {
		result = strings.ReplaceAll(result, e.token, "[redacted]")
		result = strings.ReplaceAll(result, base64.StdEncoding.EncodeToString([]byte("x-access-token:"+e.token)), "[redacted]")
	}

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

// runProbe is for expected existence checks where a non-zero status is data,
// not an operational failure worth emitting at error level.
func (e *Exec) runProbe(ctx context.Context, dir, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := e.command(ctx, dir, name, args...)
	output := &limits.Buffer{Limit: 32 << 10}
	cmd.Stdout, cmd.Stderr = output, output
	err := cmd.Run()
	result := strings.TrimSpace(output.String())
	if err != nil {
		e.logger.Debug("git probe did not match", "cmd", name, "args", args, "dir", dir)
		return result, err
	}
	return result, nil
}

// Compile-time check that Exec implements GitOps.
var _ GitOps = (*Exec)(nil)
