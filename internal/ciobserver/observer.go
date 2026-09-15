// Package ciobserver reduces GitHub check runs into durable OpenDev CI state.
package ciobserver

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/iamangus/code-mcp/internal/github"
	"github.com/iamangus/code-mcp/internal/pipeline"
)

// Evaluate considers only named required checks. Unrelated repository checks
// must not delay or fail an OpenDev job.
func Evaluate(required []string, checks *github.PRChecks) (pipeline.CIState, []pipeline.CheckResult, string) {
	byName := make(map[string]github.CheckRun)
	if checks != nil {
		for _, check := range checks.CheckRuns {
			byName[check.Name] = check
		}
	}
	results := make([]pipeline.CheckResult, 0, len(required))
	failed := make([]string, 0)
	for _, name := range required {
		check, found := byName[name]
		if !found || !strings.EqualFold(check.Status, "completed") {
			return pipeline.CIPending, results, ""
		}
		result := pipeline.CheckResult{Name: name, Status: check.Status, Conclusion: check.Conclusion, DetailsURL: check.DetailsURL, Summary: check.Output.Summary}
		results = append(results, result)
		if !strings.EqualFold(check.Conclusion, "success") {
			failed = append(failed, name+"\n"+check.Conclusion+"\n"+check.Output.Summary)
		}
	}
	if len(failed) == 0 {
		return pipeline.CIPassed, results, ""
	}
	sort.Strings(failed)
	digest := sha256.Sum256([]byte(strings.Join(failed, "\n---\n")))
	return pipeline.CIFailed, results, hex.EncodeToString(digest[:16])
}
