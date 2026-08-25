# Source-Aware LLM Provider Selection

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop using Anthropic API as the default LLM for reflect/resolve/supersede. Instead, use the `source` field from the contract envelope to select the same CLI the user was paying for in their session.

**Architecture:** The stop hook already knows which host disconnected (via `--source`). We pass that source through to reflect/supersede/resolve as a `--source` flag, which selects the matching CLI subprocess (claude → `claude -p`, opencode → `opencode run`, codex → codex CLI, goose → goose CLI). Anthropic API becomes opt-in only (user explicitly sets `ANTHROPIC_API_KEY`).

**Tech Stack:** Go 1.26+, existing ai package interfaces, hostevent.Source constants

---

## File Structure

| File | Purpose |
|------|---------|
| `internal/ai/source_provider.go` | NEW: Maps source string → CLI backend (claude-code→claude, opencode→opencode, etc.) |
| `internal/ai/source_provider_test.go` | NEW: Tests for source-based routing |
| `internal/mcpinit/stophook.go` | Pass `--source` to reflect/supersede/resolve spawns |
| `cmd/ghost/main.go` | Add `--source` flag; add `buildClassifyProviderForSource()`; update reflect tier |

---

## Task 1: Create SourceProvider — source-to-CLI mapping

**Files:**
- Create: `internal/ai/source_provider.go`
- Create: `internal/ai/source_provider_test.go`

- [ ] **Step 1: Create SourceProvider**

New file `internal/ai/source_provider.go`. Maps source string to the matching CLI subprocess. Implements both `Provider` (Classify) and the reflection `reflector` interface (Reflect).

```go
package ai

import (
	"context"
	"errors"
	"os/exec"
)

var errUnknownSource = errors.New("unknown source: no CLI backend available for this host")

type SourceProvider struct {
	backend cliBackend
	name    string
}

// NewSourceProviderForSource returns a provider matching the host source.
// Empty/unknown source falls back to best-available-on-PATH.
func NewSourceProviderForSource(source string, cfgBinaries ...string) *SourceProvider {
	claudeBin, opencodeBin := "", ""
	if len(cfgBinaries) > 0 {
		claudeBin = cfgBinaries[0]
	}
	if len(cfgBinaries) > 1 {
		opencodeBin = cfgBinaries[1]
	}
	switch source {
	case "claude-code":
		return resolveCLI(claudeBin, "claude", "cli")
	case "opencode":
		return resolveCLI(opencodeBin, "opencode", "opencode")
	case "codex":
		// Codex doesn't have a Reflect/Classify-compatible CLI yet.
		// Fall back to best available (same as unknown source).
		return fallbackCLI(claudeBin, opencodeBin)
	case "goose":
		// Goose doesn't have a Reflect/Classify-compatible CLI yet.
		return fallbackCLI(claudeBin, opencodeBin)
	default:
		return fallbackCLI(claudeBin, opencodeBin)
	}
}

func resolveCLI(configured, defaultName, providerName string) *SourceProvider {
	bin := configured
	if bin == "" {
		bin = defaultName
	}
	if _, err := exec.LookPath(bin); err != nil {
		return &SourceProvider{backend: nil, name: "none"}
	}
	switch providerName {
	case "cli":
		return &SourceProvider{backend: NewCLIClientWithBinary(bin), name: "cli"}
	case "opencode":
		return &SourceProvider{backend: NewOpenCodeClientWithBinary(bin), name: "opencode"}
	}
	return &SourceProvider{backend: nil, name: "none"}
}

func fallbackCLI(claudeBin, opencodeBin string) *SourceProvider {
	p := NewCLIProviderWithBinaries(claudeBin, opencodeBin)
	return &SourceProvider{backend: p.backend, name: p.name}
}

func (p *SourceProvider) Name() string    { return p.name }
func (p *SourceProvider) Available() bool { return p.backend != nil }

func (p *SourceProvider) Reflect(ctx context.Context, prompt string) (string, TokenUsage, error) {
	if p.backend == nil {
		return "", TokenUsage{}, errUnknownSource
	}
	return p.backend.Reflect(ctx, prompt)
}

func (p *SourceProvider) Classify(ctx context.Context, systemPrompt, userContent string) (string, error) {
	if p.backend == nil {
		return "", errUnknownSource
	}
	return p.backend.Classify(ctx, systemPrompt, userContent)
}
```

- [ ] **Step 2: Write tests**

New file `internal/ai/source_provider_test.go`:

```go
package ai

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSourceProviderForSource(t *testing.T) {
	dir := t.TempDir()
	claude := filepath.Join(dir, "claude")
	opencode := filepath.Join(dir, "opencode")
	os.WriteFile(claude, []byte("#!/bin/sh\nexit 0"), 0o755)
	os.WriteFile(opencode, []byte("#!/bin/sh\nexit 0"), 0o755)

	tests := []struct {
		source string
		name   string
		ok     bool
	}{
		{"claude-code", "cli", true},
		{"opencode", "opencode", true},
		{"codex", "", false},     // no codex CLI impl yet
		{"goose", "", false},     // no goose CLI impl yet
		{"unknown-source", "", false}, // falls back, nothing on PATH
	}
	for _, tt := range tests {
		t.Run(tt.source, func(t *testing.T) {
			p := NewSourceProviderForSource(tt.source, claude, opencode)
			if tt.name == "" && !tt.ok {
				// For unavailable providers, just check Available
				if p.Available() {
					t.Error("expected unavailable")
				}
				return
			}
			if p.Name() != tt.name {
				t.Errorf("Name() = %q, want %q", p.Name(), tt.name)
			}
			if !p.Available() {
				t.Error("expected available")
			}
		})
	}
}

func TestSourceProviderClassify(t *testing.T) {
	p := &SourceProvider{backend: nil, name: "none"}
	_, err := p.Classify(t.Context(), "sys", "user")
	if err != errUnknownSource {
		t.Errorf("expected errUnknownSource, got %v", err)
	}
}
```

- [ ] **Step 3: Run tests**

Run: `go test ./internal/ai/ -run TestSourceProvider -v`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/ai/source_provider.go internal/ai/source_provider_test.go
git commit -s -m "feat(ai): add SourceProvider for source-based CLI routing"
```

---

## Task 2: Pass source from stop hook to spawned processes

**Files:**
- Modify: `internal/mcpinit/stophook.go`

- [ ] **Step 1: Update runStop to pass source**

In `runStop` (line 77), extract source from payload and pass to spawn helpers:

```go
func runStop(p hostevent.Payload, stdout io.Writer, stderr io.Writer, nudge bool) {
	defer cleanupTransientTranscript(p)
	if nudge && p.StopHookActive {
		return
	}
	source := string(p.HostSource())
	spawnResolveIfConfigured(p.CWD, source)
	spawnSupersedeIfConfigured(p.CWD, source)
	spawnReflectIfConfigured(p.CWD, source)
	// ... rest unchanged ...
```

- [ ] **Step 2: Update spawnResolveIfConfigured signature**

Change `spawnResolveIfConfigured(cwd string)` to `spawnResolveIfConfigured(cwd, source string)`. Append `--source` to the command args when non-empty:

```go
func spawnResolveIfConfigured(cwd string, source string) {
	// ... existing guard logic unchanged ...

	cmd := exec.Command(exe, "resolve", projectName, "--apply")
	if source != "" {
		cmd.Args = append(cmd.Args, "--source", source)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	detachProcess(cmd)
	// ... rest unchanged ...
```

- [ ] **Step 3: Update spawnSupersedeIfConfigured**

Same pattern — add `source string` param, append `--source` arg.

- [ ] **Step 4: Update spawnReflectIfConfigured**

Add `source string` param. Update the no-LLM guard to also check source-matched CLI:

```go
func spawnReflectIfConfigured(cwd string, source string) {
	// ... existing guard ...
	// Updated no-LLM guard: check source-matched CLI too
	cli := ai.NewCLIProviderWithBinaries(cfg.CLI.ClaudeBinary, cfg.CLI.OpenCodeBinary)
	if cfg.API.Key == "" && !cli.Available() {
		// Also check if the source-matched CLI is available
		sp := ai.NewSourceProviderForSource(source, cfg.CLI.ClaudeBinary, cfg.CLI.OpenCodeBinary)
		if !sp.Available() {
			return
		}
	}
	// ... existing DB logic ...
	cmd := exec.Command(exe, "reflect", projectName, "--apply", "--require-llm")
	if source != "" {
		cmd.Args = append(cmd.Args, "--source", source)
	}
	// ... rest unchanged ...
```

- [ ] **Step 5: Run tests**

Run: `go test ./internal/mcpinit/ -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/mcpinit/stophook.go
git commit -s -m "feat(mcpinit): pass source from stop hook to reflect/resolve/supersede"
```

---

## Task 3: Add --source flag to resolve/supersede/reflect commands

**Files:**
- Modify: `cmd/ghost/main.go`

- [ ] **Step 1: Add buildClassifyProviderForSource**

New function in `cmd/ghost/main.go`:

```go
func buildClassifyProviderForSource(cfg *config.Config, source string, logger *slog.Logger) (*ai.FallbackProvider, error) {
	if source != "" {
		sp := ai.NewSourceProviderForSource(source, cfg.CLI.ClaudeBinary, cfg.CLI.OpenCodeBinary)
		if !sp.Available() {
			return nil, fmt.Errorf("source %q: no CLI binary available", source)
		}
		return ai.NewFallbackProvider(sp, nil, false), nil
	}
	return buildClassifyProvider(cfg, logger)
}
```

- [ ] **Step 2: Add --source to runResolve**

In `runResolve` (~line 732), add `source` flag and use it:

```go
func runResolve(args []string, logger *slog.Logger) {
	fs := flag.NewFlagSet("resolve", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "")
	source := fs.String("source", "", "host source for CLI selection")
	// ... parse args ...
	provider, err := buildClassifyProviderForSource(cfg, *source, logger)
	// ... use provider ...
```

- [ ] **Step 3: Add --source to runSupersede**

Same pattern in `runSupersede` (~line 643).

- [ ] **Step 4: Add --source to runReflect**

In `runReflect` (~line 293), add the flag. When source is set and tier is "auto", prefer the source-matched CLI:

```go
func runReflect(args []string, logger *slog.Logger) {
	fs := flag.NewFlagSet("reflect", flag.ContinueOnError)
	// ... existing flags ...
	source := fs.String("source", "", "host source for CLI selection")
	// ... parse ...

	// Source-aware auto tier: use matching CLI, skip API entirely
	if *tier == "auto" && *source != "" {
		sp := ai.NewSourceProviderForSource(*source, cfg.CLI.ClaudeBinary, cfg.CLI.OpenCodeBinary)
		if sp.Available() {
			tiers = []reflection.Consolidator{
				reflection.NewNamedConsolidator(sp, sp.Name()),
			}
			if !requireLLM {
				tiers = append(tiers, reflection.NewSQLiteConsolidator())
			}
		}
		// If not available, fall through to standard auto logic
	}
```

- [ ] **Step 5: Run tests**

Run: `go test ./cmd/ghost/ -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add cmd/ghost/main.go
git commit -s -m "feat(ghost): add --source flag to reflect/resolve/supersede for source-aware LLM routing"
```

---

## Task 4: Verify end-to-end

- [ ] **Step 1: Run full test suite**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 2: Run linter**

Run: `go vet ./...`
Expected: PASS

- [ ] **Step 3: Verify on mr-slave**

Deploy to mr-slave and test with opencode session. Verify that:
1. `ghost resolve` with `--source opencode` uses opencode subprocess
2. Stop hook passes source to reflect/resolve/supersede spawns
3. No Anthropic API key needed when source is set

- [ ] **Step 4: Final commit if needed**

```bash
git commit -s -m "chore: verify source-aware provider selection"
```
