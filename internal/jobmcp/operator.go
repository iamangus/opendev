package jobmcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/iamangus/code-mcp/internal/dispatcher"
	"github.com/iamangus/code-mcp/internal/pipeline"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type jobCanceller interface {
	CancelJob(context.Context, string) error
}

func registerOperatorTools(s *server.MCPServer, config Config) {
	s.AddTool(mcp.NewTool("retry_code_planning", mcp.WithDescription("Retry a terminal planner response that could not be applied, preserving the rejected response and dispatching a fresh attempt."), mcp.WithString("job_id", mcp.Required()), mcp.WithString("reason", mcp.Required())), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := req.RequireString("job_id")
		if err != nil {
			return toolError(err), nil
		}
		reason, err := req.RequireString("reason")
		if err != nil {
			return toolError(err), nil
		}
		job, err := config.Store.Get(id)
		if err != nil {
			return toolError(err), nil
		}
		if job.Status != pipeline.JobPlanning && job.Status != pipeline.JobCIReplanning {
			return toolError(fmt.Errorf("job is not awaiting a planner outcome")), nil
		}
		revision := job.Status == pipeline.JobCIReplanning
		key := "job"
		if revision {
			key = "revision"
		}
		if err := config.Dispatcher.Supersede(ctx, dispatcher.TaskID(id, dispatcher.RolePlanner, key, dispatcher.PlannerAttempt(job, revision)), reason); err != nil {
			return toolError(err), nil
		}
		job, err = config.Store.RetryPlanning(id)
		if err != nil {
			return toolError(err), nil
		}
		var run *dispatcher.DispatchRun
		if revision {
			run, err = config.Dispatcher.StartPlannerRevision(ctx, job)
		} else {
			run, err = config.Dispatcher.StartPlanner(ctx, job)
		}
		if err != nil {
			return toolError(err), nil
		}
		return toolJSON(map[string]any{"job_id": id, "run_id": run.RunID}), nil
	})
	s.AddTool(mcp.NewTool("archive_code_job", mcp.WithDescription("Archive a terminal job and its dispatch history outside active snapshots. Preserves source worktrees and read-only inspection."), mcp.WithString("job_id", mcp.Required())), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := req.RequireString("job_id")
		if err != nil {
			return toolError(err), nil
		}
		job, err := config.Store.Get(id)
		if err != nil {
			return toolError(err), nil
		}
		if !pipeline.Terminal(job.Status) {
			return toolError(fmt.Errorf("only terminal jobs may be archived")), nil
		}
		if canceller, ok := config.Dispatcher.(jobCanceller); ok {
			if err := canceller.CancelJob(ctx, id); err != nil {
				return toolError(err), nil
			}
		}
		if archiver, ok := config.Dispatcher.(interface {
			ArchiveJob(context.Context, string) error
		}); ok {
			if err := archiver.ArchiveJob(ctx, id); err != nil {
				return toolError(err), nil
			}
		}
		if config.Outbox != nil {
			if err := config.Outbox.Reconcile([]*pipeline.Job{job}); err != nil {
				return toolError(err), nil
			}
		}
		archived, err := config.Store.Archive(id)
		if err != nil {
			return toolError(err), nil
		}
		if registrar, ok := config.Registrar.(interface{ UnregisterJob(*pipeline.Job) }); ok {
			registrar.UnregisterJob(job)
		}
		return toolJSON(archived), nil
	})
	for _, action := range []string{"pause", "resume", "cancel"} {
		action := action
		description := map[string]string{"pause": "Pause automatic job progression. Already running agents may finish; their outcomes are retained until resume.", "resume": "Resume automatic progression of a paused coding job.", "cancel": "Cancel a coding job and request cancellation of its outstanding agent runs. Worktrees and history are preserved."}[action]
		s.AddTool(mcp.NewTool(action+"_code_job", mcp.WithDescription(description), mcp.WithString("job_id", mcp.Required()), mcp.WithString("reason", mcp.Description("Operator context for cancellation."))), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			id, err := req.RequireString("job_id")
			if err != nil {
				return toolError(err), nil
			}
			switch action {
			case "pause":
				job, err := config.Store.Pause(id)
				if err != nil {
					return toolError(err), nil
				}
				return toolJSON(job), nil
			case "resume":
				job, err := config.Store.Resume(id)
				if err != nil {
					return toolError(err), nil
				}
				return toolJSON(job), nil
			default:
				reason := strings.TrimSpace(req.GetString("reason", "cancelled by operator"))
				job, err := config.Store.Cancel(id, reason)
				if err != nil {
					return toolError(err), nil
				}
				if canceller, ok := config.Dispatcher.(jobCanceller); ok {
					if err := canceller.CancelJob(ctx, id); err != nil {
						return toolError(fmt.Errorf("job is durably cancelled; remote cancellation will retry: %w", err)), nil
					}
				}
				return toolJSON(job), nil
			}
		})
	}
	s.AddTool(mcp.NewTool("approve_code_job_merge", mcp.WithDescription("Explicitly approve gated merge of a manually held, holistically approved job. All SHA and required-CI gates still apply."), mcp.WithString("job_id", mcp.Required())), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := req.RequireString("job_id")
		if err != nil {
			return toolError(err), nil
		}
		approved := config
		approved.ForceMerge = true
		job, err := publishApprovedJob(ctx, id, approved)
		if err != nil {
			return toolError(err), nil
		}
		return toolJSON(job), nil
	})
	s.AddTool(mcp.NewTool("list_notification_events", mcp.WithDescription("Inspect durable notification delivery, including terminal failures."), mcp.WithNumber("limit", mcp.Description("Maximum events, default 50, maximum 200.")), mcp.WithNumber("offset", mcp.Description("Zero-based result offset."))), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if config.Outbox == nil {
			return toolError(fmt.Errorf("notification outbox is not configured")), nil
		}
		limit, offset := req.GetInt("limit", 50), req.GetInt("offset", 0)
		if limit < 1 || limit > 200 || offset < 0 {
			return toolError(fmt.Errorf("invalid notification pagination")), nil
		}
		events := config.Outbox.List()
		total := len(events)
		if offset > total {
			offset = total
		}
		end := min(total, offset+limit)
		return toolJSON(map[string]any{"events": events[offset:end], "total": total, "offset": offset, "limit": limit}), nil
	})
	s.AddTool(mcp.NewTool("retry_notification_event", mcp.WithDescription("Requeue an undelivered notification after fixing its delivery failure."), mcp.WithString("event_id", mcp.Required())), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if config.Outbox == nil {
			return toolError(fmt.Errorf("notification outbox is not configured")), nil
		}
		id, err := req.RequireString("event_id")
		if err != nil {
			return toolError(err), nil
		}
		if err := config.Outbox.Retry(id); err != nil {
			return toolError(err), nil
		}
		return mcp.NewToolResultText("Notification requeued"), nil
	})
}
