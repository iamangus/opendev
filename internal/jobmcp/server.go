// Package jobmcp exposes the durable, high-level coding job control plane.
package jobmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/iamangus/code-mcp/internal/dispatcher"
	githubpkg "github.com/iamangus/code-mcp/internal/github"
	"github.com/iamangus/code-mcp/internal/outbox"
	"github.com/iamangus/code-mcp/internal/pipeline"
	"github.com/iamangus/code-mcp/internal/references"
	"github.com/iamangus/code-mcp/internal/repositories"
	"github.com/iamangus/code-mcp/internal/validation"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type Dispatcher interface {
	StartPlanner(context.Context, *pipeline.Job) (*dispatcher.DispatchRun, error)
	StartWriter(context.Context, *pipeline.Job, *pipeline.Task) (*dispatcher.DispatchRun, error)
	StartReviewer(context.Context, *pipeline.Job, *pipeline.Task) (*dispatcher.DispatchRun, error)
	StartHolistic(context.Context, *pipeline.Job) (*dispatcher.DispatchRun, error)
	Supersede(context.Context, string, string) error
	InspectJob(context.Context, string) ([]dispatcher.DispatchRun, error)
}

type executionTracer interface {
	ExecutionTrace(context.Context, dispatcher.DispatchRun) (json.RawMessage, error)
}

type Worktrees interface {
	CreateWorktree(repository, branch, base string) (string, error)
	Commit(worktreePath string) (string, error)
	HasChanges(worktreePath string) (bool, error)
	MergeTask(repository, sourceBranch, targetBranch string) (string, error)
	HeadCommit(worktreePath string) (string, error)
	DiffRange(worktreePath, base, target string) (string, error)
}

// Registrar is deliberately narrow so job orchestration cannot alter HTTP routing.
type Registrar interface {
	RegisterWorktree(repository, branch, directory string)
	RegisterReference(jobID, repository, directory string)
}

// ToolFailureInfo describes one recorded workspace tool failure.
type ToolFailureInfo struct {
	Time  time.Time `json:"time"`
	Tool  string    `json:"tool"`
	Error string    `json:"error"`
}

type Config struct {
	Store         *pipeline.Store
	Controller    *pipeline.Controller
	Dispatcher    Dispatcher
	DispatchReady func(context.Context) error
	Worktrees     Worktrees
	Registrar     Registrar
	GitHub        githubpkg.Client
	Repositories  *repositories.Service
	References    *references.Service
	Validation    ValidationRunner
	AllowNoChecks bool
	Logger        *slog.Logger
	Outbox        *outbox.Store
	// WorktreeToolFailures reports tool failures recorded for a worktree at or
	// after the given time. Used to verify Writer blocker claims.
	WorktreeToolFailures func(worktreePath string, since time.Time) []ToolFailureInfo
}

type ValidationRunner interface {
	Run(context.Context, string, string) (validation.Result, error)
}

// Role limits a pipeline agent to its state-transition tools.
type Role string

const (
	RoleAdmin    Role = "admin"
	RolePlanner  Role = "planner"
	RoleWriter   Role = "writer"
	RoleReviewer Role = "reviewer"
	RoleHolistic Role = "holistic"
)

func New(config Config) *server.StreamableHTTPServer {
	return NewRole(config, RoleAdmin)
}

// NewRole creates a job MCP endpoint exposing only the tools assigned to role.
func NewRole(config Config, role Role) *server.StreamableHTTPServer {
	s := server.NewMCPServer("opendev", "2.0.0", server.WithToolCapabilities(true))
	registerJobs(s, config, role)
	return server.NewStreamableHTTPServer(s)
}

func registerJobs(s *server.MCPServer, config Config, role Role) {
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	allowed := func(roles ...Role) bool {
		if role == RoleAdmin {
			return true
		}
		for _, allowedRole := range roles {
			if role == allowedRole {
				return true
			}
		}
		return false
	}
	if allowed() {
		s.AddTool(mcp.NewTool("get_code_task_inspection",
			mcp.WithDescription("Return the complete durable audit trail for one task, including review feedback and its AgentFoundry dispatch attempts. Read-only."),
			mcp.WithString("job_id", mcp.Required()),
			mcp.WithString("task_key", mcp.Required()),
			mcp.WithBoolean("include_execution_trace", mcp.Description("Include normalized per-turn AgentFoundry model, tool, schema, and terminal events.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			job, task, err := jobTask(config.Store, req)
			if err != nil {
				return toolError(err), nil
			}
			runs, err := config.Dispatcher.InspectJob(ctx, job.ID)
			if err != nil {
				return toolError(err), nil
			}
			taskRuns := make([]dispatcher.DispatchRun, 0)
			for _, run := range runs {
				if run.TaskKey == task.Key {
					taskRuns = append(taskRuns, run)
				}
			}
			result := map[string]any{"job_id": job.ID, "repository": job.Repository, "task": task, "dispatch_runs": taskRuns}
			if req.GetBool("include_execution_trace", false) {
				traces := make(map[string]any, len(taskRuns))
				tracer, ok := config.Dispatcher.(executionTracer)
				for _, run := range taskRuns {
					if !ok {
						traces[run.TaskID] = map[string]string{"error": "execution tracing is unavailable"}
						continue
					}
					trace, traceErr := tracer.ExecutionTrace(ctx, run)
					if traceErr != nil {
						traces[run.TaskID] = map[string]string{"error": traceErr.Error()}
						continue
					}
					traces[run.TaskID] = trace
				}
				result["execution_traces"] = traces
			}
			return toolJSON(result), nil
		})

		s.AddTool(mcp.NewTool("get_code_job_inspection",
			mcp.WithDescription("Return the durable controller, review, and AgentFoundry dispatch audit trail for a coding job. This is read-only and includes raw terminal agent responses."),
			mcp.WithString("job_id", mcp.Required()),
			mcp.WithBoolean("include_execution_trace", mcp.Description("Include normalized per-turn AgentFoundry model, tool, schema, and terminal events.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			jobID, err := req.RequireString("job_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			job, err := config.Store.Get(jobID)
			if err != nil {
				return toolError(err), nil
			}
			runs, err := config.Dispatcher.InspectJob(ctx, jobID)
			if err != nil {
				return toolError(err), nil
			}
			result := map[string]any{"job": job, "dispatch_runs": runs}
			if req.GetBool("include_execution_trace", false) {
				traces := make(map[string]any, len(runs))
				tracer, ok := config.Dispatcher.(executionTracer)
				for _, run := range runs {
					if !ok {
						traces[run.TaskID] = map[string]string{"error": "execution tracing is unavailable"}
						continue
					}
					trace, traceErr := tracer.ExecutionTrace(ctx, run)
					if traceErr != nil {
						traces[run.TaskID] = map[string]string{"error": traceErr.Error()}
						continue
					}
					traces[run.TaskID] = trace
				}
				result["execution_traces"] = traces
			}
			return toolJSON(result), nil
		})

		s.AddTool(mcp.NewTool("retry_code_task",
			mcp.WithDescription("Retry a Writer task whose terminal result could not be applied, or resume a task its Writer reported blocked. Durable history is preserved."),
			mcp.WithString("job_id", mcp.Required()),
			mcp.WithString("task_key", mcp.Required()),
			mcp.WithString("reason", mcp.Required()),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			jobID, err := req.RequireString("job_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			taskKey, err := req.RequireString("task_key")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			reason, err := req.RequireString("reason")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			job, err := config.Store.Get(jobID)
			if err != nil {
				return toolError(err), nil
			}
			task := findTask(job, taskKey)
			if task == nil {
				return mcp.NewToolResultError("task not found"), nil
			}
			switch {
			case task.Status == pipeline.TaskWorking && task.WriterRunID != "":
				if err := config.Dispatcher.Supersede(ctx, dispatcher.TaskID(job.ID, dispatcher.RoleWriter, task.Key, task.WriterAttempts), reason); err != nil {
					return toolError(err), nil
				}
				job, err = config.Controller.RetryWriter(job.ID, task.Key, task.WriterRunID)
				if err != nil {
					return toolError(err), nil
				}
			case task.Status == pipeline.TaskBlocked:
				job, err = config.Controller.ResumeBlockedWriter(job.ID, task.Key)
				if err != nil {
					return toolError(err), nil
				}
			default:
				return mcp.NewToolResultError("only an unapplied or Writer-blocked task can be retried"), nil
			}
			run, err := startWriter(ctx, job, findTask(job, taskKey), config)
			if err != nil {
				return toolError(err), nil
			}
			return toolJSON(map[string]any{"job_id": job.ID, "task_key": taskKey, "run_id": run.RunID}), nil
		})

		s.AddTool(mcp.NewTool("lookup_repository",
			mcp.WithDescription("Reconcile one owned repository with the durable catalog and local clone. This creates nothing."),
			mcp.WithString("repository", mcp.Required(), mcp.Description("Owned GitHub repository name.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if config.Repositories == nil {
				return toolError(fmt.Errorf("repository lifecycle is not configured; set GITHUB_TOKEN and GITHUB_OWNER")), nil
			}
			name, err := req.RequireString("repository")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			repo, err := config.Repositories.Lookup(ctx, name)
			if err != nil {
				return toolError(err), nil
			}
			if repo == nil {
				return mcp.NewToolResultText(`{"found":false}`), nil
			}
			return toolJSON(map[string]any{"found": true, "repository": repo}), nil
		})

		s.AddTool(mcp.NewTool("provision_repository",
			mcp.WithDescription("Create one new private owned GitHub repository, clone it locally, and catalog it. Call only after a semantic request clearly requires a new project."),
			mcp.WithString("repository", mcp.Required(), mcp.Description("New repository name.")),
			mcp.WithString("description", mcp.Description("Optional repository description.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if config.Repositories == nil {
				return toolError(fmt.Errorf("repository lifecycle is not configured; set GITHUB_TOKEN and GITHUB_OWNER")), nil
			}
			name, err := req.RequireString("repository")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			repo, err := config.Repositories.ProvisionPrivate(ctx, name, req.GetString("description", ""))
			if err != nil {
				return toolError(err), nil
			}
			return toolJSON(repo), nil
		})

		s.AddTool(mcp.NewTool("fork_public_repository",
			mcp.WithDescription("Explicitly fork a public upstream into the configured owner, clone it locally, and catalog it. Do not call merely because repository lookup failed."),
			mcp.WithString("upstream_owner", mcp.Required()),
			mcp.WithString("upstream_repository", mcp.Required()),
			mcp.WithString("repository", mcp.Required(), mcp.Description("Name for the owned fork.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if config.Repositories == nil {
				return toolError(fmt.Errorf("repository lifecycle is not configured; set GITHUB_TOKEN and GITHUB_OWNER")), nil
			}
			owner, err := req.RequireString("upstream_owner")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			upstream, err := req.RequireString("upstream_repository")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			name, err := req.RequireString("repository")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			repo, err := config.Repositories.ForkPublic(ctx, owner, upstream, name)
			if err != nil {
				return toolError(err), nil
			}
			return toolJSON(repo), nil
		})

		s.AddTool(mcp.NewTool("create_code_job",
			mcp.WithDescription("Create a durable coding job and its integration branch record. This does not edit a repository or start an agent."),
			mcp.WithString("repository", mcp.Required(), mcp.Description("Configured repository name.")),
			mcp.WithString("directive", mcp.Required(), mcp.Description("High-level outcome to achieve.")),
			mcp.WithString("target_branch", mcp.Description("Target branch. Defaults to main.")),
			mcp.WithString("planner_agent_id", mcp.Description("AgentFoundry ID of the planner.")),
			mcp.WithString("writer_agent_id", mcp.Description("AgentFoundry ID of the writer.")),
			mcp.WithString("reviewer_agent_id", mcp.Description("AgentFoundry ID of the reviewer.")),
		), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			repository, err := req.RequireString("repository")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			directive, err := req.RequireString("directive")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			job, err := config.Store.Create(repository, directive, req.GetString("target_branch", "main"), "", req.GetString("planner_agent_id", ""), req.GetString("writer_agent_id", ""), req.GetString("reviewer_agent_id", ""))
			if err != nil {
				return toolError(err), nil
			}
			return toolJSON(job), nil
		})
	}

	if allowed(RolePlanner, RoleWriter, RoleReviewer, RoleHolistic) {
		s.AddTool(mcp.NewTool("get_code_job", mcp.WithDescription("Get durable coding job state."), mcp.WithString("job_id", mcp.Required())), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			id, err := req.RequireString("job_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			job, err := config.Store.Get(id)
			if err != nil {
				return toolError(err), nil
			}
			return toolJSON(job), nil
		})
	}

	if allowed(RoleWriter, RoleReviewer) {
		s.AddTool(mcp.NewTool("get_task_diff", mcp.WithDescription("Return the authoritative committed diff for this task from its recorded base SHA to its candidate commit. Every Reviewer must use this as its review target."), mcp.WithString("job_id", mcp.Required()), mcp.WithString("task_key", mcp.Required())), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			_, task, err := jobTask(config.Store, req)
			if err != nil {
				return toolError(err), nil
			}
			if task.BaseSHA == "" || task.CommitSHA == "" {
				return mcp.NewToolResultError("task has no committed candidate diff yet"), nil
			}
			diff, err := config.Worktrees.DiffRange(task.WorktreePath, task.BaseSHA, task.CommitSHA)
			if err != nil {
				return toolError(err), nil
			}
			return toolJSON(map[string]any{"base_sha": task.BaseSHA, "commit_sha": task.CommitSHA, "diff": diff}), nil
		})

	}

	if allowed() {
		s.AddTool(mcp.NewTool("start_planning", mcp.WithDescription("Start the configured Planner agent for a new coding job."), mcp.WithString("job_id", mcp.Required())), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			id, err := req.RequireString("job_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			job, err := config.Store.Get(id)
			if err != nil {
				return toolError(err), nil
			}
			if config.Repositories != nil {
				repo, err := config.Repositories.Lookup(ctx, job.Repository)
				if err != nil {
					return toolError(fmt.Errorf("refresh target repository before planning: %w", err)), nil
				}
				if repo == nil {
					return toolError(fmt.Errorf("target repository %q is no longer available", job.Repository)), nil
				}
				if _, err := config.Repositories.EnsureFoundation(ctx, job.Repository); err != nil {
					if errors.Is(err, repositories.ErrFoundationPending) {
						if _, markErr := config.Store.MarkFoundationPending(job.ID); markErr != nil {
							return toolError(markErr), nil
						}
						return toolJSON(map[string]any{"job_id": job.ID, "status": "foundation_pending"}), nil
					}
					return toolError(fmt.Errorf("ensure repository foundation before planning: %w", err)), nil
				}
			}
			if config.DispatchReady != nil {
				if err := config.DispatchReady(ctx); err != nil {
					return toolError(err), nil
				}
			}
			job, err = config.Store.StartPlanning(id)
			if err != nil {
				return toolError(err), nil
			}
			notify(config, job, outbox.EventStarted, "", "")
			run, err := config.Dispatcher.StartPlanner(ctx, job)
			if err != nil {
				return toolError(err), nil
			}
			return toolJSON(map[string]any{"job_id": job.ID, "status": job.Status, "run_id": run.RunID}), nil
		})
	}

	if allowed() {
		s.AddTool(mcp.NewTool("publish_approved_code_job", mcp.WithDescription("Resume automatic publication and gated merge for a holistically approved coding job. This never merges an arbitrary pull request."), mcp.WithString("job_id", mcp.Required())), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			jobID, err := req.RequireString("job_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			job, err := publishApprovedJob(ctx, jobID, config)
			if err != nil {
				return toolError(err), nil
			}
			return toolJSON(job), nil
		})
	}

	if allowed() {
		s.AddTool(mcp.NewTool("start_holistic_rereview", mcp.WithDescription("When a recorded PR head changed, start a new holistic review of that PR's current head. The existing PR is preserved."), mcp.WithString("job_id", mcp.Required())), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if config.GitHub == nil {
				return toolError(fmt.Errorf("GitHub integration is not configured; set GITHUB_TOKEN and GITHUB_OWNER")), nil
			}
			jobID, err := req.RequireString("job_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			job, err := config.Store.Get(jobID)
			if err != nil {
				return toolError(err), nil
			}
			if job.PullRequestNumber == 0 {
				return toolError(fmt.Errorf("job %s has no recorded pull request", job.ID)), nil
			}
			pr, err := config.GitHub.GetPR(ctx, job.Repository, job.PullRequestNumber)
			if err != nil {
				return toolError(fmt.Errorf("get pull request: %w", err)), nil
			}
			if !strings.EqualFold(pr.State, "open") || pr.Draft {
				return toolError(fmt.Errorf("pull request #%d must be open and ready for review", pr.Number)), nil
			}
			if job.Status == pipeline.JobHolisticReviewing && job.IntegrationSHA == pr.Head.SHA {
				return toolJSON(job), nil
			}
			job, err = config.Controller.StartHolisticRereview(job.ID, pr.Head.SHA)
			if err != nil {
				return toolError(err), nil
			}
			run, err := config.Dispatcher.StartHolistic(ctx, job)
			if err != nil {
				return toolError(err), nil
			}
			job, err = config.Controller.StartHolisticReviewer(job.ID, run.RunID, job.IntegrationSHA)
			if err != nil {
				return toolError(err), nil
			}
			return toolJSON(map[string]any{"job": job, "holistic_reviewer_run_id": run.RunID}), nil
		})
	}
}

func publishApprovedJob(ctx context.Context, jobID string, config Config) (*pipeline.Job, error) {
	if config.GitHub == nil {
		return nil, fmt.Errorf("GitHub integration is not configured; set GITHUB_TOKEN and GITHUB_OWNER")
	}
	job, err := config.Store.Get(jobID)
	if err != nil {
		return nil, err
	}
	if job.MergeState == pipeline.MergeMerged && job.Status == pipeline.JobPublished {
		return job, nil
	}
	if job.HolisticReviewVerdict != pipeline.ReviewApproved || (job.Status != pipeline.JobReadyToPublish && job.MergeState != pipeline.MergeOpen) {
		return nil, fmt.Errorf("job %s is not approved for publication", job.ID)
	}

	if job.PullRequestNumber == 0 {
		if job.PullRequestTitle == "" || job.PullRequestBody == "" {
			return nil, fmt.Errorf("job %s has no generated pull request presentation", job.ID)
		}
		pr, err := config.GitHub.CreatePR(ctx, githubpkg.CreatePROptions{
			Repo: job.Repository, Title: job.PullRequestTitle, Head: job.IntegrationBranch,
			Base: job.TargetBranch, Body: job.PullRequestBody, Draft: false,
		})
		if err != nil {
			return nil, fmt.Errorf("create pull request: %w", err)
		}
		job, err = config.Controller.RecordPullRequest(job.ID, pr.Number, pr.HTMLURL)
		if err != nil {
			return nil, fmt.Errorf("record pull request: %w", err)
		}
		notify(config, job, outbox.EventPROpened, "", "")
	} else if job.PullRequestTitle != "" && job.PullRequestBody != "" {
		if err := config.GitHub.UpdatePR(ctx, job.Repository, job.PullRequestNumber, job.PullRequestTitle, job.PullRequestBody); err != nil {
			return nil, fmt.Errorf("update pull request: %w", err)
		}
	} else {
		// Terminal reviews recorded before generated PR content was introduced
		// retain their existing GitHub presentation during reconciliation.
	}

	pr, err := config.GitHub.GetPR(ctx, job.Repository, job.PullRequestNumber)
	if err != nil {
		return nil, fmt.Errorf("get pull request: %w", err)
	}
	if pr.Merged {
		merged, err := config.Controller.RecordMerge(job.ID)
		notify(config, merged, outbox.EventMerged, "", "")
		return merged, err
	}
	if pr.Draft {
		if err := config.GitHub.PromotePR(ctx, job.Repository, pr.Number); err != nil {
			return nil, fmt.Errorf("promote pull request: %w", err)
		}
		for attempt := 0; attempt < 10; attempt++ {
			pr, err = config.GitHub.GetPR(ctx, job.Repository, pr.Number)
			if err != nil {
				return nil, fmt.Errorf("confirm pull request promotion: %w", err)
			}
			if !pr.Draft {
				break
			}
			if attempt == 9 {
				return nil, fmt.Errorf("pull request #%d is still draft after promotion", pr.Number)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	if !strings.EqualFold(pr.State, "open") || pr.Draft {
		return nil, fmt.Errorf("pull request #%d must be open and ready for review before merge", pr.Number)
	}
	if pr.Head.SHA != job.IntegrationSHA {
		return nil, fmt.Errorf("pull request #%d head SHA %s differs from reviewed integration SHA %s; explicit holistic re-review is required", pr.Number, pr.Head.SHA, job.IntegrationSHA)
	}
	checks, err := config.GitHub.GetPRChecks(ctx, job.Repository, pr.Head.SHA)
	if err != nil {
		return nil, fmt.Errorf("get pull request checks: %w", err)
	}
	if err := successfulChecks(checks, config.AllowNoChecks); err != nil {
		return nil, fmt.Errorf("pull request #%d merge gate: %w", pr.Number, err)
	}
	if err := config.GitHub.MergePR(ctx, job.Repository, pr.Number); err != nil {
		return nil, fmt.Errorf("merge pull request #%d: %w", pr.Number, err)
	}
	merged, err := config.Controller.RecordMerge(job.ID)
	notify(config, merged, outbox.EventMerged, "", "")
	return merged, err
}

func successfulChecks(checks *githubpkg.PRChecks, allowNone bool) error {
	if checks == nil || checks.TotalCount == 0 || len(checks.CheckRuns) == 0 {
		if allowNone {
			return nil
		}
		return fmt.Errorf("no checks reported")
	}
	if checks.TotalCount != len(checks.CheckRuns) {
		return fmt.Errorf("not all checks were returned")
	}
	for _, check := range checks.CheckRuns {
		if !strings.EqualFold(check.Status, "completed") || !strings.EqualFold(check.Conclusion, "success") {
			return fmt.Errorf("check %q is %s/%s", check.Name, check.Status, check.Conclusion)
		}
	}
	return nil
}

func ensureIntegrationWorktree(job *pipeline.Job, config Config) error {
	dir, err := config.Worktrees.CreateWorktree(job.Repository, job.IntegrationBranch, job.TargetBranch)
	if err != nil {
		return fmt.Errorf("create integration worktree: %w", err)
	}
	config.Registrar.RegisterWorktree(job.Repository, safeBranch(job.IntegrationBranch), dir)
	return nil
}

// maxWriterAttempts bounds automatic Writer dispatches per task. retry_code_task
// bypasses the cap because the operator explicitly asked for another attempt.
const maxWriterAttempts = 10

func startReadyWriters(ctx context.Context, job *pipeline.Job, config Config) ([]string, error) {
	var runs []string
	for i := range job.Plan.Tasks {
		task := &job.Plan.Tasks[i]
		if task.Status != pipeline.TaskPlanned || !taskDependenciesIntegrated(job, task) {
			continue
		}
		if task.WriterAttempts >= maxWriterAttempts {
			reason := fmt.Sprintf("writer attempt limit reached (%d automatic attempts); resume explicitly with retry_code_task if further work is justified", task.WriterAttempts)
			job, err := config.Controller.BlockTask(job.ID, task.Key, reason)
			if err != nil {
				return nil, err
			}
			notify(config, job, outbox.EventBlocked+":"+task.Key, task.Title, reason)
			continue
		}
		run, err := startWriter(ctx, job, task, config)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run.RunID)
	}
	return runs, nil
}

func startWriter(ctx context.Context, job *pipeline.Job, task *pipeline.Task, config Config) (*dispatcher.DispatchRun, error) {
	branch := taskBranch(job.ID, task.Key)
	dir, err := config.Worktrees.CreateWorktree(job.Repository, branch, job.IntegrationBranch)
	if err != nil {
		return nil, fmt.Errorf("create task worktree %q: %w", task.Key, err)
	}
	if task.BaseSHA == "" {
		baseSHA, err := config.Worktrees.HeadCommit(dir)
		if err != nil {
			return nil, fmt.Errorf("record task base SHA: %w", err)
		}
		job, err = config.Controller.RecordTaskBase(job.ID, task.Key, baseSHA)
		if err != nil {
			return nil, err
		}
		task = findTask(job, task.Key)
	}
	config.Registrar.RegisterWorktree(job.Repository, branch, dir)
	// Dispatch needs the route before StartTaskWork durably records it.
	dispatchTask := *task
	dispatchTask.Branch = branch
	run, err := config.Dispatcher.StartWriter(ctx, job, &dispatchTask)
	if err != nil {
		return nil, err
	}
	if _, err := config.Controller.StartTaskWork(job.ID, task.Key, branch, dir, run.RunID); err != nil {
		return nil, err
	}
	return run, nil
}

func integrateEligible(jobID string, config Config, ctx context.Context) (*pipeline.Job, string, error) {
	job, err := config.Store.Get(jobID)
	if err != nil {
		return nil, "", err
	}
	for {
		var task *pipeline.Task
		for i := range job.Plan.Tasks {
			if job.Plan.Tasks[i].Status == pipeline.TaskIntegrationEligible {
				task = &job.Plan.Tasks[i]
				break
			}
		}
		if task == nil {
			break
		}
		if _, err := config.Controller.StartIntegration(job.ID, task.Key); err != nil {
			return nil, "", err
		}
		var sha string
		if task.Kind == pipeline.TaskRemediation && task.Branch == job.IntegrationBranch {
			pusher, ok := config.Worktrees.(branchPusher)
			if !ok {
				return nil, "", fmt.Errorf("worktree manager does not support pushing remediation branch")
			}
			if err := pusher.PushBranch(job.Repository, job.IntegrationBranch); err != nil {
				return nil, "", fmt.Errorf("push remediation task %q: %w", task.Key, err)
			}
			sha = task.CommitSHA
		} else {
			sha, err = config.Worktrees.MergeTask(job.Repository, task.Branch, job.IntegrationBranch)
			if err != nil {
				return nil, "", fmt.Errorf("integrate task %q: %w", task.Key, err)
			}
		}
		job, err = config.Controller.RecordIntegration(job.ID, task.Key, sha)
		if err != nil {
			return nil, "", err
		}
	}
	if _, err := startReadyWriters(ctx, job, config); err != nil {
		return nil, "", err
	}
	if job.Status != pipeline.JobHolisticReviewing {
		return job, "", nil
	}
	job, err = startDraftPRCI(ctx, job, config)
	return job, "", err
}

type branchPusher interface {
	PushBranch(repository, branch string) error
}

func startDraftPRCI(ctx context.Context, job *pipeline.Job, config Config) (*pipeline.Job, error) {
	if config.GitHub == nil {
		return nil, fmt.Errorf("GitHub integration is required for draft PR CI")
	}
	pusher, ok := config.Worktrees.(branchPusher)
	if !ok {
		return nil, fmt.Errorf("worktree manager does not support pushing integration branches")
	}
	if err := pusher.PushBranch(job.Repository, job.IntegrationBranch); err != nil {
		return nil, fmt.Errorf("push integration branch before draft PR: %w", err)
	}
	if job.PullRequestNumber == 0 {
		title := "OpenDev draft"
		if job.Plan != nil && strings.TrimSpace(job.Plan.Summary) != "" {
			title += ": " + strings.TrimSpace(job.Plan.Summary)
		}
		pr, err := config.GitHub.CreatePR(ctx, githubpkg.CreatePROptions{Repo: job.Repository, Title: title, Head: job.IntegrationBranch, Base: job.TargetBranch, Body: "OpenDev is running required CI checks before final review.", Draft: true})
		if err != nil {
			return nil, fmt.Errorf("create draft pull request: %w", err)
		}
		var recordErr error
		job, recordErr = config.Controller.RecordPullRequest(job.ID, pr.Number, pr.HTMLURL)
		if recordErr != nil {
			return nil, fmt.Errorf("record draft pull request: %w", recordErr)
		}
		notify(config, job, outbox.EventPROpened, "", "")
	}
	pr, err := config.GitHub.GetPR(ctx, job.Repository, job.PullRequestNumber)
	if err != nil {
		return nil, fmt.Errorf("get draft pull request: %w", err)
	}
	if !pr.Draft || !strings.EqualFold(pr.State, "open") {
		return nil, fmt.Errorf("pull request #%d is not an open draft", pr.Number)
	}
	if pr.Head.SHA != job.IntegrationSHA {
		return nil, fmt.Errorf("draft pull request #%d head SHA %s differs from integration SHA %s", pr.Number, pr.Head.SHA, job.IntegrationSHA)
	}
	return config.Controller.StartCI(job.ID, pr.Head.SHA, []string{"ci / OpenDev CI"})
}

func taskDependenciesIntegrated(job *pipeline.Job, task *pipeline.Task) bool {
	for _, dependency := range task.DependsOn {
		prerequisite := findTask(job, dependency)
		if prerequisite == nil || (prerequisite.Status != pipeline.TaskIntegrated && prerequisite.Status != pipeline.TaskNoChanges) {
			return false
		}
	}
	return true
}

func jobTask(store *pipeline.Store, req mcp.CallToolRequest) (*pipeline.Job, *pipeline.Task, error) {
	jobID, err := req.RequireString("job_id")
	if err != nil {
		return nil, nil, err
	}
	taskKey, err := req.RequireString("task_key")
	if err != nil {
		return nil, nil, err
	}
	job, err := store.Get(jobID)
	if err != nil {
		return nil, nil, err
	}
	task := findTask(job, taskKey)
	if task == nil {
		return nil, nil, fmt.Errorf("task %q not found", taskKey)
	}
	return job, task, nil
}

func findTask(job *pipeline.Job, key string) *pipeline.Task {
	if job == nil || job.Plan == nil {
		return nil
	}
	for i := range job.Plan.Tasks {
		if job.Plan.Tasks[i].Key == key {
			return &job.Plan.Tasks[i]
		}
	}
	return nil
}

func evidenceFrom(raw string) ([]pipeline.ValidationEvidence, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var evidence []pipeline.ValidationEvidence
	if err := json.Unmarshal([]byte(raw), &evidence); err != nil {
		return nil, fmt.Errorf("invalid validation_evidence_json: %w", err)
	}
	return evidence, nil
}

var unsafeBranch = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func taskBranch(jobID, taskKey string) string {
	return "opendev-task-" + safeBranch(jobID) + "-" + safeBranch(taskKey)
}
func safeBranch(value string) string {
	return strings.Trim(unsafeBranch.ReplaceAllString(value, "-"), "-")
}

func toolJSON(value any) *mcp.CallToolResult {
	data, err := json.Marshal(value)
	if err != nil {
		return mcp.NewToolResultError("encode tool result: " + err.Error())
	}
	return mcp.NewToolResultText(string(data))
}

func toolError(err error) *mcp.CallToolResult { return mcp.NewToolResultError(err.Error()) }

func notify(config Config, job *pipeline.Job, eventType, summary, eventError string) {
	outbox.Notify(config.Outbox, job, eventType, summary, eventError, config.Logger)
}
