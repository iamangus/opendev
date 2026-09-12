package main

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/iamangus/code-mcp/internal/agentfoundry"
	"github.com/iamangus/code-mcp/internal/dispatcher"
	"github.com/iamangus/code-mcp/internal/dispatchstore"
	githubpkg "github.com/iamangus/code-mcp/internal/github"
	"github.com/iamangus/code-mcp/internal/gitops"
	"github.com/iamangus/code-mcp/internal/graphiti"
	"github.com/iamangus/code-mcp/internal/graphmcp"
	"github.com/iamangus/code-mcp/internal/jobmcp"
	"github.com/iamangus/code-mcp/internal/locks"
	"github.com/iamangus/code-mcp/internal/manager"
	"github.com/iamangus/code-mcp/internal/pipeline"
	"github.com/iamangus/code-mcp/internal/references"
	"github.com/iamangus/code-mcp/internal/repositories"
	"github.com/iamangus/code-mcp/internal/repositorycatalog"
	"github.com/iamangus/code-mcp/internal/repositoryindex"
	"github.com/mark3labs/mcp-go/server"
)

func main() {
	var (
		mode      = flag.String("mode", "stdio", "Transport mode: stdio or http (single-server mode only)")
		addr      = flag.String("addr", ":8080", "HTTP listen address")
		dir       = flag.String("dir", "", "Single-server mode: absolute path to the worktree root directory")
		reposDir  = flag.String("repos-dir", "/repos", "Multi-server mode: directory containing all repositories")
		stateDir  = flag.String("state-dir", "/data", "Directory for durable coding job state")
		logFormat = flag.String("log-format", "text", "log output format: text or json")
		logLevel  = flag.String("log-level", "info", "log level: debug, info, warn, error")
	)
	flag.Parse()

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
			ghClient = githubpkg.NewHTTPClient(githubToken, owner, slog.Default())
			logger.Info("GitHub PR integration enabled", "owner", owner)
		} else {
			logger.Warn("GITHUB_TOKEN set but GITHUB_OWNER is missing, PR integration disabled")
		}
	}

	// ── Multi-server mode ──────────────────────────────────────────────────
	runMultiServer(*addr, *reposDir, *stateDir, githubToken, ghClient, logger)
}

// runSingleServer starts profile-aware MCP servers for a single worktree.
// Each profile is served at /{profile}/mcp on the same HTTP address.
// In stdio mode only the read profile is served (stdio is single-stream).
func runSingleServer(mode, addr, dir string, logger *slog.Logger) {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "error: --dir %q does not exist or is not a directory\n", dir)
		os.Exit(1)
	}

	switch mode {
	case "http":
		mux := http.NewServeMux()
		for _, p := range Profiles {
			h := newMCPHandler(p, dir, logger)
			pattern := "/" + string(p) + "/mcp"
			mux.Handle(pattern, h)
			logger.Info("registered MCP handler", "profile", p, "dir", dir)
		}
		logger.Info("starting HTTP MCP server", "addr", addr, "dir", dir)
		if err := http.ListenAndServe(addr, mux); err != nil {
			fmt.Fprintf(os.Stderr, "HTTP server error: %v\n", err)
			os.Exit(1)
		}
	default:
		lm := locks.NewManager(slog.Default())
		s := server.NewMCPServer("opendev", "1.0.0", server.WithToolCapabilities(true))
		registerReadTools(s, lm, dir, logger)
		if err := server.ServeStdio(s); err != nil {
			fmt.Fprintf(os.Stderr, "stdio server error: %v\n", err)
			os.Exit(1)
		}
	}
}

// runMultiServer starts the multi-repo HTTP server.
//
// MCP endpoint layout:  http://host:port/{repo}/{branch}/{profile}/mcp
// Management API:       http://host:port/api/repos[/...]
func runMultiServer(addr, reposDir, stateDir, githubToken string, ghClient githubpkg.Client, logger *slog.Logger) {
	mcpToken := strings.TrimSpace(os.Getenv("OPENDEV_TOKEN"))
	if mcpToken == "" {
		logger.Error("OPENDEV_TOKEN is required; refusing to expose repository and job MCP tools unauthenticated")
		return
	}
	gitOps := gitops.NewExec(slog.Default(), githubToken)
	mgr, err := manager.New(reposDir, gitOps, slog.Default())
	if err != nil {
		logger.Error("manager initialization failed", "error", err)
		os.Exit(1)
	}
	if ghClient != nil {
		syncOwnedRepositories(context.Background(), ghClient, mgr, logger)
	}

	var mu sync.RWMutex
	handlers := make(map[string]http.Handler)
	referenceHandlers := make(map[string]http.Handler)

	addHandlers := func(repo, branch, dir string) {
		mu.Lock()
		defer mu.Unlock()
		for _, p := range Profiles {
			key := repo + "/" + branch + "/" + string(p)
			handlers[key] = newMCPHandler(p, dir, logger)
			logger.Info("registered MCP handler", "repo", repo, "branch", branch, "profile", p, "dir", dir)
		}
	}

	repos, err := mgr.Scan()
	if err != nil {
		logger.Error("scanning repos failed", "error", err)
		os.Exit(1)
	}
	catalog, err := repositorycatalog.New(stateDir, managerCatalogReader{manager: mgr})
	if err != nil {
		logger.Error("repository catalog initialization failed", "error", err)
		return
	}
	for _, repo := range repos {
		if _, err := catalog.Refresh(context.Background(), repo); err != nil {
			logger.Warn("catalog refresh failed", "repository", repo.Name, "error", err)
		}
	}
	graphServer, err := initializeRepositoryGraph(context.Background(), stateDir, mgr, catalog, repos, logger)
	if err != nil {
		logger.Warn("repository graph disabled", "error", err)
	}
	var repositoryService *repositories.Service
	if ghClient != nil {
		repositoryService, err = repositories.New(mgr, catalog, ghClient)
		if err != nil {
			logger.Error("repository provisioning initialization failed", "error", err)
			return
		}
	}
	referenceService, err := references.New(catalog, mgr)
	if err != nil {
		logger.Error("reference snapshot initialization failed", "error", err)
		return
	}
	for _, repo := range repos {
		for _, b := range repo.Branches {
			addHandlers(repo.Name, b.Name, b.Dir)
		}
	}
	logger.Info("startup: repos discovered", "count", len(repos), "dir", reposDir)

	mcpMux := http.NewServeMux()
	jobStore, err := pipeline.NewStore(stateDir)
	if err != nil {
		logger.Error("pipeline state initialization failed", "error", err)
		return
	}
	runStore, err := dispatchstore.New(stateDir)
	if err != nil {
		logger.Error("dispatch state initialization failed", "error", err)
		return
	}
	jobDispatcher := &lazyDispatcher{
		runStore: runStore,
		mcpURL:   opendevURL(addr),
		mcpToken: mcpToken,
		logger:   logger,
	}
	jobMCPConfig := jobmcp.Config{
		Store:      jobStore,
		Controller: pipeline.NewController(jobStore),
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
		}},
		GitHub:       ghClient,
		Repositories: repositoryService,
		References:   referenceService,
		AllowNoChecks: strings.EqualFold(
			strings.TrimSpace(os.Getenv("OPENDEV_ALLOW_EMPTY_PR_CHECKS")), "true",
		),
		Logger: logger,
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

	// Repository lifecycle is MCP-only. The legacy unauthenticated REST API is
	// intentionally left unregistered.
	top := requireMCPToken(mcpToken, mcpMux)

	// Reconciliation can launch agent runs with MCP attachments. Accept those
	// connections before dispatching anything from durable state.
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Error("server failed to listen", "error", err)
		return
	}
	logger.Info("starting multi-server", "addr", addr, "repos_dir", reposDir)
	go func() {
		if err := http.Serve(listener, top); err != nil {
			logger.Error("server failed", "error", err)
		}
	}()
	if _, err := jobDispatcher.get(context.Background()); err != nil {
		logger.Error("startup dispatch reconciliation failed", "error", err)
	}
	select {}
}

// syncOwnedRepositories gives a standalone OpenDev installation its own local
// mirrors. Failures are logged per repository so one inaccessible repository
// never prevents already-synced jobs from running.
type repositorySyncer interface {
	SyncRepo(repoURL, name string) error
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
		if err := syncer.SyncRepo(repo.CloneURL, repo.Name); err != nil {
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
	for _, repo := range repos {
		record, err := catalog.Get(repo.Name)
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
		snapshot, err := repositorySnapshot(*record)
		if err != nil {
			logger.Warn("repository snapshot failed", "repository", repo.Name, "error", err)
			continue
		}
		if _, err := indexer.Index(ctx, snapshot); err != nil {
			logger.Warn("repository graph indexing failed", "repository", repo.Name, "error", err)
		}
	}
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

func repositorySnapshot(record repositorycatalog.Record) (repositoryindex.Snapshot, error) {
	read := func(path string) string { return readEvidenceFile(filepath.Join(record.Path, path)) }
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

func readEvidenceFile(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 16*1024))
	if err != nil {
		return ""
	}
	return string(data)
}

type worktreeRegistrar struct {
	worktree  func(repository, branch, directory string)
	reference func(jobID, repository, directory string)
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
	runStore *dispatchstore.Store
	mcpURL   string
	mcpToken string
	logger   *slog.Logger

	mu             sync.Mutex
	runner         *agentfoundry.Client
	dispatcher     *dispatcher.Dispatcher
	workers        []*dispatcher.Dispatcher
	outcomeHandler dispatcher.OutcomeHandler
}

func (d *lazyDispatcher) StartPlanner(ctx context.Context, job *pipeline.Job) (*dispatcher.DispatchRun, error) {
	inner, err := d.withRoleServers(ctx, job, dispatcher.RolePlanner, "job", 1, job.TargetBranch, false)
	if err != nil {
		return nil, err
	}
	return inner.StartPlanner(ctx, job)
}

func (d *lazyDispatcher) StartWriter(ctx context.Context, job *pipeline.Job, task *pipeline.Task) (*dispatcher.DispatchRun, error) {
	inner, err := d.withRoleServers(ctx, job, dispatcher.RoleWriter, task.Key, task.WriterAttempts+1, task.Branch, true)
	if err != nil {
		return nil, err
	}
	return inner.StartWriter(ctx, job, task)
}

func (d *lazyDispatcher) StartReviewer(ctx context.Context, job *pipeline.Job, task *pipeline.Task) (*dispatcher.DispatchRun, error) {
	inner, err := d.withRoleServers(ctx, job, dispatcher.RoleReviewer, task.Key, task.ReviewerAttempts+1, task.Branch, false)
	if err != nil {
		return nil, err
	}
	return inner.StartReviewer(ctx, job, task)
}

func (d *lazyDispatcher) StartHolistic(ctx context.Context, job *pipeline.Job) (*dispatcher.DispatchRun, error) {
	inner, err := d.withRoleServers(ctx, job, dispatcher.RoleHolistic, "job", 1, safeMCPBranch(job.IntegrationBranch), false)
	if err != nil {
		return nil, err
	}
	return inner.StartHolistic(ctx, job)
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
	inner, err := dispatcher.New(client, d.runStore, dispatcher.Config{Logger: d.logger, OutcomeHandler: d.outcomeHandler})
	if err != nil {
		d.mu.Unlock()
		return nil, err
	}
	d.dispatcher = inner
	d.runner = client
	d.mu.Unlock()
	if err := inner.Reconcile(ctx); err != nil {
		return nil, fmt.Errorf("reconcile AgentFoundry runs: %w", err)
	}
	return inner, nil
}

func (d *lazyDispatcher) withRoleServers(ctx context.Context, job *pipeline.Job, role, taskKey string, attempt int, branch string, writer bool) (*dispatcher.Dispatcher, error) {
	if _, err := d.get(ctx); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	servers := []agentfoundry.MCPServer{{Name: ephemeralMCPServerName(job.ID, role, taskKey, attempt, "jobs"), URL: d.roleMCPURL(role), Transport: "streamable-http", Headers: d.mcpHeaders()}}
	if branch != "" {
		servers = append(servers, agentfoundry.MCPServer{Name: ephemeralMCPServerName(job.ID, role, taskKey, attempt, "read"), URL: d.worktreeMCPURL(job.Repository, branch, "read"), Transport: "streamable-http", Headers: d.mcpHeaders()})
	}
	if writer {
		servers = append(servers, agentfoundry.MCPServer{Name: ephemeralMCPServerName(job.ID, role, taskKey, attempt, "write"), URL: d.worktreeMCPURL(job.Repository, branch, "write"), Transport: "streamable-http", Headers: d.mcpHeaders()})
	}
	for _, snapshot := range job.ReferenceSnapshots {
		servers = append(servers, agentfoundry.MCPServer{Name: ephemeralMCPServerName(job.ID, role, taskKey, attempt, "reference-"+snapshot.Repository), URL: d.referenceMCPURL(job.ID, snapshot.Repository), Transport: "streamable-http", Headers: d.mcpHeaders()})
	}
	inner, err := dispatcher.New(d.runner, d.runStore, dispatcher.Config{MCPServers: servers, Logger: d.logger, OutcomeHandler: d.outcomeHandler})
	if err != nil {
		return nil, err
	}
	// Keep the dispatcher alive so its run-status watcher remains active.
	d.workers = append(d.workers, inner)
	return inner, nil
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
	return base + "/" + repository + "/" + safeMCPBranch(branch) + "/" + profile + "/mcp"
}

func (d *lazyDispatcher) referenceMCPURL(jobID, repository string) string {
	base := strings.TrimSuffix(strings.TrimRight(d.mcpURL, "/"), "/mcp")
	return base + "/references/" + jobID + "/" + repository + "/read/mcp"
}

func safeMCPBranch(branch string) string {
	return strings.NewReplacer("/", "-", "\\", "-").Replace(branch)
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
		provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
