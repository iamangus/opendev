package tools

import (
	"bufio"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/iamangus/code-mcp/internal/locks"
	"github.com/iamangus/code-mcp/internal/worktree"
)

const MaxFileSize = 1 << 20 // 1 MiB

// fuzzyMatchThreshold is the minimum LCS-based similarity ratio required to
// accept a fuzzy match in search_and_replace. A value of 0.85 requires the
// candidate window to share at least 85% of characters with the search_block
// after whitespace normalisation. Below this threshold the match is too
// ambiguous to apply safely, and the closest snippet is returned to the agent.
const fuzzyMatchThreshold = 0.85

func revision(content string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("sha256:%x", sum)
}

// WithRevision formats content with the token required by mutation tools. The
// token prevents edits against a stale view after an earlier replacement.
func WithRevision(content string) string {
	return fmt.Sprintf("Revision: %s\n\n%s", revision(content), content)
}

var ignoreDirs = map[string]bool{
	".git": true, "node_modules": true, "build": true, "dist": true,
	".next": true, "__pycache__": true, ".cache": true, "coverage": true,
	"out": true, "target": true, "vendor": true, ".venv": true, "venv": true,
}

// ReadFile reads the entire content of a file within the worktree.
func ReadFile(ctx context.Context, worktreeRoot, filePath string, lm *locks.Manager) (string, error) {
	abs, err := worktree.Resolve(worktreeRoot, filePath)
	if err != nil {
		return "", err
	}
	if err := lm.RLock(ctx, abs); err != nil {
		return "", err
	}
	defer lm.RUnlock(abs)

	info, err := os.Stat(abs)
	if err != nil {
		return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: file not found: %s", filePath)}
	}
	if info.Size() > MaxFileSize {
		return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: file %s exceeds maximum size of 1 MiB", filePath)}
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: cannot read file: %v", err)}
	}
	return string(content), nil
}

// ReadLines reads a range of lines (1-indexed, inclusive) from a file.
func ReadLines(ctx context.Context, worktreeRoot, filePath string, startLine, endLine int, lm *locks.Manager) (string, error) {
	abs, err := worktree.Resolve(worktreeRoot, filePath)
	if err != nil {
		return "", err
	}
	if err := lm.RLock(ctx, abs); err != nil {
		return "", err
	}
	defer lm.RUnlock(abs)

	f, err := os.Open(abs)
	if err != nil {
		return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: file not found: %s", filePath)}
	}
	defer f.Close()

	if startLine < 1 {
		return "", &worktree.ToolError{Message: "Tool Error: start_line must be >= 1"}
	}
	if endLine < startLine {
		return "", &worktree.ToolError{Message: "Tool Error: end_line must be >= start_line"}
	}

	var lines []string
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if lineNum >= startLine && lineNum <= endLine {
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
	abs, err := worktree.Resolve(worktreeRoot, filePath)
	if err != nil {
		return "", err
	}
	if err := lm.Lock(ctx, abs); err != nil {
		return "", err
	}
	defer lm.Unlock(abs)

	if _, err := os.Stat(abs); err == nil {
		return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: file already exists: %s", filePath)}
	}

	if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
		return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: cannot create parent directories: %v", err)}
	}

	if err := os.WriteFile(abs, []byte(content), 0644); err != nil {
		return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: cannot write file: %v", err)}
	}
	return fmt.Sprintf("File created successfully: %s", abs), nil
}

// ListDirectory lists the contents of a directory, optionally recursively.
func ListDirectory(ctx context.Context, worktreeRoot, dirPath string, recursive bool, lm *locks.Manager) (string, error) {
	abs, err := worktree.Resolve(worktreeRoot, dirPath)
	if err != nil {
		return "", err
	}

	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: %q is not a directory", dirPath)}
	}

	var sb strings.Builder

	if recursive {
		walkErr := filepath.Walk(abs, func(path string, fi os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if fi.IsDir() && ignoreDirs[fi.Name()] && path != abs {
				return filepath.SkipDir
			}
			rel, _ := filepath.Rel(abs, path)
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
		entries, err2 := os.ReadDir(abs)
		if err2 != nil {
			return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: cannot read directory: %v", err2)}
		}
		for _, entry := range entries {
			if ignoreDirs[entry.Name()] {
				continue
			}
			if entry.IsDir() {
				sb.WriteString(fmt.Sprintf("  %s/\n", entry.Name()))
			} else {
				sb.WriteString(fmt.Sprintf("  %s\n", entry.Name()))
			}
		}
	}

	return sb.String(), nil
}

// GrepSearch searches for a pattern (regex or literal) within files in a directory.
func GrepSearch(ctx context.Context, worktreeRoot, query, directory string, lm *locks.Manager) (string, error) {
	searchPath := worktreeRoot
	if directory != "" {
		searchPath = directory
	}
	abs, err := worktree.Resolve(worktreeRoot, searchPath)
	if err != nil {
		return "", err
	}

	re, err := regexp.Compile(query)
	if err != nil {
		re = regexp.MustCompile(regexp.QuoteMeta(query))
	}

	var results strings.Builder

	walkErr := filepath.Walk(abs, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if fi.IsDir() {
			if ignoreDirs[fi.Name()] && path != abs {
				return filepath.SkipDir
			}
			return nil
		}

		// Read the file under a short-lived lock so we don't hold it for the
		// entire walk duration.
		data, readErr := func() ([]byte, error) {
			if err := lm.RLock(ctx, path); err != nil {
				return nil, err
			}
			defer lm.RUnlock(path)
			return os.ReadFile(path)
		}()
		if readErr != nil {
			return nil
		}
		if !utf8.Valid(data) {
			return nil
		}

		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if re.MatchString(line) {
				rel, _ := filepath.Rel(worktreeRoot, path)
				// Include context: 2 lines before and after
				start := i - 2
				if start < 0 {
					start = 0
				}
				end := i + 2
				if end >= len(lines) {
					end = len(lines) - 1
				}
				for ctx := start; ctx <= end; ctx++ {
					results.WriteString(fmt.Sprintf("%s:%d: %s\n", rel, ctx+1, lines[ctx]))
				}
				results.WriteString("---\n")
			}
		}
		return nil
	})
	if walkErr != nil {
		return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: error walking directory: %v", walkErr)}
	}

	return results.String(), nil
}

// SearchAndReplace finds a block of text in a file and replaces it.
// It first tries an exact match, then falls back to fuzzy matching.
func SearchAndReplace(ctx context.Context, worktreeRoot, filePath, searchBlock, replaceBlock, expectedRevision string, lm *locks.Manager) (string, error) {
	abs, err := worktree.Resolve(worktreeRoot, filePath)
	if err != nil {
		return "", err
	}
	if err := lm.Lock(ctx, abs); err != nil {
		return "", err
	}
	defer lm.Unlock(abs)

	data, err := os.ReadFile(abs)
	if err != nil {
		return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: cannot read file: %v", err)}
	}
	content := string(data)
	if expectedRevision != revision(content) {
		return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: stale revision for %s. Read the file again before editing. Current revision: %s", filePath, revision(content))}
	}

	// Exact match
	count := strings.Count(content, searchBlock)
	if count > 1 {
		return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: search_block matches %d locations in %s. Provide more surrounding context to disambiguate.", count, filePath)}
	}
	if count == 1 {
		newContent := strings.Replace(content, searchBlock, replaceBlock, 1)
		if err := os.WriteFile(abs, []byte(newContent), 0644); err != nil {
			return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: cannot write file: %v", err)}
		}
		idx := strings.Index(newContent, replaceBlock)
		return fmt.Sprintf("New revision: %s\n%s", revision(newContent), buildContext(newContent, idx, replaceBlock, abs)), nil
	}

	// Fuzzy match: normalize whitespace and compare line-by-line windows
	searchLines := strings.Split(searchBlock, "\n")
	fileLines := strings.Split(content, "\n")

	normalizeLines := func(lines []string) string {
		parts := make([]string, len(lines))
		for i, l := range lines {
			parts[i] = strings.Join(strings.Fields(strings.TrimSpace(l)), " ")
		}
		return strings.Join(parts, "\n")
	}

	normalizedSearch := normalizeLines(searchLines)
	windowSize := len(searchLines)

	bestRatio := 0.0
	bestStart := -1
	bestEnd := -1

	for i := 0; i <= len(fileLines)-windowSize; i++ {
		window := fileLines[i : i+windowSize]
		normalizedWindow := normalizeLines(window)
		ratio := similarity(normalizedSearch, normalizedWindow)
		if ratio > bestRatio {
			bestRatio = ratio
			bestStart = i
			bestEnd = i + windowSize
		}
	}

	if bestRatio >= fuzzyMatchThreshold {
		var newLines []string
		newLines = append(newLines, fileLines[:bestStart]...)
		newLines = append(newLines, strings.Split(replaceBlock, "\n")...)
		newLines = append(newLines, fileLines[bestEnd:]...)
		newContent := strings.Join(newLines, "\n")
		if err := os.WriteFile(abs, []byte(newContent), 0644); err != nil {
			return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: cannot write file: %v", err)}
		}
		idx := strings.Index(newContent, replaceBlock)
		return fmt.Sprintf("New revision: %s\n%s", revision(newContent), buildContext(newContent, idx, replaceBlock, abs)), nil
	}

	snippet := ""
	if bestStart >= 0 {
		snippet = strings.Join(fileLines[bestStart:bestEnd], "\n")
	}
	return "", &worktree.ToolError{Message: fmt.Sprintf("Tool Error: search_block not found in %s (best match similarity: %.2f). Closest match:\n%s", filePath, bestRatio, snippet)}
}

// buildContext returns up to ±5 lines of context around the replaced area.
func buildContext(content string, idx int, replaceBlock, filePath string) string {
	if idx < 0 {
		return fmt.Sprintf("Successfully replaced in %s", filePath)
	}
	lines := strings.Split(content, "\n")
	before := content[:idx]
	startLine := strings.Count(before, "\n")
	replaceLines := strings.Count(replaceBlock, "\n") + 1
	endLine := startLine + replaceLines

	ctxStart := startLine - 5
	if ctxStart < 0 {
		ctxStart = 0
	}
	ctxEnd := endLine + 5
	if ctxEnd > len(lines) {
		ctxEnd = len(lines)
	}

	return fmt.Sprintf("Successfully replaced in %s\n%s", filePath, strings.Join(lines[ctxStart:ctxEnd], "\n"))
}

// similarity computes 2*LCS_chars / (len(a)+len(b)).
func similarity(a, b string) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1.0
	}
	lcs := lcsLength(a, b)
	return float64(2*lcs) / float64(len(a)+len(b))
}

// lcsLength computes the length of the longest common subsequence of two strings.
func lcsLength(a, b string) int {
	ra := []rune(a)
	rb := []rune(b)
	m, n := len(ra), len(rb)
	if m == 0 || n == 0 {
		return 0
	}
	prev := make([]int, n+1)
	curr := make([]int, n+1)
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if ra[i-1] == rb[j-1] {
				curr[j] = prev[j-1] + 1
			} else if curr[j-1] > prev[j] {
				curr[j] = curr[j-1]
			} else {
				curr[j] = prev[j]
			}
		}
		prev, curr = curr, prev
		for k := range curr {
			curr[k] = 0
		}
	}
	return prev[n]
}
