package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestToolsRejectSymlinkEscapesAndMetadata(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"link/secret", ".git/config", ".opendev/worktrees/task/file", ".opendev/references/job/file"} {
		if _, err := ReadFile(context.Background(), root, path, newLM()); err == nil {
			t.Fatalf("read allowed %s", path)
		}
		if _, err := CreateFile(context.Background(), root, path, "bad", newLM()); err == nil {
			t.Fatalf("write allowed %s", path)
		}
	}
	data, _ := os.ReadFile(secret)
	if string(data) != "private" {
		t.Fatal("external file changed")
	}
}

func TestCreateFileIsExclusiveAcrossIndependentManagers(t *testing.T) {
	root := t.TempDir()
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := CreateFile(context.Background(), root, "same", "complete", newLM()); err == nil {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("created %d times", successes.Load())
	}
}

func TestReplacementRejectsAmbiguousWhitespaceAndPreservesMode(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "script")
	original := "echo  hello\necho   hello\n"
	os.WriteFile(path, []byte(original), 0750)
	if _, err := SearchAndReplace(context.Background(), root, "script", "echo hello", "bad", revision(original), newLM()); err == nil {
		t.Fatal("ambiguous replacement accepted")
	}
	if _, err := SearchAndReplace(context.Background(), root, "script", "echo  hello", "echo fixed", revision(original), newLM()); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0750 {
		t.Fatalf("permissions changed: %v", info.Mode())
	}
}

func TestGrepInvalidRegexFallsBackToLiteral(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "text"), []byte("literal [ here\n"), 0644)
	out, err := GrepSearch(context.Background(), root, "[", "", newLM())
	if err != nil || !strings.Contains(out, "literal [ here") {
		t.Fatalf("literal fallback: %q %v", out, err)
	}
}
