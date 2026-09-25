package pipeline

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestFailedPipelineWriteRollsBackInMemoryTransitions(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.Create("repo", "directive", "main", "", "planner", "writer", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	originalPath := store.path
	store.path = filepath.Join(dir, "nonexistent", "jobs.json")
	if _, err := store.StartPlanning(job.ID); err == nil {
		t.Fatal("expected failed state write")
	}
	current, err := store.Get(job.ID)
	if err != nil || current.Status != JobCreated {
		t.Fatalf("failed transition remained in memory: %+v %v", current, err)
	}
	store.path = originalPath
	if _, err := store.StartPlanning(job.ID); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	current, err = reloaded.Get(job.ID)
	if err != nil || current.Status != JobPlanning {
		t.Fatalf("retry was not persisted: %+v %v", current, err)
	}
}

func TestPlanPersistsAndRejectsInvalidDependencies(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.Create("repo", "Fix mobile layout", "main", "orchestrator", "planner", "writer", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartPlanning(job.ID); err != nil {
		t.Fatal(err)
	}
	plan := Plan{
		Summary:          "Mobile fixes",
		Tasks:            []Task{{Key: "nav", Title: "Navigation", Description: "Make navigation responsive", AcceptanceCriteria: []string{"works at 320px"}}},
		IntegrationOrder: []string{"nav"},
		ReferenceRepositories: []ReferenceRepository{{
			Repository: "github.com/example/design-system",
			Branch:     "main",
			Purpose:    "Follow shared component conventions",
		}},
	}
	if _, err := store.SubmitPlan(job.ID, plan); err != nil {
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
	if persisted.Status != JobPlanned || persisted.Plan.Tasks[0].Status != TaskPlanned || len(persisted.Plan.ReferenceRepositories) != 1 || persisted.Plan.ReferenceRepositories[0] != plan.ReferenceRepositories[0] {
		t.Fatalf("unexpected persisted job: %+v", persisted)
	}
	persisted.Plan.ReferenceRepositories[0].Purpose = "mutated"
	stored, err := reopened.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Plan.ReferenceRepositories[0].Purpose != plan.ReferenceRepositories[0].Purpose {
		t.Fatalf("reference repositories were not cloned: %+v", stored.Plan.ReferenceRepositories)
	}
}

func TestSubmitPlanRejectsInvalidReferenceRepositories(t *testing.T) {
	basePlan := func(references []ReferenceRepository) Plan {
		return Plan{
			Summary:               "Plan",
			Tasks:                 []Task{{Key: "a", Title: "A", Description: "A", AcceptanceCriteria: []string{"A"}}},
			IntegrationOrder:      []string{"a"},
			ReferenceRepositories: references,
		}
	}
	for _, test := range []struct {
		name       string
		references []ReferenceRepository
	}{
		{name: "empty repository", references: []ReferenceRepository{{Purpose: "context"}}},
		{name: "empty purpose", references: []ReferenceRepository{{Repository: "github.com/example/reference"}}},
		{name: "duplicate repository", references: []ReferenceRepository{{Repository: "github.com/example/reference", Purpose: "one"}, {Repository: "github.com/example/reference", Branch: "next", Purpose: "two"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			job, err := store.Create("repo", "Fix", "main", "o", "p", "w", "r")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.StartPlanning(job.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := store.SubmitPlan(job.ID, basePlan(test.references)); !errors.Is(err, ErrInvalidPlan) {
				t.Fatalf("expected invalid plan, got %v", err)
			}
		})
	}
}

func TestSubmitPlanRejectsUnknownDependency(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.Create("repo", "Fix", "main", "o", "p", "w", "r")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartPlanning(job.ID); err != nil {
		t.Fatal(err)
	}
	_, err = store.SubmitPlan(job.ID, Plan{Summary: "Plan", Tasks: []Task{{Key: "a", Title: "A", Description: "A", AcceptanceCriteria: []string{"A"}, DependsOn: []string{"missing"}}}, IntegrationOrder: []string{"a"}})
	if !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("expected invalid plan, got %v", err)
	}
}

func TestSubmitPlanRejectsDependencyAfterDependent(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.Create("repo", "Fix", "main", "o", "p", "w", "r")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartPlanning(job.ID); err != nil {
		t.Fatal(err)
	}
	_, err = store.SubmitPlan(job.ID, Plan{Summary: "Plan", Tasks: []Task{
		{Key: "a", Title: "A", Description: "A", AcceptanceCriteria: []string{"A"}, DependsOn: []string{"b"}},
		{Key: "b", Title: "B", Description: "B", AcceptanceCriteria: []string{"B"}},
	}, IntegrationOrder: []string{"a", "b"}})
	if !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("expected invalid plan, got %v", err)
	}
}

func TestBeginCIReplanCapFailsJob(t *testing.T) {
	store, controller, job := plannedController(t, t.TempDir(), []Task{task("one")}, []string{"one"})
	if _, err := controller.StartTaskWork(job.ID, "one", "branch", "/work", "writer-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.RecordWriterCompletion(job.ID, "one", "writer-1", "commit-1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.RecordReview(job.ID, "one", "reviewer-1", ReviewApproved); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.StartIntegration(job.ID, "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.RecordIntegration(job.ID, "one", "integration-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.StartCI(job.ID, "head-1", []string{"ci"}); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.RecordCI(job.ID, "head-1", CIFailed, nil, "fp-x"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxCIReplanRounds; i++ {
		updated, err := controller.BeginCIReplan(job.ID, "fp-x")
		if err != nil {
			t.Fatal(err)
		}
		if updated.Status != JobCIReplanning {
			t.Fatalf("round %d should be replanning: %s", i+1, updated.Status)
		}
		// White-box: emulate the revision landing and a new identical failure.
		store.mu.Lock()
		store.jobs[job.ID].Status = JobAwaitingCI
		store.mu.Unlock()
		if _, err := controller.RecordCI(job.ID, "head-1", CIFailed, nil, "fp-x"); err != nil {
			t.Fatal(err)
		}
	}
	updated, err := controller.BeginCIReplan(job.ID, "fp-x")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != JobFailed {
		t.Fatalf("re-plan cap should fail the job: %s", updated.Status)
	}
	if updated.CIReplanRounds != maxCIReplanRounds {
		t.Fatalf("unexpected replan rounds: %d", updated.CIReplanRounds)
	}
}
