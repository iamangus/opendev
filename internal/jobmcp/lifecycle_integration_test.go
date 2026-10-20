//go:build integration

package jobmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iamangus/code-mcp/internal/agentfoundry"
	"github.com/iamangus/code-mcp/internal/dispatcher"
	"github.com/iamangus/code-mcp/internal/dispatchstore"
	githubpkg "github.com/iamangus/code-mcp/internal/github"
	"github.com/iamangus/code-mcp/internal/gitops"
	"github.com/iamangus/code-mcp/internal/manager"
	"github.com/iamangus/code-mcp/internal/pipeline"
)

// This exercises real Git, JSON stores and both production HTTP clients. The
// fake services only stand in for GitHub and model execution, not orchestration.
func TestLifecycleRecoversWriterHandoffThenRemediatesAndPublishes(t *testing.T) {
	for key, value := range map[string]string{"GIT_AUTHOR_NAME": "Test", "GIT_AUTHOR_EMAIL": "test@example.test", "GIT_COMMITTER_NAME": "Test", "GIT_COMMITTER_EMAIL": "test@example.test"} {
		t.Setenv(key, value)
	}
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	git(root, "init", "--bare", "-b", "main", origin)
	seed := filepath.Join(root, "seed")
	git(root, "clone", origin, seed)
	os.WriteFile(filepath.Join(seed, "code.txt"), []byte("initial\n"), 0644)
	git(seed, "add", ".")
	git(seed, "commit", "-m", "initial")
	git(seed, "push", "origin", "main")
	mgr, err := manager.New(filepath.Join(root, "repos"), gitops.NewExec(slog.Default(), ""), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.SyncRepo(origin, "repo", "main"); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "state")
	store, _ := pipeline.NewStore(state)
	job, _ := store.Create("repo", "Improve the code", "main", "", "planner", "writer", "reviewer")
	store.StartPlanning(job.ID)

	var mu sync.Mutex
	runs := map[string]*agentfoundry.Run{}
	denyReviewer := true
	agents := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost {
			var options agentfoundry.RunOptions
			if err := json.NewDecoder(r.Body).Decode(&options); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			if strings.Contains(options.TaskID, ":reviewer:") && denyReviewer {
				http.Error(w, "temporarily unavailable", 503)
				return
			}
			run := runs[options.TaskID]
			if run == nil {
				run = &agentfoundry.Run{ID: fmt.Sprintf("run-%d", len(runs)+1), Status: "running", WorkflowID: options.TaskID}
				runs[options.TaskID] = run
			}
			json.NewEncoder(w).Encode(map[string]string{"run_id": run.ID, "workflow_id": run.WorkflowID})
			return
		}
		if id := r.URL.Query().Get("task_id"); id != "" {
			if run := runs[id]; run != nil {
				json.NewEncoder(w).Encode(run)
				return
			}
			http.NotFound(w, r)
			return
		}
		for _, run := range runs {
			if strings.HasSuffix(r.URL.Path, "/"+run.ID) {
				json.NewEncoder(w).Encode(run)
				return
			}
		}
		http.NotFound(w, r)
	}))
	defer agents.Close()

	var integrationBranch string
	draft, merged := true, false
	ciSuccess := false
	githubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			var options map[string]any
			json.NewDecoder(r.Body).Decode(&options)
			integrationBranch = options["head"].(string)
			json.NewEncoder(w).Encode(githubpkg.PR{Number: 1, HTMLURL: "https://example.test/pr/1"})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
			json.NewEncoder(w).Encode([]githubpkg.PR{})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/1"):
			sha := git(origin, "rev-parse", integrationBranch)
			json.NewEncoder(w).Encode(githubpkg.PR{Number: 1, NodeID: "node", State: "open", Draft: draft, Merged: merged, Head: githubpkg.PRHead{SHA: sha}})
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			conclusion := "failure"
			if ciSuccess {
				conclusion = "success"
			}
			check := githubpkg.CheckRun{Name: "ci / OpenDev CI", Status: "completed", Conclusion: conclusion}
			check.Output.Summary = "Focused test failed"
			json.NewEncoder(w).Encode(githubpkg.PRChecks{TotalCount: 1, CheckRuns: []githubpkg.CheckRun{check}})
		case r.URL.Path == "/graphql":
			draft = false
			fmt.Fprint(w, `{"data":{"markPullRequestReadyForReview":{"pullRequest":{"isDraft":false}}}}`)
		case strings.HasSuffix(r.URL.Path, "/merge"):
			merged = true
			fmt.Fprint(w, `{"merged":true}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	defer githubServer.Close()
	client, _ := agentfoundry.NewClient(agents.URL, "")
	gh := githubpkg.NewHTTPClient("token", "owner", slog.Default(), githubpkg.WithBaseURL(githubServer.URL))
	var config Config
	newDispatcher := func() *dispatcher.Dispatcher {
		runStore, err := dispatchstore.New(state)
		if err != nil {
			t.Fatal(err)
		}
		d, err := dispatcher.New(client, runStore, dispatcher.Config{PollInterval: 5 * time.Millisecond, OutcomeHandler: func(ctx context.Context, run dispatcher.DispatchRun) error { return OutcomeHandler(config)(ctx, run) }})
		if err != nil {
			t.Fatal(err)
		}
		config = Config{Store: store, Controller: pipeline.NewController(store), Dispatcher: d, Worktrees: mgr, Registrar: &fakeRegistrar{}, GitHub: gh}
		return d
	}
	d := newDispatcher()
	defer func() { d.Close() }()
	wait := func(condition func(*pipeline.Job) bool) *pipeline.Job {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			current, err := store.Get(job.ID)
			if err == nil && condition(current) {
				return current
			}
			time.Sleep(5 * time.Millisecond)
		}
		current, _ := store.Get(job.ID)
		t.Fatalf("lifecycle stalled: %+v", current)
		return nil
	}
	complete := func(role, key, response string) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		found := false
		for taskID, run := range runs {
			if strings.Contains(taskID, ":"+role+":"+key+":") && run.Status == "running" {
				run.Status, run.Response = "completed", response
				found = true
			}
		}
		if !found {
			t.Fatalf("no running %s/%s", role, key)
		}
	}
	if _, err := d.StartPlanner(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	complete("planner", "job", `{"summary":"Improve code","tasks":[{"key":"one","title":"Improve","description":"Improve code","acceptance_criteria":["Works"]}],"integration_order":["one"]}`)
	current := wait(func(j *pipeline.Job) bool { return j.Plan != nil && j.Plan.Tasks[0].Status == pipeline.TaskWorking })
	os.WriteFile(filepath.Join(current.Plan.Tasks[0].WorktreePath, "code.txt"), []byte("first improvement\n"), 0644)
	complete("writer", "one", `{"status":"completed","changed_files":["code.txt"],"validation_evidence":[]}`)
	wait(func(j *pipeline.Job) bool { return j.Plan.Tasks[0].Status == pipeline.TaskReviewing })
	d.Close() // crash window: commit recorded, reviewer submission unavailable
	mu.Lock()
	denyReviewer = false
	mu.Unlock()
	store, err = pipeline.NewStore(state)
	if err != nil {
		t.Fatal(err)
	}
	d = newDispatcher()
	if err := d.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	wait(func(j *pipeline.Job) bool { return j.Plan.Tasks[0].ReviewerRunID != "" })
	complete("reviewer", "one", `{"verdict":"approved"}`)
	wait(func(j *pipeline.Job) bool { return j.Status == pipeline.JobAwaitingCI })
	if err := ObservePendingCI(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	current = wait(func(j *pipeline.Job) bool {
		return len(j.Plan.Tasks) == 2 && j.Plan.Tasks[1].Status == pipeline.TaskWorking
	})
	remediation := current.Plan.Tasks[1]
	os.WriteFile(filepath.Join(remediation.WorktreePath, "code.txt"), []byte("reviewed CI fix\n"), 0644)
	complete("writer", remediation.Key, `{"status":"completed","changed_files":["code.txt"],"validation_evidence":[]}`)
	wait(func(j *pipeline.Job) bool { return j.Plan.Tasks[1].ReviewerRunID != "" })
	complete("reviewer", remediation.Key, `{"verdict":"approved"}`)
	wait(func(j *pipeline.Job) bool { return j.Status == pipeline.JobAwaitingCI })
	mu.Lock()
	ciSuccess = true
	mu.Unlock()
	if err := ObservePendingCI(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	wait(func(j *pipeline.Job) bool { return j.HolisticReviewRunID != "" })
	complete("holistic", "job", `{"verdict":"approved","pull_request":{"title":"Improve code","body":"Reviewed changes with passing CI"}}`)
	current = wait(func(j *pipeline.Job) bool { return j.Status == pipeline.JobPublished })
	if current.MergeState != pipeline.MergeMerged || git(origin, "show", current.IntegrationSHA+":code.txt") != "reviewed CI fix" {
		t.Fatal("published artifact differs from reviewed integration")
	}
}
