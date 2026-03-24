// Package claudeimport reads Claude Code's auto-memory files and imports
// them into Ghost's memory store. This is a read-only operation — Claude's
// files are never modified or deleted.
package claudeimport

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/wcatz/ghost/internal/provider"
)

const (
	maxContentLen = 4000
	minContentLen = 20
	source        = "onboarding"
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

// EncodeProjectPath converts an absolute path to Claude's directory name format.
// E.g., "/home/wayne/git/ghost" → "-home-wayne-git-ghost".
func EncodeProjectPath(projectPath string) string {
	return strings.ReplaceAll(projectPath, "/", "-")
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
	data, err := os.ReadFile(cleanPath) // #nosec G304 — path is validated by caller via ClaudeMemoryDir
	if err != nil {
		return "", "", 0, false, err
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

	// Truncate if too long.
	if len(content) > maxContentLen {
		content = content[:maxContentLen]
	}

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
// Claude's files. Uses store.Upsert for dedup.
func Import(ctx context.Context, store provider.MemoryStore, projectID, projectPath string, logger *slog.Logger) (int, error) {
	dir := ClaudeMemoryDir(projectPath)
	if dir == "" {
		return 0, nil
	}
	return importFromDir(ctx, store, projectID, dir, logger)
}

// importFromDir imports all Claude Code memories from a specific directory.
func importFromDir(ctx context.Context, store provider.MemoryStore, projectID, dir string, logger *slog.Logger) (int, error) {
	cleanDir := filepath.Clean(dir)
	entries, err := os.ReadDir(cleanDir)
	if err != nil {
		return 0, fmt.Errorf("read claude memory dir: %w", err)
	}

	var imported int
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}

		path := filepath.Join(cleanDir, filepath.Base(entry.Name()))
		content, category, importance, skip, err := ParseMemoryFile(path)
		if err != nil {
			logger.Warn("claude import: file read failed", "file", entry.Name(), "error", err)
			continue
		}
		if skip {
			continue
		}

		_, _, err = store.Upsert(ctx, projectID, category, content, source, importance, importTags)
		if err != nil {
			logger.Warn("claude import: upsert failed", "file", entry.Name(), "error", err)
			continue
		}
		imported++
	}

	if imported > 0 {
		logger.Info("claude memory import complete", "project", projectID, "memories", imported)
	}

	return imported, nil
}
