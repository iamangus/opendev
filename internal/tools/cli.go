package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/iamangus/code-mcp/internal/worktree"
)

const DefaultTimeout = 120 * time.Second

// ExecuteTerminalCommand runs a shell command in the given worktree directory.
// It returns stdout, stderr, exit code, a timeout flag, and any unexpected error.
func ExecuteTerminalCommand(worktreeRoot, command string, timeout time.Duration) (stdout, stderr string, exitCode int, timedOut bool, err error) {
	info, statErr := os.Stat(worktreeRoot)
	if statErr != nil || !info.IsDir() {
		err = &worktree.ToolError{Message: fmt.Sprintf("Tool Error: worktree root %q does not exist or is not a directory", worktreeRoot)}
		return
	}

	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = worktreeRoot

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	runErr := cmd.Run()
	stdout = outBuf.String()
	stderr = errBuf.String()

	if ctx.Err() == context.DeadlineExceeded {
		timedOut = true
		exitCode = -1
		return
	}

	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			err = runErr
		}
	}
	return
}
