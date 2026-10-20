// Package references creates immutable, clean reference repository snapshots.
package references

import (
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/iamangus/code-mcp/internal/pipeline"
	"github.com/iamangus/code-mcp/internal/repositorycatalog"
	"github.com/iamangus/code-mcp/internal/worktree"
)

var validJobID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Catalog looks up durable, locally managed repository records.
type Catalog interface {
	Get(name string) (*repositorycatalog.Record, error)
}

// WorktreeManager performs local Git operations for reference snapshots.
type WorktreeManager interface {
	Status(dir string) (string, error)
	ResolveRevision(dir, revision string) (string, error)
	AddDetachedWorktree(repoDir, worktreeDir, revision string) error
	IsDetachedWorktree(repoDir, worktreeDir string) (bool, error)
}

// Snapshot describes a detached reference repository snapshot.
type Snapshot struct {
	Repository string `json:"repository"`
	SHA        string `json:"sha"`
	Path       string `json:"path"`
	Purpose    string `json:"purpose"`
}

// Service validates reference repositories against the local catalog and snapshots them.
type Service struct {
	catalog Catalog
	manager WorktreeManager
	mu      sync.Mutex
}

// New creates a reference snapshot service.
func New(catalog Catalog, manager WorktreeManager) (*Service, error) {
	if catalog == nil {
		return nil, fmt.Errorf("reference repository catalog is required")
	}
	if manager == nil {
		return nil, fmt.Errorf("reference worktree manager is required")
	}
	return &Service{catalog: catalog, manager: manager}, nil
}

// ValidateAndSnapshot creates or reuses clean detached snapshots for a job's references.
// Repositories are resolved exclusively through existing local catalog records.
func (s *Service) ValidateAndSnapshot(jobID string, references []pipeline.ReferenceRepository) ([]Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validJobID.MatchString(jobID) || strings.Contains(jobID, "..") {
		return nil, fmt.Errorf("invalid job ID %q", jobID)
	}

	snapshots := make([]Snapshot, 0, len(references))
	paths := make(map[string]struct{}, len(references))
	for _, reference := range references {
		record, err := s.catalog.Get(reference.Repository)
		if err != nil {
			return nil, fmt.Errorf("look up reference repository %q: %w", reference.Repository, err)
		}
		if record == nil {
			return nil, fmt.Errorf("reference repository %q is not in the local catalog", reference.Repository)
		}
		if status, err := s.manager.Status(record.Path); err != nil {
			return nil, fmt.Errorf("check reference repository %q: %w", record.Name, err)
		} else if status != "" {
			return nil, fmt.Errorf("reference repository %q has uncommitted changes", record.Name)
		}

		revision, err := referenceRevision(reference)
		if err != nil {
			return nil, fmt.Errorf("reference repository %q: %w", record.Name, err)
		}
		sha, err := s.manager.ResolveRevision(record.Path, revision)
		if err != nil {
			return nil, fmt.Errorf("resolve reference repository %q: %w", record.Name, err)
		}
		if sha == "" {
			return nil, fmt.Errorf("resolve reference repository %q: empty commit SHA", record.Name)
		}

		path := filepath.Join(record.Path, ".opendev", "references", jobID, safeName(record.Name))
		if _, err := worktree.ResolveManaged(record.Path, path); err != nil {
			return nil, err
		}
		sha, err = pinRevision(record.Path, path, sha, strings.TrimSpace(reference.Revision) != "")
		if err != nil {
			return nil, fmt.Errorf("pin reference repository %q: %w", record.Name, err)
		}
		if _, duplicate := paths[path]; duplicate {
			return nil, fmt.Errorf("reference repositories resolve to the same snapshot path %q", path)
		}
		paths[path] = struct{}{}
		if err := s.ensureSnapshot(record.Path, path, sha); err != nil {
			return nil, fmt.Errorf("snapshot reference repository %q: %w", record.Name, err)
		}
		snapshots = append(snapshots, Snapshot{Repository: record.Name, SHA: sha, Path: path, Purpose: reference.Purpose})
	}
	return snapshots, nil
}

// Persist the selected branch head before creating its worktree. If a process
// crashes between the filesystem side effect and the job write, a moved source
// branch must not silently select a different snapshot on retry.
func pinRevision(root, path, sha string, explicit bool) (string, error) {
	if sha == "" || len(sha) > 128 {
		return "", fmt.Errorf("invalid reference revision")
	}
	pin := path + ".revision"
	if _, err := worktree.ResolveManaged(root, pin); err != nil {
		return "", err
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer r.Close()
	rel, err := filepath.Rel(root, pin)
	if err != nil {
		return "", err
	}
	if err := r.MkdirAll(filepath.Dir(rel), 0750); err != nil {
		return "", err
	}
	readPinned := func() (string, error) {
		f, err := r.Open(rel)
		if err != nil {
			return "", err
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, 129))
		if err != nil {
			return "", err
		}
		stored := strings.TrimSpace(string(data))
		if stored == "" || len(data) > 128 {
			return "", fmt.Errorf("invalid pinned revision")
		}
		if explicit && stored != sha {
			return "", fmt.Errorf("requested revision differs from the immutable job snapshot")
		}
		return stored, nil
	}
	if _, err := r.Lstat(rel); err == nil {
		return readPinned()
	} else if !os.IsNotExist(err) {
		return "", err
	}
	temporary := rel + "." + rand.Text() + ".tmp"
	f, err := r.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	defer r.Remove(temporary)
	if _, err = f.WriteString(sha); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	if err := r.Link(temporary, rel); os.IsExist(err) {
		return readPinned()
	} else if err != nil {
		return "", err
	}
	dir, err := r.Open(filepath.Dir(rel))
	if err != nil {
		return "", err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return "", err
	}
	return sha, nil
}

func (s *Service) ensureSnapshot(sourcePath, snapshotPath, sha string) error {
	_, err := os.Lstat(snapshotPath)
	switch {
	case err == nil:
		// An existing path must be the exact clean snapshot requested by this job.
	case os.IsNotExist(err):
		if err := os.MkdirAll(filepath.Dir(snapshotPath), 0o750); err != nil {
			return fmt.Errorf("create snapshot directory: %w", err)
		}
		if err := s.manager.AddDetachedWorktree(sourcePath, snapshotPath, sha); err != nil {
			return err
		}
	default:
		return fmt.Errorf("inspect snapshot path: %w", err)
	}

	head, err := s.manager.ResolveRevision(snapshotPath, "HEAD")
	if err != nil {
		return fmt.Errorf("read snapshot revision: %w", err)
	}
	if head != sha {
		return fmt.Errorf("snapshot revision %q does not match requested SHA %q", head, sha)
	}
	detached, err := s.manager.IsDetachedWorktree(sourcePath, snapshotPath)
	if err != nil {
		return fmt.Errorf("check detached snapshot: %w", err)
	}
	if !detached {
		return fmt.Errorf("snapshot is not detached")
	}
	status, err := s.manager.Status(snapshotPath)
	if err != nil {
		return fmt.Errorf("check snapshot status: %w", err)
	}
	if status != "" {
		return fmt.Errorf("snapshot has uncommitted changes")
	}
	return nil
}

func referenceRevision(reference pipeline.ReferenceRepository) (string, error) {
	branch := strings.TrimSpace(reference.Branch)
	revision := strings.TrimSpace(reference.Revision)
	if branch != "" && revision != "" {
		return "", fmt.Errorf("branch and revision cannot both be set")
	}
	if revision != "" {
		return revision, nil
	}
	if branch != "" {
		return branch, nil
	}
	return "HEAD", nil
}

func safeName(name string) string {
	var result strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			result.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			result.WriteByte('-')
			lastDash = true
		}
	}
	name = strings.Trim(result.String(), "-")
	if name == "" {
		return "repository"
	}
	return name
}
