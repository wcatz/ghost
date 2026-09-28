# Ghost CLI reference

The `ghost` binary is both the MCP server and the maintenance CLI. Run `ghost help` for the built-in top-level summary, or `ghost help <command>` (including a two-word path such as `ghost help project bind`) for that one command's own usage on stdout — `ghost -h <command>` does the same. A name that matches no command is reported on stderr and the summary is shown instead — a help token in place of the name is a second help request rather than a typo, so it is answered with the summary and nothing else, and `ghost help -h <command>` names the command as `ghost -h <command>` does.

Every subcommand accepts `-h` or `--help`: it prints that command's usage on stdout and exits `0`, before anything with a side effect runs — no configuration load, no database open, no file written, no harness spawned. The top-level summary (`ghost help`, `ghost --help`) is unchanged by this and still prints to stderr.

Two spellings decide whether a flag is a request. The token after a flag that *this* command takes a value for is a value, never a help request — `ghost reflect --project -h` runs reflect for a project named `-h` — and a flag belonging to a different command is not a value at all, so `ghost upgrade --cwd -h` prints the upgrade usage instead of upgrading with the help flag swallowed as `--cwd`'s value. And a bare `--` ends the options for that scan: a token after it is an operand, never a help request, so `ghost reflect -- --help` runs reflect rather than printing usage. What the command then does with that operand is its own parser's business — none of them implements `--` (several report it as an unknown flag), so a project whose name looks like a flag is still addressed with the verbatim `--project <name>` form.

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

Registration means what each client reads: `claude mcp get ghost` for Claude Code, the `[mcp_servers.ghost]` table in `~/.codex/config.toml` for codex, the packaged `mcp.json` for goose, and the `mcp.ghost` entry in opencode's config file for opencode. For opencode, status judges your user-level layers — opencode's config directory (`$OPENCODE_CONFIG_DIR` when set, else `$XDG_CONFIG_HOME/opencode`) with **both** of its config spellings read as separate layers, `opencode.json` then `opencode.jsonc` (opencode loads both, and the later one wins a conflict), then `$OPENCODE_CONFIG` (a file path), then `$OPENCODE_CONFIG_CONTENT` (inline JSON), later sources overriding earlier ones key by key — and checks the entry is present, enabled, and resolving to the ghost binary on PATH; a per-checkout `opencode.json` and the `.opencode` directory are deliberately not judged, because either would make the verdict depend on the directory the command was typed in. The entry is the fallback: opencode's lifecycle plugin registers `mcp.ghost` at startup, so a broken entry is an informational warning while the plugin is current and an error only when the plugin is missing as well. A config layer status cannot read or parse is an error under either gate — opencode drops such a layer, so the entry in it is unknown rather than covered. When the entry itself is the error, the failing line names the file to edit and the edit that repairs it — the whole `mcp.ghost` block to paste when the entry is absent — because `ghost mcp init --client opencode` installs the lifecycle adapter and never writes opencode's config; when a layer could not be read or parsed, the line names that file instead.

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
| `--allow-drops` | Apply even when memories would be removed without a merge. Every category is under the drop guard, so without this flag an input memory no surviving memory explains is re-added verbatim instead of deleted. **Nothing is exempt**: an explicit `obsolete` drop, a rewrite, and a `superseded by <id>` drop are all audited the same way, and a row the model disposed of comes back unless a single output memory carries at least 45% of its tokens. A merge source is audited against the text of its OWN merge, not the whole result. A memory the harness simply never named is carried through unchanged, so a rewrite is not a free change of wording — its replacement has to carry the memory's substance, or the old row survives beside it until a later `ghost resolve` or `ghost supersede` demotes it. The cost of that is a possible duplicate; the alternative is a silent deletion with nobody watching. |
| `--promote-globals` | Promote cross-project candidates into `_global`; without this flag they remain project-scoped. |
| `--skip-unchanged` | Skip the LLM call when the consolidatable set is unchanged since the last applied pass. |
| `--full` | Print the full text of every memory the run reports, instead of the 120-byte preview. Display only — the result and the write are identical either way. |
| `--source <host>` | Explicit harness: `claude-code`, `opencode`, `codex`, or `goose`. |
| `--project <name>` | Project name instead of the positional form. Takes the next argument verbatim, so dash-prefixed names work. |

The `auto` tier uses the explicit source when provided, otherwise detects the calling harness. It does not silently switch to a different harness or billing path. When a source is known but its CLI binary is unavailable, auto can fall back to SQLite; the offline tier is also available for an explicit local run.

A harness-backed consolidation is asked for operations on the memory ids it is shown — `keep <id>`, `merge <id>,<id> -> <text>`, `rewrite <id> -> <text>`, `drop <id> reason: obsolete | superseded by <id>` — rather than for a rewritten list of memories. A memory leaves the corpus only through one of those operations: named as a merge source, named for a rewrite, or named in a drop with a reason — and only where a surviving memory accounts for it, or `--allow-drops` accepts the deletion. Naming an id is not by itself enough: a `rewrite` whose replacement does not carry the old row's substance leaves that row in the corpus verbatim, so a rewrite is not a free change of wording on an unattended run. Anything the response does not name is carried through unchanged, byte for byte, so it keeps its id, its embedding, its links and its age. A merge or rewrite that introduces a path, hash, version, hostname or number found in none of the memories it names is rejected and those memories are kept as they are, which is why a `rewrite` fixes a claim and never a specific. An operation Ghost cannot read, an id it did not supply, or a response carrying no operations fails that tier's result, and consolidation falls through to the next tier.

The report ends with an accounting of every input id, printed the same way for a dry run and for `--apply` and before the write, so a dry run previews it exactly:

```text
Inputs (7) accounted for; every count below is ids, so they add up to it:
Merges (3):
  new <- A1B2…02, A1B2…03, A1B2…04   (48 B from 141 B)
Refused by the grounding check (0):
Rewrites (1):
  A1B2…05 -> the ledger ingests through the bastion on port 2222, never 22
Dropped (1, each audited by the drop guard):
  A1B2…06 reason: obsolete — nothing in the result carries it; the drop guard re-added 1 row verbatim
Deleted (0):
Kept verbatim: 1    Passed through (not named): 1
```

Every input id appears in exactly one line or one count, and every count is a count of ids — a merge may name any number of sources, so `Merges (3)` above is three ids folded into one row, and the numbers add up to the input total. Every id is quoted in the spelling the database holds: the parser accepts any case (`memIDKey` compares case-insensitively) and normalises only a drop's successor target, so a response that spelled an id differently would otherwise put a key on the page that looks up nothing. That quoted-id discipline is what makes the count arithmetic checkable, and the reason an input a merge consumed can no longer disappear from the report. A merge line names no successor id because the merged row does not exist until `--apply` writes it; `ghost history <id>` shows the `related_id` that names it. `Deleted` is the set of rows an apply removes, one line each with the reason, and it is counted by the replace's own reuse pass rather than by asking whether the row's text is still in the result: reuse is content-keyed and claims **one** row per emission, so two inputs holding the same bytes both have their text in the result and only one of them is still there afterwards. A row nothing carries is a loss; a row whose identical twin was reused is a deduplication, and the knowledge is still in the project either way. That is the whole of the offline SQLite tier's absorptions, which name no ids and so appeared in no bucket of any report. A line the drop guard overrode says so, rather than leaving a drop to be read as a deletion that did not happen. A `superseded by` reason names the successor it replaces, so that id is quoted on the drop line and accounted for by its own count — the accounting is per input id, not per mention.

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

Each candidate is classified as `supersedes`, `causes`, `reversed`, or `neither`, with each note's creation timestamp in the prompt. A `supersedes` link only ever points from the newer note to the older one, so a `reversed` verdict — the classifier says the older note holds the current value and the newer one restates an obsolete claim — is reported and refused instead of written; `--apply` also invalidates any `supersedes`/`causes` link the pair already carries. A refused verdict is never recorded in the NEITHER cache, so the pair is not skipped on later passes. The default source is the calling harness. Applying the pass enables targeted demotion during search for genuine replacement pairs.

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

The source project's recorded checkout and repository do not survive it: the survivor keeps its own, and the source row is deleted. A merge is therefore how two records of one project become one, not how two checkouts are joined — `ghost project bind` is what records a checkout, and a project that already records a repository refuses to take on another. Joining a checkout to a project that was refusing it is a merge *and then* a bind, in that order, and the bind is refused until the merge has deleted the project that recorded the directory.

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

The file is created at `0600` **before** a byte of the snapshot is written to it, and the path is claimed with `O_EXCL`. Two things follow, both of which matter for an `--out` outside the `0700` data directory:

- **No window.** `VACUUM INTO` names no mode for the file it creates, so a copy it creates lands at SQLite's default minus the umask — `0644` under a permissive umask. Creating the file first means a full copy of the memory database is never group- or world-readable, not even briefly. SQLite accepts an existing *empty* file as a `VACUUM INTO` destination and keeps its mode; it refuses a non-empty one, which is what makes the emptiness the reservation guarantees into the thing SQLite checks.
- **No overwrite, and no symlink.** `O_EXCL` is the atomic claim, so there is no gap between "I looked and it was absent" and "I created it" for another writer or a symlink swap to slip into, and a dangling symlink at the destination is refused rather than written *through* to whatever it points at.

A vacuum that fails removes its own reservation, so a retry is not blocked by an empty leftover this run created.

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

These things are deliberately **not** exported:

| Excluded | Why |
|---|---|
| `memory_embeddings` | The vector is derived from the content by a local model, not stored knowledge. The embedding worker rebuilds it for every imported memory. |
| `memory_links` | A link only means something between two memories that are both present, so importing edges ahead of their endpoints would fail the foreign key or fabricate relationships. The linking worker recomputes related edges after an import. |
| `resolve_kept_hash` | A cache of the resolve classifier's verdicts keyed by content hash. Ghost recomputes it. |
| Ghost's own `_global` `builtin` seeds | The shipped rules this Ghost carries. Each is written under a per-install random id, so your copy and the destination's could never be recognised as the same row and the import would add a second one beside it — and nothing would ever remove it. The destination writes them itself, by content, on every open, so the restored store ends up with one copy, written by Ghost. A memory of your own filed under `_global` is **not** one of these: it is the only copy of itself, and it exports. |

That last row is why an export's memory count can be one lower than the row count `ghost backup` prints for the same store: the seed is in the database copy and deliberately not in the artifact.

### `ghost import`

Loads an artifact written by `ghost export`:

```bash
ghost import ~/backups/one.jsonl
ghost import ~/backups/one.jsonl --apply
ghost import ~/backups/one.jsonl --apply --trust-provenance
```

| Flag | Meaning |
|---|---|
| `--apply` | Actually write. Without it the command is a dry run that reports what it would create, skip and reject, and writes nothing. |
| `--trust-provenance` | Keep each memory's own source and pin state instead of downgrading them. Off by default; pass it when the artifact is **your own** export. |

A dry run is not a separate code path: every record goes through the same validation an apply run would, so the preview describes the run that follows rather than a similar one. It also opens the database **read-only** — `ghost import` without `--apply` cannot migrate a database whose schema is behind, or seed the builtin rows, while reporting "nothing written". Two consequences worth knowing:

- A dry run needs a database to preview **into**, so on a machine with no Ghost store yet it reports that rather than creating one. Start a session (or run `ghost mcp init`) first, as `ghost export` also requires.
- Because it cannot migrate, a store from an **older** Ghost is refused with a message naming both versions rather than failing on a missing column:

  ```text
  error: the database at ~/.local/share/ghost/ghost.db is at schema v11 and this Ghost reads v17
         — start a session, or run ghost mcp init, to migrate it before exporting it or
         previewing an import into it
  ```

  A store from a **newer** Ghost gets a different message — upgrade Ghost — because migrating backwards is not the fix. Both are checked by reading `PRAGMA user_version` on the read-only connection, so the check never writes.

  The check is strict: the store must be at exactly this Ghost's schema version, not merely at or above some floor. The only columns `export` and a dry run select that postdate v10 are `projects.repo_remote` (v11) and `memories.scope` (v12), so a v12–v16 store would in fact query fine — but a floor would hardcode which columns exist at which version, and the day a reader selects a newer column it would quietly admit a store that fails with a missing-column error again. So a v12–v16 store is asked to run one read-write open first, which is `ghost mcp init` or any session. `ghost backup` runs no version check of its own, but it is itself a read-write open: it reaches the store through the same path as a session, so it migrates and seeds the store it copies, and a store from a newer Ghost is refused outright. That makes it one way to *pay* this cost — run it once, then the export works — rather than a way around it. It is safe to run against a live MCP server, which is why it uses `VACUUM INTO` at all.

### Imported provenance

By default an imported memory is stamped `source = "onboarding"` and unpinned, whatever the artifact says:

```text
  create  memory:   line 3  "carried across machines" (a1) (provenance downgraded to onboarding, unpinned)
imported 1 memory from ~/backups/one.jsonl
  provenance downgraded to onboarding and unpinned — pass --trust-provenance to keep the artifact's own
```

The reason is that an artifact is a file that arrived from somewhere, and on its own authority it would otherwise be able to plant rows that read as yours or as Ghost's:

| The artifact claims | What the store would then treat it as |
|---|---|
| `source: "manual"` | the user's own words — excluded from consolidation by name |
| `source: "builtin"` | a rule Ghost ships — excluded from consolidation by name, and presented as Ghost's own |
| `pinned: 1` | exempt from consolidation whatever its source |

`onboarding` is the source `internal/claudeimport` already uses for memories brought in from outside Ghost at first contact, and it is in the database's `CHECK` already, so downgrading needs no migration. A downgraded memory stays ordinary: consolidatable, and honest about where it came from.

**Importing your own export? Pass `--trust-provenance`.** The rows come back with their own source and pin, which is what a restore wants. The asymmetry is deliberate: the cost of the default being wrong is planted provenance, and the cost of the flag being wrong is passing it once.

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

- **A record with no `created_at` or no `importance` takes the column's own default**, not a bound zero. A stored empty string makes `julianday('')` NULL, which makes the whole time-decay expression NULL and sorts the memory out of every ranked read — present in the store, invisible to recall. A *stated* `importance: 0` is kept as 0: a memory saved without an importance is stored as 0, exported as `"importance":0`, and re-importing it must not promote it to the 0.5 default.

Import does not run Upsert's near-duplicate probe. A restore is putting back what was there, not adding knowledge, and folding two rows of the artifact into one would silently drop a memory the user chose to keep.

## Context and benchmarks

### `ghost context`

Prints the passive session-start context block:

```bash
ghost context
ghost context --cwd /path/to/project
```

This is primarily used by the opencode adapter, which injects the returned block as instructions because opencode does not consume a stdout hook response.

### `ghost history <memory-id>` / `ghost history purge <memory-id>`

Prints one memory's append-only history: every insert, edit, reflection rewrite,
duplicate fold, resolve, supersession, restore, import and deletion, oldest
first.

```bash
ghost history <memory-id>                    # human-readable changelog
ghost history <memory-id> --limit 5          # the newest 5 entries
ghost history <memory-id> --json | jq .phase # one JSON object per entry

ghost history purge <memory-id>              # erase the row AND its history
```

```bash
ghost history <memory-id>                    # human-readable changelog
ghost history <memory-id> --limit 5          # the newest 5 entries
ghost history <memory-id> --json | jq .phase # one JSON object per entry
```

Each entry names when it happened, which write path made it (`save`, `update`,
`reflect`, `merge`, `resolve`, `unresolve`, `supersede`, `unsupersede`, `restore`,
`import`, `delete`, `baseline`), which agent and session performed it when the
write path knew — the lifecycle passes do not, and the entry says so rather than
inventing one — and the content, category, importance, `resolved_at` and source
the memory held once that write landed.

Two more fields appear when they apply: the memory on the other end of the event
(`related memory:` — what replaced a deleted row, or whose edge claims this one)
and the text a duplicate fold brought in and did not keep (`folded-in text:`).

The history outlives the memory: a deleted memory's last state is still readable
here, and a report that finds neither a row nor a history says so instead of
printing nothing. In `--json` form that refusal is a one-line `{"error": ...}`
object and a non-zero exit, never an empty stream a script would read as "no
history"; the live / no-longer-live distinction is not repeated per line, and does
not need to be — the last entry's phase says it, and a deleted memory ends in a
`delete` row.

`purge` is the exception, and it is the redaction path. Because the history keeps
the text a memory **used** to hold, deleting a memory that contained a credential
leaves that credential in the database and still readable with `ghost history`.
`purge` erases the text in both directions (the store primitive for the second
case is `Store.PurgeMemoryHistory`), and it also removes the reflection
snapshots naming the memory — otherwise `ghost reflect --restore` would bring the
row back, with its text and no history:

- a memory that is still there is deleted along with its history, in one
  transaction, so neither can survive the other;
- a memory that is **already deleted** has its history erased on its own. That
  second case is the one a delete-time purge cannot cover: the tombstone is the
  feature, so "erase that secret" asked an hour after the memory was deleted would
  otherwise report the memory as not found and leave the text where it is. The
  memory is not brought back — only the text goes.

The MCP equivalent is `ghost_memory_delete` with `purge_history: true`, in both
directions — including for a memory that is already deleted, where it purges the
recorded text without restoring the row; use it whenever the intent is to erase
something rather than to retire a memory. Neither can reach a backup taken before
the purge, or another machine's copy of the store. The MCP equivalent is
`ghost_memory_delete` with `purge_history: true`; use it whenever the intent is to
erase something rather than to retire a memory.

The command writes no memory, history or project row. It does open the store read-write — the same open `ghost maintenance status` and `ghost backup` use — so a database predating the history table is migrated by the open, and that migration first writes the full pre-migration backup copy it always takes. The strictly read-only opener is not used here because it refuses a store behind the current schema, which is exactly the store someone is most likely to run this against after upgrading.

Entries are kept per the growth policy in [architecture.md](architecture.md#memory-history): the newest 50 versions of one memory, and the newest 20 000 rows in the store.

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

Checks GitHub Releases and replaces a standalone binary after verifying the release:

```bash
ghost upgrade
ghost upgrade --allow-downgrade   # install a release older than this binary
```

A plugin-managed binary refuses this path because the plugin manager owns it; use `/plugin update` in Claude Code instead.

**What is verified.** The downloaded archive has to agree with two things the release publishes before a single byte reaches the installed binary:

1. the `digest` GitHub reports for that asset in the releases API (`sha256:<hex>`) — a digest GitHub computed for the bytes it holds, rather than one uploaded beside them. A missing digest, a digest for an algorithm this binary cannot compute, and a mismatch are all refusals, not warnings;
2. the release's `checksums.txt`, which every ghost release carries. The two checks are independent: `checksums.txt` is a second file in the same release, so whoever can replace the archive can replace the manifest that vouches for it.

The archive is checked before it is unpacked, so a substituted release is refused without its bytes ever being decompressed. Cryptographic *signature* verification (cosign, minisign, or GitHub artifact attestations against a key shipped in the binary) is not implemented — a digest from the release API proves the download matches what GitHub holds, not who published it.

**Ordering.** The release tag and the running version are compared as semantic versions, so a release older than the one already installed is refused rather than installed. `--allow-downgrade` turns that refusal into a deliberate install (with a warning on stderr) for a release that was withdrawn, or a build that has to be pinned while a newer one is investigated; it does not re-install the release you already have. A version that cannot be ordered — a `dev` build, a tag that is not a semver — keeps upgrading unless it is identical to the release tag (ignoring a leading `v`), which reports up to date as before; the digests still have to agree before anything is replaced.

**Bounds.** Every request carries a context deadline (30s for the release lookup, 10 minutes for an asset transfer) that a caller can shorten or cancel, and every response is size-capped — 4 MiB of release metadata, 1 MiB of `checksums.txt`, 200 MiB of archive, and 128 MiB of what an archive inflates into. For a `.tar.gz` that last cap covers *every* entry, not just the binary: opening one inflates the entries ahead of it too, so a few KiB of highly compressible data in `README.md` would otherwise expand without limit. A stalled connection, a hung server or an oversized body fails the command instead of hanging or exhausting memory.

**Archive formats.** Windows releases ship a `.zip` and everything else a `.tar.gz`; both are unpacked, and the container decides which, not the file name. Only a regular, non-empty `ghost` or `ghost.exe` at the archive root is accepted — a directory or link entry carrying that name has no body, and installing one would leave a zero-byte executable behind a "Updated" line.

**Replacing a running binary on Windows.** Windows holds a running executable's image open, so a new binary cannot be renamed over it. The old one is renamed to `<binary>.old` first — which Windows does allow — and the new one takes the path it vacates. If the second step fails, the old binary is moved back, so a failed upgrade cannot leave an install with no binary. The `.old` file is this process's own image and cannot be deleted until the process exits, so it stays until the next upgrade reuses the name; delete it once no `ghost` is running. Unix renames over the target atomically and leaves nothing behind.

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
