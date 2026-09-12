package github

import (
	"context"
	"sync"
)

// FakeClient is a test double for Client.
type FakeClient struct {
	mu    sync.Mutex
	Calls []FakeCall

	ListOwnedRepositoriesResult []Repository
	ListOwnedRepositoriesError  error
	GetRepositoryResult         *Repository
	GetRepositoryError          error
	CreateRepositoryResult      *Repository
	CreateRepositoryError       error
	ForkPublicRepositoryResult  *Repository
	ForkPublicRepositoryError   error
	CreatePRResult              *PR
	CreatePRError               error
	UpdatePRError               error
	PromotePRError              error
	GetPRResult                 *PR
	GetPRError                  error
	GetPRChecksResult           *PRChecks
	GetPRChecksError            error
	MergePRError                error
}

// FakeCall records a method invocation.
type FakeCall struct {
	Method string
	Args   []any
}

// NewFakeClient creates a FakeClient with sensible defaults.
func NewFakeClient() *FakeClient {
	return &FakeClient{
		ListOwnedRepositoriesResult: []Repository{{Name: "test", FullName: "test/test", CloneURL: "https://github.com/test/test.git", DefaultBranch: "main"}},
		GetRepositoryResult:         &Repository{Name: "test", FullName: "test/test", CloneURL: "https://github.com/test/test.git", DefaultBranch: "main"},
		CreateRepositoryResult:      &Repository{Name: "test", FullName: "test/test", CloneURL: "https://github.com/test/test.git", DefaultBranch: "main"},
		ForkPublicRepositoryResult:  &Repository{Name: "test", FullName: "test/test", CloneURL: "https://github.com/test/test.git", DefaultBranch: "main", Fork: true},
		CreatePRResult:              &PR{Number: 1, HTMLURL: "https://github.com/test/test/pull/1"},
		GetPRResult:                 &PR{Number: 1, HTMLURL: "https://github.com/test/test/pull/1"},
	}
}

func (f *FakeClient) ListOwnedRepositories(_ context.Context) ([]Repository, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, FakeCall{Method: "ListOwnedRepositories"})
	return f.ListOwnedRepositoriesResult, f.ListOwnedRepositoriesError
}

func (f *FakeClient) GetRepository(_ context.Context, name string) (*Repository, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, FakeCall{Method: "GetRepository", Args: []any{name}})
	return f.GetRepositoryResult, f.GetRepositoryError
}

func (f *FakeClient) CreateRepository(_ context.Context, name, description string, private bool) (*Repository, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, FakeCall{Method: "CreateRepository", Args: []any{name, description, private}})
	return f.CreateRepositoryResult, f.CreateRepositoryError
}

func (f *FakeClient) ForkPublicRepository(_ context.Context, upstreamOwner, upstreamRepo, name string, private bool) (*Repository, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, FakeCall{Method: "ForkPublicRepository", Args: []any{upstreamOwner, upstreamRepo, name, private}})
	return f.ForkPublicRepositoryResult, f.ForkPublicRepositoryError
}

func (f *FakeClient) CreatePR(_ context.Context, opts CreatePROptions) (*PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, FakeCall{Method: "CreatePR", Args: []any{opts}})
	return f.CreatePRResult, f.CreatePRError
}

func (f *FakeClient) UpdatePR(_ context.Context, repo string, number int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, FakeCall{Method: "UpdatePR", Args: []any{repo, number, body}})
	return f.UpdatePRError
}

func (f *FakeClient) PromotePR(_ context.Context, repo string, number int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, FakeCall{Method: "PromotePR", Args: []any{repo, number}})
	return f.PromotePRError
}

func (f *FakeClient) GetPR(_ context.Context, repo string, number int) (*PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, FakeCall{Method: "GetPR", Args: []any{repo, number}})
	return f.GetPRResult, f.GetPRError
}

func (f *FakeClient) GetPRChecks(_ context.Context, repo, ref string) (*PRChecks, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, FakeCall{Method: "GetPRChecks", Args: []any{repo, ref}})
	return f.GetPRChecksResult, f.GetPRChecksError
}

func (f *FakeClient) MergePR(_ context.Context, repo string, number int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, FakeCall{Method: "MergePR", Args: []any{repo, number}})
	return f.MergePRError
}

// Compile-time check that FakeClient implements Client.
var _ Client = (*FakeClient)(nil)
