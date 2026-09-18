package tools

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iamangus/code-mcp/internal/locks"
	"github.com/iamangus/code-mcp/internal/worktree"
)

func newLM() *locks.Manager { return locks.NewManager(slog.Default()) }

var bgCtx = context.Background()

// TestReadFile_Success verifies that ReadFile returns the correct content.
func TestReadFile_Success(t *testing.T) {
	dir := t.TempDir()
	content := "hello world"
	if err := os.WriteFile(filepath.Join(dir, "test.txt"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFile(bgCtx, dir, "test.txt", newLM())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != content {
		t.Errorf("got %q, want %q", got, content)
	}
}

// TestReadFile_NotFound verifies that ReadFile returns a ToolError for missing files.
func TestReadFile_NotFound(t *testing.T) {
	dir := t.TempDir()
	_, err := ReadFile(bgCtx, dir, "missing.txt", newLM())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if _, ok := err.(*worktree.ToolError); !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
}

// TestReadFile_ExceedsSize verifies the 1 MiB limit.
func TestReadFile_ExceedsSize(t *testing.T) {
	dir := t.TempDir()
	large := make([]byte, MaxFileSize+1)
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), large, 0644); err != nil {
		t.Fatal(err)
	}
	_, err := ReadFile(bgCtx, dir, "big.bin", newLM())
	if err == nil {
		t.Fatal("expected size error, got nil")
	}
	te, ok := err.(*worktree.ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if !strings.Contains(te.Message, "exceeds maximum size") {
		t.Errorf("unexpected message: %s", te.Message)
	}
}

// TestReadLines_Success verifies that ReadLines returns the expected range.
func TestReadLines_Success(t *testing.T) {
	dir := t.TempDir()
	lines := "line1\nline2\nline3\nline4\nline5"
	if err := os.WriteFile(filepath.Join(dir, "lines.txt"), []byte(lines), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLines(bgCtx, dir, "lines.txt", 2, 4, newLM())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "line2\nline3\nline4"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestReadLines_StartLineTooLarge verifies ToolError when start_line exceeds file length.
func TestReadLines_StartLineTooLarge(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "short.txt"), []byte("one\ntwo"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := ReadLines(bgCtx, dir, "short.txt", 100, 200, newLM())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if _, ok := err.(*worktree.ToolError); !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
}

// TestCreateFile_Success verifies successful file creation.
func TestCreateFile_Success(t *testing.T) {
	dir := t.TempDir()
	msg, err := CreateFile(bgCtx, dir, "new.txt", "content", newLM())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(msg, "created successfully") {
		t.Errorf("unexpected message: %s", msg)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "new.txt"))
	if string(data) != "content" {
		t.Errorf("file content mismatch: %s", data)
	}
}

// TestCreateFile_AlreadyExists verifies ToolError when file exists.
func TestCreateFile_AlreadyExists(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "exists.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := CreateFile(bgCtx, dir, "exists.txt", "y", newLM())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	te, ok := err.(*worktree.ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if !strings.Contains(te.Message, "already exists") {
		t.Errorf("unexpected message: %s", te.Message)
	}
}

// TestListDirectory_Flat verifies non-recursive listing.
func TestListDirectory_Flat(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte{}, 0644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte{}, 0644)
	os.MkdirAll(filepath.Join(dir, "subdir"), 0755)

	out, err := ListDirectory(bgCtx, dir, ".", false, newLM())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "a.txt") || !strings.Contains(out, "b.txt") {
		t.Errorf("missing files in listing: %s", out)
	}
	if !strings.Contains(out, "subdir/") {
		t.Errorf("missing subdir in listing: %s", out)
	}
}

// TestListDirectory_Recursive verifies recursive listing.
func TestListDirectory_Recursive(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0755)
	os.WriteFile(filepath.Join(dir, "sub", "deep.txt"), []byte{}, 0644)

	out, err := ListDirectory(bgCtx, dir, ".", true, newLM())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "deep.txt") {
		t.Errorf("missing deep file in listing: %s", out)
	}
}

// TestListDirectory_IgnoresNodeModules verifies that node_modules is skipped.
func TestListDirectory_IgnoresNodeModules(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "node_modules"), 0755)
	os.WriteFile(filepath.Join(dir, "node_modules", "pkg.js"), []byte{}, 0644)
	os.WriteFile(filepath.Join(dir, "app.js"), []byte{}, 0644)

	// Flat listing should skip node_modules
	out, err := ListDirectory(bgCtx, dir, ".", false, newLM())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, "node_modules") {
		t.Errorf("node_modules should be ignored in flat listing: %s", out)
	}

	// Recursive listing should skip node_modules
	out, err = ListDirectory(bgCtx, dir, ".", true, newLM())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, "pkg.js") {
		t.Errorf("node_modules contents should be ignored in recursive listing: %s", out)
	}
}

// TestListDirectory_IgnoresGit verifies that .git is skipped.
func TestListDirectory_IgnoresGit(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".git"), 0755)
	os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref"), 0644)

	out, err := ListDirectory(bgCtx, dir, ".", false, newLM())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, ".git") {
		t.Errorf(".git should be ignored: %s", out)
	}
}

// TestGrepSearch_FindsMatches verifies basic grep matching.
func TestGrepSearch_FindsMatches(t *testing.T) {
	dir := t.TempDir()
	content := "foo bar\nbaz\nfoo qux\n"
	os.WriteFile(filepath.Join(dir, "test.txt"), []byte(content), 0644)

	out, err := GrepSearch(bgCtx, dir, "foo", "", newLM())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "foo bar") {
		t.Errorf("expected match for 'foo bar', got: %s", out)
	}
	if !strings.Contains(out, "foo qux") {
		t.Errorf("expected match for 'foo qux', got: %s", out)
	}
}

// TestGrepSearch_RegexSearch verifies regex pattern matching.
func TestGrepSearch_RegexSearch(t *testing.T) {
	dir := t.TempDir()
	// Put info line far away from error lines so it doesn't appear as context.
	var sb strings.Builder
	sb.WriteString("error: something failed\n")
	for i := 0; i < 10; i++ {
		sb.WriteString("padding line\n")
	}
	sb.WriteString("info: all good\n")
	for i := 0; i < 10; i++ {
		sb.WriteString("padding line\n")
	}
	sb.WriteString("error: another failure\n")
	os.WriteFile(filepath.Join(dir, "log.txt"), []byte(sb.String()), 0644)

	out, err := GrepSearch(bgCtx, dir, `^error:`, "", newLM())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "error: something failed") {
		t.Errorf("expected first error line, got: %s", out)
	}
	if !strings.Contains(out, "error: another failure") {
		t.Errorf("expected second error line, got: %s", out)
	}
	// info line is far from any error line so should not appear as context
	if strings.Contains(out, "info: all good") {
		t.Errorf("should not match info line as context, got: %s", out)
	}
}

// TestGrepSearch_IgnoresOpenDevState verifies internal OpenDev state (nested
// worktrees under .opendev) never floods search results.
func TestGrepSearch_IgnoresOpenDevState(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.txt"), []byte("needle here\n"), 0644)
	stateDir := filepath.Join(dir, ".opendev", "worktrees", "job-1")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(stateDir, "copy.txt"), []byte("needle in worktree\n"), 0644)

	out, err := GrepSearch(bgCtx, dir, "needle", "", newLM())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "main.txt") {
		t.Errorf("expected main match, got: %s", out)
	}
	if strings.Contains(out, "job-1") || strings.Contains(out, "worktree") {
		t.Errorf(".opendev state leaked into results: %s", out)
	}
}

// TestGrepSearch_BoundsOutput verifies oversized result sets are truncated
// instead of returning megabytes to the model.
func TestGrepSearch_BoundsOutput(t *testing.T) {
	dir := t.TempDir()
	line := strings.Repeat("match ", 100) + "\n"
	for i := 0; i < 400; i++ {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%03d.txt", i)), []byte(line), 0644)
	}

	out, err := GrepSearch(bgCtx, dir, "match", "", newLM())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) > 40<<10 {
		t.Errorf("output not bounded: %d bytes", len(out))
	}
	if !strings.Contains(out, "output truncated") {
		t.Errorf("missing truncation notice: %d bytes ending %q", len(out), out[len(out)-100:])
	}
}

// TestSearchAndReplace_UniqueMatch verifies replacement with a unique match.
func TestSearchAndReplace_UniqueMatch(t *testing.T) {
	dir := t.TempDir()
	original := "hello world\nfoo bar\ngoodbye"
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte(original), 0644)

	out, err := SearchAndReplace(bgCtx, dir, "file.txt", "foo bar", "replaced", revision(original), newLM())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "Successfully replaced") {
		t.Errorf("expected success message, got: %s", out)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "file.txt"))
	if !strings.Contains(string(data), "replaced") {
		t.Errorf("file not updated: %s", data)
	}
	if strings.Contains(string(data), "foo bar") {
		t.Errorf("old content still present: %s", data)
	}
}

// TestSearchAndReplace_MultipleMatches verifies ToolError when search_block is ambiguous.
func TestSearchAndReplace_MultipleMatches(t *testing.T) {
	dir := t.TempDir()
	original := "foo\nfoo\nbar"
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte(original), 0644)

	_, err := SearchAndReplace(bgCtx, dir, "file.txt", "foo", "baz", revision(original), newLM())
	if err == nil {
		t.Fatal("expected error for multiple matches, got nil")
	}
	te, ok := err.(*worktree.ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if !strings.Contains(te.Message, "matches 2 locations") {
		t.Errorf("unexpected message: %s", te.Message)
	}
}

// TestSearchAndReplace_FuzzyMatch verifies fuzzy replacement with whitespace differences.
func TestSearchAndReplace_FuzzyMatch(t *testing.T) {
	dir := t.TempDir()
	// File has extra spaces
	original := "func  hello()  {\n    return  nil\n}"
	os.WriteFile(filepath.Join(dir, "file.go"), []byte(original), 0644)

	// Search with normalized spaces
	searchBlock := "func hello() {\n    return nil\n}"
	replaceBlock := "func hello() {\n    return \"world\"\n}"

	out, err := SearchAndReplace(bgCtx, dir, "file.go", searchBlock, replaceBlock, revision(original), newLM())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "Successfully replaced") {
		t.Errorf("expected success message, got: %s", out)
	}
}

// TestSearchAndReplace_NoMatchBelowThreshold verifies ToolError when no match found.
func TestSearchAndReplace_NoMatchBelowThreshold(t *testing.T) {
	dir := t.TempDir()
	original := "completely different content here"
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte(original), 0644)

	_, err := SearchAndReplace(bgCtx, dir, "file.txt", "totally unrelated search block that won't match", "replacement", revision(original), newLM())
	if err == nil {
		t.Fatal("expected error for no match, got nil")
	}
	te, ok := err.(*worktree.ToolError)
	if !ok {
		t.Fatalf("expected ToolError, got %T", err)
	}
	if !strings.Contains(te.Message, "not found") {
		t.Errorf("unexpected message: %s", te.Message)
	}
}

func TestSearchAndReplace_RejectsStaleRevision(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("old"), 0644)
	_, err := SearchAndReplace(bgCtx, dir, "file.txt", "old", "new", "sha256:stale", newLM())
	if err == nil || !strings.Contains(err.Error(), "stale revision") {
		t.Fatalf("expected stale revision error, got %v", err)
	}
}

func TestRevisionAuthorizer_RequiresFreshReadAfterSuccessfulEdit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	original := "first"
	os.WriteFile(path, []byte(original), 0644)

	authorizer := NewRevisionAuthorizer()
	firstRevision := RevisionToken(original)
	authorizer.Authorize(path, firstRevision)
	if err := authorizer.Verify(path, firstRevision); err != nil {
		t.Fatalf("Verify initial read: %v", err)
	}
	if _, err := SearchAndReplace(bgCtx, dir, "file.txt", "first", "second", firstRevision, newLM()); err != nil {
		t.Fatalf("SearchAndReplace: %v", err)
	}
	authorizer.Consume(path, firstRevision)

	secondRevision := RevisionToken("second")
	if err := authorizer.Verify(path, secondRevision); err == nil {
		t.Fatal("expected edit to require a fresh read")
	}
	authorizer.Authorize(path, secondRevision)
	if err := authorizer.Verify(path, secondRevision); err != nil {
		t.Fatalf("Verify fresh read: %v", err)
	}
}
