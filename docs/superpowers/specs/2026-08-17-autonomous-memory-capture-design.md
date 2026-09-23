# Autonomous memory creation: in-session transcript capture

> **Historical record — not current documentation.** This file preserves the
> design or implementation state at the time it was written. For current Ghost
> behavior, start with [`docs/README.md`](../../README.md) and the source.

**Status:** Design approved (2026-08-17). Ready for implementation planning.
**Author:** Wayne (wcatz)
**Builds on:** #297 "feat: autonomous memory lifecycle — auto reflect, supersede, and resolve"

---

## 1. Problem

Ghost has a fully working memory *maintenance* story (reflect/resolve/supersede)
and an explicit memory *creation* path (`ghost_memory_save` etc.), but **creation is
entirely on the assistant to remember**. Nothing creates memories on its own. The
stop hook only *nudges* — it scans the transcript, counts saves, and if a tool-using
turn ends with zero saves it blocks once with "you used tools but saved nothing".
The nudge relies on the model deciding *what* to save, and it can't distinguish
"nothing worth saving" from "five important discoveries the model forgot".

Three concrete gaps:

1. **No autonomous creation.** Memories appear only when the model explicitly calls a
   save tool. A long session that never saves loses everything it learned.
2. **Detail is lost at compaction/session-end.** There is no "save before you forget"
   point. Compaction summarizes context away; session end discards the transcript.
3. **Lifecycle runs on the wrong event.** `spawnResolveIfConfigured` and
   `spawnSupersedeIfConfigured` live in the `Stop` hook, which fires **every turn** —
   their full prologue (config load → `sql.Open` → `ResolveProject` → PID check) runs
   per turn when enabled. That work is semantically session-scoped and belongs on the
   once-per-session `SessionEnd` event.

---

## 2. Core concept: the session's own model reads its own transcript

The pivotal constraint, discovered during research: **MCP sampling only sees what the
server sends.** `ai.SamplingProvider.Classify` passes exactly `systemPrompt +
userContent` (and caps `MaxTokens: 16`); the sampled model has no access to the
conversation context. Therefore autonomous creation must hand the raw material to the
sampling call itself — and that raw material is the **transcript**, which the hook
already knows how to locate (`transcript_path` arrives on every hook's stdin).

The design adds `ghost_capture`: an MCP tool that reads a bounded slice of the current
session's transcript and asks the *calling session's own model* (via MCP sampling, zero
Anthropic credits) to extract memories from it. The tool supplies the transcript slice,
dedup context, and the write gate; the sampling model does the extraction.

**Trigger** is a nudge from the `Stop` hook (which supports `additionalContext`, a
non-error guidance that continues the turn). Because `Stop` fires every turn, capture
runs *as discoveries surface* — by the time compaction or session end arrives, detail is
already in the DB. This is the direct answer to the "pre-compaction vs end-of-session"
question: **neither** — capture per-turn so detail is never at risk.

---

## 3. Mechanism

### 3.1 `ghost_capture` MCP tool

Signature: `ghost_capture(project_id, transcript_path?)`. `transcript_path` is optional;
when omitted the tool reads the pending marker (see §3.3) for the latest session.

Flow:

1. **Locate the transcript.** Prefer the explicit `transcript_path` arg; otherwise read
   the pending marker (`<dataDir>/capture-pending.json`). No marker and no arg → return a
   short "nothing pending" message, not an error.
2. **Resume from the capture cursor.** A per-project state file
   (`<dataDir>/capture-state-<projectID>.json`) records the last-captured transcript
   path and byte offset. Same path → stream from the offset (incremental); different
   path (new session) → stream from 0.
3. **Bound the slice.** Stream from the cursor up to `capture.max_transcript_bytes`
   (default 64 KiB), condensing tool results (large and noisy) to a short elision so the
   slice carries *conversation and tool intent*, not megabytes of file dumps.
4. **Sampling call.** `systemPrompt` = extraction instructions with the JSON schema
   `{"memories":[{"content","category","importance","confidence"}]}` and the category
   enum; `userContent` = the existing project memories (dedup hint) + the condensed
   slice. `MaxTokens` ~2000.
5. **Parse + validate.** Clamp importance/confidence to `[0,1]`, validate category.
6. **Confidence gate.** A candidate is **auto-saved** when
   `confidence ≥ capture.confidence_threshold` **and**
   `importance ≥ capture.importance_threshold`. Everything else is returned as a
   *proposal* for the model to save explicitly. Auto-save reuses the same `Upsert`
   path `ghost_memory_save` uses, so FTS dedup, embedding, linking, and the
   un-resolve-on-write revive all fire; then `notifyProjectResource(projectID, "context")`
   like the other write tools.
7. **Empty-set guard.** Zero candidates → no write, just "nothing new to save".
8. **Commit.** Advance the capture cursor, clear the pending marker, and return a
   summary of saved vs proposed memories.

### 3.2 Refined `Stop` hook

When `capture.enabled`, `ghost hook stop` becomes:

1. `stop_hook_active` → return early (unchanged — the turn is already continuing due to a
   prior hook, so never re-nudge).
2. Scan the transcript as today: count tool calls, `ghost_memory_save`/`ghost_save_global`
   calls, **and** `ghost_capture` calls.
3. No tool calls → return.
4. Saves or captures present → clear the pending marker and return (already recorded).
5. Read the pending marker. If it is for the **same session** and already marked
   `nudged`, emit the original `decision:block` fallback and return.
6. Otherwise write the marker (`session_id`, `transcript_path`, `cwd`, `nudged: true`) and
   emit **`additionalContext`** (not `decision:block`):
   > "This session used tools but saved nothing to Ghost. Call `ghost_capture` to
   > extract discoveries from this session automatically."

The `nudged` flag is what makes "nudge once, then block" work. The immediate follow-up
`Stop` after an `additionalContext` continuation carries `stop_hook_active: true` and
returns at step 1 — so the block only fires on a *later* turn in the same session that
still recorded nothing. Any save or capture clears the marker via step 4, resetting the
cycle for the next burst of unrecorded work. The `stop_hook_active` guard plus Claude
Code's 8-consecutive-continuation cap bound both mechanisms, exactly as today.

### 3.3 `ghost hook session-end` (new event)

New `SessionEnd` hook moves the lifecycle spawns off the per-turn `Stop` path:

- `spawnResolveIfConfigured`, `spawnSupersedeIfConfigured` (and, from #297,
  `spawnReflectIfConfigured`) move from `HandleStopHook` into `HandleSessionEndHook`.
- `SessionEnd` fires once at termination and supports only side effects (no decision
  control) — which is precisely what fire-and-forget detached spawns need. Its 1.5s
  budget comfortably covers the existing read-only project lookup + `exec.Command`
  start; raise it via the hook's `timeout` field if a slow first-run proves otherwise.
- The `Stop` hook keeps the nudge (which genuinely needs per-turn + block/additionalContext);
  the lifecycle keeps its PID-file serialization and detached `--apply` semantics,
  unchanged.

### 3.4 Config

```go
type CaptureConfig struct {
    Enabled             bool    `koanf:"enabled"`               // default TRUE (opt-out)
    ConfidenceThreshold float64 `koanf:"confidence_threshold"`  // default 0.8
    ImportanceThreshold float64 `koanf:"importance_threshold"`  // default 0.5
    MaxTranscriptBytes  int     `koanf:"max_transcript_bytes"`  // default 65536
}
```

`reflection.auto_reflect` (default `false`) lands here per #297, completing the
reflect/resolve/supersede trifecta on `SessionEnd`.

---

## 4. Guardrails (hard)

- **Hooks do zero DB/LLM work.** The `Stop` hook writes a marker and emits text only.
  `SessionEnd` performs the same small read-only project lookup + detached spawn the
  existing spawn helpers already do — no LLM, no write inline.
- **Sampling only, no credits.** `ghost_capture` is constructible only with a live MCP
  session (`req.Session != nil`), mirroring `ghost_resolve`; headless invocation fails
  fast with a clear message. No Anthropic API key is ever touched by capture.
- **No blind writes.** Confidence gate + empty-set guard + existing upsert dedup. A
  low-confidence or empty extraction never mutates the store.
- **Never trap a session.** The nudge is `additionalContext`; the block is a bounded
  fallback; both are loop-protected by `stop_hook_active` and the 8-continuation cap.
- **Untrusted content stays delimited.** Extracted memory text is stored data, and the
  existing `«…»` delimiter guard at injection continues to apply. The extraction prompt
  instructs the model to treat the existing-memories block as data, not instructions.
- **Single-active-session marker.** `capture-pending.json` is one well-known file; two
  concurrent foreground sessions in different projects race on it (last-writer-wins).
  The explicit `transcript_path` arg is the escape hatch. Acceptable for a single-user
  local tool — documented here rather than silently ignored.

---

## 5. Data flow

```
Stop hook fires (per turn)
  → scan transcript (toolCalls, saves, captures)
  → toolCalls>0 && saves==0 && captures==0 ?
       → marker already marked `nudged` for this session? → decision:block (fallback)
       → else → write marker {session_id, transcript_path, cwd, nudged:true}
              → additionalContext: "call ghost_capture"

Model calls ghost_capture(project_id)
  → read marker → transcript_path
  → read capture cursor (byte offset)
  → stream bounded slice from offset
  → sampling(extraction prompt + existing memories + slice)
  → parse candidates
  → gate: auto-save high-conf/high-importance; propose rest
  → advance cursor, clear marker, report

SessionEnd hook fires (once)
  → spawn detached reflect --apply / resolve --apply / supersede --apply (PID-serialized)
```

---

## 6. Components & boundaries

| Unit | Purpose | Depends on |
|---|---|---|
| `internal/capture` | transcript slicing, prompt build, JSON parse, confidence gate | `internal/ai` (sampling), `internal/memory` (types) |
| `ai.SamplingProvider.Extract` | configurable-`MaxTokens` sampling call (vs 16-token `Classify`) | MCP `CreateMessage` |
| `ghost_capture` tool handler | wires store + sampling + capture pkg; marker/cursor read-write | `internal/capture`, `internal/mcpserver` |
| `HandleStopHook` refine | marker write + additionalContext nudge + block fallback | `internal/mcpinit/stophook.go` |
| `HandleSessionEndHook` (new) | detached reflect/resolve/supersede spawns | `internal/mcpinit` |
| `cmd/ghost` hook wiring | `ghost hook session-end` subcommand + init/status entries + `mcp__ghost__ghost_capture` permission allowlist | `internal/mcpinit` |
| `internal/config` | `capture.*` + `reflection.auto_reflect` | koanf defaults |

Each unit is independently testable: the capture package against a fixture transcript;
the tool handler against a fake sampler + seeded store (mirroring the existing
`ghost_resolve` test); the stop/session-end hooks against canned stdin JSON.

---

## 7. Rejected alternatives

- **`PreCompact` trigger** — the natural "save before you forget" point, but the event
  supports only `decision: "block"` (it discards `systemMessage`/`continue` and has no
  `additionalContext`), so it cannot nudge the model to call a capture tool. Worse,
  blocking auto-compaction triggered by an already-returned context-limit error makes the
  current request fail. Deferred; a future "block manual `/compact` to remind the user"
  remains possible but is not autonomous.
- **Detached Anthropic-API capture** (paid, works without a live session) — rejected in
  favor of free in-session sampling; a headless path could be added later behind
  `capture.provider` without disturbing this design.
- **Model-driven notes** (`ghost_capture(notes=…)` where the model summarizes) — higher
  signal-to-noise but still depends on the model's diligence, defeating the autonomy
  goal. Transcript-driven was chosen.
- **Daemon/cron for lifecycle** — out of scope; noted as future in #297.
- **Keeping lifecycle on `Stop`** — per-turn prologue overhead is the bug this design
  fixes; `SessionEnd` is the semantically correct event.

---

## 8. Testing

- **Capture package** — fixture transcript: bounding at `max_transcript_bytes`, cursor
  resume across calls, tool-result elision, prompt construction, JSON parse, category/
  range validation, confidence gate (auto-save vs proposal split), empty-set guard.
- **Tool handler** — fake sampler + seeded store: saved list correct, proposals returned,
  cursor advanced, marker cleared; `req.Session == nil` fails fast.
- **Stop hook** — canned stdin: marker written; nudge emitted on `toolCalls>0, saves==0,
  captures==0`; pass-through on saves/captures; block fallback on the second fire; silent
  on `stop_hook_active`.
- **Session-end hook** — detached spawns gated by PID file; no spawn when disabled or no
  matching project.
- **Config** — `capture.*` defaults (`enabled=true`, thresholds 0.8/0.5); `auto_reflect`
  default `false`.
- `go vet ./...` before commit; feature branch + PR.
