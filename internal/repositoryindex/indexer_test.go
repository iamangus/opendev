package repositoryindex

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/iamangus/code-mcp/internal/graphiti"
)

type memoryStore struct {
	states map[string]State
}

func (s *memoryStore) Get(repository string) (*State, error) {
	state, ok := s.states[repository]
	if !ok {
		return nil, nil
	}
	return &state, nil
}

func (s *memoryStore) Save(state State) (*State, error) {
	s.states[state.Repository] = state
	return &state, nil
}

type fakeIngestionClient struct {
	messages [][]graphiti.Message
	err      error
}

func (c *fakeIngestionClient) AddMessages(_ context.Context, messages []graphiti.Message) error {
	c.messages = append(c.messages, messages)
	return c.err
}

func TestIndexerCompletesAndSkipsIdenticalSuccessfulSnapshot(t *testing.T) {
	store := &memoryStore{states: make(map[string]State)}
	client := &fakeIngestionClient{}
	indexer, err := NewIndexer(store, client)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{
		Name:          "owner/repo",
		Path:          "/repos/repo",
		SHA:           "abc123",
		OriginURL:     "https://github.com/owner/repo.git",
		DefaultBranch: "main",
		README:        "Repository documentation",
		Docs:          map[string]string{"docs/design.md": "Design evidence"},
		Manifests:     map[string]string{"go.mod": "module example.test/repo"},
	}
	state, err := indexer.Index(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != StatusComplete || state.IndexedAt.IsZero() || len(client.messages) != 1 {
		t.Fatalf("Index = %+v; calls = %d", state, len(client.messages))
	}
	evidence := client.messages[0][0].Content
	message := client.messages[0][0]
	if message.RoleType != "user" || message.Name != "opendev-repository-indexer" || message.SourceDescription != "repository owner/repo at commit abc123" || message.Timestamp == "" {
		t.Fatalf("unexpected Graphiti message metadata: %+v", message)
	}
	for _, want := range []string{"Repository: owner/repo", "Path: /repos/repo", "Commit SHA: abc123", "docs/design.md", "go.mod"} {
		if !strings.Contains(evidence, want) {
			t.Errorf("evidence does not contain %q: %s", want, evidence)
		}
	}
	if _, err := indexer.Index(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if len(client.messages) != 1 {
		t.Fatalf("duplicate snapshot calls = %d, want 1", len(client.messages))
	}
}

func TestIndexerRecordsFailureAndBoundsEvidence(t *testing.T) {
	store := &memoryStore{states: make(map[string]State)}
	client := &fakeIngestionClient{err: errors.New("Graphiti unavailable")}
	indexer, err := NewIndexer(store, client)
	if err != nil {
		t.Fatal(err)
	}
	_, err = indexer.Index(context.Background(), Snapshot{Name: "repo", Path: "/repos/repo", SHA: "abc", README: strings.Repeat("x", maxContentPerFile+1)})
	if err == nil || !strings.Contains(err.Error(), "Graphiti unavailable") {
		t.Fatalf("Index error = %v", err)
	}
	state, err := store.Get("repo")
	if err != nil || state == nil || state.Status != StatusFailed || state.Error != "Graphiti unavailable" {
		t.Fatalf("failed state = %+v, %v", state, err)
	}
	if len(client.messages) != 1 || !strings.Contains(client.messages[0][0].Content, "[content truncated]") || strings.Count(client.messages[0][0].Content, "x") > maxContentPerFile {
		t.Fatalf("unbounded evidence: %d bytes", len(client.messages[0][0].Content))
	}
}

func TestIndexerBoundsManySmallFiles(t *testing.T) {
	docs := make(map[string]string, maxEvidenceFiles+1)
	for n := range maxEvidenceFiles + 1 {
		docs[strings.Repeat("p", maxEvidenceLabel+1)+string(rune(n))] = "x"
	}
	evidence := buildEvidence(Snapshot{Name: "repo", Path: "/repos/repo", SHA: "abc", Docs: docs})
	if strings.Count(evidence, "Documentation (") != maxEvidenceFiles {
		t.Fatalf("document count = %d, want %d", strings.Count(evidence, "Documentation ("), maxEvidenceFiles)
	}
	if len(evidence) > maxContentTotal+maxEvidenceFiles*(maxEvidenceLabel+20) {
		t.Fatalf("evidence size = %d", len(evidence))
	}
}
