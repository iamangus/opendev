package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	githubpkg "github.com/iamangus/code-mcp/internal/github"
	"github.com/iamangus/code-mcp/internal/gitops"
	"github.com/iamangus/code-mcp/internal/locks"
	"github.com/iamangus/code-mcp/internal/manager"
	"github.com/iamangus/code-mcp/internal/tools"
	"github.com/mark3labs/mcp-go/server"
)

type fakeRepositorySyncer struct {
	calls [][2]string
	err   error
}

func (s *fakeRepositorySyncer) SyncRepo(url, name string, _ ...string) error {
	s.calls = append(s.calls, [2]string{url, name})
	return s.err
}

func TestSyncOwnedRepositories(t *testing.T) {
	client := githubpkg.NewFakeClient()
	client.ListOwnedRepositoriesResult = []githubpkg.Repository{
		{Name: "valid", FullName: "owner/valid", CloneURL: "https://github.com/owner/valid.git"},
		{Name: "missing-url", FullName: "owner/missing-url"},
	}
	syncer := &fakeRepositorySyncer{}
	syncOwnedRepositories(context.Background(), client, syncer, slog.Default())
	if len(syncer.calls) != 1 || syncer.calls[0] != [2]string{"https://github.com/owner/valid.git", "valid"} {
		t.Fatalf("unexpected sync calls: %#v", syncer.calls)
	}
}

func TestSyncOwnedRepositoriesContinuesAfterSyncError(t *testing.T) {
	client := githubpkg.NewFakeClient()
	client.ListOwnedRepositoriesResult = []githubpkg.Repository{{Name: "valid", CloneURL: "https://github.com/owner/valid.git"}}
	syncer := &fakeRepositorySyncer{err: errors.New("clone failed")}
	syncOwnedRepositories(context.Background(), client, syncer, slog.Default())
	if len(syncer.calls) != 1 {
		t.Fatalf("expected one sync attempt, got %d", len(syncer.calls))
	}
}

// initGitRepo creates a minimal git repository with an initial commit.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "init")
}

// TestProfiles verifies that the Profiles slice contains the expected profiles.
func TestProfiles(t *testing.T) {
	if len(Profiles) < 2 {
		t.Fatalf("expected at least 2 profiles, got %d", len(Profiles))
	}
	seen := map[Profile]bool{}
	for _, p := range Profiles {
		seen[p] = true
	}
	for _, want := range []Profile{ProfileRead, ProfileWrite} {
		if !seen[want] {
			t.Errorf("profile %q not found in Profiles", want)
		}
	}
}

func TestEphemeralMCPServerNamesAreUniquePerDispatch(t *testing.T) {
	seen := map[string]bool{}
	for _, tc := range []struct {
		jobID   string
		role    string
		taskKey string
		attempt int
	}{
		{"job-one", "writer", "task-one", 1},
		{"job-one", "writer", "task-two", 1},
		{"job-two", "writer", "task-one", 1},
		{"job-one", "writer", "task-one", 2},
		{"job-one", "reviewer", "task-one", 1},
	} {
		for _, kind := range []string{"jobs", "read", "write"} {
			name := ephemeralMCPServerName(tc.jobID, tc.role, tc.taskKey, tc.attempt, kind)
			if seen[name] {
				t.Fatalf("duplicate ephemeral MCP server name %q", name)
			}
			seen[name] = true
		}
	}
	if ephemeralMCPServerName("a/b", "writer", "task", 1, "jobs") == ephemeralMCPServerName("a-b", "writer", "task", 1, "jobs") {
		t.Fatal("MCP server names must remain distinct when identifiers contain separators")
	}
}

// TestNewMCPHandler_ProfilesProduceHandlers verifies that newMCPHandler returns
// a non-nil handler for each known profile.
func TestNewMCPHandler_ProfilesProduceHandlers(t *testing.T) {
	dir := t.TempDir()
	for _, p := range Profiles {
		h := newMCPHandler(p, dir, slog.Default())
		if h == nil {
			t.Errorf("newMCPHandler(%q) returned nil", p)
		}
	}
}

// TestRegisterReadTools_ToolList verifies that the read profile exposes only
// read-only tools and not write tools.
func TestRegisterReadTools_ToolList(t *testing.T) {
	dir := t.TempDir()
	lm := locks.NewManager(slog.Default())
	s := server.NewMCPServer("opendev", "1.0.0", server.WithToolCapabilities(true))
	registerReadTools(s, lm, tools.NewRevisionAuthorizer(), dir, slog.Default())

	// Use stateless mode so tests don't need to manage session negotiation.
	h := server.NewStreamableHTTPServer(s, server.WithStateLess(true))
	ts := httptest.NewServer(h)
	defer ts.Close()

	toolNames := listMCPTools(t, ts.URL)

	wantPresent := []string{"read_file", "read_lines", "list_directory", "grep_search"}
	wantAbsent := []string{"create_file", "search_and_replace"}

	for _, name := range wantPresent {
		if !toolNames[name] {
			t.Errorf("read profile: expected tool %q to be present", name)
		}
	}
	for _, name := range wantAbsent {
		if toolNames[name] {
			t.Errorf("read profile: expected tool %q to be absent", name)
		}
	}
}

// TestRegisterWriteTools_ToolList verifies that the write profile exposes only
// write/mutate tools and not read tools.
func TestRegisterWriteTools_ToolList(t *testing.T) {
	dir := t.TempDir()
	lm := locks.NewManager(slog.Default())
	s := server.NewMCPServer("opendev", "1.0.0", server.WithToolCapabilities(true))
	registerWriteTools(s, lm, tools.NewRevisionAuthorizer(), dir, slog.Default())

	h := server.NewStreamableHTTPServer(s, server.WithStateLess(true))
	ts := httptest.NewServer(h)
	defer ts.Close()

	toolList := listMCPTools(t, ts.URL)

	wantPresent := []string{"create_file", "search_and_replace"}
	wantAbsent := []string{"read_file", "read_lines", "list_directory", "grep_search"}

	for _, name := range wantPresent {
		if !toolList[name] {
			t.Errorf("write profile: expected tool %q to be present", name)
		}
	}
	for _, name := range wantAbsent {
		if toolList[name] {
			t.Errorf("write profile: expected tool %q to be absent", name)
		}
	}
}

// TestMultiServerRouting verifies that /{repo}/{branch}/{profile}/mcp routes
// to the correct handler and that unknown profiles or repos return 404.
func TestMultiServerRouting(t *testing.T) {
	reposDir := t.TempDir()
	repoDir := filepath.Join(reposDir, "myrepo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, repoDir)

	gitOps := gitops.NewExec(slog.Default(), "")
	mgr, err := manager.New(reposDir, gitOps, slog.Default())
	if err != nil {
		t.Fatalf("manager.New: %v", err)
	}

	handlers := map[string]http.Handler{}

	repos, err := mgr.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for _, repo := range repos {
		for _, b := range repo.Branches {
			for _, p := range Profiles {
				key := repo.Name + "/" + b.Name + "/" + string(p)
				handlers[key] = newMCPHandler(p, b.Dir, slog.Default())
			}
		}
	}

	mcpMux := http.NewServeMux()
	mcpMux.HandleFunc("/{repo}/{branch}/{profile}/mcp", func(w http.ResponseWriter, r *http.Request) {
		repo := r.PathValue("repo")
		branch := r.PathValue("branch")
		profile := r.PathValue("profile")
		key := repo + "/" + branch + "/" + profile
		h, ok := handlers[key]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		h.ServeHTTP(w, r)
	})

	srv := httptest.NewServer(mcpMux)
	defer srv.Close()

	// Each profile should return 200 for a valid initialize POST.
	for _, p := range Profiles {
		path := "/myrepo/main/" + string(p) + "/mcp"
		resp := postInitialize(t, srv.URL+path)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("profile %q: expected 200, got %d", p, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// Unknown profile should 404.
	resp := postInitialize(t, srv.URL+"/myrepo/main/unknown/mcp")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown profile: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Unknown repo should 404.
	resp = postInitialize(t, srv.URL+"/norepo/main/read/mcp")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown repo: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// listMCPTools performs a stateless MCP initialize + tools/list and returns the
// set of tool names advertised by the server at baseURL.
func listMCPTools(t *testing.T, baseURL string) map[string]bool {
	t.Helper()

	post := func(body string) []byte {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, baseURL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		// Server may return SSE — extract the first data line.
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "data: ") {
				return []byte(strings.TrimPrefix(line, "data: "))
			}
		}
		return raw
	}

	// Step 1: initialize (required even in stateless mode).
	post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`)

	// Step 2: tools/list.
	raw := post(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)

	var envelope struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("parse tools/list response: %v\nraw: %s", err, raw)
	}
	out := map[string]bool{}
	for _, tool := range envelope.Result.Tools {
		out[tool.Name] = true
	}
	return out
}

// postInitialize sends an MCP initialize request and returns the response.
func postInitialize(t *testing.T, url string) *http.Response {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("initialize request to %s: %v", url, err)
	}
	return resp
}
