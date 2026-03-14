package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/wcatz/ghost/internal/ai"
)

type grepInput struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	Context    int    `json:"context"`
	MaxResults int    `json:"max_results"`
}

func registerGrep(r *Registry) {
	r.Register(
		ai.ToolDefinition{
			Name:        "grep",
			Description: "Search file contents using regex patterns. Uses ripgrep if available, falls back to grep.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"pattern":     map[string]interface{}{"type": "string", "description": "Regex pattern to search for"},
					"path":        map[string]interface{}{"type": "string", "description": "Directory or file to search (default: project root)"},
					"glob":        map[string]interface{}{"type": "string", "description": "File glob filter, e.g. '*.go'"},
					"context":     map[string]interface{}{"type": "integer", "description": "Lines of context around matches (default: 0)"},
					"max_results": map[string]interface{}{"type": "integer", "description": "Maximum matches to return (default: 50)"},
				},
				"required": []string{"pattern"},
			},
		},
		execGrep,
		ApprovalNone,
	)
}

func execGrep(ctx context.Context, projectPath string, input json.RawMessage) Result {
	var in grepInput
	if err := json.Unmarshal(input, &in); err != nil {
		return Result{Content: fmt.Sprintf("invalid input: %v", err), IsError: true}
	}

	searchPath := projectPath
	if in.Path != "" {
		searchPath = resolvePath(projectPath, in.Path)
	}

	maxResults := in.MaxResults
	if maxResults <= 0 {
		maxResults = 50
	}

	// Try ripgrep first, fall back to grep.
	args := buildGrepArgs(in, searchPath, maxResults)
	cmd := "rg"
	if _, err := exec.LookPath("rg"); err != nil {
		cmd = "grep"
		args = buildGrepFallbackArgs(in, searchPath, maxResults)
	}

	c := exec.CommandContext(ctx, cmd, args...)
	output, err := c.CombinedOutput()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return Result{Content: "no matches found"}
		}
		if len(output) > 0 {
			return Result{Content: string(output), IsError: true}
		}
		return Result{Content: fmt.Sprintf("grep error: %v", err), IsError: true}
	}

	result := strings.TrimRight(string(output), "\n")
	lines := strings.Count(result, "\n") + 1
	if lines > maxResults*3 {
		// Truncate if too verbose.
		parts := strings.SplitN(result, "\n", maxResults*3+1)
		result = strings.Join(parts[:maxResults*3], "\n") + fmt.Sprintf("\n... (%d more lines)", lines-maxResults*3)
	}

	return Result{Content: result}
}

func buildGrepArgs(in grepInput, searchPath string, maxResults int) []string {
	args := []string{"-n", "--no-heading", "--color=never", fmt.Sprintf("-m%d", maxResults)}
	if in.Context > 0 {
		args = append(args, fmt.Sprintf("-C%d", in.Context))
	}
	if in.Glob != "" {
		args = append(args, "--glob", in.Glob)
	}
	args = append(args, "-e", in.Pattern, "--", searchPath)
	return args
}

func buildGrepFallbackArgs(in grepInput, searchPath string, maxResults int) []string {
	args := []string{"-rn", "--color=never", fmt.Sprintf("-m%d", maxResults)}
	if in.Context > 0 {
		args = append(args, fmt.Sprintf("-C%d", in.Context))
	}
	if in.Glob != "" {
		args = append(args, "--include", in.Glob)
	}
	args = append(args, "-e", in.Pattern, "--", searchPath)
	return args
}
