package tools

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/iamangus/code-mcp/internal/locks"
	"github.com/iamangus/code-mcp/internal/worktree"
)

const MaxFileSize = 1 << 20 // 1 MiB

const maxToolOutput = 32 << 10

var temporarySequence atomic.Uint64

func readBounded(root, path string) ([]byte, error) {
	r, rel, err := worktree.OpenRoot(root, path)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	f, err := r.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
	if len(data) > MaxFileSize {
		return nil, fmt.Errorf("file exceeds maximum size of 1 MiB")
	}
	return data, err
}

func atomicReplace(root, path string, content []byte) error {
	if len(content) > MaxFileSize {
		return fmt.Errorf("file exceeds maximum size of 1 MiB")
	}
	r, rel, err := worktree.OpenRoot(root, path)
	if err != nil {
		return err
	}
	defer r.Close()
	fi, err := r.Stat(rel)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("not a regular file")
	}
	tmp := filepath.Join(filepath.Dir(rel), fmt.Sprintf(".opendev-edit-%d-%d", os.Getpid(), temporarySequence.Add(1)))
	f, err := r.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer r.Remove(tmp)
	if _, err = f.Write(content); err == nil {
		err = f.Chmod(fi.Mode().Perm())
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return r.Rename(tmp, rel)
}

func revision(content string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("sha256:%x", sum)
}

// WithRevision formats content with the token required by mutation tools. The
// token prevents edits against a stale view after an earlier replacement.
func WithRevision(content string) string {
	return fmt.Sprintf("Revision: %s\n\n%s", RevisionToken(content), content)
}

func RevisionToken(content string) string {
	return revision(content)
}

// RevisionAuthorizer permits exactly one edit using a token produced by
// read_file. A replacement result deliberately does not authorize another edit.
type RevisionAuthorizer struct {
	mu       sync.Mutex
	approved map[string]string
}

func NewRevisionAuthorizer() *RevisionAuthorizer {
	return &RevisionAuthorizer{approved: make(map[string]string)}
}

func (a *RevisionAuthorizer) Authorize(absPath, token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.approved[absPath] = token
}

func (a *RevisionAuthorizer) Verify(absPath, token string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.approved[absPath] == token {
		return nil
	}
	if _, pending := a.approved[absPath]; pending {
		return &worktree.ToolError{Message: "Tool Error: the supplied expected_revision does not match the current read_file revision for this file. Call read_file on it again and pass the revision token from that exact result."}
	}
	return &worktree.ToolError{Message: "Tool Error: read_file must be called after the previous successful edit before editing this file again. Call read_file on this file, then retry with the revision token from that result as expected_revision."}
}

// VerifyOrAdopt authorizes an edit when the token was issued by read_file, or
// when it matches the file's current content hash. The content match tolerates
// a model passing an equivalent token from its context while still rejecting
// every token that predates the current file state.
func (a *RevisionAuthorizer) VerifyOrAdopt(absPath, token, currentRevision string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.approved[absPath] == token {
		return nil
	}
	if currentRevision != "" && token == currentRevision {
		a.approved[absPath] = token
		return nil
	}
	if _, pending := a.approved[absPath]; pending {
		return &worktree.ToolError{Message: "Tool Error: the supplied expected_revision matches neither the current file revision nor an authorized read. Call read_file on this file and pass the revision token from that exact result."}
	}
	return &worktree.ToolError{Message: "Tool Error: read_file must be called after the previous successful edit before editing this file again. Call read_file on this file, then retry with the revision token from that result as expected_revision."}
}

func (a *RevisionAuthorizer) Consume(absPath, token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.approved[absPath] == token {
		delete(a.approved, absPath)
	}
}

var ignoreDirs = map[string]bool{
	".git": true, ".opendev": true, "node_modules": true, "build": true, "dist": true,
	".next": true, "__pycache__": true, ".cache": true, "coverage": true,
	"out": true, "target": true, "vendor": true, ".venv": true, "venv": true,
}

// ReadFile reads the entire content of a file within the worktree.
func ReadFile(ctx context.Context, worktreeRoot, filePath string, lm *locks.Manager) (string, error) {
	release, err := worktree.Coordinate(ctx, worktreeRoot, false)
	if err != nil {
		return "", err
	}
	defer release()
	abs, err := worktree.Resolve(worktreeRoot, filePath)
	if err != nil {
		return "", err
	}
	if err := lm.RLock(ctx, abs); err != nil {
		return "", err
	}
	defer lm.RUnlock(abs)

	content, err := readBounded(worktreeRoot, filePath)
	if err != nil {
		return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: cannot read file: %v", err)}
	}
	return string(content), nil
}

// ReadLines reads a range of lines (1-indexed, inclusive) from a file.
func ReadLines(ctx context.Context, worktreeRoot, filePath string, startLine, endLine int, lm *locks.Manager) (string, error) {
	release, err := worktree.Coordinate(ctx, worktreeRoot, false)
	if err != nil {
		return "", err
	}
	defer release()
	abs, err := worktree.Resolve(worktreeRoot, filePath)
	if err != nil {
		return "", err
	}
	if err := lm.RLock(ctx, abs); err != nil {
		return "", err
	}
	defer lm.RUnlock(abs)

	r, rel, err := worktree.OpenRoot(worktreeRoot, filePath)
	if err != nil {
		return "", err
	}
	defer r.Close()
	f, err := r.Open(rel)
	if err != nil {
		return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: file not found: %s", filePath)}
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file")
	}

	if startLine < 1 {
		return "", &worktree.ToolError{Message: "Tool Error: start_line must be >= 1"}
	}
	if endLine < startLine {
		return "", &worktree.ToolError{Message: "Tool Error: end_line must be >= start_line"}
	}

	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), MaxFileSize)
	lineNum := 0
	outputSize := 0
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		lineNum++
		if lineNum >= startLine && lineNum <= endLine {
			outputSize += len(scanner.Text()) + 1
			if outputSize > maxToolOutput {
				return strings.Join(lines, "\n") + "\n[output truncated; request a smaller line range]", nil
			}
			lines = append(lines, scanner.Text())
		}
		if lineNum > endLine {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: error reading file: %v", err)}
	}
	if lineNum < startLine {
		return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: start_line %d exceeds file length %d", startLine, lineNum)}
	}
	return strings.Join(lines, "\n"), nil
}

// CreateFile creates a new file at the given path with the provided content.
func CreateFile(ctx context.Context, worktreeRoot, filePath, content string, lm *locks.Manager) (string, error) {
	release, err := worktree.Coordinate(ctx, worktreeRoot, false)
	if err != nil {
		return "", err
	}
	defer release()
	abs, err := worktree.Resolve(worktreeRoot, filePath)
	if err != nil {
		return "", err
	}
	if err := lm.Lock(ctx, abs); err != nil {
		return "", err
	}
	defer lm.Unlock(abs)

	if len(content) > MaxFileSize {
		return "", fmt.Errorf("file exceeds maximum size of 1 MiB")
	}
	r, rel, err := worktree.OpenRoot(worktreeRoot, filePath)
	if err != nil {
		return "", err
	}
	defer r.Close()
	if err := r.MkdirAll(filepath.Dir(rel), 0755); err != nil {
		return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: cannot create parent directories: %v", err)}
	}

	f, err := r.OpenFile(rel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if os.IsExist(err) {
		return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: file already exists: %s", filePath)}
	}
	if err != nil {
		return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: cannot create file: %v", err)}
	}
	_, err = f.WriteString(content)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: cannot write file: %v", err)}
	}
	return fmt.Sprintf("File created successfully: %s", abs), nil
}

// ListDirectory lists the contents of a directory, optionally recursively.
func ListDirectory(ctx context.Context, worktreeRoot, dirPath string, recursive bool, lm *locks.Manager) (string, error) {
	release, err := worktree.Coordinate(ctx, worktreeRoot, false)
	if err != nil {
		return "", err
	}
	defer release()
	abs, err := worktree.Resolve(worktreeRoot, dirPath)
	if err != nil {
		return "", err
	}

	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: %q is not a directory", dirPath)}
	}

	var sb strings.Builder
	r, relDir, err := worktree.OpenRoot(worktreeRoot, dirPath)
	if err != nil {
		return "", err
	}
	defer r.Close()

	if recursive {
		walkErr := fs.WalkDir(r.FS(), relDir, func(path string, fi fs.DirEntry, err error) error {
			if cancelErr := ctx.Err(); cancelErr != nil {
				return cancelErr
			}
			if sb.Len() >= maxToolOutput {
				sb.WriteString("[output truncated]\n")
				return filepath.SkipAll
			}
			if err != nil {
				return nil
			}
			if fi.IsDir() && ignoreDirs[fi.Name()] && path != relDir {
				return filepath.SkipDir
			}
			rel, _ := filepath.Rel(relDir, path)
			if rel == "." {
				return nil
			}
			depth := strings.Count(rel, string(filepath.Separator))
			indent := strings.Repeat("  ", depth)
			if fi.IsDir() {
				sb.WriteString(fmt.Sprintf("%s%s/\n", indent, fi.Name()))
			} else {
				sb.WriteString(fmt.Sprintf("%s%s\n", indent, fi.Name()))
			}
			return nil
		})
		if walkErr != nil {
			return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: error walking directory: %v", walkErr)}
		}
	} else {
		f, err2 := r.Open(relDir)
		if err2 != nil {
			return "", err2
		}
		defer f.Close()
		entries, err2 := f.ReadDir(1024)
		if err2 == io.EOF {
			err2 = nil
		}
		if err2 != nil {
			return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: cannot read directory: %v", err2)}
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if sb.Len() >= maxToolOutput {
				sb.WriteString("[output truncated]\n")
				break
			}
			if ignoreDirs[entry.Name()] {
				continue
			}
			if entry.IsDir() {
				sb.WriteString(fmt.Sprintf("  %s/\n", entry.Name()))
			} else {
				sb.WriteString(fmt.Sprintf("  %s\n", entry.Name()))
			}
		}
		if len(entries) == 1024 {
			sb.WriteString("[output truncated: directory entry limit]\n")
		}
	}

	return sb.String(), nil
}

// GrepSearch searches for a pattern (regex or literal) within files in a
// directory. It prefers ripgrep, which respects ignore files and skips
// binaries, and always bounds the returned output: a multi-megabyte result is
// useless to a model and has starved the MCP client pipeline before.
const maxGrepOutput = 32 << 10 // 32 KiB

func GrepSearch(ctx context.Context, worktreeRoot, query, directory string, lm *locks.Manager) (string, error) {
	release, err := worktree.Coordinate(ctx, worktreeRoot, false)
	if err != nil {
		return "", err
	}
	defer release()
	searchPath := "."
	if directory != "" {
		searchPath = directory
	}
	abs, err := worktree.Resolve(worktreeRoot, searchPath)
	if err != nil {
		return "", err
	}

	if _, lookErr := exec.LookPath("rg"); lookErr == nil {
		if out, truncated, runErr := grepWithRipgrep(ctx, worktreeRoot, abs, query); runErr == nil {
			return boundGrepOutput(out, truncated), nil
		}
		// Fall through to the in-process walker when ripgrep is unusable.
	}

	re, err := regexp.Compile(query)
	if err != nil {
		re = regexp.MustCompile(regexp.QuoteMeta(query))
	}

	var results strings.Builder
	truncated := false

	walkErr := filepath.Walk(abs, func(path string, fi os.FileInfo, err error) error {
		if cancelErr := ctx.Err(); cancelErr != nil {
			return cancelErr
		}
		if err != nil {
			return nil
		}
		if fi.IsDir() {
			if ignoreDirs[fi.Name()] && path != abs {
				return filepath.SkipDir
			}
			return nil
		}
		if truncated {
			return filepath.SkipAll
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		// Skip files that cannot reasonably contain a useful text match.
		if fi.Size() > 1<<20 {
			return nil
		}

		// Read the file under a short-lived lock so we don't hold it for the
		// entire walk duration.
		data, readErr := func() ([]byte, error) {
			if err := lm.RLock(ctx, path); err != nil {
				return nil, err
			}
			defer lm.RUnlock(path)
			return readBounded(worktreeRoot, path)
		}()
		if readErr != nil {
			return nil
		}
		if !utf8.Valid(data) {
			return nil
		}

		rel, _ := filepath.Rel(worktreeRoot, path)
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if !re.MatchString(line) {
				continue
			}
			start := i - 2
			if start < 0 {
				start = 0
			}
			end := i + 2
			if end >= len(lines) {
				end = len(lines) - 1
			}
			for ctx := start; ctx <= end; ctx++ {
				if results.Len() > maxGrepOutput {
					truncated = true
					return filepath.SkipAll
				}
				results.WriteString(fmt.Sprintf("%s:%d: %s\n", rel, ctx+1, lines[ctx]))
			}
			if truncated {
				return filepath.SkipAll
			}
			results.WriteString("---\n")
		}
		return nil
	})
	if walkErr != nil {
		return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: error walking directory: %v", walkErr)}
	}

	return boundGrepOutput(results.String(), truncated), nil
}

// grepWithRipgrep runs ripgrep over the resolved path and returns up to
// maxGrepOutput+1 bytes so callers can detect truncation without buffering an
// unbounded result.
func grepWithRipgrep(ctx context.Context, root, abs, query string) (string, bool, error) {
	r, rel, err := worktree.OpenRoot(root, abs)
	if err != nil {
		return "", false, err
	}
	defer r.Close()
	dir, err := r.Open(rel)
	if err != nil {
		return "", false, err
	}
	defer dir.Close()
	cmd := exec.CommandContext(ctx, "rg", "--no-heading", "-n", "-C", "2", "--glob", "!.opendev/**", "--glob", "!.git/**", "-e", query, ".")
	cmd.ExtraFiles = []*os.File{dir}
	cmd.Dir = "/proc/self/fd/3"
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", false, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", false, err
	}
	data := make([]byte, maxGrepOutput+1)
	n, readErr := io.ReadFull(stdout, data)
	if readErr != nil && readErr != io.ErrUnexpectedEOF && readErr != io.EOF {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return "", false, readErr
	}
	truncated := n == len(data)
	if truncated {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if !truncated && waitErr != nil {
		if exit, ok := waitErr.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
			return "", false, fmt.Errorf("ripgrep failed: %w", waitErr)
		}
	}
	return string(data[:n]), truncated, nil
}

// boundGrepOutput appends a truncation notice when the output was cut short.
func boundGrepOutput(out string, truncated bool) string {
	if !truncated {
		return out
	}
	return out + "\n[output truncated: too many matches. Narrow the query or search a subdirectory; use list_directory to find candidates.]\n"
}

// SearchAndReplace finds a block of text in a file and replaces it.
// It first tries an exact match, then falls back to fuzzy matching.
func SearchAndReplace(ctx context.Context, worktreeRoot, filePath, searchBlock, replaceBlock, expectedRevision string, lm *locks.Manager) (string, error) {
	release, err := worktree.Coordinate(ctx, worktreeRoot, false)
	if err != nil {
		return "", err
	}
	defer release()
	if searchBlock == "" || len(searchBlock) > MaxFileSize || len(replaceBlock) > MaxFileSize {
		return "", &worktree.ToolError{Message: "Tool Error: search_block must be nonempty and edit blocks must not exceed 1 MiB"}
	}
	abs, err := worktree.Resolve(worktreeRoot, filePath)
	if err != nil {
		return "", err
	}
	if err := lm.Lock(ctx, abs); err != nil {
		return "", err
	}
	defer lm.Unlock(abs)

	data, err := readBounded(worktreeRoot, filePath)
	if err != nil {
		return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: cannot read file: %v", err)}
	}
	content := string(data)
	if expectedRevision != revision(content) {
		return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: stale revision for %s. Call read_file on it again; pass the revision token from that result as expected_revision.", filePath)}
	}

	// Exact match
	count := strings.Count(content, searchBlock)
	if count > 1 {
		return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: search_block matches %d locations in %s. Provide more surrounding context to disambiguate.", count, filePath)}
	}
	if count == 1 {
		newContent := strings.Replace(content, searchBlock, replaceBlock, 1)
		if err := atomicReplace(worktreeRoot, filePath, []byte(newContent)); err != nil {
			return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: cannot write file: %v", err)}
		}
		return fmt.Sprintf("Successfully replaced in %s. This result does not authorize another edit: call read_file on %s again before the next search_and_replace; its result carries the expected_revision to use.", filePath, filePath), nil
	}

	// Normalize whitespace only: approximate character matches can silently
	// edit the wrong code. KMP over lines keeps the fallback linear in file size.
	searchLines := strings.Split(searchBlock, "\n")
	fileLines := strings.Split(content, "\n")
	for i := range searchLines {
		searchLines[i] = strings.Join(strings.Fields(searchLines[i]), " ")
	}
	prefix := make([]int, len(searchLines))
	for i, j := 1, 0; i < len(searchLines); i++ {
		for j > 0 && searchLines[i] != searchLines[j] {
			j = prefix[j-1]
		}
		if searchLines[i] == searchLines[j] {
			j++
		}
		prefix[i] = j
	}
	bestStart := -1
	for i, j := 0, 0; i < len(fileLines); i++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		line := strings.Join(strings.Fields(fileLines[i]), " ")
		for j > 0 && line != searchLines[j] {
			j = prefix[j-1]
		}
		if line == searchLines[j] {
			j++
		}
		if j == len(searchLines) {
			if bestStart >= 0 {
				return "", &worktree.ToolError{Message: "Tool Error: ambiguous whitespace-normalized search_block; provide more context"}
			}
			bestStart = i - j + 1
			j = prefix[j-1]
		}
	}
	if bestStart >= 0 {
		bestEnd := bestStart + len(searchLines)
		var newLines []string
		newLines = append(newLines, fileLines[:bestStart]...)
		newLines = append(newLines, strings.Split(replaceBlock, "\n")...)
		newLines = append(newLines, fileLines[bestEnd:]...)
		newContent := strings.Join(newLines, "\n")
		if err := atomicReplace(worktreeRoot, filePath, []byte(newContent)); err != nil {
			return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: cannot write file: %v", err)}
		}
		return fmt.Sprintf("Successfully replaced in %s. This result does not authorize another edit: call read_file on %s again before the next search_and_replace; its result carries the expected_revision to use.", filePath, filePath), nil
	}

	return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: search_block not found in %s; provide exact text or whitespace-only differences", filePath)}
}
