# Autonomous Reflect Implementation Plan

> **Historical record — not current documentation.** This plan describes the
> original autonomous-reflection and OpenCode V1 design. Current behavior is
> documented in [`../../README.md`](../../README.md).

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give `ghost reflect` a generalized subprocess LLM tier (claude and opencode), auto-trigger it from the Stop hook, and improve the Jaccard fallback dedup.

**Architecture:** A new `ai.OpenCodeClient` drives `opencode run --format json --pure` and parses its JSON-lines stream; a new `ai.CLIProvider` resolver picks claude → opencode by PATH and exposes the existing `reflector`/`Provider` interfaces. The reflect CLI gains `--tier opencode` and uses `CLIProvider` in `auto`. `reflection.auto_reflect` (default false) plus `spawnReflectIfConfigured` mirror resolve/supersede, with a no-LLM guard. The sqlite tier's Jaccard dedup gains stopword filtering, a containment score, and a numeric-difference guard.

**Tech Stack:** Go 1.26, standard library only (no new deps), `opencode`/`claude` CLI subprocesses, modernc.org/sqlite.

## Global Constraints

- `go vet ./...` before every commit; `go test ./...` for the test cycle.
- Zero new third-party dependencies — everything uses `os/exec` + stdlib.
- Commit messages use conventional prefixes (`feat:`, `fix:`, `docs:`); no `Co-Authored-By`; DCO signoff + GPG signing are already configured globally (do not add flags).
- Work on the `feat/autonomous-reflect` branch; never commit to `main`.
- Config keys use snake_case koanf tags (e.g. `auto_reflect`), defaults live in the `defaults` map in `internal/config/config.go`.
- Hooks never do inline DB/LLM work: `spawnReflectIfConfigured` does a small read-only lookup then forks a detached `ghost reflect --apply`, exactly like the resolve/supersede twins.

---

### Task 1: `OpenCodeClient` subprocess LLM

**Files:**
- Create: `internal/ai/opencode_client.go`
- Test: `internal/ai/opencode_client_test.go`

**Interfaces:**
- Consumes: `defaultTimeout` (const in `internal/ai/cli_client.go`), `TokenUsage` (`internal/ai/models.go`).
- Produces:
  - `func NewOpenCodeClient() *OpenCodeClient`
  - `func (c *OpenCodeClient) Reflect(ctx context.Context, prompt string) (string, TokenUsage, error)`
  - `func (c *OpenCodeClient) Classify(ctx context.Context, systemPrompt, userContent string) (string, error)`

- [ ] **Step 1: Write the failing test**

Create `internal/ai/opencode_client_test.go`:

```go
package ai

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeOpenCodeBinary writes a shell script standing in for `opencode` that
// emits the given lines to stdout, so run()'s plumbing can be verified without
// a real opencode install.
func fakeOpenCodeBinary(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binary requires a POSIX shell")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	return path
}

func TestParseOpenCodeOutput_ConcatenatesTextEvents(t *testing.T) {
	raw := `{"type":"step_start","timestamp":1,"part":{"type":"step-start"}}
{"type":"text","timestamp":2,"part":{"id":"a","type":"text","text":"{\"memories\":"}}
{"type":"text","timestamp":3,"part":{"id":"b","type":"text","text":"[]}"}}
{"type":"step_finish","timestamp":4,"part":{"id":"c","type":"step-finish","reason":"stop"}}
`
	got := parseOpenCodeOutput(raw)
	if got != `{"memories":[]}` {
		t.Errorf("got %q, want concatenated text events", got)
	}
}

func TestParseOpenCodeOutput_IgnoresNonText(t *testing.T) {
	raw := `{"type":"step_start","part":{"type":"step-start"}}
{"type":"reasoning","part":{"type":"reasoning","text":"thinking aloud"}}
{"type":"text","part":{"id":"a","type":"text","text":"OK"}}
`
	if got := parseOpenCodeOutput(raw); got != "OK" {
		t.Errorf("got %q, want only text events", got)
	}
}

func TestOpenCodeClient_Reflect_ReturnsConcatenatedText(t *testing.T) {
	bin := fakeOpenCodeBinary(t, `printf '%s\n' '{"type":"text","part":{"type":"text","text":"HELLO"}}' '{"type":"text","part":{"type":"text","text":" WORLD"}}'`)
	c := &OpenCodeClient{binary: bin}
	text, usage, err := c.Reflect(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if text != "HELLO WORLD" {
		t.Errorf("got %q, want %q", text, "HELLO WORLD")
	}
	if usage != (TokenUsage{}) {
		t.Errorf("expected zero TokenUsage, got %+v", usage)
	}
}

func TestOpenCodeClient_Run_PropagatesStderrOnFailure(t *testing.T) {
	bin := fakeOpenCodeBinary(t, `echo "boom" >&2; exit 1`)
	c := &OpenCodeClient{binary: bin}
	_, _, err := c.Reflect(context.Background(), "prompt")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("expected error to include stderr, got %v", err)
	}
}

func TestOpenCodeClient_Classify_JoinsSystemAndUserIntoPrompt(t *testing.T) {
	bin := fakeOpenCodeBinary(t, `for last; do :; done
case "$last" in *SYSTEM*USER*) printf '%s\n' '{"type":"text","part":{"type":"text","text":"KEEP"}}';; *) echo "prompt not joined" >&2; exit 1;; esac`)
	c := &OpenCodeClient{binary: bin}
	text, err := c.Classify(context.Background(), "SYSTEM", "USER")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if text != "KEEP" {
		t.Errorf("got %q, want %q", text, "KEEP")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/ai/ -run 'OpenCode' -v`
Expected: FAIL — `undefined: parseOpenCodeOutput`, `undefined: OpenCodeClient`.

- [ ] **Step 3: Write the implementation**

Create `internal/ai/opencode_client.go`:

```go
package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// OpenCodeClient drives the `opencode` CLI as a subprocess LLM, the way
// CLIClient drives `claude -p`. It bills to whatever provider opencode is
// configured with, so it works with no ANTHROPIC_API_KEY and no `claude`
// binary. It implements the same Reflect/Classify shapes as CLIClient
// (reflector / Provider), so it substitutes for the direct API client in the
// reflect tier.
type OpenCodeClient struct {
	binary string
}

// NewOpenCodeClient creates an OpenCodeClient that invokes `opencode` on PATH.
func NewOpenCodeClient() *OpenCodeClient {
	return &OpenCodeClient{binary: "opencode"}
}

// Reflect satisfies reflection's reflector interface (see
// internal/reflection/tier_haiku.go). TokenUsage is always zero: subscription
// calls have no per-token API cost to record.
func (c *OpenCodeClient) Reflect(ctx context.Context, prompt string) (string, TokenUsage, error) {
	text, err := c.run(ctx, prompt)
	return text, TokenUsage{}, err
}

// Classify satisfies the Provider interface (see internal/ai/provider.go).
// opencode run has no --system-prompt flag, so the system prompt is joined into
// the message — the same join anthropicClient.Classify performs.
func (c *OpenCodeClient) Classify(ctx context.Context, systemPrompt, userContent string) (string, error) {
	return c.run(ctx, systemPrompt + "\n\n" + userContent)
}

func (c *OpenCodeClient) run(ctx context.Context, prompt string) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
	}
	// --format json emits a JSON-lines stream; --pure skips plugins. The prompt
	// is the last argument. cmd.Dir is a neutral temp dir so the subprocess does
	// not load the repo's CLAUDE.md/AGENTS.md, project opencode.json, or git
	// context — the reflect prompt is self-contained. (The global opencode
	// config, including any configured MCP servers, may still load; see the
	// spec's Guardrails for why that is bounded here.)
	args := []string{"run", "--format", "json", "--pure", prompt}
	cmd := exec.CommandContext(ctx, c.binary, args...)
	cmd.Dir = os.TempDir()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("opencode run: %w: %s", err, stderr.String())
	}
	return parseOpenCodeOutput(stdout.String()), nil
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
// stream into the model's answer, ignoring step_start/step_finish/reasoning and
// any unparseable lines.
func parseOpenCodeOutput(raw string) string {
	var sb strings.Builder
	sc := bufio.NewScanner(strings.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var ev opencodeEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		if ev.Type == "text" && ev.Part.Type == "text" {
			sb.WriteString(ev.Part.Text)
		}
	}
	return sb.String()
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/ai/ -run 'OpenCode' -v`
Expected: PASS.

- [ ] **Step 5: Vet and commit**

Run: `go vet ./internal/ai/`
Then:
```bash
git add internal/ai/opencode_client.go internal/ai/opencode_client_test.go
git commit -m "feat(ai): add OpenCodeClient subprocess LLM (opencode run --format json)"
```

---

### Task 2: `CLIProvider` resolver

**Files:**
- Create: `internal/ai/cli_provider.go`
- Test: `internal/ai/cli_provider_test.go`

**Interfaces:**
- Consumes: `NewCLIClient()` (`internal/ai/cli_client.go`), `NewOpenCodeClient()` (Task 1).
- Produces:
  - `func NewCLIProvider() *CLIProvider`
  - `func (p *CLIProvider) Name() string`
  - `func (p *CLIProvider) Available() bool`
  - `func (p *CLIProvider) Reflect(ctx context.Context, prompt string) (string, TokenUsage, error)`
  - `func (p *CLIProvider) Classify(ctx context.Context, systemPrompt, userContent string) (string, error)`

- [ ] **Step 1: Write the failing test**

Create `internal/ai/cli_provider_test.go`:

```go
package ai

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func writeFake(t *testing.T, dir, name string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binary requires a POSIX shell")
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
}

func TestCLIProvider_PrefersClaudeOverOpencode(t *testing.T) {
	dir := t.TempDir()
	writeFake(t, dir, "claude")
	writeFake(t, dir, "opencode")
	t.Setenv("PATH", dir)

	p := NewCLIProvider()
	if p.Name() != "cli" {
		t.Errorf("expected claude preference, got %q", p.Name())
	}
}

func TestCLIProvider_FallsBackToOpencode(t *testing.T) {
	dir := t.TempDir()
	writeFake(t, dir, "opencode")
	t.Setenv("PATH", dir)

	p := NewCLIProvider()
	if !p.Available() {
		t.Fatal("expected available via opencode")
	}
	if p.Name() != "opencode" {
		t.Errorf("expected opencode fallback, got %q", p.Name())
	}
}

func TestCLIProvider_UnavailableWhenNeither(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty dir: no claude, no opencode

	p := NewCLIProvider()
	if p.Available() {
		t.Fatal("expected unavailable")
	}
	if p.Name() != "none" {
		t.Errorf("expected name %q, got %q", "none", p.Name())
	}
	if _, _, err := p.Reflect(context.Background(), "prompt"); err == nil {
		t.Fatal("expected error from Reflect when no backend")
	}
	if _, err := p.Classify(context.Background(), "sys", "user"); err == nil {
		t.Fatal("expected error from Classify when no backend")
	}
}

func TestCLIProvider_DelegatesReflect(t *testing.T) {
	dir := t.TempDir()
	writeFake(t, dir, "opencode")
	t.Setenv("PATH", dir)

	// Note: the fake opencode above exits 0 with no output, so Reflect returns
	// an empty string, not an error — this asserts delegation plumbing only.
	p := NewCLIProvider()
	if _, _, err := p.Reflect(context.Background(), "prompt"); err != nil {
		t.Fatalf("Reflect delegation: %v", err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/ai/ -run 'CLIProvider' -v`
Expected: FAIL — `undefined: NewCLIProvider`.

- [ ] **Step 3: Write the implementation**

Create `internal/ai/cli_provider.go`:

```go
package ai

import (
	"context"
	"errors"
	"os/exec"
)

// errNoCLIBackend is returned when no subprocess LLM binary is available.
var errNoCLIBackend = errors.New("no CLI LLM backend available (install claude or opencode)")

// cliBackend is the shared capability of the two subprocess LLM clients.
type cliBackend interface {
	Reflect(ctx context.Context, prompt string) (string, TokenUsage, error)
	Classify(ctx context.Context, systemPrompt, userContent string) (string, error)
}

// CLIProvider resolves the best available CLI backend — claude first, then
// opencode — and delegates to it, so callers need no knowledge of which binary
// is installed. It satisfies reflection's reflector interface and ai.Provider,
// so it drops in wherever CLIClient is used.
type CLIProvider struct {
	backend cliBackend
	name    string
}

// NewCLIProvider picks the best backend available on PATH: claude if present,
// else opencode if present, else an unavailable provider.
func NewCLIProvider() *CLIProvider {
	if _, err := exec.LookPath("claude"); err == nil {
		return &CLIProvider{backend: NewCLIClient(), name: "cli"}
	}
	if _, err := exec.LookPath("opencode"); err == nil {
		return &CLIProvider{backend: NewOpenCodeClient(), name: "opencode"}
	}
	return &CLIProvider{backend: nil, name: "none"}
}

// Name reports which backend was selected ("cli", "opencode", or "none").
func (p *CLIProvider) Name() string { return p.name }

// Available reports whether any subprocess LLM backend is on PATH.
func (p *CLIProvider) Available() bool { return p.backend != nil }

func (p *CLIProvider) Reflect(ctx context.Context, prompt string) (string, TokenUsage, error) {
	if p.backend == nil {
		return "", TokenUsage{}, errNoCLIBackend
	}
	return p.backend.Reflect(ctx, prompt)
}

func (p *CLIProvider) Classify(ctx context.Context, systemPrompt, userContent string) (string, error) {
	if p.backend == nil {
		return "", errNoCLIBackend
	}
	return p.backend.Classify(ctx, systemPrompt, userContent)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/ai/ -run 'CLIProvider' -v`
Expected: PASS.

- [ ] **Step 5: Vet and commit**

Run: `go vet ./internal/ai/`
Then:
```bash
git add internal/ai/cli_provider.go internal/ai/cli_provider_test.go
git commit -m "feat(ai): add CLIProvider resolver (claude -> opencode by PATH)"
```

---

### Task 3: Reflect tier wiring

**Files:**
- Modify: `cmd/ghost/main.go` (the `reflect` command's tier switch, ~lines 268–341)

**Interfaces:**
- Consumes: `ai.NewOpenCodeClient()` (Task 1), `ai.NewCLIProvider()` (Task 2), `reflection.NewNamedConsolidator(client reflector, name string)`, `reflection.NewSQLiteConsolidator()`, `reflection.NewTieredConsolidator`.
- Produces: `--tier opencode` selects `OpenCodeClient`; `--tier auto` uses `CLIProvider`.

- [ ] **Step 1: Update the usage string**

In `cmd/ghost/main.go`, the reflect usage block currently reads:

```go
  --tier string   Consolidation tier: auto, haiku, cli, sqlite (default "auto")
```

Change it to:

```go
  --tier string   Consolidation tier: auto, haiku, cli, opencode, sqlite (default "auto")
```

- [ ] **Step 2: Add the `opencode` case and switch `auto` to `CLIProvider`**

Replace the tier switch body (the `switch tierValue` block plus its `default: // "auto"` arm) with:

```go
	var consolidator reflection.Consolidator
	switch tierValue {
	case "haiku":
		if cfg.API.Key == "" {
			fmt.Fprintln(os.Stderr, "error: haiku tier requires ANTHROPIC_API_KEY")
			os.Exit(1)
		}
		client := ai.NewClient(cfg.API.Key, logger)
		consolidator = reflection.NewHaikuConsolidator(client)
	case "cli":
		if _, err := exec.LookPath("claude"); err != nil {
			fmt.Fprintln(os.Stderr, "error: cli tier requires the `claude` binary on PATH")
			os.Exit(1)
		}
		consolidator = reflection.NewNamedConsolidator(ai.NewCLIClient(), "cli")
	case "opencode":
		if _, err := exec.LookPath("opencode"); err != nil {
			fmt.Fprintln(os.Stderr, "error: opencode tier requires the `opencode` binary on PATH")
			os.Exit(1)
		}
		consolidator = reflection.NewNamedConsolidator(ai.NewOpenCodeClient(), "opencode")
	case "sqlite":
		consolidator = reflection.NewSQLiteConsolidator()
	default: // "auto"
		var tiers []reflection.Consolidator
		if cfg.API.Key != "" {
			client := ai.NewClient(cfg.API.Key, logger)
			tiers = append(tiers, reflection.NewHaikuConsolidator(client))
		}
		if cli := ai.NewCLIProvider(); cli.Available() {
			tiers = append(tiers, reflection.NewNamedConsolidator(cli, cli.Name()))
		}
		tiers = append(tiers, reflection.NewSQLiteConsolidator())
		consolidator = reflection.NewTieredConsolidator(tiers, logger)
	}
```

- [ ] **Step 3: Build and smoke-test**

Run: `go build ./... && go vet ./cmd/ghost/`
Expected: build succeeds, vet clean.

Then a manual dry-run against a real opencode (no write — omit `--apply`):

```bash
go run ./cmd/ghost reflect _global --tier opencode 2>&1 | head -20
```

Expected: prints `Consolidator: opencode` (or `tiered:...`) and a DRY RUN summary; it must not error with "opencode tier requires". If `opencode` is not on PATH in this shell, this step is skipped and the `auto` path is trusted from Task 2's tests.

- [ ] **Step 4: Commit**

```bash
git add cmd/ghost/main.go
git commit -m "feat(reflect): add --tier opencode and use CLIProvider in auto tier"
```

---

### Task 4: `reflection.auto_reflect` config

**Files:**
- Modify: `internal/config/config.go` (the `ReflectionConfig` struct and the `defaults` map)

**Interfaces:**
- Produces: `Config.Reflection.AutoReflect` (bool), koanf key `auto_reflect`, default `false`.

- [ ] **Step 1: Add the field**

In `internal/config/config.go`, extend `ReflectionConfig`:

```go
type ReflectionConfig struct {
	AutoResolve   bool `koanf:"auto_resolve"`
	AutoSupersede bool `koanf:"auto_supersede"`
	AutoReflect   bool `koanf:"auto_reflect"`
}
```

- [ ] **Step 2: Add the default**

In the `defaults` map, after `"reflection.auto_supersede": false,` add:

```go
	"reflection.auto_reflect":    false,
```

- [ ] **Step 3: Build and test**

Run: `go build ./... && go test ./internal/config/`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/config/config.go
git commit -m "feat(config): add reflection.auto_reflect (default false)"
```

---

### Task 5: `spawnReflectIfConfigured` + Stop hook + no-LLM guard

**Files:**
- Modify: `internal/mcpinit/stophook.go` (add `spawnReflectIfConfigured`, call it in `HandleStopHook`, add `ai` import)
- Test: `internal/mcpinit/stophook_test.go` (add two tests)

**Interfaces:**
- Consumes: `config.Load()`, `config.DataDir()`, `roDSN`, `memory.NewStore`, `isAlive`, `claimPidFile`, `atomicWritePID`, `processStartTime`, `detachProcess` (all already in `internal/mcpinit`), and `ai.NewCLIProvider()` (Task 2).
- Produces: `func spawnReflectIfConfigured(cwd string)`.

- [ ] **Step 1: Add the call site in `HandleStopHook`**

In `internal/mcpinit/stophook.go`, immediately after the two existing spawn calls:

```go
	spawnResolveIfConfigured(input.CWD)
	spawnSupersedeIfConfigured(input.CWD)
```

add:

```go
	spawnReflectIfConfigured(input.CWD)
```

- [ ] **Step 2: Add the `ai` import**

In the same file's import block, add `"github.com/wcatz/ghost/internal/ai"` (alphabetized between the existing `config` and `memory` imports).

- [ ] **Step 3: Write the failing tests**

Append to `internal/mcpinit/stophook_test.go`:

```go
func TestSpawnReflectIfConfigured_NoOpWhenDisabled(t *testing.T) {
	// With no config file present, reflection.auto_reflect defaults to false
	// (internal/config/config.go's defaults map). spawnReflectIfConfigured must
	// return immediately after that check — before ever calling config.DataDir
	// (which creates ~/.local/share/ghost), let alone touching ghost.db, the
	// pidfile, or reflect.log.
	dataHome := isolatedHome(t)

	spawnReflectIfConfigured("/tmp/does-not-matter")

	if _, err := os.Stat(filepath.Join(dataHome, "ghost")); !os.IsNotExist(err) {
		t.Errorf("expected ghost data dir to never be created when auto_reflect is disabled, stat err = %v", err)
	}
}

func TestSpawnReflectIfConfigured_NoOpWithoutLLM(t *testing.T) {
	// auto_reflect enabled, but no LLM is available (no API key, no claude, no
	// opencode on PATH). The no-LLM guard must return before config.DataDir, so
	// no Jaccard-only reflect ever spawns and no data dir is created.
	dataHome := isolatedHome(t)
	cfgDir := os.Getenv("XDG_CONFIG_HOME")
	if err := os.MkdirAll(filepath.Join(cfgDir, "ghost"), 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "ghost", "config.yaml"), []byte("reflection:\n  auto_reflect: true\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("PATH", t.TempDir()) // no claude, no opencode

	spawnReflectIfConfigured("/tmp/does-not-matter")

	if _, err := os.Stat(filepath.Join(dataHome, "ghost")); !os.IsNotExist(err) {
		t.Errorf("expected no ghost data dir when no LLM is available, stat err = %v", err)
	}
}
```

- [ ] **Step 4: Run the tests to verify they fail**

Run: `go test ./internal/mcpinit/ -run 'SpawnReflect' -v`
Expected: FAIL — `undefined: spawnReflectIfConfigured`.

- [ ] **Step 5: Write the implementation**

Append to `internal/mcpinit/stophook.go`:

```go
// spawnReflectIfConfigured starts `ghost reflect <project> --apply` as a
// detached background process for the project matching cwd, if one isn't
// already running for that project. Opt-in via reflection.auto_reflect
// (default false). Every failure path returns silently: this must never block
// or fail the stop hook.
//
// Unlike the resolve/supersede twins, this adds a no-LLM guard: consolidation
// is only worth an unattended write when a real LLM tier is available. Without
// an API key or a claude/opencode binary, --tier auto would fall through to the
// Jaccard-only sqlite tier and rewrite every non-manual memory for no quality
// gain, so the spawn is skipped entirely — before the DB is even opened, so
// this stays a cheap read-only no-op.
func spawnReflectIfConfigured(cwd string) {
	if cwd == "" {
		return
	}
	cfg, err := config.Load()
	if err != nil || !cfg.Reflection.AutoReflect {
		return
	}
	if cfg.API.Key == "" && !ai.NewCLIProvider().Available() {
		return
	}

	dataDir, err := config.DataDir()
	if err != nil {
		return
	}
	dbPath := filepath.Join(dataDir, "ghost.db")
	if _, err := os.Stat(dbPath); err != nil {
		return
	}
	db, err := sql.Open("sqlite", roDSN(dbPath))
	if err != nil {
		return
	}
	defer db.Close() //nolint:errcheck

	store := memory.NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	projectID, projectName, err := store.ResolveProject(context.Background(), cwd)
	if err != nil || projectID == "" || projectName == "" {
		return
	}

	pidPath := filepath.Join(dataDir, "reflect-"+projectID+".pid")
	if isAlive(pidPath) {
		return
	}
	if !claimPidFile(pidPath) {
		return
	}

	exe, err := os.Executable()
	if err != nil {
		return
	}
	logFile, err := os.OpenFile(filepath.Join(dataDir, "reflect.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer logFile.Close() //nolint:errcheck

	cmd := exec.Command(exe, "reflect", projectName, "--apply")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		return
	}
	token, haveToken := processStartTime(cmd.Process.Pid)
	_ = atomicWritePID(pidPath, cmd.Process.Pid, token, haveToken)
	_ = cmd.Process.Release()
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/mcpinit/ -run 'SpawnReflect|SpawnResolve|SpawnSupersede' -v`
Expected: PASS (all spawn tests).

- [ ] **Step 7: Vet and commit**

Run: `go vet ./internal/mcpinit/`
Then:
```bash
git add internal/mcpinit/stophook.go internal/mcpinit/stophook_test.go
git commit -m "feat(mcpinit): auto-reflect on Stop hook with no-LLM guard"
```

---

### Task 6: Jaccard improvement (sqlite tier)

**Files:**
- Modify: `internal/reflection/tier_sqlite.go` (tokenize, merge loop; add `containment`, `numericConflict`, stopwords)
- Test: `internal/reflection/consolidator_test.go` (add tests)

**Interfaces:**
- Produces (internal to the package): `func containment(a, b map[string]bool) float64`, `func numericConflict(a, b map[string]bool) bool`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/reflection/consolidator_test.go`:

```go
func TestSQLiteConsolidator_SubsetMergesViaContainment(t *testing.T) {
	sc := NewSQLiteConsolidator()
	input := ReflectionInput{
		ExistingMemories: []memory.Memory{
			{Category: "fact", Content: "use sqlite", Importance: 0.6},
			{Category: "fact", Content: "use sqlite for storage with fts5 search", Importance: 0.8},
		},
	}
	result, err := sc.Consolidate(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Memories) != 1 {
		t.Fatalf("expected subset to merge into superset, got %d memories", len(result.Memories))
	}
	if result.Memories[0].Importance != 0.8 {
		t.Errorf("merged memory should keep higher importance 0.8, got %v", result.Memories[0].Importance)
	}
}

func TestSQLiteConsolidator_NumericDifferenceBlocksMerge(t *testing.T) {
	sc := NewSQLiteConsolidator()
	input := ReflectionInput{
		ExistingMemories: []memory.Memory{
			{Category: "fact", Content: "k3s runs grafana on port 80", Importance: 0.7},
			{Category: "fact", Content: "k3s runs grafana on port 81", Importance: 0.7},
		},
	}
	result, err := sc.Consolidate(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Memories) != 2 {
		t.Fatalf("expected numeric difference to block merge, got %d memories", len(result.Memories))
	}
}

func TestTokenize_ExcludesStopwords(t *testing.T) {
	tokens := tokenize("ghost uses sqlite for the storage with fts5")
	if tokens["the"] || tokens["for"] || tokens["with"] {
		t.Errorf("stopwords should be excluded, got %v", tokens)
	}
	if !tokens["ghost"] || !tokens["sqlite"] || !tokens["storage"] {
		t.Errorf("content words should be kept, got %v", tokens)
	}
}

func TestContainment(t *testing.T) {
	a := map[string]bool{"go": true, "sqlite": true}
	b := map[string]bool{"go": true, "sqlite": true, "fts5": true, "storage": true}
	if got := containment(a, b); got != 1.0 {
		t.Errorf("containment(subset, superset) = %v, want 1.0", got)
	}
	c := map[string]bool{"go": true}
	d := map[string]bool{"sqlite": true}
	if got := containment(c, d); got != 0.0 {
		t.Errorf("containment(disjoint) = %v, want 0.0", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/reflection/ -run 'SubsetMerges|NumericDifference|ExcludesStopwords|TestContainment' -v`
Expected: FAIL (subset test asserts 1 memory but gets 2; numeric test asserts 2 but gets 1; `containment`/`numericConflict` undefined).

- [ ] **Step 3: Write the implementation**

In `internal/reflection/tier_sqlite.go`, make three changes.

(3a) Add a stopword set and filter in `tokenize`:

```go
// stopwords are filler words that carry no consolidation signal; dropping them
// keeps Jaccard/containment from being diluted on longer memories.
var stopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "by": true, "for": true, "from": true, "in": true, "is": true,
	"it": true, "of": true, "on": true, "or": true, "that": true, "the": true,
	"this": true, "to": true, "with": true,
}

// tokenize splits text into a set of lowercase word tokens (length > 1),
// excluding stopwords.
func tokenize(s string) map[string]bool {
	tokens := make(map[string]bool)
	for _, word := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len(word) > 1 && !stopwords[word] {
			tokens[word] = true
		}
	}
	return tokens
}
```

(3b) Add `containment` and `numericConflict` next to `jaccard`:

```go
// containment is the overlap coefficient |A∩B| / min(|A|,|B|): it catches a
// memory that is a strict subset of another ("use sqlite" inside "use sqlite
// for storage"), which symmetric Jaccard scores too low to merge.
func containment(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	intersection := 0
	for token := range a {
		if b[token] {
			intersection++
		}
	}
	denom := len(a)
	if len(b) < denom {
		denom = len(b)
	}
	return float64(intersection) / float64(denom)
}

// numericConflict reports whether the two token sets differ on a purely-numeric
// token — a precise fact ("port 80" vs "port 81") that must not be merged even
// when the surrounding words overlap heavily.
func numericConflict(a, b map[string]bool) bool {
	for token := range a {
		if isNumericToken(token) && !b[token] {
			return true
		}
	}
	for token := range b {
		if isNumericToken(token) && !a[token] {
			return true
		}
	}
	return false
}

func isNumericToken(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return len(s) > 0
}
```

(3c) Update the merge condition in `Consolidate`. Replace the line:

```go
			sim := jaccard(items[i].tokens, items[j].tokens)
			if sim >= 0.5 {
```

with:

```go
			sim := jaccard(items[i].tokens, items[j].tokens)
			if c := containment(items[i].tokens, items[j].tokens); c > sim {
				sim = c
			}
			if sim >= 0.5 && !numericConflict(items[i].tokens, items[j].tokens) {
```

- [ ] **Step 4: Run the full reflection test suite**

Run: `go test ./internal/reflection/ -v`
Expected: PASS — the new tests pass and the pre-existing `TestSQLiteConsolidator_MergesDuplicates`, `TestJaccard`, `TestTokenize`, `TestInferGlobalScope`, and tiered-consolidator tests still pass unchanged.

- [ ] **Step 5: Vet and commit**

Run: `go vet ./internal/reflection/`
Then:
```bash
git add internal/reflection/tier_sqlite.go internal/reflection/consolidator_test.go
git commit -m "feat(reflection): improve sqlite dedup with stopwords, containment, numeric guard"
```

---

## Final verification

After all tasks, run the full suite and vet:

```bash
go vet ./...
go test ./...
```

Both must pass. Then open a PR from `feat/autonomous-reflect` against `main` (via the `waltskinner` fork, as established for this repo).
