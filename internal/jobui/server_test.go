package jobui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/iamangus/code-mcp/internal/pipeline"
)

type testStore struct {
	jobs     []*pipeline.Job
	archived *pipeline.Job
}

func (s *testStore) List() []*pipeline.Job { return s.jobs }
func (s *testStore) Get(id string) (*pipeline.Job, error) {
	for _, job := range s.jobs {
		if job.ID == id {
			return job, nil
		}
	}
	if s.archived != nil && s.archived.ID == id {
		return s.archived, nil
	}
	return nil, pipeline.ErrNotFound
}

func dashboard(t *testing.T, store Store) *httptest.Server {
	t.Helper()
	public, protected := http.NewServeMux(), http.NewServeMux()
	Register(public, protected, store)
	public.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		protected.ServeHTTP(w, r)
	}))
	server := httptest.NewServer(public)
	t.Cleanup(server.Close)
	return server
}

func get(t *testing.T, server *httptest.Server, path, token string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response
}

func TestOnlyStaticShellIsPublic(t *testing.T) {
	store := &testStore{jobs: []*pipeline.Job{{ID: "private-job", Directive: "private directive", Repository: "private-repo", Status: pipeline.JobWorking}}}
	server := dashboard(t, store)
	for _, path := range []string{"/ui/", "/ui/app.js", "/ui/app.css"} {
		response := get(t, server, path, "")
		data, _ := io.ReadAll(response.Body)
		if response.StatusCode != 200 || strings.Contains(string(data), "private directive") {
			t.Fatalf("public asset %s exposed job state or failed", path)
		}
		if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Content-Security-Policy") == "" {
			t.Fatal("missing dashboard security headers")
		}
	}
	for _, path := range []string{"/ui/api/jobs", "/ui/api/jobs/private-job", "/ui/api/jobs?token=test-secret"} {
		if response := get(t, server, path, ""); response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("job API is public: %s", path)
		}
		if response := get(t, server, path, "wrong"); response.StatusCode != http.StatusUnauthorized {
			t.Fatal("invalid existing token accepted")
		}
	}
	if response := get(t, server, "/ui/api/jobs/private-job", "test-secret"); response.StatusCode != 200 {
		t.Fatal("existing token cannot access job state")
	}
}

func TestJobListSummariesFiltersAndPagination(t *testing.T) {
	store := &testStore{jobs: []*pipeline.Job{
		{ID: "working", Repository: "alpha", Directive: "Update API", Status: pipeline.JobWorking, Plan: &pipeline.Plan{Summary: "Improve endpoints", Tasks: []pipeline.Task{{Status: pipeline.TaskIntegrated}, {Status: pipeline.TaskBlocked, BlockReason: "Needs credentials"}}}},
		{ID: "failed", Repository: "beta", Directive: "Fix search", Status: pipeline.JobFailed, Failure: "Agent failed"},
		{ID: "done", Repository: "alpha", Directive: "Improve docs", Status: pipeline.JobPublished},
		{ID: "foundation", Repository: "beta", Directive: "Set up project", Status: pipeline.JobCreated, FoundationPending: true},
	}}
	server := dashboard(t, store)
	var result struct {
		Jobs   []summary      `json:"jobs"`
		Total  int            `json:"total"`
		Counts map[string]int `json:"counts"`
	}
	response := get(t, server, "/ui/api/jobs?limit=1&offset=1", "test-secret")
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Total != 4 || len(result.Jobs) != 1 || result.Jobs[0].ID != "failed" {
		t.Fatalf("pagination: %+v", result)
	}
	if !reflect.DeepEqual(result.Counts, map[string]int{"all": 4, "active": 2, "attention": 2, "completed": 1}) {
		t.Fatalf("counts: %+v", result.Counts)
	}
	response = get(t, server, "/ui/api/jobs?filter=attention&search=ALPHA", "test-secret")
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 || result.Jobs[0].TasksDone != 1 || result.Jobs[0].TasksBlocked != 1 || result.Jobs[0].Title != "Improve endpoints" || result.Jobs[0].Reason != "Needs credentials" {
		t.Fatalf("filtered task progress: %+v", result)
	}
	for _, query := range []string{"limit=101", "limit=0", "offset=-1", "offset=bad", "filter=unknown", "search=" + strings.Repeat("x", 201)} {
		if response := get(t, server, "/ui/api/jobs?"+query, "test-secret"); response.StatusCode != 400 {
			t.Fatalf("accepted invalid query: %s", query)
		}
	}
}

func TestJobInspectionIsReadOnlyAndSupportsArchives(t *testing.T) {
	dir := t.TempDir()
	store, err := pipeline.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.Create("repo", "<script>alert('unsafe')</script>", "main", "", "p", "w", "r")
	if err != nil {
		t.Fatal(err)
	}
	server := dashboard(t, store)
	response := get(t, server, "/ui/api/jobs/"+job.ID, "test-secret")
	var inspected pipeline.Job
	if err := json.NewDecoder(response.Body).Decode(&inspected); err != nil {
		t.Fatal(err)
	}
	if inspected.Directive != job.Directive || inspected.Status != pipeline.JobCreated {
		t.Fatal("inspection changed source data")
	}
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/ui/api/jobs/"+job.ID, nil)
	request.Header.Set("Authorization", "Bearer test-secret")
	response, err = server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatal("dashboard accepted a mutation request")
	}
	current, _ := store.Get(job.ID)
	if !reflect.DeepEqual(current, job) {
		t.Fatal("dashboard mutated the job")
	}
	if _, err := store.Cancel(job.ID, "operator"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Archive(job.ID); err != nil {
		t.Fatal(err)
	}
	response = get(t, server, "/ui/api/jobs/"+job.ID, "test-secret")
	if response.StatusCode != 200 {
		t.Fatal("archived job inspection unavailable")
	}
	if response := get(t, server, "/ui/api/jobs/missing", "test-secret"); response.StatusCode != 404 {
		t.Fatal("unknown job did not return 404")
	}
}

func TestSummaryRecognizesAttentionWithoutLosingJobStage(t *testing.T) {
	job := &pipeline.Job{ID: "job", Status: pipeline.JobWorking, Directive: strings.Repeat("界", 200), UpdatedAt: time.Now(), CI: &pipeline.CIRecord{State: pipeline.CIFailed}}
	result := summarize(job)
	if !result.NeedsAttention || result.Status != pipeline.JobWorking || len([]rune(result.Title)) != 181 {
		t.Fatalf("summary: %+v", result)
	}
	job.Status = pipeline.JobCancelled
	job.Failure = "operator cancelled"
	if summarize(job).NeedsAttention {
		t.Fatal("cancelled job reported as requiring intervention")
	}
}
