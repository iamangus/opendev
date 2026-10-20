# opendev

A Go MCP server that exposes coding tools scoped to a single Git worktree (branch).

## Tool profiles

Tools are grouped into named **profiles**. Each profile is served at a separate endpoint so consumers only see the tools they need.

| Profile | Endpoint suffix | Tools |
|---------|-----------------|-------|
| `read`  | `/read/mcp`     | `read_file`, `read_lines`, `list_directory`, `grep_search` |
| `write` | `/write/mcp`    | `create_file`, `search_and_replace` |

## Tools

### read profile

| Tool | Parameters | Description |
|------|------------|-------------|
| `read_file` | `filepath` | Read the entire contents of a file |
| `read_lines` | `filepath`, `start_line`, `end_line` | Read a line range (1-indexed, inclusive) |
| `list_directory` | `dirpath`, `recursive` | List directory contents |
| `grep_search` | `query`, `directory?` | Search for a regex/literal pattern across files |

### write profile

| Tool | Parameters | Description |
|------|------------|-------------|
| `create_file` | `filepath`, `content` | Create a new file (fails if it already exists) |
| `search_and_replace` | `filepath`, `search_block`, `replace_block`, `expected_revision` | Replace a unique block (exact, then whitespace-normalized); pass the revision returned by `read_file` |

All `filepath` and `dirpath` values are relative to the worktree root. Symlink paths, Git metadata, and OpenDev runtime paths are rejected. Mutations are bounded to 1 MiB; edits use atomic replacement and preserve file permissions. Oversized listings, line ranges, searches, and diffs are truncated explicitly.

HTTP mode always requires the existing `OPENDEV_TOKEN` bearer token. The same token still authorizes all HTTP endpoints: role-specific tool lists are not separate authorization boundaries. Use only trusted clients and restrict network access. Stdio does not require HTTP authentication.

## Usage

### Single-server mode

```
opendev --dir /path/to/worktree [--mode stdio|http] [--addr :8080]
```

In HTTP mode each profile is available at `/{profile}/mcp`:

```
http://localhost:8080/read/mcp
http://localhost:8080/write/mcp
```

In stdio mode the `read` profile is served (stdio is single-stream).

| Flag | Default | Description |
|------|---------|-------------|
| `--dir` | *(required)* | Absolute path to the worktree root directory |
| `--mode` | `stdio` | Transport mode: `stdio` or `http` |
| `--addr` | `:8080` | HTTP listen address (only used when `--mode=http`) |

### Multi-server mode

When `--dir` is omitted the server runs in multi-repo mode, scanning `--repos-dir` for
repositories and their worktrees on startup.

```
opendev [--repos-dir /repos] [--addr :8080]
```

MCP endpoints follow the pattern:

```
http://host:port/{repo}/{branch}/{profile}/mcp
```

For example:

```
http://localhost:8080/myrepo/main/read/mcp
http://localhost:8080/myrepo/my-feature/write/mcp
```

Branches containing slashes use URL-escaped route segments, for example `feature%2Flogin`. Primary repository clones are read-only in multi-repository mode. Writers use isolated task worktrees.

The job control plane is exposed through MCP at `/mcp`. Role-scoped
endpoints are `/mcp/planner`, `/mcp/writer`, `/mcp/reviewer`, and
`/mcp/holistic`. Use `lookup_repository`, `provision_repository`,
`fork_public_repository`, `create_code_job`, and `start_planning` on the
admin endpoint. Repository lookups and startup catalog sync do not create
foundation changes. Provisioning an owned, non-fork repository or creating
its first coding job starts a versioned foundation PR; jobs wait for its
CI to pass and the PR to merge before planning starts. Later foundation
  migrations require the admin-only `migrate_repository_foundation` tool with
  the current target version; catalog refresh and ordinary coding jobs do not
  initiate upgrades.
  If a planner launch fails after the job enters planning, reconciliation
  retries that same durable dispatch rather than leaving the job stranded.

The active validation contract is `.opendev/validations.yml` in the
target repository. The previous `.opendev/config.yaml` test-command API
is no longer served.

```yaml
validations:
  tests:
    directory: .
    command: go test -race ./... && go vet ./...
    timeout_seconds: 300
```

These commands run on the GitHub Actions runner, not through an agent shell tool. Without a policy, CI discovers Go, npm, Python, and Rust projects (including nested modules), or a root Makefile. A repository with no supported validations fails rather than receiving a false green check. npm auto-discovery requires a package lock and a test or build script. Explicit profiles are `auto`, `go`, `node`, `python`, `rust`, and `make`.

### Job controls and recovery

- `create_code_job` accepts `merge_policy: auto|manual` (default `auto`). Manual jobs prepare a reviewed, CI-gated PR and wait for `approve_code_job_merge`; all merge gates remain enforced.
- `pause_code_job` pauses progression, not already running agents. Their outcomes remain durable until `resume_code_job`.
- `cancel_code_job` requests remote cancellation and preserves worktrees and audit history.
- `archive_code_job` moves terminal job/dispatch history out of active snapshots, preserves inspection and source worktrees, and unregisters its runtime routes.
- `list_notification_events` and `retry_notification_event` inspect and requeue failed delivery.
- `retry_code_planning` and `retry_code_task` recover malformed/unapplied planner, writer, and reviewer responses after an operator fixes the underlying agent configuration.
- Reconciliation periodically retries dispatch, outcome application, integration, CI remediation, and publication. MCP attachments and reference routes are reconstructed after restart.
- Required CI has a 30-minute deadline; paused time is excluded.
- Optional graph indexing refreshes committed, clean repository evidence every five minutes. Repository search hints are not authorization filters.

`/healthz`, `/readyz`, and `/metrics` use the same bearer token. `opendev --health-check --addr :8080` checks readiness locally. HTTP requests carry an `X-Request-ID`; metrics cover requests, errors, job states, and pending notifications. Shutdown responds to SIGTERM/SIGINT. Only one process may own a state directory.

## Docker

A `Dockerfile` is provided that builds the server and runs it in multi-server mode.

### Build

```sh
docker build -t ghcr.io/iamangus/opendev .
```

### Run

```sh
docker run --rm -p 127.0.0.1:8080:8080 \
  -e OPENDEV_TOKEN \
  -v opendev-repos:/repos -v opendev-data:/data \
  ghcr.io/iamangus/opendev
```

Configure the GitHub integration to discover owned repositories or use
the provisioning and fork tools on `/mcp`.

The container runs as UID/GID `10001`. Existing bind mounts/volumes must be writable by that user before upgrading. Git author/committer identity defaults to OpenDev and can be overridden with the standard `GIT_AUTHOR_*` / `GIT_COMMITTER_*` variables. Credentials are supplied to Git through process configuration, not stored in origin URLs. Existing credential-bearing origins are sanitized on their next fetch/push; rotate any previously exposed token.

Use `sha-<commit>` image tags for reproducible deployments. When releasing reusable CI changes, publish the workflow revision and explicitly migrate foundation callers to a reviewed immutable SHA; changing this checkout alone does not upgrade existing `@v4` callers.

### Environment variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `REPOS_DIR` | no | `/repos` | Root directory for repositories |
| `STATE_DIR` | no | `/data` | Durable state directory (Docker entrypoint) |
| `MCP_ADDR` | no | `:8080` | HTTP listen address |
| `OPENDEV_TOKEN` | yes | | Bearer token required for MCP endpoints |
| `OPENDEV_URL` | no | derived from `MCP_ADDR` | Public MCP URL used in dispatched job callbacks |
| `GITHUB_TOKEN` | no | | GitHub credentials for repository and PR management |
| `GITHUB_OWNER` | with GitHub integration | | GitHub user or organization |
| `GITHUB_OWNER_TYPE` | no | `user` | Set `organization` for organization discovery, creation, and forks |
| `OPENDEV_OWNED_GITHUB_ACCOUNTS` | no | `GITHUB_OWNER` | Comma-separated accounts eligible for foundation management |
| `AGENTFOUNDRY_URL` | for dispatch | | AgentFoundry API URL |
| `AGENTFOUNDRY_API_KEY` | deployment-dependent | | Existing AgentFoundry bearer credential |
| `GRAPHITI_URL` | no | | Optional repository knowledge service |
| `OPENDEV_ALLOW_EMPTY_PR_CHECKS` | no | `false` | Allow pull requests with no configured checks |
| `EVE_URL` | no | | Eve authenticated notification webhook URL. When unset, events remain durably queued in `/data/notification-outbox.json`. |
| `EVE_WEBHOOK_TOKEN` | no | | Bearer token sent to `EVE_URL`; required with `EVE_URL` to enable delivery. |

Private repository access uses the configured GitHub credentials.

## Development

Go 1.26.9 or later is required (including the HTTP/TLS standard-library security fixes). Before publishing:

```sh
go test -race -tags=integration ./...
go vet ./...
```

Integration tests use temporary local Git repositories and do not require GitHub credentials.
