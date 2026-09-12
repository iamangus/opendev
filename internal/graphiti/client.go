// Package graphiti provides a small client for Graphiti's message and search API.
package graphiti

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

// Message is one item to add to a Graphiti group.
type Message struct {
	Content   string `json:"content"`
	RoleType  string `json:"role_type,omitempty"`
	Role      string `json:"role,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
}

// SearchOptions controls an optional Graphiti search limit.
type SearchOptions struct {
	MaxResults int `json:"max_results,omitempty"`
}

// Searcher is deliberately read-only. Consumers such as graphmcp depend on
// this interface rather than Client, so they cannot add Graphiti messages.
type Searcher interface {
	Search(context.Context, string, SearchOptions) (json.RawMessage, error)
}

// Client accesses one fixed Graphiti group.
type Client struct {
	baseURL    *url.URL
	groupID    string
	httpClient *http.Client
}

func NewClient(baseURL, groupID string) (*Client, error) {
	return NewClientWithHTTPClient(baseURL, groupID, http.DefaultClient)
}

func NewClientWithHTTPClient(baseURL, groupID string, httpClient *http.Client) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("invalid Graphiti URL")
	}
	if strings.TrimSpace(groupID) == "" {
		return nil, fmt.Errorf("Graphiti group ID is required")
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{baseURL: u, groupID: groupID, httpClient: httpClient}, nil
}

// AddMessages adds messages to the client's configured group.
func (c *Client) AddMessages(ctx context.Context, messages []Message) error {
	if len(messages) == 0 {
		return fmt.Errorf("at least one Graphiti message is required")
	}
	for i, message := range messages {
		if strings.TrimSpace(message.Content) == "" {
			return fmt.Errorf("Graphiti message %d content is required", i)
		}
	}
	return c.doJSON(ctx, "/messages", map[string]any{"group_id": c.groupID, "messages": messages}, nil)
}

// Search searches only the client's configured group. The raw JSON response is
// preserved because Graphiti search result schemas may evolve independently.
func (c *Client) Search(ctx context.Context, query string, options SearchOptions) (json.RawMessage, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("Graphiti search query is required")
	}
	payload := map[string]any{"query": query, "group_ids": []string{c.groupID}}
	if options.MaxResults > 0 {
		payload["max_facts"] = options.MaxResults
	}
	var result json.RawMessage
	if err := c.doJSON(ctx, "/search", payload, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Client) doJSON(ctx context.Context, path string, input, output any) error {
	data, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encode Graphiti request: %w", err)
	}
	u := c.baseURL.JoinPath(path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create Graphiti request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("perform Graphiti request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("Graphiti request failed: %s", resp.Status)
	}
	if output != nil {
		if err := json.NewDecoder(resp.Body).Decode(output); err != nil {
			return fmt.Errorf("decode Graphiti response: %w", err)
		}
	}
	return nil
}

var _ Searcher = (*Client)(nil)
