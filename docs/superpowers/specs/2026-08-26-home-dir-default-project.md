# Home-Dir Session Routing → Configurable Default Project — Design

Issue: #391. Status: implemented in this spec's branch.

## Corrected root cause (2026-08-26 investigation)

The issue presumed current ghost still mints hash-named buckets for
unmatched cwds. It does not, and hasn't since v0.8.0:

- The 12-hex IDs (`6bdc098af7f5`, `4c1e5f85af17`, …) were minted by the
  original Phase A+B daemon (`internal/project/context.go`, commit
  `340d14c`): `ID = hex(sha256(absPath)[:6])`. That subsystem was deleted in
  the v0.8.0 MCP-only strip (`9aff8a7`). No hashing exists anywhere in
  current Go/TS code.
- Today every creation path is name-keyed: MCP saves require `project_id`
  and create-on-first-save under the caller-provided string
  (mcpserver.go save handler → `EnsureProject(id, "", id)`).
- The live problem is therefore *steering*, not bucket-minting: sessions
  whose cwd matches no project (`~`, `/`) inject no project context ("no
  project matched this directory"), so models saving discoveries must invent
  a project_id — the origin of the stray name buckets (`quack`,
  `traderjoes`, `clanker`, `undying-codex`).

## Design

### 1. Config knob

    routing:
      default_project: infrastructure   # empty = feature off (default)

- `RoutingConfig{DefaultProject}` in internal/config; layered through the
  existing koanf stack. Env override `GHOST_ROUTING_DEFAULT_PROJECT` joins
  the explicit envOverrides map (underscore-in-key precedent).
- Empty value = zero behavior change everywhere.

### 2. Session-context routing (the steering lever)

Shared helper in internal/mcpinit used by the session-start context render
(`loadSessionContext`) and the stop hook's three resolution sites:

    resolveSessionProject(store, cwd):
      id, name := store.ResolveProject(cwd); hit → return
      if DefaultProject == "" → return miss (unchanged)
      if cwd != $HOME && cwd != "/" → return miss (unchanged)
      return store.ResolveProject(DefaultProject)  // miss degrades to today

Deliberate narrowness:

- Only `$HOME` and `/` route to the default. Other unmatched dirs keep
  today's behavior — a session under `/tmp/x` saying nothing is safer than
  silently writing into `infrastructure`.
- An explicit non-empty `project_id` on any tool call is never overridden:
  create-on-first-save semantics stay intact for deliberate names.
- A configured-but-nonexistent default behaves as if unset.

The knob accepts anything `ResolveProject` accepts (name, basename, id),
resolved at use time — renames don't break config.

### 3. `ghost project merge <old> <new>`

Exposes the previously unreachable `Store.MergeProject` (reassigns all child
records preserving memory IDs/links/pins, deletes the old row — tx-wrapped,
already store-tested). Both args go through `ResolveProject`; ambiguity or
miss errors out with the known-projects listing; `old == new` is rejected.
This replaces #348's hand-written SQL as the sanctioned consolidation path.

## Out of scope

- Gating auto-creation by root-set (`~/git/*`) — unnecessary now that
  creation is name-keyed and steering handles the home-dir case.
- MCP exposure of project merge (admin operation; CLI suffices).
