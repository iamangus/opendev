package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/iamangus/code-mcp/internal/outbox"
	"github.com/iamangus/code-mcp/internal/pipeline"
)

var requestSequence atomic.Uint64
var requestCount atomic.Uint64
var httpErrorCount atomic.Uint64

type observedResponse struct {
	http.ResponseWriter
	status int
}

func (w *observedResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *observedResponse) WriteHeader(code int) {
	if w.status != 0 {
		return
	}
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}
func (w *observedResponse) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}
func (w *observedResponse) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func observeHTTP(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := fmt.Sprintf("%x-%x", time.Now().UnixNano(), requestSequence.Add(1))
		w.Header().Set("X-Request-ID", id)
		response := &observedResponse{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(response, r)
		requestCount.Add(1)
		if response.status >= 400 {
			httpErrorCount.Add(1)
		}
		if r.URL.Path != "/readyz" && r.URL.Path != "/healthz" {
			logger.Info("HTTP request completed", "request_id", id, "method", r.Method, "path", r.URL.EscapedPath(), "status", response.status, "duration_ms", time.Since(start).Milliseconds())
		}
	})
}

func registerMetrics(mux *http.ServeMux, jobs *pipeline.Store, notifications *outbox.Store) {
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "# TYPE opendev_http_requests_total counter\nopendev_http_requests_total %d\n# TYPE opendev_http_errors_total counter\nopendev_http_errors_total %d\n", requestCount.Load(), httpErrorCount.Load())
		for status, count := range jobs.StatusCounts() {
			fmt.Fprintf(w, "opendev_jobs{status=%q} %d\n", status, count)
		}
		fmt.Fprintf(w, "opendev_notifications_pending %d\n", notifications.PendingCount())
	})
}
