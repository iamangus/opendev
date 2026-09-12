package validation

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRunDeclaredValidation(t *testing.T) {
	worktree := t.TempDir()
	if err := os.Mkdir(filepath.Join(worktree, ".opendev"), 0755); err != nil {
		t.Fatal(err)
	}
	policy := "validations:\n  check:\n    command: printf ok\n    timeout_seconds: 5\n"
	if err := os.WriteFile(filepath.Join(worktree, policyPath), []byte(policy), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := Run(context.Background(), worktree, "check")
	if err != nil {
		t.Fatal(err)
	}
	if got.ExitCode != 0 || got.Output != "ok" {
		t.Fatalf("result = %#v", got)
	}
}

func TestRunRejectsUndeclaredValidation(t *testing.T) {
	worktree := t.TempDir()
	if err := os.Mkdir(filepath.Join(worktree, ".opendev"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, policyPath), []byte("validations: {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), worktree, "unknown"); err == nil {
		t.Fatal("Run succeeded for undeclared validation")
	}
}
