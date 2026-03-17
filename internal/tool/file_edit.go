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
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

func registerFileEdit(r *Registry) {
	r.Register(
		ai.ToolDefinition{
			Name:        "file_edit",
			Description: "Perform exact string replacement in a file. old_string must match exactly (including whitespace/indentation). If old_string appears more than once, set replace_all=true or provide more surrounding context to make it unique. Always read the file first to get the exact content.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":        map[string]interface{}{"type": "string", "description": "Absolute or project-relative file path"},
					"old_string":  map[string]interface{}{"type": "string", "description": "Exact text to find and replace"},
					"new_string":  map[string]interface{}{"type": "string", "description": "Text to replace it with"},
					"replace_all": map[string]interface{}{"type": "boolean", "description": "Replace all occurrences (default: false — fails if more than one match)"},
				},
				"required": []string{"path", "old_string", "new_string"},
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

	path, err := safePath(projectPath, in.Path)
	if err != nil {
		return Result{Content: err.Error(), IsError: true}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return Result{Content: fmt.Sprintf("cannot read file: %v", err), IsError: true}
	}
	content := string(data)

	count := strings.Count(content, in.OldString)
	if count == 0 {
		return Result{Content: "old_string not found in file — read the file first to get exact content", IsError: true}
	}
	if count > 1 && !in.ReplaceAll {
		return Result{Content: fmt.Sprintf("old_string found %d times — set replace_all=true or provide more surrounding context to make it unique", count), IsError: true}
	}

	var updated string
	if in.ReplaceAll {
		updated = strings.ReplaceAll(content, in.OldString, in.NewString)
	} else {
		updated = strings.Replace(content, in.OldString, in.NewString, 1)
	}

	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return Result{Content: fmt.Sprintf("cannot write file: %v", err), IsError: true}
	}

	n := count
	if !in.ReplaceAll {
		n = 1
	}
	return Result{Content: fmt.Sprintf("replaced %d occurrence(s) in %s", n, path)}
}
