# Autonomous Reflect — Design

> **SUPERSEDED (2026-09-12):** the Anthropic API / FallbackProvider credit-exhaustion seam is removed wholesale — see `2026-09-12-harness-only-memory-management-design.md`. The file is kept for history.

**Status:** Implemented (2026-08-19).
**Author:** Wayne
**Builds on:** #297 "feat: autonomous memory lifecycle — auto reflect, supersede, and resolve"; #312 "fix(resolve): fall back to claude CLI when MCP sampling is unavailable".

## Context

`ghost reflect` consolidates a project's memories through a tiered consolidator
(`internal/reflection/consolidator.go`): an LLM tier (direct Anthropic API when
`ANTHROPIC_API_KEY` is set, or `claude -p` when the `claude` binary is on PATH),
falling back to a mechanical Jaccard dedup tier (`internal/reflection/tier_sqlite.go`)
that can only merge near-duplicates, not summarize, restructure, or re-scope.

On this machine the LLM path is unreachable: no `ANTHROPIC_API_KEY`, no `claude`
binary, and no Ollama (so embeddings are also off). The only LLM runtime present is
the `opencode` CLI (`~/.opencode/bin/opencode`), whose `run --format json` subcommand
emits a JSON-lines stream (`step_start` / `text` / `step_finish` events) that carries
the model's answer in the `text` events. MCP sampling is not an option either —
opencode's client does not implement `sampling/createMessage` (tracked as
`anomalyco/opencode#11948`), the same reason `ghost_resolve` gained a CLI fallback
in #312.

Separately, `ghost reflect` has no autonomous trigger. `ghost resolve` and
`ghost supersede` both spawn detached `--apply` processes from the Stop hook
(`spawnResolveIfConfigured` / `spawnSupersedeIfConfigured` in
`internal/mcpinit/stophook.go`, gated by `reflection.auto_resolve` /
`reflection.auto_supersede`). Reflect — the third leg of #297's lifecycle — has no
equivalent, so consolidation only happens when a user runs `ghost reflect` by hand.

This spec closes both gaps: a generalized subprocess-LLM client that can drive either
`claude` or `opencode`, and an `auto_reflect` Stop-hook spawn mirroring the two that
already exist.

## Goals

- Add an `opencode` LLM path for consolidation, generalized alongside the existing
  `claude` path behind one resolver so `ghost reflect` "just works" on machines that
  have either binary.
- Wire the resolver into the reflect tier system so `--tier auto` prefers haiku (API)
  → CLI (claude → opencode) → sqlite, and so `--tier cli` / `--tier opencode` select a
  specific backend explicitly.
- Add `reflection.auto_reflect` (default `false`) and a `spawnReflectIfConfigured`
  Stop-hook spawn that mirrors resolve/supersede exactly: pid-file serialization,
  detached `ghost reflect <project> --apply`, output to `reflect.log`.
- Make autonomous reflect a no-op when no real LLM tier is available, so it never
  fires a Jaccard-only consolidation (which would rewrite every non-manual memory for
  no quality gain).

## Non-goals

- No change to resolve/supersede. #312 already gave them a sampling→claude fallback;
  this spec does not add opencode to their fallback chain (the generalized client is
  usable there later, but nothing here rewires them).
- No in-session capture. `ghost_capture` / transcript extraction is a separate design
  (`docs/superpowers/specs/2026-08-17-autonomous-memory-capture-design.md`) on its own
  branch.
- No `SessionEnd` hook. The capture spec proposes relocating the lifecycle spawns to a
  once-per-session event; this spec deliberately mirrors the existing Stop-hook pattern
  instead, and defers any relocation to that work.
- No embedding improvement. Ollama is not running here, so embeddings are out of
  scope. The sqlite tier's token-overlap dedup IS improved in place (see §5) and
  remains the final fallback.
- No new config surface beyond `reflection.auto_reflect` and the two `cli.*`
  binary-path overrides (`cli.claude_binary` / `cli.opencode_binary`). Backend
  selection is availability-driven (PATH), with explicit `--tier` overrides and
  the binary-path config for installs that aren't on the hook process's PATH.

## Design

### 1. Generalized subprocess-LLM client (`internal/ai/`)

Two sibling backends implement the existing `reflector` interface
(`internal/reflection/tier_haiku.go`) and the `Provider` interface
(`internal/ai/provider.go`):

- `CLIClient` (existing) — drives `claude -p`, plain-text stdout.
- `OpenCodeClient` (new) — drives `opencode run --format json --pure <prompt>`,
  parses the JSON-lines stream, and concatenates the `text` events' `part.text`
  fields into the answer.

A resolver picks the best available backend and exposes it behind the same two
interfaces, so the reflect tier treats `CLIProvider` like any other consolidator
client:

```go
// CLIProvider reports the best available CLI backend and delegates to it.
// It satisfies reflection's reflector interface and ai.Provider, so it drops in
// wherever CLIClient is used today. The selected backend's name is stored on the
// struct, not on the interface, so Name() reports which binary was resolved.
type CLIProvider struct {
    backend cliBackend
    name    string
}

// cliBackend is the narrow capability the two CLI clients share: the exported
// Reflect/Classify shapes each already implements.
type cliBackend interface {
    Reflect(ctx context.Context, prompt string) (string, TokenUsage, error)
    Classify(ctx context.Context, systemPrompt, userContent string) (string, error)
}

func NewCLIProvider() *CLIProvider            // claude if on PATH, else opencode if on PATH, else unavailable
```

`CLIProvider` implements `Reflect` (returns `TokenUsage{}`, like `CLIClient`, since
subscription calls carry no per-token cost) and `Classify`. `OpenCodeClient.Classify`
inlines `systemPrompt` into the message because `opencode run` has no
`--system-prompt` flag — the same join `anthropicClient.Classify` already performs.
Explicit backend selection is `--tier cli` / `--tier opencode`, which construct
`NewCLIClient()` / `NewOpenCodeClient()` directly; the resolver is only for `auto`.

`OpenCodeClient` and the resolver return an explicit "unavailable" signal when the
binary is missing rather than erroring mid-call, so tier selection and the
no-LLM guard below can decide cheaply without spawning.

### 2. Reflect tier wiring (`cmd/ghost/main.go`)

The reflect command's tier switch gains an `opencode` case:

- `--tier haiku` — unchanged (requires `ANTHROPIC_API_KEY`).
- `--tier cli` — unchanged (requires `claude` on PATH).
- `--tier opencode` — new (requires `opencode` on PATH).
- `--tier sqlite` — unchanged.
- `--tier auto` (default) — haiku if a key is set, then `CLIProvider` (claude →
  opencode) if any CLI is on PATH, then sqlite as the always-available fallback.

The usage string and any tier-name validation are updated to include `opencode`.

Backend resolution honors two optional config keys — `cli.claude_binary` and
`cli.opencode_binary` (both default empty = PATH lookup). When set, the explicit
path wins over `exec.LookPath`, so a binary installed under `~/.opencode/bin`
(or similar) is found even though that directory is not on the Stop hook
process's PATH. This is what keeps `auto_reflect` from silently never firing on
machines where the CLI lives only on the interactive shell's PATH.

### 3. `reflection.auto_reflect` (`internal/config/config.go`)

Add `AutoReflect bool `koanf:"auto_reflect"`` to `ReflectionConfig`, with a
`"reflection.auto_reflect": false` default in the `defaults` map — matching
`auto_resolve` / `auto_supersede`.

### 4. `spawnReflectIfConfigured` (`internal/mcpinit/stophook.go`)

Mirrors `spawnResolveIfConfigured` / `spawnSupersedeIfConfigured` step for step:

1. Return silently unless `cfg.Reflection.AutoReflect` is set.
2. Resolve the project from `cwd` (same read-only lookup + `ResolveProject`).
3. Guard on a `reflect-<projectID>.pid` file via the same `isAlive` / `claimPidFile`
   serialization (so only one reflect runs per project at a time).
4. Spawn detached `ghost reflect <project> --apply`, stdout/stderr to `reflect.log`.

One addition over the resolve/supersede twins: the **no-LLM guard**. Before spawning,
the hook checks whether any real LLM tier is available — `ANTHROPIC_API_KEY` set,
`claude` on PATH, or `opencode` on PATH. If none is, it returns silently without
spawning (optionally logging one line to `reflect.log`). This keeps autonomous reflect
from ever running a Jaccard-only `--apply` that would rewrite non-manual memories with
no consolidation quality gained.

### 5. Jaccard improvement (`internal/reflection/tier_sqlite.go`)

The sqlite tier is the fallback when no LLM is available, so its dedup quality matters.
The current Jaccard approach has three concrete weaknesses, each fixed in place:

1. **Noisy tokens.** `tokenize` keeps filler words ("the", "for", "with", "and",
   "is", …), which dilute the score on longer memories. Fix: drop a small stopword
   set in `tokenize`.
2. **Subset memories under-merge.** Jaccard is symmetric and length-sensitive, so a
   short memory that is a strict subset of a longer one ("use SQLite" vs "use SQLite
   for storage with FTS5") scores low and never merges. Fix: score by
   `max(jaccard, containment)` where `containment = |A∩B| / min(|A|,|B|)` (overlap
   coefficient), so a subsumed memory merges into its superset.
3. **Numeric differences over-merge.** Two memories that differ only in a number
   ("k3s runs Grafana on port 80" vs "… port 81") share enough non-numeric tokens to
   cross the 0.5 threshold and get wrongly merged. Fix: block a merge when the
   symmetric difference contains a purely-numeric token — a precise fact that differs
   is a different fact.

The merge threshold (0.5) and the same-category restriction are unchanged. This is a
self-contained, dependency-free change with its own tests; it does not touch the LLM
tiers.

## Guardrails

- **Hooks do zero DB/LLM work inline.** The spawn does only the small read-only
  project lookup it already does for resolve/supersede, then forks a detached child.
  The LLM call and the write happen in the child, exactly like today.
- **No LLM, no spawn.** See §4 — the guard is the one behavioral difference from the
  resolve/supersede pattern, and it is the point of this feature.
- **Recursion isolation for the opencode subprocess.** The detached `ghost reflect`
  spawns `opencode run`; that subprocess must not load Ghost's own MCP server or
  hooks (which would open the same SQLite DB mid-consolidation or let the subprocess
  model call ghost tools). `OpenCodeClient` achieves this by: running with `--pure`
  (no plugins); setting `cmd.Dir` to a neutral temp dir (no repo CLAUDE.md/git
  context or project config); and pointing `XDG_CONFIG_HOME` at a fresh empty dir so
  the user's global opencode config — which declares the Ghost MCP server — never
  loads. It also strips `ANTHROPIC_API_KEY` (parity with `CLIClient`) and never passes
  `--continue`/`--session`. Trade-off: the child runs on opencode's built-in default
  model rather than the user's configured `model` key; acceptable for consolidation,
  and worth revisiting if a model override is ever wanted.
- **Opt-in destructive write.** `auto_reflect` defaults to `false`; the `--apply` it
  spawns runs `ReplaceNonManual`, the same destructive replace resolve/supersede
  already gate behind opt-in config. Pinned and manual memories are excluded by the
  #295 fix already in main.
- **Resolved memories survive reflect (issue #318).** `ghost resolve --apply` de-weights
  resolved-evidence memories via `resolved_at`; reflect must not undo that verdict.
  `ReplaceNonManual`/`RestoreSnapshot` exclude `resolved_at IS NOT NULL` rows from their
  delete/snapshot/preserve queries, and reflect drops resolved memories from its
  consolidation input so the consolidator can't re-emit them as fresh unresolved
  duplicates. Without this, the #297 lifecycle would compose incorrectly: the
  reflect → resolve → supersede stop-hook ordering would clobber the previous
  session's resolve before it re-ran.

## Data flow

```
Stop hook fires (per turn, Claude Code)
  └─ HandleStopHook
       ├─ spawnResolveIfConfigured(cwd)     (unchanged)
       ├─ spawnSupersedeIfConfigured(cwd)   (unchanged)
       └─ spawnReflectIfConfigured(cwd)     (new)
            ├─ auto_reflect off → silent return
            ├─ no LLM on PATH/key → silent return (no Jaccard-only run)
            └─ else claim pid file → detach `ghost reflect <project> --apply`
                 └─ reflect --tier auto
                      └─ haiku (key) → CLIProvider (claude → opencode) → sqlite
```

## Testing

- `OpenCodeClient` — a fake `opencode` binary on PATH emitting fixture JSON-lines:
  text is concatenated correctly across multiple `text` events; non-JSON or truncated
  output errors cleanly; a missing binary reports unavailable; a stalled subprocess
  honors the caller's context deadline (mirroring `CLIClient`'s `defaultTimeout`
  behavior).
- `CLIProvider` — prefers `claude` over `opencode` when both are present; falls back
  to opencode when claude is absent; reports unavailable when neither is; the explicit
  backend constructor errors on an absent binary.
- `spawnReflectIfConfigured` — config-gate (off → no spawn), no-LLM guard (no key and
  no CLIs → no spawn), pid-file claim (second concurrent call skips), and a successful
  spawn path, mirroring the existing resolve/supersede hook tests.
- Reflect CLI — `--tier opencode` selects the opencode backend; `--tier auto` resolves
  to the expected tier given each availability combination; a dry-run e2e with a fake
  opencode binary returns parsed memories without writing.
- Jaccard improvement — stopword tokens are excluded from `tokenize`; a strict-subset
  memory merges into its superset via the containment score; two memories differing
  only in a numeric token do not merge; existing dedup tests still pass unchanged.
- `go vet ./...` and `go test ./...` before and after, feature branch + PR.

## Rejected alternatives

- **opencode as the only new client (no resolver).** Rejected — the user wants one
  generalized path that works whether a machine has `claude` or `opencode`, not a
  hard-coded preference for one.
- **Config-driven backend selection (`reflection.llm: claude|opencode|auto`).**
  Rejected — availability on PATH is enough; an extra key is surface to manage for no
  gain, and `--tier` already provides explicit override when needed.
- **Embedding-based dedup instead of token overlap.** Rejected — Ollama isn't running
  here, and embeddings are a heavier, config-dependent change than the LLM tier this
  spec adds. The token-overlap dedup is improved in place (§5) instead.
- **Trigger on `SessionEnd`.** Deferred — correct for a heavy once-per-session pass,
  but it is new hook-event work already specced in the capture design; this change
  mirrors the existing Stop-hook pattern to stay focused.
- **Reuse `ghost_resolve`'s sampling→claude fallback for reflect.** Rejected —
  reflect runs headless from a detached child process, where there is no MCP session to
  sample from; the CLI subprocess is the only LLM path available there.
