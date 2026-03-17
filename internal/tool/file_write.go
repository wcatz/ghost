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
			Description: "Write content to a file, creating it and any parent directories if needed. Overwrites existing files. Always use file_read first if modifying an existing file — prefer file_edit for surgical changes.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":    map[string]interface{}{"type": "string", "description": "Absolute or project-relative file path"},
					"content": map[string]interface{}{"type": "string", "description": "Content to write to the file"},
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

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Result{Content: fmt.Sprintf("cannot create directories: %v", err), IsError: true}
	}

	if err := os.WriteFile(path, []byte(in.Content), 0o644); err != nil {
		return Result{Content: fmt.Sprintf("cannot write file: %v", err), IsError: true}
	}

	return Result{Content: fmt.Sprintf("wrote %d bytes to %s", len(in.Content), path)}
}
