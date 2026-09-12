package repositoryindex

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/iamangus/code-mcp/internal/graphiti"
)

const (
	// StatusComplete identifies content successfully accepted by the graph service.
	StatusComplete = "complete"
	// StatusFailed identifies an attempted ingestion that failed.
	StatusFailed = "failed"

	maxContentPerFile = 16 * 1024
	maxContentTotal   = 64 * 1024
	maxEvidenceFiles  = 128
	maxEvidenceLabel  = 512
)

// Snapshot is caller-supplied repository evidence to index. Docs and Manifests
// map repository-relative paths to their content.
type Snapshot struct {
	Name          string
	Path          string
	SHA           string
	OriginURL     string
	DefaultBranch string
	README        string
	Docs          map[string]string
	Manifests     map[string]string
}

// StateStore is the durable state needed to avoid duplicate successful ingestions.
type StateStore interface {
	Get(string) (*State, error)
	Save(State) (*State, error)
}

// IngestionClient accepts graph evidence.
type IngestionClient interface {
	AddMessages(context.Context, []graphiti.Message) error
}

// Indexer programmatically turns bounded repository evidence into graph ingestion messages.
type Indexer struct {
	store  StateStore
	client IngestionClient
}

// NewIndexer creates an indexer with independent persistence and graph client dependencies.
func NewIndexer(store StateStore, client IngestionClient) (*Indexer, error) {
	if store == nil {
		return nil, fmt.Errorf("repository index store is required")
	}
	if client == nil {
		return nil, fmt.Errorf("Graphiti ingestion client is required")
	}
	return &Indexer{store: store, client: client}, nil
}

// Index ingests snapshot evidence unless the same SHA and content digest completed previously.
// It records every attempted outcome so a failed ingestion can be retried.
func (i *Indexer) Index(ctx context.Context, snapshot Snapshot) (*State, error) {
	if err := validateSnapshot(snapshot); err != nil {
		return nil, err
	}
	digest := snapshotDigest(snapshot)
	previous, err := i.store.Get(snapshot.Name)
	if err != nil {
		return nil, fmt.Errorf("get repository index state: %w", err)
	}
	if previous != nil && previous.Status == StatusComplete && previous.IndexedSHA == snapshot.SHA && previous.ContentDigest == digest {
		return previous, nil
	}
	message := graphiti.Message{
		Content:           buildEvidence(snapshot),
		RoleType:          "user",
		Name:              "opendev-repository-indexer",
		SourceDescription: fmt.Sprintf("repository %s at commit %s", snapshot.Name, snapshot.SHA),
		Timestamp:         time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := i.client.AddMessages(ctx, []graphiti.Message{message}); err != nil {
		failed, saveErr := i.store.Save(State{Repository: snapshot.Name, IndexedSHA: snapshot.SHA, ContentDigest: digest, Status: StatusFailed, Error: err.Error()})
		if saveErr != nil {
			return failed, fmt.Errorf("ingest repository evidence: %w; record failure: %v", err, saveErr)
		}
		return failed, fmt.Errorf("ingest repository evidence: %w", err)
	}
	state, err := i.store.Save(State{Repository: snapshot.Name, IndexedSHA: snapshot.SHA, ContentDigest: digest, Status: StatusComplete, IndexedAt: time.Now().UTC()})
	if err != nil {
		return nil, fmt.Errorf("record completed repository index: %w", err)
	}
	return state, nil
}

func validateSnapshot(snapshot Snapshot) error {
	if strings.TrimSpace(snapshot.Name) == "" {
		return fmt.Errorf("repository name is required")
	}
	if strings.TrimSpace(snapshot.Path) == "" {
		return fmt.Errorf("repository path is required")
	}
	if strings.TrimSpace(snapshot.SHA) == "" {
		return fmt.Errorf("repository SHA is required")
	}
	return nil
}

func snapshotDigest(snapshot Snapshot) string {
	hash := sha256.New()
	writeDigestField(hash, "name", snapshot.Name)
	writeDigestField(hash, "path", snapshot.Path)
	writeDigestField(hash, "sha", snapshot.SHA)
	writeDigestField(hash, "origin", snapshot.OriginURL)
	writeDigestField(hash, "default_branch", snapshot.DefaultBranch)
	writeDigestField(hash, "README.md", snapshot.README)
	writeDigestFiles(hash, "docs", snapshot.Docs)
	writeDigestFiles(hash, "manifests", snapshot.Manifests)
	return fmt.Sprintf("sha256:%x", hash.Sum(nil))
}

func writeDigestFiles(hash interface{ Write([]byte) (int, error) }, category string, files map[string]string) {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		writeDigestField(hash, category+":"+path, files[path])
	}
}

func writeDigestField(hash interface{ Write([]byte) (int, error) }, key, value string) {
	_, _ = hash.Write([]byte(key))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(value))
	_, _ = hash.Write([]byte{0})
}

func buildEvidence(snapshot Snapshot) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Repository: %s\nPath: %s\nCommit SHA: %s\n", snapshot.Name, snapshot.Path, snapshot.SHA)
	if origin := strings.TrimSpace(snapshot.OriginURL); origin != "" {
		fmt.Fprintf(&builder, "Origin URL: %s\n", origin)
	}
	if branch := strings.TrimSpace(snapshot.DefaultBranch); branch != "" {
		fmt.Fprintf(&builder, "Default branch: %s\n", branch)
	}
	remaining := maxContentTotal
	appendEvidence(&builder, &remaining, "README.md", snapshot.README)
	filesRemaining := maxEvidenceFiles
	appendFileEvidence(&builder, &remaining, &filesRemaining, "Documentation", snapshot.Docs)
	appendFileEvidence(&builder, &remaining, &filesRemaining, "Manifest", snapshot.Manifests)
	return builder.String()
}

func appendFileEvidence(builder *strings.Builder, remaining, filesRemaining *int, category string, files map[string]string) {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if *filesRemaining == 0 {
			return
		}
		appendEvidence(builder, remaining, category+" ("+path+")", files[path])
		*filesRemaining--
	}
}

func appendEvidence(builder *strings.Builder, remaining *int, label, content string) {
	if *remaining <= 0 || strings.TrimSpace(content) == "" {
		return
	}
	limit := min(maxContentPerFile, *remaining)
	truncated := len(content) > limit
	if truncated {
		content = content[:limit]
	}
	if len(label) > maxEvidenceLabel {
		label = label[:maxEvidenceLabel] + "..."
	}
	fmt.Fprintf(builder, "\n%s:\n%s\n", label, content)
	*remaining -= len(content)
	if truncated {
		builder.WriteString("[content truncated]\n")
	}
}

var _ IngestionClient = (*graphiti.Client)(nil)
