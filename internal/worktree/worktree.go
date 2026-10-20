package worktree

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ToolError is a user-facing error that should be returned as an MCP tool error.
type ToolError struct {
	Message string
}

func (e *ToolError) Error() string { return e.Message }

// Resolve resolves a relative path against worktreeRoot, ensuring it stays within the root.
func Resolve(worktreeRoot, relativePath string) (string, error) {
	return resolve(worktreeRoot, relativePath, false)
}

// ResolveManaged is for server-owned runtime directories, never coding tools.
func ResolveManaged(worktreeRoot, relativePath string) (string, error) {
	return resolve(worktreeRoot, relativePath, true)
}

func resolve(worktreeRoot, relativePath string, runtime bool) (string, error) {
	if strings.ContainsRune(relativePath, 0) {
		return "", &ToolError{Message: "Tool Error: path contains null bytes"}
	}
	root, err := filepath.Abs(worktreeRoot)
	if err != nil {
		return "", fmt.Errorf("cannot resolve worktree root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "", &ToolError{Message: fmt.Sprintf("Tool Error: worktree root %q does not exist or is not a directory", worktreeRoot)}
	}
	var abs string
	if filepath.IsAbs(relativePath) {
		abs = filepath.Clean(relativePath)
	} else {
		abs = filepath.Join(root, relativePath)
	}
	abs = filepath.Clean(abs)
	rel, err := filepath.Rel(root, abs)
	if err != nil || !filepath.IsLocal(rel) {
		return "", &ToolError{Message: fmt.Sprintf("Tool Error: path %q escapes worktree root. All paths must be relative to the worktree root.", relativePath)}
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for i, part := range parts {
		if part == ".git" || (!runtime && part == ".opendev" && i+1 < len(parts) && (parts[i+1] == "state" || parts[i+1] == "worktrees" || parts[i+1] == "references")) {
			return "", &ToolError{Message: "Tool Error: Git metadata and OpenDev runtime paths are protected"}
		}
	}
	// Reject symlink components, including dangling links. Root-based file IO
	// below provides the race-safe boundary; this also protects subprocess paths.
	current := root
	for _, part := range parts {
		current = filepath.Join(current, part)
		fi, statErr := os.Lstat(current)
		if os.IsNotExist(statErr) {
			break
		}
		if statErr != nil {
			return "", statErr
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return "", &ToolError{Message: "Tool Error: symlink paths are not supported"}
		}
	}
	return abs, nil
}

// OpenRoot returns a race-safe filesystem boundary and a validated relative path.
// Callers must close the root and perform IO through it rather than the absolute path.
func OpenRoot(root, path string) (*os.Root, string, error) {
	abs, err := Resolve(root, path)
	if err != nil {
		return nil, "", err
	}
	base, err := filepath.Abs(root)
	if err != nil {
		return nil, "", err
	}
	rel, err := filepath.Rel(base, abs)
	if err != nil {
		return nil, "", err
	}
	r, err := os.OpenRoot(base)
	return r, rel, err
}
