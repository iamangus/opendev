package jobmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/iamangus/code-mcp/internal/dispatcher"
	githubpkg "github.com/iamangus/code-mcp/internal/github"
	"github.com/iamangus/code-mcp/internal/pipeline"
)

func TestRoleEndpointsExposeOnlyAssignedTools(t *testing.T) {
	for _, tc := range []struct {
		role Role
		want []string
	}{
		{RolePlanner, []string{"get_code_job"}},
		{RoleWriter, []string{"get_code_job", "get_task_diff"}},
		{RoleReviewer, []string{"get_code_job", "get_task_diff"}},
		{RoleHolistic, []string{"get_code_job"}},
		{RoleAdmin, []string{"create_code_job", "fork_public_repository", "get_code_job", "get_code_job_inspection", "get_code_task_inspection", "get_task_diff", "lookup_repository", "provision_repository", "publish_approved_code_job", "reset_holistic_review", "retry_code_task", "start_holistic_rereview", "start_planning"}},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			ts := httptest.NewServer(NewRole(Config{}, tc.role))
			defer ts.Close()
			got := listTools(t, ts.URL)
			if len(got) != len(tc.want) {
				t.Fatalf("tools = %v, want %v", got, tc.want)
			}
			for _, name := range tc.want {
				if !got[name] {
					t.Fatalf("missing tool %q in %v", name, got)
				}
			}
		})
	}
}

func listTools(t *testing.T, url string) map[string]bool {
	t.Helper()
	post := func(body, session string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		bodyBytes, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		return resp, bodyBytes
	}
	resp, _ := post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`, "")
	_, raw := post(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`, resp.Header.Get("Mcp-Session-Id"))
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "data: ") {
			raw = []byte(strings.TrimPrefix(strings.TrimSpace(line), "data: "))
			break
		}
	}
	var envelope struct {
		Result struct {
			Tools []struct{ Name string } `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("parse tools/list response: %v; raw: %s", err, raw)
	}
	tools := make(map[string]bool, len(envelope.Result.Tools))
	for _, tool := range envelope.Result.Tools {
		tools[tool.Name] = true
	}
	return tools
}

type fakeDispatcher struct {
	writerTasks      []pipeline.Task
	reviewerTasks    []pipeline.Task
	plannerRevisions int
}

func (f *fakeDispatcher) StartPlanner(context.Context, *pipeline.Job) (*dispatcher.DispatchRun, error) {
	return nil, fmt.Errorf("unexpected planner dispatch")
}
func (f *fakeDispatcher) StartPlannerRevision(_ context.Context, _ *pipeline.Job) (*dispatcher.DispatchRun, error) {
	f.plannerRevisions++
	return &dispatcher.DispatchRun{RunID: fmt.Sprintf("planner-rev-%d", f.plannerRevisions), TaskKey: "revision"}, nil
}
func (f *fakeDispatcher) StartWriter(_ context.Context, _ *pipeline.Job, task *pipeline.Task) (*dispatcher.DispatchRun, error) {
	f.writerTasks = append(f.writerTasks, *task)
	return &dispatcher.DispatchRun{RunID: "writer-" + task.Key}, nil
}
func (f *fakeDispatcher) StartReviewer(_ context.Context, _ *pipeline.Job, task *pipeline.Task) (*dispatcher.DispatchRun, error) {
	f.reviewerTasks = append(f.reviewerTasks, *task)
	return &dispatcher.DispatchRun{RunID: "reviewer-" + task.Key}, nil
}
func (f *fakeDispatcher) StartHolistic(context.Context, *pipeline.Job) (*dispatcher.DispatchRun, error) {
	return &dispatcher.DispatchRun{RunID: "holistic"}, nil
}
func (*fakeDispatcher) Supersede(context.Context, string, string) error { return nil }
func (*fakeDispatcher) InspectJob(context.Context, string) ([]dispatcher.DispatchRun, error) {
	return nil, nil
}

type fakeWorktrees struct {
	created    []string
	hasChanges bool
	headCommit string
}

func (f *fakeWorktrees) CreateWorktree(_, branch, _ string) (string, error) {
	f.created = append(f.created, branch)
	return "/work/" + branch, nil
}
func (*fakeWorktrees) Commit(string) (string, error)     { return "commit-sha", nil }
func (f *fakeWorktrees) HasChanges(string) (bool, error) { return f.hasChanges, nil }
func (*fakeWorktrees) MergeTask(string, string, string) (string, error) {
	return "integration-sha", nil
}
func (*fakeWorktrees) PushBranch(string, string) error { return nil }
func (f *fakeWorktrees) HeadCommit(string) (string, error) {
	if f.headCommit != "" {
		return f.headCommit, nil
	}
	return "base-sha", nil
}
func (*fakeWorktrees) DiffRange(string, string, string) (string, error) { return "diff", nil }

type fakeRegistrar struct{ branches []string }

func (f *fakeRegistrar) RegisterWorktree(_, branch, _ string) {
	f.branches = append(f.branches, branch)
}

func (*fakeRegistrar) RegisterReference(_, _, _ string) {}

func TestStartReadyWritersCreatesSlashFreeTaskBranches(t *testing.T) {
	store, err := pipeline.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.Create("repo", "directive", "main", "", "", "writer", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartPlanning(job.ID); err != nil {
		t.Fatal(err)
	}
	job, err = store.SubmitPlan(job.ID, pipeline.Plan{Summary: "plan", Tasks: []pipeline.Task{
		{Key: "api/v1", Title: "API", Description: "API", AcceptanceCriteria: []string{"works"}},
		{Key: "depends", Title: "Depends", Description: "Depends", AcceptanceCriteria: []string{"works"}, DependsOn: []string{"api/v1"}},
	}, IntegrationOrder: []string{"api/v1", "depends"}})
	if err != nil {
		t.Fatal(err)
	}
	dispatch := &fakeDispatcher{}
	worktrees := &fakeWorktrees{}
	registrar := &fakeRegistrar{}
	config := Config{Store: store, Controller: pipeline.NewController(store), Dispatcher: dispatch, Worktrees: worktrees, Registrar: registrar, Logger: slog.Default()}
	runs, err := startReadyWriters(context.Background(), job, config)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || len(dispatch.writerTasks) != 1 || dispatch.writerTasks[0].Branch != "opendev-task-"+job.ID+"-api-v1" {
		t.Fatalf("unexpected writer starts: runs=%v tasks=%+v", runs, dispatch.writerTasks)
	}
	updated, err := store.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := findTask(updated, "api/v1"); got.Status != pipeline.TaskWorking || got.Branch != dispatch.writerTasks[0].Branch {
		t.Fatalf("writer state was not recorded: %+v", got)
	}
	if got := findTask(updated, "depends"); got.Status != pipeline.TaskPlanned {
		t.Fatalf("dependent task should not start: %+v", got)
	}
}

func TestPlannerOutcomeStartsWritersAndInvalidResponseDoesNotApply(t *testing.T) {
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
	dispatch := &fakeDispatcher{}
	config := Config{Store: store, Controller: pipeline.NewController(store), Dispatcher: dispatch, Worktrees: &fakeWorktrees{}, Registrar: &fakeRegistrar{}}
	handler := OutcomeHandler(config)
	invalid := dispatcher.DispatchRun{JobID: job.ID, Role: dispatcher.RolePlanner, Status: "completed", Response: `{"summary":"missing tasks"}`}
	if err := handler(context.Background(), invalid); err == nil {
		t.Fatal("invalid plan response was accepted")
	}
	current, _ := store.Get(job.ID)
	if current.Plan != nil || current.Status != pipeline.JobPlanning {
		t.Fatalf("invalid plan changed state: %+v", current)
	}
	valid := dispatcher.DispatchRun{JobID: job.ID, Role: dispatcher.RolePlanner, Status: "completed", Response: `{"summary":"plan","tasks":[{"key":"one","title":"One","description":"Do one","acceptance_criteria":["works"]}],"integration_order":["one"]}`}
	if err := handler(context.Background(), valid); err != nil {
		t.Fatal(err)
	}
	current, _ = store.Get(job.ID)
	if len(dispatch.writerTasks) != 1 || findTask(current, "one").Status != pipeline.TaskWorking {
		t.Fatalf("planner outcome did not start writer: %+v", current)
	}
}

func TestWriterNoChangesSkipsCommitAndReview(t *testing.T) {
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
	job, err = store.SubmitPlan(job.ID, pipeline.Plan{Summary: "plan", Tasks: []pipeline.Task{{Key: "one", Title: "One", Description: "Inspect", AcceptanceCriteria: []string{"checked"}}}, IntegrationOrder: []string{"one"}})
	if err != nil {
		t.Fatal(err)
	}
	dispatch := &fakeDispatcher{}
	config := Config{Store: store, Controller: pipeline.NewController(store), Dispatcher: dispatch, Worktrees: &fakeWorktrees{}, Registrar: &fakeRegistrar{}}
	if _, err := startReadyWriters(context.Background(), job, config); err != nil {
		t.Fatal(err)
	}
	handler := OutcomeHandler(config)
	run := dispatcher.DispatchRun{JobID: job.ID, Role: dispatcher.RoleWriter, TaskKey: "one", RunID: "writer-one", Status: "completed", Response: `{"status":"no_changes","reason":"the existing implementation already satisfies the task","validation_evidence":[]}`}
	if err := handler(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	current, _ := store.Get(job.ID)
	task := findTask(current, "one")
	if task.Status != pipeline.TaskNoChanges || current.Status != pipeline.JobNoChanges || len(dispatch.reviewerTasks) != 0 {
		t.Fatalf("no-change writer outcome advanced incorrectly: job=%+v task=%+v", current, task)
	}
}

func TestWriterCompletedWithoutChangesIsRecordedWithoutCommit(t *testing.T) {
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
	job, err = store.SubmitPlan(job.ID, pipeline.Plan{Summary: "plan", Tasks: []pipeline.Task{{Key: "one", Title: "One", Description: "Implement", AcceptanceCriteria: []string{"works"}}}, IntegrationOrder: []string{"one"}})
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Store: store, Controller: pipeline.NewController(store), Dispatcher: &fakeDispatcher{}, Worktrees: &fakeWorktrees{}, Registrar: &fakeRegistrar{}}
	if _, err := startReadyWriters(context.Background(), job, config); err != nil {
		t.Fatal(err)
	}
	err = OutcomeHandler(config)(context.Background(), dispatcher.DispatchRun{JobID: job.ID, Role: dispatcher.RoleWriter, TaskKey: "one", RunID: "writer-one", Status: "completed", Response: `{"status":"completed","summary":"existing code already satisfies the task","changed_files":[],"validation_evidence":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	current, _ := store.Get(job.ID)
	if task := findTask(current, "one"); task.Status != pipeline.TaskNoChanges || current.Status != pipeline.JobNoChanges {
		t.Fatalf("completed clean worktree was not recorded as no changes: job=%+v task=%+v", current, task)
	}
}

func TestUnverifiedWriterBlockerSubmitsWorkForReview(t *testing.T) {
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
	job, err = store.SubmitPlan(job.ID, pipeline.Plan{Summary: "plan", Tasks: []pipeline.Task{{Key: "one", Title: "One", Description: "Implement", AcceptanceCriteria: []string{"works"}}}, IntegrationOrder: []string{"one"}})
	if err != nil {
		t.Fatal(err)
	}
	dispatch := &fakeDispatcher{}
	worktrees := &fakeWorktrees{hasChanges: true}
	config := Config{Store: store, Controller: pipeline.NewController(store), Dispatcher: dispatch, Worktrees: worktrees, Registrar: &fakeRegistrar{}}
	if _, err := startReadyWriters(context.Background(), job, config); err != nil {
		t.Fatal(err)
	}
	handler := OutcomeHandler(config)
	run := dispatcher.DispatchRun{JobID: job.ID, Role: dispatcher.RoleWriter, TaskKey: "one", RunID: "writer-one", Status: "completed", Response: `{"status":"blocked","summary":"claimed tooling problems","reason":"revision-token synchronization failures","validation_evidence":[]}`}
	if err := handler(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	current, _ := store.Get(job.ID)
	task := findTask(current, "one")
	if task.Status != pipeline.TaskReviewing || task.CommitSHA != "commit-sha" || task.WriterRunID != "writer-one" {
		t.Fatalf("unverified blocker did not submit work for review: job=%+v task=%+v", current, task)
	}
	if current.Status != pipeline.JobReviewing || len(dispatch.reviewerTasks) != 1 {
		t.Fatalf("reviewer not started for unverified blocker: job=%+v reviewers=%d", current, len(dispatch.reviewerTasks))
	}
}

func TestVerifiedWriterBlockerBlocksTask(t *testing.T) {
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
	job, err = store.SubmitPlan(job.ID, pipeline.Plan{Summary: "plan", Tasks: []pipeline.Task{{Key: "one", Title: "One", Description: "Implement", AcceptanceCriteria: []string{"works"}}}, IntegrationOrder: []string{"one"}})
	if err != nil {
		t.Fatal(err)
	}
	dispatch := &fakeDispatcher{}
	worktrees := &fakeWorktrees{hasChanges: true}
	config := Config{Store: store, Controller: pipeline.NewController(store), Dispatcher: dispatch, Worktrees: worktrees, Registrar: &fakeRegistrar{},
		WorktreeToolFailures: func(path string, since time.Time) []ToolFailureInfo {
			return []ToolFailureInfo{
				{Time: time.Now().UTC(), Tool: "search_and_replace", Error: "revision mismatch"},
				{Time: time.Now().UTC(), Tool: "search_and_replace", Error: "revision mismatch again"},
				{Time: time.Now().UTC(), Tool: "read_file", Error: "unreadable"},
			}
		}}
	if _, err := startReadyWriters(context.Background(), job, config); err != nil {
		t.Fatal(err)
	}
	handler := OutcomeHandler(config)
	run := dispatcher.DispatchRun{JobID: job.ID, Role: dispatcher.RoleWriter, TaskKey: "one", RunID: "writer-one", Status: "completed", Response: `{"status":"blocked","summary":"real tooling problem","reason":"revision mismatch","validation_evidence":[]}`}
	if err := handler(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	current, _ := store.Get(job.ID)
	task := findTask(current, "one")
	if task.Status != pipeline.TaskBlocked || task.WriterRunID != "writer-one" {
		t.Fatalf("verified blocker did not block the task: job=%+v task=%+v", current, task)
	}
}

func TestWriterAndReviewerOutcomesAdvanceStages(t *testing.T) {
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
	job, err = store.SubmitPlan(job.ID, pipeline.Plan{Summary: "plan", Tasks: []pipeline.Task{{Key: "one", Title: "One", Description: "Do one", AcceptanceCriteria: []string{"works"}}}, IntegrationOrder: []string{"one"}})
	if err != nil {
		t.Fatal(err)
	}
	dispatch := &fakeDispatcher{}
	github := githubpkg.NewFakeClient()
	github.CreatePRResult = &githubpkg.PR{Number: 9, HTMLURL: "https://example.test/pr/9"}
	github.GetPRResult = &githubpkg.PR{Number: 9, State: "open", Draft: true, Head: githubpkg.PRHead{SHA: "integration-sha"}}
	config := Config{Store: store, Controller: pipeline.NewController(store), Dispatcher: dispatch, Worktrees: &fakeWorktrees{hasChanges: true}, Registrar: &fakeRegistrar{}, GitHub: github}
	if _, err := startReadyWriters(context.Background(), job, config); err != nil {
		t.Fatal(err)
	}
	handler := OutcomeHandler(config)
	if err := handler(context.Background(), dispatcher.DispatchRun{JobID: job.ID, Role: dispatcher.RoleWriter, TaskKey: "one", RunID: "writer-one", Status: "completed", Response: `{"status":"completed","validation_evidence":[{"command":"go test ./...","result":"passed"}]}`}); err != nil {
		t.Fatal(err)
	}
	current, _ := store.Get(job.ID)
	task := findTask(current, "one")
	if task.Status != pipeline.TaskReviewing || task.CommitSHA != "commit-sha" || task.ReviewerRunID != "reviewer-one" {
		t.Fatalf("writer outcome did not commit and start review: %+v", task)
	}
	if err := handler(context.Background(), dispatcher.DispatchRun{JobID: job.ID, Role: dispatcher.RoleReviewer, TaskKey: "one", RunID: "reviewer-one", Status: "completed", Response: `{"verdict":"approved"}`}); err != nil {
		t.Fatal(err)
	}
	current, _ = store.Get(job.ID)
	if findTask(current, "one").Status != pipeline.TaskIntegrated {
		t.Fatalf("review outcome did not integrate task: %+v", current)
	}
	if current.Status != pipeline.JobAwaitingCI || current.CI == nil || current.CI.HeadSHA != "integration-sha" {
		t.Fatalf("integrated task did not enter draft PR CI: %+v", current)
	}
}

func TestRepeatedIdenticalCIFailureEscalatesToPlanner(t *testing.T) {
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
	job, err = store.SubmitPlan(job.ID, pipeline.Plan{Summary: "plan", Tasks: []pipeline.Task{{Key: "one", Title: "One", Description: "Work", AcceptanceCriteria: []string{"works"}}}, IntegrationOrder: []string{"one"}})
	if err != nil {
		t.Fatal(err)
	}
	dispatch := &fakeDispatcher{}
	github := githubpkg.NewFakeClient()
	github.CreatePRResult = &githubpkg.PR{Number: 4, HTMLURL: "https://example.test/pr/4"}
	github.GetPRResult = &githubpkg.PR{Number: 4, State: "open", Draft: true, Head: githubpkg.PRHead{SHA: "integration-sha"}}
	github.GetPRChecksResult = &githubpkg.PRChecks{CheckRuns: []githubpkg.CheckRun{{Name: "ci / OpenDev CI", Status: "completed", Conclusion: "failure", Output: struct {
		Title   string `json:"title,omitempty"`
		Summary string `json:"summary,omitempty"`
	}{Summary: "npm test failed: module not found"}}}}
	worktrees := &fakeWorktrees{hasChanges: true}
	config := Config{Store: store, Controller: pipeline.NewController(store), Dispatcher: dispatch, Worktrees: worktrees, Registrar: &fakeRegistrar{}, GitHub: github}
	if _, err := startReadyWriters(context.Background(), job, config); err != nil {
		t.Fatal(err)
	}
	handler := OutcomeHandler(config)
	// First writer run integrates.
	if err := handler(context.Background(), dispatcher.DispatchRun{JobID: job.ID, Role: dispatcher.RoleWriter, TaskKey: "one", RunID: "writer-one", Status: "completed", Response: `{"status":"completed","summary":"work done","changed_files":["a.go"],"validation_evidence":[]}`}); err != nil {
		t.Fatal(err)
	}
	// Review approves; integrate.
	if err := handler(context.Background(), dispatcher.DispatchRun{JobID: job.ID, Role: dispatcher.RoleReviewer, TaskKey: "one", RunID: "reviewer-one", Status: "completed", Response: `{"verdict":"approved","summary":"good","findings":[],"acceptance_criteria":[],"reason":""}`}); err != nil {
		t.Fatal(err)
	}
	current, _ := store.Get(job.ID)
	if _, _, err := config.Controller.StartCIRemediation(current.ID, "fp-1", nil); err == nil {
		t.Fatal("remediation requires awaiting CI state")
	}
	// First identical failure: remediation writer.
	if err := ObservePendingCI(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	current, _ = store.Get(job.ID)
	remediationStarted := false
	for i := range current.Plan.Tasks {
		if current.Plan.Tasks[i].Kind == pipeline.TaskRemediation {
			remediationStarted = true
		}
	}
	if !remediationStarted {
		t.Fatalf("first failure should start remediation: %+v", current.Plan.Tasks)
	}
	// Complete remediation and integrate a new head; same fingerprint again.
	if err := handler(context.Background(), dispatcher.DispatchRun{JobID: job.ID, Role: dispatcher.RoleWriter, TaskKey: "ci-remediation-2", RunID: "writer-ci-remediation-2", Status: "completed", Response: `{"status":"completed","summary":"fixed","changed_files":["b.go"],"validation_evidence":[]}`}); err != nil {
		t.Fatal(err)
	}
	if err := handler(context.Background(), dispatcher.DispatchRun{JobID: job.ID, Role: dispatcher.RoleReviewer, TaskKey: "ci-remediation-2", RunID: "reviewer-ci-remediation-2", Status: "completed", Response: `{"verdict":"approved","summary":"good","findings":[],"acceptance_criteria":[],"reason":""}`}); err != nil {
		t.Fatal(err)
	}
	// Second identical failure: planner revision, not another remediation task.
	if err := ObservePendingCI(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	current, _ = store.Get(job.ID)
	if current.Status != pipeline.JobCIReplanning || dispatch.plannerRevisions != 1 {
		t.Fatalf("second identical failure should escalate to planner: status=%s revisions=%d", current.Status, dispatch.plannerRevisions)
	}
	// Planner revision appends namespaced tasks and dispatches their writers.
	if err := handler(context.Background(), dispatcher.DispatchRun{JobID: job.ID, Role: dispatcher.RolePlanner, TaskKey: "revision", RunID: "planner-rev-1", Status: "completed", Response: `{"summary":"revised","tasks":[{"key":"root-cause","title":"Root cause","description":"fix root cause","acceptance_criteria":["CI passes"]}]}`}); err != nil {
		t.Fatal(err)
	}
	current, _ = store.Get(job.ID)
	if current.Status != pipeline.JobWorking {
		t.Fatalf("revision should dispatch its writers: %s", current.Status)
	}
	var revisionTask *pipeline.Task
	for i := range current.Plan.Tasks {
		if current.Plan.Tasks[i].Key == "rev1-root-cause" {
			revisionTask = &current.Plan.Tasks[i]
		}
	}
	if revisionTask == nil || revisionTask.Status != pipeline.TaskWorking || revisionTask.WriterAttempts != 1 {
		t.Fatalf("revision task not dispatched: %+v", current.Plan.Tasks)
	}
}

// The re-plan cap itself is covered in the pipeline store tests.

func TestStartReadyWritersReleasesOnlyIntegratedDependencies(t *testing.T) {
	_, _, job := approvedJob(t)
	job.Plan.Tasks = append(job.Plan.Tasks, pipeline.Task{Key: "dependent", Title: "Dependent", Description: "Depends", AcceptanceCriteria: []string{"done"}, DependsOn: []string{"one"}, Status: pipeline.TaskPlanned})
	job.Plan.Tasks[0].Status = pipeline.TaskApproved
	if taskDependenciesIntegrated(job, &job.Plan.Tasks[1]) {
		t.Fatal("dependency should not be ready before integration")
	}
	job.Plan.Tasks[0].Status = pipeline.TaskIntegrated
	if !taskDependenciesIntegrated(job, &job.Plan.Tasks[1]) {
		t.Fatal("dependency should be ready after integration")
	}
}

func TestPublishApprovedJobCreatesAndMergesAfterSuccessfulChecks(t *testing.T) {
	store, controller, job := approvedJob(t)
	github := githubpkg.NewFakeClient()
	github.CreatePRResult = &githubpkg.PR{Number: 9, HTMLURL: "https://example.test/pr/9"}
	github.GetPRResult = &githubpkg.PR{Number: 9, State: "open", Head: githubpkg.PRHead{SHA: job.IntegrationSHA}}
	github.GetPRChecksResult = &githubpkg.PRChecks{TotalCount: 1, CheckRuns: []githubpkg.CheckRun{{Name: "test", Status: "completed", Conclusion: "success"}}}

	updated, err := publishApprovedJob(context.Background(), job.ID, Config{Store: store, Controller: controller, GitHub: github})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != pipeline.JobPublished || updated.MergeState != pipeline.MergeMerged || updated.PullRequestNumber != 9 {
		t.Fatalf("unexpected published job: %+v", updated)
	}
	if got := fakeMethods(github); fmt.Sprint(got) != "[CreatePR GetPR GetPRChecks MergePR]" {
		t.Fatalf("unexpected GitHub calls: %v", got)
	}
	create := github.Calls[0].Args[0].(githubpkg.CreatePROptions)
	if create.Title != "Test change" || create.Body != "## Summary\nTest" {
		t.Fatalf("pull request presentation = %#v", create)
	}
	if _, err := publishApprovedJob(context.Background(), job.ID, Config{Store: store, Controller: controller, GitHub: github}); err != nil {
		t.Fatal(err)
	}
	if got := fakeMethods(github); fmt.Sprint(got) != "[CreatePR GetPR GetPRChecks MergePR]" {
		t.Fatalf("published retry made GitHub calls: %v", got)
	}
}

func TestPublishApprovedJobRejectsMismatchedHeadAndMissingChecks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pr     *githubpkg.PR
		checks *githubpkg.PRChecks
		want   string
	}{
		{name: "head", pr: &githubpkg.PR{Number: 9, State: "open", Head: githubpkg.PRHead{SHA: "different"}}, checks: &githubpkg.PRChecks{}, want: "explicit holistic re-review"},
		{name: "checks", pr: &githubpkg.PR{Number: 9, State: "open", Head: githubpkg.PRHead{SHA: "integration"}}, checks: &githubpkg.PRChecks{}, want: "no checks reported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, controller, job := approvedJob(t)
			github := githubpkg.NewFakeClient()
			github.CreatePRResult = &githubpkg.PR{Number: 9, HTMLURL: "https://example.test/pr/9"}
			github.GetPRResult = tc.pr
			github.GetPRChecksResult = tc.checks

			_, err := publishApprovedJob(context.Background(), job.ID, Config{Store: store, Controller: controller, GitHub: github})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q error, got %v", tc.want, err)
			}
			for _, method := range fakeMethods(github) {
				if method == "MergePR" {
					t.Fatal("merge was called despite failed gate")
				}
			}
		})
	}
}

func TestPublishApprovedJobUpdatesRecordedPRAndRecoversMergedPR(t *testing.T) {
	store, controller, job := approvedJob(t)
	if _, err := controller.RecordPullRequest(job.ID, 9, "https://example.test/pr/9"); err != nil {
		t.Fatal(err)
	}
	github := githubpkg.NewFakeClient()
	github.GetPRResult = &githubpkg.PR{Number: 9, State: "closed", Merged: true}

	updated, err := publishApprovedJob(context.Background(), job.ID, Config{Store: store, Controller: controller, GitHub: github})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != pipeline.JobPublished || updated.MergeState != pipeline.MergeMerged {
		t.Fatalf("merged PR was not recovered: %+v", updated)
	}
	if got := fakeMethods(github); fmt.Sprint(got) != "[UpdatePR GetPR]" {
		t.Fatalf("unexpected GitHub calls: %v", got)
	}
}

func TestPublishApprovedJobRequiresGitHub(t *testing.T) {
	store, controller, job := approvedJob(t)
	_, err := publishApprovedJob(context.Background(), job.ID, Config{Store: store, Controller: controller})
	if err == nil || !strings.Contains(err.Error(), "GitHub integration is not configured") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSuccessfulChecksAllowsEmptyChecksOnlyWhenConfigured(t *testing.T) {
	if err := successfulChecks(&githubpkg.PRChecks{}, false); err == nil {
		t.Fatal("expected empty checks to be rejected by default")
	}
	if err := successfulChecks(&githubpkg.PRChecks{}, true); err != nil {
		t.Fatalf("configured empty checks should be allowed: %v", err)
	}
}

func approvedJob(t *testing.T) (*pipeline.Store, *pipeline.Controller, *pipeline.Job) {
	t.Helper()
	store, err := pipeline.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	controller := pipeline.NewController(store)
	job, err := store.Create("repo", "directive", "main", "", "", "writer", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartPlanning(job.ID); err != nil {
		t.Fatal(err)
	}
	job, err = store.SubmitPlan(job.ID, pipeline.Plan{Summary: "plan", Tasks: []pipeline.Task{{Key: "one", Title: "one", Description: "one", AcceptanceCriteria: []string{"works"}}}, IntegrationOrder: []string{"one"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.StartTaskWork(job.ID, "one", "branch", "/work", "writer"); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.RecordWriterCompletion(job.ID, "one", "writer", "commit", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.RecordReview(job.ID, "one", "review", pipeline.ReviewApproved); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.StartIntegration(job.ID, "one"); err != nil {
		t.Fatal(err)
	}
	job, err = controller.RecordIntegration(job.ID, "one", "integration")
	if err != nil {
		t.Fatal(err)
	}
	job, err = controller.RecordHolisticReview(job.ID, "holistic", "integration", pipeline.ReviewApproved, pipeline.PullRequestContent{Title: "Test change", Body: "## Summary\nTest"})
	if err != nil {
		t.Fatal(err)
	}
	return store, controller, job
}

func fakeMethods(client *githubpkg.FakeClient) []string {
	methods := make([]string, len(client.Calls))
	for i, call := range client.Calls {
		methods[i] = call.Method
	}
	return methods
}
