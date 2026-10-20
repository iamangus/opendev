package worktree

import (
	"context"
	"log/slog"
	"path/filepath"

	"github.com/iamangus/code-mcp/internal/locks"
)

var workspaceLocks = locks.NewManager(slog.Default())

// Coordinate prevents a commit/merge from observing half a group of concurrent
// file mutations. File-level locks still allow independent files to be edited.
func Coordinate(ctx context.Context, root string, exclusive bool) (func(), error) {
	key, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(key); err == nil {
		key = resolved
	}
	if exclusive {
		if err := workspaceLocks.Lock(ctx, key); err != nil {
			return nil, err
		}
		return func() { workspaceLocks.Unlock(key) }, nil
	}
	if err := workspaceLocks.RLock(ctx, key); err != nil {
		return nil, err
	}
	return func() { workspaceLocks.RUnlock(key) }, nil
}
