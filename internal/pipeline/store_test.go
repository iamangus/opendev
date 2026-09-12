package pipeline

import (
	"errors"
	"testing"
)

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

func TestCreatePersistsHolisticAgentID(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.Create("repo", "Fix", "main", "orchestrator", "planner", "writer", "reviewer", "holistic-reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if job.HolisticAgentID != "holistic-reviewer" || job.HolisticAgentID == job.ReviewerAgentID {
		t.Fatalf("unexpected holistic agent ID: %+v", job)
	}
	reopened, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := reopened.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.HolisticAgentID != "holistic-reviewer" {
		t.Fatalf("holistic agent ID was not persisted: %+v", persisted)
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
