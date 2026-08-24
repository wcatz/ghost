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

### 2.0 Grounding: read against the actual specs (researched 2026-08-23)

Primary sources consulted: MCP specification revisions 2025-06-18 and 2025-11-25,
the Agent Plugins Specification v1.0.0 (agent-plugins.org), Claude Code's hooks
reference, OpenAI Codex's hooks + config reference, goose's hooks documentation,
and opencode's plugin docs/issues. Findings that shape this design:

1. **MCP has no host-conversation lifecycle — verified in both spec revisions.**
   The MCP lifecycle is *connection*-scoped: initialize → operation → shutdown,
   where stdio shutdown is just "client closes stdin, then SIGTERM/SIGKILL." There
   is no primitive for "user session started/ended" or "turn ended." Streamable
   HTTP's `Mcp-Session-Id` + HTTP DELETE is the closest thing to a session-end
   signal and only applies to remote servers. Conclusion stands: MCP is the tool
   surface (ghost must never grow a non-MCP one), lifecycle needs a separate seam.
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
   - **goose** implements the Open Plugins hooks specification: same shape
     (`hooks/hooks.json`, JSON-on-stdin, `{"decision":"block"}` stdout signal),
     fail-open on broken hooks, Stop blocks with a host-side consecutive cap.
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
   `session.idle`). Its plugins *can* register MCP servers by mutating `cfg.mcp`
   in the `config(cfg)` hook — widely relied upon upstream but officially
   unsupported (anomalyco/opencode #24065, closed not-planned).
5. **Host timing budgets make our spawn-and-return design mandatory, not
   optional.** Claude Code gives `SessionEnd` handlers a shared 1.5 s budget;
   codex 1–3 s. All lifecycle work (resolve/supersede/reflect) already runs in
   detached children claimed via PID files — this is validated by the specs, not
   just convenient. Both hosts also offer richer seams we may adopt later
   (Claude Code `mcp_tool`/`http` hook handler types; codex SessionStart
   `additionalContext` injection).

Consequence: ghost does **not** invent a wire format. The contract below is a thin
versioned envelope over the de-facto hook dialect, so Claude Code, codex, and
goose payloads pass through essentially verbatim; only opencode needs a
translation shim.

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

- **Dialect passthrough.** A native Claude Code, codex, or goose hook payload
  already parses as-is — their common fields (`session_id`, `transcript_path`,
  `cwd`, `hook_event_name`, `stop_hook_active`) are the contract's fields.
  Host-specific extras (`model`, `turn_id`, `permission_mode`, `reason`,
  `source`, `last_assistant_message`) are **tolerated and ignored**; unknown
  fields never cause rejection.
- **CLI args are authoritative; payload must agree.** `hook_event_name` (from argv
  `<event>`) and `contract.source` appear in both argv and payload so the payload is
  self-describing in logs, but
  dispatch validates equality (and `contract.version == 1`) *before* anything else. Any
  mismatch — including a stale adapter sending `"source": "claude-code"` with
  `--source opencode` — is rejected as fail-open (log line, empty stdout, exit 0).
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
  | context injection (`session-start`) | host-visible injection text (Claude: system-reminder block; codex: `additionalContext` JSON) | — | 0 |
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
| Claude Code | SessionStart hook → contract (native dialect) | Stop hook → contract (native dialect) | stdio via plugin `.mcp.json` | Claude plugin (per `2026-08-20` spec) |
| codex | SessionStart hook `additionalContext` → contract | **Native hooks** — `Stop`/`SessionEnd` in `~/.codex/hooks.json`; payload passes through verbatim | `[mcp_servers.ghost]` TOML merge | hooks.json install + TOML merge; user completes one-time `/hooks` trust review |
| goose | — | Open Plugins hooks (`SessionEnd`/`Stop`) → contract near-natively; payload passes through almost verbatim | plugin `mcp.json` | Agent Plugins package in `~/.agents/plugins/ghost/` (`plugin.json` + `mcp.json` + client-extension hooks dir) |
| opencode | MCP instructions block today; optionally `ctx.session.hook("context", …)` later | JS plugin on `session.status`(idle) / `session.idle` → contract (the only translation shim) | npm-plugin path: `cfg.mcp` mutation in `config(cfg)` hook (works, undocumented upstream — #24065); supported fallback: config-file merge stays | One npm package doing both + `mcp init --client opencode` writes the config merge as belt-and-braces |

Honest constraints, stated up front:

- **Blocking exists where hosts block.** Claude Code, codex, and goose all accept
  a Stop-block decision (goose caps consecutive blocks host-side). Only opencode
  cannot block; its nudge degrades to a log line. The matrix makes degradation
  explicit rather than silent.
- **codex hook trust is a real UX step.** Codex requires one-time review of every
  non-managed hook definition before it runs. Install must tell the user to run
  `/hooks` and trust ghost's entries; until then codex silently skips them.
- **Session-end budgets are tight everywhere.** Claude Code: 1.5 s shared;
  codex: 1–3 s; both fire synchronously. Ghost's handlers must only spawn
  detached workers and return — never do DB or LLM work inline. This validates
  the existing spawn architecture and rules out any future "do it inline" drift.
- **opencode's `cfg.mcp` plugin registration is real but unsupported** — it works
  today via live-reference mutation and much of the ecosystem depends on it, but
  upstream closed the request to bless it without committing. Ghost treats the
  config-file merge as the source of truth and plugin self-registration as an
  optimization that must be safe to lose.
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
- `plugin/ghost-opencode/` in-repo: TypeScript plugin listening for
  `session.status`→idle transitions; spawns `ghost hook stop --source opencode`
  with SDK-fetched messages serialized to a temp JSONL.
- `RunOpencode` extended: also install the plugin file under
  `~/.config/opencode/plugin/` (idempotent, versioned header comment) alongside the
  MCP merge done today.
- Status check: verify plugin file present + mcp entry (mirrors `hasStop`).

### Phase 2 — codex + goose adapters

- `RunCodex`: merge `[mcp_servers.ghost]` into `~/.codex/config.toml` **and**
  install a `hooks` block (`~/.codex/hooks.json` or inline TOML) wiring
  `SessionStart`, `Stop`, and `SessionEnd` to the contract. Payloads pass through
  verbatim — codex shares Claude Code's input fields exactly. The installer must
  instruct the user to run `/hooks` and trust the entries (codex skips untrusted
  hooks silently).
- `RunGoose`: install an Agent Plugins-conformant package under
  `~/.agents/plugins/ghost/` — `plugin.json`, `mcp.json` (stdio ghost server),
  and the client-extension hooks dir for its Open Plugins hooks. Near-native
  passthrough; blocking honored with goose's host-side cap.
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
