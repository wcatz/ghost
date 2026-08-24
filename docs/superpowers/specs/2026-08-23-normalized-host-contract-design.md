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

### 2.0 Grounding: read against the actual specs (researched 2026-08-23, re-verified 2026-08-24)

Primary sources consulted: the MCP specification (current revision **2026-07-28**;
prior revisions 2025-06-18 and 2025-11-25 were checked during initial research),
the Agent Plugins Specification v1.0.0 (agent-plugins.org), Claude Code's hooks
reference, OpenAI Codex's hooks + config reference, goose's hooks documentation
and announcement post, and opencode's plugin docs/issues. Findings that shape
this design:

1. **MCP has no host-conversation lifecycle — even less than first researched.**
   The current revision, 2026-07-28, made the protocol stateless per-request:
   it removed the `initialize`/`initialized` handshake entirely (SEP-2575) and
   removed protocol-level sessions and the `Mcp-Session-Id` header from
   Streamable HTTP outright (SEP-2567). The "closest thing to a session-end
   signal" cited by earlier research no longer exists in the spec. It also
   deprecates Sampling, Roots, and Logging under a 12-month lifecycle policy
   (SEP-2577). The conclusion is *stronger* than originally stated: MCP now
   has no session concept at all; lifecycle needs a separate seam.
2. **The Claude Code hook wire format became the de-facto cross-host standard.**
   - **Codex** ships `hooks.json` / inline `[hooks]` with the *same event names*
     (`SessionStart`, `Stop`, `SessionEnd`, `PreToolUse`, `PostToolUse`,
     `UserPromptSubmit`, …) and *verbatim-shared input fields*: `session_id`,
     `transcript_path`, `cwd`, `hook_event_name`, plus `stop_hook_active` on Stop.
     Its Stop-blocking output is byte-compatible with ours (`{"decision":"block",
     "reason":…}`, exit 2 alternative). Codex plugins bundle `hooks/hooks.json`
     and Codex even sets `CLAUDE_PLUGIN_ROOT`/`CLAUDE_PLUGIN_DATA` for
     compatibility. Caveats: non-managed hooks require one-time user trust review
     (`/hooks`); `SessionEnd` timeout budget is 1–3 s.
   - **goose** implements the Open Plugins hooks specification (`hooks/hooks.json`
     auto-discovered under `~/.agents/plugins/<name>/`, JSON-on-stdin, fail-open
     on broken hooks, Stop blocks with a host-side consecutive cap) — but its
     payload does **not** share the Claude field names: its published shape uses
     `event` and `working_dir` where the dialect uses `hook_event_name` and
     `cwd`. Goose therefore needs a small translation shim like opencode's, not
     verbatim passthrough.
   - **Zed** has an open feature request for exactly these hooks, citing Claude
     Code as prior art.
3. **Agent Plugins Spec v1.0.0** (governed alongside goose's move to the Agentic
   AI Foundation) is the portable *packaging* layer: closed `plugin.json`
   manifest, `mcp.json` (stdio + streamable-http, `${PLUGIN_ROOT}`/`${PLUGIN_DATA}`
   expansion), `skills/`. Hooks are deliberately **not** portable in v1 — they are
   client extensions under reverse-domain namespaces (`com.<vendor>/hooks/…`),
   with the spec noting hooks stay out "until their formats converge." Given
   finding 2, convergence is effectively happening around the Claude dialect.
4. **opencode is the one real deviant**: no hooks.json; lifecycle arrives via
   in-process JS plugin events (`session.status`→idle, deprecated-but-emitted
   `session.idle`). Its plugins **can** register MCP servers: `Hooks.config` is
   a first-class, typed hook in `@opencode-ai/plugin` whose input is the full
   SDK config (including `mcp`), and ghost has verified registration
   end-to-end on v1.18.21 with no config file present — the installer ships a
   single plugin artifact that self-registers MCP and bridges lifecycle
   events, never touching opencode's own config file (no jsonc rewriting).
5. **Host timing budgets make our spawn-and-return design mandatory, not
   optional.** Claude Code gives `SessionEnd` handlers a shared budget with a
   1.5 s floor that escalates to the longest configured per-hook timeout,
   capped at 60 s; codex gives 1–3 s. All lifecycle work (resolve/supersede/
   reflect) already runs in detached children claimed via PID files — this is
   validated by the specs, not just convenient. Both hosts also offer richer
   seams we may adopt later (Claude Code `mcp_tool`/`http` hook handler types;
   codex SessionStart `additionalContext` injection).

Consequence: ghost does **not** invent a wire format. The contract below is a thin
versioned envelope over the de-facto hook dialect: Claude Code and codex payloads
pass through essentially verbatim (envelope completed from argv); goose and
opencode each need a small translation shim — goose for field names, opencode for
the entire event surface.

### 2.1 The contract

```text
ghost hook <event> --source <host> [--version 1]
```

with a versioned JSON payload on stdin. Field names deliberately match the
Open Plugins / Claude hook dialect wherever one exists; the `contract` object is
ghost's envelope extension, nested under its own key rather than merged into the
top level because hosts already use top-level `source` for their own semantics
(the SessionStart reason: startup|resume|clear|compact) — a collision there would
break verbatim passthrough of native payloads:

```json
{
  "contract": {
    "version": 1,
    "source": "claude-code | goose | opencode | codex",
    "transcript_format": "claude-jsonl | opencode-messages | codex-rollout | none"
  },
  "hook_event_name": "session-start | stop | session-end",
  "session_id": "…",
  "transcript_path": "~/.claude/projects/….jsonl",
  "cwd": "/abs/path",
  "stop_hook_active": false
}
```

Rules:

- **Dialect passthrough with argv-completed envelopes.** A native Claude Code
  or codex hook payload parses as-is — their common fields
  (`session_id`, `transcript_path`, `cwd`, `hook_event_name`,
  `stop_hook_active`) are the contract's fields. Those hosts send no
  `contract` object; when it is absent, the authoritative argv values complete
  the envelope (`version` = 1, `source` = `--source`,
  `transcript_format` = the source's native format — `claude-jsonl` for
  Claude Code, `none` otherwise until its adapter lands). An EXPLICIT contract
  object must agree with argv strictly and is never repaired: a wrong version
  or disagreeing source is rejected even though completion could have fixed
  it, keeping one strict validation path with no legacy mode.
  Host-specific extras (`model`, `turn_id`, `permission_mode`, `reason`,
  `source`, `last_assistant_message`) are **tolerated and ignored**; unknown
  fields never cause rejection.
- **CLI args are authoritative; payload must agree.** `hook_event_name` (from argv
  `<event>`) appears in both argv and payload so the payload is
  self-describing in logs, but
  dispatch validates equality *before* anything else. Any
  mismatch is rejected as fail-open (log line, empty stdout, exit 0).
  Routing never consults a field that failed validation.
- **Output protocol is capability-scoped, enforced by a source matrix.** The v1
  matrix tracks what hosts can actually do per their docs (codex blocks Stop with
  `{"decision":"block"}` and injects via SessionStart `additionalContext`; goose
  blocks Stop with a host-side consecutive cap):

  | source | block_stop | inject_context | nudge on save-less stop |
  | --- | --- | --- | --- |
  | claude-code | yes | yes | yes |
  | codex | yes | yes (`additionalContext`) | yes |
  | goose | yes (capped by host) | no (unverified) | yes |
  | opencode | no | no | log-only |

- **Executable outcome table.** Every path has defined stdout / stderr / exit code:

  | outcome | stdout | stderr | exit |
  | --- | --- | --- | --- |
  | block (claude-code, codex, goose; eligible) | `{"decision":"block","reason":…}` | — | 0 |
  | context injection (`session-start`, inject-capable hosts) | host-visible injection text (Claude: system-reminder block; codex: `additionalContext` JSON) | — | 0 |
  | session-start on non-injecting host (goose, opencode) | empty (silent success) | — | 0 |
  | normal completion (spawns done, nothing to say) | empty | — | 0 |
  | nudge on non-blocking host | empty | one log line | 0 |
  | parse error / contract mismatch / unknown source / unreadable or partially-read transcript | empty | one log line | 0 |
  | absent transcript path (`transcript_path: ""`) — nothing to scan, not an error | empty | — | 0 |

  Fail-open is absolute and **always exits 0** — a nonzero exit could itself be
  read by hosts as hook failure; ghost treats "allow the stop" as the only
  failure response it ever emits. A scanner that stops early (read error,
  line-limit abort) is fail-open too: partial counts never drive a block,
  because the save proving otherwise could sit after the cut.
- **Transcripts are adapter-normalized where the adapter can.** opencode's plugin has
  typed SDK access (`client.session.messages`), so it materializes a temp JSONL in the
  `opencode-messages` format and passes the path. Claude Code forwards its native
  `transcript_path`. Ghost selects a scanner by `format`, never by `source`.
- **No legacy mode.** The contract is mandatory: `--source` is required, and an
  invocation without it fails open with one stderr line pointing at
  `ghost mcp init`. Ghost owns both ends of the wire — the installer writes the
  hook commands — so pre-contract wiring (`… hook stop` with no flags) is
  migrated in place by the next idempotent `ghost mcp init`
  (`migratePreContractHook`), and `ghost mcp status` reports such wiring as
  missing-or-pre-contract until then. This decision was made during
  implementation: with a single user and no external installs, two parse modes
  would only double the test surface.

### 2.2 v1 event handlers

All three declared events ship with defined behavior from Phase 0:

| event | handler | behavior |
| --- | --- | --- |
| `session-start` | wraps today's `HandleSessionStartHook` logic (`internal/mcpinit/hook.go:105`) | read-only project-context load + globals + obsidian sync kick; subagent gate and resume/compact short-circuits preserved verbatim; **gated on `inject_context`** — non-injecting hosts (goose, opencode) get a silent no-op |
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
| Claude Code | SessionStart hook → contract (native dialect) | Stop hook → contract (native dialect) | stdio via plugin `.mcp.json` | Claude plugin (per `2026-08-20` spec) |
| codex | SessionStart hook `additionalContext` → contract | **Native hooks** — `Stop`/`SessionEnd` in `~/.codex/hooks.json`; payload passes through verbatim | `[mcp_servers.ghost]` TOML merge | hooks.json install + TOML merge; user completes one-time `/hooks` trust review |
| goose | — | Open Plugins hooks (`SessionEnd`/`Stop`) → contract via a small field-mapping shim (`event`→`hook_event_name`, `working_dir`→`cwd`; goose's names differ from the dialect) | plugin `mcp.json` | Agent Plugins package in `~/.agents/plugins/ghost/` (`plugin.json` + `mcp.json` + client-extension hooks dir + shim script) |
| opencode | MCP instructions block today; optionally `ctx.session.hook("context", …)` later | JS plugin on `session.status`(idle) / `session.idle` → contract (translation shim #1: the whole event surface) | plugin `config(cfg)` hook mutating `cfg.mcp` — typed, first-class, verified working; the plugin is the only registration surface, so `opencode.json` is never touched | One npm package = one file to install and delete |

Honest constraints, stated up front:

- **Blocking exists where hosts block.** Claude Code, codex, and goose all accept
  a Stop-block decision (goose caps consecutive blocks host-side). Only opencode
  cannot block; its nudge degrades to a log line. The matrix makes degradation
  explicit rather than silent.
- **codex hook trust is a real UX step.** Codex requires one-time review of every
  non-managed hook definition before it runs. Install must tell the user to run
  `/hooks` and trust ghost's entries; until then codex silently skips them.
- **Session-end budgets are tight everywhere.** Claude Code: a shared budget
  with a 1.5 s floor escalating to the longest configured per-hook timeout
  (capped at 60 s); codex: 1–3 s; both fire synchronously. Ghost's handlers must only spawn
  detached workers and return — never do DB or LLM work inline. This validates
  the existing spawn architecture and rules out any future "do it inline" drift.
- **opencode's plugin MCP registration works but carries no written stability
  guarantee** — `Hooks.config` is typed in the official plugin package and the
  pattern is ecosystem-load-bearing (a v1.14.32 regression that silently broke
  it was patched within one release), but upstream has declined to commit to it
  in writing (#24065). Ghost accepts that risk deliberately: one plugin file is
  the entire integration (MCP + lifecycle), which makes install/uninstall
  trivial and config rewriting unnecessary — and `ghost mcp status --client
  opencode` byte-compares the installed plugin against the embedded source, so
  drift is visible and repairable with one command.
- **Transcript formats are explicitly unstable.** Codex documents that
  `transcript_path` "isn't a stable interface"; formats may change per host
  release. Scanner registry entries are versioned and fail open, so format drift
  costs at most a lost capture pass, never a broken host.
- **Claude Code's Stop fires every turn**, other hosts fire closer to true session
  end. This asymmetry already exists and is harmless because every spawned worker
  is idempotent under PID claims; the contract carries both `stop` and
  `session-end` so hosts declare what they emit.
- **Dialect drift is a risk, not a blocker.** If the de-facto standard diverges,
  only the envelope mapping in `internal/hostevent` changes — adapters and the
  engine are insulated.

## 4. Implementation plan

### Phase 0 — contract + Claude Code parity (implemented on this branch)

- New `internal/hostevent` package: `Payload` (envelope + shared dialect fields
  + `Raw` passthrough for host extras), strict validation (`Parse` — contract
  version, argv/payload equality, event-name normalization across
  camelCase/kebab spellings), the capability matrix, and the format-keyed
  transcript scanner registry (`claude-jsonl` is v1's only entry; moved out of
  the stop hook verbatim).
- New dispatch entrypoint `mcpinit.RunHostEvent(event, source, stdin, stdout,
  stderr)` covering all three v1 events: session-start injects context,
  stop runs lifecycle spawns + capability-gated nudge, session-end runs spawns
  only. The old per-event handlers are gone — one entrypoint, one code path.
- **Contract is mandatory** (see §2.1 "No legacy mode"): init writes
  `--source claude-code` into both hooks and migrates pre-contract wiring in
  place; status flags stale wiring until init re-runs.
- Status groundwork: the SessionStart/Stop checks require the `--source` form,
  so pre-contract installs surface as actionable failures instead of failing
  open silently at every fire.

### Phase 1 — opencode adapter (closes #345)

- **Scanner first (implemented on this branch):** `hostevent.ScanOpencodeMessages`
  and its golden fixture are in the registry ahead of any adapter. It parses one
  `{info, parts}` JSONL object per line — verbatim `client.session.messages`
  serialization — counting assistant tool-call parts, with save detection keyed
  to opencode's `<server>_<tool>` MCP naming (`ghost_ghost_memory_save`,
  `ghost_save_global`), not Claude Code's `mcp__ghost__*`. Without this, an
  adapter that invoked the contract successfully but failed open on an
  unsupported format would silently disable reflection/resolve/supersede for
  opencode users.
- `plugin` source of truth: `internal/mcpinit/opencode_ghost.ts`, embedded via
  `go:embed` and installed by `RunOpencode` to `~/.config/opencode/plugins/ghost-opencode.ts`
  (idempotent; a drifted or outdated file is repaired by the next init, detected by the
  versioned `// ghost-opencode v1` header). The plugin listens for
  `session.status`→idle (falling back to legacy `session.idle` until the first status
  event is seen, with a per-session debounce), fetches messages via
  `client.session.messages({path:{id}})`, serializes them verbatim to a temp JSONL,
  and spawns `ghost hook stop --source opencode` detached. All errors are one
  best-effort `client.app.log` line — fail-open.
- Status check: verify plugin file present + versioned marker alongside the mcp entry
  (mirrors `hasStop`).
- **Verified end-to-end (2026-08-24):** a real opencode session (`hy3-free`, scratch
  XDG dirs) read files via `bash`+`read`; on idle the plugin materialized a 4-message
  `{info, parts}` JSONL and fired `ghost hook stop --source opencode`. Replaying the
  captured payload through the ghost binary produced the exact §2.1 non-blocking-host
 outcome: exit 0, empty stdout, one "nudge suppressed" stderr line.

### Phase 2 — codex + goose adapters (implemented on this branch)

- `RunCodex`: merges `[mcp_servers.ghost]` into `~/.codex/config.toml`
  **textually** (line-based block splice — user comments preserved
  byte-for-byte, never parse-and-rewrite) and installs SessionStart/Stop/
  SessionEnd into `~/.codex/hooks.json` (`~/.codex` honors `$CODEX_HOME`),
  sharing the Claude dialect verbatim with `--source codex`. The installer
  prints the mandatory one-time `/hooks` trust instruction; codex skips
  untrusted hooks silently. SessionEnd gets an explicit 3 s timeout (codex
  default is 1 s). `StatusCodex` token-validates each hook command against
  codex's own files.
- `RunGoose`: installs the Agent Plugins package at `~/.agents/plugins/ghost/`
  — `plugin.json`, `mcp.json` (stdio ghost server), and a top-level
  `hooks/hooks.json` per goose's documented discovery. Goose's field-name
  deviation is aliased **inside core** (`hostevent.Parse`: for `--source
  goose`, native `event`/`working_dir` backfill absent hook_event_name/cwd;
  dialect fields always win; argv agreement still enforced), so hooks invoke
  `<ghost> hook <event> --source goose` directly — no shim scripts,
  cross-platform by construction.
- Nudge stays log-only for opencode; document it.

### Phase 3 — distribution polish

- npm publish of the opencode plugin; Claude plugin marketplace entry per the
  existing spec; evaluate Gemini CLI extensions (bundles MCP + context files) as
  a fifth adapter; watch Zed's hooks proposal — if it lands on this dialect, Zed
  becomes a config-only target like codex.

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
