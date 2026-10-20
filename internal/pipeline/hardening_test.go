package pipeline

import "testing"

func TestNoChangesReleasesAlreadyApprovedSuccessor(t *testing.T) {
	store, c, job := plannedController(t, t.TempDir(), []Task{task("a"), task("b")}, []string{"a", "b"})
	c.StartTaskWork(job.ID, "b", "b", "/b", "wb")
	c.RecordWriterCompletion(job.ID, "b", "wb", "sha-b", nil)
	if _, err := c.RecordReview(job.ID, "b", "rb", ReviewApproved); err != nil {
		t.Fatal(err)
	}
	c.StartTaskWork(job.ID, "a", "a", "/a", "wa")
	if _, err := c.RecordNoChanges(job.ID, "a", "wa", "already done", nil); err != nil {
		t.Fatal(err)
	}
	current, _ := store.Get(job.ID)
	if current.Plan.Tasks[1].Status != TaskIntegrationEligible {
		t.Fatalf("successor stuck: %s", current.Plan.Tasks[1].Status)
	}
}

func TestPlannerCannotPopulateExecutionState(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	job, _ := store.Create("repo", "directive", "main", "", "p", "w", "r")
	store.StartPlanning(job.ID)
	input := task("a")
	input.WriterAttempts, input.WriterRunID, input.BaseSHA, input.WorktreePath = 10, "fake", "fake", "/outside"
	job, err := store.SubmitPlan(job.ID, Plan{Summary: "plan", Tasks: []Task{input}, IntegrationOrder: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	got := job.Plan.Tasks[0]
	if got.WriterAttempts != 0 || got.WriterRunID != "" || got.BaseSHA != "" || got.WorktreePath != "" {
		t.Fatalf("planner execution state retained: %+v", got)
	}
}

func TestRevisionRewritesDependenciesAndPreservesOrder(t *testing.T) {
	store, _, job := plannedController(t, t.TempDir(), []Task{task("old")}, []string{"old"})
	store.updateJob(job.ID, func(job *Job) error { job.Status = JobCIReplanning; job.CIReplanRounds = 1; return nil })
	a, b := task("a"), task("b")
	b.DependsOn = []string{"a"}
	updated, err := store.RevisePlan(job.ID, Plan{Summary: "revision", Tasks: []Task{b, a}, IntegrationOrder: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Plan.Tasks[1].DependsOn[0] != "rev1-a" || updated.Plan.IntegrationOrder[1] != "rev1-a" {
		t.Fatalf("bad revision: %+v", updated.Plan)
	}
	if _, err := store.RevisePlan(job.ID, Plan{Summary: "revision", Tasks: []Task{b, a}, IntegrationOrder: []string{"a", "b"}}); err != nil {
		t.Fatalf("replay: %v", err)
	}
}

func TestRejectedMutationDoesNotLeakIntoMemory(t *testing.T) {
	store, c, job := plannedController(t, t.TempDir(), []Task{task("a")}, []string{"a"})
	c.StartTaskWork(job.ID, "a", "a", "/a", "w")
	c.RecordWriterCompletion(job.ID, "a", "w", "candidate", nil)
	c.RecordReview(job.ID, "a", "r", ReviewApproved)
	c.StartIntegration(job.ID, "a")
	c.RecordIntegration(job.ID, "a", "integrated")
	if _, err := c.RecordHolisticReview(job.ID, "h", "integrated", ReviewApproved, PullRequestContent{}); err == nil {
		t.Fatal("missing PR content accepted")
	}
	current, _ := store.Get(job.ID)
	if current.HolisticReviewVerdict != ReviewPending || current.HolisticReviewRunID != "" {
		t.Fatalf("rejected transition mutated state: %+v", current)
	}
}
