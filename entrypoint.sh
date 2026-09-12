#!/bin/sh
set -e

# ---------------------------------------------------------------------------
# Multi-repo MCP server entrypoint
#
# The server scans REPOS_DIR (default: /repos) at startup to discover all
# existing repositories and their worktrees. Coding job state is persisted in
# STATE_DIR; repository lifecycle is migrating to MCP tools.
#
# Environment variables:
#   REPOS_DIR   Root directory for repositories (default: /repos)
#   STATE_DIR   Root directory for durable coding job state (default: /data)
#   OPENDEV_TOKEN   Required bearer token for every MCP endpoint
#   MCP_ADDR    HTTP listen address              (default: :8080)
# ---------------------------------------------------------------------------

REPOS_DIR="${REPOS_DIR:-/repos}"
STATE_DIR="${STATE_DIR:-/data}"
MCP_ADDR="${MCP_ADDR:-:8080}"

mkdir -p "$REPOS_DIR"
mkdir -p "$STATE_DIR"

exec /usr/local/bin/opendev \
    --repos-dir "$REPOS_DIR" \
    --state-dir "$STATE_DIR" \
    --addr      "$MCP_ADDR"
