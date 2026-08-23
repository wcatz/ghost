# Normalized host-event contract + plugin-first distribution

**Status:** Draft (2026-08-23).
**Author:** Wayne (wcatz)
**Builds on:** #345 "Wire the stop hook into opencode (currently Claude Code-only)"; `2026-08-20-ghost-claude-plugin-design.md`; `2026-08-17-autonomous-memory-capture-design.md`.

## 1. Problem

Ghost's memory engine (`internal/memory`, `internal/reflection`, `internal/resolve`,
`internal/supersede`, `internal/linking`, `internal/embedding`) is client-agnostic —
but nothing else is. Every integration surface hardcodes Claude Code:

1. **Hook input shape.** `HandleStopHook` (`internal/mcpinit/stophook.go:66`) parses
   Claude Code's stdin schema (`transcript_path`, `stop_hook_active`, `cwd`) and
   answers with the proprietary `{"decision":"block"}` protocol.
2. **Transcript format.** `scanTranscript` (`stophook.go:104`) parses Claude Code's
   assistant/tool_use JSONL. No other client's transcript can feed the pipeline.
3. **Config mutation per client.** `ensureStopHook`/`ensureHook` write Claude Code's
   settings.json; `RunOpencode` only merges an MCP entry and installs *nothing* at
   session boundaries — which is exactly why opencode sessions never reflect,
   resolve, supersede, or get the save nudge (#345).
4. **Capability assumptions.** The pipeline assumes hooks can block a stop, that a
   transcript file path always exists, and that project == cwd on a local repo.

The result: adding a client means core surgery, and "install ghost" means something
different on every host.

## 2. Core concept: hosts are thin adapters over one contract

Invert the dependency. Ghost defines **one normalized event contract**; each host
ships a small out-of-tree artifact (plugin, hook config, shim) that translates its
native events into that contract. Ghost core learns zero new formats per client.

### 2.1 The contract

```text
ghost hook <event> --source <host> [--version 1]
```

with a versioned JSON payload on stdin:

```json
{
  "contract": 1,
  "source": "claude-code | opencode | codex",
  "event": "session-start | stop | session-end",
  "session_id": "…",
  "cwd": "/abs/path",
  "stop_hook_active": false,
  "transcript": {
    "format": "claude-jsonl | opencode-messages | codex-rollout | none",
    "path": "~/.claude/projects/….jsonl",
    "inline_jsonl": ""
  },
  "capabilities": { "block_stop": true, "inject_context": true }
}
```

Rules:

- **CLI args are authoritative; payload must agree.** `event` and `source` exist in
  both the argv and the payload so the payload is self-describing in logs, but
  dispatch validates equality (and `contract == 1`) *before* anything else. Any
  mismatch — including a stale adapter sending `"source": "claude-code"` with
  `--source opencode` — is rejected as fail-open (log line, empty stdout, exit 0).
  Routing never consults a field that failed validation.
- **Output protocol is capability-scoped, enforced by a source matrix.** Blocking
  output exists only for sources whose v1 capability grants it. The v1 matrix:

  | source | block_stop | inject_context | nudge on save-less stop |
  | --- | --- | --- | --- |
  | claude-code | yes | yes | yes |
  | opencode | no | no | log-only |
  | codex | no | no | log-only |

- **Executable outcome table.** Every path has defined stdout / stderr / exit code:

  | outcome | stdout | stderr | exit |
  | --- | --- | --- | --- |
  | block (claude-code, eligible) | `{"decision":"block","reason":…}` | — | 0 |
  | context injection (`session-start`) | host-visible injection text (Claude: system-reminder block) | — | 0 |
  | normal completion (spawns done, nothing to say) | empty | — | 0 |
  | nudge on non-blocking host | empty | one log line | 0 |
  | parse error / contract mismatch / unknown source / missing transcript | empty | one log line | 0 |

  Fail-open is absolute and **always exits 0** — a nonzero exit could itself be
  read by hosts as hook failure; ghost treats "allow the stop" as the only
  failure response it ever emits.
- **Transcripts are adapter-normalized where the adapter can.** opencode's plugin has
  typed SDK access (`client.session.messages`), so it materializes a temp JSONL in the
  `opencode-messages` format and passes the path. Claude Code forwards its native
  `transcript_path`. Ghost selects a scanner by `format`, never by `source`.
- **Backward compatible by default.** Bare `ghost hook stop` (no `--source`) keeps
  parsing the exact Claude Code stdin shape shipped today — existing installs,
  the Claude plugin spec, and all tests are untouched.

### 2.2 v1 event handlers

All three declared events ship with defined behavior from Phase 0:

| event | handler | behavior |
| --- | --- | --- |
| `session-start` | wraps today's `HandleSessionStartHook` logic (`internal/mcpinit/hook.go:105`) | read-only project-context load + globals + obsidian sync kick; subagent gate and resume/compact short-circuits preserved verbatim |
| `stop` | wraps today's `HandleStopHook` logic | capability-gated spawns (resolve/supersede/reflect) + save-nudge when `block_stop` granted |
| `session-end` | spawns-only variant of `stop` | lifecycle spawns run; **no nudge** — designed for hosts that emit once per real session end rather than per turn |

### 2.3 Why adapters live outside ghost core

A resident daemon was considered and rejected for now: the current spawn-per-event
model with flock-serialized PID claims (`claimPidFile`) is proven, dependency-free,
and matches hook lifecycles. Adapters are ~50–100 lines each, fail independently,
and can be distributed per-host (npm package for opencode, marketplace entry for
Claude Code, TOML snippet for codex) without a ghost release.

## 3. Distribution end-state ("all of it as a plugin")

| Host | Session-start injection | Lifecycle trigger (stop/idle) | MCP server | Artifact |
| --- | --- | --- | --- | --- |
| Claude Code | SessionStart hook → `ghost hook session-start --source claude-code` | Stop hook → contract | stdio via plugin `.mcp.json` | Plugin (per `2026-08-20` spec) |
| opencode | MCP instructions block today; optionally `ctx.session.hook("context", …)` later | JS plugin on `session.status`(idle) / `session.idle` → contract | **config-file merge stays** (plugins cannot register MCP servers) | npm package + `mcp init --client opencode` writes both entries |
| codex | none (no injection surface) | `notify = […]` shim on `agent-turn-complete` → contract | `[mcp_servers.ghost]` TOML merge | `mcp init --client codex` + shim script |

Honest constraints, stated up front:

- **Blocking is Claude Code-only.** The save-nudge degrades to a no-op elsewhere;
  do not fake it. Injection surfaces differ per host; the capabilities block makes
  degradation explicit rather than silent.
- **opencode's `session.idle` is deprecated but still emitted**; adapters listen on
  `session.status` (status → idle transition) with `session.idle` as fallback.
- **Claude Code's Stop fires every turn**, other hosts fire closer to true session
  end. This asymmetry already exists and is harmless because every spawned worker
  (`resolve`/`supersede`/`reflect`) is idempotent under PID claims; the contract
  carries both `stop` and `session-end` so hosts declare what they emit.

## 4. Implementation plan

### Phase 0 — contract + Claude Code parity (this PR's scope, code to follow)

- New `internal/hostevent` package: payload struct, validation (contract version,
  argv/payload equality per §2.1), capability matrix, outcome table.
- `HandleStopHook` / `HandleSessionStartHook` refactored into: parse contract →
  validate → dispatch per §2.2's event table. The Claude Code path becomes an
  adapter producing the same structs it produces today, so all three v1 events are
  handled from day one with zero behavior change; existing tests green unchanged.
- Transcript scanner registry keyed by `format`; `claude-jsonl` is v1's only entry.
- `ghost mcp status` gains `--client` awareness groundwork (report which sources
  have wiring present).

### Phase 1 — opencode adapter (closes #345)

- **Scanner first:** add the `opencode-messages` transcript scanner and its golden
  fixtures to the registry *before* any adapter lands — an adapter that invokes the
  contract successfully but fails open on an unsupported format would silently
  disable reflection/resolve/supersede for opencode users.
- `plugin/ghost-opencode/` in-repo: TypeScript plugin listening for
  `session.status`→idle transitions; spawns `ghost hook stop --source opencode`
  with SDK-fetched messages serialized to a temp JSONL.
- `RunOpencode` extended: also install the plugin file under
  `~/.config/opencode/plugin/` (idempotent, versioned header comment) alongside the
  MCP merge done today.
- Status check: verify plugin file present + mcp entry (mirrors `hasStop`).

### Phase 2 — codex adapter

- `RunCodex`: merge `[mcp_servers.ghost]` into `~/.codex/config.toml` + install
  notify shim invoking the contract with `capabilities.block_stop=false`.
- Nudge degrades to log-only; document it.

### Phase 3 — distribution polish

- npm publish of the opencode plugin; Claude plugin marketplace entry per the
  existing spec; evaluate Gemini CLI extensions (bundles MCP + context files) as a
  fourth adapter.

## 5. Non-goals

- No changes to memory schema, ranking, reflection, resolve, or supersede semantics.
- No resident daemon / HTTP API (`ghost serve`) — tracked separately; this contract
  is deliberately shaped so a future daemon can expose the same events over a socket.
- No implementation of `ghost_capture` transcript extraction here; the contract's
  transcript plumbing is designed to be its supply line, not its implementation.
- No Windows-specific adapter work beyond what the existing hook quoting handles.

## 6. Testing

- Contract conformance fixtures: one golden stdin payload + expected stdout per
  (host × event × capability) combination.
- Adapter tests live next to each adapter; core tests never read host formats
  except through the scanner registry.
- `go test ./...` + `golangci-lint v2.12.2` gates as usual; Phase 1 additionally
  verified by driving a real opencode session end-to-end on a scratch config dir
  (see the opencode subprocess-isolation gotcha in project memory).
