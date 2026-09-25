# Ghost CLI reference

The `ghost` binary is both the MCP server and the maintenance CLI. Run `ghost help` for the built-in top-level summary.

## MCP server

### `ghost mcp`

Starts the MCP server over stdio:

```bash
ghost mcp
```

MCP clients normally spawn this command themselves. Do not run it as a long-lived daemon unless your client is configured to manage the process.

### `ghost mcp init`

Configures supported MCP clients:

```bash
ghost mcp init
ghost mcp init --client claude
ghost mcp init --client opencode
ghost mcp init --client codex
ghost mcp init --client goose
ghost mcp init --client all
```

| Flag | Meaning |
|---|---|
| `--client <name>` | Target `claude`, `opencode`, `codex`, `goose`, or `all`. Without it, detect clients on `PATH`. |
| `--dry-run` | Print the changes without writing them. |

The operation is idempotent. If multiple supported clients are detected, Ghost configures all of them unless a target is specified. Plugin-managed Claude Code integrations are detected and left under plugin control.

### `ghost mcp status`

Checks one client integration:

```bash
ghost mcp status --client claude
ghost mcp status --client opencode
ghost mcp status --client codex
ghost mcp status --client goose
```

Without `--client`, status targets Claude Code. The checks include client registration, lifecycle wiring, the database, Ollama reachability, and embedding/link coverage where applicable. A generic MCP client has no Ghost-specific status integration.

Status also lists any project that records no usable checkout and no repository remote, with the `ghost project bind` command that repairs it. Those projects are not a health failure — every check above can pass while sessions in such a checkout silently get no injected context — and the section is omitted entirely when there is nothing to fix.

## Hooks

```bash
ghost hook <event> --source <host>
```

Events currently used by the host adapters include:

- `session-start`
- `stop`
- `session-end`

Supported source tokens include `claude-code`, `opencode`, `codex`, and `goose`. A missing or unknown source fails open with one diagnostic line and exit status `0`; it does not silently route through another host. Re-run `ghost mcp init` to repair legacy wiring.

`ghost hook` is normally called by an MCP client or lifecycle adapter, not by hand.

## Memory maintenance

All maintenance commands are dry-run by default. Preview the result before passing `--apply`.

### `ghost reflect <project>`

Consolidates memories for a project:

```bash
ghost reflect myproject
ghost reflect myproject --apply
```

| Flag | Meaning |
|---|---|
| `--tier auto\|cli\|opencode\|sqlite` | Select the consolidation backend. `auto` is the default and routes to the calling harness when available. |
| `--apply` | Save the consolidated result. |
| `--restore` | Restore the most recent consolidation snapshot. |
| `--require-llm` | Fail instead of falling back to the offline SQLite/Jaccard tier. |
| `--allow-drops` | Apply even when guarded-category memories would be removed without a merge. |
| `--promote-globals` | Promote cross-project candidates into `_global`; without this flag they remain project-scoped. |
| `--skip-unchanged` | Skip the LLM call when the consolidatable set is unchanged since the last applied pass. |
| `--source <host>` | Explicit harness: `claude-code`, `opencode`, `codex`, or `goose`. |
| `--project <name>` | Project name instead of the positional form. Takes the next argument verbatim, so dash-prefixed names work. |

The `auto` tier uses the explicit source when provided, otherwise detects the calling harness. It does not silently switch to a different harness or billing path. When a source is known but its CLI binary is unavailable, auto can fall back to SQLite; the offline tier is also available for an explicit local run.

CLI-backed maintenance runs each harness with an allowlisted environment, isolated configuration, and tools/MCP disabled; see [Harness subprocess environment](configuration.md#harness-subprocess-environment).


### `ghost resolve <project>`

Marks resolved-evidence memories so they leave ranked session injection while remaining searchable:

```bash
ghost resolve myproject
ghost resolve myproject --apply
```

| Flag | Meaning |
|---|---|
| `--apply` | Stamp `resolved_at` on confirmed memories. |
| `--source <host>` | Classify through `claude-code`, `opencode`, `codex`, or `goose`. |
| `--project <name>` | Project name instead of the positional form. Takes the next argument verbatim, so dash-prefixed names work. |

The classifier uses a local keyword prefilter and batched KEEP-biased calls. Only explicit KEEP verdicts enter the content-hash cache; missing or garbled verdicts are counted as UNKNOWN and retried on a later pass. It fails when no source-specific harness can be selected; it never silently falls back to another harness.

### `ghost supersede <project>`

Proposes and classifies directed replacement relationships:

```bash
ghost supersede myproject
ghost supersede myproject --apply
```

| Flag | Meaning |
|---|---|
| `--apply` | Write `supersedes` and `causes` links. |
| `--threshold <float>` | Minimum cosine similarity for a candidate pair; default `0.80`. |
| `--source <host>` | Classify through `claude-code`, `opencode`, `codex`, or `goose`. |
| `--project <name>` | Project name instead of the positional form. Takes the next argument verbatim, so dash-prefixed names work. |

Each candidate is classified as `supersedes`, `causes`, or `neither`. The default source is the calling harness. Applying the pass enables targeted demotion during search for genuine replacement pairs.

## Project operations

### `ghost project delete <name-or-id>`

Permanently removes a project and all of its child records. The command always previews the deletion first:

```bash
ghost project delete myproject
ghost project delete myproject --apply
```

The second form requires the project name to be retyped at the prompt. It is irreversible and refuses to delete `_global`.

### `ghost project merge <old> <new>`

Moves all records from the old project into the surviving project while preserving memory IDs, links, and pin state:

```bash
ghost project merge old-name new-name
```

Both arguments accept a project name, ID, path-prefix match, or basename match. A basename match is accepted only when exactly one candidate survives the recorded-path and repository-remote checks; an ambiguous match is rejected rather than guessed. The command refuses to merge a project into itself.

### `ghost project bind <project-id> <checkout-directory>`

Gives a project a recorded checkout, so a session in that directory resolves it:

```bash
ghost project bind infrastructure /home/wayne/git/infrastructure
```

The first argument is an existing project **id** — not a name or a path. A project that records no usable location cannot be resolved from a directory, so it gets no session-start context and no Stop-hook lifecycle work; this is the command that repairs that, and `ghost mcp status` lists every project that needs it.

The directory is made absolute and cleaned, and must exist and be a directory. Ghost also records the checkout's Git remote when the project records none, so a second worktree of the same repository resolves to the same project.

The command refuses, writing nothing, when:

| Refusal | Reason |
|---|---|
| the project is `_global` | it holds every project's memories, not a checkout |
| the project id does not exist | bind never guesses which project was meant |
| the path is missing, is not a directory, or is the filesystem root | a path that cannot be compared against a session directory would never resolve |
| another project already records that path | two projects on one checkout leave a session there resolving to whichever row ranked higher |
| another project already records the detected remote | one repository is one project |
| the project already belongs to a different remote | merging is the repair; rebinding is not |

Binding the same project to the same directory again succeeds and changes nothing, so the command printed by `ghost mcp status` is safe to re-run.

## Obsidian

### `ghost obsidian export`

Writes a one-way Markdown vault:

```bash
ghost obsidian export
ghost obsidian export --out ~/Documents/GhostVault
ghost obsidian export --project myproject --out ~/Documents/GhostVault
```

| Flag | Meaning |
|---|---|
| `--out <path>` | Vault directory. Defaults to `obsidian.vault_dir` or `~/Documents/GhostVault`. |
| `--project <name>` | Mirror one project plus global records. |

### `ghost obsidian sync`

Keeps the vault current by polling the database:

```bash
ghost obsidian sync --interval 30s
```

| Flag | Meaning |
|---|---|
| `--out <path>` | Vault directory. |
| `--project <name>` | Mirror one project plus global records. |
| `--interval <duration>` | Positive Go duration; defaults to `obsidian.interval` or `30s`. |

The sync opens the database read-only and is safe alongside a live MCP server. Press `Ctrl-C` to stop it.

## Context and benchmarks

### `ghost context`

Prints the passive session-start context block:

```bash
ghost context
ghost context --cwd /path/to/project
```

This is primarily used by the opencode adapter, which injects the returned block as instructions because opencode does not consume a stdout hook response.

### `ghost bench`

Runs the built-in retrieval-quality benchmark without a network call or LLM judge:

```bash
ghost bench
ghost bench --sweep
```

`--sweep` grid-searches the fusion parameters. See [Benchmarks and methodology](benchmarks.md).

## Scratch hygiene

### `ghost maintenance status`

Shows the live scratch root usage (bytes, file count) against the configured
`scratch.max_bytes` budget, followed by the most recent hygiene events — one
line per run with its timestamp, scratch bytes, and reaped counts:

```bash
ghost maintenance status
```

Budget checks that fired before a harness spawn (root was over budget, so
stale entries were reaped and possibly a loud warning emitted) are recorded to
the database; quiet under-budget spawns add no rows. `0` budget prints as
disabled.

### `ghost maintenance clean-scratch`

Reports pre-scratch-root debris — `~/.cache/ghost-tmp` and the shared system
temp dir's hidden `.<hex>-00000000.so` JIT-cache droppings — with counts,
bytes, and exact paths. Report-only by default:

```bash
ghost maintenance clean-scratch            # report: counts, bytes, paths
ghost maintenance clean-scratch --apply    # remove strict-signature matches
```

`--apply` removes only regular files matching the strict droppings signature
(never directories, even ones named like droppings), skips anything an
`lsof`/`fuser` probe reports as open or mmap'd, and — when neither probe tool
is available (Windows, minimal containers) — refuses to remove anything and
says so rather than risk deleting an open file.

## Maintenance and installation

### `ghost upgrade`

Checks GitHub Releases and replaces a standalone binary after verifying the release checksum:

```bash
ghost upgrade
```

A plugin-managed binary refuses this path because the plugin manager owns it; use `/plugin update` in Claude Code instead.

### `ghost version`

Prints the binary version:

```bash
ghost version
```

## Automatic lifecycle command

When automatic lifecycle work is enabled, the Stop hook may spawn:

```bash
ghost lifecycle <project>
```

This is an internal integration command rather than the normal way to start maintenance. It runs enabled phases in order—`reflect`, `resolve`, then `supersede`—and logs progress to the Ghost data directory. Each phase is bounded by the lifecycle timeout configured in `docs/configuration.md`.
