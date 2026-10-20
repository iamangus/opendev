package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iamangus/code-mcp/internal/dispatcher"
	"github.com/iamangus/code-mcp/internal/pipeline"
)

func TestExistingBearerAuthRequiresSchemeAndNonemptyToken(t *testing.T) {
	for _, header := range []string{"", "secret", "Basic secret", "Bearer wrong"} {
		request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		request.Header.Set("Authorization", header)
		response := httptest.NewRecorder()
		requireMCPToken("secret", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("unauthorized request reached handler") })).ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("accepted %q", header)
		}
	}
	response := httptest.NewRecorder()
	requireMCPToken("", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("empty token accepted") })).ServeHTTP(response, httptest.NewRequest("GET", "/mcp", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatal("empty token accepted")
	}
}

func TestSlashBranchCallbackIsOneEncodedRouteSegment(t *testing.T) {
	d := &lazyDispatcher{mcpURL: "http://opendev.test/mcp"}
	endpoint := d.worktreeMCPURL("repo", "feature/fix", "read")
	if !strings.Contains(endpoint, "feature%2Ffix") {
		t.Fatal(endpoint)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/{repo}/{branch}/{profile}/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("branch") != "feature/fix" {
			t.Error(r.PathValue("branch"))
		}
		w.WriteHeader(204)
	})
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest("GET", endpoint, nil))
	if response.Code != 204 {
		t.Fatalf("route unavailable: %d", response.Code)
	}
}

func TestRecoveredWriterUsesTaskBranchNotSyntheticIntegrationBranch(t *testing.T) {
	store, _ := pipeline.NewStore(t.TempDir())
	job, _ := store.Create("repo", "directive", "main", "", "p", "w", "r")
	store.StartPlanning(job.ID)
	job, _ = store.SubmitPlan(job.ID, pipeline.Plan{Summary: "plan", Tasks: []pipeline.Task{{Key: "a", Title: "a", Description: "a", AcceptanceCriteria: []string{"a"}}}, IntegrationOrder: []string{"a"}})
	d := &lazyDispatcher{jobStore: store, mcpURL: "http://opendev.test/mcp", mcpToken: "secret"}
	servers, err := d.serversForRun(context.Background(), dispatcher.DispatchRun{JobID: job.ID, Role: dispatcher.RoleWriter, TaskKey: "a", Attempt: 1})
	if err != nil || len(servers) != 3 {
		t.Fatalf("attachments: %+v %v", servers, err)
	}
	if !strings.Contains(servers[2].URL, "opendev-task-") {
		t.Fatal(servers[2].URL)
	}
}
