package github

import (
	"context"
	"errors"
)

// ErrNotFound indicates that GitHub did not find the requested resource.
var ErrNotFound = errors.New("github: resource not found")

// ErrPlanLimited indicates GitHub rejected the operation because the account or
// repository plan does not support it, such as branch protection on a private
// repository without GitHub Pro.
var ErrPlanLimited = errors.New("github: operation unavailable on this GitHub plan")

// Client is the interface for GitHub API operations.
type Client interface {
	ListOwnedRepositories(ctx context.Context) ([]Repository, error)
	GetRepository(ctx context.Context, name string) (*Repository, error)
	CreateRepository(ctx context.Context, name, description string, private bool) (*Repository, error)
	ForkPublicRepository(ctx context.Context, upstreamOwner, upstreamRepo, name string, private bool) (*Repository, error)
	CreatePR(ctx context.Context, opts CreatePROptions) (*PR, error)
	UpdatePR(ctx context.Context, repo string, number int, title, body string) error
	PromotePR(ctx context.Context, repo string, number int) error
	GetPR(ctx context.Context, repo string, number int) (*PR, error)
	GetPRChecks(ctx context.Context, repo, ref string) (*PRChecks, error)
	MergePR(ctx context.Context, repo string, number int) error
}

// Repository contains the fields used to reconcile a GitHub repository.
// Source is GitHub's root upstream repository when this repository is a fork.
type Repository struct {
	Name          string      `json:"name"`
	FullName      string      `json:"full_name"`
	CloneURL      string      `json:"clone_url"`
	DefaultBranch string      `json:"default_branch"`
	Private       bool        `json:"private"`
	Fork          bool        `json:"fork"`
	Parent        *Repository `json:"parent"`
	Source        *Repository `json:"source"`
}

// CreatePROptions holds parameters for creating a pull request.
type CreatePROptions struct {
	Repo  string
	Title string
	Head  string
	Base  string
	Body  string
	Draft bool
}

// PR represents a GitHub pull request.
type PR struct {
	Number  int    `json:"number"`
	NodeID  string `json:"node_id"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Merged  bool   `json:"merged"`
	Draft   bool   `json:"draft"`
	Head    PRHead `json:"head"`
}

// PRHead identifies the commit at the head of a pull request.
type PRHead struct {
	SHA string `json:"sha"`
}

// PRChecks contains the check runs GitHub reports for a commit.
type PRChecks struct {
	TotalCount int        `json:"total_count"`
	CheckRuns  []CheckRun `json:"check_runs"`
}

// CheckRun is a GitHub check run associated with a commit.
type CheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	DetailsURL string `json:"details_url,omitempty"`
	Output     struct {
		Title   string `json:"title,omitempty"`
		Summary string `json:"summary,omitempty"`
	} `json:"output,omitempty"`
}
