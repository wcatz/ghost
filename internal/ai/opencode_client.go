package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// OpenCodeClient drives the `opencode` CLI as a subprocess LLM, the way
// CLIClient drives `claude -p`. It uses whatever authentication and billing
// opencode is configured with, so Ghost does not need an ANTHROPIC_API_KEY or
// a `claude` binary. It implements the same Reflect/Classify shapes as
// CLIClient (reflector / Provider) and is the CLI-backed LLM tier for the
// current architecture; there is no direct API tier.
type OpenCodeClient struct {
	binary string
	// model pins the model via -m. Constructor-set, it wins over the
	// GHOST_OPENCODE_MODEL env var: a long-lived MCP server must apply its
	// configured pin per-tool without mutating the process environment (which
	// would leak across tools and concurrent calls). Empty falls back to env,
	// then to Ghost's explicit default below.
	model string
}

// DefaultOpenCodeModel is used when neither a constructor pin nor
// GHOST_OPENCODE_MODEL is set. The child runs with a scrubbed config directory,
// so relying on the user's OpenCode default would make behavior depend on a
// config Ghost intentionally does not load. Big Pickle is the validated
// low-cost classifier model and keeps unpinned calls reproducible.
const DefaultOpenCodeModel = "opencode/big-pickle"

// NewOpenCodeClient creates an OpenCodeClient that invokes `opencode` on PATH.
func NewOpenCodeClient() *OpenCodeClient {
	return NewOpenCodeClientWithBinary("opencode")
}

// NewOpenCodeClientWithBinary creates an OpenCodeClient that invokes the given
// binary path (absolute, or a name resolved from PATH).
func NewOpenCodeClientWithBinary(binary string) *OpenCodeClient {
	return &OpenCodeClient{binary: binary}
}

// NewOpenCodeClientWithBinaryAndModel is NewOpenCodeClientWithBinary plus a
// constructor-level model pin (e.g. "opencode/big-pickle"). When model is
// non-empty it takes precedence over GHOST_OPENCODE_MODEL for this client only.
func NewOpenCodeClientWithBinaryAndModel(binary, model string) *OpenCodeClient {
	return &OpenCodeClient{binary: binary, model: model}
}

// Reflect satisfies reflection's reflector interface (see
// internal/reflection/tier_llm.go). TokenUsage is currently always zero: the
// CLI adapters do not parse provider usage metadata, and the harness owns its
// own billing rather than Ghost's token accounting.
func (c *OpenCodeClient) Reflect(ctx context.Context, prompt string) (string, TokenUsage, error) {
	text, err := c.run(ctx, prompt)
	return text, TokenUsage{}, err
}

// Classify satisfies the Provider interface (see internal/ai/provider.go).
// opencode run has no --system-prompt flag, so the system prompt is joined into
// the user message before the subprocess call.
func (c *OpenCodeClient) Classify(ctx context.Context, systemPrompt, userContent string) (string, error) {
	return c.run(ctx, systemPrompt+"\n\n"+userContent)
}

func (c *OpenCodeClient) run(ctx context.Context, prompt string) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
	}
	// --format json emits a JSON-lines stream; --pure skips plugins. The prompt
	// is the last argument. A configured model is passed with `-m`; when none is
	// configured, Ghost passes its explicit Big Pickle default because the
	// child's config dir is scrubbed below. subprocessEnv (below) confines the
	// child to a Ghost-owned scratch dir, which doubles as the neutral working
	// directory (so the subprocess does not load the repo's CLAUDE.md/AGENTS.md,
	// project opencode.json, or git context — the reflect prompt is
	// self-contained) and as a fresh empty XDG_CONFIG_HOME (so the child does not
	// load the user's global opencode config, which would start Ghost's own MCP
	// server against this process's SQLite DB), and strips ANTHROPIC_API_KEY.
	//
	// opencode V2 dropped --pure (it rejects the flag outright) and, without
	// --standalone, attaches `run` to the user's shared background service —
	// which loads the user's real config, Ghost's plugin and MCP server
	// included, defeating the scrub above. V2 therefore gets --standalone,
	// so the child runs a private server on the scrubbed config instead.
	args := []string{"run", "--format", "json", "--pure", "--title", "[ghost]"}
	if c.majorVersion(ctx) >= 2 {
		args = []string{"run", "--format", "json", "--standalone", "--title", "[ghost]"}
	}
	model := c.model
	if model == "" {
		model = os.Getenv("GHOST_OPENCODE_MODEL")
	}
	if model == "" {
		model = DefaultOpenCodeModel
	}
	args = append(args, "-m", model)
	args = append(args, prompt)
	cmd, cleanup, err := c.subprocessEnv(ctx, args)
	if err != nil {
		return "", err
	}
	defer cleanup()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("opencode run: %w: %s", err, stderr.String())
	}
	return parseOpenCodeOutput(stdout.String())
}

// opencodeMajors caches each opencode binary's major version: Ghost spawns one
// harness process per consolidation, resolve candidate, and supersede pair, so
// probing `--version` every time would double the process count. Entries are
// keyed by the resolved binary's identity (path, size, mtime), so a
// long-lived Ghost process — the MCP server — re-probes after opencode is
// upgraded in place instead of reusing a stale flag set.
var opencodeMajors sync.Map // opencodeBinaryID -> int

type opencodeBinaryID struct {
	path    string
	size    int64
	modTime time.Time
}

// majorVersion returns the major version of c.binary via `--version`, cached
// per binary identity. An unresolvable binary or failed probe is not cached
// and reports 0, which keeps the V1 invocation — the behavior Ghost had before
// V2 existed.
func (c *OpenCodeClient) majorVersion(ctx context.Context) int {
	path, err := exec.LookPath(c.binary)
	if err != nil {
		return 0
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	id := opencodeBinaryID{path: path, size: info.Size(), modTime: info.ModTime()}
	if v, ok := opencodeMajors.Load(id); ok {
		return v.(int)
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	probe, release, _ := harnessCommand(pctx, path, []string{"--version"}, os.Environ(), harnessOpencode)
	defer release()
	out, err := probe.Output()
	if err != nil {
		return 0
	}
	major := OpencodeMajorVersion(string(out))
	opencodeMajors.Store(id, major)
	return major
}

// OpencodeMajorVersion extracts the major version from `opencode --version`
// output ("1.18.32" on V1, "opencode v2.0.14" on V2). Unparseable output
// returns 0, which callers treat as V1.
func OpencodeMajorVersion(out string) int {
	for _, field := range strings.Fields(out) {
		field = strings.TrimPrefix(field, "v")
		major, _, ok := strings.Cut(field, ".")
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(major); err == nil {
			return n
		}
	}
	return 0
}

// openCodeNoToolsConfig is written into an invocation-owned config tree. The
// wildcard permission denies every tool, while the explicit tool map keeps
// older OpenCode versions from exposing a built-in tool that predates the
// wildcard rule. An empty MCP map and an empty plugin list make the isolation
// intent visible in the child config as well as in the command line.
const openCodeNoToolsConfig = `{
  "$schema": "https://opencode.ai/config.json",
  "permission": {
    "*": "deny",
    "mcp_*": "deny"
  },
  "tools": {
    "bash": false,
    "edit": false,
    "write": false,
    "read": false,
    "grep": false,
    "glob": false,
    "list": false,
    "patch": false,
    "webfetch": false,
    "task": false,
    "todowrite": false,
    "todoread": false
  },
  "mcp": {},
  "plugin": []
}`

// subprocessEnv builds the opencode child command and environment. It routes
// through harnessCommand, so the child's working directory, temp-dir
// variables, and backend-specific environment all follow the same policy as
// the other three clients. It then replaces the user's home/config roots with
// an invocation-owned tree and writes a no-tools/no-MCP config. This is
// deliberately stronger than changing XDG_CONFIG_HOME alone: OpenCode versions
// differ in which home/config variable they consult, and a user-level MCP
// server must not be able to start merely because one version ignores the
// XDG override.
//
// Pinning the temp-dir variables matters beyond tidiness: opencode writes a
// hidden JIT-cache shared object (~4.7 MiB) into its temp dir on every
// invocation, and Ghost spawns one process per consolidation, per resolve
// candidate, and per supersede candidate PAIR — hundreds per lifecycle. Left
// in the shared temp dir those accumulate indefinitely (a single dingo
// lifecycle measured ~1.4 GB) until the filesystem fills, at which point every
// LLM-backed phase fails and maintenance silently becomes a no-op. The scratch
// dir is removed by the returned cleanup, so the cache dies with it.
//
// When the scratch root is unusable, harnessCommand has already logged a WARN;
// this falls back to a private MkdirTemp tree under the inherited temp dir,
// while retaining the same allowlisted environment and no-tools config.
func (c *OpenCodeClient) subprocessEnv(ctx context.Context, args []string) (*exec.Cmd, func(), error) {
	cmd, release, ok := harnessCommand(ctx, c.binary, args, os.Environ(), harnessOpencode)
	if ok {
		if err := configureOpenCodeIsolation(cmd); err != nil {
			release()
			return nil, nil, err
		}
		return cmd, release, nil
	}

	dir, err := os.MkdirTemp("", "ghost-opencode-")
	if err != nil {
		release()
		return nil, nil, err
	}
	cmd.Dir = dir
	env := make([]string, 0, len(cmd.Env)+len(tempDirKeys))
	for _, kv := range cmd.Env {
		if isTempDirKey(kv) {
			continue // replaced below so the child's cache lives in the private dir
		}
		env = append(env, kv)
	}
	for _, key := range tempDirKeys {
		env = append(env, key+"="+dir)
	}
	cmd.Env = env
	if err := configureOpenCodeIsolation(cmd); err != nil {
		_ = os.RemoveAll(dir)
		release()
		return nil, nil, err
	}
	return cmd, func() { _ = os.RemoveAll(dir) }, nil
}

// configureOpenCodeIsolation gives the child a private home and config tree,
// writes the no-tools policy into it, and replaces inherited duplicates with
// the exact values the child should see. It intentionally does not copy the
// user's OpenCode config or auth files into the tree; OPENCODE_API_KEY is the
// supported authentication path for this isolated invocation.
func configureOpenCodeIsolation(cmd *exec.Cmd) error {
	if cmd.Dir == "" {
		return fmt.Errorf("opencode scratch directory is empty")
	}
	home := filepath.Join(cmd.Dir, "home")
	configDir := filepath.Join(cmd.Dir, "opencode-config")
	dataDir := filepath.Join(cmd.Dir, "opencode-data")
	cacheDir := filepath.Join(cmd.Dir, "opencode-cache")
	stateDir := filepath.Join(cmd.Dir, "opencode-state")
	runtimeDir := filepath.Join(cmd.Dir, "opencode-runtime")
	for _, dir := range []string{home, configDir, dataDir, cacheDir, stateDir, runtimeDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("opencode isolated dir %s: %w", dir, err)
		}
	}
	configPath := filepath.Join(configDir, "opencode.json")
	if err := os.WriteFile(configPath, []byte(openCodeNoToolsConfig), 0o600); err != nil {
		return fmt.Errorf("opencode isolated config: %w", err)
	}

	env := cmd.Env
	for key, value := range map[string]string{
		"HOME":                            home,
		"USERPROFILE":                     home,
		"XDG_CONFIG_HOME":                 configDir,
		"XDG_DATA_HOME":                   dataDir,
		"XDG_CACHE_HOME":                  cacheDir,
		"XDG_STATE_HOME":                  stateDir,
		"XDG_RUNTIME_DIR":                 runtimeDir,
		"OPENCODE_CONFIG_DIR":             configDir,
		"OPENCODE_CONFIG":                 configPath,
		"OPENCODE_CONFIG_CONTENT":         openCodeNoToolsConfig,
		"OPENCODE_DISABLE_PROJECT_CONFIG": "1",
	} {
		env = setHarnessEnvValue(env, key, value)
	}
	cmd.Env = env
	return nil
}

func setHarnessEnvValue(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		name, _, ok := strings.Cut(kv, "=")
		if ok && strings.EqualFold(name, key) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, key+"="+value)
}

// tempDirKeys are the variables that steer a child's temporary files — and so
// opencode's per-invocation JIT cache — on each platform.
var tempDirKeys = []string{"TMPDIR", "TMP", "TEMP"}

// isTempDirKey reports whether kv assigns one of tempDirKeys. The comparison is
// case-insensitive to match Windows, where the same variable may appear as
// TEMP or Temp.
func isTempDirKey(kv string) bool {
	key, _, found := strings.Cut(kv, "=")
	if !found {
		return false
	}
	for _, candidate := range tempDirKeys {
		if strings.EqualFold(key, candidate) {
			return true
		}
	}
	return false
}

// opencodeEvent is the minimal shape of one JSON line in `opencode run --format
// json` output. Everything else in the line is ignored.
type opencodeEvent struct {
	Type string `json:"type"`
	Part struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"part"`
}

// parseOpenCodeOutput concatenates the text events from an opencode JSON-lines
// stream into the model's answer, ignoring step_start/step_finish/reasoning. A
// line that isn't valid JSON, or a line exceeding the scanner buffer, is an
// error rather than a silent skip — a malformed stream means the answer is
// incomplete, and treating it as a clean reflection output would be wrong.
// Blank lines are skipped defensively (opencode may emit an empty line
// mid-stream without it indicating truncation).
func parseOpenCodeOutput(raw string) (string, error) {
	var sb strings.Builder
	sc := bufio.NewScanner(strings.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if len(strings.TrimSpace(string(sc.Bytes()))) == 0 {
			continue
		}
		var ev opencodeEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return "", fmt.Errorf("opencode run: unparseable output line: %w", err)
		}
		if ev.Type == "text" && ev.Part.Type == "text" {
			sb.WriteString(ev.Part.Text)
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("opencode run: read output: %w", err)
	}
	return sb.String(), nil
}
