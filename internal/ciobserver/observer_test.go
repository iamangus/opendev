package ciobserver

import (
	"testing"

	"github.com/iamangus/code-mcp/internal/github"
	"github.com/iamangus/code-mcp/internal/pipeline"
)

func TestEvaluateWaitsForEveryRequiredCheck(t *testing.T) {
	state, _, fingerprint := Evaluate([]string{"OpenDev CI", "lint"}, &github.PRChecks{CheckRuns: []github.CheckRun{{Name: "OpenDev CI", Status: "completed", Conclusion: "success"}}})
	if state != pipeline.CIPending || fingerprint != "" {
		t.Fatalf("state = %q, fingerprint = %q", state, fingerprint)
	}
}

func TestEvaluateFailsOnlyRequiredChecks(t *testing.T) {
	checks := &github.PRChecks{CheckRuns: []github.CheckRun{
		{Name: "OpenDev CI", Status: "completed", Conclusion: "success"},
		{Name: "lint", Status: "completed", Conclusion: "failure"},
		{Name: "unrelated", Status: "completed", Conclusion: "failure"},
	}}
	state, results, fingerprint := Evaluate([]string{"OpenDev CI", "lint"}, checks)
	if state != pipeline.CIFailed || len(results) != 2 || fingerprint == "" {
		t.Fatalf("state = %q, results = %#v, fingerprint = %q", state, results, fingerprint)
	}
}
