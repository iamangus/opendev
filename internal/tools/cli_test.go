package tools

import (
	"strings"
	"testing"
	"time"

	"github.com/iamangus/code-mcp/internal/worktree"
)

// TestExecuteTerminalCommand_EchoStdout verifies stdout capture.
func TestExecuteTerminalCommand_EchoStdout(t *testing.T) {
	dir := t.TempDir()
	stdout, stderr, code, timedOut, err := ExecuteTerminalCommand(dir, "echo hello", 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if timedOut {
		t.Fatal("should not have timed out")
	}
	if code != 0 {
		t.Errorf("expected exit code 0, got %d", code)
	}
	if !strings.Contains(stdout, "hello") {
		t.Errorf("expected 'hello' in stdout, got %q", stdout)
	}
	_ = stderr
}

// TestExecuteTerminalCommand_NonZeroExit verifies non-zero exit code capture.
func TestExecuteTerminalCommand_NonZeroExit(t *testing.T) {
	dir := t.TempDir()
	_, _, code, timedOut, err := ExecuteTerminalCommand(dir, "exit 42", 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if timedOut {
		t.Fatal("should not have timed out")
	}
	if code != 42 {
		t.Errorf("expected exit code 42, got %d", code)
	}
}

// TestExecuteTerminalCommand_StderrCapture verifies stderr capture.
func TestExecuteTerminalCommand_StderrCapture(t *testing.T) {
	dir := t.TempDir()
	_, stderr, _, _, err := ExecuteTerminalCommand(dir, "echo error_msg >&2", 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stderr, "error_msg") {
		t.Errorf("expected 'error_msg' in stderr, got %q", stderr)
	}
}

// TestExecuteTerminalCommand_Timeout verifies that the timeout flag is set.
func TestExecuteTerminalCommand_Timeout(t *testing.T) {
	dir := t.TempDir()
	_, _, _, timedOut, _ := ExecuteTerminalCommand(dir, "sleep 10", 1*time.Millisecond)
	if !timedOut {
		t.Error("expected timeout flag to be true")
	}
}

// TestExecuteTerminalCommand_InvalidDir verifies ToolError for bad worktree.
func TestExecuteTerminalCommand_InvalidDir(t *testing.T) {
	_, _, _, _, err := ExecuteTerminalCommand("/nonexistent/dir/xyz", "echo hi", 5*time.Second)
	if err == nil {
		t.Fatal("expected error for invalid dir, got nil")
	}
	if _, ok := err.(*worktree.ToolError); !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
}
