package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/ai"
)

type bashInput struct {
	Command        string `json:"command"`
	Description    string `json:"description"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

// blockedBashPatterns are command fragments that are always rejected.
// The real safety gate is ApprovalRequire — the user must approve every command.
// This list catches catastrophic mistakes only.
var blockedBashPatterns = []string{
	"rm -rf /",
	"rm -rf ~/",
	"mkfs",
	"dd if=/dev/zero",
	"dd if=/dev/urandom",
	":(){ :|:& };:",
	"> /dev/sd",
}

func registerBash(r *Registry) {
	r.Register(
		ai.ToolDefinition{
			Name:        "bash",
			Description: "Run a shell command in the project directory. Use for git operations (branch, commit, push, PR), build/test (go build, go test, go vet), cluster management (kubectl, helmfile, helm), and other CLI tools (gh). Always provide a clear human-readable description.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"command":         map[string]interface{}{"type": "string", "description": "Shell command to execute"},
					"description":     map[string]interface{}{"type": "string", "description": "Human-readable description shown in the approval dialog (e.g. 'Create feature branch feat/foo', 'Run go vet')"},
					"timeout_seconds": map[string]interface{}{"type": "integer", "description": "Timeout in seconds (default: 60, max: 300)"},
				},
				"required": []string{"command", "description"},
			},
		},
		execBash,
		ApprovalRequire,
	)
}

func execBash(ctx context.Context, projectPath string, input json.RawMessage) Result {
	var in bashInput
	if err := json.Unmarshal(input, &in); err != nil {
		return Result{Content: fmt.Sprintf("invalid input: %v", err), IsError: true}
	}

	lower := strings.ToLower(in.Command)
	for _, pattern := range blockedBashPatterns {
		if strings.Contains(lower, strings.ToLower(pattern)) {
			return Result{Content: fmt.Sprintf("blocked: command matches disallowed pattern %q", pattern), IsError: true}
		}
	}

	timeout := time.Duration(in.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if timeout > 300*time.Second {
		timeout = 300 * time.Second
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/bash", "-c", in.Command)
	cmd.Dir = projectPath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	out := strings.TrimRight(stdout.String(), "\n")
	errOut := strings.TrimRight(stderr.String(), "\n")

	combined := out
	if errOut != "" {
		if combined != "" {
			combined += "\n"
		}
		combined += errOut
	}
	if combined == "" {
		combined = "(no output)"
	}

	if err != nil {
		if ctx.Err() != nil {
			return Result{Content: fmt.Sprintf("timed out after %s\n%s", timeout, combined), IsError: true}
		}
		return Result{Content: fmt.Sprintf("exit error: %v\n%s", err, combined), IsError: true}
	}

	return Result{Content: combined}
}
