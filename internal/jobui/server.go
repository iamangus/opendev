// Package jobui serves the embedded, read-only coding job dashboard.
package jobui

import (
	"embed"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/iamangus/code-mcp/internal/pipeline"
)

//go:embed assets/index.html assets/app.css assets/app.js
var assets embed.FS

// Store deliberately excludes every mutation and dispatch operation.
type Store interface {
	List() []*pipeline.Job
	Get(string) (*pipeline.Job, error)
}

type summary struct {
	ID                string             `json:"id"`
	Repository        string             `json:"repository"`
	Title             string             `json:"title"`
	Status            pipeline.JobStatus `json:"status"`
	FoundationPending bool               `json:"foundation_pending"`
	NeedsAttention    bool               `json:"needs_attention"`
	TasksTotal        int                `json:"tasks_total"`
	TasksDone         int                `json:"tasks_done"`
	TasksBlocked      int                `json:"tasks_blocked"`
	Reason            string             `json:"reason,omitempty"`
	CIState           pipeline.CIState   `json:"ci_state,omitempty"`
	PullRequestURL    string             `json:"pull_request_url,omitempty"`
	CreatedAt         time.Time          `json:"created_at"`
	UpdatedAt         time.Time          `json:"updated_at"`
}

// Register keeps only the static shell public. All job data is registered on
// protected, which the application must wrap in its existing bearer middleware.
// No browser sessions, cookies, new credentials, or token URL parameters exist.
func Register(public, protected *http.ServeMux, store Store) {
	public.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "ui/", http.StatusTemporaryRedirect)
	})
	public.HandleFunc("GET /ui", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "ui/", http.StatusTemporaryRedirect)
	})
	for _, asset := range []struct{ route, file, contentType string }{
		{"GET /ui/{$}", "index.html", "text/html; charset=utf-8"},
		{"GET /ui/app.css", "app.css", "text/css; charset=utf-8"},
		{"GET /ui/app.js", "app.js", "text/javascript; charset=utf-8"},
	} {
		data, err := assets.ReadFile("assets/" + asset.file)
		if err != nil {
			panic(err) // An embedded asset missing from the binary is a build defect.
		}
		public.HandleFunc(asset.route, func(w http.ResponseWriter, r *http.Request) {
			securityHeaders(w)
			w.Header().Set("Content-Type", asset.contentType)
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.WriteHeader(http.StatusOK)
			if r.Method != http.MethodHead {
				_, _ = w.Write(data)
			}
		})
	}
	protected.HandleFunc("GET /ui/api/jobs", func(w http.ResponseWriter, r *http.Request) {
		limit, err := integer(r, "limit", 50, 1, 100)
		if err != nil {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 100")
			return
		}
		offset, err := integer(r, "offset", 0, 0, 1_000_000)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid offset")
			return
		}
		filter := r.URL.Query().Get("filter")
		if filter == "" {
			filter = "all"
		}
		if filter != "all" && filter != "active" && filter != "attention" && filter != "completed" {
			writeError(w, http.StatusBadRequest, "invalid job filter")
			return
		}
		query := strings.TrimSpace(r.URL.Query().Get("search"))
		if utf8.RuneCountInString(query) > 200 {
			writeError(w, http.StatusBadRequest, "search exceeds 200 characters")
			return
		}
		query = strings.ToLower(query)
		counts := map[string]int{"all": 0, "active": 0, "attention": 0, "completed": 0}
		matches := make([]summary, 0)
		for _, job := range store.List() {
			if job == nil {
				continue
			}
			item := summarize(job)
			active := !pipeline.Terminal(job.Status)
			completed := job.Status == pipeline.JobPublished || job.Status == pipeline.JobNoChanges || job.Status == pipeline.JobCancelled
			counts["all"]++
			if active {
				counts["active"]++
			}
			if item.NeedsAttention {
				counts["attention"]++
			}
			if completed {
				counts["completed"]++
			}
			if filter == "active" && !active || filter == "attention" && !item.NeedsAttention || filter == "completed" && !completed {
				continue
			}
			if query != "" && !strings.Contains(strings.ToLower(job.Repository+"\n"+job.ID+"\n"+job.Directive+"\n"+item.Title), query) {
				continue
			}
			matches = append(matches, item)
		}
		total := len(matches)
		start := min(offset, total)
		writeJSON(w, map[string]any{"jobs": matches[start:min(start+limit, total)], "total": total, "offset": start, "limit": limit, "counts": counts, "updated_at": time.Now().UTC()})
	})
	protected.HandleFunc("GET /ui/api/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if len(id) > 200 {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		job, err := store.Get(id)
		if errors.Is(err, pipeline.ErrNotFound) {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not read job")
			return
		}
		// Get also supports archived jobs when opened using their exact job ID.
		writeJSON(w, job)
	})
}

func summarize(job *pipeline.Job) summary {
	title := job.Directive
	if job.Plan != nil && strings.TrimSpace(job.Plan.Summary) != "" {
		title = job.Plan.Summary
	}
	item := summary{ID: job.ID, Repository: job.Repository, Title: clip(title, 180), Status: job.Status, FoundationPending: job.FoundationPending, Reason: clip(job.Failure, 240), PullRequestURL: job.PullRequestURL, CreatedAt: job.CreatedAt, UpdatedAt: job.UpdatedAt}
	item.NeedsAttention = job.Status == pipeline.JobFailed || job.Status == pipeline.JobPaused || job.Failure != ""
	if job.CI != nil {
		item.CIState = job.CI.State
		if job.CI.State == pipeline.CIFailed || job.CI.State == pipeline.CIBlocked {
			item.NeedsAttention = true
		}
	}
	if job.Plan != nil {
		item.TasksTotal = len(job.Plan.Tasks)
		for _, task := range job.Plan.Tasks {
			if task.Status == pipeline.TaskIntegrated || task.Status == pipeline.TaskNoChanges {
				item.TasksDone++
			}
			if task.Status == pipeline.TaskBlocked {
				item.TasksBlocked++
				item.NeedsAttention = true
				if item.Reason == "" {
					item.Reason = clip(task.BlockReason, 240)
				}
			}
		}
	}
	if job.Status == pipeline.JobPublished || job.Status == pipeline.JobNoChanges || job.Status == pipeline.JobCancelled {
		item.NeedsAttention = false
	}
	return item
}

func integer(r *http.Request, name string, fallback, minimum, maximum int) (int, error) {
	text := r.URL.Query().Get(name)
	if text == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(text)
	if err != nil || value < minimum || value > maximum {
		return 0, strconv.ErrRange
	}
	return value, nil
}

func clip(value string, maximum int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) > maximum {
		return string(runes[:maximum]) + "…"
	}
	return value
}

func securityHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'; form-action 'self'")
}

func writeError(w http.ResponseWriter, status int, message string) {
	securityHeaders(w)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, data any) {
	securityHeaders(w)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(data)
}
