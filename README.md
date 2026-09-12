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

Management API endpoints:

```
GET    /api/repos
POST   /api/repos
DELETE /api/repos/{repo}
GET    /api/repos/{repo}/branches
POST   /api/repos/{repo}/branches
DELETE /api/repos/{repo}/branches/{branch}
POST   /api/repos/{repo}/branches/{branch}/push
POST   /api/repos/{repo}/branches/{branch}/merge
GET    /api/repos/{repo}/branches/{branch}/commits
POST   /api/repos/{repo}/branches/{branch}/test/run
POST   /api/repos/{repo}/pulls
PATCH  /api/repos/{repo}/pulls/{number}
POST   /api/repos/{repo}/pulls/{number}/ready
```

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

Use the management API to add repositories after startup:

```sh
curl -X POST http://localhost:8080/api/repos \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://github.com/owner/repo.git","name":"myrepo"}'
```

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

> **Private repositories** — set `GIT_TOKEN` on the clone request body or embed it in the URL (`https://TOKEN@host/…`).
