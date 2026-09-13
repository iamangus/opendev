package jobmcp

import (
	"testing"

	"github.com/iamangus/code-mcp/internal/pipeline"
)

func TestRepeatedReviewFeedback(t *testing.T) {
	task := &pipeline.Task{ReviewHistory: []pipeline.ReviewReport{
		{Verdict: pipeline.ReviewChangesRequested, Summary: "fix cleanup", Findings: []string{"listener remains"}},
		{Verdict: pipeline.ReviewChangesRequested, Summary: "fix cleanup", Findings: []string{"listener remains"}},
		{Verdict: pipeline.ReviewChangesRequested, Summary: "fix cleanup", Findings: []string{"listener remains"}},
	}}
	if got := repeatedReviewFeedback(task); got != 3 {
		t.Fatalf("repeat count = %d, want 3", got)
	}
	task.ReviewHistory[1].Findings[0] = "different finding"
	if got := repeatedReviewFeedback(task); got != 1 {
		t.Fatalf("repeat count after changed feedback = %d, want 1", got)
	}
}
