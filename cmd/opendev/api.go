package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/iamangus/code-mcp/internal/config"
	githubpkg "github.com/iamangus/code-mcp/internal/github"
	"github.com/iamangus/code-mcp/internal/manager"
	"github.com/iamangus/code-mcp/internal/tools"
)

// registerAPIRoutes registers the management REST API routes directly on mux.
//
// Routes:
//
//	GET    /api/repos                                        – list all repos and their branches
//	POST   /api/repos                                        – clone or sync a repo
//	DELETE /api/repos/{repo}                                 – remove a repo and all its worktrees
//	GET    /api/repos/{repo}/branches                        – list branches for a repo
//	POST   /api/repos/{repo}/branches                        – create a worktree for a branch
//	DELETE /api/repos/{repo}/branches/{branch}               – remove a worktree / notify of merge
//	POST   /api/repos/{repo}/branches/{branch}/push          – push branch to origin
//	POST   /api/repos/{repo}/branches/{branch}/merge         – merge branch into another branch
//	GET    /api/repos/{repo}/branches/{branch}/commits       – list commits unique to branch
//	POST   /api/repos/{repo}/pulls                           – create a draft PR (GitHub)
//	PATCH  /api/repos/{repo}/pulls/{number}                  – update PR body (GitHub)
//	POST   /api/repos/{repo}/pulls/{number}/ready            – promote draft PR to ready-for-review (GitHub)
func registerAPIRoutes(mux *http.ServeMux, mgr *manager.Manager, ghClient githubpkg.Client, logger *slog.Logger, onAdded func(repo, branch, dir string), onRemoved func(repo, branch string)) {
	// GET /api/repos
	mux.HandleFunc("GET /api/repos", func(w http.ResponseWriter, r *http.Request) {
		repos, err := mgr.Scan()
		if err != nil {
			apiError(w, err.Error(), http.StatusInternalServerError, logger)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"repos": repos}, logger)
	})

	// POST /api/repos  {"url":"...", "name":"..."}
	mux.HandleFunc("POST /api/repos", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			URL  string `json:"url"`
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			apiError(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest, logger)
			return
		}
		if body.URL == "" || body.Name == "" {
			apiError(w, "url and name are required", http.StatusBadRequest, logger)
			return
		}
		if err := mgr.SyncRepo(body.URL, body.Name); err != nil {
			apiError(w, err.Error(), http.StatusBadRequest, logger)
			return
		}
		// Scan to discover the default branch of the newly cloned repo.
		repos, _ := mgr.Scan()
		for _, repo := range repos {
			if repo.Name == body.Name {
				for _, b := range repo.Branches {
					onAdded(repo.Name, b.Name, b.Dir)
				}
				writeJSON(w, http.StatusCreated, map[string]any{"repo": repo}, logger)
				return
			}
		}
		writeJSON(w, http.StatusCreated, map[string]any{"repo": map[string]string{"name": body.Name}}, logger)
	})

	// DELETE /api/repos/{repo}
	mux.HandleFunc("DELETE /api/repos/{repo}", func(w http.ResponseWriter, r *http.Request) {
		repo := r.PathValue("repo")
		// Notify callers before removing so they can clean up handlers.
		repos, _ := mgr.Scan()
		for _, ri := range repos {
			if ri.Name == repo {
				for _, b := range ri.Branches {
					onRemoved(repo, b.Name)
				}
				break
			}
		}
		if err := mgr.RemoveRepo(repo); err != nil {
			apiError(w, err.Error(), http.StatusNotFound, logger)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true}, logger)
	})

	// GET /api/repos/{repo}/branches
	mux.HandleFunc("GET /api/repos/{repo}/branches", func(w http.ResponseWriter, r *http.Request) {
		repo := r.PathValue("repo")
		repos, err := mgr.Scan()
		if err != nil {
			apiError(w, err.Error(), http.StatusInternalServerError, logger)
			return
		}
		for _, ri := range repos {
			if ri.Name == repo {
				writeJSON(w, http.StatusOK, map[string]any{"branches": ri.Branches}, logger)
				return
			}
		}
		apiError(w, "repo not found", http.StatusNotFound, logger)
	})

	// POST /api/repos/{repo}/branches  {"branch":"...", "base":"..."}
	// base is optional; when omitted the new branch is forked from HEAD of the
	// main clone (the default branch). base is ignored if the branch already
	// exists locally or at origin.
	mux.HandleFunc("POST /api/repos/{repo}/branches", func(w http.ResponseWriter, r *http.Request) {
		repo := r.PathValue("repo")
		var body struct {
			Branch string `json:"branch"`
			Base   string `json:"base"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			apiError(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest, logger)
			return
		}
		if body.Branch == "" {
			apiError(w, "branch is required", http.StatusBadRequest, logger)
			return
		}
		wtDir, err := mgr.CreateWorktree(repo, body.Branch, body.Base)
		if err != nil {
			apiError(w, err.Error(), http.StatusBadRequest, logger)
			return
		}
		// Fail loudly if .opendev/config.yaml is missing or invalid — the
		// worktree was created successfully but we cannot proceed without a
		// registered test command.
		if _, err := config.Load(wtDir); err != nil {
			// Only remove if this is a real worktree (not the main repo dir).
			if wtDir != mgr.RepoDir(repo) {
				_ = mgr.RemoveWorktree(repo, body.Branch)
			}
			apiError(w, "branch created but .opendev/config.yaml is invalid: "+err.Error(), http.StatusBadRequest, logger)
			return
		}
		onAdded(repo, body.Branch, wtDir)
		writeJSON(w, http.StatusCreated, map[string]any{
			"branch": manager.BranchInfo{Name: body.Branch, Dir: wtDir},
		}, logger)
	})

	// DELETE /api/repos/{repo}/branches/{branch}
	mux.HandleFunc("DELETE /api/repos/{repo}/branches/{branch}", func(w http.ResponseWriter, r *http.Request) {
		repo := r.PathValue("repo")
		branch := r.PathValue("branch")
		onRemoved(repo, branch)
		if err := mgr.RemoveWorktree(repo, branch); err != nil {
			apiError(w, err.Error(), http.StatusNotFound, logger)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true}, logger)
	})

	// POST /api/repos/{repo}/branches/{branch}/push
	// Pushes the branch worktree's commits to origin.
	// Body is optional (ignored).
	mux.HandleFunc("POST /api/repos/{repo}/branches/{branch}/push", func(w http.ResponseWriter, r *http.Request) {
		repo := r.PathValue("repo")
		branch := r.PathValue("branch")

		// Drain body (optional, ignore decode errors).
		var body struct{}
		_ = json.NewDecoder(r.Body).Decode(&body)

		if err := mgr.PushBranch(repo, branch); err != nil {
			apiError(w, err.Error(), http.StatusBadRequest, logger)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true}, logger)
	})

	// POST /api/repos/{repo}/branches/{branch}/merge  {"into":"<target-branch>"}
	// Merges the source branch (path param) into the target branch (body).
	// Returns 200 on success, 409 on merge conflict (body contains git output),
	// 404 if either worktree does not exist.
	mux.HandleFunc("POST /api/repos/{repo}/branches/{branch}/merge", func(w http.ResponseWriter, r *http.Request) {
		repo := r.PathValue("repo")
		sourceBranch := r.PathValue("branch")
		var body struct {
			Into string `json:"into"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			apiError(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest, logger)
			return
		}
		if body.Into == "" {
			apiError(w, "into is required", http.StatusBadRequest, logger)
			return
		}
		if err := mgr.MergeBranch(repo, sourceBranch, body.Into); err != nil {
			if conflictErr, ok := err.(*manager.MergeConflictError); ok {
				writeJSON(w, http.StatusConflict, map[string]any{"error": conflictErr.Output}, logger)
				return
			}
			apiError(w, err.Error(), http.StatusNotFound, logger)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true}, logger)
	})

	// GET /api/repos/{repo}/branches/{branch}/commits
	// Returns commits reachable from branch but not from the default branch.
	mux.HandleFunc("GET /api/repos/{repo}/branches/{branch}/commits", func(w http.ResponseWriter, r *http.Request) {
		repo := r.PathValue("repo")
		branch := r.PathValue("branch")
		commits, err := mgr.GetCommits(repo, branch)
		if err != nil {
			apiError(w, err.Error(), http.StatusNotFound, logger)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"commits": commits}, logger)
	})

	// POST /api/repos/{repo}/branches/{branch}/test/run
	mux.HandleFunc("POST /api/repos/{repo}/branches/{branch}/test/run", func(w http.ResponseWriter, r *http.Request) {
		repo := r.PathValue("repo")
		branch := r.PathValue("branch")

		wtDir, err := mgr.WorktreeDir(repo, branch)
		if err != nil {
			logger.Error("test run: worktree not found", "repo", repo, "branch", branch, "error", err)
			apiError(w, err.Error(), http.StatusNotFound, logger)
			return
		}

		// Optional timeout from request body.
		var body struct {
			TimeoutSeconds int `json:"timeout_seconds"`
		}
		// Body is optional; ignore decode errors.
		_ = json.NewDecoder(r.Body).Decode(&body)

		timeout := tools.DefaultTimeout
		if body.TimeoutSeconds > 0 {
			timeout = time.Duration(body.TimeoutSeconds) * time.Second
		}

		logger.Info("test run: starting", "repo", repo, "branch", branch, "timeout_s", timeout.Seconds())

		result, err := tools.RunTest(wtDir, timeout, logger)
		if err != nil {
			logger.Error("test run: failed to execute", "repo", repo, "branch", branch, "error", err)
			apiError(w, err.Error(), http.StatusNotFound, logger)
			return
		}

		logger.Info("test run: completed",
			"repo", repo,
			"branch", branch,
			"exit_code", result.ExitCode,
			"timed_out", result.TimedOut,
			"stdout", result.Stdout,
			"stderr", result.Stderr,
		)

		writeJSON(w, http.StatusOK, result, logger)
	})

	// ── GitHub PR endpoints (require ghClient; return 501 if not configured) ─

	// POST /api/repos/{repo}/pulls  {"title":"...", "head":"...", "base":"...", "body":"...", "draft":true}
	mux.HandleFunc("POST /api/repos/{repo}/pulls", func(w http.ResponseWriter, r *http.Request) {
		repo := r.PathValue("repo")
		if ghClient == nil {
			apiError(w, "GitHub integration not configured (set GITHUB_TOKEN and GITHUB_OWNER)", http.StatusNotImplemented, logger)
			return
		}
		var body struct {
			Title string `json:"title"`
			Head  string `json:"head"`
			Base  string `json:"base"`
			Body  string `json:"body"`
			Draft bool   `json:"draft"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			apiError(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest, logger)
			return
		}
		if body.Title == "" || body.Head == "" || body.Base == "" {
			apiError(w, "title, head, and base are required", http.StatusBadRequest, logger)
			return
		}
		pr, err := ghClient.CreatePR(r.Context(), githubpkg.CreatePROptions{
			Repo: repo, Title: body.Title, Head: body.Head, Base: body.Base, Body: body.Body, Draft: body.Draft,
		})
		if err != nil {
			apiError(w, "creating PR: "+err.Error(), http.StatusBadGateway, logger)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"pr_number": pr.Number, "pr_url": pr.HTMLURL}, logger)
	})

	// PATCH /api/repos/{repo}/pulls/{number}  {"body":"...", "draft":false}
	mux.HandleFunc("PATCH /api/repos/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		repo := r.PathValue("repo")
		if ghClient == nil {
			apiError(w, "GitHub integration not configured (set GITHUB_TOKEN and GITHUB_OWNER)", http.StatusNotImplemented, logger)
			return
		}
		prNumber, err := strconv.Atoi(r.PathValue("number"))
		if err != nil || prNumber <= 0 {
			apiError(w, "invalid PR number", http.StatusBadRequest, logger)
			return
		}
		var body struct {
			Body  string `json:"body"`
			Draft bool   `json:"draft"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			apiError(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest, logger)
			return
		}
		if err := ghClient.UpdatePR(r.Context(), repo, prNumber, body.Body); err != nil {
			apiError(w, "updating PR: "+err.Error(), http.StatusBadGateway, logger)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true}, logger)
	})

	// POST /api/repos/{repo}/pulls/{number}/ready
	mux.HandleFunc("POST /api/repos/{repo}/pulls/{number}/ready", func(w http.ResponseWriter, r *http.Request) {
		repo := r.PathValue("repo")
		if ghClient == nil {
			apiError(w, "GitHub integration not configured (set GITHUB_TOKEN and GITHUB_OWNER)", http.StatusNotImplemented, logger)
			return
		}
		prNumber, err := strconv.Atoi(r.PathValue("number"))
		if err != nil || prNumber <= 0 {
			apiError(w, "invalid PR number", http.StatusBadRequest, logger)
			return
		}
		if err := ghClient.PromotePR(r.Context(), repo, prNumber); err != nil {
			apiError(w, "promoting PR: "+err.Error(), http.StatusBadGateway, logger)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true}, logger)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any, logger *slog.Logger) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		logger.Error("api: JSON encode failed", "error", err)
	}
}

func apiError(w http.ResponseWriter, msg string, status int, logger *slog.Logger) {
	writeJSON(w, status, map[string]any{"error": msg}, logger)
}
