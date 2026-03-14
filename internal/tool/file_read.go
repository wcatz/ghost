package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wcatz/ghost/internal/ai"
)

type fileReadInput struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

func registerFileRead(r *Registry) {
	r.Register(
		ai.ToolDefinition{
			Name:        "file_read",
			Description: "Read a file's contents. Returns the file content with line numbers. Use offset and limit for large files.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":   map[string]interface{}{"type": "string", "description": "Absolute or project-relative file path"},
					"offset": map[string]interface{}{"type": "integer", "description": "Start line (1-based, optional, default 1)"},
					"limit":  map[string]interface{}{"type": "integer", "description": "Max lines to read (optional, default 2000)"},
				},
				"required": []string{"path"},
			},
		},
		execFileRead,
		ApprovalNone,
	)
}

func execFileRead(ctx context.Context, projectPath string, input json.RawMessage) Result {
	var in fileReadInput
	if err := json.Unmarshal(input, &in); err != nil {
		return Result{Content: fmt.Sprintf("invalid input: %v", err), IsError: true}
	}

	path, err := safePath(projectPath, in.Path)
	if err != nil {
		return Result{Content: err.Error(), IsError: true}
	}

	info, err := os.Stat(path)
	if err != nil {
		return Result{Content: fmt.Sprintf("file not found: %s", path), IsError: true}
	}
	if info.IsDir() {
		return Result{Content: fmt.Sprintf("%s is a directory, not a file", path), IsError: true}
	}

	f, err := os.Open(path)
	if err != nil {
		return Result{Content: fmt.Sprintf("cannot open: %v", err), IsError: true}
	}
	defer f.Close()

	offset := in.Offset
	if offset < 1 {
		offset = 1
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 2000
	}

	var sb strings.Builder
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	lineNum := 0
	linesRead := 0

	for scanner.Scan() {
		lineNum++
		if lineNum < offset {
			continue
		}
		if linesRead >= limit {
			sb.WriteString(fmt.Sprintf("\n... (truncated at %d lines)", limit))
			break
		}
		line := scanner.Text()
		if len(line) > 2000 {
			line = line[:2000] + "..."
		}
		sb.WriteString(fmt.Sprintf("%6d\t%s\n", lineNum, line))
		linesRead++
	}

	if err := scanner.Err(); err != nil {
		return Result{Content: fmt.Sprintf("read error: %v", err), IsError: true}
	}

	if linesRead == 0 {
		return Result{Content: "(empty file)"}
	}

	return Result{Content: sb.String()}
}

// resolvePath resolves a potentially relative path against the project root.
func resolvePath(projectPath, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(projectPath, path)
}

// safePath resolves a path and ensures it stays within the project directory.
// Returns the resolved path and an error if the path escapes the project root.
func safePath(projectPath, path string) (string, error) {
	resolved := resolvePath(projectPath, path)
	cleaned := filepath.Clean(resolved)
	projCleaned := filepath.Clean(projectPath)

	if !strings.HasPrefix(cleaned, projCleaned+string(filepath.Separator)) && cleaned != projCleaned {
		return "", fmt.Errorf("path %q escapes project directory", path)
	}
	return cleaned, nil
}
