// Package agentfoundry provides the small subset of the AgentFoundry HTTP API
// needed by the OpenDev dispatcher.
package agentfoundry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// MCPServer configures an ephemeral MCP server for one agent run.
type MCPServer struct {
	Name      string            `json:"name"`
	URL       string            `json:"url"`
	Transport string            `json:"transport"`
	Headers   map[string]string `json:"headers,omitempty"`
}

// RunOptions controls an AgentFoundry run.
type RunOptions struct {
	Message    string      `json:"message"`
	TaskID     string      `json:"task_id,omitempty"`
	MCPServers []MCPServer `json:"mcp_servers,omitempty"`
}

// Run is the observable state returned by AgentFoundry.
type Run struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Response string `json:"response,omitempty"`
	Error    string `json:"error,omitempty"`
}

type runResponse struct {
	RunID string `json:"run_id"`
}

// Client calls AgentFoundry's run API. It never logs requests or headers.
type Client struct {
	baseURL    *url.URL
	httpClient *http.Client
	apiKey     string
}

func NewClient(baseURL, apiKey string) (*Client, error) {
	return NewClientWithHTTPClient(baseURL, apiKey, http.DefaultClient)
}

// NewClientWithHTTPClient is useful when callers need custom transport or timeout settings.
func NewClientWithHTTPClient(baseURL, apiKey string, httpClient *http.Client) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid AgentFoundry URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("invalid AgentFoundry URL scheme")
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{baseURL: u, httpClient: httpClient, apiKey: apiKey}, nil
}

// StartRun starts an asynchronous agent run. The caller owns retry safety and
// can use GetRunByTaskID to recover an accepted submission after a restart.
func (c *Client) StartRun(ctx context.Context, agentID string, options RunOptions) (string, error) {
	if strings.TrimSpace(agentID) == "" || strings.TrimSpace(options.Message) == "" {
		return "", fmt.Errorf("agent ID and message are required")
	}
	data, err := json.Marshal(options)
	if err != nil {
		return "", fmt.Errorf("encode AgentFoundry run request: %w", err)
	}
	var response runResponse
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/agents/"+url.PathEscape(agentID)+"/run", bytes.NewReader(data), &response); err != nil {
		return "", err
	}
	if response.RunID == "" {
		return "", fmt.Errorf("AgentFoundry run response omitted run_id")
	}
	return response.RunID, nil
}

// GetRun returns the latest state for a run.
func (c *Client) GetRun(ctx context.Context, runID string) (*Run, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, fmt.Errorf("run ID is required")
	}
	var run Run
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/runs/"+url.PathEscape(runID), nil, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// GetRunByTaskID finds an in-memory AgentFoundry run associated with taskID.
// It returns nil when AgentFoundry has no such run, including after its normal
// completed-run cleanup.
func (c *Client) GetRunByTaskID(ctx context.Context, taskID string) (*Run, error) {
	if strings.TrimSpace(taskID) == "" {
		return nil, fmt.Errorf("task ID is required")
	}
	u := c.baseURL.JoinPath("/api/v1/runs")
	query := u.Query()
	query.Set("task_id", taskID)
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create AgentFoundry request: %w", err)
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("perform AgentFoundry request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("AgentFoundry request failed: %s", resp.Status)
	}
	var run Run
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		return nil, fmt.Errorf("decode AgentFoundry response: %w", err)
	}
	return &run, nil
}

// CancelRun requests cancellation of a running run.
func (c *Client) CancelRun(ctx context.Context, runID string) error {
	if strings.TrimSpace(runID) == "" {
		return fmt.Errorf("run ID is required")
	}
	return c.doJSON(ctx, http.MethodPost, "/api/v1/runs/"+url.PathEscape(runID)+"/cancel", nil, nil)
}

func (c *Client) doJSON(ctx context.Context, method, path string, body io.Reader, output any) error {
	u := c.baseURL.JoinPath(path)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return fmt.Errorf("create AgentFoundry request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("perform AgentFoundry request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		// Do not include server response bodies: they can contain sensitive detail.
		return fmt.Errorf("AgentFoundry request failed: %s", resp.Status)
	}
	if output != nil {
		if err := json.NewDecoder(resp.Body).Decode(output); err != nil {
			return fmt.Errorf("decode AgentFoundry response: %w", err)
		}
	}
	return nil
}
