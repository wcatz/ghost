# Implementation Plan — Harness-Only Memory Management (removal of the Anthropic API tier)

Date: 2026-09-12
Spec: `docs/superpowers/specs/2026-09-12-harness-only-memory-management-design.md`
Branch: `feat/harness-only-memory-management` (feature branch + DCO-signed `git commit -s`, PR per repo convention).

## Overview

Remove Ghost's direct Anthropic HTTP API path entirely: `internal/ai` loses `Client`
(`client.go`), the wire models (`models.go`), the `FallbackProvider` credit-exhaustion
seam (`fallback_provider.go`), `ErrCreditExhausted`, `ClassifyResult`, and
`NewAnthropicProvider`. All memory-management LLM calls (reflect, resolve, supersede —
CLI, MCP `ghost_resolve`, and stop-hook auto spawns) run only through the calling
client's CLI harness (`claude`/`opencode`/`codex`/`goose` subprocesses, subscription-billed)
or the fully offline SQLite tiers. Config drops `api.key` / `ANTHROPIC_API_KEY`.
`HaikuConsolidator` is renamed `LlmConsolidator`.

The tree must stay green after every commit (each task is one commit, `go build ./...` +
`go vet ./...` + `go test ./... -count=1` pass at each checkpoint).

## Goals

- Zero capabilities lost: reflect, resolve, supersede, `ghost_resolve` MCP tool, auto-spawns.
- Remove the anthropicClient seam and every `fromFallback`/`SkippedApply`/`anyFallback` guardrail that only existed for it.
- Only config surface is `api.*` / `ANTHROPIC_API_KEY`.
- Doc drift fixed wherever it names the removed path.

## Non-goals

- `token_usage` / `RecordUsage` / `GetMonthlyCost` / `internal/ai/cost.go` (zero runtime callers today; untouched).
- Python `bench/longmemeval/phase4` harness (owns its own provider; README wording-only tweak).
- `GetTopMemories` pin "boost" comment drift (separate open task DCE155185C4CB8D75AEDC448CB58A642).
- Pull-request review bot path (PR-Agent API key, not Ghost's).

---

## Task 1 — Remove the API client + seam; reroute every consumer (commit 1)

Ordering note: `internal/ai` is the leaf; deleting `tests` files and shrinking `provider.go`
breaks `internal/resolve`, `internal/supersede`, `internal/mcpserver`,
`bench/memoryagentbench`, and `cmd/ghost`. All of Task 1 must land as one commit so
`go test ./...` stays green.

### 1a. `internal/ai/token.go` — NEW

```go
package ai

// TokenUsage reports the token counts of a CLI-harness call (claude -p
// --output-format json, opencode run --json, codex/goose equivalents). The
// json tags keep the wire shape stable — cli_client.go and opencode_client.go
// parse `usage` from the subprocess's JSON output into this shape.
type TokenUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}
```

### 1b. `internal/ai/provider.go` — REPLACE whole file

```go
package ai

// Package ai provides LLM backends for Ghost: four CLI subprocess adapters
// (claude/opencode/codex/goose) plus source-aware routing to the calling
// client's own harness. There is no direct Anthropic HTTP API client — every
// memory-management call (reflect, resolve, supersede) runs through a
// subscription-billed CLI binary or the fully offline SQLite tiers.
//
// Key types: Provider, TokenUsage.

import (
	"context"
)

// Provider answers a one-word classification question given a system prompt
// (the task instructions) and user content (the data to classify).
type Provider interface {
	Classify(ctx context.Context, systemPrompt, userContent string) (string, error)
}
```

Removed: `ErrCreditExhausted`, `ClassifyResult`, `reflectClient`, `anthropicClient`,
`NewAnthropicProvider`, `isCreditExhausted`, and the `errors` import.

### 1c. DELETE files

- `internal/ai/client.go`
- `internal/ai/fallback_provider.go`
- `internal/ai/models.go`
- `internal/ai/client_test.go` (10 tests, all against the removed client)
- `internal/ai/fallback_provider_test.go` (7 tests)
- `internal/ai/provider_test.go` (3 tests: anthropicClient join/error + isCreditExhausted)
- `internal/ai/models_test.go` (6 tests: blocks, messages, constants) — but see 1d.

### 1d. `internal/ai/token_test.go` — NEW (rescued from models_test.go)

Keep the only still-valid model test. Move it verbatim into a new file
`internal/ai/token_test.go`:

```go
package ai

import "testing"

func TestTokenUsage_ZeroValues(t *testing.T) {
	// copy the body of the old TestTokenUsage_ZeroValues untouched
}
```

### 1e. `internal/ai/cli_client.go` — doc update (lines 19–24)

Replace:

```go
// CLIClient drives Claude via the `claude` CLI (a `claude -p` subprocess)
// instead of the direct Anthropic HTTP API, so it bills to the caller's
// Claude Code subscription rather than API credits. It implements the same
// Reflect/Classify shapes as Client (reflectClient / Provider), so it can
// substitute for the direct API client anywhere ANTHROPIC_API_KEY would
// otherwise be required.
```

with:

```go
// CLIClient drives Claude via the `claude` CLI (a `claude -p` subprocess).
// It bills to the caller's Claude Code subscription rather than API credits.
// It implements the same Reflect/Classify shapes as the other CLI adapters
// (the cliBackend interface), so it serves reflect/resolve/supersede without
// any API key.
```

(The lines 26–33 paragraph about scrubbing `ANTHROPIC_API_KEY` and
`--setting-sources` stays — still correct and important.)

### 1f. `internal/ai/source_provider.go` — no functional change

No seam references in this file. Leave as-is.

### 1g. `internal/resolve/resolution.go` — REWRITE (whole file)

`classifyProvider` now returns `(string, error)`; `IsResolved` drops the
`fromFallback` result. Remove the `ai` import.

```go
package resolve

import (
	"context"
	"strings"
)

// classifyProvider is the one method the classifier needs — satisfied by
// *ai.CLIProvider and *ai.SourceProvider. Narrowed so tests never need a real
// provider.
type classifyProvider interface {
	Classify(ctx context.Context, systemPrompt, userContent string) (string, error)
}

// ResolutionClassifier answers the conclusion-vs-evidence question with a single
// fast classify call per memory. It is biased to KEEP: a false RESOLVED buries
// a still-useful memory (dropping it from injection), whereas a missed one
// merely leaves the status quo — so anything short of an explicit RESOLVED is
// KEEP.
//
// The name is deliberately provider- and model-agnostic: it only needs a
// classifyProvider with a Classify method (typically *ai.CLIProvider or an
// *ai.SourceProvider from the calling session), which any CLI harness — a
// `claude`, `opencode`, `codex`, or `goose` subprocess — can satisfy. It is not
// tied to a specific model tier.
type ResolutionClassifier struct {
	client classifyProvider
}

// NewResolutionClassifier wraps a classifyProvider (typically *ai.CLIProvider
// or *ai.SourceProvider) as a Classifier.
func NewResolutionClassifier(client classifyProvider) *ResolutionClassifier {
	return &ResolutionClassifier{client: client}
}

const classifySystemPrompt = `...` // UNCHANGED, copy verbatim from current file

// IsResolved returns true iff the classifier explicitly answers RESOLVED.
// Every call goes through one CLI-harness provider, so there is no fallback
// distinction for callers to withhold — a degraded answer simply doesn't
// count as RESOLVED (KEEP bias).
func (h *ResolutionClassifier) IsResolved(ctx context.Context, content string) (resolved bool, err error) {
	result, err := h.client.Classify(ctx, classifySystemPrompt, "NOTE: "+quoteData(content))
	if err != nil {
		return false, err
	}
	// Bias to KEEP: only an explicit "resolved" counts, and only the first
	// decisive token is honored so a rambling reply can't smuggle a flip.
	for _, field := range strings.Fields(strings.ToLower(result)) {
		t := strings.Trim(field, ".,!\"'`:;—-")
		if t == "resolved" {
			return true, nil
		}
		if t == "keep" {
			return false, nil
		}
	}
	return false, nil
}

// quoteData ... UNCHANGED, copy verbatim
```

### 1h. `internal/resolve/resolve.go` — edits

- Package doc line 9: `The LLM Classifier implementation lives in haiku.go.`
  → `The LLM Classifier implementation lives in resolution.go; the hosting binary supplies a CLI-harness provider (see internal/ai).`
- `Classifier` interface (lines 45–52): signature → `IsResolved(ctx context.Context, content string) (resolved bool, err error)`; doc: "The LLM implementation lives in resolution.go; tests inject a deterministic fake."
- `Result` struct (lines 62–75): delete the `SkippedApply bool` field and its doc comment block.
- `Run` (lines 110–134):
  - Delete `anyFallback := false` (line 111).
  - Replace the classify loop (lines 112–125) with:

```go
	var confirmed []memory.Memory
	for _, m := range cands {
		ok, err := cls.IsResolved(ctx, m.Content)
		if err != nil {
			return res, nil, fmt.Errorf("classify %s: %w", m.ID, err)
		}
		if !ok {
			continue
		}
		res.Confirmed++
		confirmed = append(confirmed, m)
	}
```

  - Delete the `if apply && anyFallback && len(confirmed) > 0 { ... }` block (lines 127–134) entirely.

### 1i. `internal/resolve/resolution_test.go` — edits

- `fakeProvider` (lines 12–23): drop the `fromFallback` field; `Classify` returns `(string, error)`:

```go
type fakeProvider struct {
	resp            string
	lastSystem      string
	lastUserContent string
}

func (f *fakeProvider) Classify(_ context.Context, systemPrompt, userContent string) (string, error) {
	f.lastSystem = systemPrompt
	f.lastUserContent = userContent
	return f.resp, nil
}
```

- `TestHaikuParsesResolved` → rename `TestIsResolvedParsesResolved`; `IsResolved` call becomes `got, err := h.IsResolved(...)`.
- `TestHaikuWrapsContentAsData` → rename `TestIsResolvedWrapsContentAsData`; call → `_, err := h.IsResolved(...)`.
- DELETE `TestHaikuPropagatesFromFallback` (lines 62–75).

### 1j. `internal/resolve/resolve_test.go` — edits

- `fakeClassifier.IsResolved` (line 20): signature → `(bool, error)`; body returns `f.drop[content], nil`.
- DELETE `fallbackClassifier` (lines 26–43).
- `TestRunResolvesConfirmedEvidence`: delete the `if res.SkippedApply { ... }` block (lines 124–126).
- DELETE tests: `TestRun_FallbackClassificationButNothingConfirmed_DoesNotReportSkip` (148–176), `TestRun_FallbackClassification_SkipsApply` (178–200), `TestRun_MixedFallbackAndPrimary_SkipsApply` (202–230).

### 1k. `internal/supersede/relation.go` — REWRITE (whole file)

Mirrors 1g. `classifyProvider` → `(string, error)`; `Classify` → `(Relation, error)`;
remove the `ai` import.

```go
package supersede

import (
	"context"
	"fmt"
	"strings"
)

// classifyProvider is the one method the classifier needs — satisfied by
// *ai.CLIProvider and *ai.SourceProvider. Narrowed so tests never need a real
// provider.
type classifyProvider interface {
	Classify(ctx context.Context, systemPrompt, userContent string) (string, error)
}

// RelationClassifier classifies a NEWER/OLDER memory pair with a single fast
// classify call per candidate pair. The prompt forces a 3-way choice so a
// decision that merely *cites* still-valid evidence (CAUSES) is never
// conflated with a genuine same-fact replacement (SUPERSEDES): conflating the
// two would bury independently useful memories under supersede-demote
// ranking. When uncertain the prompt biases toward NEITHER — writing no link
// is cheaper to recover from than a false SUPERSEDES or false CAUSES.
//
// The name is deliberately provider- and model-agnostic: RelationClassifier
// only needs a classifyProvider with a Classify method (typically
// *ai.CLIProvider or *ai.SourceProvider), which any CLI harness — a `claude`,
// `opencode`, `codex`, or `goose` subprocess — can satisfy.
type RelationClassifier struct {
	client classifyProvider
}

// NewRelationClassifier wraps a classifyProvider (typically *ai.CLIProvider
// or *ai.SourceProvider) as a Classifier.
func NewRelationClassifier(client classifyProvider) *RelationClassifier {
	return &RelationClassifier{client: client}
}

const classifySystemPrompt = `...` // UNCHANGED, copy verbatim

// Classify asks the classifier to judge the relationship between newer and
// older. Every call goes through one CLI-harness provider, so there is no
// fallback distinction for callers to withhold. An unparseable response is a
// fatal error, not a silent NEITHER default — a silent default would mask a
// broken prompt or model regression as normal, uneventful traffic.
func (h *RelationClassifier) Classify(ctx context.Context, newer, older string) (Relation, error) {
	content := "OLDER: " + quoteData(older) + "\nNEWER: " + quoteData(newer)
	result, err := h.client.Classify(ctx, classifySystemPrompt, content)
	if err != nil {
		return "", err
	}
	rel, ok := parseRelation(result)
	if !ok {
		return "", fmt.Errorf("unparseable classifier response: %q", result)
	}
	return rel, nil
}

// parseRelation ... UNCHANGED
// quoteData ... UNCHANGED
```

### 1l. `internal/supersede/supersede.go` — edits

- `Classifier` interface (lines 66–74): signature → `Classify(ctx context.Context, newer, older string) (relation Relation, err error)`; doc → "It also reports whether the answer came from a fallback provider" sentence deleted; "The LLM implementation lives in the CLI layer; tests inject a deterministic mock." stays.
- `Run` doc comment (lines 214–218): DELETE the paragraph "If any classification in the batch came from a fallback provider, apply is skipped entirely for the whole batch (mirroring internal/resolve) ... what would have happened."
- `Run` body:
  - Delete `anyFallback := false` (line 320).
  - classify loop (lines 322–343): `verdict, err := cls.Classify(ctx, c.NewerContent, c.OlderContent)`; delete the `if fromFallback { anyFallback = true }` lines (326–328).
  - Delete the `if apply && anyFallback { ... return res, classified, nil }` block (lines 345–351).

### 1m. `internal/supersede/relation_test.go` — edits

- `fakeProvider` (lines 14–29): drop `fromFallback`; `Classify` → `(string, error)`.
- All `got, _, err := cls.Classify(...)` calls (46, 59, 70, 84, 92) → `got, err := ...` / `_, err := ...`.
- `TestHaikuWrapsContentAsData` → `TestRelationClassifierWrapsContentAsData`; `TestHaikuPropagatesFromFallback` (67–80) → DELETE; keep the rest.
- `TestRelationClassifierLive` (lines 106–166): delete line 128 `provider := ai.NewFallbackProvider(cli, nil, false)`; `cls := NewRelationClassifier(cli)`; doc comment (106–114) fine already — remove the words "the one piece of the creation path with no deterministic test" parenthetical stays accurate? It says "(the one piece ... no deterministic test)". Keep. The `ai` import stays (used by `NewSourceProviderForSource`).

### 1n. `internal/supersede/supersede_test.go` — edits

- DELETE `fallbackClassifier` (lines 27–40) and tests `TestRun_FallbackClassification_SkipsApply` (386–407) and `TestRun_MixedFallbackAndPrimary_SkipsApply` (409–443).
- Grep the package for any other `Classify(` fake (e.g. a stub used by other `TestRun_*`) and update its signature to `(Relation, error)`.

### 1o. `internal/mcpserver/mcpserver.go` — resolve handler (lines 1001–1019)

Delete line 1018 `provider := ai.NewFallbackProvider(cli, nil, false)` and change line 1019 to:

```go
		cls := resolve.NewResolutionClassifier(cli)
```

Also update lines 1001–1009 comment: delete the final "MCP sampling was retired here per spec 2026-07-28 (SEP-2577 deprecates Sampling) — see docs/superpowers/specs/2026-08-24-resolve-sampling-path-design.md." if it no longer matches the handler (this handler also reads `cfg.CLI.*` binaries? note: current line 1014 calls `ai.NewSourceProviderForSource(ai.SourceForClientName(clientName))` WITHOUT the cfg binaries — leave that behavior as-is, it is pre-existing and not part of this task).

### 1p. `bench/memoryagentbench/classifier.go` (line 32)

```go
	cls := supersede.NewRelationClassifier(ai.NewOpenCodeClientWithBinary(binary))
```

(deletes the `ai.NewFallbackProvider(...)` wrap; drop the now-unused `provider` local.)

### 1q. `cmd/ghost/main.go` — edits

**(q1) Reflect usage text** (line 462):

```
  --tier string   Consolidation tier: auto, cli, opencode, sqlite (default "auto")
```

**(q2) Reflect source-aware comment** (lines 490–493):
current:
```go
	// Source-aware auto tier: when --source is set and tier is "auto", prefer
	// the source-matched CLI backend directly, skipping API entirely. This
	// lets the stop-hook or cron reflect use the same CLI that served the
	// session, avoiding ANTHROPIC_API_KEY when not needed.
```
new:
```go
	// Source-aware auto tier: when --source is set and tier is "auto", prefer
	// the source-matched CLI backend directly over the cascade. This lets the
	// stop-hook or cron reflect use the same CLI that served the session.
```

**(q3) Reflect tier switch** (lines 509–569). Replace `case "haiku"` (510–516):
```go
		case "haiku":
			fmt.Fprintln(os.Stderr, "error: haiku tier removed — Ghost's memory management no longer calls the Anthropic API; use --tier auto (default) or --tier cli")
			os.Exit(1)
```
Replace `default: // "auto"` branch (551–569):
```go
		default: // "auto"
			var tiers []reflection.Consolidator
			if cli := ai.NewCLIProviderWithBinaries(cfg.CLI.ClaudeBinary, cfg.CLI.OpenCodeBinary, cfg.CLI.CodexBinary, cfg.CLI.GooseBinary); cli.Available() {
				tiers = append(tiers, reflection.NewNamedConsolidator(cli, cli.Name()))
			}
			// --require-llm is the autonomous-reflect guard: it must never silently
			// degrade to the Jaccard-only sqlite tier (which would rewrite every
			// non-manual memory with no consolidation quality). Without a CLI
			// harness on PATH (or cli.*_binary), there is no LLM to require, so a
			// stale/unset binary means --require-llm fails the pass at write time.
			if !requireLLM {
				tiers = append(tiers, reflection.NewSQLiteConsolidator())
			}
			consolidator = reflection.NewTieredConsolidator(tiers, logger)
```

**(q4) buildClassifyProvider + buildClassifyProviderForSource** (lines 779–812). Replace both with:

```go
// buildClassifyProvider returns the classification provider for the headless
// CLI path (ghost resolve / ghost supersede — and the stop hook's detached
// spawns): a subscription-billed CLI call, claude first, then opencode, codex,
// goose (see ai.CLIProvider), picked from PATH or cli.*_binary config. These
// commands fail fast with a clear message when no CLI binary is installed —
// there is no Anthropic API fallback.
func buildClassifyProvider(cfg *config.Config) (ai.Provider, error) {
	cli := ai.NewCLIProviderWithBinaries(cfg.CLI.ClaudeBinary, cfg.CLI.OpenCodeBinary, cfg.CLI.CodexBinary, cfg.CLI.GooseBinary)
	if !cli.Available() {
		return nil, fmt.Errorf("requires a `claude`, `opencode`, `codex`, or `goose` binary (on PATH or via cli.claude_binary/cli.opencode_binary/cli.codex_binary/cli.goose_binary)")
	}
	return cli, nil
}

// buildClassifyProviderForSource routes classification through the CLI harness
// that served the session (--source claude-code → the claude binary, etc.) or,
// when source is empty, the best harness on PATH (buildClassifyProvider).
func buildClassifyProviderForSource(cfg *config.Config, source string) (ai.Provider, error) {
	if source != "" {
		sp := ai.NewSourceProviderForSource(source, cfg.CLI.ClaudeBinary, cfg.CLI.OpenCodeBinary, cfg.CLI.CodexBinary, cfg.CLI.GooseBinary)
		if !sp.Available() {
			return nil, fmt.Errorf("source %q: no CLI binary available", source)
		}
		return sp, nil
	}
	return buildClassifyProvider(cfg)
}
```

**(q5) runSupersede** (line 870) and **runResolve** (line 958):
`provider, err := buildClassifyProviderForSource(cfg, source, logger)` →
`provider, err := buildClassifyProviderForSource(cfg, source)`.

**(q6) runSupersede usage text** (lines 858–861):
```
Classifies each candidate as supersedes, causes, or neither. Requires a
'claude', 'opencode', 'codex', or 'goose' binary on PATH (or via
cli.claude_binary / cli.opencode_binary / cli.codex_binary /
cli.goose_binary) — billing runs through that CLI's subscription.`)
```

**(q7) runResolve usage text** (lines 946–949):
```
Marks resolved-evidence memories so they drop from session-start injection
(still searchable). Requires a 'claude', 'opencode', 'codex', or 'goose'
binary on PATH (or via cli.claude_binary / cli.opencode_binary /
cli.codex_binary / cli.goose_binary) — billing runs through that CLI's
subscription.`)
```

**(q8) printUsage reflect flags** (line 1467):
```
  --tier string   Consolidation tier: auto, cli, opencode, sqlite (default "auto")
```

Check top-of-file imports: `errors` may become unused once q4 drops `errors.New`
— remove from the import block if `go vet`/build complains; `fmt` stays; `log/slog`
may become unused if no other use (bootstrap returns logger everywhere; keep, verify).

### 1r. `cmd/ghost/main_test.go` — edits (lines 370–414)

Replace the three `TestBuildClassifyProvider_*` tests:

```go
// TestBuildClassifyProvider_NoKeyNoBackendErrors: with no claude/opencode/codex/
// goose binary resolvable, building the classifier must fail loudly with a
// message naming the required binaries.
func TestBuildClassifyProvider_NoKeyNoBackendErrors(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty dir defeats bare-name LookPath lookups
	cfg := &config.Config{}
	_, err := buildClassifyProvider(cfg)
	if err == nil || !strings.Contains(err.Error(), "`claude`") {
		t.Fatalf("want backend error, got %v", err)
	}
}

// TestBuildClassifyProvider_NoKeyFallsToOpencodeStub: with no claude binary but
// an explicit opencode binary configured, the returned provider must classify
// through that opencode binary.
func TestBuildClassifyProvider_NoKeyFallsToOpencodeStub(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	stub := stubBinary(t, `{"type":"text","part":{"type":"text","text":"STUB_CLASSIFY_OK"}}`)
	cfg := &config.Config{}
	cfg.CLI.OpenCodeBinary = stub
	p, err := buildClassifyProvider(cfg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	out, err := p.Classify(context.Background(), "sys prompt", "user content")
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if out != "STUB_CLASSIFY_OK" {
		t.Fatalf("got %q", out)
	}
}
```

(Delete `TestBuildClassifyProvider_KeySetBuildsRegardless` entirely. `slogDiscard()`
can stay if still used elsewhere in the file, else delete it and its `io.Discard`
import if now unused.)

### 1s. Checkpoint

```
cd /home/wayne/git/ghost
go build ./...
go vet ./...
go test ./... -count=1
grep -rn "FallbackProvider\|ClassifyResult\|ErrCreditExhausted\|NewAnthropicProvider\|NewHaikuConsolidator\|haiku tier\|ANTHROPIC_API_KEY" --include=*.go internal cmd bench  # expect: only cost.go/GetMonthlyCost, cli_client scrub comment, eval/cycle scrub, stophook_test isolatedHome
git add -A && git commit -s -m "chore(ai): remove Anthropic API tier and credit-exhaustion seam — harness-only memory management"
```
(`cmd/ghost` needs `git add -f` for new files under that dir, per the known .gitignore quirk — untracked `token.go`/`token_test.go` are under `internal/ai` so plain add works; double-check `git status` shows new files staged.)

---

## Task 2 — Remove `api.*` config; CLI-only stop-hook guard (commit 2)

### 2a. `internal/config/config.go`

- Package doc line 7: `//  4. GHOST_* environment variables (plus ANTHROPIC_API_KEY)` → `//  4. GHOST_* environment variables`
- `Config` struct (line 29): delete `API        APIConfig        koanf:"api"`.
- DELETE `APIConfig` (lines 47–50).
- Line 143 comment `(e.g. cfg.API.ModelQuality = *modelFlag)` → `(e.g. cfg.CLI.OpenCodeBinary = *binaryFlag)`.
- Line 163 comment: `// e.g. GHOST_API_KEY → api.key, GHOST_DEFAULTS_MODE → defaults.mode` → `// e.g. GHOST_EMBEDDING_MODEL → embedding.model, GHOST_OBSIDIAN_VAULT_DIR → obsidian.vault_dir`.
- DELETE lines 171–176:
```go
	// Also support the standard ANTHROPIC_API_KEY.
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		_ = k.Load(confmap.Provider(map[string]interface{}{
			"api.key": key,
		}, "."), nil)
	}
```

### 2b. `internal/config/config.example.yaml`

- Line 8: `#   4. GHOST_* environment variables (plus ANTHROPIC_API_KEY)` → `#   4. GHOST_* environment variables`
- DELETE lines 10–14 (the `# --- Claude API ---` + `api:` block).
- Line 39 heading `# --- Subprocess LLM binaries (reflect CLI tier) ---` → `# --- Subprocess LLM binaries (reflect/resolve/supersede CLI tier) ---`; line 40 mention "reflect CLI tier" → "The reflect/resolve/supersede CLI tiers shell out to `claude -p`, `opencode run`, `codex exec`, or `goose run`."

### 2c. `internal/config/config_test.go`

- Line 30 `unsetEnvVars(t, []string{"GHOST_API_KEY", "GHOST_EMBEDDING_ENABLED", "ANTHROPIC_API_KEY"})` → `unsetEnvVars(t, []string{"GHOST_EMBEDDING_ENABLED"})`.
- Lines 45–46: delete the `if cfg.API.Key != "" {` assertion block in `TestLoad_Defaults`.
- DELETE `TestLoad_AnthropicAPIKey` (lines 50–61).
- Lines 91, 109, 134, 197, 219, 297: remove `"GHOST_API_KEY", "ANTHROPIC_API_KEY"` from the `unsetEnvVars` lists (keep the var that each test actually cares about).
- `TestLoad_GhostEnvOverrides` (146–169): delete line 152 `"GHOST_API_KEY",` `"ANTHROPIC_API_KEY"` from the unset list; delete line 155 `t.Setenv("GHOST_API_KEY", "sk-ghost-override")`; delete lines 163–165 (the `cfg.API.Key` assertion). Test now only asserts `cfg.Embedding.Model`.
- `TestLoad_YAMLFileOverride` (249–259): remove the `api:\n  key: "sk-from-yaml"\n` lines from `yamlContent`; delete lines 269–270 (`if cfg.API.Key != "sk-from-yaml" {...}`).

### 2d. `internal/mcpinit/stophook.go` — guard + comments

- **Guard** (line 340):
```go
	if !ai.NewCLIProviderWithBinaries(cfg.CLI.ClaudeBinary, cfg.CLI.OpenCodeBinary, cfg.CLI.CodexBinary, cfg.CLI.GooseBinary).Available() {
		sp := ai.NewSourceProviderForSource(source, cfg.CLI.ClaudeBinary, cfg.CLI.OpenCodeBinary, cfg.CLI.CodexBinary, cfg.CLI.GooseBinary)
		if !sp.Available() {
			slog.Warn("reflect: skipping — no CLI binary available", "source", source)
			return
		}
	}
```
- **spawnResolveIfConfigured doc** (165–167): replace "If the Anthropic API is out of credit, the spawned process itself fails fast ... until credits are restored." with:
```
// If no CLI binary is available to the spawned process, it fails fast and
// logs the failure to resolve.log — no local fallback runs in this path, so
// auto-resolve simply does nothing until a binary is on PATH.
```
- **spawnSupersedeIfConfigured doc** (245–247): same replacement with `supersede.log`.
- **claim-pid docs** (lines 207 and 288): "both decide to spawn a paid-API, DB-writing process" → "both decide to spawn a DB-writing process".
- **spawnReflectIfConfigured doc** (326–331): replace "Without an API key or a claude/opencode binary, --tier auto would fall through to the Jaccard-only sqlite tier ..." with:
```
// Unlike the resolve/supersede twins, this adds a no-LLM guard: consolidation
// is only worth an unattended write when a real LLM tier is available. Without
// a claude/opencode/codex/goose CLI binary, --tier auto would fall through to
// the Jaccard-only sqlite tier and rewrite every non-manual memory for no
// quality gain, so the spawn is skipped entirely — before the DB is even
// opened, so this stays a cheap read-only no-op.
```

`stophook_test.go` needs no logic change (the NoLLM skip test at 395–414 still holds with empty PATH; `isolatedHome` keeps scrubbing `ANTHROPIC_API_KEY`, which stays harmless).

### 2e. Checkpoint

```
cd /home/wayne/git/ghost
go build ./... && go vet ./... && go test ./... -count=1
grep -rn "cfg.API\|api.key\|APIConfig\|ANTHROPIC_API_KEY" --include=*.go internal cmd | grep -v "_test.go"   # expect: cli_client scrub comment, opencode_client? (verify), eval/cycle scrub — none of cfg-related
git add -A && git commit -s -m "chore(config): drop api.key/ANTHROPIC_API_KEY config; stop-hook guard is CLI-harness-only"
```

---

## Task 3 — Rename `HaikuConsolidator` → `LlmConsolidator` (commit 3)

### 3a. `internal/reflection/tier_haiku.go` → `tier_llm.go`

`git mv internal/reflection/tier_haiku.go internal/reflection/tier_llm.go`, then edit:

- Package doc: `// HaikuConsolidator uses an LLM (direct Anthropic API by default) for` → `// LlmConsolidator uses the calling client's CLI harness (a claude/opencode/`
- `type HaikuConsolidator` → `type LlmConsolidator` (struct + all methods: `Name`, `Mechanical`, `Available`, `Consolidate`).
- `NewHaikuConsolidator` → `NewLlmConsolidator` (name `"llm"`).
- `NewNamedConsolidator` doc: "is NewHaikuConsolidator with an explicit tier name" → "is NewLlmConsolidator with an explicit tier name"; return type `*HaikuConsolidator` → `*LlmConsolidator`.
- `Available` body has no `ai` type dep that changes; the `reflector` interface + `ai` import stay.
- Line 73 comment "the old fallback returned" — this refers to the API client's error handling; rewrite to "a prior implementation silently returned a result" (or leave — verify context and reword minimally).

### 3b. `internal/reflection/consolidator.go` — package doc edits

- Line 3: `// Package reflection performs memory consolidation: HaikuConsolidator (Anthropic API)` → `// Package reflection performs memory consolidation: LlmConsolidator (via the calling client's CLI harness)`.
- Line 5: "fallback, and TieredConsolidator that tries tiers in priority order with a quality" — stays.
- Line 23: "Mechanical reports whether this tier is the deterministic fallback" — stays (sqlite).
- Line 39: `NewNamedConsolidator` doc mention of HaikuConsolidator → LlmConsolidator (verify exact wording when editing).

### 3c. `internal/reflection/consolidator_test.go`

- Line 559–560: `func TestHaikuConsolidator_NilClient` → `func TestLlmConsolidator_NilClient`; `NewHaikuConsolidator(nil)` → `NewLlmConsolidator(nil)`.
- Grep file for other `Haiku` mentions (test names/comments) and rename or reword to `Llm`.

### 3d. Checkpoint

```
cd /home/wayne/git/ghost
go build ./... && go vet ./... && go test ./... -count=1
grep -rn "HaikuConsolidator\|NewHaikuConsolidator\|tier_haiku" --include=*.go .   # expect: empty
git add -A && git commit -s -m "refactor(reflection): rename HaikuConsolidator to LlmConsolidator"
```

---

## Task 4 — Docs sweep (commit 4)

No Go changes; the tree must still pass the checkpoint (docs only).

### 4a. `README.md`

- Line 98: "(the MCP server itself never uses the API key)" → "(the MCP server itself never calls an API)".
- Lines 100–103 (docker example): remove `-e ANTHROPIC_API_KEY=sk-ant-...` from the `reflect` invocation; the comment above (line 98) already centers on `XDG_DATA_HOME`/`-i`, so the command becomes:
```bash
docker run -i -e XDG_DATA_HOME=/data -v ghost-data:/data \
  ghcr.io/wcatz/ghost:latest reflect myproject --apply
```
- Line 150: replace "the Anthropic API when `ANTHROPIC_API_KEY` is set and no CLI binary (claude/opencode/codex/goose) is available for consolidation, resolve, or supersede, and the GitHub API *only if* you run `ghost upgrade`" → "an LLM call through the calling client's own CLI harness (claude/opencode/codex/goose) when you run reflect/resolve/supersede, and the GitHub API *only if* you run `ghost upgrade`".
- Line 177: replace "When `ANTHROPIC_API_KEY` is set and no CLI binary is available, consolidate/resolve/supersede use the Anthropic API (cost scales with memory count — roughly $0.001 for a typical project); otherwise they fall back to a free CLI call (claude, opencode, codex, or goose) or the fully offline SQLite tier." → "consolidate/resolve/supersede run through your own CLI harness — `claude`, `opencode`, `codex`, or `goose` on PATH, billed to that CLI's subscription — or the fully offline SQLite tier."
- Line 211: replace "Tiered: Anthropic API (Haiku) first, then a CLI tier (claude, opencode, codex, or goose — whichever is on PATH), falling back to a fully offline SQLite tier (Jaccard >= 0.5, same-category merges). When `--source` is set (e.g. `--source opencode`), the API tier is skipped entirely and the matching CLI binary is used directly." → "Tiered: a CLI-harness tier (claude, opencode, codex, or goose — whichever is on PATH), then a fully offline SQLite tier (Jaccard >= 0.5, same-category merges). When `--source` is set (e.g. `--source opencode`), the matching CLI binary is used directly."
- Line 316: "4. `GHOST_*` environment variables, plus `ANTHROPIC_API_KEY` (used by reflect/resolve/supersede when no CLI binary is available)" → "4. `GHOST_*` environment variables".

### 4b. `docs/architecture.md`

- Line 43: `haiku.go               LLM classifier for 3-way SUPERSEDES/CAUSES/NEITHER classification` → `relation.go             LLM classifier for 3-way SUPERSEDES/CAUSES/NEITHER classification` (match neighboring column alignment).
- Line 74: `tier_haiku.go          Haiku LLM consolidation (requires ANTHROPIC_API_KEY or use CLI tier)` → `tier_llm.go             LLM consolidation via the calling client's CLI harness (claude/opencode/codex/goose)`.
- Lines 126–127:
```
      → HaikuConsolidator (if configured with Anthropic key or via CLI)
          → Anthropic API (haiku model)
```
→
```
      → LlmConsolidator via the calling client's CLI harness
          → claude/opencode/codex/goose subprocess (subscription-billed)
```

### 4c. `docs/benchmarks.md`

- Line 105: "confirms each with a single Haiku call" → "confirms each with a single CLI-harness classify call"; the trailing "and on a labeled set of genuine-vs-parallel pairs it scored 8/8 (`TestHaikuClassifierLive`, run manually with an API key; skipped in CI)" → "and on a labeled set of genuine-vs-parallel pairs it scored 8/8 (`TestRelationClassifierLive`, run manually against a CLI harness; skipped when none is installed)".
- Line 271: "expected non-determinism across two separate Haiku calls, not a bug" → "expected non-determinism across two separate classification calls, not a bug".

### 4d. `docs/ROADMAP.md`

- Line 147: "Data Processing Agreement / subprocessor disclosure before offering the Haiku reflection tier to any enterprise customer" → "...before offering the reflection tier to any enterprise customer" (keep the DPA item; drop "Haiku").
- Line 148: delete the bullet `- [ ] Secrets manager for ANTHROPIC_API_KEY in shared deployments (Vault, or k8s External Secrets) instead of a bare env var` (the env var no longer exists).
- Line 238: "**`ghost supersede`'s Haiku classifier is validated on only 8/8 labeled examples.**" → "**`ghost supersede`'s relation classifier is validated on only 8/8 labeled examples.**"

### 4e. `CLAUDE.md` (repo root)

- `internal/ai/` bullet: "Claude API client (non-streaming Reflect call, used by reflection + resolve + supersede); `Provider` seam (`anthropicClient`/`SamplingProvider`) with `FallbackProvider` credit-exhaustion fallover for resolve/supersede" → "CLI-harness providers (`CLIClient`/`OpenCodeClient`/`SourceProvider`) used by reflection + resolve + supersede — no Anthropic HTTP API client".
- Resolution classifier bullet: "a single KEEP-biased Haiku call per candidate" → "a single KEEP-biased CLI-harness classify call per candidate".
- Replace the entire "Classifier fallback:" bullet with:
  "`resolve`/`supersede` classify via the calling session's own CLI harness — the backend is picked from the MCP client's reported identity (an opencode session uses the opencode binary, claude uses claude, etc.) via `ai.SourceProvider`, or from the CLIProvider cascade (claude→opencode→codex→goose) on the headless CLI path; a machine with no CLI binary fails fast with a clear message. MCP sampling was retired from this path per SEP-2577 — see `docs/superpowers/specs/2026-08-24-resolve-sampling-path-design.md`."

### 4f. Superseded/amended specs

- `docs/superpowers/specs/2026-07-26-classifier-fallback-design.md`: prepend a banner at top: `> **SUPERSEDED (2026-09-12):** the Anthropic API / FallbackProvider credit-exhaustion seam is removed wholesale — see 2026-09-12-harness-only-memory-management. The file is kept for history.`
- `docs/superpowers/specs/2026-08-19-autonomous-reflect-design.md`: same banner if it references the API tier (verify; it primarily describes the stop-hook spawn + `--require-llm` guard — banner only if it names haiku/API).
- `docs/superpowers/specs/2026-08-24-resolve-sampling-path-design.md`: in the headless-CLI row of the "backends" table (the row that says "no secondary in headless / fails fast on credit exhaustion"), replace "fails fast on credit exhaustion" → "fails fast when no CLI binary is on PATH". Leave the MCP-sampling-retirement content alone.

### 4g. `bench/longmemeval/phase4/README.md` (wording-only)

- In the "keys" / API section, keep the standalone-provider note but drop any implication that mainline Ghost uses `api.key`; if it says "falls back to api.key from ~/.config/ghost/config.yaml", change to "reads `api.key` under `[api]` only if the user still has a legacy config file; new installs have none".

### 4h. Checkpoint

```
cd /home/wayne/git/ghost
grep -rn "ANTHROPIC_API_KEY\|Haiku\|haiku\|FallbackProvider" README.md docs CLAUDE.md | grep -v "superseded\|SUPERSEDED\|legacy\|2026-07-26-classifier-fallback-design"   # review each hit
go build ./... && go vet ./... && go test ./... -count=1
git add -A && git commit -s -m "docs: sweep Anthropic-API/Haiku references after harness-only memory management"
```

---

## Task 5 — Final verification (commit 5, if anything surfaces)

1. `go build ./... && go vet ./... && go test ./... -count=1` (top-level).
2. `go test ./... -race ./internal/resolve ./internal/supersede ./internal/memory ./internal/mcpinit` (racy paths that touched).
3. Residual-reference greps — each remaining hit must be a deliberate keeper:
   - `grep -rn "ANTHROPIC_API_KEY" --include=*.go .` → expect only: `internal/ai/cli_client.go` (scrub comment), `cmd/ghost/main.go` none, `eval/cycle/main.go` (scrub), `internal/mcpinit/stophook_test.go` (`isolatedHome` scrub — keep), any bench harness that still scrubs env.
   - `grep -rn "Haiku\|FallbackProvider\|ClassifyResult\|ErrCreditExhausted" --include=*.go .` → expect empty or deliberate keepers.
   - `grep -rn "ghost_api\|api.key" internal/config cmd/ghost` → expect no config loads.
4. Manual smoke (local, MR-SLAVE via PATH doesn't matter — run on this machine):
   - `go run ./cmd/ghost resolve <proj>` dry-run with opencode on PATH → resolves fine.
   - `go run ./cmd/ghost supersede <proj>` dry-run.
   - `go run ./cmd/ghost reflect <proj> --dry-run --tier auto` → logs `tier=opencode`-style (CLI named).
   - `go run ./cmd/ghost reflect <proj> --tier haiku` → the clear "removed" error, exit 1.
   - `PATH=/tmp/go-nowhere go run ./cmd/ghost resolve <proj>` → clean "requires a `claude`..." error.
   - `ghost resolve ghost --source opencode` behaves identically to cascade (source-aware routing intact).
5. Cross-check the open task `DCE155185C4CB8D75AEDC448CB58A642` (comment drift): the ai.Client streaming claim and resolve pkg doc are fixed by this work; `GetTopMemories` pin wording remains deliberately untouched. Update that open task/notes.
6. Push branch, open DCO-signed PR (`--source` none), point PR-Agent at it per repo convention; watch CI + bot review per standing directive.

## Risks / mitigations

- **Tree red mid-Task-1:** unavoidable cross-package cascade; Task 1 deliberately bundles every consumer of the removed `ai` types into one green commit. Smoke-test `go build ./...` after each file group inside the task.
- **Forgotten `ai` call site:** the grep in 1s covers the six packages; if `go build` finds more, fix inline in Task 1 (no commit-splitting).
- **`cliBackend`/`reflector` interface drift:** `CLIProvider` still satisfies both `reflector` (Reflect) and `Provider` (Classify) — verified from source; the auto tier keeps passing `*CLIProvider` to `NewNamedConsolidator`.
- **`TestRelationClassifierLive` with no CLI:** unchanged skip behavior (line 125–127); only the fallback wrap is removed.
- **Docs "Superseded" noise:** banners are deliberate (history), not removed content.

## Definitions of done

- Every memory-management LLM path resolves to a CLI-harness provider or the SQLite tier; nothing references `ANTHROPIC_API_KEY`/`api.key`/`FallbackProvider`/`ClassifyResult`/`HaikuConsolidator` except deliberate scrub-comments in the CLI adapters.
- `go build ./...`, `go vet ./...`, `go test ./... -count=1` green at every commit.
- PR opened with the 4 (or 5) scope commits, DCO-signed, on a feature branch.