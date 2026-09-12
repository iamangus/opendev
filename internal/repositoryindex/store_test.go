package repositoryindex

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStorePersistsRepositoryIndexStateAtomically(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.Save(State{Repository: "owner/repo", IndexedSHA: "abc", ContentDigest: "sha256:def", Status: "indexed"})
	if err != nil {
		t.Fatal(err)
	}
	if saved.CreatedAt.IsZero() || saved.UpdatedAt.IsZero() {
		t.Fatalf("timestamps were not set: %+v", saved)
	}
	if info, err := os.Stat(filepath.Join(dir, "repository-index.json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state file permissions: %v, %v", info, err)
	}
	reopened, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Get("owner/repo")
	if err != nil || got == nil || got.IndexedSHA != "abc" || got.ContentDigest != "sha256:def" || got.Status != "indexed" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	updated, err := reopened.Save(State{Repository: "owner/repo", IndexedSHA: "next", Status: "failed", Error: "timeout"})
	if err != nil || !updated.CreatedAt.Equal(saved.CreatedAt) {
		t.Fatalf("Save update = %+v, %v", updated, err)
	}
}
