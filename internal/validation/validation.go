// Package validation runs repository-declared checks in isolated worktrees.
package validation

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/iamangus/code-mcp/internal/limits"
	"github.com/iamangus/code-mcp/internal/worktree"

	"gopkg.in/yaml.v3"
)

const policyPath = ".opendev/validations.yml"

type Policy struct {
	Validations map[string]Definition `yaml:"validations"`
}

type Definition struct {
	Directory string `yaml:"directory"`
	Command   string `yaml:"command"`
	Timeout   int    `yaml:"timeout_seconds"`
}

type Result struct {
	Name       string `json:"name"`
	Command    string `json:"command"`
	Directory  string `json:"directory"`
	ExitCode   int    `json:"exit_code"`
	Output     string `json:"output"`
	DurationMS int64  `json:"duration_ms"`
}

func Run(ctx context.Context, worktreePath, name string) (Result, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Result{}, fmt.Errorf("validation_name is required")
	}
	r, relPolicy, err := worktree.OpenRoot(worktreePath, policyPath)
	if err != nil {
		return Result{}, err
	}
	defer r.Close()
	data, err := r.ReadFile(relPolicy)
	if err != nil {
		return Result{}, fmt.Errorf("read %s: %w", policyPath, err)
	}
	var policy Policy
	if err := yaml.Unmarshal(data, &policy); err != nil {
		return Result{}, fmt.Errorf("parse %s: %w", policyPath, err)
	}
	definition, ok := policy.Validations[name]
	if !ok {
		return Result{}, fmt.Errorf("validation %q is not declared in %s", name, policyPath)
	}
	if strings.TrimSpace(definition.Command) == "" {
		return Result{}, fmt.Errorf("validation %q has no command", name)
	}
	if definition.Timeout <= 0 || definition.Timeout > 1800 {
		definition.Timeout = 300
	}
	dir := filepath.Clean(filepath.Join(worktreePath, definition.Directory))
	if _, err := worktree.Resolve(worktreePath, dir); err != nil {
		return Result{}, err
	}
	rel, err := filepath.Rel(worktreePath, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return Result{}, fmt.Errorf("validation %q has an invalid directory", name)
	}

	started := time.Now()
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(definition.Timeout)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "sh", "-c", definition.Command)
	cmd.Dir = dir
	cmd.WaitDelay = time.Second
	output := &limits.Buffer{Limit: 64 << 10}
	cmd.Stdout, cmd.Stderr = output, output
	runErr := cmd.Run()
	result := Result{Name: name, Command: definition.Command, Directory: definition.Directory, Output: output.String(), DurationMS: time.Since(started).Milliseconds()}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	if runCtx.Err() == context.DeadlineExceeded {
		return result, fmt.Errorf("validation %q timed out", name)
	}
	return result, runErr
}
