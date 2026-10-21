# ---- builder ----
FROM golang:1.27.2-alpine AS builder

WORKDIR /src

# Cache module downloads separately from source
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /opendev ./cmd/opendev

# ---- runtime ----
FROM debian:bookworm-slim

# git              – needed for clone/worktree operations
# ca-certificates  – needed for HTTPS clones
# ripgrep          – fast ignore-aware search backend for the grep_search tool
RUN apt-get update -qq \
 && apt-get install -y --no-install-recommends git ca-certificates ripgrep \
 && rm -rf /var/lib/apt/lists/*

COPY --from=builder /opendev /usr/local/bin/opendev
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# Repository mirrors and durable job state must survive restarts.
RUN groupadd --gid 10001 opendev \
 && useradd --uid 10001 --gid opendev --create-home opendev \
 && mkdir -p /repos /data \
 && chown opendev:opendev /repos /data
ENV GIT_AUTHOR_NAME="OpenDev" GIT_AUTHOR_EMAIL="opendev@users.noreply.github.com" \
    GIT_COMMITTER_NAME="OpenDev" GIT_COMMITTER_EMAIL="opendev@users.noreply.github.com"
USER opendev
VOLUME ["/repos", "/data"]

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=60s CMD opendev --health-check --addr "${MCP_ADDR:-:8080}"

ENTRYPOINT ["/entrypoint.sh"]
