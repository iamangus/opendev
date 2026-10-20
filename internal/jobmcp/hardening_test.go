package jobmcp

import (
	"context"
	"strings"
	"testing"

	"github.com/iamangus/code-mcp/internal/dispatcher"
	githubpkg "github.com/iamangus/code-mcp/internal/github"
	"github.com/iamangus/code-mcp/internal/pipeline"
)

func TestSupersededTerminalFailureCannotBlockNewWriter(t *testing.T) {
	store, c, _ := approvedJob(t)
	// Use a fresh job so its task is actively owned by the current writer.
	job, _ := store.Create("repo", "directive", "main", "", "p", "w", "r")
	store.StartPlanning(job.ID)
	store.SubmitPlan(job.ID, pipeline.Plan{Summary: "plan", Tasks: []pipeline.Task{{Key: "a", Title: "a", Description: "a", AcceptanceCriteria: []string{"a"}}}, IntegrationOrder: []string{"a"}})
	c.StartTaskWork(job.ID, "a", "a", "/a", "new-writer")
	if err := OutcomeHandler(Config{Store: store, Controller: c})(context.Background(), dispatcher.DispatchRun{JobID: job.ID, TaskKey: "a", Role: dispatcher.RoleWriter, RunID: "old-writer", Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	current, _ := store.Get(job.ID)
	if current.Plan.Tasks[0].Status != pipeline.TaskWorking {
		t.Fatal("stale failure blocked current work")
	}
}

func TestManualMergeRequiresExplicitApproval(t *testing.T) {
	store, c, job := approvedJob(t)
	if _, err := store.SetMergePolicy(job.ID, "manual"); err != nil {
		t.Fatal(err)
	}
	gh := githubpkg.NewFakeClient()
	gh.CreatePRResult = &githubpkg.PR{Number: 9, HTMLURL: "https://example.test/9"}
	gh.GetPRResult = &githubpkg.PR{Number: 9, State: "open", Head: githubpkg.PRHead{SHA: job.IntegrationSHA}}
	gh.GetPRChecksResult = &githubpkg.PRChecks{TotalCount: 1, CheckRuns: []githubpkg.CheckRun{{Name: "test", Status: "completed", Conclusion: "success"}}}
	config := Config{Store: store, Controller: c, GitHub: gh}
	held, err := publishApprovedJob(context.Background(), job.ID, config)
	if err != nil {
		t.Fatal(err)
	}
	if held.Status != pipeline.JobReadyToPublish {
		t.Fatalf("manual job published: %s", held.Status)
	}
	for _, call := range gh.Calls {
		if call.Method == "MergePR" {
			t.Fatal("manual gate bypassed")
		}
	}
	config.ForceMerge = true
	published, err := publishApprovedJob(context.Background(), job.ID, config)
	if err != nil || published.Status != pipeline.JobPublished {
		t.Fatalf("explicit approval: %+v %v", published, err)
	}
}

func TestPausedOrCancelledApprovedJobsCannotPublish(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		store, c, job := approvedJob(t)
		c.RecordPullRequest(job.ID, 9, "https://example.test/9")
		if cancel {
			store.Cancel(job.ID, "operator")
		} else {
			store.Pause(job.ID)
		}
		gh := githubpkg.NewFakeClient()
		if _, err := publishApprovedJob(context.Background(), job.ID, Config{Store: store, Controller: c, GitHub: gh, ForceMerge: true}); err == nil {
			t.Fatal("terminal/pause gate bypassed")
		}
		if len(gh.Calls) != 0 {
			t.Fatal("paused/cancelled job touched GitHub")
		}
	}
}

func TestHolisticFeedbackCreatesAndDispatchesFocusedRemediation(t *testing.T) {
	store, c, job := approvedJob(t)
	c.RecordPullRequest(job.ID, 9, "https://example.test/9")
	job, err := c.StartHolisticRereview(job.ID, job.IntegrationSHA)
	if err != nil {
		t.Fatal(err)
	}
	c.StartHolisticReviewer(job.ID, "fresh-holistic", job.IntegrationSHA)
	dispatch := &fakeDispatcher{}
	config := Config{Store: store, Controller: c, Dispatcher: dispatch, Worktrees: &fakeWorktrees{}, Registrar: &fakeRegistrar{}}
	if err := OutcomeHandler(config)(context.Background(), dispatcher.DispatchRun{JobID: job.ID, Role: dispatcher.RoleHolistic, RunID: "fresh-holistic", Status: "completed", Response: `{"verdict":"changes_requested","summary":"Fix accessibility","findings":["Add button labels"]}`}); err != nil {
		t.Fatal(err)
	}
	current, _ := store.Get(job.ID)
	task := current.Plan.Tasks[len(current.Plan.Tasks)-1]
	if task.Status != pipeline.TaskWorking || !strings.Contains(task.Description, "Add button labels") || len(dispatch.writerTasks) != 1 {
		t.Fatalf("remediation not dispatched: %+v", task)
	}
}
