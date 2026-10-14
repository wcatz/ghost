// Package claudeimport reads Claude Code's auto-memory files and imports
// them into Ghost's memory store. This is a read-only operation — Claude's
// files are never modified or deleted.
package claudeimport

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/provider"
)

const (
	minContentLen = 20
	source        = "onboarding"

	// maxMemoryFileBytes bounds one auto-memory file. A memory this import
	// keeps is capped at memory.MaxContentLen — 8 KB — so a file orders of
	// magnitude past that is not a memory, and reading it would hand a file
	// ghost did not choose the whole of the process's memory budget on a path
	// that runs unattended during `ghost mcp init`. A file past the ceiling is
	// skipped, like a stub: nothing that would have been kept is lost.
	maxMemoryFileBytes = 1 << 20

	// maxFrontmatterLine bounds one front-matter line, for the same reason
	// bufio.Scanner has a default. The default is 64 KiB, which is short enough
	// that an ordinary long description crossed it — and a crossed scanner
	// stops, so the whole block was being discarded rather than the one long
	// line. 256 KiB is far past any description a person writes and well inside
	// maxMemoryFileBytes, so a line past it means the file is not a memory file
	// rather than that the file is large.
	maxFrontmatterLine = 256 << 10
)

var importTags = []string{"claude-code", "auto-import"}

// categoryMap converts Claude Code memory types to Ghost categories.
var categoryMap = map[string]string{
	"feedback":  "preference",
	"project":   "architecture",
	"reference": "fact",
	"pattern":   "pattern",
	"decision":  "decision",
	"gotcha":    "gotcha",
	"user":      "preference",
}

// importanceMap returns importance scores by Claude type.
var importanceMap = map[string]float32{
	"feedback": 0.9,
	"project":  0.8,
	"user":     0.9,
}

const defaultImportance float32 = 0.7

// maxEncodedPathLen mirrors Claude Code's MAX_ENCODED_LENGTH. A name at or
// under it is the plain fold; past it Claude Code truncates to this many
// UTF-16 code units and appends a hash of the full path so long workspaces
// stay distinguishable.
const maxEncodedPathLen = 200

// EncodeProjectPath converts an absolute path to the directory name Claude
// Code stores it under, e.g. "/home/wayne/git/ghost" →
// "-home-wayne-git-ghost". Claude Code replaces every character outside
// [a-zA-Z0-9] with '-' — not just path separators — so a path carrying a
// '.', '_', space or non-ASCII rune gets one '-' where this package used to
// keep the character, and Claude Code then never read the redirect or memory
// file Ghost wrote. The fold, the truncation and the hash are copied from
// Claude Code's own encoder (a Java-style String.hashCode rendered base36),
// so the name matches byte for byte.
//
// The fold walks UTF-16 code units rather than runes, because Claude Code is
// a JavaScript program and its [^a-zA-Z0-9] regex matches each surrogate half
// of a non-BMP character separately: one emoji becomes two '-'. The hash is
// over the same units.
func EncodeProjectPath(projectPath string) string {
	units := utf16.Encode([]rune(projectPath))
	keep := len(units)
	if keep > maxEncodedPathLen {
		keep = maxEncodedPathLen
	}

	var b strings.Builder
	b.Grow(keep)
	for _, u := range units[:keep] {
		if isEncodedAlnum(u) {
			b.WriteByte(byte(u))
		} else {
			b.WriteByte('-')
		}
	}
	encoded := b.String()

	if len(units) > maxEncodedPathLen {
		encoded += "-" + strconv.FormatInt(encodedPathHash(units), 36)
	}
	return encoded
}

// isEncodedAlnum reports whether a UTF-16 code unit survives Claude Code's
// [^a-zA-Z0-9] fold unchanged.
func isEncodedAlnum(u uint16) bool {
	return (u >= 'a' && u <= 'z') || (u >= 'A' && u <= 'Z') || (u >= '0' && u <= '9')
}

// encodedPathHash is Claude Code's 32-bit rolling String.hashCode —
// h = h*31 + unit, wrapped to int32 — over the path's UTF-16 code units, its
// absolute value taken in int64 so the int32 minimum does not overflow.
func encodedPathHash(units []uint16) int64 {
	var h int32
	for _, u := range units {
		h = h*31 + int32(u)
	}
	if h < 0 {
		return -int64(h)
	}
	return int64(h)
}

// ClaudeMemoryDir returns the path to Claude Code's memory directory for the
// given absolute project path. Returns "" if the directory does not exist or
// the resolved path escapes the expected base directory.
func ClaudeMemoryDir(projectPath string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	baseDir := filepath.Join(home, ".claude", "projects")
	encoded := EncodeProjectPath(projectPath)
	dir := filepath.Join(baseDir, encoded, "memory")

	// Prevent path traversal — resolved path must stay under baseDir.
	resolved, err := filepath.Abs(dir)
	if err != nil || !strings.HasPrefix(resolved, filepath.Clean(baseDir)+string(os.PathSeparator)) {
		return ""
	}

	if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
		return ""
	}
	return resolved
}

// skipFile returns true if the filename should not be imported.
func skipFile(name string) bool {
	lower := strings.ToLower(name)
	switch {
	case lower == "memory.md":
		return true
	case strings.HasPrefix(lower, "plan_"):
		return true
	case strings.HasPrefix(lower, "research_"):
		return true
	}
	return false
}

// isStub returns true if the content looks like a migration stub.
func isStub(content string) bool {
	if len(content) < minContentLen {
		return true
	}
	lower := strings.ToLower(content)
	return strings.Contains(lower, "migrated to ghost")
}

// ParseMemoryFile reads a Claude Code memory file and extracts its content,
// Ghost category, and importance. Returns skip=true for files that should
// not be imported.
func ParseMemoryFile(path string) (content, category string, importance float32, skip bool, err error) {
	cleanPath := filepath.Clean(path)
	// Read through a LimitReader rather than os.ReadFile so the ceiling bounds
	// the read, not just the outcome: the extra byte is what distinguishes "at
	// the ceiling" from "past it" without reading the rest of the file.
	f, err := os.Open(cleanPath) // #nosec G304 — path is validated by caller via ClaudeMemoryDir
	if err != nil {
		return "", "", 0, false, err
	}
	defer f.Close() //nolint:errcheck
	data, err := io.ReadAll(io.LimitReader(f, maxMemoryFileBytes+1))
	if err != nil {
		return "", "", 0, false, err
	}
	if len(data) > maxMemoryFileBytes {
		return "", "", 0, true, nil
	}

	name := filepath.Base(path)
	if skipFile(name) {
		return "", "", 0, true, nil
	}

	raw := string(data)
	fmName, fmDesc, fmType, body := parseFrontmatter(raw)

	// Build content: prepend name/description as header if available.
	var sb strings.Builder
	if fmName != "" {
		sb.WriteString(fmName)
		if fmDesc != "" {
			sb.WriteString(" — ")
			sb.WriteString(fmDesc)
		}
		sb.WriteString("\n\n")
	}
	sb.WriteString(strings.TrimSpace(body))
	content = sb.String()

	if isStub(content) {
		return "", "", 0, true, nil
	}

	// One cap for every writer: imports share memory.MaxContentLen with the
	// MCP save path, cut on a rune boundary with the explicit marker
	// appended — not a silent byte slice that could split a rune.
	content, _ = memory.ClampContent(content)

	// Map type to category.
	category = mapCategory(fmType)
	importance = importanceForType(fmType)

	return content, category, importance, false, nil
}

// parseFrontmatter extracts YAML frontmatter fields from markdown content.
// Returns the body without frontmatter.
func parseFrontmatter(raw string) (name, description, fileType, body string) {
	if !strings.HasPrefix(raw, "---\n") {
		return "", "", "", raw
	}

	end := strings.Index(raw[4:], "\n---")
	if end < 0 {
		return "", "", "", raw
	}

	fm := raw[4 : 4+end]
	body = raw[4+end+4:] // skip closing "---\n"

	scanner := bufio.NewScanner(strings.NewReader(fm))
	scanner.Buffer(nil, maxFrontmatterLine)
	for scanner.Scan() {
		line := scanner.Text()
		if k, v, ok := parseYAMLLine(line); ok {
			switch k {
			case "name":
				name = v
			case "description":
				description = v
			case "type":
				fileType = v
			}
		}
	}
	// A line past maxFrontmatterLine stops the scan with an error, and the
	// fields read before it describe a block this function never finished
	// reading. Reporting those would attach a name and a type the file may not
	// have meant; reporting none of them is the same whole-file fallback an
	// unterminated block already gets, and it is the honest one — the file
	// still imports, as body text.
	if err := scanner.Err(); err != nil {
		return "", "", "", raw
	}
	return name, description, fileType, body
}

// parseYAMLLine extracts a simple key: value pair from a YAML line.
func parseYAMLLine(line string) (key, value string, ok bool) {
	idx := strings.Index(line, ":")
	if idx < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:idx])
	value = strings.TrimSpace(line[idx+1:])
	// Strip surrounding quotes.
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
		value = value[1 : len(value)-1]
	}
	return key, value, true
}

// mapCategory converts a Claude memory type to a Ghost category.
func mapCategory(claudeType string) string {
	if cat, ok := categoryMap[strings.ToLower(claudeType)]; ok {
		return cat
	}
	return "fact"
}

// importanceForType returns the importance score for a Claude memory type.
func importanceForType(claudeType string) float32 {
	if imp, ok := importanceMap[strings.ToLower(claudeType)]; ok {
		return imp
	}
	return defaultImportance
}

// Import discovers and imports all Claude Code memories for a project.
// Returns the number of memories imported. Read-only — never modifies
// Claude's files. Writes through store.UpsertWithOptions with SkipExactContent;
// see importFromDir.
func Import(ctx context.Context, store provider.MemoryStore, projectID, projectPath string, logger *slog.Logger) (int, []string, error) {
	dir := ClaudeMemoryDir(projectPath)
	if dir == "" {
		return 0, nil, nil
	}
	return importFromDir(ctx, store, projectID, dir, logger)
}

// importFromDir imports all Claude Code memories from a specific directory.
//
// Dedup guard: this is the ONLY caller in the codebase whose re-runs must be
// true no-ops (ghost mcp init documents itself as idempotent and safe to re-run,
// and calls this unconditionally every time). A plain Upsert is
// intentionally non-destructive — a byte-identical save creates a new, linked
// row rather than silently merging — so the import asks for the skip itself:
// each file is saved with UpsertOptions.SkipExactContent, which writes nothing
// when the project already holds that exact content. This keeps re-imports of
// previously-seen Claude Code memory files from doubling the project's memory
// count on every init.
//
// Only an EXACT match is skipped. A file that is a near-duplicate of a stored
// row (or of a file earlier in the same run) takes Upsert's ordinary path: it
// is stored as its own row, linked to that row as a 'duplicate', and the row
// it folded into is strengthened.
//
// The exact-content check runs inside Upsert's write transaction, ahead of its
// near-duplicate lookups, so it holds across handles: two clients that reach
// first contact together cannot both store a file (#1234). The GetAll pre-read
// below is only a cheap skip that saves a write transaction per file already
// present; it does not decide anything on its own.
//
// The guard is the original first-contact one: the import runs only when the
// project has no memories, so a second run on an established project imports
// nothing. A file refused by the token guard, or one that failed for any other
// reason, is reported by name in the returned slice. The report uses errors.As
// on wrapped errors so a wrapped rejection is still recognised. The name is
// quoted downstream through quoteData or memory.ProjectArg; its text is never
// echoed.
//
// The write goes through provider.MemoryStore's own UpsertWithOptions, so
// every store the import can be handed has the guarded path, and there is no
// fallback to an unguarded one.
func importFromDir(ctx context.Context, store provider.MemoryStore, projectID, dir string, logger *slog.Logger) (int, []string, error) {
	cleanDir := filepath.Clean(dir)
	entries, err := os.ReadDir(cleanDir)
	if err != nil {
		return 0, nil, fmt.Errorf("read claude memory dir: %w", err)
	}

	existing, err := store.GetAll(ctx, projectID, 100000)
	if err != nil {
		logger.Warn("claude import: could not load existing content for dedup guard", "error", err)
	}
	seen := make(map[string]bool, len(existing))
	for _, m := range existing {
		seen[m.Content] = true
	}

	var imported int
	var skipped []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}

		path := filepath.Join(cleanDir, filepath.Base(entry.Name()))
		content, category, importance, skip, err := ParseMemoryFile(path)
		if err != nil {
			logger.Warn("claude import: file read failed", "file", entry.Name(), "error", err)
			skipped = append(skipped, entry.Name())
			continue
		}
		if skip {
			continue
		}
		if seen[content] {
			continue
		}

		id, _, _, err := store.UpsertWithOptions(ctx, projectID, category, content, source, importance, importTags,
			memory.UpsertOptions{SkipExactContent: true})
		if err != nil {
			var secretErr *memory.SecretContentError
			if errors.As(err, &secretErr) {
				skipped = append(skipped, entry.Name())
			} else {
				skipped = append(skipped, entry.Name())
			}
			logger.Warn("claude import: upsert failed", "file", entry.Name(), "error", err)
			continue
		}
		seen[content] = true
		// An empty id is the skip: another handle stored this exact content
		// between the pre-read and the write. The file is imported either way,
		// so it is counted once, by whoever stored it.
		if id != "" {
			imported++
		}
	}

	if imported > 0 {
		logger.Info("claude memory import complete", "project", projectID, "memories", imported)
	}

	return imported, skipped, nil
}
