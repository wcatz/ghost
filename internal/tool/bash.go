package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
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

// blockedPatterns are destructive commands that are always rejected.
var blockedPatterns = []string{
	"rm -rf /", "rm -rf ~", "mkfs", "dd if=", ":(){", "fork bomb",
	"chmod 777", "chmod -r 777",
	"> /dev/sd",
	"shutdown", "reboot", "halt", "poweroff",
	"init 0", "init 6",
}

// exfilPatterns detect pipe-based data exfiltration to network tools or subshells.
var exfilPatterns = []string{
	"| curl ", "|curl ",
	"| wget ", "|wget ",
	"| nc ", "|nc ",
	"| netcat ", "|netcat ",
	"| ncat ", "|ncat ",
}

// cmdSubRe matches command substitution: $(...) or `...`.
var cmdSubRe = regexp.MustCompile(`\$\(|\x60[^\x60]+\x60`)

// checkBlocked returns a reason if the command matches any blocked pattern.
func checkBlocked(cmd string) string {
	lower := strings.ToLower(cmd)
	for _, b := range blockedPatterns {
		if strings.Contains(lower, b) {
			return fmt.Sprintf("dangerous command pattern '%s'", b)
		}
	}
	for _, p := range exfilPatterns {
		if strings.Contains(lower, p) {
			return fmt.Sprintf("pipe to network tool '%s'", strings.TrimSpace(p))
		}
	}
	if cmdSubRe.MatchString(cmd) {
		return "command substitution ($() or backticks) is not allowed"
	}
	return ""
}

// safeEnv returns a restricted set of environment variables for the child process.
// Secrets (API keys, tokens) are excluded.
func safeEnv() []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"USER=" + os.Getenv("USER"),
		"TERM=" + os.Getenv("TERM"),
		"LANG=" + os.Getenv("LANG"),
		"SHELL=/bin/bash",
	}
	// XDG dirs for tool compatibility.
	for _, key := range []string{"XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR"} {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}
	// Git env vars.
	for _, key := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"} {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}
	// Go toolchain.
	for _, key := range []string{"GOPATH", "GOROOT", "GOMODCACHE", "GOPROXY", "GONOSUMCHECK", "GOTOOLCHAIN"} {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}
	return env
}

func execBash(ctx context.Context, projectPath string, input json.RawMessage) Result {
	var in bashInput
	if err := json.Unmarshal(input, &in); err != nil {
		return Result{Content: fmt.Sprintf("invalid input: %v", err), IsError: true}
	}

	if reason := checkBlocked(in.Command); reason != "" {
		return Result{Content: fmt.Sprintf("blocked: %s", reason), IsError: true}
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
	cmd.Env = safeEnv()

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
