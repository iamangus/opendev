package agentfoundry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientRunGetAndCancel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret-key" {
			t.Fatalf("authorization = %q", got)
		}
		switch r.URL.Path {
		case "/api/v1/agents/planner/run":
			if r.Method != http.MethodPost {
				t.Fatalf("method = %s", r.Method)
			}
			var request RunOptions
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.TaskID != "task-1" || len(request.MCPServers) != 1 || request.MCPServers[0].Headers["X-Token"] != "mcp-secret" {
				t.Fatalf("unexpected run request: %+v", request)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"run_id":"run-1"}`))
		case "/api/v1/runs/run-1":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"run-1","status":"completed"}`))
		case "/api/v1/runs":
			if r.URL.Query().Get("task_id") != "task-1" {
				t.Fatalf("task_id = %q", r.URL.Query().Get("task_id"))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"run-1","status":"running"}`))
		case "/api/v1/runs/run-1/cancel":
			if r.Method != http.MethodPost {
				t.Fatalf("method = %s", r.Method)
			}
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "secret-key")
	if err != nil {
		t.Fatal(err)
	}
	runID, err := client.StartRun(context.Background(), "planner", RunOptions{Message: "plan", TaskID: "task-1", MCPServers: []MCPServer{{Name: "jobs", URL: "http://mcp.test", Transport: "streamable-http", Headers: map[string]string{"X-Token": "mcp-secret"}}}})
	if err != nil || runID != "run-1" {
		t.Fatalf("StartRun = %q, %v", runID, err)
	}
	run, err := client.GetRun(context.Background(), runID)
	if err != nil || run.Status != "completed" {
		t.Fatalf("GetRun = %+v, %v", run, err)
	}
	byTask, err := client.GetRunByTaskID(context.Background(), "task-1")
	if err != nil || byTask.ID != runID {
		t.Fatalf("GetRunByTaskID = %+v, %v", byTask, err)
	}
	if err := client.CancelRun(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
}

func TestClientDoesNotExposeErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Authorization: Bearer server-secret", http.StatusInternalServerError)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "secret-key")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.StartRun(context.Background(), "planner", RunOptions{Message: "plan"})
	if err == nil || contains(err.Error(), "server-secret") || contains(err.Error(), "secret-key") {
		t.Fatalf("unsafe error: %v", err)
	}
}

func contains(s, needle string) bool {
	for i := 0; i+len(needle) <= len(s); i++ {
		if s[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
