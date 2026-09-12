package references

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/iamangus/code-mcp/internal/pipeline"
	"github.com/iamangus/code-mcp/internal/repositorycatalog"
)

type fakeCatalog struct {
	records map[string]*repositorycatalog.Record
	err     error
}

func (c *fakeCatalog) Get(name string) (*repositorycatalog.Record, error) {
	if c.err != nil {
		return nil, c.err
	}
	return c.records[name], nil
}

type fakeWorktreeManager struct {
	status    map[string]string
	revisions map[string]string
	addCalls  int
	err       error
	detached  map[string]bool
}

func (m *fakeWorktreeManager) Status(dir string) (string, error) {
	return m.status[dir], m.err
}

func (m *fakeWorktreeManager) ResolveRevision(dir, revision string) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	return m.revisions[dir+":"+revision], nil
}

func (m *fakeWorktreeManager) AddDetachedWorktree(_, _, _ string) error {
	m.addCalls++
	return m.err
}

func (m *fakeWorktreeManager) IsDetachedWorktree(_, worktreeDir string) (bool, error) {
	return m.detached[worktreeDir], m.err
}

func TestValidateAndSnapshotCreatesDetachedCleanSHA(t *testing.T) {
	source := t.TempDir()
	manager := &fakeWorktreeManager{
		status:   map[string]string{source: ""},
		detached: map[string]bool{},
		revisions: map[string]string{
			source + ":release": "abc123",
		},
	}
	service, err := New(&fakeCatalog{records: map[string]*repositorycatalog.Record{
		"owner/reference": {Name: "owner/reference", Path: source},
	}}, manager)
	if err != nil {
		t.Fatal(err)
	}
	snapshotPath := filepath.Join(source, ".opendev", "references", "job-1", "owner-reference")
	manager.revisions[snapshotPath+":HEAD"] = "abc123"
	manager.status[snapshotPath] = ""
	manager.detached[snapshotPath] = true

	snapshots, err := service.ValidateAndSnapshot("job-1", []pipeline.ReferenceRepository{{Repository: "owner/reference", Branch: "release", Purpose: "API context"}})
	if err != nil {
		t.Fatal(err)
	}
	if manager.addCalls != 1 {
		t.Fatalf("AddDetachedWorktree calls = %d, want 1", manager.addCalls)
	}
	if len(snapshots) != 1 || snapshots[0].SHA != "abc123" || snapshots[0].Path != snapshotPath || snapshots[0].Purpose != "API context" {
		t.Fatalf("snapshots = %+v", snapshots)
	}
}

func TestValidateAndSnapshotReusesOnlyCorrectCleanSnapshot(t *testing.T) {
	source := t.TempDir()
	snapshotPath := filepath.Join(source, ".opendev", "references", "job-1", "reference")
	if err := os.MkdirAll(snapshotPath, 0755); err != nil {
		t.Fatal(err)
	}
	manager := &fakeWorktreeManager{
		status:    map[string]string{source: "", snapshotPath: ""},
		revisions: map[string]string{source + ":HEAD": "abc123", snapshotPath + ":HEAD": "abc123"},
		detached:  map[string]bool{snapshotPath: true},
	}
	service, err := New(&fakeCatalog{records: map[string]*repositorycatalog.Record{
		"reference": {Name: "reference", Path: source},
	}}, manager)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ValidateAndSnapshot("job-1", []pipeline.ReferenceRepository{{Repository: "reference", Purpose: "context"}}); err != nil {
		t.Fatal(err)
	}
	if manager.addCalls != 0 {
		t.Fatalf("AddDetachedWorktree calls = %d, want 0", manager.addCalls)
	}

	manager.revisions[snapshotPath+":HEAD"] = "wrong"
	if _, err := service.ValidateAndSnapshot("job-1", []pipeline.ReferenceRepository{{Repository: "reference", Purpose: "context"}}); err == nil {
		t.Fatal("ValidateAndSnapshot succeeded with wrong snapshot SHA")
	}
	manager.revisions[snapshotPath+":HEAD"] = "abc123"
	manager.status[snapshotPath] = " M changed.go"
	if _, err := service.ValidateAndSnapshot("job-1", []pipeline.ReferenceRepository{{Repository: "reference", Purpose: "context"}}); err == nil {
		t.Fatal("ValidateAndSnapshot succeeded with dirty snapshot")
	}
}

func TestValidateAndSnapshotRejectsUnknownDirtyAndAmbiguousReferences(t *testing.T) {
	source := t.TempDir()
	manager := &fakeWorktreeManager{status: map[string]string{source: " M changed.go"}, revisions: map[string]string{}, detached: map[string]bool{}}
	service, err := New(&fakeCatalog{records: map[string]*repositorycatalog.Record{
		"reference": {Name: "reference", Path: source},
	}}, manager)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ValidateAndSnapshot("job-1", []pipeline.ReferenceRepository{{Repository: "missing", Purpose: "context"}}); err == nil {
		t.Fatal("ValidateAndSnapshot succeeded for unknown catalog record")
	}
	if _, err := service.ValidateAndSnapshot("job-1", []pipeline.ReferenceRepository{{Repository: "reference", Purpose: "context"}}); err == nil {
		t.Fatal("ValidateAndSnapshot succeeded for dirty source")
	}
	manager.status[source] = ""
	if _, err := service.ValidateAndSnapshot("job-1", []pipeline.ReferenceRepository{{Repository: "reference", Branch: "main", Revision: "abc123", Purpose: "context"}}); err == nil {
		t.Fatal("ValidateAndSnapshot succeeded with branch and revision")
	}
	manager.err = errors.New("git failed")
	if _, err := service.ValidateAndSnapshot("job-1", []pipeline.ReferenceRepository{{Repository: "reference", Purpose: "context"}}); err == nil {
		t.Fatal("ValidateAndSnapshot succeeded after Git error")
	}
}
