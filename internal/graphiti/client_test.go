package graphiti

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientPostsMessagesAndSearchesConfiguredGroup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		switch r.URL.Path {
		case "/messages":
			if r.Method != http.MethodPost || string(payload["group_id"]) != `"opendev-repositories"` {
				t.Fatalf("unexpected message request: %s %s", r.Method, payload["group_id"])
			}
			var messages []Message
			if err := json.Unmarshal(payload["messages"], &messages); err != nil || len(messages) != 1 || messages[0].RoleType != "user" || messages[0].Name != "test-indexer" || messages[0].SourceDescription != "unit test" || messages[0].Timestamp != "2026-09-12T00:00:00Z" {
				t.Fatalf("unexpected messages: %s, %v", payload["messages"], err)
			}
			w.WriteHeader(http.StatusNoContent)
		case "/search":
			if got := string(payload["group_ids"]); got != `["opendev-repositories"]` {
				t.Fatalf("group_ids = %s", got)
			}
			_, _ = w.Write([]byte(`[{"fact":"repository uses Go"}]`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "opendev-repositories")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.AddMessages(context.Background(), []Message{{Content: "repository indexed", RoleType: "user", Name: "test-indexer", SourceDescription: "unit test", Timestamp: "2026-09-12T00:00:00Z"}}); err != nil {
		t.Fatal(err)
	}
	got, err := client.Search(context.Background(), "what language?", SearchOptions{MaxResults: 3})
	if err != nil || string(got) != `[{"fact":"repository uses Go"}]` {
		t.Fatalf("Search = %s, %v", got, err)
	}
}

func TestClientReportsNonSuccessAndHonorsCanceledContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "opendev-repositories")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Search(context.Background(), "status", SearchOptions{}); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("Search error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Search(ctx, "status", SearchOptions{}); err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("Search canceled error = %v", err)
	}
}
