package graphmcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/iamangus/code-mcp/internal/graphiti"
	"github.com/iamangus/code-mcp/internal/repositoryindex"
)

type fakeSearcher struct {
	query string
}

func (f *fakeSearcher) Search(_ context.Context, query string, _ graphiti.SearchOptions) (json.RawMessage, error) {
	f.query = query
	return json.RawMessage(`[{"fact":"uses Go"}]`), nil
}

type fakeStateStore struct{ state *repositoryindex.State }

func (f fakeStateStore) Get(string) (*repositoryindex.State, error) { return f.state, nil }

func TestServerExposesOnlyReadOnlyGraphTools(t *testing.T) {
	searcher := &fakeSearcher{}
	ts := httptest.NewServer(New(Config{
		Searcher: searcher,
		State:    fakeStateStore{state: &repositoryindex.State{Repository: "owner/repo", Status: "indexed", IndexedAt: time.Now().UTC()}},
	}))
	defer ts.Close()

	session := initialize(t, ts.URL)
	tools := listTools(t, ts.URL, session)
	want := map[string]bool{"search_repository_graph": true, "get_repository_index_state": true}
	if len(tools) != len(want) {
		t.Fatalf("tools = %v, want %v", tools, want)
	}
	for name := range want {
		if !tools[name] {
			t.Fatalf("missing tool %q", name)
		}
	}
	for name := range tools {
		if strings.Contains(name, "ingest") || strings.Contains(name, "write") || strings.Contains(name, "message") {
			t.Fatalf("unexpected graph mutation tool %q", name)
		}
	}

	raw := post(t, ts.URL, session, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_repository_graph","arguments":{"repository":"owner/repo","query":"language"}}}`)
	if !strings.Contains(string(raw), "uses Go") || searcher.query != "owner/repo: language" {
		t.Fatalf("search response/query = %s / %q", raw, searcher.query)
	}
}

func initialize(t *testing.T, url string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize status = %s", resp.Status)
	}
	return resp.Header.Get("Mcp-Session-Id")
}

func listTools(t *testing.T, url, session string) map[string]bool {
	t.Helper()
	raw := post(t, url, session, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	var envelope struct {
		Result struct {
			Tools []struct{ Name string } `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(sseData(raw), &envelope); err != nil {
		t.Fatalf("parse tools/list: %v; raw: %s", err, raw)
	}
	tools := make(map[string]bool, len(envelope.Result.Tools))
	for _, tool := range envelope.Result.Tools {
		tools[tool.Name] = true
	}
	return tools
}

func post(t *testing.T, url, session, body string) []byte {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST status = %s; body: %s", resp.Status, raw)
	}
	return sseData(raw)
}

func sseData(raw []byte) []byte {
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "data: ") {
			return []byte(strings.TrimPrefix(strings.TrimSpace(line), "data: "))
		}
	}
	return raw
}
