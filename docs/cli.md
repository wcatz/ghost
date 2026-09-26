# Ghost CLI reference

The `ghost` binary is both the MCP server and the maintenance CLI. Run `ghost help` for the built-in top-level summary.

Every subcommand accepts `-h` or `--help`: it prints that command's usage on stdout and exits `0`, before anything with a side effect runs — no configuration load, no database open, no file written, no harness spawned. The top-level summary (`ghost help`, `ghost --help`) is unchanged by this and still prints to stderr.

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

Status also lists any project that records no usable checkout and no repository remote, with the `ghost project bind` command that repairs it. Those projects are not a health failure — every check above can pass while sessions in such a checkout silently get no injected context — and the section is omitted entirely when there is nothing to fix. The listing reads the database without opening it for writing, so a status run never creates the store it is reporting on. The section also names the follow-up step for the standalone, init-managed Claude integration: after binding, `ghost mcp init` writes the per-checkout memory redirect that a newly absolute path makes the redirect check expect — unless that checkout has a `MEMORY.md` of its own, which init only overwrites when the content looks like a stale Ghost redirect.

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
| `--allow-drops` | Apply even when memories would be removed without a merge. Every category is under the drop guard, so without this flag any input memory the consolidation never referenced is re-added verbatim instead of deleted. |
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
ghost resolve myproject --reassess
```

| Flag | Meaning |
|---|---|
| `--apply` | Stamp `resolved_at` on confirmed memories. |
| `--reassess` | Re-judge memories that are already resolved instead of unresolved ones. |
| `--source <host>` | Classify through `claude-code`, `opencode`, `codex`, or `goose`. |
| `--project <name>` | Project name instead of the positional form. Takes the next argument verbatim, so dash-prefixed names work. |

The classifier is KEEP-biased in code, not only in prose. A local keyword prefilter proposes candidates, then a deterministic veto settles a candidate as KEEP with no harness call when its text carries a standing imperative (`never`, `do not`, `don't`, `must`, `always`, `required`) or an open marker (`not yet`, `outstanding`, `still pending`, `still open`, `still stale`, `unresolved`, `todo`) — case-insensitive and word-bounded. The prompt asks whether an agent starting a fresh session would make a mistake, repeat work, or break a rule without the note, and states that a date, PR number, commit hash, or "fixed in" does not resolve one. A `RESOLVED` verdict must carry a `closed-by:` fact naming what made the note obsolete; a verdict without one is read as KEEP, so a note the harness cannot explain away stays injectable.

Only explicit KEEP verdicts enter the content-hash cache (`memories.resolve_kept_hash`, prefix `v3` — `v1` and `v2` entries are re-asked, since they predate the veto and the `closed-by` rule); missing or garbled verdicts are counted as UNKNOWN and retried on a later pass. It fails when no source-specific harness can be selected; it never silently falls back to another harness.

#### `--reassess`

Re-runs the vetoes and the classifier over the memories that are **already** resolved, and repairs the ones that now come back KEEP:

```bash
ghost resolve myproject --reassess           # preview: prints the list
ghost resolve myproject --reassess --apply   # clear resolved_at on those rows
```

It is how a wrong resolution gets undone — the ordinary pass never looks at a row that already carries `resolved_at`, so a rule it buried was invisible to every later pass. The repair pass skips the keyword prefilter on purpose: the pool is already the small resolved subset, and a wrongly resolved note is usually hidden by the *absence* of a resolution keyword or by a narrative that reads like a fix. With `--apply` it clears `resolved_at` so the notes return to ranked session-start injection, and records the classifier's KEEP verdicts in the content-hash cache so the ordinary pass does not ask about them again. A classify failure is fatal and repairs nothing; a clear failure is fatal too and clears nothing either, because every batch runs in one transaction. An UNKNOWN verdict leaves `resolved_at` alone and is offered again. The stop hook's lifecycle phase never passes `--reassess`: it is an operator command.

Rows that the ordinary pass would re-stamp for free are left alone and reported as **still asserted by a link or correction**: the older endpoint of a live `supersedes` link, a row an unresolved correction still pairs with, and a row whose own correction is being repaired in the same run (clearing that correction would put it back in the pool and re-assert the pairing on the next pass). Clearing any of those would print a repair that the next ordinary pass immediately undoes.

Only rows the ordinary pass would actually consider are held back: its pairing mechanism pairs the keyword-prefiltered subset, so a durable rule with no resolution keyword is repaired even when a correction being cleared in the same run shares its subject tokens. A hold is not a soft warning — it is reported as asserted on every later run, so the filter keeps the pass from parking a row nothing asserts.

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

The first argument is an existing project **id** — not a name or a path. A project that records no usable location cannot be resolved from a directory, so it gets no session-start context and no Stop-hook lifecycle work; this is the command that repairs that, and `ghost mcp status` lists every project that needs it. Pass `-h` or `--help` for this section.

It is also the repair for a checkout that has moved or been deleted, which `ghost mcp status` does *not* report: the status notice tests the recorded path's shape, not whether the directory still exists, so a moved checkout needs this command with its new path.

The directory is made absolute and cleaned, must exist and be a directory, and is stored as its **physical** path — symlinks resolved, because that is the directory a session reports. Ghost also records the checkout's Git remote when the project records none, so a second worktree of the same repository resolves to the same project.

The command refuses, writing nothing, when:

| Refusal | Reason |
|---|---|
| the project is `_global` | it holds every project's memories, not a checkout |
| the project id does not exist | bind never guesses which project was meant |
| the path is missing, is not a directory, or is the filesystem root | a path that cannot be compared against a session directory would never resolve |
| another project already records that directory | two projects on one checkout leave a session there resolving to whichever row ranked higher; the same directory reached through a symlink counts as already recorded |
| the path contains another project's checkout | a recorded path matches by prefix, so binding `~/git` while a project records `~/git/infra` would hand that project every unregistered clone beneath it |
| the path is inside another project's checkout, and that project has no remote | that project answers for every directory beneath it, clones of unrelated repositories included, so nesting a second project there entrenches an overlap only a repository identity resolves. Give the enclosing project a remote and the same bind succeeds: a session in a different repository then contradicts it, so a nested checkout — a submodule, a vendored repository — is legitimately its own project |
| path resolution could never match the path | resolution only considers recorded paths longer than ten characters, and refuses a tie for the longest match, so binding one would record a project no session can find |
| another project already records the detected remote | one repository is one project (git-ssh and https spellings normalize to the same remote) |
| the project already belongs to a different remote | merging is the repair; rebinding is not |

Binding the same project to the same directory again succeeds and changes nothing, so the command printed by `ghost mcp status` is safe to re-run. "The same directory" is compared as text, not as location: a row some earlier writer left as `/x/checkout/` is invisible to path resolution — the candidate query matches stored text, and that spelling is neither equal to a session's `/x/checkout` nor a prefix of it — so bind rewrites it to the spelling resolution can return rather than deciding the two are the same location. Such a project is not in the unbound notice either, so this is the only repair.

A successful bind that records a directory is followed by one more step **on the standalone, init-managed Claude Code integration**: run `ghost mcp init` to write that checkout's memory redirect. The installer only writes redirects for projects with an absolute path, and `ghost mcp status` counts every project that has one — looking under the path currently recorded — so until init runs again the redirect check reports the repair as incomplete. That includes **re-pointing** a project at a moved checkout: the redirect on disk is under the old directory, so status goes red exactly as it does after a first bind. A re-run that records the same directory again changes nothing and needs no step. This applies to neither of the other setups: the ghost Claude Code plugin manages its own wiring (its `mcp init` returns early writing nothing, and `mcp status` returns before the redirect check), and the opencode, codex and goose installers have no redirect at all.

One case needs action before init will write anything: `writeRedirects` decides by file content, not by who wrote the file. It skips a `MEMORY.md` that does not contain the `stored in Ghost` marker, and rewrites one that does when it still carries the stale `ghost_list_projects` tool-call marker — so a checkout whose own `MEMORY.md` still holds an old Ghost redirect can be replaced, while one that merely mentions Ghost is left alone. Merge or remove such a file first. `mcp status` counts a skipped project as not redirected and prints the same line as any other failure, so the bind output states the condition init uses rather than promising a redirect that will not appear. The bind output names the installation the step belongs to as well.

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

## Backup, export and import

The memory database is a plain SQLite file, and these three commands are the supported way to move it around. `backup` and `export` answer different questions — a **backup** is a restorable copy of the database; an **export** is an inspectable artifact you can read, edit and diff — and neither is built on the other.

### `ghost backup`

Writes a consistent snapshot of the live database, safe to run while the MCP server is using it:

```bash
ghost backup
ghost backup --out ~/backups/ghost.db.snapshot
```

| Flag | Meaning |
|---|---|
| `--out <path>` | Where to write the snapshot. Defaults to a timestamped file beside the database in the data directory. The path must not already exist. |

The snapshot is taken with SQLite's `VACUUM INTO`, which reads one consistent snapshot of a live WAL database. Copying the file by hand cannot: copying `ghost.db` without its `-wal` loses whatever the write-ahead log held, and copying both while a write lands can capture a torn state. The write lock is held for the length of the vacuum and no longer.

The command prints the path, the file size, and the row count of each table it counted, so a restore can be checked against what the file actually contains:

```text
backed up ~/.local/share/ghost/ghost.db.backup-20260926T153207Z (204800 bytes)
  projects:     1
  memories:     4
  memory_links: 1
  tasks:        1
  decisions:    1
```

The snapshot is created at `0600` — a full copy of the memory database is no wider than the database itself — and an existing path is never replaced, because the file already there is the previous backup.

To restore: stop Ghost, remove `ghost.db`, `ghost.db-wal` and `ghost.db-shm` from the data directory, move the snapshot in as `ghost.db`, and start Ghost again. Ghost migrates the restored file on the next open, taking a pre-migration copy first.

### `ghost export`

Writes memories, tasks, decisions and projects as JSON Lines:

```bash
ghost export
ghost export --project myproject --out ~/backups/myproject.jsonl
ghost export --out - | head -3
```

| Flag | Meaning |
|---|---|
| `--project <name>` | Export one project, matched by name or id **exactly**. No path-prefix or basename fallback: a filter is a choice about what to copy, and one that resolved like project resolution could select a different project on another machine than the one named here. A filter that matches nothing is an error. |
| `--out <path>` | Where to write it. Defaults to a timestamped `.jsonl` beside the database. `-` writes to standard output, with the summary on stderr so the stream stays pipeable. |

The file is JSON Lines: one self-describing object per line, opened by a header line carrying the schema version.

```json
{"type":"header","schema_version":1}
{"type":"project","project":{"id":"p1","path":"/src/p1","name":"one","repo_remote":"github.com/wcatz/one","created_at":"…","updated_at":"…"}}
{"type":"memory","memory":{"id":"…","project_id":"p1","category":"gotcha","content":"…","importance":0.5,"source":"manual","pinned":false,"created_at":"…","scope":{"environment":"production"}}}
{"type":"task","task":{"id":"…","project_id":"p1","title":"…","status":"pending","priority":2}}
{"type":"decision","decision":{"id":"…","project_id":"p1","title":"…","decision":"…","rationale":"…","status":"active"}}
```

A memory record carries every column of the row that describes the memory: category, importance, access count, pin state, source, tags, created/updated timestamps, `resolved_at`, the validity triple (`valid_from`, `valid_until`, `verified_at`), the provenance fields (`agent`, `session_id`, `source_ref`, `confidence`) and `scope`. A project record carries its id, path, name, repository remote and timestamps.

Two exports of an unchanged database are **byte-identical**: projects come first, then memories, tasks and decisions, each in id order, and the header carries no timestamp. An artifact can therefore be diffed against the previous one, and a diff shows only what changed in the store.

Three things are deliberately **not** exported:

| Excluded | Why |
|---|---|
| `memory_embeddings` | The vector is derived from the content by a local model, not stored knowledge. The embedding worker rebuilds it for every imported memory. |
| `memory_links` | A link only means something between two memories that are both present, so importing edges ahead of their endpoints would fail the foreign key or fabricate relationships. The linking worker recomputes related edges after an import. |
| `resolve_kept_hash` | A cache of the resolve classifier's verdicts keyed by content hash. Ghost recomputes it. |

### `ghost import`

Loads an artifact written by `ghost export`:

```bash
ghost import ~/backups/one.jsonl
ghost import ~/backups/one.jsonl --apply
```

| Flag | Meaning |
|---|---|
| `--apply` | Actually write. Without it the command is a dry run that reports what it would create, skip and reject, and writes nothing. |

A dry run is not a separate code path: every record goes through the same validation an apply run would, so the preview describes the run that follows rather than a similar one.

```text
  skip    project:  line 2  "one" (p1)
  create  memory:   line 3  "the WAL holds a transaction…" (3A6B…)
would import 1 project, 1 memory from ~/backups/one.jsonl

nothing written — pass --apply to import
```

Rules the import follows:

- **A record whose id already exists is skipped, never overwritten.** The artifact is the older of the two copies by construction, so overwriting would restore stale data over live data. This also makes re-running an import always safe, which is how you repair a run that rejected a record.
- **A file whose schema version this build does not read is refused outright**, in either direction. Reading a newer one would insert records whose fields this build interprets by guesswork.
- **Imported memory content goes through the same length cap and the same validation as a normal save** — category, source and importance are checked against the schema's own value sets, and over-long content is cut at `MaxContentLen` with the same explicit marker. An artifact from another machine is another way to reach the memories table, and must not be a way around its rules.
- **Projects are created when missing**, and records are applied projects first, then memories, tasks and decisions, so a file whose records were reordered by an editor still imports. A `blocked_by` or `superseded_by` pointer is only honoured when the record it names is in the same artifact and is applied first; a pointer to a record the artifact does not contain, or one inside a cycle, is dropped.
- **A record that cannot be imported is rejected and the run continues**, so one hand-edited line does not abandon the rest of a large artifact. Rejections are counted and the command exits non-zero, so a partial import is never reported as a complete one.
- **A line that will not parse is rejected on its own.** A truncated, badly merged or hand-edited artifact still imports every record around the damage, and the bad line is reported with its line number. This is what the line-per-record shape buys: a file-level refusal is reserved for the problems that are not one line's — a missing or unreadable schema version, a second header, an unknown record type — where applying part of the file would mean importing data whose meaning is a guess.
- **A project this store already has is adopted, not duplicated.** Project ids are per-install, so an artifact from another machine names a project the destination has never seen while the destination very often has its own project for the same checkout or the same repository. Both `path` and `repo_remote` are unique, so inserting the artifact's project would collide — and because every memory, task and decision names the artifact's project id, one collision would take the whole file with it. Instead the records are attached to the project that is already there, and the report line says so:

  ```text
    skip    project:  line 2  "thing → ghost (this store already records that checkout or repository)"
    create  memory:   line 3  "the WAL holds a transaction…" (3A6B…)
  ```

  A collision that survives that — another writer claiming the same directory between the plan and the write — is refused by name, in the dry run as well as the apply:

  ```text
  error: project laptop-1 cannot be imported: this Ghost already records /src/thing as project ghost — import into that project, or merge it with `ghost project merge`
  ```

- **A record with no `created_at` or no `importance` takes the column's own default**, not a bound zero. A stored empty string makes `julianday('')` NULL, which makes the whole time-decay expression NULL and sorts the memory out of every ranked read — present in the store, invisible to recall. An artifact this build writes always states both fields, so this only matters for a hand-edited record.

Import does not run Upsert's near-duplicate probe. A restore is putting back what was there, not adding knowledge, and folding two rows of the artifact into one would silently drop a memory the user chose to keep.

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

## OpenCode sessions

### `ghost opencode cleanup-sessions`

One-shot cleanup of the lifecycle sessions OpenCode stored before the child's
data directory was isolated (#588): it targets sessions whose title is
**exactly** `[ghost]` and whose most recent activity is older than a grace
period. Sessions with any other title — including `[ghost] follow-up` or a
different casing — are never touched. Report-only by default:

```bash
ghost opencode cleanup-sessions                 # count what would go
ghost opencode cleanup-sessions --grace 24h     # older than a day instead of 1h
ghost opencode cleanup-sessions --apply         # delete them
```

| Flag | Meaning |
|---|---|
| `--grace <duration>` | Minimum age before a session is eligible, measured from its most recent activity (`created`/`updated`, whichever is later). Default `1h`; `0` accepts any age. |
| `--limit <n>` | How many sessions `opencode session list` is asked for. Default `20000` (`0` means that default, unlike `--grace 0`); a list that reaches the limit prints a truncation warning instead of pretending it is complete. |
| `--apply` | Perform the deletes. Without it nothing is deleted. |

Sessions are read through the real `opencode` binary (`session list --format
json`), so — like your own `opencode session list` — the run is scoped to the
current project: run it from the checkout whose sessions you want cleaned. The
child receives Ghost's harness environment allowlist, whose OpenCode-specific
entry is `OPENCODE_API_KEY`, so a store override such as `OPENCODE_DB` in your
shell does not reach it (opt in with `GHOST_PASSTHROUGH_ENV`, which re-exposes
whatever else you name). Timestamps are read as Unix milliseconds on the CLI's
word alone, so a timestamp that is missing or outside a plausible window
(before 2020-01-01, or more than five minutes ahead of the clock) is refused
instead of being read as "very old": a unit or session-JSON-shape change in
OpenCode's output fails closed on one session rather than making every session
look eligible, and the command prints a `warning:` line with the number it
skipped so the refusal never reads as an empty, clean run. To verify the unit
against your real CLI — `GHOST_LIVE_TESTS=1 go test ./internal/ai/ -v
-run TestOpenCodeSessionList_TimestampsAreMilliseconds`; it lists read-only,
resolves the binary the same way this command does (`cli.opencode_binary`,
else `PATH`), skips when your checkout has no sessions to inspect, and *fails*
when sessions are listed but none carries a timestamp — that is the shape
drift, not an empty checkout. Deletion uses `opencode session delete <id>` with
up to three attempts per session; a failed delete is reported and counted but
never stops the rest, and the command exits non-zero when any delete failed.
The timestamp and truncation warnings do not change that exit status: the run
did what it was asked, so read the report. The binary comes from `cli.opencode_binary`
(`GHOST_CLI_OPENCODE_BINARY`) when configured, otherwise from `PATH`.

New lifecycle sessions no longer need this command: since #568 the opencode
child runs with its data directory inside Ghost's invocation-owned scratch
tree, so it writes to its own store rather than the user's (see
[Harness subprocess environment](configuration.md#harness-subprocess-environment)).
This command exists for the backlog that predates that isolation.

## Maintenance and installation

### `ghost upgrade`

Checks GitHub Releases and replaces a standalone binary after verifying the release checksum:

```bash
ghost upgrade
```

A plugin-managed binary refuses this path because the plugin manager owns it; use `/plugin update` in Claude Code instead.

The release tag and the running version are compared as semantic versions, so a release older than the one already installed is refused rather than installed. A version that cannot be ordered — a `dev` build, a tag that is not a semver — keeps upgrading unless it is identical to the release tag (ignoring a leading `v`), which reports up to date as before; the checksum still has to agree before anything is replaced. Requests carry a deadline (30s for the release lookup, 10 minutes for the archive) and every response is size-capped — 4 MiB of release metadata, 1 MiB of `checksums.txt`, 200 MiB of archive — so a stalled or oversized download fails instead of hanging. The archive cap bounds what is transferred; the binary extracted from it is not separately capped.

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
