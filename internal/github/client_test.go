package github

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPClient_ListOwnedRepositories(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/user/repos" || r.URL.Query().Get("affiliation") != "owner" || r.URL.Query().Get("per_page") != "100" {
			t.Errorf("unexpected request: %s", r.URL.String())
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("wrong auth header: %s", r.Header.Get("Authorization"))
		}
		json.NewEncoder(w).Encode([]map[string]any{
			{"name": "owned", "full_name": "owner/owned", "clone_url": "https://github.com/owner/owned.git", "default_branch": "main"},
			{"name": "other", "full_name": "other/other", "clone_url": "https://github.com/other/other.git", "default_branch": "main"},
		})
	}))
	defer srv.Close()

	c := NewHTTPClient("test-token", "owner", slog.Default(), WithBaseURL(srv.URL))
	repos, err := c.ListOwnedRepositories(context.Background())
	if err != nil {
		t.Fatalf("ListOwnedRepositories: %v", err)
	}
	if len(repos) != 1 || repos[0].FullName != "owner/owned" {
		t.Fatalf("unexpected repositories: %+v", repos)
	}
}

func TestHTTPClient_GetRepository(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/repos/owner/myrepo" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("wrong auth header: %s", r.Header.Get("Authorization"))
		}
		json.NewEncoder(w).Encode(map[string]any{
			"name": "myrepo", "full_name": "owner/myrepo", "clone_url": "https://github.com/owner/myrepo.git",
			"default_branch": "main", "private": true, "fork": true,
			"parent": map[string]any{"full_name": "forker/myrepo"},
			"source": map[string]any{"full_name": "upstream/myrepo"},
		})
	}))
	defer srv.Close()

	c := NewHTTPClient("test-token", "owner", slog.Default(), WithBaseURL(srv.URL))
	repo, err := c.GetRepository(context.Background(), "myrepo")
	if err != nil {
		t.Fatalf("GetRepository: %v", err)
	}
	if repo.Name != "myrepo" || repo.FullName != "owner/myrepo" || repo.CloneURL != "https://github.com/owner/myrepo.git" || repo.DefaultBranch != "main" || !repo.Private || !repo.Fork {
		t.Errorf("unexpected repository: %+v", repo)
	}
	if repo.Parent == nil || repo.Parent.FullName != "forker/myrepo" || repo.Source == nil || repo.Source.FullName != "upstream/myrepo" {
		t.Errorf("unexpected fork lineage: %+v", repo)
	}
}

func TestHTTPClient_GetRepository_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()

	c := NewHTTPClient("test-token", "owner", slog.Default(), WithBaseURL(srv.URL))
	_, err := c.GetRepository(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetRepository error = %v, want ErrNotFound", err)
	}
}

func TestHTTPClient_CreateRepository(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/user/repos" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if body["name"] != "myrepo" || body["description"] != "A repository" || body["private"] != true {
			t.Errorf("unexpected body: %+v", body)
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"name": "myrepo", "full_name": "owner/myrepo", "clone_url": "https://github.com/owner/myrepo.git", "default_branch": "main", "private": true})
	}))
	defer srv.Close()

	c := NewHTTPClient("test-token", "owner", slog.Default(), WithBaseURL(srv.URL))
	repo, err := c.CreateRepository(context.Background(), "myrepo", "A repository", true)
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	if repo.FullName != "owner/myrepo" || !repo.Private || repo.DefaultBranch != "main" {
		t.Errorf("unexpected repository: %+v", repo)
	}
}

func TestHTTPClient_ForkPublicRepository(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/repos/upstream/source/forks" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if body["owner"] != "owner" || body["name"] != "myfork" || body["private"] != false {
			t.Errorf("unexpected body: %+v", body)
		}
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{
			"name": "myfork", "full_name": "owner/myfork", "clone_url": "https://github.com/owner/myfork.git", "default_branch": "main", "fork": true,
			"parent": map[string]any{"full_name": "upstream/source"}, "source": map[string]any{"full_name": "upstream/source"},
		})
	}))
	defer srv.Close()

	c := NewHTTPClient("test-token", "owner", slog.Default(), WithBaseURL(srv.URL))
	repo, err := c.ForkPublicRepository(context.Background(), "upstream", "source", "myfork", false)
	if err != nil {
		t.Fatalf("ForkPublicRepository: %v", err)
	}
	if repo.FullName != "owner/myfork" || !repo.Fork || repo.Parent == nil || repo.Parent.FullName != "upstream/source" || repo.Source == nil || repo.Source.FullName != "upstream/source" {
		t.Errorf("unexpected repository: %+v", repo)
	}
}

func TestFakeClient_RepositoryLifecycle(t *testing.T) {
	fake := NewFakeClient()
	if _, err := fake.GetRepository(context.Background(), "existing"); err != nil {
		t.Fatalf("GetRepository: %v", err)
	}

	wantErr := errors.New("create failed")
	fake.CreateRepositoryError = wantErr
	if _, err := fake.CreateRepository(context.Background(), "new", "description", true); !errors.Is(err, wantErr) {
		t.Fatalf("CreateRepository error = %v, want %v", err, wantErr)
	}

	if _, err := fake.ForkPublicRepository(context.Background(), "upstream", "source", "fork", false); err != nil {
		t.Fatalf("ForkPublicRepository: %v", err)
	}

	if len(fake.Calls) != 3 {
		t.Fatalf("got %d calls, want 3", len(fake.Calls))
	}
	if call := fake.Calls[0]; call.Method != "GetRepository" || call.Args[0] != "existing" {
		t.Errorf("unexpected GetRepository call: %+v", call)
	}
	if call := fake.Calls[1]; call.Method != "CreateRepository" || call.Args[0] != "new" || call.Args[1] != "description" || call.Args[2] != true {
		t.Errorf("unexpected CreateRepository call: %+v", call)
	}
	if call := fake.Calls[2]; call.Method != "ForkPublicRepository" || call.Args[0] != "upstream" || call.Args[1] != "source" || call.Args[2] != "fork" || call.Args[3] != false {
		t.Errorf("unexpected ForkPublicRepository call: %+v", call)
	}
}

func TestHTTPClient_CreatePR(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/repos/owner/myrepo/pulls" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("wrong auth header: %s", r.Header.Get("Authorization"))
		}

		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["title"] != "Test PR" {
			t.Errorf("unexpected title: %v", body["title"])
		}
		if body["draft"] != true {
			t.Errorf("expected draft=true, got %v", body["draft"])
		}

		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"number":   42,
			"html_url": "https://github.com/owner/myrepo/pull/42",
		})
	}))
	defer srv.Close()

	c := NewHTTPClient("test-token", "owner", slog.Default(), WithBaseURL(srv.URL))
	pr, err := c.CreatePR(context.Background(), CreatePROptions{
		Repo:  "myrepo",
		Title: "Test PR",
		Head:  "feature",
		Base:  "main",
		Body:  "description",
		Draft: true,
	})
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	if pr.Number != 42 {
		t.Errorf("expected PR #42, got #%d", pr.Number)
	}
	if pr.HTMLURL != "https://github.com/owner/myrepo/pull/42" {
		t.Errorf("unexpected URL: %s", pr.HTMLURL)
	}
}

func TestHTTPClient_CreatePR_NotDraft(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["draft"] != false {
			t.Errorf("expected draft=false, got %v", body["draft"])
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"number": 1, "html_url": "https://example.com/1"})
	}))
	defer srv.Close()

	c := NewHTTPClient("tok", "owner", slog.Default(), WithBaseURL(srv.URL))
	_, err := c.CreatePR(context.Background(), CreatePROptions{Repo: "r", Title: "t", Head: "h", Base: "b", Draft: false})
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
}

func TestHTTPClient_UpdatePR(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("expected PATCH, got %s", r.Method)
		}
		if r.URL.Path != "/repos/owner/myrepo/pulls/42" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["body"] != "new body" {
			t.Errorf("unexpected body: %v", body["body"])
		}
		if body["title"] != "new title" {
			t.Errorf("unexpected title: %v", body["title"])
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewHTTPClient("test-token", "owner", slog.Default(), WithBaseURL(srv.URL))
	if err := c.UpdatePR(context.Background(), "myrepo", 42, "new title", "new body"); err != nil {
		t.Fatalf("UpdatePR: %v", err)
	}
}

func TestHTTPClient_PromotePR(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("expected PATCH, got %s", r.Method)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["draft"] != false {
			t.Errorf("expected draft=false, got %v", body["draft"])
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewHTTPClient("test-token", "owner", slog.Default(), WithBaseURL(srv.URL))
	if err := c.PromotePR(context.Background(), "myrepo", 42); err != nil {
		t.Fatalf("PromotePR: %v", err)
	}
}

func TestHTTPClient_GetPR(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/repos/owner/myrepo/pulls/42" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("wrong auth header: %s", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Accept") != "application/vnd.github+json" {
			t.Errorf("wrong accept header: %s", r.Header.Get("Accept"))
		}
		json.NewEncoder(w).Encode(map[string]any{
			"number": 42, "html_url": "https://github.com/owner/myrepo/pull/42",
			"state": "open", "merged": false, "draft": true,
			"head": map[string]any{"sha": "abc123"},
		})
	}))
	defer srv.Close()

	c := NewHTTPClient("test-token", "owner", slog.Default(), WithBaseURL(srv.URL))
	pr, err := c.GetPR(context.Background(), "myrepo", 42)
	if err != nil {
		t.Fatalf("GetPR: %v", err)
	}
	if pr.Number != 42 || pr.State != "open" || !pr.Draft || pr.Merged || pr.Head.SHA != "abc123" {
		t.Errorf("unexpected PR: %+v", pr)
	}
}

func TestHTTPClient_GetPRChecks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/repos/owner/myrepo/commits/abc123/check-runs" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "" {
			t.Errorf("unexpected content type: %s", r.Header.Get("Content-Type"))
		}
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 2,
			"check_runs": []map[string]any{
				{"name": "lint", "status": "completed", "conclusion": "success"},
				{"name": "test", "status": "in_progress", "conclusion": nil},
			},
		})
	}))
	defer srv.Close()

	c := NewHTTPClient("test-token", "owner", slog.Default(), WithBaseURL(srv.URL))
	checks, err := c.GetPRChecks(context.Background(), "myrepo", "abc123")
	if err != nil {
		t.Fatalf("GetPRChecks: %v", err)
	}
	if checks.TotalCount != 2 || len(checks.CheckRuns) != 2 {
		t.Fatalf("unexpected checks: %+v", checks)
	}
	if checks.CheckRuns[0].Name != "lint" || checks.CheckRuns[0].Conclusion != "success" {
		t.Errorf("unexpected completed check: %+v", checks.CheckRuns[0])
	}
	if checks.CheckRuns[1].Status != "in_progress" || checks.CheckRuns[1].Conclusion != "" {
		t.Errorf("unexpected pending check: %+v", checks.CheckRuns[1])
	}
}

func TestHTTPClient_MergePR(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("expected PUT, got %s", r.Method)
		}
		if r.URL.Path != "/repos/owner/myrepo/pulls/42/merge" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if body["merge_method"] != "squash" {
			t.Errorf("unexpected merge method: %v", body["merge_method"])
		}
		json.NewEncoder(w).Encode(map[string]any{"merged": true, "message": "Pull Request successfully merged"})
	}))
	defer srv.Close()

	c := NewHTTPClient("test-token", "owner", slog.Default(), WithBaseURL(srv.URL))
	if err := c.MergePR(context.Background(), "myrepo", 42); err != nil {
		t.Fatalf("MergePR: %v", err)
	}
}

func TestHTTPClient_MergePR_NotMerged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"merged": false, "message": "Required status check is expected"})
	}))
	defer srv.Close()

	c := NewHTTPClient("test-token", "owner", slog.Default(), WithBaseURL(srv.URL))
	err := c.MergePR(context.Background(), "myrepo", 42)
	if err == nil {
		t.Fatal("expected error when merge is rejected")
	}
}

func TestHTTPClient_ErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"message":"Validation Failed"}`))
	}))
	defer srv.Close()

	c := NewHTTPClient("test-token", "owner", slog.Default(), WithBaseURL(srv.URL))
	_, err := c.CreatePR(context.Background(), CreatePROptions{
		Repo: "myrepo", Title: "t", Head: "h", Base: "b",
	})
	if err == nil {
		t.Fatal("expected error for 422 response")
	}
}
