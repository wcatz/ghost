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

// blockedGitOps maps subcommands to exact flags that are never allowed.
var blockedGitOps = map[string][]string{
	"push":  {"--force", "-f"},
	"reset": {"--hard"},
}

// blockedGitClean checks if a git clean arg contains the -f flag
// in any combination (e.g. -f, -fd, -fdx, -xf).
func isBlockedCleanArg(arg string) bool {
	return strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.ContainsRune(arg, 'f')
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

	// Check for blocked operations by matching subcommand + individual flags.
	if blockedFlags, ok := blockedGitOps[in.Subcommand]; ok {
		for _, arg := range in.Args {
			for _, blocked := range blockedFlags {
				if arg == blocked {
					return Result{Content: fmt.Sprintf("blocked: 'git %s %s' is a destructive operation", in.Subcommand, blocked), IsError: true}
				}
			}
		}
	}
	// git clean with -f in any flag combination is always blocked.
	if in.Subcommand == "clean" {
		for _, arg := range in.Args {
			if isBlockedCleanArg(arg) {
				return Result{Content: fmt.Sprintf("blocked: 'git clean %s' is a destructive operation", arg), IsError: true}
			}
		}
	}
	// Block flags that redirect git to a different repository.
	for _, arg := range in.Args {
		lower := strings.ToLower(arg)
		if lower == "-c" || strings.HasPrefix(lower, "--git-dir") || strings.HasPrefix(lower, "--work-tree") {
			return Result{Content: fmt.Sprintf("blocked: '%s' flag not allowed — git must operate in project directory", arg), IsError: true}
		}
	}
	// Also block -C passed as the subcommand itself (git -C /path ...).
	if strings.ToLower(in.Subcommand) == "-c" {
		return Result{Content: "blocked: '-C' flag not allowed — git must operate in project directory", IsError: true}
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

