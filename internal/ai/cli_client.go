package ai

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// defaultTimeout bounds a claude -p subprocess call when the caller's context
// carries no earlier deadline — otherwise a stalled subprocess (auth prompt,
// network stall, hung MCP init inside it) blocks the classify/reflect loop
// indefinitely, since resolve/supersede classify candidates one at a time.
const defaultTimeout = 5 * time.Minute

type claudeCapabilities struct {
	safeMode        bool
	restricted      bool
	strictMCP       bool
	disableSlash    bool
	tools           bool
	disallowedTools bool
	settingSources  bool
}

type claudeBinaryID struct {
	path    string
	size    int64
	modTime time.Time
}

// probeKey is this identity as the string claudeProbeGroup keys on, and it is
// built from exactly the three fields the cache key compares — no more, so two
// identities can never share a flight, and no fewer, so the same binary always
// lands on the same one. NUL joins them because a path cannot contain one, so
// no combination of values can spell another identity's key.
func (id claudeBinaryID) probeKey() string {
	return id.path + "\x00" + strconv.FormatInt(id.size, 10) + "\x00" + strconv.FormatInt(id.modTime.UnixNano(), 10)
}

var claudeCapabilityCache sync.Map // claudeBinaryID -> claudeCapabilities

// claudeProbeGroup collapses a cold burst into ONE `--help` child, keyed on the
// binary identity, so N concurrent first-callers spawn 1 process and all share
// its answer. Without it the cache is check-then-probe and every caller that
// arrives before the first one stores runs its own (#741).
//
// It is a singleflight.Group and not a per-key mutex map because of the failure
// path, which is the whole difference between the two. A mutex map serializes
// the burst: the leader probes, and each follower then takes the lock, finds the
// cache STILL EMPTY (a failed claude probe is deliberately not cached, so the
// next call retries with its own live context), and probes again — N processes
// for the same burst, which is the bug rather than a fix for it. singleflight
// shares the outcome of the concurrent call whether it succeeded or not.
//
// What it does NOT do is decide what is RETAINED. A flight is forgotten the
// moment it lands and keeps nothing of its own, so the cache above still owns
// that decision — a success is stored, a failure is not, and nothing here can
// turn an unprobed identity into a cached one.
var claudeProbeGroup singleflight.Group

func parseClaudeCapabilities(help string) claudeCapabilities {
	help = strings.ToLower(help)
	return claudeCapabilities{
		safeMode:        strings.Contains(help, "--safe-mode"),
		restricted:      strings.Contains(help, "--restricted"),
		strictMCP:       strings.Contains(help, "--strict-mcp-config"),
		disableSlash:    strings.Contains(help, "--disable-slash-commands"),
		tools:           strings.Contains(help, "--tools"),
		disallowedTools: strings.Contains(help, "--disallowedtools"),
		settingSources:  strings.Contains(help, "--setting-sources"),
	}
}

func claudeCapabilitiesFor(ctx context.Context, binary string) (claudeCapabilities, error) {
	path, err := exec.LookPath(binary)
	if err != nil {
		return claudeCapabilities{}, fmt.Errorf("claude capability probe: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return claudeCapabilities{}, fmt.Errorf("claude capability probe: %w", err)
	}
	id := claudeBinaryID{path: path, size: info.Size(), modTime: info.ModTime()}
	if cached, ok := claudeCapabilityCache.Load(id); ok {
		return cached.(claudeCapabilities), nil
	}

	// A cold miss runs ONE probe for the whole burst. The re-read inside is the
	// re-check, not a second guess: a flight that landed between the lookup above
	// and this call has already stored its answer. A caller arriving after even
	// that is served from the cache by the first check and never reaches the
	// group at all, so the warm path costs what it always did.
	outcome, err, _ := claudeProbeGroup.Do(id.probeKey(), func() (any, error) {
		if cached, ok := claudeCapabilityCache.Load(id); ok {
			return cached.(claudeCapabilities), nil
		}
		return probeClaudeCapabilities(ctx, path, id)
	})
	if err != nil {
		return claudeCapabilities{}, err
	}
	return outcome.(claudeCapabilities), nil
}

// probeClaudeCapabilities is the body of the single flight: the one child, and
// the store on the answer path ONLY. Not caching the failure is deliberate and
// predates #741 — an unanswered probe leaves the identity cold, so the next
// caller re-asks rather than inheriting this caller's error.
func probeClaudeCapabilities(ctx context.Context, path string, id claudeBinaryID) (claudeCapabilities, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	probe, release, _ := harnessCommand(probeCtx, path, []string{"--help"}, os.Environ(), harnessClaude)
	defer release()
	out, err := probe.Output()
	if err != nil {
		return claudeCapabilities{}, fmt.Errorf("claude capability probe: %w", err)
	}
	caps := parseClaudeCapabilities(string(out))
	claudeCapabilityCache.Store(id, caps)
	return caps, nil
}

// claudeInvocationArgs builds the whole `claude -p` argv, or refuses to.
//
// Refusal rather than degradation is the contract, and the capability probe is
// what makes it possible: each flag is read out of the INSTALLED binary's own
// `--help`, so a claude that lacks one cannot be run with a silently weaker
// policy than the one this function returns. A missing no-tools flag is an
// error the caller reports, not an invocation that proceeds with tools.
//
// The five required capabilities are the load-bearing ones, and each maps to a
// documented claude behaviour (verified against `claude --help` on 2.1.283):
//
//   - --tools ""          "disable all tools" — claude's own wording. This is
//     what removes the built-in set; the pattern is claude's, not a
//     list-of-names approximation that misses a tool shipped later.
//   - --disallowedTools "mcp__*"
//     denies MCP tools by prefix. Kept as a second layer rather than as the
//     primary: --strict-mcp-config already leaves no MCP server configured, so
//     there is nothing to expose, and a belt-and-braces rule keeps that true if
//     a future claude widens what --tools covers.
//   - --safe-mode        disables every customization (CLAUDE.md, skills,
//     plugins, hooks, MCP servers, custom agents). The child reads memory text,
//     so none of those should reach it in the first place.
//   - --restricted       drops the command- and code-running tools, ignores
//     user/project/local settings, and refuses bypassPermissions. It is the
//     second boundary for a tool --tools somehow still names.
//   - --strict-mcp-config  ignores every MCP configuration, so no MCP server is
//     started and therefore none can be reached.
//
// --disable-slash-commands and --setting-sources are conditional because they
// are hardening rather than the restriction itself: skills are already off under
// --safe-mode, and the child's working directory is a neutral scratch dir with
// no project to load settings from. Both are still passed when present.
func claudeInvocationArgs(caps claudeCapabilities) ([]string, error) {
	if !caps.safeMode || !caps.restricted || !caps.strictMCP || !caps.tools || !caps.disallowedTools {
		return nil, fmt.Errorf("installed claude lacks required no-tools flags; upgrade claude")
	}
	args := []string{
		"-p",
		"--safe-mode",
		"--restricted",
		"--strict-mcp-config",
		"--tools", "",
		"--disallowedTools", "mcp__*",
	}
	if caps.disableSlash {
		args = append(args, "--disable-slash-commands")
	}
	if caps.settingSources {
		args = append(args, "--setting-sources", "project,local")
	}
	return args, nil
}

// CLIClient drives Claude via the `claude` CLI (a `claude -p` subprocess).
// Authentication and billing belong to the caller's Claude CLI configuration.
// It implements the same Reflect/Classify shapes as the other CLI adapters
// (the cliBackend interface), so it serves reflect/resolve/supersede without a
// Ghost-managed API key.
//
// ANTHROPIC_API_KEY is stripped from the subprocess environment: if present,
// it would override subscription/OAuth login and bill the call as
// pay-per-token API usage instead, defeating the point of this client.
// The invocation also runs in restricted/safe mode with an empty built-in
// tool set and strict MCP configuration. This keeps an untrusted memory
// prompt from reaching shell, plugin, or MCP operations while preserving the
// caller's Claude authentication in CLAUDE_CONFIG_DIR.
type CLIClient struct {
	binary string
}

// NewCLIClient creates a CLIClient that invokes the `claude` binary on PATH.
func NewCLIClient() *CLIClient {
	return NewCLIClientWithBinary("claude")
}

// NewCLIClientWithBinary creates a CLIClient that invokes the given binary path
// (absolute, or a name resolved from PATH).
func NewCLIClientWithBinary(binary string) *CLIClient {
	return &CLIClient{binary: binary}
}

// Reflect satisfies reflection's reflector interface (see
// internal/reflection/tier_llm.go). TokenUsage is currently always zero: the
// CLI adapters do not parse provider usage metadata, and the harness owns its
// own billing.
func (c *CLIClient) Reflect(ctx context.Context, prompt string) (string, TokenUsage, error) {
	text, err := c.run(ctx, prompt)
	return text, TokenUsage{}, err
}

// Classify satisfies the Provider interface (see internal/ai/provider.go).
// systemPrompt is passed via the CLI's --system-prompt flag rather than being
// concatenated into the prompt text, so it can't be confused with user content.
func (c *CLIClient) Classify(ctx context.Context, systemPrompt, userContent string) (string, error) {
	return c.run(ctx, userContent, "--system-prompt", systemPrompt)
}

func (c *CLIClient) run(ctx context.Context, prompt string, extraArgs ...string) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
	}
	caps, err := claudeCapabilitiesFor(ctx, c.binary)
	if err != nil {
		return "", err
	}
	args, err := claudeInvocationArgs(caps)
	if err != nil {
		return "", err
	}
	args = append(args, extraArgs...)
	// The prompt goes on stdin, never as an argv element (issue #560). Linux
	// caps one argument at 32 pages — 128 KiB on a 4 KiB-page x86, 512 KiB on a
	// 16 KiB-page one — and a reflect prompt is built from up to 2000 memories
	// of 8000 bytes, so a large project produced a prompt that failed the spawn
	// outright with E2BIG. claude -p reads the prompt from stdin when no
	// positional prompt is given, which is what "-p ... useful for pipes"
	// documents. --system-prompt stays an argument: it is small and fixed, and
	// keeping it out of the piped text is what stops it being confused with
	// user content.
	cmd, release, _ := harnessCommand(ctx, c.binary, args, os.Environ(), harnessClaude)
	defer release()
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude -p: %w: %s", err, harnessFailureOutput(stdout.String(), stderr.String()))
	}
	return stdout.String(), nil
}

func stripAPIKey(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, found := strings.Cut(kv, "=")
		if found && strings.EqualFold(key, "ANTHROPIC_API_KEY") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// stripLLMKeys strips API keys for all LLM providers from the environment,
// preventing the subprocess from billing to the wrong account.
func stripLLMKeys(env []string) []string {
	out := stripAPIKey(env)
	out = stripEnvKey(out, "OPENAI_API_KEY")
	out = stripEnvKey(out, "GOOSE_PROVIDER__API_KEY")
	return out
}

func stripEnvKey(env []string, key string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, found := strings.Cut(kv, "=")
		if found && strings.EqualFold(k, key) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
