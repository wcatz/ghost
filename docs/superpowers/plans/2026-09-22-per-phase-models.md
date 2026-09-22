# Per-Phase Harness Model Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let each lifecycle phase pin its own harness model — a small/fast model (`opencode/big-pickle`) for supersede's crisp verdicts (validated), provisionally for resolve (unmeasured), the strong default for reflect's consolidation.

**Architecture:** Three `cli.model_*` config fields; each phase subcommand sets `GHOST_OPENCODE_MODEL` (read per invocation by `ai.OpenCodeClient`, `internal/ai/opencode_client.go`) when its configured model is non-empty. Each lifecycle phase runs in its own process, so the env cannot leak across phases; empty config leaves any inherited value untouched. Model pinning applies to the opencode harness only (the other clients have no model flag).

**Tech Stack:** Go 1.26, `internal/config` (koanf), `cmd/ghost` phase entrypoints.

---

## Background and measurement (task B1F8C738)

`opencode/big-pickle` measured on the supersede labeled set: 10/10 and 9/10 single-pair across two live runs (the miss was the subtle NATS CAUSES case — run-to-run variance) and 10/10 batched at batchSize 3. `opencode/claude-haiku-4-5` is NOT entitled (403 "Model access is disabled") — do not use or reference haiku as a candidate.

**Provisional for resolve:** those measurements are supersede-only, and batched at batchSize 3 while supersede ships at 8. Resolve adjudicates with a KEEP-biased conclusion-vs-evidence rubric, which is unmeasured on a small model — pin resolve to big-pickle only experimentally.

## File structure

| File | Change |
|---|---|
| `internal/config/config.go` | `CLIConfig` gains `ModelReflect`, `ModelResolve`, `ModelSupersede` |
| `internal/config/config_test.go` | parse + default tests |
| `internal/config/config.example.yaml` | document the three keys |
| `cmd/ghost/main.go` | `applyPhaseModel` helper + call in `runReflect`/`runResolve`/`runSupersede` |
| `cmd/ghost/main_test.go` | helper tests |
| `internal/ai/cli_client.go`, `internal/ai/opencode_client.go` | stale `tier_haiku.go` comment refs → `tier_llm.go` |
| `CLAUDE.md` | CLI bullet documents per-phase models |

**Worktree:** `.worktrees/phase-models` on `feat/phase-models`, based on `origin/main` at `27ec8c6`.

---

### Task 1: Config fields

- [ ] **Step 1:** add to `CLIConfig` (internal/config/config.go:52):

```go
	ClaudeBinary   string `koanf:"claude_binary"`
	OpenCodeBinary string `koanf:"opencode_binary"`
	CodexBinary    string `koanf:"codex_binary"`
	GooseBinary    string `koanf:"goose_binary"`

	// Per-phase harness model pins for the opencode backend (e.g.
	// "opencode/big-pickle"). Empty means "use the harness default". Each
	// lifecycle phase runs as its own process, so a pin cannot leak between
	// phases. Ignored by the claude/codex/goose clients, which have no model
	// flag.
	ModelReflect   string `koanf:"model_reflect"`
	ModelResolve   string `koanf:"model_resolve"`
	ModelSupersede string `koanf:"model_supersede"`
```

- [ ] **Step 2:** config_test: YAML `cli: {model_reflect: opencode/big-pickle, model_resolve: opencode/big-pickle}` parses into the fields; defaults are empty strings (mirror the existing `cli.*_binary` test style).
- [ ] **Step 3:** config.example.yaml: document the three keys under `cli:` with a comment that they apply to the opencode backend only, empty = harness default.
- [ ] **Step 4:** `go test ./internal/config/ -count=1`; commit `feat(config): per-phase harness model pins`.

---

### Task 2: Wiring + helper + comment cleanup

- [ ] **Step 1:** helper in cmd/ghost/main.go (near `buildClassifyProvider`):

```go
// applyPhaseModel pins the opencode harness model for this phase process when
// the config sets one. ai.OpenCodeClient reads GHOST_OPENCODE_MODEL per
// invocation; each lifecycle phase runs as its own process, so setting it here
// cannot leak across phases. An empty config leaves any inherited value alone.
func applyPhaseModel(model string) {
	if model != "" {
		_ = os.Setenv("GHOST_OPENCODE_MODEL", model)
	}
}
```

- [ ] **Step 2:** call it right after `bootstrap(...)` in each phase entrypoint, before the provider/consolidator is built:
  - `runReflect`: `applyPhaseModel(cfg.CLI.ModelReflect)`
  - `runResolve`: `applyPhaseModel(cfg.CLI.ModelResolve)`
  - `runSupersede`: `applyPhaseModel(cfg.CLI.ModelSupersede)`
- [ ] **Step 3:** main_test:

```go
func TestApplyPhaseModel(t *testing.T) {
	t.Setenv("GHOST_OPENCODE_MODEL", "inherited/model")
	applyPhaseModel("")
	if got := os.Getenv("GHOST_OPENCODE_MODEL"); got != "inherited/model" {
		t.Errorf("empty config must leave the inherited value, got %q", got)
	}
	applyPhaseModel("opencode/big-pickle")
	if got := os.Getenv("GHOST_OPENCODE_MODEL"); got != "opencode/big-pickle" {
		t.Errorf("config must pin the model, got %q", got)
	}
}
```
- [ ] **Step 4:** fix the two stale comments (`internal/ai/cli_client.go:48`, `internal/ai/opencode_client.go:36`): `internal/reflection/tier_haiku.go` → `internal/reflection/tier_llm.go`.
- [ ] **Step 5:** `gofmt -l cmd/ghost/ internal/ai/` clean; `go vet ./...`; `go test ./cmd/... ./internal/ai/ -count=1`; commit `feat(lifecycle): per-phase harness model pins`.

---

### Task 3: Docs, full suite, PR

- [ ] **Step 1:** CLAUDE.md CLI bullet: add that `cli.model_reflect` / `model_resolve` / `model_supersede` pin the opencode harness model per phase (empty = default).
- [ ] **Step 2:** `go vet ./... && go test ./... -count=1` (live tests run, not skipped).
- [ ] **Step 3:** commit `docs: document per-phase model pins`; push; open PR:

```
gh pr create --title "feat(lifecycle): per-phase harness model pins" --body "Reflect, resolve, and supersede all ran on the same harness model. This adds cli.model_reflect / cli.model_resolve / cli.model_supersede: each phase subcommand pins GHOST_OPENCODE_MODEL for its own process when configured (empty = harness default, ignored by claude/codex/goose which have no model flag). Measured on the labeled supersede set: opencode/big-pickle scores 10/10 and 9/10 single-pair across two live runs (variance on the subtle CAUSES case) and 10/10 batched, so it is a viable small-model tier for supersede's crisp verdicts while reflect keeps the strong default; applying it to resolve is provisional (unmeasured). Related: Ghost task B1F8C738."
```
- [ ] **Step 4:** `gh pr checks --watch`; do not merge without the user.

---

## Self-review

**Spec coverage:** config (Task 1), wiring/cleanup (Task 2), docs/PR (Task 3). Reflect keeps the strong default (its pin is available but unset by default); resolve/supersede can be pinned to big-pickle by config.

**Type consistency:** `ModelReflect/ModelResolve/ModelSupersede` (koanf `model_reflect/model_resolve/model_supersede`), `applyPhaseModel(string)`, env `GHOST_OPENCODE_MODEL` used consistently.
