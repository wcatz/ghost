# Harness-only memory management: remove the Anthropic API tier

Date: 2026-09-12

## Goal

Route **all** of Ghost's memory-management LLM calls — `reflect`, `resolve`, and
`supersede`, on every path (CLI, MCP `ghost_resolve`, stop-hook autonomous
spawns) — exclusively through the calling client's CLI harness (a `claude`,
`opencode`, `codex`, or `goose` subprocess, subscription-billed). Remove the
direct Anthropic HTTP API path entirely: the Haiku client, the `--tier haiku`
tier, the `cfg.API` / `ANTHROPIC_API_KEY` configuration, and the
credit-exhaustion fallback seam that existed only to degrade around that API.

Also fix the closely-coupled comment/doc drift that describes the removed
architecture (resolve package doc, `runResolve`/`runSupersede` doc comments,
`ai` package doc, README/architecture/ROADMAP/config example).

## Non-goals

- Removing `token_usage`, `RecordUsage`, `GetMonthlyCost`, or `internal/ai/cost.go`.
  Cost tracking has zero runtime callers today (only tests) and the `memory → ai`
  edge exists solely for `CostWithoutCacheForUsage`. Deleting it is a separate
  concern; CLI providers report zero `TokenUsage` regardless, so the table just
  stays silent (out of scope, follow-up).
- The Python LongMemEval phase 4 harness (`bench/longmemeval/phase4`). It is a
  standalone evaluation tool that owns its own provider/auth and never imports
  Go AI code. It currently reads `api.key` from Ghost's config as a fallback
  (`phase4_run.py`); once Ghost no longer writes/documents `api.key` that
  fallback silently stops matching — accepted, README wording only.
- The `GetTopMemories` pin "boost" comment drift (`internal/memory/store.go`)
  tracked by open task `DCE155185C4CB8D75AEDC448CB58A642` — unrelated to the
  AI tier, left to that task.

## Background

Today the API tier still lives in:

| Path | API usage | Rob |
|---|---|---|
| `ghost reflect --tier haiku` | `ai.NewClient` (main.go:510-516) | delete |
| `ghost reflect` auto tier | API-first, then CLI, then SQLite (main.go:553-556) | delete API branch |
| `ghost resolve`/`ghost supersede` (headless, no `--source`) | `buildClassifyProvider` API-primary, CLI dry-run-only on credit exhaustion (main.go:785-798) | rewrite CLI-only |
| `ghost resolve`/`ghost supersede` stop-hook spawns | inherit the above (stophook.go:225, 305 — no `--source` passed) | rewrite + fix gap |
| Auto-reflect stop-hook guard | `cfg.API.Key` counts as "has an LLM" (stophook.go:340) | rewrite CLI-only |
| Config | `api.key` from `ANTHROPIC_API_KEY`/`GHOST_API_KEY`/YAML (config.go:29,47-50,172-176) | remove |
| `internal/ai` | `Client`, `models.go` API types, `NewAnthropicProvider`, `ErrCreditExhausted`, `FallbackProvider`, `ClassifyResult.FromFallback` | delete |

Already harness-only (the pattern being extended): MCP `ghost_resolve`
(mcpserver.go:1001-1019), every `--source` path via `SourceProvider`
(source_provider.go), and `--tier cli|opencode`.

## Design

### 1. Delete the Anthropic API path

- **`internal/ai/client.go`** — delete (the `Client`, `NewClient`, HTTP `Reflect`,
  `parseAPIError` including its 400-credit mapping).
- **`internal/ai/models.go`** — delete everything except `TokenUsage` (still
  needed by the `reflector` interface, the four CLI clients, `SourceProvider`,
  and `CostTracker`): `APIURL`, `APIVersion`, `BetaInterleavedThinking`,
  `ModelSonnet46/Haiku45/Opus46`, `SystemBlock`, `cacheControl`,
  `CacheControlEphemeral`, `CachedBlock`, `PlainBlock`, `Message`,
  `TextMessage`, `ContentBlock`, `ImageSource`, `apiRequest`.
- **`internal/ai/provider.go`** — delete `ErrCreditExhausted`, `isCreditExhausted`,
  `ClassifyResult`, the `reflectClient` seam note, `anthropicClient`, and
  `NewAnthropicProvider`. `Provider` keeps its `Classify(ctx, systemPrompt,
  userContent) (string, error)` shape. Update the package doc (drop "Anthropic
  HTTP client", the stale "MCP sampling provider", and the `Client` key-type
  listing).
- **`internal/ai/fallback_provider.go`** — delete.
- **`internal/config/config.go`** — remove `APIConfig` and `Config.API`; remove
  the `ANTHROPIC_API_KEY` block in `Load` (lines 171-176); update the package
  doc (lines 4, 7) and the `GHOST_API_KEY → api.key` comment (163). The generic
  `GHOST_*` env provider remains (its `GHOST_API_KEY → api.key` mapping simply
  has no target field).
- **`internal/config/config.example.yaml`** — delete the `# --- Claude API ---`
  section; update the loading-order header (removes "plus ANTHROPIC_API_KEY").
- **`cmd/ghost/main.go` reflect** — remove `case "haiku"` and the API branch in
  the `auto` case. The `--tier` help text (462, and the trap at 1467) becomes
  `auto, cli, opencode, sqlite`.
- Decide unknown-tier handling: a `--tier haiku` argument errors loudly with
  `error: haiku tier removed — Ghost no longer calls the Anthropic API; use auto (default) or cli` rather than silently mapping anywhere.

### 2. Delete the credit-exhaustion seam

- **`internal/resolve/resolution.go`** — the `classifyProvider` interface is
  satisfied directly by `ai.Provider`; `ResolutionClassifier` holds an
  `ai.Provider`; `IsResolved` returns `(resolved bool, err error)` (parses the
  plain text response for the first decisive `resolved`/`keep` token;
  `quoteData` prompt-wrapping unchanged).
- **`internal/resolve/resolve.go`** — `Classifier.IsResolved` drops
  `fromFallback`; `Result` drops `SkippedApply`; `Run` drops the `anyFallback`
  tracking and the withhold block (lines 111-134). Package doc (line 9) no
  longer says "lives in haiku.go".
- **`internal/supersede/relation.go`** — same: classifier holds `ai.Provider`,
  `Classify` returns `(Relation, error)`.
- **`internal/supersede/supersede.go`** — `Classifier` drops `fromFallback`;
  `Run` drops `anyFallback` and the whole-batch apply-skip (lines 319-351).
  Classifier doc comment (68-71) drops the FallbackProvider mention.
- Callers that constructed the seam collapse to raw providers:
  - `buildClassifyProvider` (see §3), MCP `ghost_resolve`
    (`ai.NewSourceProviderForSource(...)` passed straight in),
    `bench/memoryagentbench/classifier.go`
    (`ai.NewOpenCodeClientWithBinary(binary)` passed straight in).
- Tests: delete the fallback-specific cases (`TestRun_FallbackClassification_SkipsApply`,
  `TestRun_MixedFallbackAndPrimary_SkipsApply`, `TestHaikuPropagatesFromFallback`
  in both resolve and supersede), remove the `FromFallback` field from the
  deterministic fakes, and update fake-method signatures.

### 3. Rewire the classifiers (CLI-only)

- **`buildClassifyProvider`** (main.go:785-798) becomes:
  `func buildClassifyProvider(cfg *config.Config) (*ai.CLIProvider, error)`,
  building `ai.NewCLIProviderWithBinaries(...)`, erroring
  `requires a claude/opencode/codex/goose binary (on PATH or via cli.*_binary)`
  with none available. No logger parameter (nothing HTTP left to log).
- **`buildClassifyProviderForSource`** returns `ai.Provider`: source-aware via
  `ai.NewSourceProviderForSource` when `--source` set, else the cascade.
- **reflect** `--tier auto` with `--source` unchanged (source-matched CLI);
  without `--source` becomes CLI-provider cascade → SQLite (unless
  `--require-llm`). `--tier cli` stays claude-only (documented); the cascade is
  what `auto` provides. The `--require-llm` rationale comment (560-566) is
  rewritten without the stale-API-key false-positive framing.
- **stop hook** — `spawnReflectIfConfigured` guard becomes CLI-only: when
  `source` is set, check `SourceProviderForSource(source).Available()`; else
  check the CLI cascade. `cfg.API.Key` never satisfies the guard. Add a
  regression test: `ANTHROPIC_API_KEY` set with no CLI binary ⇒ no reflect spawn.

### 4. Stop-hook `--source` gap fix

`spawnResolveIfConfigured` and `spawnSupersedeIfConfigured` (stophook.go
225, 305) spawn `ghost resolve/supersede <project> --apply` without `--source`,
so the headless auto pass uses the cascade instead of the session's own CLI.
Pass `--source` through when the hook knows it (mirroring
`spawnReflectIfConfigured`), and update the spawn doc comments that still
describe API-credit failure modes (165-167, 245-247, 328).

### 5. Kill the "Haiku" naming in reflection

- Rename `internal/reflection/tier_haiku.go` → `tier_llm.go`; the
  `reflector` interface doc; `HaikuConsolidator` → `LlmConsolidator`.
- Delete `NewHaikuConsolidator` (its only callers were the removed haiku tier
  and a test); keep `NewNamedConsolidator(client, name)` as the single
  constructor; update `consolidator_test.go:560` to use it.
- Update `consolidator.go` package doc and interface docs (lines 3-7, 16-18, 39).

### 6. Docs & comments

- `CLAUDE.md`: reflection line (HaikuConsolidator), resolution-classifier line.
- `README.md`: lines 102 (docker `-e ANTHROPIC_API_KEY`), 150 (network-calls
  list), 177 (pricing), 211 (tiered reflect), 316 (env-var list).
- `docs/architecture.md`: lines 43, 74, 126-127.
- `docs/benchmarks.md`: lines 105, 271.
- `docs/ROADMAP.md`: lines 147-148, 238.
- `internal/ai/cli_client.go` doc (the "ANTHROPIC_API_KEY would otherwise be
  required" framing); `internal/ai/opencode_client.go` comments (keep the env
  stripping — still required so the subprocess bills to subscription — but drop
  "no ANTHROPIC_API_KEY" framing where it implies API support remains);
  `bench/memoryagentbench/classifier.go` comment.
- Mark `docs/superpowers/specs/2026-07-26-classifier-fallback-design.md` and
  `2026-08-19-autonomous-reflect-design.md` superseded by this spec; amend the
  headless row in `2026-08-24-resolve-sampling-path-design.md`.
- Keep the ANTHROPIC_API_KEY scrubbing in `eval/cycle/main.go`,
  `cli_client.go` and `opencode_client.go` — that still prevents the subprocess
  from billing to API credits.

## Files touched

- Delete: `internal/ai/client.go`, `internal/ai/fallback_provider.go`
- Edit: `internal/ai/{provider,models,cli_client,source_provider,opencode_client,codex_client,goose_client}.go`
- Edit: `internal/ai/{client_test,models_test,provider_test}.go` (delete/trim), keep cost/cli/opencode tests
- Edit: `internal/config/{config.go,config.example.yaml,config_test.go}`
- Edit: `internal/reflection/{tier_llm.go,consolidator.go,consolidator_test.go}`
- Edit: `internal/resolve/{resolution.go,resolve.go,resolution_test.go,resolve_test.go}`
- Edit: `internal/supersede/{relation.go,supersede.go,relation_test.go,supersede_test.go}`
- Edit: `cmd/ghost/{main.go,main_test.go}`
- Edit: `internal/mcpinit/{stophook.go,stophook_test.go}`
- Edit: `internal/mcpserver/mcpserver.go` (resolve tool handler)
- Edit: `bench/memoryagentbench/classifier.go`
- Edit: `CLAUDE.md`, `README.md`, `docs/architecture.md`, `docs/benchmarks.md`,
  `docs/ROADMAP.md`, superseded/amended specs

Not touched: `internal/memory/*` (token_usage/cost), `eval/*` scrubbing,
`bench/longmemeval/phase4/*`.

## Tests

- Trim/convert: `internal/ai` client/models/provider tests; `config_test.go`
  api.key asserts; `resolve`/`supersede` fallback tests + fakes;
  `cmd/ghost/main_test.go` (`TestBuildClassifyProvider_*`, the error-message
  assert at 376 updates to the new message).
- Add: stop-hook guard regression (API key without binary ⇒ no reflect spawn);
  `--tier haiku` clean error.
- `go build ./...`, `go vet ./...`, `go test ./...` (with `-count=1`).
- Manual smoke: `ghost resolve <proj>` and `ghost supersede <proj>` dry-runs via
  the opencode harness; `ghost reflect <proj> --dry-run --tier auto` prints the
  CLI tier name; add `--source opencode` and confirm route; confirm `--tier
  haiku` errors cleanly.

## Risks

- **Cascade choice**: with multiple CLI binaries present the cascade order is
  claude→opencode→codex→goose (unchanged from today); users pin with
  `cli.*_binary` or `--source`. The quota-wall gotcha workaround
  (`GHOST_CLI_CLAUDE_BINARY=/nonexistent`) still works.
- **Behavioral remove**: users who only had `ANTHROPIC_API_KEY` and no CLI
  binary lose reflect/resolve/supersede and get a clear error — intended.
- **Auto-reflect guard hardening already removes the stale-key false positive**;
  this change completes that by making the guard CLI-only.
- `token_usage` cost rows stay at `$0` (out of scope).