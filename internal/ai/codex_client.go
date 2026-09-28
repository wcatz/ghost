package ai

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
)

type CodexClient struct {
	binary string
}

func NewCodexClient() *CodexClient {
	return NewCodexClientWithBinary("codex")
}

func NewCodexClientWithBinary(binary string) *CodexClient {
	return &CodexClient{binary: binary}
}

func (c *CodexClient) Reflect(ctx context.Context, prompt string) (string, TokenUsage, error) {
	text, err := c.run(ctx, prompt)
	return text, TokenUsage{}, err
}

func (c *CodexClient) Classify(ctx context.Context, systemPrompt, userContent string) (string, error) {
	return c.run(ctx, systemPrompt+"\n\n"+userContent)
}

func (c *CodexClient) run(ctx context.Context, prompt string) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
	}
	cmd, release, _ := harnessCommand(ctx, c.binary, codexInvocationArgs(), os.Environ(), harnessCodex)
	defer release()
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("codex exec: %w: %s", err, harnessFailureOutput(stdout.String(), stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// codexInvocationArgs is the whole argv of a `codex exec` turn, kept as a named
// value so the no-tool policy can be pinned as a golden rather than grepped for.
//
// Every `-c features.<key>` below is a key codex's own feature registry
// declares (features/src/lib.rs, matched by `key:`), and every one of them is
// on by default, so omitting one re-opens a surface. A key codex does not
// recognise is not silently ignored either: `-c` is a config override and an
// unknown key is a config error, so a rename upstream fails the call loudly
// rather than quietly restoring a tool.
//
// codex has no "no tools" flag — `--sandbox read-only` bounds what a tool may
// DO, not which tools EXIST — so the policy is the list. The three that matter
// most:
//
//   - Both exec tools. codex registers its shell surface in one place gated on
//     features.shell_tool, and inside that it picks the unified PTY-backed exec
//     tool (features.unified_exec) or the one-shot exec, so disabling only
//     features.shell_tool still leaves the other available.
//   - view_image, a plain local file read, which the sandbox's write policy
//     does not cover at all.
//   - web_search, which is the top-level `web_search` setting rather than a
//     feature key, and "disabled" is its documented off value.
//
// The plugin and connector surfaces go as a group because each contributes a
// tool of its own: plugins (local plugin skills and tools), tool_suggest (offers
// to install one mid-turn) and apps (connectors), with
// skill_mcp_dependency_install closing the last path in — installing an MCP
// server on demand runs a command. agents.enabled and features.multi_agent are
// both spelled out though one subsumes the other in current codex: they are
// separate config keys, and only spelling both means a codex that drops one does
// not silently re-enable sub-agent spawning.
//
// RESIDUAL, named rather than papered over: codex has NO flag or config key that
// disables apply_patch. Its registry entry is gated on the model catalog
// advertising an apply_patch tool type, not on an invocation-settable key, and
// the apply_patch_freeform feature is marked Removed. The read-only sandbox is
// therefore the boundary for it, not a missing flag: a model that tries to write
// is refused by the sandbox rather than never offered the tool. The prompt is
// memory text, so the exposure is a refused write, not a written file. For the
// same reason no config key reaches the skills extension's own read/list tools;
// they are off unless a skill exists, and skills are discovered from the
// invocation's neutral scratch working directory, which holds none.
//
// `-` is codex's documented sentinel for "the prompt is on stdin" (its default
// when the prompt argument is omitted, stated explicitly here so the intent
// survives a flag default change). The prompt is never an argv element (issue
// #560): the kernel caps one argument at 32 pages and a reflect prompt is built
// from up to 2000 memories of 8000 bytes, so a large project produced a prompt
// that failed the spawn with E2BIG.
func codexInvocationArgs() []string {
	return []string{
		"exec",
		"--sandbox", "read-only",
		"--ignore-user-config",
		"--ignore-rules",
		"--skip-git-repo-check",
		"--ephemeral",
		"-c", "features.shell_tool=false",
		"-c", "features.unified_exec=false",
		"-c", "features.view_image=false",
		"-c", "features.apps=false",
		"-c", "features.plugins=false",
		"-c", "features.tool_suggest=false",
		"-c", "features.skill_mcp_dependency_install=false",
		"-c", "features.remote_plugin=false",
		"-c", "features.hooks=false",
		"-c", "features.multi_agent=false",
		"-c", "agents.enabled=false",
		"-c", `web_search="disabled"`,
		"-",
	}
}
