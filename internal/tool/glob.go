package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wcatz/ghost/internal/ai"
)

type globInput struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

func registerGlob(r *Registry) {
	r.Register(
		ai.ToolDefinition{
			Name:        "glob",
			Description: "Find files matching a glob pattern. Returns file paths sorted by modification time (newest first).",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"pattern": map[string]interface{}{"type": "string", "description": "Glob pattern, e.g. '**/*.go', 'internal/**/*.test.ts'"},
					"path":    map[string]interface{}{"type": "string", "description": "Base directory (default: project root)"},
				},
				"required": []string{"pattern"},
			},
		},
		execGlob,
		ApprovalNone,
	)
}

func execGlob(ctx context.Context, projectPath string, input json.RawMessage) Result {
	var in globInput
	if err := json.Unmarshal(input, &in); err != nil {
		return Result{Content: fmt.Sprintf("invalid input: %v", err), IsError: true}
	}

	basePath := projectPath
	if in.Path != "" {
		basePath = resolvePath(projectPath, in.Path)
	}

	type fileEntry struct {
		path    string
		modTime int64
	}

	const maxDepth = 15

	var matches []fileEntry
	pattern := in.Pattern

	err := filepath.Walk(basePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip errors
		}
		// Skip common ignored directories and enforce depth limit.
		if info.IsDir() {
			name := info.Name()
			if name == ".git" || name == "node_modules" || name == "vendor" || name == ".next" || name == "dist" || name == "build" {
				return filepath.SkipDir
			}
			relDir, _ := filepath.Rel(basePath, path)
			if relDir != "." && strings.Count(relDir, string(filepath.Separator)) >= maxDepth {
				return filepath.SkipDir
			}
			return nil
		}

		relPath, _ := filepath.Rel(basePath, path)
		matched, _ := filepath.Match(pattern, filepath.Base(path))
		if !matched {
			// Try matching against the full relative path for ** patterns.
			matched, _ = filepath.Match(pattern, relPath)
		}
		if !matched && strings.Contains(pattern, "**") {
			// Simple ** support: match suffix after **/
			suffix := strings.TrimPrefix(pattern, "**/")
			matched, _ = filepath.Match(suffix, filepath.Base(path))
		}

		if matched {
			matches = append(matches, fileEntry{relPath, info.ModTime().Unix()})
			if len(matches) >= 1000 {
				return fmt.Errorf("match limit reached")
			}
		}
		return nil
	})
	if err != nil && len(matches) == 0 {
		return Result{Content: fmt.Sprintf("glob error: %v", err), IsError: true}
	}

	// Sort by modification time, newest first.
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].modTime > matches[j].modTime
	})

	if len(matches) == 0 {
		return Result{Content: "no files found matching pattern"}
	}

	var sb strings.Builder
	maxShow := 200
	for i, m := range matches {
		if i >= maxShow {
			sb.WriteString(fmt.Sprintf("\n... and %d more files", len(matches)-maxShow))
			break
		}
		sb.WriteString(m.path)
		sb.WriteByte('\n')
	}

	return Result{Content: strings.TrimRight(sb.String(), "\n")}
}
