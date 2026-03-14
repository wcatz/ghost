package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wcatz/ghost/internal/ai"
)

type fileWriteInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func registerFileWrite(r *Registry) {
	r.Register(
		ai.ToolDefinition{
			Name:        "file_write",
			Description: "Create or overwrite a file with the given content. Creates parent directories if needed.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":    map[string]interface{}{"type": "string", "description": "Absolute or project-relative file path"},
					"content": map[string]interface{}{"type": "string", "description": "Complete file content to write"},
				},
				"required": []string{"path", "content"},
			},
		},
		execFileWrite,
		ApprovalWarn,
	)
}

func execFileWrite(ctx context.Context, projectPath string, input json.RawMessage) Result {
	var in fileWriteInput
	if err := json.Unmarshal(input, &in); err != nil {
		return Result{Content: fmt.Sprintf("invalid input: %v", err), IsError: true}
	}

	path, err := safePath(projectPath, in.Path)
	if err != nil {
		return Result{Content: err.Error(), IsError: true}
	}

	// Create parent directories.
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{Content: fmt.Sprintf("cannot create directory: %v", err), IsError: true}
	}

	if err := os.WriteFile(path, []byte(in.Content), 0o644); err != nil {
		return Result{Content: fmt.Sprintf("write failed: %v", err), IsError: true}
	}

	return Result{Content: fmt.Sprintf("wrote %d bytes to %s", len(in.Content), path)}
}
