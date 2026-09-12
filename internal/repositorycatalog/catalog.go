// Package repositorycatalog stores durable metadata about managed repositories.
package repositorycatalog

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/iamangus/code-mcp/internal/manager"
)

// GitMetadata is metadata read from a local Git clone during a catalog refresh.
type GitMetadata struct {
	OriginURL string
	HeadSHA   string
	Aliases   []string
	Domains   []string
	Topics    []string
}

// GitMetadataReader reads Git metadata without coupling the catalog to a Git implementation.
type GitMetadataReader interface {
	ReadGitMetadata(context.Context, string) (GitMetadata, error)
}

// Record is one repository's durable catalog entry.
type Record struct {
	Name          string    `json:"name"`
	Path          string    `json:"path"`
	OriginURL     string    `json:"origin_url,omitempty"`
	DefaultBranch string    `json:"default_branch,omitempty"`
	HeadSHA       string    `json:"head_sha,omitempty"`
	Aliases       []string  `json:"aliases,omitempty"`
	Domains       []string  `json:"domains,omitempty"`
	Topics        []string  `json:"topics,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type persisted struct {
	Records []Record `json:"records"`
}

// Catalog atomically persists records keyed by canonical repository name.
type Catalog struct {
	path    string
	reader  GitMetadataReader
	mu      sync.RWMutex
	records map[string]Record
}

// New opens a catalog in dataDir. reader is used by Refresh to read local Git metadata.
func New(dataDir string, reader GitMetadataReader) (*Catalog, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, fmt.Errorf("repository catalog data directory is required")
	}
	if reader == nil {
		return nil, fmt.Errorf("repository catalog Git metadata reader is required")
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("create repository catalog data directory: %w", err)
	}
	c := &Catalog{
		path:    filepath.Join(dataDir, "repository-catalog.json"),
		reader:  reader,
		records: make(map[string]Record),
	}
	if err := c.load(); err != nil {
		return nil, err
	}
	return c, nil
}

// Refresh reads Git metadata for repo and atomically records the resulting catalog entry.
func (c *Catalog) Refresh(ctx context.Context, repo manager.RepoInfo) (*Record, error) {
	name, err := canonicalName(repo.Name)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(repo.Dir) == "" {
		return nil, fmt.Errorf("repository path is required")
	}
	metadata, err := c.reader.ReadGitMetadata(ctx, repo.Dir)
	if err != nil {
		return nil, fmt.Errorf("read Git metadata for %s: %w", name, err)
	}
	return c.Save(Record{
		Name:          name,
		Path:          repo.Dir,
		OriginURL:     strings.TrimSpace(metadata.OriginURL),
		DefaultBranch: strings.TrimSpace(repo.DefaultBranch),
		HeadSHA:       strings.TrimSpace(metadata.HeadSHA),
		Aliases:       normalizedStrings(metadata.Aliases),
		Domains:       normalizedStrings(metadata.Domains),
		Topics:        normalizedStrings(metadata.Topics),
	})
}

// Get returns a copy of a record, or nil when it is unknown.
func (c *Catalog) Get(name string) (*Record, error) {
	canonical, err := canonicalName(name)
	if err != nil {
		return nil, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	record, ok := c.records[canonical]
	if !ok {
		return nil, nil
	}
	return &record, nil
}

// Save records an entry and atomically replaces the persisted catalog.
func (c *Catalog) Save(record Record) (*Record, error) {
	name, err := canonicalName(record.Name)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(record.Path) == "" {
		return nil, fmt.Errorf("repository path is required")
	}
	record.Name = name
	record.Aliases = normalizedStrings(record.Aliases)
	record.Domains = normalizedStrings(record.Domains)
	record.Topics = normalizedStrings(record.Topics)
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now().UTC()
	if previous, ok := c.records[name]; ok {
		record.CreatedAt = previous.CreatedAt
	} else if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	}
	record.UpdatedAt = now
	if err := c.saveLocked(record); err != nil {
		return nil, err
	}
	c.records[name] = record
	return &record, nil
}

func (c *Catalog) load() error {
	data, err := os.ReadFile(c.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read repository catalog: %w", err)
	}
	var saved persisted
	if err := json.Unmarshal(data, &saved); err != nil {
		return fmt.Errorf("decode repository catalog: %w", err)
	}
	for _, record := range saved.Records {
		name, err := canonicalName(record.Name)
		if err != nil || strings.TrimSpace(record.Path) == "" {
			return fmt.Errorf("decode repository catalog: invalid record")
		}
		record.Name = name
		c.records[name] = record
	}
	return nil
}

func (c *Catalog) saveLocked(next Record) error {
	records := make([]Record, 0, len(c.records)+1)
	for name, record := range c.records {
		if name != next.Name {
			records = append(records, record)
		}
	}
	records = append(records, next)
	sort.Slice(records, func(i, j int) bool { return records[i].Name < records[j].Name })
	data, err := json.MarshalIndent(persisted{Records: records}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode repository catalog: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.path), ".repository-catalog-*.tmp")
	if err != nil {
		return fmt.Errorf("create repository catalog temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write repository catalog: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set repository catalog permissions: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync repository catalog: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close repository catalog: %w", err)
	}
	if err := os.Rename(tmpName, c.path); err != nil {
		return fmt.Errorf("replace repository catalog: %w", err)
	}
	return nil
}

func canonicalName(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return "", fmt.Errorf("repository name is required")
	}
	return name, nil
}

func normalizedStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			seen[value] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
