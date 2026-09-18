package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/iamangus/code-mcp/internal/locks"
	"github.com/iamangus/code-mcp/internal/tools"
	"github.com/iamangus/code-mcp/internal/worktree"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Profile identifies a named set of MCP tools.
type Profile string

const (
	ProfileRead  Profile = "read"
	ProfileWrite Profile = "write"
)

// Profiles is the ordered list of all known profiles. A handler is created for
// each profile on every repo/branch.
var Profiles = []Profile{ProfileRead, ProfileWrite}

type toolFailure struct {
	Time  time.Time
	Tool  string
	Error string
}

type workspaceState struct {
	locks     *locks.Manager
	revisions *tools.RevisionAuthorizer

	mu       sync.Mutex
	failures []toolFailure
}

// recordFailure keeps a bounded log of workspace tool failures so Writer
// blocker claims can be verified against what actually happened.
func (s *workspaceState) recordFailure(tool, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = append(s.failures, toolFailure{Time: time.Now().UTC(), Tool: tool, Error: message})
	if len(s.failures) > 200 {
		s.failures = s.failures[len(s.failures)-200:]
	}
}

// FailuresSince returns tool failures recorded at or after the given time.
func (s *workspaceState) FailuresSince(since time.Time) []toolFailure {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []toolFailure
	for _, f := range s.failures {
		if !f.Time.Before(since) {
			out = append(out, f)
		}
	}
	return out
}

var workspaceStates sync.Map

func workspaceStateFor(worktreeRoot string, logger *slog.Logger) *workspaceState {
	root, err := filepath.Abs(worktreeRoot)
	if err != nil {
		root = worktreeRoot
	}
	state := &workspaceState{
		locks:     locks.NewManager(logger),
		revisions: tools.NewRevisionAuthorizer(),
	}
	actual, _ := workspaceStates.LoadOrStore(root, state)
	return actual.(*workspaceState)
}

// registerReadTools registers the read-only tool set on s.
// Included: read_file, read_lines, list_directory, grep_search.
func registerReadTools(s *server.MCPServer, lm *locks.Manager, revisions *tools.RevisionAuthorizer, worktreeRoot string, logger *slog.Logger) {
	// read_file
	s.AddTool(
		mcp.NewTool("read_file",
			mcp.WithDescription("Read the entire contents of a file within the worktree. Returns a revision token required for search_and_replace."),
			mcp.WithString("filepath", mcp.Required(), mcp.Description("Path to the file, relative to the worktree root.")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			start := time.Now()
			fp, err := req.RequireString("filepath")
			if err != nil {
				logger.Error("tool call failed", "tool", "read_file", "error", err, "duration_ms", time.Since(start).Milliseconds())
				return mcp.NewToolResultError(err.Error()), nil
			}
			content, toolErr := tools.ReadFile(ctx, worktreeRoot, fp, lm)
			if toolErr != nil {
				logger.Error("tool call failed", "tool", "read_file", "filepath", fp, "error", toolErr, "duration_ms", time.Since(start).Milliseconds())
				return mcp.NewToolResultError(toolErr.Error()), nil
			}
			abs, _ := worktree.Resolve(worktreeRoot, fp)
			revisions.Authorize(abs, tools.RevisionToken(content))
			logger.Info("tool call completed", "tool", "read_file", "filepath", fp, "duration_ms", time.Since(start).Milliseconds())
			return mcp.NewToolResultText(tools.WithRevision(content)), nil
		},
	)

	// read_lines
	s.AddTool(
		mcp.NewTool("read_lines",
			mcp.WithDescription("Read a range of lines from a file within the worktree (1-indexed, inclusive)."),
			mcp.WithString("filepath", mcp.Required(), mcp.Description("Path to the file, relative to the worktree root.")),
			mcp.WithNumber("start_line", mcp.Required(), mcp.Description("First line to read (1-indexed).")),
			mcp.WithNumber("end_line", mcp.Required(), mcp.Description("Last line to read (1-indexed, inclusive).")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			start := time.Now()
			fp, err := req.RequireString("filepath")
			if err != nil {
				logger.Error("tool call failed", "tool", "read_lines", "error", err, "duration_ms", time.Since(start).Milliseconds())
				return mcp.NewToolResultError(err.Error()), nil
			}
			startLine := req.GetInt("start_line", 1)
			endLine := req.GetInt("end_line", 1)
			content, toolErr := tools.ReadLines(ctx, worktreeRoot, fp, startLine, endLine, lm)
			if toolErr != nil {
				logger.Error("tool call failed", "tool", "read_lines", "filepath", fp, "start", startLine, "end", endLine, "error", toolErr, "duration_ms", time.Since(start).Milliseconds())
				return mcp.NewToolResultError(toolErr.Error()), nil
			}
			logger.Info("tool call completed", "tool", "read_lines", "filepath", fp, "start", startLine, "end", endLine, "duration_ms", time.Since(start).Milliseconds())
			return mcp.NewToolResultText(content), nil
		},
	)

	// list_directory
	s.AddTool(
		mcp.NewTool("list_directory",
			mcp.WithDescription("List the contents of a directory within the worktree."),
			mcp.WithString("dirpath", mcp.Required(), mcp.Description("Path to the directory to list, relative to the worktree root.")),
			mcp.WithBoolean("recursive", mcp.Description("If true, list recursively. Default: false.")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			start := time.Now()
			dirPath, err := req.RequireString("dirpath")
			if err != nil {
				logger.Error("tool call failed", "tool", "list_directory", "error", err, "duration_ms", time.Since(start).Milliseconds())
				return mcp.NewToolResultError(err.Error()), nil
			}
			recursive := req.GetBool("recursive", false)
			listing, toolErr := tools.ListDirectory(ctx, worktreeRoot, dirPath, recursive, lm)
			if toolErr != nil {
				logger.Error("tool call failed", "tool", "list_directory", "dirpath", dirPath, "recursive", recursive, "error", toolErr, "duration_ms", time.Since(start).Milliseconds())
				return mcp.NewToolResultError(toolErr.Error()), nil
			}
			logger.Info("tool call completed", "tool", "list_directory", "dirpath", dirPath, "recursive", recursive, "duration_ms", time.Since(start).Milliseconds())
			return mcp.NewToolResultText(listing), nil
		},
	)

	// grep_search
	s.AddTool(
		mcp.NewTool("grep_search",
			mcp.WithDescription("Search for a pattern (regex or literal) within files in the worktree."),
			mcp.WithString("query", mcp.Required(), mcp.Description("Search pattern (regex or literal string).")),
			mcp.WithString("directory", mcp.Description("Optional subdirectory to search within, relative to the worktree root.")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			start := time.Now()
			query, err := req.RequireString("query")
			if err != nil {
				logger.Error("tool call failed", "tool", "grep_search", "error", err, "duration_ms", time.Since(start).Milliseconds())
				return mcp.NewToolResultError(err.Error()), nil
			}
			directory := req.GetString("directory", "")
			results, toolErr := tools.GrepSearch(ctx, worktreeRoot, query, directory, lm)
			if toolErr != nil {
				logger.Error("tool call failed", "tool", "grep_search", "query", query, "directory", directory, "error", toolErr, "duration_ms", time.Since(start).Milliseconds())
				return mcp.NewToolResultError(toolErr.Error()), nil
			}
			logger.Info("tool call completed", "tool", "grep_search", "query", query, "directory", directory, "duration_ms", time.Since(start).Milliseconds())
			return mcp.NewToolResultText(results), nil
		},
	)

}

// registerWriteTools registers the write/mutate tool set on s.
// Included: create_file, search_and_replace.
func registerWriteTools(s *server.MCPServer, lm *locks.Manager, revisions *tools.RevisionAuthorizer, worktreeRoot string, logger *slog.Logger) {
	// create_file
	s.AddTool(
		mcp.NewTool("create_file",
			mcp.WithDescription("Create a new file with specified content within the worktree. Fails if the file already exists."),
			mcp.WithString("filepath", mcp.Required(), mcp.Description("Path for the new file, relative to the worktree root.")),
			mcp.WithString("content", mcp.Required(), mcp.Description("Content to write to the new file.")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			start := time.Now()
			fp, err := req.RequireString("filepath")
			if err != nil {
				logger.Error("tool call failed", "tool", "create_file", "error", err, "duration_ms", time.Since(start).Milliseconds())
				return mcp.NewToolResultError(err.Error()), nil
			}
			content, err := req.RequireString("content")
			if err != nil {
				logger.Error("tool call failed", "tool", "create_file", "filepath", fp, "error", err, "duration_ms", time.Since(start).Milliseconds())
				return mcp.NewToolResultError(err.Error()), nil
			}
			msg, toolErr := tools.CreateFile(ctx, worktreeRoot, fp, content, lm)
			if toolErr != nil {
				logger.Error("tool call failed", "tool", "create_file", "filepath", fp, "error", toolErr, "duration_ms", time.Since(start).Milliseconds())
				return mcp.NewToolResultError(toolErr.Error()), nil
			}
			logger.Info("tool call completed", "tool", "create_file", "filepath", fp, "duration_ms", time.Since(start).Milliseconds())
			return mcp.NewToolResultText(msg), nil
		},
	)

	// search_and_replace
	s.AddTool(
		mcp.NewTool("search_and_replace",
			mcp.WithDescription("Replace text in a file using the revision returned by read_file. You must read the file again after every successful edit before editing it again."),
			mcp.WithString("filepath", mcp.Required(), mcp.Description("Path to the file, relative to the worktree root.")),
			mcp.WithString("search_block", mcp.Required(), mcp.Description("The exact block of text to find.")),
			mcp.WithString("replace_block", mcp.Required(), mcp.Description("The text to replace the search_block with.")),
			mcp.WithString("expected_revision", mcp.Required(), mcp.Description("Revision token from the latest read_file result for this file.")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			start := time.Now()
			fp, err := req.RequireString("filepath")
			if err != nil {
				logger.Error("tool call failed", "tool", "search_and_replace", "error", err, "duration_ms", time.Since(start).Milliseconds())
				return mcp.NewToolResultError(err.Error()), nil
			}
			searchBlock, err := req.RequireString("search_block")
			if err != nil {
				logger.Error("tool call failed", "tool", "search_and_replace", "filepath", fp, "error", err, "duration_ms", time.Since(start).Milliseconds())
				return mcp.NewToolResultError(err.Error()), nil
			}
			replaceBlock, err := req.RequireString("replace_block")
			if err != nil {
				logger.Error("tool call failed", "tool", "search_and_replace", "filepath", fp, "error", err, "duration_ms", time.Since(start).Milliseconds())
				return mcp.NewToolResultError(err.Error()), nil
			}
			expectedRevision, err := req.RequireString("expected_revision")
			if err != nil {
				logger.Error("tool call failed", "tool", "search_and_replace", "filepath", fp, "error", err, "duration_ms", time.Since(start).Milliseconds())
				return mcp.NewToolResultError(err.Error()), nil
			}
			abs, resolveErr := worktree.Resolve(worktreeRoot, fp)
			if resolveErr != nil {
				logger.Error("tool call failed", "tool", "search_and_replace", "filepath", fp, "error", resolveErr, "duration_ms", time.Since(start).Milliseconds())
				return mcp.NewToolResultError(resolveErr.Error()), nil
			}
			if authErr := revisions.Verify(abs, expectedRevision); authErr != nil {
				logger.Error("tool call failed", "tool", "search_and_replace", "filepath", fp, "error", authErr, "duration_ms", time.Since(start).Milliseconds())
				return mcp.NewToolResultError(authErr.Error()), nil
			}
			result, toolErr := tools.SearchAndReplace(ctx, worktreeRoot, fp, searchBlock, replaceBlock, expectedRevision, lm)
			if toolErr != nil {
				logger.Error("tool call failed", "tool", "search_and_replace", "filepath", fp, "error", toolErr, "duration_ms", time.Since(start).Milliseconds())
				return mcp.NewToolResultError(toolErr.Error()), nil
			}
			revisions.Consume(abs, expectedRevision)
			logger.Info("tool call completed", "tool", "search_and_replace", "filepath", fp, "duration_ms", time.Since(start).Milliseconds())
			return mcp.NewToolResultText(result), nil
		},
	)

}

func newMCPHandler(profile Profile, worktreeRoot string, logger *slog.Logger) *server.StreamableHTTPServer {
	return newMCPHandlerWithState(profile, worktreeRoot, logger, workspaceStateFor(worktreeRoot, logger))
}
func newMCPHandlerWithState(profile Profile, worktreeRoot string, logger *slog.Logger, state *workspaceState) *server.StreamableHTTPServer {
	s := server.NewMCPServer("opendev", "1.0.0",
		server.WithToolCapabilities(true),
		server.WithToolHandlerMiddleware(func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
			return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				result, err := next(ctx, req)
				if err != nil {
					state.recordFailure(req.Params.Name, err.Error())
					return result, err
				}
				if result != nil && result.IsError {
					state.recordFailure(req.Params.Name, toolResultError(result))
				}
				return result, err
			}
		}),
	)
	switch profile {
	case ProfileRead:
		registerReadTools(s, state.locks, state.revisions, worktreeRoot, logger)
	case ProfileWrite:
		registerWriteTools(s, state.locks, state.revisions, worktreeRoot, logger)
	}
	return server.NewStreamableHTTPServer(s)
}

// toolResultError extracts a bounded error message from an error tool result.
func toolResultError(result *mcp.CallToolResult) string {
	var parts []string
	for _, c := range result.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	msg := strings.Join(parts, "\n")
	if len(msg) > 500 {
		msg = msg[:500]
	}
	return msg
}
