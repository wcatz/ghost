# Ghost as a Claude Code Plugin — Design

> **Historical record — not current documentation.** This file preserves the
> design or implementation state at the time it was written. For current Ghost
> behavior, start with [`docs/README.md`](../../README.md) and the source.

## Context

Ghost currently integrates with Claude Code via `ghost mcp init`, which mutates the user's `~/.claude.json` (MCP server registration), `~/.claude/settings.json` (tool permissions, SessionStart + Stop hooks, `autoMemoryEnabled: false`), writes `MEMORY.md` redirects per known project, and imports Claude Code's existing file memories (`internal/mcpinit/init.go`). This works, but the wiring is fragile: it depends on the `claude` CLI's exact flag surface (`mcp add-json`, `mcp get`, `mcp remove`), hand-quotes hook commands per-platform, and re-runs to heal staleness after upgrades.

The Claude Code **plugin system** offers a declarative, versioned, marketplace-discoverable home for exactly this wiring: `.mcp.json` declares the stdio MCP server, `hooks/hooks.json` declares the SessionStart + Stop hooks, and the plugin ships as a single installable artifact that Claude Code copies into a managed cache and updates through the plugin manager.

This spec designs shipping Ghost as a Claude Code plugin: **one-command install, zero init** — install the plugin and the MCP server, hooks, and one-time finalization all happen without the user ever running `ghost mcp init`.

## Goals

- A single marketplace entry installable with `/plugin install` that gives a working Ghost memory server — MCP tools, SessionStart context injection, Stop-hook save nudge — with **no `ghost mcp init` step**.
- Bundle the platform-specific Ghost binary inside the plugin (per-arch, offline, zero extra installs), because the plugin is the distribution.
- Works on all six platforms goreleaser already builds: `darwin/{arm64,amd64}`, `linux/{arm64,amd64}`, `windows/{arm64,amd64}`.
- Updates flow through the plugin manager (`/plugin update`), not `ghost upgrade`.
- Coexist with `ghost mcp init`: init stays the documented default for non-plugin users (opencode, manual setups); a plugin install and a standalone init detect each other and never fight.
- Ghost's data lives in its existing config/data dirs (`~/.config/ghost`), shared with any standalone binary, surviving plugin updates and reinstallation.

## Non-goals

- No opencode plugin in this milestone. `ghost mcp init --client opencode` keeps working as today.
- No launcher/dispatcher binary. The only documented mechanism that resolves a bundled binary path for all six platforms from one artifact is `userConfig` substitution into the MCP/hook command; we use that rather than shipping a hand-rolled platform detector.
- No `command`-source plugin. It requires Ghost pre-installed (circular for new users) and Claude Code ≥ v2.1.229.
- No changes to the memory schema, search, reflection, or any other subsystem. The plugin is pure distribution + one idempotent finalize path.
- No plugin-level restore of user settings on uninstall in v1 (see Edge Cases).

## Key research findings (grounding)

Verified against the Claude Code plugin docs (plugins-reference, plugin-marketplaces):

- A marketplace `source` is a **single artifact per entry** — there is no per-OS/arch URL matrix. Bundling all six binaries in one artifact is the only way to serve all platforms from one entry.
- The community marketplace (`anthropics/claude-plugins-community`, 2,281 entries) is ~100% git sources (`url`/`git-subdir`); `archive` and `command` sources are new and effectively unused in the wild. The one canonical example of shipping compiled binaries (`ForteScarlet/codex-kkp`) bundles every platform's executable in one artifact and picks at runtime — the same "bundle all, select at install" shape this design uses, but via the documented `userConfig` mechanism instead of a hand-rolled selector.
- `${CLAUDE_PLUGIN_ROOT}` resolves to the plugin's installation cache directory and **changes on every update**; `${CLAUDE_PLUGIN_DATA}` is a persistent directory that survives updates (`~/.claude/plugins/data/{id}/`). Never write state to `CLAUDE_PLUGIN_ROOT`.
- `${user_config.KEY}` substitutes into **MCP server configs and exec-form hook commands** (plain-string substitution, no shell re-parsing — safe for a path value). It is **rejected for shell-form hook commands and monitors** for injection safety. Hook processes also receive `CLAUDE_PLUGIN_OPTION_<KEY>` env vars.
- `userConfig` supports only `string`, `number`, `boolean`, `directory`, `file` types — no select/enum, so the platform picker is a free-text string with a strict description. Values are stored in `settings.json` under `pluginConfigs[<plugin-id>].options`.
- Plugin MCP servers are **auto-approved** by default, so the 19-permission grant in `ensurePermissions` (`init.go:236`) disappears entirely.
- Exec-form hooks: `command` must resolve to a real executable on PATH or an absolute path; on Windows it must be a real `.exe` (`.cmd`/`.bat` shims can't be spawned without a shell). The bundled `ghost.exe` satisfies this.
- The `archive` source is a zip over HTTPS with an optional `sha256` pin; Claude Code refuses archives larger than 256 MiB; requires Claude Code ≥ v2.1.224. Version derives from the `sha256` pin (or the file digest) so a pin bump drives updates.
- `claude plugin config set <plugin> <key=value>` sets a `userConfig` option non-interactively — the automation/testing route for the platform picker.
- `claude plugin validate --strict` validates a plugin directory against the manifest schema — the CI gate.
- Plugin `settings.json` supports only `agent` and `subagentStatusLine` keys — so `autoMemoryEnabled: false` **cannot** ship declaratively; it must be a one-time hook mutation.

## Design

### Artifact layout

```
ghost-plugin/
├── .claude-plugin/
│   └── plugin.json          # name, displayName, version, description, userConfig
├── .mcp.json                # mcpServers.ghost → bundled binary via ${CLAUDE_PLUGIN_ROOT}
├── hooks/
│   └── hooks.json           # SessionStart + Stop hooks (exec form)
└── bin/
    ├── darwin-arm64/ghost
    ├── darwin-amd64/ghost
    ├── linux-arm64/ghost
    ├── linux-amd64/ghost
    ├── windows-arm64/ghost.exe
    └── windows-amd64/ghost.exe
```

`.claude-plugin/plugin.json`:

```json
{
  "name": "ghost",
  "displayName": "Ghost Memory",
  "version": "0.1.0",
  "description": "MCP memory server for Claude Code",
  "author": { "name": "Ghost" },
  "defaultEnabled": true,
  "userConfig": {
    "platform": {
      "type": "string",
      "title": "Your platform",
      "description": "One of: darwin-arm64, darwin-amd64, linux-arm64, linux-amd64, windows-arm64, windows-amd64",
      "required": true
    }
  }
}
```

`.mcp.json`:

```json
{
  "mcpServers": {
    "ghost": {
      "command": "${CLAUDE_PLUGIN_ROOT}/bin/${user_config.platform}/ghost",
      "args": ["mcp"]
    }
  }
}
```

`hooks/hooks.json` (exec form so `${user_config.platform}` is legal):

```json
{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "${CLAUDE_PLUGIN_ROOT}/bin/${user_config.platform}/ghost",
            "args": ["hook", "session-start"]
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "${CLAUDE_PLUGIN_ROOT}/bin/${user_config.platform}/ghost",
            "args": ["hook", "stop"]
          }
        ]
      }
    ]
  }
}
```

### Distribution

`marketplace.json` lives at the root of the `wcatz/ghost` repo (the catalog is tiny JSON; the repo stays clean — the plugin zip itself is a release asset, not committed):

```json
{
  "name": "ghost",
  "owner": { "name": "Ghost" },
  "plugins": [
    {
      "name": "ghost",
      "description": "MCP memory server for Claude Code",
      "source": {
        "source": "archive",
        "url": "https://github.com/wcatz/ghost/releases/download/v0.1.0/ghost-plugin.zip",
        "sha256": "<release-computed-digest>"
      }
    }
  ]
}
```

Install flow:

```
/plugin marketplace add wcatz/ghost
/plugin install ghost@ghost        # prompts once: "Your platform"
```

`/plugin marketplace add wcatz/ghost` uses the GitHub shorthand (repo-relative `.claude-plugin/marketplace.json`); the plugin itself is fetched from the archive asset.

### Release pipeline

The landed flow is draft-first so the marketplace catalog can never reference an asset that does not exist yet (it implements the `sha256` + digest-script promise above):

1. GoReleaser runs with `release.draft: true`: the release is created unpublished, so `releases/latest` keeps serving the previous release.
2. The `plugin` job assembles and attaches all three archives (`ghost-plugin.zip`, `ghost-plugin-windows-{amd64,arm64}.zip`).
3. `scripts/update-marketplace-digest.sh` rewrites each marketplace entry to a version-pinned `releases/download/vX.Y.Z` URL plus the archive's `sha256`.
4. `gh release edit --draft=false` publishes — the first irreversible point.
5. Every version-pinned and `releases/latest/download` URL is curl-verified to return 200.
6. The catalog reaches main as a pull request rather than a direct write: `main` requires a pull request plus three status checks, and a workflow's `GITHUB_TOKEN` cannot be a bypass actor on a personal repository (measured at v0.30.18: contents-API PUT returned HTTP 409 GH013, run 35928382388; the git-push route under the same context was rejected identically, run 35929704681). The job writes the pinned catalog to `automation/marketplace-pin-v<version>` and opens a PR from it. GitHub creates workflow runs for a `GITHUB_TOKEN`-opened PR in an approval-required state, released from the merge box with "Approve workflows to run", so the three required checks run and the branch rule governs the merge. A GitHub App or PAT token would skip that click but adds a standing credential for a PR a human was always going to merge. The branch is unique per release and is deleted at merge by the repository's "Automatically delete head branches" setting. If the PR is abandoned, the manual admin runbook below remains the fallback. The pin is skipped when main already matches (re-runs of the same release are idempotent given reproducible archives) and skipped behind a downgrade guard when main already pins a newer version, so an older release's re-run never rolls the catalog back; those runs stay pending until a human with write access approves them, so an unattended re-run cannot merge the pin on its own.

Because the URLs are version-pinned, a stale catalog always points at still-valid assets: there is no window in which an entry 404s or mismatches, and a post-publish `gh release upload --clobber` swap of an already-pinned archive becomes an install-time sha256 failure instead of landing silently.

### Self-finalize on first session (the zero-init trick)

The plugin's `SessionStart` hook is Ghost's own binary running `ghost hook session-start`, so it has full power to finalize on first contact — the same operations `ghost mcp init` performs today, minus the registration steps the plugin already owns.

**Plugin-mode detection:** Claude Code exports `CLAUDE_PLUGIN_ROOT` to hook processes. `ghost hook session-start` checks `os.Getenv("CLAUDE_PLUGIN_ROOT")` — set means it's the plugin hook. This is the safety gate: standalone `ghost mcp init` installs never run the finalize path. No new CLI flag is needed.

**One-time finalize (idempotent, mirrors `init.go` steps 7–9):**

1. **Disable Claude's built-in file memory** — the one thing a plugin `settings.json` can't express. The hook writes `"autoMemoryEnabled": false` into `~/.claude/settings.json` (reusing the existing `ensureAutoMemoryDisabled` logic from `init.go:498`, parameterized by settings path).
2. **Import Claude Code memories** — opens the Ghost DB (same data dir as standalone, so memory is shared and survives plugin reinstall/update) and imports `~/.claude/projects/*/memory/*.md` for known projects (reuses `claudeimport`).
3. **Write MEMORY.md redirects** — per known project, the same `writeRedirects` logic (`init.go:604`).

**Idempotency marker:** a marker file in `CLAUDE_PLUGIN_DATA` (survives updates: `~/.claude/plugins/data/ghost-ghost/finalized`) records completion. Every subsequent session does one stat; if present, the hook skips straight to context injection. All three operations are already written idempotently, so a marker-less crash self-heals on the next run.

**Output discipline (hard invariant):** the hook's stdout is injected into the system prompt — finalize progress must never print there. Finalize logs go to stderr (visible in Claude Code's debug log); only the normal context markdown goes to stdout.

### Update semantics

- Plugin `version` in `plugin.json` drives updates: bump on every release; users get the new bundled binary via `/plugin update` or auto-update.
- `ghost upgrade` detects a plugin-managed binary (its executable path contains `/.claude/plugins/`) and no-ops with a message pointing at `/plugin update`. It never replaces a file inside the managed plugin cache.
- When a plugin updates, `CLAUDE_PLUGIN_ROOT` changes; Claude Code keeps the previous version's path until `/reload-plugins` or restart. Ghost's DB lives outside the plugin, so nothing breaks across the switch.

### Plugin-mode awareness in the existing CLI

- `ghost mcp status` detects plugin-managed mode and reports "managed by the ghost plugin" instead of looking up `claude mcp get ghost`.
- `ghost mcp init` detects plugin-managed mode (env var or the finalize marker) and skips its own Claude Code wiring (steps 3–7) with a "the ghost plugin manages this" message, and clears the plugin finalize marker if it writes anything — staying non-destructive and idempotent on re-run as it is today.

## Edge cases & error handling

- **Wrong platform pick:** the MCP server fails to start and `/plugin` → Errors shows the missing path. The `Setup` hook (fires on `--init`/`--init-only`) validates `${CLAUDE_PLUGIN_ROOT}/bin/${user_config.platform}/ghost` exists and fails with the six allowed values otherwise; the SessionStart hook also checks before injecting context and logs a clear stderr message. User re-enables the plugin to correct the prompt.
- **`ghost upgrade` under a plugin:** refuses loudly, never guesses (see Update semantics).
- **Coexistence:** standalone `ghost mcp init` detects plugin mode and defers; plugin finalize adopts whatever DB exists. Manual permission grants in `settings.json` are never clobbered — the plugin adds nothing there (auto-approve covers MCP tools).
- **Uninstall:** plugin hooks vanish with the plugin. `autoMemoryEnabled: false` and MEMORY.md redirects remain (harmless; Claude Code re-enables its own file memory as needed). Restoring settings on uninstall is a v1 non-goal.
- **DB location invariant:** ghost data stays in its config/data dirs. The plugin never writes state to `CLAUDE_PLUGIN_ROOT` (ephemeral) or relies on it persisting; `CLAUDE_PLUGIN_DATA` holds only the finalize marker. Uninstalling deletes `CLAUDE_PLUGIN_DATA` (unless `--keep-data`), which is fine — a fresh install simply re-finalizes against the same DB.
- **Config sharing:** the bundled binary reads the same `~/.config/ghost/config.yaml` as any standalone install, so plugin and standalone share embeddings, Ollama settings, and data paths.

## Verification

- **Unit tests:** finalize idempotency (marker present/absent, crash-mid-finalize), platform-path validation, upgrade-refusal under a plugin path, stdout-cleanliness (context markdown only, finalize logs on stderr), plugin-mode detection (env var set/unset).
- **Integration (`claude --plugin-dir ./ghost-plugin`):** local load; set the platform via `claude plugin config set ghost platform=darwin-arm64`; verify MCP server appears in `/plugin`, SessionStart context is injected, finalize marker is written, `autoMemoryEnabled` is false in `settings.json`.
- **Release / PR CI:** goreleaser builds the six binaries and assembles `ghost-plugin.zip`; a step computes and pins the `sha256` in `marketplace.json`; PR CI runs `claude plugin validate --strict` against the assembled trees and `go vet ./...`.
- **E2E matrix:** one environment per OS (macOS arm64 host, Linux x86_64 + arm64, Windows via the existing QEMU rig) — install the plugin, confirm MCP boots, confirm memory persists across a plugin update (proves the data-dir invariant).

## Open questions for the implementing agent

- Exact platform-picker UX inside the enable prompt (free-text description vs. recommending a default for the most common case).
- Whether the `Setup` hook should be in v1 or deferred to a follow-up (it is cheap insurance against the free-text footgun, but the SessionStart guard may suffice).
- Whether to publish to the Anthropic community marketplace after internal validation, or self-host the marketplace repo only at first.
- Exact Windows code-signing story for the bundled `ghost.exe` (unsigned binaries trigger SmartScreen; the existing release assets already face this).