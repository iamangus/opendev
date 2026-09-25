package jobmcp

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/iamangus/code-mcp/internal/dispatcher"
	"github.com/iamangus/code-mcp/internal/foundation"
	"github.com/iamangus/code-mcp/internal/github"
	"github.com/iamangus/code-mcp/internal/pipeline"
	"github.com/iamangus/code-mcp/internal/repositories"
	"github.com/iamangus/code-mcp/internal/repositorycatalog"
)

type recoveringPlanner struct {
	*fakeDispatcher
	calls int
}

func (r *recoveringPlanner) StartPlanner(_ context.Context, _ *pipeline.Job) (*dispatcher.DispatchRun, error) {
	r.calls++
	if r.calls == 1 {
		return nil, errors.New("dispatch state temporarily unavailable")
	}
	return &dispatcher.DispatchRun{RunID: "planner-run"}, nil
}

func TestResumePlanningAfterDispatchStartFailure(t *testing.T) {
	store, err := pipeline.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.Create("repo", "directive", "main", "", "planner", "writer", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartPlanning(job.ID); err != nil {
		t.Fatal(err)
	}
	planner := &recoveringPlanner{fakeDispatcher: &fakeDispatcher{}}
	config := Config{Store: store, Dispatcher: planner}
	if err := ResumeFoundationPlanning(context.Background(), config); err == nil {
		t.Fatal("expected first dispatch to fail")
	}
	if err := ResumeFoundationPlanning(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	if planner.calls != 2 {
		t.Fatalf("planner was not retried: %d calls", planner.calls)
	}
	stored, err := store.Get(job.ID)
	if err != nil || stored.Status != pipeline.JobPlanning {
		t.Fatalf("job status: %+v %v", stored, err)
	}
}

type foundationTestManager struct{ root string }

func (m foundationTestManager) SyncRepo(string, string, ...string) error { return nil }
func (m foundationTestManager) RepoDir(name string) string               { return filepath.Join(m.root, name) }
func (m foundationTestManager) CreateFoundationBranch(string, string, string) (string, string, error) {
	return "", "", errors.New("must not recreate pending foundation branch")
}

type foundationTestMetadata struct{}

func (foundationTestMetadata) ReadGitMetadata(context.Context, string) (repositorycatalog.GitMetadata, error) {
	return repositorycatalog.GitMetadata{OriginURL: "https://github.com/acme/repo.git", HeadSHA: "base-sha"}, nil
}

func TestResumeFoundationJobAfterVerifiedMerge(t *testing.T) {
	root := t.TempDir()
	store, err := pipeline.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.Create("repo", "directive", "main", "", "planner", "writer", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkFoundationPending(job.ID); err != nil {
		t.Fatal(err)
	}
	catalog, err := repositorycatalog.New(t.TempDir(), foundationTestMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	client := github.NewFakeClient()
	client.GetRepositoryResult = &github.Repository{Name: "repo", FullName: "acme/repo", CloneURL: "https://github.com/acme/repo.git", DefaultBranch: "main", Private: true}
	client.GetPRResult = &github.PR{Number: 7, Merged: true, State: "closed", Head: github.PRHead{SHA: "verified-sha"}}
	client.GetPRChecksResult = &github.PRChecks{CheckRuns: []github.CheckRun{{Name: "ci / OpenDev CI", Status: "completed", Conclusion: "success"}}}
	service, err := repositories.New(foundationTestManager{root: root}, catalog, client, "acme")
	if err != nil {
		t.Fatal(err)
	}
	record, err := service.Lookup(context.Background(), "repo")
	if err != nil {
		t.Fatal(err)
	}
	record.Foundation = &repositorycatalog.Foundation{Version: foundation.Version, Status: "pending", PRNumber: 7, Branch: "opendev/foundation-" + foundation.Version}
	if _, err := catalog.Save(*record); err != nil {
		t.Fatal(err)
	}
	planner := &recoveringPlanner{fakeDispatcher: &fakeDispatcher{}, calls: 1}
	if err := ResumeFoundationPlanning(context.Background(), Config{Store: store, Dispatcher: planner, Repositories: service}); err != nil {
		t.Fatal(err)
	}
	updated, err := store.Get(job.ID)
	if err != nil || updated.Status != pipeline.JobPlanning || updated.FoundationPending || planner.calls != 2 {
		t.Fatalf("job not resumed: %+v, planner=%d, err=%v", updated, planner.calls, err)
	}
	ready, err := catalog.Get("repo")
	if err != nil || ready.Foundation.Status != "ready" {
		t.Fatalf("foundation not recorded ready: %+v %v", ready, err)
	}
}
