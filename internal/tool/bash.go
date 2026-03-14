package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/ai"
)

type bashInput struct {
	Command    string `json:"command"`
	TimeoutMs  int    `json:"timeout_ms"`
	WorkingDir string `json:"working_dir"`
}

func registerBash(r *Registry) {
	r.Register(
		ai.ToolDefinition{
			Name:        "bash",
			Description: "Execute a shell command and return its output. Use for running tests, builds, or other system commands.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"command":     map[string]interface{}{"type": "string", "description": "Shell command to execute"},
					"timeout_ms":  map[string]interface{}{"type": "integer", "description": "Timeout in milliseconds (default: 30000, max: 120000)"},
					"working_dir": map[string]interface{}{"type": "string", "description": "Working directory (default: project root)"},
				},
				"required": []string{"command"},
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

	// Block destructive commands.
	lower := strings.ToLower(in.Command)
	blocked := []string{"rm -rf /", "rm -rf ~", "mkfs", "dd if=", ":(){", "fork bomb"}
	for _, b := range blocked {
		if strings.Contains(lower, b) {
			return Result{Content: fmt.Sprintf("blocked: dangerous command pattern '%s'", b), IsError: true}
		}
	}

	timeout := time.Duration(in.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if timeout > 120*time.Second {
		timeout = 120 * time.Second
	}

	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	workDir := projectPath
	if in.WorkingDir != "" {
		safeDir, err := safePath(projectPath, in.WorkingDir)
		if err != nil {
			return Result{Content: fmt.Sprintf("invalid working directory: %v", err), IsError: true}
		}
		workDir = safeDir
	}

	cmd := exec.CommandContext(execCtx, "bash", "-c", in.Command)
	cmd.Dir = workDir

	output, err := cmd.CombinedOutput()
	result := string(output)

	// Truncate very large output.
	const maxOutput = 50000
	if len(result) > maxOutput {
		result = result[:maxOutput] + fmt.Sprintf("\n... (truncated, %d total bytes)", len(output))
	}

	if err != nil {
		if execCtx.Err() == context.DeadlineExceeded {
			return Result{Content: fmt.Sprintf("command timed out after %s\n%s", timeout, result), IsError: true}
		}
		return Result{Content: fmt.Sprintf("exit status: %v\n%s", err, result), IsError: true}
	}

	if result == "" {
		result = "(no output)"
	}

	return Result{Content: result}
}
