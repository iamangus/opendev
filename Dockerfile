# ---- builder ----
FROM golang:1.24-alpine AS builder

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

COPY --from=builder /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:$PATH"

COPY --from=builder /opendev /usr/local/bin/opendev
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# Repository mirrors and durable job state must survive restarts.
RUN mkdir -p /repos /data
VOLUME ["/repos", "/data"]

EXPOSE 8080

ENTRYPOINT ["/entrypoint.sh"]
