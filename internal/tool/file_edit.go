package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/wcatz/ghost/internal/ai"
)

type fileEditInput struct {
	Path    string `json:"path"`
	OldText string `json:"old_text"`
	NewText string `json:"new_text"`
}

func registerFileEdit(r *Registry) {
	r.Register(
		ai.ToolDefinition{
			Name:        "file_edit",
			Description: "Replace exact text in a file. The old_text must match uniquely in the file. Use file_read first to see the exact content.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":     map[string]interface{}{"type": "string", "description": "File path"},
					"old_text": map[string]interface{}{"type": "string", "description": "Exact text to find (must match uniquely)"},
					"new_text": map[string]interface{}{"type": "string", "description": "Replacement text"},
				},
				"required": []string{"path", "old_text", "new_text"},
			},
		},
		execFileEdit,
		ApprovalWarn,
	)
}

func execFileEdit(ctx context.Context, projectPath string, input json.RawMessage) Result {
	var in fileEditInput
	if err := json.Unmarshal(input, &in); err != nil {
		return Result{Content: fmt.Sprintf("invalid input: %v", err), IsError: true}
	}

	path := resolvePath(projectPath, in.Path)

	content, err := os.ReadFile(path)
	if err != nil {
		return Result{Content: fmt.Sprintf("cannot read %s: %v", path, err), IsError: true}
	}

	fileStr := string(content)
	count := strings.Count(fileStr, in.OldText)

	if count == 0 {
		return Result{Content: "old_text not found in file", IsError: true}
	}
	if count > 1 {
		return Result{Content: fmt.Sprintf("old_text matches %d times — must be unique. Provide more context.", count), IsError: true}
	}

	newContent := strings.Replace(fileStr, in.OldText, in.NewText, 1)
	if err := os.WriteFile(path, []byte(newContent), 0o644); err != nil {
		return Result{Content: fmt.Sprintf("write failed: %v", err), IsError: true}
	}

	return Result{Content: fmt.Sprintf("edited %s (replaced %d chars with %d chars)", path, len(in.OldText), len(in.NewText))}
}
