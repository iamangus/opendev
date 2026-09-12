// Package graphmcp exposes a read-only repository graph MCP server.
package graphmcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/iamangus/code-mcp/internal/graphiti"
	"github.com/iamangus/code-mcp/internal/repositoryindex"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// IndexStateStore is the read-only portion of repository index state storage.
type IndexStateStore interface {
	Get(repository string) (*repositoryindex.State, error)
}

type Config struct {
	Searcher graphiti.Searcher
	State    IndexStateStore
}

// New creates a Streamable HTTP MCP endpoint with no graph mutation tools.
func New(config Config) *server.StreamableHTTPServer {
	s := server.NewMCPServer("opendev", "1.0.0", server.WithToolCapabilities(true))
	s.AddTool(mcp.NewTool("search_repository_graph",
		mcp.WithDescription("Search read-only Graphiti repository knowledge."),
		mcp.WithString("repository", mcp.Description("Optional repository name to narrow the search.")),
		mcp.WithString("query", mcp.Required(), mcp.Description("Question or search terms.")),
		mcp.WithNumber("max_results", mcp.Description("Optional maximum number of results.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if config.Searcher == nil {
			return mcp.NewToolResultError("repository graph search is not configured"), nil
		}
		query, err := req.RequireString("query")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if repository := req.GetString("repository", ""); repository != "" {
			query = repository + ": " + query
		}
		result, err := config.Searcher.Search(ctx, query, graphiti.SearchOptions{MaxResults: req.GetInt("max_results", 0)})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(string(result)), nil
	})
	s.AddTool(mcp.NewTool("get_repository_index_state",
		mcp.WithDescription("Get durable, read-only repository graph indexing state."),
		mcp.WithString("repository", mcp.Required(), mcp.Description("Repository name, for example owner/repository.")),
	), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if config.State == nil {
			return mcp.NewToolResultError("repository index state is not configured"), nil
		}
		repository, err := req.RequireString("repository")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		state, err := config.State.Get(repository)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if state == nil {
			return mcp.NewToolResultError(fmt.Sprintf("repository index state not found for %q", repository)), nil
		}
		data, err := json.Marshal(state)
		if err != nil {
			return mcp.NewToolResultError("encode repository index state: " + err.Error()), nil
		}
		return mcp.NewToolResultText(string(data)), nil
	})
	return server.NewStreamableHTTPServer(s)
}
