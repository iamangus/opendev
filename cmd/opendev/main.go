package main

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/iamangus/code-mcp/internal/agentfoundry"
	"github.com/iamangus/code-mcp/internal/dispatcher"
	"github.com/iamangus/code-mcp/internal/dispatchstore"
	"github.com/iamangus/code-mcp/internal/durable"
	githubpkg "github.com/iamangus/code-mcp/internal/github"
	"github.com/iamangus/code-mcp/internal/gitops"
	"github.com/iamangus/code-mcp/internal/graphiti"
	"github.com/iamangus/code-mcp/internal/graphmcp"
	"github.com/iamangus/code-mcp/internal/jobmcp"
	"github.com/iamangus/code-mcp/internal/manager"
	"github.com/iamangus/code-mcp/internal/outbox"
	"github.com/iamangus/code-mcp/internal/pipeline"
	"github.com/iamangus/code-mcp/internal/references"
	"github.com/iamangus/code-mcp/internal/repositories"
	"github.com/iamangus/code-mcp/internal/repositorycatalog"
	"github.com/iamangus/code-mcp/internal/repositoryindex"
	"github.com/iamangus/code-mcp/internal/validation"
	"github.com/mark3labs/mcp-go/server"
)

func main() {
	var (
		mode        = flag.String("mode", "stdio", "Transport mode: stdio or http (single-server mode only)")
		addr        = flag.String("addr", ":8080", "HTTP listen address")
		dir         = flag.String("dir", "", "Single-server mode: absolute path to the worktree root directory")
		reposDir    = flag.String("repos-dir", "/repos", "Multi-server mode: directory containing all repositories")
		stateDir    = flag.String("state-dir", "/data", "Directory for durable coding job state")
		logFormat   = flag.String("log-format", "text", "log output format: text or json")
		logLevel    = flag.String("log-level", "info", "log level: debug, info, warn, error")
		healthCheck = flag.Bool("health-check", false, "Check authenticated local HTTP readiness and exit")
	)
	flag.Parse()
	if *healthCheck {
		_, port, err := net.SplitHostPort(*addr)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		request, requestErr := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+port+"/readyz", nil)
		if requestErr != nil {
			fmt.Fprintln(os.Stderr, requestErr)
			os.Exit(1)
		}
		request.Header.Set("Authorization", "Bearer "+os.Getenv("OPENDEV_TOKEN"))
		response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		return
	}

	var level slog.Level
	switch strings.ToLower(*logLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if *logFormat == "json" {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}
	logger := slog.New(handler)
	slog.SetDefault(logger)

	// ── Single-server (backward-compatible) mode ──────────────────────────
	if *dir != "" {
		runSingleServer(*mode, *addr, *dir, logger)
		return
	}

	// ── GitHub client (optional) ───────────────────────────────────────────
	// If GITHUB_TOKEN and GITHUB_OWNER are both set, a GitHub client is
	// constructed and passed to the multi-server so it can manage PRs.
	// The token is also passed to the manager so all git operations
	// (push/fetch) authenticate automatically.
	var ghClient githubpkg.Client
	githubToken := os.Getenv("GITHUB_TOKEN")
	if githubToken != "" {
		if owner := os.Getenv("GITHUB_OWNER"); owner != "" {
			var options []githubpkg.HTTPClientOption
			if os.Getenv("GITHUB_OWNER_TYPE") == "organization" {
				options = append(options, githubpkg.WithOrganizationOwner())
			}
			ghClient = githubpkg.NewHTTPClient(githubToken, owner, slog.Default(), options...)
			logger.Info("GitHub PR integration enabled", "owner", owner)
		} else {
			logger.Warn("GITHUB_TOKEN set but GITHUB_OWNER is missing, PR integration disabled")
		}
	}

	// ── Multi-server mode ──────────────────────────────────────────────────
	if err := runMultiServer(*addr, *reposDir, *stateDir, githubToken, ghClient, logger); err != nil {
		logger.Error("OpenDev stopped", "error", err)
		os.Exit(1)
	}
}

// runSingleServer starts profile-aware MCP servers for a single worktree.
// Each profile is served at /{profile}/mcp on the same HTTP address.
// In stdio mode only the read profile is served (stdio is single-stream).
func runSingleServer(mode, addr, dir string, logger *slog.Logger) {
	if mode != "stdio" && mode != "http" {
		logger.Error("invalid transport mode", "mode", mode)
		os.Exit(1)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "error: --dir %q does not exist or is not a directory\n", dir)
		os.Exit(1)
	}

	switch mode {
	case "http":
		token := strings.TrimSpace(os.Getenv("OPENDEV_TOKEN"))
		if token == "" {
			logger.Error("OPENDEV_TOKEN is required for HTTP mode")
			os.Exit(1)
		}
		mux := http.NewServeMux()
		registerHealth(mux)
		state := workspaceStateFor(dir, logger)
		for _, p := range Profiles {
			h := newMCPHandlerWithState(p, dir, logger, state)
			pattern := "/" + string(p) + "/mcp"
			mux.Handle(pattern, h)
			logger.Info("registered MCP handler", "profile", p, "dir", dir)
		}
		logger.Info("starting HTTP MCP server", "addr", addr, "dir", dir)
		if err := serveHTTP(addr, observeHTTP(logger, requireMCPToken(token, mux))); err != nil {
			fmt.Fprintf(os.Stderr, "HTTP server error: %v\n", err)
			os.Exit(1)
		}
	default:
		state := workspaceStateFor(dir, logger)
		s := server.NewMCPServer("opendev", "1.0.0", server.WithToolCapabilities(true))
		registerReadTools(s, state.locks, state.revisions, dir, logger)
		if err := server.ServeStdio(s); err != nil {
			fmt.Fprintf(os.Stderr, "stdio server error: %v\n", err)
			os.Exit(1)
		}
	}
}

// runMultiServer starts the multi-repo HTTP server.
//
// MCP endpoint layout:  http://host:port/{repo}/{branch}/{profile}/mcp
// Job control MCP:      http://host:port/mcp
func runMultiServer(addr, reposDir, stateDir, githubToken string, ghClient githubpkg.Client, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	mcpToken := strings.TrimSpace(os.Getenv("OPENDEV_TOKEN"))
	if mcpToken == "" {
		logger.Error("OPENDEV_TOKEN is required; refusing to expose repository and job MCP tools unauthenticated")
		return fmt.Errorf("OPENDEV_TOKEN is required")
	}
	stateLock, err := durable.Acquire(stateDir)
	if err != nil {
		logger.Error("acquire state directory", "error", err)
		return err
	}
	defer stateLock.Close()
	gitOps := gitops.NewExec(slog.Default(), githubToken)
	mgr, err := manager.New(reposDir, gitOps, slog.Default(), ctx)
	if err != nil {
		logger.Error("manager initialization failed", "error", err)
		return err
	}
	if ghClient != nil {
		syncOwnedRepositories(ctx, ghClient, mgr, logger)
	}

	var mu sync.RWMutex
	handlers := make(map[string]http.Handler)
	referenceHandlers := make(map[string]http.Handler)

	addHandlers := func(repo, branch, dir string) {
		mu.Lock()
		defer mu.Unlock()
		for _, p := range Profiles {
			if branch == "" {
				continue
			} // detached reference snapshots have their own routes
			if info, err := os.Stat(filepath.Join(dir, ".git")); err == nil && info.IsDir() && p == ProfileWrite {
				continue
			}
			key := repo + "/" + branch + "/" + string(p)
			if _, exists := handlers[key]; exists {
				continue
			}
			handlers[key] = newMCPHandler(p, dir, logger)
			logger.Info("registered MCP handler", "repo", repo, "branch", branch, "profile", p, "dir", dir)
		}
	}

	repos, err := mgr.Scan()
	if err != nil {
		logger.Error("scanning repos failed", "error", err)
		return err
	}
	catalog, err := repositorycatalog.New(stateDir, managerCatalogReader{manager: mgr})
	if err != nil {
		logger.Error("repository catalog initialization failed", "error", err)
		return err
	}
	for _, repo := range repos {
		if _, err := catalog.Refresh(context.Background(), repo); err != nil {
			logger.Warn("catalog refresh failed", "repository", repo.Name, "error", err)
		}
	}
	graphServer, err := initializeRepositoryGraph(ctx, stateDir, mgr, catalog, repos, logger)
	if err != nil {
		logger.Warn("repository graph disabled", "error", err)
	}
	var repositoryService *repositories.Service
	if ghClient != nil {
		owners := strings.Split(os.Getenv("OPENDEV_OWNED_GITHUB_ACCOUNTS"), ",")
		if len(owners) == 1 && strings.TrimSpace(owners[0]) == "" {
			owners = []string{os.Getenv("GITHUB_OWNER")}
		}
		repositoryService, err = repositories.New(mgr, catalog, ghClient, owners...)
		if err != nil {
			logger.Error("repository provisioning initialization failed", "error", err)
			return err
		}
	}
	referenceService, err := references.New(catalog, mgr)
	if err != nil {
		logger.Error("reference snapshot initialization failed", "error", err)
		return err
	}
	for _, repo := range repos {
		for _, b := range repo.Branches {
			if strings.HasPrefix(filepath.Clean(b.Dir), filepath.Join(repo.Dir, ".opendev")+string(filepath.Separator)) {
				continue
			}
			addHandlers(repo.Name, b.Name, b.Dir)
		}
	}
	logger.Info("startup: repos discovered", "count", len(repos), "dir", reposDir)

	mcpMux := http.NewServeMux()
	registerHealth(mcpMux)
	jobStore, err := pipeline.NewStore(stateDir)
	if err != nil {
		logger.Error("pipeline state initialization failed", "error", err)
		return err
	}
	for _, job := range jobStore.List() {
		if branches, err := mgr.ListBranches(job.Repository); err == nil {
			for _, branch := range branches {
				if branch.Name == job.TargetBranch || branch.Name == job.IntegrationBranch || strings.HasPrefix(branch.Name, "opendev-task-"+job.ID+"-") {
					addHandlers(job.Repository, branch.Name, branch.Dir)
				}
			}
		}
		for _, snapshot := range job.ReferenceSnapshots {
			referenceHandlers[job.ID+"/"+snapshot.Repository] = newMCPHandler(ProfileRead, snapshot.Path, logger)
		}
	}
	notificationOutbox, err := outbox.New(stateDir)
	if err != nil {
		logger.Error("notification outbox initialization failed", "error", err)
		return err
	}
	registerMetrics(mcpMux, jobStore, notificationOutbox)
	if err := notificationOutbox.Reconcile(jobStore.List()); err != nil {
		logger.Warn("notification outbox reconciliation failed", "error", err)
	}
	eventURL, eventToken := strings.TrimSpace(os.Getenv("EVE_URL")), strings.TrimSpace(os.Getenv("EVE_WEBHOOK_TOKEN"))
	if eventURL == "" || eventToken == "" {
		logger.Info("Eve notifications disabled; set EVE_URL and EVE_WEBHOOK_TOKEN to enable delivery")
	} else {
		outbox.NewDeliverer(notificationOutbox, eventURL, eventToken, logger, jobStore).Start(ctx)
	}
	runStore, err := dispatchstore.New(stateDir)
	if err != nil {
		logger.Error("dispatch state initialization failed", "error", err)
		return err
	}
	jobDispatcher := &lazyDispatcher{
		runStore: runStore,
		mcpURL:   opendevURL(addr),
		mcpToken: mcpToken,
		logger:   logger,
		jobStore: jobStore,
		registerRoutes: func(job *pipeline.Job) error {
			branches, err := mgr.ListBranches(job.Repository)
			if err != nil {
				return err
			}
			foundTarget := false
			wanted := map[string]bool{job.TargetBranch: true, job.IntegrationBranch: true}
			if job.Plan != nil {
				for _, task := range job.Plan.Tasks {
					wanted[task.Branch] = true
					wanted[jobmcp.WriterBranch(job.ID, &task)] = true
				}
			}
			for _, b := range branches {
				if b.Name == job.TargetBranch {
					foundTarget = true
				}
				if wanted[b.Name] {
					addHandlers(job.Repository, b.Name, b.Dir)
				}
			}
			if !foundTarget {
				dir, err := mgr.CreateWorktree(job.Repository, job.TargetBranch, "")
				if err != nil {
					return err
				}
				addHandlers(job.Repository, job.TargetBranch, dir)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, snapshot := range job.ReferenceSnapshots {
				key := job.ID + "/" + snapshot.Repository
				if _, ok := referenceHandlers[key]; !ok {
					referenceHandlers[key] = newMCPHandler(ProfileRead, snapshot.Path, logger)
				}
			}
			return nil
		},
	}
	defer jobDispatcher.Close()
	jobController := pipeline.NewController(jobStore)
	jobMCPConfig := jobmcp.Config{
		Store:      jobStore,
		Controller: jobController,
		Dispatcher: jobDispatcher,
		DispatchReady: func(ctx context.Context) error {
			_, err := jobDispatcher.get(ctx)
			return err
		},
		Worktrees: mgr,
		Registrar: worktreeRegistrar{worktree: addHandlers, reference: func(jobID, repository, directory string) {
			mu.Lock()
			defer mu.Unlock()
			referenceHandlers[jobID+"/"+repository] = newMCPHandler(ProfileRead, directory, logger)
		}, unregister: func(job *pipeline.Job) {
			mu.Lock()
			defer mu.Unlock()
			branches := []string{job.IntegrationBranch}
			paths := []string{mgr.BranchWorktreeDir(job.Repository, job.IntegrationBranch)}
			if job.Plan != nil {
				for _, task := range job.Plan.Tasks {
					branches = append(branches, task.Branch)
					paths = append(paths, task.WorktreePath)
				}
			}
			for _, branch := range branches {
				for _, profile := range Profiles {
					delete(handlers, job.Repository+"/"+branch+"/"+string(profile))
				}
			}
			for _, snapshot := range job.ReferenceSnapshots {
				delete(referenceHandlers, job.ID+"/"+snapshot.Repository)
				paths = append(paths, snapshot.Path)
			}
			for _, path := range paths {
				if path != "" {
					if absolute, err := filepath.Abs(path); err == nil {
						workspaceStates.Delete(absolute)
					}
				}
			}
		}},
		GitHub:       ghClient,
		Repositories: repositoryService,
		References:   referenceService,
		Validation:   validationRunner{},
		AllowNoChecks: strings.EqualFold(
			strings.TrimSpace(os.Getenv("OPENDEV_ALLOW_EMPTY_PR_CHECKS")), "true",
		),
		Logger: logger,
		Outbox: notificationOutbox,
		WorktreeToolFailures: func(worktreePath string, since time.Time) []jobmcp.ToolFailureInfo {
			state := workspaceStateFor(worktreePath, logger)
			recorded := state.FailuresSince(since)
			info := make([]jobmcp.ToolFailureInfo, 0, len(recorded))
			for _, f := range recorded {
				info = append(info, jobmcp.ToolFailureInfo{Time: f.Time, Tool: f.Tool, Error: f.Error})
			}
			return info
		},
	}
	// Agent outcomes are applied only by the dispatcher after its terminal
	// response has been durably stored; role MCP endpoints are inspection-only.
	jobDispatcher.outcomeHandler = jobmcp.OutcomeHandler(jobMCPConfig)
	mcpMux.Handle("/mcp", jobmcp.New(jobMCPConfig))
	if graphServer != nil {
		mcpMux.Handle("/mcp/repository-graph", graphServer)
	}
	mcpMux.HandleFunc("/references/{job}/{repository}/read/mcp", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("job") + "/" + r.PathValue("repository")
		mu.RLock()
		h, ok := referenceHandlers[key]
		mu.RUnlock()
		if !ok {
			http.Error(w, "reference snapshot not found", http.StatusNotFound)
			return
		}
		h.ServeHTTP(w, r)
	})
	for _, role := range []jobmcp.Role{jobmcp.RolePlanner, jobmcp.RoleWriter, jobmcp.RoleReviewer, jobmcp.RoleHolistic} {
		mcpMux.Handle("/mcp/"+string(role), jobmcp.NewRole(jobMCPConfig, role))
	}
	mcpMux.HandleFunc("/{repo}/{branch}/{profile}/mcp", func(w http.ResponseWriter, r *http.Request) {
		repo := r.PathValue("repo")
		branch := r.PathValue("branch")
		profile := r.PathValue("profile")
		key := repo + "/" + branch + "/" + profile
		mu.RLock()
		h, ok := handlers[key]
		mu.RUnlock()
		if !ok {
			http.Error(w, fmt.Sprintf("no MCP server for %s/%s/%s", repo, branch, profile), http.StatusNotFound)
			return
		}
		h.ServeHTTP(w, r)
	})

	// Repository lifecycle is exposed through the authenticated MCP endpoint.
	top := observeHTTP(logger, requireMCPToken(mcpToken, mcpMux))

	// Reconciliation can launch agent runs with MCP attachments. Accept those
	// connections before dispatching anything from durable state.
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Error("server failed to listen", "error", err)
		return err
	}
	logger.Info("starting multi-server", "addr", addr, "repos_dir", reposDir)
	httpServer := configuredHTTPServer(top)
	serverErrors := make(chan error, 1)
	go func() {
		if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server failed", "error", err)
			serverErrors <- err
		}
	}()
	if _, err := jobDispatcher.get(ctx); err != nil {
		logger.Error("startup dispatch reconciliation failed", "error", err)
	}
	observeCI := func() {
		if inner, err := jobDispatcher.get(ctx); err == nil {
			if err := inner.Reconcile(ctx); err != nil {
				logger.Error("reconcile dispatches", "error", err)
			}
		}
		if repositoryService != nil {
			if err := repositoryService.ReconcilePendingFoundations(ctx); err != nil {
				logger.Error("reconcile repository foundations", "error", err)
			}
		}
		if err := jobmcp.ResumeFoundationPlanning(ctx, jobMCPConfig); err != nil {
			logger.Error("resume foundation planning", "error", err)
		}
		if err := jobmcp.ReconcileJobs(ctx, jobMCPConfig); err != nil {
			logger.Error("reconcile job progression", "error", err)
		}
		if err := jobmcp.ObservePendingCI(ctx, jobMCPConfig); err != nil {
			logger.Error("observe draft PR CI", "error", err)
		}
	}
	observeCI()
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				observeCI()
			}
		}
	}()
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-serverErrors:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	return serveErr
}

// syncOwnedRepositories gives a standalone OpenDev installation its own local
// mirrors. Failures are logged per repository so one inaccessible repository
// never prevents already-synced jobs from running.
type repositorySyncer interface {
	SyncRepo(repoURL, name string, expectedDefaultBranch ...string) error
}

func syncOwnedRepositories(ctx context.Context, ghClient githubpkg.Client, syncer repositorySyncer, logger *slog.Logger) {
	repos, err := ghClient.ListOwnedRepositories(ctx)
	if err != nil {
		logger.Warn("owned repository bootstrap failed", "error", err)
		return
	}
	for _, repo := range repos {
		if strings.TrimSpace(repo.Name) == "" || strings.TrimSpace(repo.CloneURL) == "" {
			logger.Warn("owned repository bootstrap skipped incomplete repository metadata", "repository", repo.FullName)
			continue
		}
		if err := syncer.SyncRepo(repo.CloneURL, repo.Name, repo.DefaultBranch); err != nil {
			logger.Warn("owned repository sync failed", "repository", repo.Name, "error", err)
		}
	}
	logger.Info("owned repository bootstrap complete", "count", len(repos))
}

// initializeRepositoryGraph refreshes the local catalog and indexes only clean,
// committed repository evidence. Graph failures do not prevent code jobs.
func initializeRepositoryGraph(ctx context.Context, stateDir string, mgr *manager.Manager, catalog *repositorycatalog.Catalog, repos []manager.RepoInfo, logger *slog.Logger) (*server.StreamableHTTPServer, error) {
	graphURL := strings.TrimSpace(os.Getenv("GRAPHITI_URL"))
	if graphURL == "" {
		return nil, fmt.Errorf("GRAPHITI_URL is not configured")
	}
	client, err := graphiti.NewClient(graphURL, "opendev-repositories")
	if err != nil {
		return nil, err
	}
	indexStore, err := repositoryindex.New(stateDir)
	if err != nil {
		return nil, err
	}
	indexer, err := repositoryindex.NewIndexer(indexStore, client)
	if err != nil {
		return nil, err
	}
	refresh := func(repos []manager.RepoInfo) {
		for _, repo := range repos {
			if ctx.Err() != nil {
				return
			}
			record, err := catalog.Refresh(ctx, repo)
			if err != nil {
				logger.Warn("catalog lookup failed", "repository", repo.Name, "error", err)
				continue
			}
			if record == nil {
				logger.Warn("catalog record is missing", "repository", repo.Name)
				continue
			}
			status, err := mgr.Status(record.Path)
			if err != nil {
				logger.Warn("repository status check failed", "repository", repo.Name, "error", err)
				continue
			}
			if status != "" {
				logger.Info("repository graph indexing skipped for dirty checkout", "repository", repo.Name)
				continue
			}
			snapshot, err := repositorySnapshot(*record, func(path string) string {
				content, err := mgr.ReadFileAt(ctx, record.Path, record.HeadSHA, path)
				if err != nil {
					return ""
				}
				if len(content) > 16<<10 {
					content = content[:16<<10]
				}
				return content
			})
			if err != nil {
				logger.Warn("repository snapshot failed", "repository", repo.Name, "error", err)
				continue
			}
			if _, err := indexer.Index(ctx, snapshot); err != nil {
				logger.Warn("repository graph indexing failed", "repository", repo.Name, "error", err)
			}
		}
	}
	go func() {
		refresh(repos)
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				discovered, err := mgr.Scan()
				if err != nil {
					logger.Warn("graph repository scan failed", "error", err)
					continue
				}
				refresh(discovered)
			}
		}
	}()
	return graphmcp.New(graphmcp.Config{Searcher: client, State: indexStore}), nil
}

type managerCatalogReader struct{ manager *manager.Manager }

func (r managerCatalogReader) ReadGitMetadata(_ context.Context, dir string) (repositorycatalog.GitMetadata, error) {
	metadata, err := r.manager.RepositoryMetadata(filepath.Base(dir))
	if err != nil {
		return repositorycatalog.GitMetadata{}, err
	}
	return repositorycatalog.GitMetadata{OriginURL: metadata.OriginURL, HeadSHA: metadata.HeadSHA}, nil
}

func repositorySnapshot(record repositorycatalog.Record, read func(string) string) (repositoryindex.Snapshot, error) {
	manifests := make(map[string]string)
	for _, name := range []string{"go.mod", "package.json", "pyproject.toml", "Cargo.toml", "Dockerfile", "docker-compose.yml", "Chart.yaml", "kustomization.yaml"} {
		if content := read(name); content != "" {
			manifests[name] = content
		}
	}
	docs := make(map[string]string)
	for _, name := range []string{"ARCHITECTURE.md", "CONTRIBUTING.md", "docs/README.md"} {
		if content := read(name); content != "" {
			docs[name] = content
		}
	}
	return repositoryindex.Snapshot{Name: record.Name, Path: record.Path, SHA: record.HeadSHA, OriginURL: record.OriginURL, DefaultBranch: record.DefaultBranch, Epoch: strings.TrimSpace(os.Getenv("GRAPHITI_INDEX_EPOCH")), README: read("README.md"), Docs: docs, Manifests: manifests}, nil
}

type worktreeRegistrar struct {
	worktree   func(repository, branch, directory string)
	reference  func(jobID, repository, directory string)
	unregister func(*pipeline.Job)
}

func (r worktreeRegistrar) UnregisterJob(job *pipeline.Job) {
	if r.unregister != nil {
		r.unregister(job)
	}
}

type validationRunner struct{}

func (validationRunner) Run(ctx context.Context, worktreePath, name string) (validation.Result, error) {
	return validation.Run(ctx, worktreePath, name)
}

func (r worktreeRegistrar) RegisterWorktree(repository, branch, directory string) {
	r.worktree(repository, branch, directory)
}

func (r worktreeRegistrar) RegisterReference(jobID, repository, directory string) {
	r.reference(jobID, repository, directory)
}

// lazyDispatcher defers validating external AgentFoundry configuration until a
// tool actually requests dispatch, so repository management remains available.
type lazyDispatcher struct {
	runStore       *dispatchstore.Store
	mcpURL         string
	mcpToken       string
	logger         *slog.Logger
	jobStore       *pipeline.Store
	registerRoutes func(*pipeline.Job) error

	mu             sync.Mutex
	dispatcher     *dispatcher.Dispatcher
	outcomeHandler dispatcher.OutcomeHandler
}

func (d *lazyDispatcher) StartPlanner(ctx context.Context, job *pipeline.Job) (*dispatcher.DispatchRun, error) {
	inner, err := d.get(ctx)
	if err != nil {
		return nil, err
	}
	return inner.StartPlanner(ctx, job)
}

func (d *lazyDispatcher) StartPlannerRevision(ctx context.Context, job *pipeline.Job) (*dispatcher.DispatchRun, error) {
	inner, err := d.get(ctx)
	if err != nil {
		return nil, err
	}
	return inner.StartPlannerRevision(ctx, job)
}

func (d *lazyDispatcher) StartWriter(ctx context.Context, job *pipeline.Job, task *pipeline.Task) (*dispatcher.DispatchRun, error) {
	inner, err := d.get(ctx)
	if err != nil {
		return nil, err
	}
	return inner.StartWriter(ctx, job, task)
}

func (d *lazyDispatcher) StartReviewer(ctx context.Context, job *pipeline.Job, task *pipeline.Task) (*dispatcher.DispatchRun, error) {
	inner, err := d.get(ctx)
	if err != nil {
		return nil, err
	}
	return inner.StartReviewer(ctx, job, task)
}

func (d *lazyDispatcher) StartHolistic(ctx context.Context, job *pipeline.Job) (*dispatcher.DispatchRun, error) {
	inner, err := d.get(ctx)
	if err != nil {
		return nil, err
	}
	return inner.StartHolistic(ctx, job)
}

func (d *lazyDispatcher) Supersede(ctx context.Context, taskID, reason string) error {
	inner, err := d.get(ctx)
	if err != nil {
		return err
	}
	return inner.Supersede(ctx, taskID, reason)
}

func (d *lazyDispatcher) CancelJob(ctx context.Context, id string) error {
	runs, err := d.runStore.ListByJob(ctx, id)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		return nil
	}
	needsRemote := false
	for _, run := range runs {
		if !dispatcher.IsTerminal(run.Status) && run.RunID != "" {
			needsRemote = true
		}
	}
	if !needsRemote {
		for _, run := range runs {
			if run.OutcomeApplied {
				continue
			}
			if !dispatcher.IsTerminal(run.Status) {
				run.Status = "cancelled"
			}
			run.OutcomeApplied = true
			if err := d.runStore.Save(ctx, run); err != nil {
				return err
			}
		}
		return nil
	}
	inner, err := d.get(ctx)
	if err != nil {
		return err
	}
	return inner.CancelJob(ctx, id)
}

func (d *lazyDispatcher) ArchiveJob(ctx context.Context, id string) error {
	return d.runStore.ArchiveJob(id)
}

func (d *lazyDispatcher) InspectJob(ctx context.Context, jobID string) ([]dispatcher.DispatchRun, error) {
	return d.runStore.ListByJob(ctx, jobID)
}

func (d *lazyDispatcher) get(ctx context.Context) (*dispatcher.Dispatcher, error) {
	d.mu.Lock()
	if d.dispatcher != nil {
		inner := d.dispatcher
		d.mu.Unlock()
		return inner, nil
	}
	url := strings.TrimSpace(os.Getenv("AGENTFOUNDRY_URL"))
	if url == "" {
		d.mu.Unlock()
		return nil, fmt.Errorf("AGENTFOUNDRY_URL is required to dispatch an agent run")
	}
	client, err := agentfoundry.NewClient(url, os.Getenv("AGENTFOUNDRY_API_KEY"))
	if err != nil {
		d.mu.Unlock()
		return nil, fmt.Errorf("configure AgentFoundry dispatcher: %w", err)
	}
	inner, err := dispatcher.New(client, d.runStore, dispatcher.Config{Logger: d.logger, OutcomeHandler: d.outcomeHandler, MCPServersForRun: d.serversForRun})
	if err != nil {
		d.mu.Unlock()
		return nil, err
	}
	d.dispatcher = inner
	d.mu.Unlock()
	return inner, nil
}

func (d *lazyDispatcher) serversForRun(ctx context.Context, run dispatcher.DispatchRun) ([]agentfoundry.MCPServer, error) {
	job, err := d.jobStore.Get(run.JobID)
	if err != nil {
		return nil, err
	}
	if pipeline.Terminal(job.Status) || job.Status == pipeline.JobPaused {
		return nil, fmt.Errorf("job %s does not permit agent dispatch", job.Status)
	}
	if d.registerRoutes != nil {
		if err := d.registerRoutes(job); err != nil {
			return nil, err
		}
	}
	branch := job.TargetBranch
	writer := run.Role == dispatcher.RoleWriter
	if run.Role == dispatcher.RoleHolistic || run.TaskKey == "revision" {
		branch = job.IntegrationBranch
	}
	if run.Role == dispatcher.RoleWriter || run.Role == dispatcher.RoleReviewer {
		branch = jobmcp.TaskBranch(job.ID, run.TaskKey)
		if job.Plan == nil {
			return nil, fmt.Errorf("task dispatch has no plan")
		}
		for _, task := range job.Plan.Tasks {
			if task.Key == run.TaskKey {
				if writer {
					branch = jobmcp.WriterBranch(job.ID, &task)
				} else if task.Branch != "" {
					branch = task.Branch
				}
			}
		}
	}
	makeServer := func(kind, endpoint string) agentfoundry.MCPServer {
		return agentfoundry.MCPServer{Name: ephemeralMCPServerName(job.ID, run.Role, run.TaskKey, run.Attempt, kind), URL: endpoint, Transport: "streamable-http", Headers: d.mcpHeaders()}
	}
	servers := []agentfoundry.MCPServer{makeServer("jobs", d.roleMCPURL(run.Role)), makeServer("read", d.worktreeMCPURL(job.Repository, branch, "read"))}
	if writer {
		servers = append(servers, makeServer("write", d.worktreeMCPURL(job.Repository, branch, "write")))
	}
	for _, snapshot := range job.ReferenceSnapshots {
		servers = append(servers, makeServer("reference-"+snapshot.Repository, d.referenceMCPURL(job.ID, snapshot.Repository)))
	}
	return servers, nil
}

func (d *lazyDispatcher) ExecutionTrace(ctx context.Context, run dispatcher.DispatchRun) (json.RawMessage, error) {
	inner, err := d.get(ctx)
	if err != nil {
		return nil, err
	}
	return inner.ExecutionTrace(ctx, run)
}

func (d *lazyDispatcher) Close() {
	d.mu.Lock()
	inner := d.dispatcher
	d.mu.Unlock()
	if inner != nil {
		inner.Close()
	}
}

func (d *lazyDispatcher) mcpHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer " + d.mcpToken}
}

func (d *lazyDispatcher) roleMCPURL(role string) string {
	return strings.TrimRight(d.mcpURL, "/") + "/" + role
}

func ephemeralMCPServerName(jobID, role, taskKey string, attempt int, kind string) string {
	return fmt.Sprintf("opendev-%s-%s-%s-%d-%s", safeMCPName(jobID), safeMCPName(role), safeMCPName(taskKey), attempt, safeMCPName(kind))
}

func safeMCPName(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func (d *lazyDispatcher) worktreeMCPURL(repository, branch, profile string) string {
	base := strings.TrimSuffix(strings.TrimRight(d.mcpURL, "/"), "/mcp")
	return base + "/" + url.PathEscape(repository) + "/" + url.PathEscape(branch) + "/" + profile + "/mcp"
}

func (d *lazyDispatcher) referenceMCPURL(jobID, repository string) string {
	base := strings.TrimSuffix(strings.TrimRight(d.mcpURL, "/"), "/mcp")
	return base + "/references/" + jobID + "/" + repository + "/read/mcp"
}

func opendevURL(addr string) string {
	if configured := strings.TrimSpace(os.Getenv("OPENDEV_URL")); configured != "" {
		return configured
	}
	if strings.HasPrefix(addr, ":") {
		return "http://localhost" + addr + "/mcp"
	}
	return "http://" + addr + "/mcp"
}

func requireMCPToken(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !bearer || token == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func configuredHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{Handler: http.MaxBytesHandler(handler, 8<<20), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 32 << 10}
}

func registerHealth(mux *http.ServeMux) {
	for _, path := range []string{"/healthz", "/readyz"} {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ok"}`)
		})
	}
}

func serveHTTP(addr string, handler http.Handler) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	s := configuredHTTPServer(handler)
	s.Addr = addr
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.Shutdown(shutdown)
	}()
	err := s.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
