// Package claudeimport imports Claude Code auto-memory files into Ghost.
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

	maxMemoryFileBytes = 1 << 20
	maxFrontmatterLine = 256 << 10
)

var importTags = []string{"claude-code", "auto-import"}

var preSkipWindow = 100000

var categoryMap = map[string]string{
	"feedback":  "preference",
	"project":   "architecture",
	"reference": "fact",
	"pattern":   "pattern",
	"decision":  "decision",
	"gotcha":    "gotcha",
	"user":      "preference",
}

var importanceMap = map[string]float32{
	"feedback": 0.9,
	"project":  0.8,
	"user":     0.9,
}

const defaultImportance float32 = 0.7

const maxEncodedPathLen = 200

// EncodeProjectPath returns the directory name Claude Code uses for projectPath.
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

func isEncodedAlnum(u uint16) bool {
	return (u >= 'a' && u <= 'z') || (u >= 'A' && u <= 'Z') || (u >= '0' && u <= '9')
}

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

// ClaudeMemoryDir returns the Claude Code memory directory for projectPath, or "" if unavailable.
func ClaudeMemoryDir(projectPath string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	baseDir := filepath.Join(home, ".claude", "projects")
	encoded := EncodeProjectPath(projectPath)
	dir := filepath.Join(baseDir, encoded, "memory")

	resolved, err := filepath.Abs(dir)
	if err != nil || !strings.HasPrefix(resolved, filepath.Clean(baseDir)+string(os.PathSeparator)) {
		return ""
	}

	if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
		return ""
	}
	return resolved
}

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

func isStub(content string) bool {
	if len(content) < minContentLen {
		return true
	}
	lower := strings.ToLower(content)
	return strings.Contains(lower, "migrated to ghost")
}

// ParseMemoryFile reads a Claude Code memory file and returns its imported content.
// skip is true when the file should not be imported.
func ParseMemoryFile(path string) (content, category string, importance float32, skip bool, err error) {
	cleanPath := filepath.Clean(path)
	f, err := openMemoryFile(cleanPath)
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

	content, _ = memory.ClampContent(content)
	category = mapCategory(fmType)
	importance = importanceForType(fmType)

	return content, category, importance, false, nil
}

func openMemoryFile(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file (%s)", info.Mode().Type())
	}
	f, err := os.OpenFile(path, memoryFileOpenFlags, 0) // #nosec G304 — path is validated by caller via ClaudeMemoryDir
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !opened.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("not a regular file (%s)", opened.Mode().Type())
	}
	return f, nil
}

func parseFrontmatter(raw string) (name, description, fileType, body string) {
	if !strings.HasPrefix(raw, "---\n") {
		return "", "", "", raw
	}

	end := strings.Index(raw[4:], "\n---")
	if end < 0 {
		return "", "", "", raw
	}

	fm := raw[4 : 4+end]
	body = raw[4+end+4:]

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
	if err := scanner.Err(); err != nil {
		return "", "", "", raw
	}
	return name, description, fileType, body
}

func parseYAMLLine(line string) (key, value string, ok bool) {
	idx := strings.Index(line, ":")
	if idx < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:idx])
	value = strings.TrimSpace(line[idx+1:])
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
		value = value[1 : len(value)-1]
	}
	return key, value, true
}

func mapCategory(claudeType string) string {
	if cat, ok := categoryMap[strings.ToLower(claudeType)]; ok {
		return cat
	}
	return "fact"
}

func importanceForType(claudeType string) float32 {
	if imp, ok := importanceMap[strings.ToLower(claudeType)]; ok {
		return imp
	}
	return defaultImportance
}

// Import discovers and imports Claude Code memories for projectPath.
// It returns the import count and the names of refused, failed, and non-regular entries.
func Import(ctx context.Context, store provider.MemoryStore, projectID, projectPath string, logger *slog.Logger) (int, []string, []string, []string, error) {
	dir := ClaudeMemoryDir(projectPath)
	if dir == "" {
		return 0, nil, nil, nil, nil
	}
	return importFromDir(ctx, store, projectID, dir, logger)
}

func importFromDir(ctx context.Context, store provider.MemoryStore, projectID, dir string, logger *slog.Logger) (int, []string, []string, []string, error) {
	cleanDir := filepath.Clean(dir)
	entries, err := os.ReadDir(cleanDir)
	if err != nil {
		return 0, nil, nil, nil, fmt.Errorf("read claude memory dir: %w", err)
	}

	existing, err := store.GetAll(ctx, projectID, preSkipWindow)
	if err != nil {
		logger.Warn("claude import: could not load existing content for dedup guard", "error", err)
	}
	seen := make(map[string]bool, len(existing))
	for _, m := range existing {
		seen[m.Content] = true
	}

	var imported int
	var refused, failed, notRegular []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		if !entry.Type().IsRegular() {
			if skipFile(entry.Name()) {
				continue
			}
			logger.Warn("claude import: not a regular file", "file", entry.Name(), "type", entry.Type().String())
			notRegular = append(notRegular, entry.Name())
			continue
		}

		path := filepath.Join(cleanDir, filepath.Base(entry.Name()))
		content, category, importance, skip, err := ParseMemoryFile(path)
		if err != nil {
			logger.Warn("claude import: file read failed", "file", entry.Name(), "error", err)
			failed = append(failed, entry.Name())
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
				refused = append(refused, entry.Name())
			} else {
				failed = append(failed, entry.Name())
			}
			logger.Warn("claude import: upsert failed", "file", entry.Name(), "error", err)
			continue
		}
		seen[content] = true
		if id != "" {
			imported++
		}
	}

	if imported > 0 {
		logger.Info("claude memory import complete", "project", projectID, "memories", imported)
	}

	return imported, refused, failed, notRegular, nil
}
