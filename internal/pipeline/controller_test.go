package pipeline

import (
	"errors"
	"testing"
)

func plannedController(t *testing.T, dir string, tasks []Task, order []string) (*Store, *Controller, *Job) {
	t.Helper()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.Create("repo", "Implement pipeline", "main", "orchestrator", "planner", "writer", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartPlanning(job.ID); err != nil {
		t.Fatal(err)
	}
	job, err = store.SubmitPlan(job.ID, Plan{Summary: "Plan", Tasks: tasks, IntegrationOrder: order})
	if err != nil {
		t.Fatal(err)
	}
	return store, NewController(store), job
}

func task(key string, dependsOn ...string) Task {
	return Task{Key: key, Title: key, Description: key, AcceptanceCriteria: []string{"works"}, DependsOn: dependsOn}
}

func TestControllerTaskTransitionsAndRetries(t *testing.T) {
	_, controller, job := plannedController(t, t.TempDir(), []Task{task("one")}, []string{"one"})
	job, err := controller.StartTaskWork(job.ID, "one", "", "/work/one", "writer-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := job.Plan.Tasks[0]; got.Status != TaskWorking || got.WriterAttempts != 1 || got.Branch != job.IntegrationBranch+"/one" || got.WorktreePath == "" {
		t.Fatalf("unexpected working task: %+v", got)
	}
	if _, err := controller.RecordWriterCompletion(job.ID, "one", "wrong-run", "commit-1", nil); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("expected run ID rejection, got %v", err)
	}
	job, err = controller.RecordWriterCompletion(job.ID, "one", "writer-1", "commit-1", []ValidationEvidence{{Command: "go test", Result: "passed", RunID: "test-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != JobReviewing || job.Plan.Tasks[0].Status != TaskReviewing || job.Plan.Tasks[0].CommitSHA != "commit-1" {
		t.Fatalf("writer completion not recorded: %+v", job)
	}
	job, err = controller.RecordReview(job.ID, "one", "review-1", ReviewChangesRequested)
	if err != nil {
		t.Fatal(err)
	}
	if job.Plan.Tasks[0].Status != TaskChangesRequested || job.Plan.Tasks[0].ReviewerAttempts != 1 {
		t.Fatalf("changes request not recorded: %+v", job.Plan.Tasks[0])
	}
	job, err = controller.StartTaskWork(job.ID, "one", "opendev/job/one", "/work/one", "writer-2")
	if err != nil {
		t.Fatal(err)
	}
	if job.Plan.Tasks[0].WriterAttempts != 2 || job.Plan.Tasks[0].WriterRunID != "writer-2" || job.Plan.Tasks[0].ReviewerRunID != "" || job.Plan.Tasks[0].ReviewVerdict != ReviewPending {
		t.Fatalf("retry not recorded: %+v", job.Plan.Tasks[0])
	}
	job, err = controller.RecordWriterCompletion(job.ID, "one", "writer-2", "commit-2", nil)
	if err != nil {
		t.Fatal(err)
	}
	job, err = controller.StartReviewer(job.ID, "one", "review-2")
	if err != nil {
		t.Fatalf("retry review should replace its prior reviewer: %v", err)
	}
	if got := job.Plan.Tasks[0]; got.ReviewerRunID != "review-2" || got.ReviewerAttempts != 2 {
		t.Fatalf("retry reviewer not recorded: %+v", got)
	}
}

func TestControllerDependencyGateAndIntegrationOrder(t *testing.T) {
	_, controller, job := plannedController(t, t.TempDir(), []Task{task("base"), task("dependent", "base")}, []string{"base", "dependent"})
	for _, key := range []string{"base", "dependent"} {
		if _, err := controller.StartTaskWork(job.ID, key, "branch-"+key, "/work/"+key, "writer-"+key); err != nil {
			t.Fatal(err)
		}
		if _, err := controller.RecordWriterCompletion(job.ID, key, "writer-"+key, "commit-"+key, nil); err != nil {
			t.Fatal(err)
		}
	}
	job, err := controller.RecordReview(job.ID, "dependent", "review-dependent", ReviewApproved)
	if err != nil {
		t.Fatal(err)
	}
	if job.Plan.Tasks[1].Status != TaskApproved {
		t.Fatalf("dependent should await its dependency: %s", job.Plan.Tasks[1].Status)
	}
	if _, err := controller.StartIntegration(job.ID, "dependent"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("expected eligibility gate, got %v", err)
	}
	job, err = controller.RecordReview(job.ID, "base", "review-base", ReviewApproved)
	if err != nil {
		t.Fatal(err)
	}
	if job.Plan.Tasks[0].Status != TaskIntegrationEligible {
		t.Fatalf("base should be eligible: %s", job.Plan.Tasks[0].Status)
	}
	if _, err := controller.StartIntegration(job.ID, "base"); err != nil {
		t.Fatal(err)
	}
	job, err = controller.RecordIntegration(job.ID, "base", "integration-base")
	if err != nil {
		t.Fatal(err)
	}
	if job.Plan.Tasks[1].Status != TaskIntegrationEligible {
		t.Fatalf("dependent was not made eligible: %s", job.Plan.Tasks[1].Status)
	}
}

func TestControllerRecoversStaleReviewerOwnershipAfterRevision(t *testing.T) {
	store, controller, job := plannedController(t, t.TempDir(), []Task{task("one")}, []string{"one"})
	if _, err := controller.StartTaskWork(job.ID, "one", "branch", "/work", "writer-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.RecordWriterCompletion(job.ID, "one", "writer-1", "commit-1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.RecordReview(job.ID, "one", "reviewer-1", ReviewChangesRequested); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.StartTaskWork(job.ID, "one", "branch", "/work", "writer-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.RecordWriterCompletion(job.ID, "one", "writer-2", "commit-2", nil); err != nil {
		t.Fatal(err)
	}
	// Simulate the stale ownership written by the pre-fix controller.
	if _, err := store.updateTask(job.ID, "one", func(_ *Job, task *Task) error {
		task.ReviewerRunID = "reviewer-1"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	job, err := controller.RecordReview(job.ID, "one", "reviewer-2", ReviewApproved)
	if err != nil {
		t.Fatalf("stale reviewer ownership should recover: %v", err)
	}
	if got := job.Plan.Tasks[0]; got.ReviewerRunID != "reviewer-2" || got.ReviewerAttempts != 2 || got.Status != TaskIntegrationEligible {
		t.Fatalf("stale reviewer recovery not recorded: %+v", got)
	}
}

func TestControllerRecordsBlockedReview(t *testing.T) {
	_, controller, job := plannedController(t, t.TempDir(), []Task{task("one")}, []string{"one"})
	if _, err := controller.StartTaskWork(job.ID, "one", "branch", "/work", "writer"); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.RecordWriterCompletion(job.ID, "one", "writer", "commit", nil); err != nil {
		t.Fatal(err)
	}
	job, err := controller.RecordReview(job.ID, "one", "review", ReviewBlocked)
	if err != nil {
		t.Fatal(err)
	}
	if got := job.Plan.Tasks[0]; got.Status != TaskBlocked || got.ReviewVerdict != ReviewBlocked || got.ReviewerAttempts != 1 {
		t.Fatalf("blocked review not recorded: %+v", got)
	}
}

func TestControllerHolisticApprovalIsBoundToIntegrationSHA(t *testing.T) {
	dir := t.TempDir()
	_, controller, job := plannedController(t, dir, []Task{task("one")}, []string{"one"})
	if _, err := controller.StartTaskWork(job.ID, "one", "branch", "/work", "writer"); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.RecordWriterCompletion(job.ID, "one", "writer", "commit", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.RecordReview(job.ID, "one", "review", ReviewApproved); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.StartIntegration(job.ID, "one"); err != nil {
		t.Fatal(err)
	}
	job, err := controller.RecordIntegration(job.ID, "one", "integration")
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != JobHolisticReviewing || job.IntegrationSHA != "integration" {
		t.Fatalf("not ready for holistic review: %+v", job)
	}
	content := PullRequestContent{Title: "Test change", Body: "## Summary\nTest"}
	if _, err := controller.RecordHolisticReview(job.ID, "holistic", "other", ReviewApproved, content); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("expected SHA rejection, got %v", err)
	}
	job, err = controller.RecordHolisticReview(job.ID, "holistic", "integration", ReviewApproved, content)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != JobReadyToPublish {
		t.Fatalf("expected publication readiness, got %s", job.Status)
	}
	if _, err := controller.RecordPullRequest(job.ID, 12, "https://example.test/pr/12"); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.RecordPullRequest(job.ID, 12, "https://example.test/pr/12"); err != nil {
		t.Fatalf("idempotent PR record: %v", err)
	}
	job, err = controller.RecordMerge(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != JobPublished || job.MergeState != MergeMerged {
		t.Fatalf("merge not recorded: %+v", job)
	}
	if _, err := controller.RecordMerge(job.ID); err != nil {
		t.Fatalf("idempotent merge record: %v", err)
	}
	reopened, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := reopened.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.IntegrationSHA != "integration" || persisted.HolisticReviewSHA != "integration" || persisted.PullRequestNumber != 12 || persisted.MergeState != MergeMerged {
		t.Fatalf("publication state was not persisted: %+v", persisted)
	}
}

func TestControllerStartsHolisticRereviewForRecordedPR(t *testing.T) {
	_, controller, job := plannedController(t, t.TempDir(), []Task{task("one")}, []string{"one"})
	if _, err := controller.StartTaskWork(job.ID, "one", "branch", "/work", "writer"); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.RecordWriterCompletion(job.ID, "one", "writer", "commit", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.RecordReview(job.ID, "one", "review", ReviewApproved); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.StartIntegration(job.ID, "one"); err != nil {
		t.Fatal(err)
	}
	job, err := controller.RecordIntegration(job.ID, "one", "integration")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.RecordHolisticReview(job.ID, "review-1", "integration", ReviewApproved, PullRequestContent{Title: "Test change", Body: "## Summary\nTest"}); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.RecordPullRequest(job.ID, 12, "https://example.test/pr/12"); err != nil {
		t.Fatal(err)
	}
	job, err = controller.StartHolisticRereview(job.ID, "updated-integration")
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != JobHolisticReviewing || job.IntegrationSHA != "updated-integration" || job.HolisticReviewVerdict != ReviewPending || job.PullRequestNumber != 12 {
		t.Fatalf("unexpected re-review state: %+v", job)
	}
	if _, err := controller.StartHolisticRereview(job.ID, "updated-integration"); err != nil {
		t.Fatalf("idempotent re-review start: %v", err)
	}
}

func TestControllerPersistsExtendedStateAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	_, controller, job := plannedController(t, dir, []Task{task("one")}, []string{"one"})
	if _, err := controller.StartTaskWork(job.ID, "one", "branch", "/work", "writer"); err != nil {
		t.Fatal(err)
	}
	job, err := controller.RecordWriterCompletion(job.ID, "one", "writer", "commit", []ValidationEvidence{{Command: "go test", Result: "passed", RunID: "validation"}})
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := reopened.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := persisted.Plan.Tasks[0]
	if got.Branch != "branch" || got.WorktreePath != "/work" || got.WriterRunID != "writer" || got.CommitSHA != "commit" || len(got.ValidationEvidence) != 1 || persisted.Status != JobReviewing {
		t.Fatalf("extended state was not persisted: %+v", persisted)
	}
}
