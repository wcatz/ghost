package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/wcatz/ghost/internal/ai"
)

type gitInput struct {
	Subcommand string   `json:"subcommand"`
	Args       []string `json:"args"`
}

// readOnlyGitCmds are git subcommands that are safe to run without approval.
var readOnlyGitCmds = map[string]bool{
	"status": true, "diff": true, "log": true, "show": true,
	"branch": true, "tag": true, "remote": true, "rev-parse": true,
	"ls-files": true, "ls-tree": true, "blame": true, "shortlog": true,
}

// blockedGitPatterns are dangerous operations that are never allowed.
var blockedGitPatterns = []string{
	"push --force", "push -f", "reset --hard", "clean -f",
	"clean -fd", "clean -fx",
}

func registerGit(r *Registry) {
	r.Register(
		ai.ToolDefinition{
			Name:        "git",
			Description: "Run git commands. Read operations (status, diff, log, branch) are auto-approved. Write operations (add, commit, checkout, stash) require confirmation. Destructive operations (push --force, reset --hard) are blocked.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"subcommand": map[string]interface{}{"type": "string", "description": "Git subcommand: status, diff, log, branch, add, commit, checkout, stash, etc."},
					"args":       map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Arguments to the subcommand"},
				},
				"required": []string{"subcommand"},
			},
		},
		execGit,
		ApprovalWarn, // Dynamic: read-only gets auto-approved, write requires confirm
	)
}

func execGit(ctx context.Context, projectPath string, input json.RawMessage) Result {
	var in gitInput
	if err := json.Unmarshal(input, &in); err != nil {
		return Result{Content: fmt.Sprintf("invalid input: %v", err), IsError: true}
	}

	// Check for blocked patterns.
	fullCmd := in.Subcommand + " " + strings.Join(in.Args, " ")
	for _, blocked := range blockedGitPatterns {
		if strings.Contains(fullCmd, blocked) {
			return Result{Content: fmt.Sprintf("blocked: '%s' is a destructive operation", blocked), IsError: true}
		}
	}

	args := append([]string{in.Subcommand}, in.Args...)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = projectPath

	output, err := cmd.CombinedOutput()
	result := strings.TrimRight(string(output), "\n")

	if err != nil {
		return Result{Content: fmt.Sprintf("git error: %v\n%s", err, result), IsError: true}
	}

	if result == "" {
		result = "(no output)"
	}

	return Result{Content: result}
}

// IsReadOnlyGit returns true if a git subcommand is read-only.
func IsReadOnlyGit(subcommand string) bool {
	return readOnlyGitCmds[subcommand]
}
