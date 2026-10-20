package github

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRepositoryAndCheckPagination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		second := r.URL.Query().Get("page") == "2"
		if r.URL.Path == "/user/repos" {
			count := 100
			if second {
				count = 1
			}
			repos := make([]Repository, count)
			for i := range repos {
				repos[i] = Repository{FullName: fmt.Sprintf("owner/repo-%t-%d", second, i)}
			}
			json.NewEncoder(w).Encode(repos)
			return
		}
		count := 100
		if second {
			count = 1
		}
		checks := make([]CheckRun, count)
		json.NewEncoder(w).Encode(PRChecks{TotalCount: 101, CheckRuns: checks})
	}))
	defer server.Close()
	client := NewHTTPClient("token", "owner", slog.Default(), WithBaseURL(server.URL))
	repos, err := client.ListOwnedRepositories(context.Background())
	if err != nil || len(repos) != 101 {
		t.Fatalf("repos: %d %v", len(repos), err)
	}
	checks, err := client.GetPRChecks(context.Background(), "repo", "sha")
	if err != nil || len(checks.CheckRuns) != 101 {
		t.Fatalf("checks: %+v %v", checks, err)
	}
}

func TestPublicForkRejectsPrivateSource(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Error("attempted private fork")
		}
		json.NewEncoder(w).Encode(Repository{Private: true})
	}))
	defer server.Close()
	client := NewHTTPClient("token", "owner", slog.Default(), WithBaseURL(server.URL))
	if _, err := client.ForkPublicRepository(context.Background(), "upstream", "private", "fork", false); err == nil {
		t.Fatal("private source accepted")
	}
}
