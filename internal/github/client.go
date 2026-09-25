package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const defaultBaseURL = "https://api.github.com"

// HTTPClient implements Client using the GitHub REST API.
type HTTPClient struct {
	token   string
	owner   string
	baseURL string
	http    *http.Client
	logger  *slog.Logger
}

// HTTPClientOption configures an HTTPClient.
type HTTPClientOption func(*HTTPClient)

// WithBaseURL overrides the GitHub API base URL (used in tests).
func WithBaseURL(url string) HTTPClientOption {
	return func(c *HTTPClient) { c.baseURL = url }
}

// NewHTTPClient creates a new GitHub API client.
func NewHTTPClient(token, owner string, logger *slog.Logger, opts ...HTTPClientOption) *HTTPClient {
	c := &HTTPClient{
		token:   token,
		owner:   owner,
		baseURL: defaultBaseURL,
		http:    &http.Client{},
		logger:  logger,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// ListOwnedRepositories returns repositories owned by the authenticated user
// that belong to this client's configured owner. This seeds OpenDev's local
// repository mirror without exposing arbitrary clone URLs to agents.
func (c *HTTPClient) ListOwnedRepositories(ctx context.Context) ([]Repository, error) {
	start := time.Now()
	var repos []Repository
	if err := c.do(ctx, http.MethodGet, "/user/repos?affiliation=owner&per_page=100", nil, &repos); err != nil {
		c.logger.Error("github: ListOwnedRepositories failed", "owner", c.owner, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return nil, err
	}

	prefix := c.owner + "/"
	owned := repos[:0]
	for _, repo := range repos {
		if repo.FullName == "" || repo.FullName == prefix[:len(prefix)-1] || len(repo.FullName) <= len(prefix) || repo.FullName[:len(prefix)] != prefix {
			continue
		}
		owned = append(owned, repo)
	}
	c.logger.Info("github: owned repositories retrieved", "owner", c.owner, "count", len(owned), "duration_ms", time.Since(start).Milliseconds())
	return owned, nil
}

func (c *HTTPClient) GetRepository(ctx context.Context, name string) (*Repository, error) {
	start := time.Now()
	path := fmt.Sprintf("/repos/%s/%s", c.owner, name)

	var repo Repository
	if err := c.do(ctx, http.MethodGet, path, nil, &repo); err != nil {
		c.logger.Error("github: GetRepository failed", "repo", name, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return nil, err
	}
	c.logger.Debug("github: repository retrieved", "repo", name, "duration_ms", time.Since(start).Milliseconds())
	return &repo, nil
}

func (c *HTTPClient) CreateRepository(ctx context.Context, name, description string, private bool) (*Repository, error) {
	start := time.Now()
	path := "/user/repos"
	payload := map[string]any{
		"name":        name,
		"description": description,
		"private":     private,
		// New owned repositories need a default branch before the deterministic
		// OpenDev foundation can be committed and reviewed.
		"auto_init": true,
	}

	var repo Repository
	if err := c.do(ctx, http.MethodPost, path, payload, &repo); err != nil {
		c.logger.Error("github: CreateRepository failed", "repo", name, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return nil, err
	}
	c.logger.Info("github: repository created", "repo", repo.FullName, "duration_ms", time.Since(start).Milliseconds())
	return &repo, nil
}

func (c *HTTPClient) ForkPublicRepository(ctx context.Context, upstreamOwner, upstreamRepo, name string, private bool) (*Repository, error) {
	start := time.Now()
	path := fmt.Sprintf("/repos/%s/%s/forks", upstreamOwner, upstreamRepo)
	payload := map[string]any{
		"owner":   c.owner,
		"name":    name,
		"private": private,
	}

	var repo Repository
	if err := c.do(ctx, http.MethodPost, path, payload, &repo); err != nil {
		c.logger.Error("github: ForkPublicRepository failed", "upstream", upstreamOwner+"/"+upstreamRepo, "repo", name, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return nil, err
	}
	c.logger.Info("github: repository forked", "repo", repo.FullName, "upstream", upstreamOwner+"/"+upstreamRepo, "duration_ms", time.Since(start).Milliseconds())
	return &repo, nil
}

func (c *HTTPClient) CreatePR(ctx context.Context, opts CreatePROptions) (*PR, error) {
	start := time.Now()
	path := fmt.Sprintf("/repos/%s/%s/pulls", c.owner, opts.Repo)
	payload := map[string]any{
		"title": opts.Title,
		"head":  opts.Head,
		"base":  opts.Base,
		"body":  opts.Body,
		"draft": opts.Draft,
	}

	var pr PR
	if err := c.do(ctx, http.MethodPost, path, payload, &pr); err != nil {
		c.logger.Error("github: CreatePR failed", "repo", opts.Repo, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return nil, err
	}
	c.logger.Info("github: PR created", "repo", opts.Repo, "number", pr.Number, "duration_ms", time.Since(start).Milliseconds())
	return &pr, nil
}

func (c *HTTPClient) FindPR(ctx context.Context, repo, headBranch string) (*PR, error) {
	start := time.Now()
	path := fmt.Sprintf("/repos/%s/%s/pulls?head=%s:%s&state=open", c.owner, repo, c.owner, headBranch)

	var prs []PR
	if err := c.do(ctx, http.MethodGet, path, nil, &prs); err != nil {
		c.logger.Error("github: FindPR failed", "repo", repo, "head", headBranch, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return nil, err
	}
	if len(prs) == 0 {
		return nil, ErrNotFound
	}
	c.logger.Info("github: open PR found", "repo", repo, "head", headBranch, "number", prs[0].Number, "duration_ms", time.Since(start).Milliseconds())
	return &prs[0], nil
}

func (c *HTTPClient) UpdatePR(ctx context.Context, repo string, number int, title, body string) error {
	start := time.Now()
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", c.owner, repo, number)
	payload := map[string]any{"title": title, "body": body}

	if err := c.do(ctx, http.MethodPatch, path, payload, nil); err != nil {
		c.logger.Error("github: UpdatePR failed", "repo", repo, "number", number, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return err
	}
	c.logger.Info("github: PR updated", "repo", repo, "number", number, "duration_ms", time.Since(start).Milliseconds())
	return nil
}

func (c *HTTPClient) PromotePR(ctx context.Context, repo string, number int) error {
	start := time.Now()
	pr, err := c.GetPR(ctx, repo, number)
	if err != nil {
		c.logger.Error("github: PromotePR failed", "repo", repo, "number", number, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return err
	}
	if pr.NodeID == "" {
		err := fmt.Errorf("promote pull request #%d: missing GraphQL node ID", number)
		c.logger.Error("github: PromotePR failed", "repo", repo, "number", number, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return err
	}
	// The REST PATCH draft:false transition is not reliably observable to
	// follow-up REST reads. The GraphQL ready-for-review mutation is the
	// authoritative promotion path.
	payload := map[string]any{
		"query": `mutation($id: ID!) {
			markPullRequestReadyForReview(input: {pullRequestId: $id}) {
				pullRequest { isDraft }
			}
		}`,
		"variables": map[string]any{"id": pr.NodeID},
	}
	var result struct {
		Data struct {
			MarkPullRequestReadyForReview struct {
				PullRequest struct {
					IsDraft bool `json:"isDraft"`
				} `json:"pullRequest"`
			} `json:"markPullRequestReadyForReview"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := c.do(ctx, http.MethodPost, "/graphql", payload, &result); err != nil {
		c.logger.Error("github: PromotePR failed", "repo", repo, "number", number, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return err
	}
	if len(result.Errors) > 0 {
		err := fmt.Errorf("github promote pull request #%d: %s", number, result.Errors[0].Message)
		c.logger.Error("github: PromotePR failed", "repo", repo, "number", number, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return err
	}
	c.logger.Info("github: PR promoted", "repo", repo, "number", number, "duration_ms", time.Since(start).Milliseconds())
	return nil
}

func (c *HTTPClient) GetPR(ctx context.Context, repo string, number int) (*PR, error) {
	start := time.Now()
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", c.owner, repo, number)

	var pr PR
	if err := c.do(ctx, http.MethodGet, path, nil, &pr); err != nil {
		c.logger.Error("github: GetPR failed", "repo", repo, "number", number, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return nil, err
	}
	c.logger.Debug("github: PR retrieved", "repo", repo, "number", number, "duration_ms", time.Since(start).Milliseconds())
	return &pr, nil
}

func (c *HTTPClient) GetPRChecks(ctx context.Context, repo, ref string) (*PRChecks, error) {
	start := time.Now()
	path := fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs", c.owner, repo, ref)

	var checks PRChecks
	if err := c.do(ctx, http.MethodGet, path, nil, &checks); err != nil {
		c.logger.Error("github: GetPRChecks failed", "repo", repo, "ref", ref, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return nil, err
	}
	c.logger.Debug("github: PR checks retrieved", "repo", repo, "ref", ref, "duration_ms", time.Since(start).Milliseconds())
	return &checks, nil
}

func (c *HTTPClient) MergePR(ctx context.Context, repo string, number int, expectedSHA ...string) error {
	start := time.Now()
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/merge", c.owner, repo, number)
	payload := map[string]any{"merge_method": "squash"}
	if len(expectedSHA) > 0 && expectedSHA[0] != "" {
		payload["sha"] = expectedSHA[0]
	}
	var result struct {
		Merged  bool   `json:"merged"`
		Message string `json:"message"`
	}

	if err := c.do(ctx, http.MethodPut, path, payload, &result); err != nil {
		c.logger.Error("github: MergePR failed", "repo", repo, "number", number, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return err
	}
	if !result.Merged {
		return fmt.Errorf("github: merge PR %d: %s", number, result.Message)
	}
	c.logger.Info("github: PR merged", "repo", repo, "number", number, "duration_ms", time.Since(start).Milliseconds())
	return nil
}

// EnsureRequiredCheck protects an otherwise unprotected default branch with the
// named required check. Existing protection is never overwritten implicitly.
func (c *HTTPClient) EnsureRequiredCheck(ctx context.Context, repo, branch, check string) error {
	var current struct {
		RequiredStatusChecks *struct {
			Contexts []string `json:"contexts"`
		} `json:"required_status_checks"`
	}
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/branches/%s/protection", c.owner, repo, branch), nil, &current)
	if err == nil {
		if current.RequiredStatusChecks != nil {
			for _, context := range current.RequiredStatusChecks.Contexts {
				if context == check {
					return nil
				}
			}
		}
		return fmt.Errorf("existing branch protection for %s requires an explicit update to add %q", branch, check)
	}
	if errors.Is(err, ErrNotFound) {
		payload := map[string]any{"required_status_checks": map[string]any{"strict": true, "contexts": []string{check}}, "enforce_admins": false, "required_pull_request_reviews": nil, "restrictions": nil, "required_linear_history": false, "allow_force_pushes": false, "allow_deletions": false, "block_creations": false, "required_conversation_resolution": false, "lock_branch": false, "allow_fork_syncing": false}
		return c.do(ctx, http.MethodPut, fmt.Sprintf("/repos/%s/%s/branches/%s/protection", c.owner, repo, branch), payload, nil)
	}
	if strings.Contains(err.Error(), ": 403 ") {
		return fmt.Errorf("%w: %v", ErrPlanLimited, err)
	}
	return err
}

func (c *HTTPClient) do(ctx context.Context, method, path string, reqBody any, out any) error {
	var bodyReader io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: github API %s %s: %d %s", ErrNotFound, method, path, resp.StatusCode, string(respBody))
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("github API %s %s: %d %s", method, path, resp.StatusCode, string(respBody))
	}

	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// Compile-time check that HTTPClient implements Client.
var _ Client = (*HTTPClient)(nil)
