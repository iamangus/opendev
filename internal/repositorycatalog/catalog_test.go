package repositorycatalog

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/iamangus/code-mcp/internal/manager"
)

type fakeMetadataReader struct {
	metadata GitMetadata
	err      error
	path     string
}

func (r *fakeMetadataReader) ReadGitMetadata(_ context.Context, path string) (GitMetadata, error) {
	r.path = path
	return r.metadata, r.err
}

func TestCatalogRefreshPersistsCanonicalRepositoryRecord(t *testing.T) {
	dir := t.TempDir()
	reader := &fakeMetadataReader{metadata: GitMetadata{
		OriginURL: "https://x-access-token:secret@github.com/Owner/Repo.git",
		HeadSHA:   "abc123",
		Aliases:   []string{"repo", "repo"},
		Domains:   []string{"api.example.test", "example.test"},
		Topics:    []string{"go", "tools", "go"},
	}}
	catalog, err := New(dir, reader)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := catalog.Refresh(context.Background(), manager.RepoInfo{Name: " Owner/Repo ", Dir: "/repos/repo", DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if reader.path != "/repos/repo" || saved.Name != "owner/repo" || saved.Path != "/repos/repo" || saved.HeadSHA != "abc123" {
		t.Fatalf("Refresh = %+v, reader path = %q", saved, reader.path)
	}
	if saved.OriginURL != "https://github.com/Owner/Repo.git" {
		t.Fatalf("OriginURL = %q", saved.OriginURL)
	}
	if !reflect.DeepEqual(saved.Aliases, []string{"repo"}) || !reflect.DeepEqual(saved.Domains, []string{"api.example.test", "example.test"}) || !reflect.DeepEqual(saved.Topics, []string{"go", "tools"}) {
		t.Fatalf("normalized fields = %+v", saved)
	}
	if saved.CreatedAt.IsZero() || saved.UpdatedAt.IsZero() {
		t.Fatalf("timestamps were not set: %+v", saved)
	}
	if info, err := os.Stat(filepath.Join(dir, "repository-catalog.json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("catalog file permissions: %v, %v", info, err)
	}
	reopened, err := New(dir, reader)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Get("OWNER/REPO")
	if err != nil || !reflect.DeepEqual(got, saved) {
		t.Fatalf("Get = %+v, %v; want %+v", got, err, saved)
	}
}

func TestCatalogRefreshDoesNotPersistMetadataReadFailure(t *testing.T) {
	catalog, err := New(t.TempDir(), &fakeMetadataReader{err: errors.New("git unavailable")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Refresh(context.Background(), manager.RepoInfo{Name: "repo", Dir: "/repos/repo"}); err == nil {
		t.Fatal("Refresh succeeded")
	}
	got, err := catalog.Get("repo")
	if err != nil || got != nil {
		t.Fatalf("Get = %+v, %v", got, err)
	}
}
