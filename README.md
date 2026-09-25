# opendev

A Go MCP server that exposes coding tools scoped to a single Git worktree (branch).

## Tool profiles

Tools are grouped into named **profiles**. Each profile is served at a separate endpoint so consumers only see the tools they need.

| Profile | Endpoint suffix | Tools |
|---------|-----------------|-------|
| `read`  | `/read/mcp`     | `read_file`, `read_lines`, `list_directory`, `grep_search`, `get_git_diff` |
| `write` | `/write/mcp`    | `create_file`, `search_and_replace` |

## Tools

### read profile

| Tool | Parameters | Description |
|------|------------|-------------|
| `read_file` | `filepath` | Read the entire contents of a file |
| `read_lines` | `filepath`, `start_line`, `end_line` | Read a line range (1-indexed, inclusive) |
| `list_directory` | `dirpath`, `recursive` | List directory contents |
| `grep_search` | `query`, `directory?` | Search for a regex/literal pattern across files |
| `get_git_diff` | *(none)* | Show `git diff HEAD` and `git status --short` |

### write profile

| Tool | Parameters | Description |
|------|------------|-------------|
| `create_file` | `filepath`, `content` | Create a new file (fails if it already exists) |
| `search_and_replace` | `filepath`, `search_block`, `replace_block` | Replace a block of text (exact then fuzzy) |

All `filepath` and `dirpath` values are relative to the worktree root. Path traversal outside the root is rejected.

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

## Docker

A `Dockerfile` is provided that builds the server and runs it in multi-server mode.

### Build

```sh
docker build -t ghcr.io/iamangus/opendev .
```

### Run

```sh
docker run --rm -p 8080:8080 ghcr.io/iamangus/opendev
```

Configure the GitHub integration to discover owned repositories or use
the provisioning and fork tools on `/mcp`.

### Environment variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `REPOS_DIR` | no | `/repos` | Root directory for repositories |
| `MCP_ADDR` | no | `:8080` | HTTP listen address |
| `OPENDEV_TOKEN` | yes | | Bearer token required for MCP endpoints |
| `OPENDEV_URL` | no | derived from `MCP_ADDR` | Public MCP URL used in dispatched job callbacks |
| `OPENDEV_ALLOW_EMPTY_PR_CHECKS` | no | `false` | Allow pull requests with no configured checks |
| `EVE_URL` | no | | Eve authenticated notification webhook URL. When unset, events remain durably queued in `/data/notification-outbox.json`. |
| `EVE_WEBHOOK_TOKEN` | no | | Bearer token sent to `EVE_URL`; required with `EVE_URL` to enable delivery. |

Private repository access uses the configured GitHub credentials.
