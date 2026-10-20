//go:build integration

package main

import (
	"bytes"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iamangus/code-mcp/internal/pipeline"
)

func TestMultiServerRestoresAuthenticatedRuntimeRoutes(t *testing.T) {
	if os.Getenv("OPENDEV_ROUTING_HELPER") == "1" {
		if err := runMultiServer(os.Getenv("OPENDEV_TEST_ADDR"), os.Getenv("OPENDEV_TEST_REPOS"), os.Getenv("OPENDEV_TEST_STATE"), "", nil, slog.Default()); err != nil {
			t.Fatal(err)
		}
		return
	}
	root := t.TempDir()
	repos := filepath.Join(root, "repos")
	repo := filepath.Join(repos, "repo")
	os.MkdirAll(repo, 0755)
	initGitRepo(t, repo)
	runtime := filepath.Join(repo, ".opendev", "worktrees", "legacy-feature")
	command := exec.Command("git", "worktree", "add", "-b", "feature/login", runtime, "main")
	command.Dir = repo
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("worktree: %v %s", err, out)
	}
	state := filepath.Join(root, "state")
	store, _ := pipeline.NewStore(state)
	job, _ := store.Create("repo", "directive", "feature/login", "", "p", "w", "r")
	store.StartPlanning(job.ID)
	store.SubmitPlan(job.ID, pipeline.Plan{Summary: "plan", Tasks: []pipeline.Task{{Key: "one", Title: "One", Description: "One", AcceptanceCriteria: []string{"works"}}}, IntegrationOrder: []string{"one"}})
	reference := filepath.Join(root, "reference")
	os.MkdirAll(reference, 0755)
	if _, err := store.RecordReferenceSnapshots(job.ID, []pipeline.ReferenceSnapshot{{Repository: "reference", SHA: "sha", Path: reference, Purpose: "context"}}); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	executable, _ := os.Executable()
	child := exec.Command(executable, "-test.run=^TestMultiServerRestoresAuthenticatedRuntimeRoutes$")
	child.Env = append(os.Environ(), "OPENDEV_ROUTING_HELPER=1", "OPENDEV_TEST_ADDR="+address, "OPENDEV_TEST_REPOS="+repos, "OPENDEV_TEST_STATE="+state, "OPENDEV_TOKEN=test-secret", "AGENTFOUNDRY_URL=", "GRAPHITI_URL=", "EVE_URL=")
	var output bytes.Buffer
	child.Stdout, child.Stderr = &output, &output
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = child.Process.Signal(os.Interrupt)
		if err := child.Wait(); err != nil {
			t.Errorf("server exit: %v\n%s", err, output.String())
		}
	}()
	client := &http.Client{Timeout: time.Second}
	send := func(path, authorization string) (int, error) {
		request, _ := http.NewRequest(http.MethodPost, "http://"+address+path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		request.Header.Set("Authorization", authorization)
		response, err := client.Do(request)
		if err != nil {
			return 0, err
		}
		defer response.Body.Close()
		return response.StatusCode, nil
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		status, err := send("/readyz", "Bearer test-secret")
		if err == nil && status == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server never became ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, test := range []struct {
		path, auth string
		status     int
	}{
		{"/repo/feature%2Flogin/read/mcp", "Bearer test-secret", 200},
		{"/references/" + job.ID + "/reference/read/mcp", "Bearer test-secret", 200},
		{"/repo/main/write/mcp", "Bearer test-secret", 404},
		{"/repo/main/read/mcp", "test-secret", 401},
	} {
		status, err := send(test.path, test.auth)
		if err != nil || status != test.status {
			t.Errorf("%s: got %d %v, want %d", test.path, status, err, test.status)
		}
	}
}
