# Configure Ghost

Ghost works with compiled defaults and no required configuration file. The first command that opens the store may create the user config file from the embedded example.

## Configuration precedence

Later layers override earlier layers:

1. Compiled defaults
2. `/etc/ghost/config.yaml`
3. The user config file
4. `GHOST_*` environment variables
5. Supported command-line flags, applied by the command after loading

## Invalid configuration

A config file that exists but does not parse is never ignored. The error names the file and the line, so it is reported differently depending on who asked for the configuration:

| Caller | Behaviour |
|---|---|
| CLI subcommands (`ghost reflect`, `ghost resolve`, `ghost supersede`, `ghost obsidian …`, `ghost maintenance status`, `ghost project …`) | Exit non-zero with the parse error. Nothing is run against half the intended configuration. |
| The `ghost mcp` server | Warn on its log channel — stderr, or `GHOST_LOG_FILE` when set — and serve the environment plus the compiled defaults. It does not exit: that would not fail a command, it would leave your editor with no Ghost tools at all, because of a typo in a file you may not know exists. The warning sink follows the server's log channel rather than raw stderr, so setting `GHOST_LOG_FILE` keeps it out of the MCP client's face. |
| Host-session hooks (SessionStart injection, the stop hook, obsidian auto-sync, session routing) | Report the same error on stderr and continue with the environment plus the compiled defaults. A typo in the config must not fail the session you are currently working in. |
| `ghost mcp status` | Prints the path as informational, then a `!` line carrying the parse error. The remaining checks then run on the defaults, so nothing is missing from the output — but the run is **not** marked unhealthy, because the `!` line is the pointer, not a verdict. |

"Environment plus the compiled defaults" means every layer that does not read a config file. The `GHOST_*` variables still apply, so a broken file cannot undo an opt-out you set in the environment (`GHOST_EMBEDDING_ENABLED=false`, `GHOST_SCRATCH_MAX_BYTES=0`). Only the file layers are lost.

A file that exists but cannot be **read** — wrong permissions, or one owned by root — is a warning and is skipped, as it always was; only a file that is readable and does not **parse** stops a command.

```text
ghost: config: parse /home/you/.config/ghost/config.yaml: yaml: line 3: found unexpected end of stream — falling back to the environment and built-in defaults
ghost: config: cannot read /etc/ghost/config.yaml: open /etc/ghost/config.yaml: permission denied — skipping it
```

## Unknown keys

A key in a config file that no setting binds — a typo such as `linking.thresholdd` — is a warning on stderr rather than a failure: it cannot affect anything, so it should not stop a session, and the other keys in the same file still load.

```text
ghost: config: /home/you/.config/ghost/config.yaml: unknown key(s) ignored: linking.thresholdd
```

This check covers **config files only**, and the limits are worth knowing:

- A misspelled `GHOST_*` variable is still ignored silently. Ghost cannot report it, because most of its variables are not config keys at all — `GHOST_DEBUG`, `GHOST_LOG_FILE`, `GHOST_SCRATCH_DIR`, `GHOST_PASSTHROUGH_ENV` and `GHOST_OPENCODE_MODEL` are documented here or in the harness section below, and none of them binds a `Config` field.
- A `GHOST_*` value that cannot be read as its key's type is an error naming the variable, and only that variable is skipped — the rest of your environment still applies. This is true of the generic mapping too, not just the explicit shortcuts in the table below: `GHOST_EMBEDDING_DIMENSIONS=abc` is reported and ignored, and `GHOST_EMBEDDING_ENABLED=false` beside it still takes effect.

Each distinct warning is printed once per process, so a SessionStart hook and a `ghost mcp status` in the same session do not repeat the same line. A second, different problem is still reported.

## File locations

The user config file is normally:

| Platform | Path |
|---|---|
| Linux | `$XDG_CONFIG_HOME/ghost/config.yaml`, or `~/.config/ghost/config.yaml` when `XDG_CONFIG_HOME` is unset |
| macOS | `$XDG_CONFIG_HOME/ghost/config.yaml`, or `~/Library/Application Support/ghost/config.yaml` when `XDG_CONFIG_HOME` is unset |
| Windows | `%AppData%\ghost\config.yaml` when `XDG_CONFIG_HOME` is unset |

The data directory is separate from the config directory:

| Platform | Default |
|---|---|
| Any | `$XDG_DATA_HOME/ghost/ghost.db` |
| Any, when `XDG_DATA_HOME` is unset | `~/.local/share/ghost/ghost.db` |

The data path is intentionally consistent across operating systems. A Windows installation therefore commonly uses `%USERPROFILE%\.local\share\ghost\ghost.db` for the database, while the config file follows the platform convention above.

### Data directory permissions

Ghost treats the database as private to your account. Whenever it writes to the database — starting the MCP server, running a command, or a Claude Code session starting — it strips the group and other permission bits from:

- the data directory itself (`ghost/`, which becomes `0700`), and
- `ghost.db`, `ghost.db-wal` and `ghost.db-shm` (which become `0600`).

Why the modes needed enforcing at all: `MkdirAll` only applies its mode when it creates a directory, so a directory that already existed keeps whatever mode it had, and SQLite names no mode for the files it creates, so a new database takes your umask. A data directory that something outside Ghost created first — a packaging script, a pre-existing `XDG_DATA_HOME`, a `mkdir` you ran by hand — was therefore typically `0750` or `0755` instead of `0700`, and a `0644` or `0640` `ghost.db` beside it: the whole memory store readable by everyone in your group. The pass above runs on the open path, so it also repairs a mode that drifts later.

Three things it deliberately does not do:

- **It never widens a mode.** Only group and other bits are cleared, so a database you locked down yourself — `chmod 0400 ghost.db` — is left alone rather than handed back `0600`. The setuid, setgid and sticky bits are not preserved either: a data directory that was group-shared with `setgid` (`2775`) loses it, which is the intended direction — the directory is no longer shared.
- **It touches nothing else.** Every other file in the directory keeps the mode it has. A pre-migration backup Ghost writes during a schema upgrade (`ghost.db.pre-migrate-<timestamp>`) is tightened to `0600` as it is created, since it is a full copy of the database; backups from earlier versions keep the modes they were given.
- **It does not follow a symlinked `ghost.db`.** If your `ghost.db` is a symlink — a synced data directory, a dotfiles checkout — the pass skips it, because chmod'ing through the link would change the mode of whatever it points at, which may not be yours to change. It logs a warning naming the path, since a symlinked database means the real one keeps whatever mode it has. Point Ghost at the database directly if you want it protected.

A database Ghost opens outside the data directory — an `eval` or bench scratch tree, a directory you pointed an environment variable at — has its three files tightened but not the directory holding it.

A mode that cannot be tightened (a read-only or foreign-owned mount) is logged as a warning and the command continues. Refusing to run over a mode bit would be the worse outcome.

The pass runs on the two functions that open the database read-write, so which commands tighten a mode follows from which of them they use:

What decides it is the *open*, not the command. A read-write open tightens wherever it is called; a read-only one never does.

- **Read-only opens change nothing.** The stop hook's own database reads, the lifecycle marker, the lifecycle lock and `ghost obsidian sync` all connect read-only. A diagnostic has to be able to report on a database it cannot modify, and a read-only connection cannot create one either.
- **Read-write opens tighten**, including commands whose job is only to read. `ghost mcp status` and `ghost maintenance status` open the database read-write to check store health and report recent runs, `ghost project bind` opens it to write the binding, and the opencode plugin's `ghost context` opens it to render a session's context. Most other commands reach it through the same shared startup that already ran migrations on their behalf.

A session *start* can tighten as a side effect, but only on some hosts. Claude Code and Codex go through `ghost hook`, where a genuine new session in a directory that resolves to a project bumps the project's session counter — a write — while a resume, a clear, a compaction and a subagent fire do not. Goose's session start tightens nothing: it cannot consume injected context, so Ghost does not process it. opencode instead spawns `ghost context` at start, which has none of those exclusions. Separately, the first session start after a Claude Code *plugin* install imports your memory files, which is a read-write open whatever the session's shape.

A session *stop* can tighten indirectly, on any host that has one: when lifecycle reflection is configured, the stop hook spawns `ghost lifecycle`, which runs in its own process with its own open.

On Windows the pass is skipped entirely: access there is carried by an ACL inherited from the parent directory, not by the mode bits `chmod` maps onto read-only, so tightening a number would not change who can read the database.

### Data directory growth

Two kinds of file in the data directory grow on their own, and Ghost bounds both.

**Pre-migration backups.** A schema upgrade writes one full copy of the database beside it first, named `ghost.db.pre-migrate-<unix timestamp>`. That copy is the only way back if a migration step destroys something, so it is written before anything runs and an upgrade that cannot write it does not start. Once it exists, Ghost keeps the **three newest** copies of that database and deletes the older ones — the new one plus two upgrades of hindsight, rather than one per upgrade forever.

- The match is exact: only `ghost.db.pre-migrate-<digits>` regular files are considered, ordered by the timestamp as a number (a string sort would rank `999` above `1003`). A directory, a symlink wearing the name, or a file with any other suffix is left as it is — a symlink is never followed, so pruning cannot delete what it points at.
- **The copy the upgrade just wrote is excluded by name**, before any ordering happens, and holds one of the three slots. Deciding "the newest stamp is mine" instead would let a backup stamped further ahead than this machine's clock — a data directory restored from a machine that ran ahead, a corrected clock, a file you renamed — push the one file with a way back out of the set and delete it before the migration ran.
- Nothing else in the directory is in scope. Hand-made copies (`ghost.db.backup-…`), backups taken before an earlier version's naming (`ghost.db.pre-0.33.0-…`), the database itself and its `-wal`/`-shm` sidecars are never touched.
- Deletion is best effort. A file that cannot be removed is logged as a warning and the migration continues; a cleanup pass failing is not a reason to hold up an open.

Backups are only culled when this open has just written a fresh one, so an ordinary open — no upgrade to run — leaves every existing backup exactly where it was.

**Logs.** The logs Ghost appends to in the data directory — `lifecycle.log` and `obsidian-sync.log`, plus any phase log a build writes — are opened through one helper that rotates them at **5 MiB**: an existing file at or above the cap is renamed to `<name>.1`, replacing the previous `.1`, and appending continues in a fresh file. One rotation, one spare copy, instead of a chain of them. Rotation happens where the log is opened, so the cap applies to every writer of that file.

- Rotation is best effort for the same reason: it fails open wherever it can. The rename happens *before* anything is opened — Windows refuses to move a file another handle holds, so a descriptor held across the rename would make the cap Linux-only — and every failure that can degrade does. A rename that cannot happen (a directory at `<name>.1`, a read-only mount) leaves the log exactly where it was and the open proceeds as it did before rotation existed; if the fresh file then cannot be created — a disk that just filled up, which is the case rotation exists for — and the log's own name is *gone*, the line is written through the rotated copy instead of being lost or reported as an error, because every caller treats the log as diagnostic and the spawn sites would otherwise drop the spawned process's stdout entirely.
- The fallback needs the name to be gone, not merely unopenable: with no file under the name, `<name>.1` is either where the rename just put the recent lines or the only copy left, so it is the right place for a new run's output. A name that still exists but will not open — wrong permissions, a directory wearing it — is reported as an error instead, since the lines are there and `<name>.1` beside it is an earlier generation that a new run must not be spliced into.
- While the fallback lasts, the log's own name does not exist — and it stays gone for as long as the filesystem has no room to create it, so "the next open recreates it empty" is only true once space returns. Until then every open takes the same path again: the line still lands in `<name>.1` (appending to an existing file allocates nothing), and each open logs the same warning naming both paths. Anything tailing `<name>` by name therefore sees nothing while the disk is full and finds the file reappearing, empty, when the disk recovers; the lines it missed are in `<name>.1`. Ghost warns because this is the one degradation it cannot hand back to the caller.
- During that window `<name>.1` is the file that grows, and nothing rotates it again — rotation only ever looks at `<name>`, which does not exist — so for as long as the fallback lasts the 5 MiB cap holds only once the name returns. Ghost keeps appending rather than refusing or truncating: a disk that is out of blocks refuses these writes as well, and a diagnostic log dropping its lines to enforce a size target would be the wrong trade. The condition is reported on every open, so a copy outliving its cap is visible rather than silent.
- Two cases still return an error: there is no rotated copy to fall back to — the name exists but would not open, or `<name>.1` is absent or not a regular file — and that error names the fresh path; or both opens fail, and that error names both paths and both causes.
- A symlink wearing the log's name is appended through but never renamed; untangling what it points at is yours.
- A run whose output spans a rotation is split: the lines written before it are in the rotated copy, and the rest follow the descriptor the process was handed. If the log rotates a *second* time while that run is still going — the previous `.1` is replaced — anything it writes after that point goes to an inode nothing will read. That content is diagnostic, so the trade is deliberate; a run that long is exactly the one you want bounded.
- If you want history past two generations, rotate or archive the files yourself — Ghost only guarantees the cap.

Earlier versions wrote `reflect.log`, `resolve.log` and `supersede.log` directly; those phases now log to `lifecycle.log`, so files with those names are leftovers of a retired code path. Ghost does not delete them — remove them yourself if they are in the way.

## Minimal example

```yaml
embedding:
  enabled: true
  ollama_url: "http://localhost:11434"
  model: "nomic-embed-text:v1.5"
  dimensions: 768

linking:
  enabled: true
  threshold: 0.70
  demotion_threshold: 0.90
```

The complete annotated template is embedded in [`internal/config/config.example.yaml`](../internal/config/config.example.yaml). Ghost creates a copy at the user config path when needed.

## Embeddings

Embedding is enabled by default but degrades gracefully when Ollama is unavailable:

```yaml
embedding:
  enabled: true
  ollama_url: "http://localhost:11434"
  model: "nomic-embed-text:v1.5"
  dimensions: 768
```

Install the model once:

```bash
ollama pull nomic-embed-text:v1.5
```

To use FTS5 only, set:

```yaml
embedding:
  enabled: false
```

The embedding worker runs asynchronously. A newly saved memory may appear in full-text search before its vector is available.

### Model changes re-embed in the background

Every stored vector records the identity of the space that produced it — the
model, its `dimensions`, and whether a task prefix is applied (`nomic-embed-text`
is embedded with `search_document: ` for stored text and `search_query: ` for
queries, so both halves stay comparable). Change `model` or `dimensions` and the
recorded identity stops matching the configured one, so those vectors are:

- excluded from the vector leg, because a vector from one model is not
  comparable with a query embedded by another, and
- reported back to the embedding worker as unembedded, which rewrites them in
  the background — 50 memories per project per sweep, on the worker's own
  schedule, never blocking a search or a startup.

Until a memory is rewritten it is full-text searchable only — its text is never
hidden, only its vector retires — and the state is visible in three places: the
log carries one warning per retired identity (not one per search, but a second
model change warns again) naming how many vectors
were skipped and which identity wrote them, `ghost mcp status` counts only
vectors in the configured space and splits the remainder
(`embeddings: 120/547 memories (427 awaiting re-embed: 45 stale, 382 unembedded)`),
and `ghost_health` warns about the same gap, likewise separating stale rows
(those are vectors written under a retired identity) from unembedded ones (no
vector at all). Linking and `ghost supersede` skip a memory whose
vector is not in the current space, and re-check it on their next pass rather
than linking it from a cross-space similarity. Rewriting a memory's vector under
a new identity also retires the link scan it earned in the old space, so the
memory is re-queued for linking and its next scan adds edges built in the new
space alongside any the old space produced (existing `related` edges are not
deleted). Expect a one-time re-embed of the
whole corpus on the first run after upgrading to a version that records the
prefix setting, even if your model did not change.

## Linking

Linking is active when embeddings are enabled:

```yaml
linking:
  enabled: true
  threshold: 0.70
  demotion_threshold: 0.90
```

- `threshold` is the cosine similarity required for a `related` edge.
- `demotion_threshold` is the stronger similarity required before a related edge can demote a near-duplicate during injection.

The link graph is used by the Obsidian mirror and supersession ranking. It is not a graph-expansion bonus in production search.

## Search ranking

The vector leg can use a cosine floor before results enter Reciprocal Rank Fusion:

```yaml
search:
  min_similarity: 0.0
```

FTS candidates are not subject to this floor. Raise it only after testing against your corpus with `ghost bench`; a higher value can remove weak semantic matches, while `0.0` preserves the historical behavior of dropping only non-positive cosine scores.

Two more ranking signals are fixed rather than configurable, because they describe the rows being ranked instead of a preference:

- A resolved memory's fused score is multiplied by `0.5`.
- A `_global` memory's fused score is multiplied by `0.5` when a specific project is searched (never in `ghost_search_all`).

Both apply before the result window is chosen, so they affect which memories are returned as well as their order. Neither removes a memory from the candidate pool, but both can drop it from the window: a demoted row is cut as soon as enough candidates outscore its halved score, or loses its slot to the keyword reservation's score-blind eviction when a top-`limit/5` keyword hit is admitted — an exchange that can put a strictly lower-scoring row in its place; and the reservation never admits a row whose factor is below 1, so a demoted keyword hit must make that cut on its demoted score. With RRF k=60 the factors effectively rank a demoted row below every live candidate in the fetched pool: the best fused score any row can earn is 1/61 ≈ 0.0164, so a halved row sits at ≈ 0.0082 — under the 0.7/80 ≈ 0.0088 the deepest vector-leg row in a default `limit`-10 window still scores, and under the 1/80 = 0.0125 of the deepest row in a keyword-only search. They only reorder what the legs fetched: each leg pulls `limit*2` rows from the project plus `_global`, and `_global` rows count against that budget, so a project that matches fewer rows than the limit still has demoted `_global` rows fill the remainder. Session injection never sees either factor: `loadSessionContext` (`internal/mcpinit/hook.go`) builds the session-start digest and `Store.GetTopMemories` backs the MCP tool surface, both rank in SQL without reaching fusion, and both queries already exclude resolved rows. `ghost_memory_search` with `explain: true` reports them per row as `status_factor`.

## Session injection

The SessionStart hook injects a bounded context digest. The default category bias reserves slots for high-signal behavioral notes:

```yaml
injection:
  behavior_floor: 8
  behavior_categories:
    - gotcha
    - convention
    - preference
    - decision
  category_weights: {}
  category_caps:
    gotcha: 4
  session_scope: {}
```

Set `behavior_floor: 0` to disable the category bias and use rank-only selection. `category_weights` can give a category a small ordering boost. `category_caps` prevents one behavioral category from consuming every reserved slot.

### `injection.session_scope`

A memory's scope is a set of machine-readable key/value pairs naming where it applies. Every surface renders it as `scope{environment=production}`, and `ghost_memory_search` takes one as an argument. `session_scope` gives the injected block one: the scope this session is working in, so a memory scoped to another environment is not spent on this one's tokens, and the agent is not handed a fact about production while it edits development.

```yaml
injection:
  session_scope:
    environment: development
```

The rule is `memory.ScopeMatches`, the one search applies to a row against a request, and it is deliberately asymmetric:

- A memory that **does not mention** a requested key applies everywhere, so it is kept. A store's most general knowledge — conventions, gotchas, the build command — never names an environment, and a filter that hid it would leave a session holding the memories it needs least.
- A memory that **names a requested key and disagrees** is excluded. `environment=production` never reaches a `development` session, however nearly the sentence reads.
- An **absent or empty** `session_scope` filters nothing, which is the default. Selection, ranking and the 15/8 caps are then the ones that shipped; for a store whose rows carry no scope — every store written before the column existed — so is the whole block, byte for byte. A row that *does* carry scope is labelled either way, because showing the axis is the other half of this feature and the key only decides whether the block is filtered.

The linker and the dedup folds ask the same question the other way round — could these two rows be the same claim in different words — which is `memory.ScopesConflict`, and the answers agree by construction. A key with an empty or whitespace-only value would ask for the empty value, dropping every memory that names that key at all, so both forms refuse it: `environment: ""` in the file fails the load (a host hook then falls back to the compiled defaults plus the `GHOST_*` environment, as it does for any unreadable file; the defaults carry no `session_scope`, so that session's block is not scope-filtered at all until the file is fixed; a `GHOST_INJECTION_SESSION_SCOPE` replaces the file's whole `session_scope` first, so with it set the file is not refused and the env scope applies), and `GHOST_INJECTION_SESSION_SCOPE=environment=` is an error.

The filter narrows the retrieval rather than the rows that come back, because the over-fetch (45 project rows, 16 global) and the caps (15 and 8) are one budget: a production row that had spent a slot of it would have hidden a development row the session asked for.

It is a presentation default, not an access control. The block's counts stay the store's — "N shown of M total" counts every live row in the project, so a row the filter excluded is part of that difference rather than a count of its own — and the MCP tools are not narrowed by it, so an agent that wants a row from another scope can still ask `ghost_memory_search` for it. `ghost context` and every host that renders the session-start block read the same key, because they are the same loaders.

## Lifecycle and reflection

Automatic lifecycle phases are off by default:

```yaml
reflection:
  auto_reflect: false
  auto_resolve: false
  auto_supersede: false
  lifecycle_timeout_minutes: 60
  consolidation_timeout_minutes: 10
```

When enabled, the Stop hook spawns one detached lifecycle process and runs the phases in this order:

```text
reflect → resolve → supersede
```

- `consolidation_timeout_minutes` bounds one `ghost reflect` consolidation call.
- `lifecycle_timeout_minutes` bounds each lifecycle phase. Set it to `0` to remove the bound, but keep it above the consolidation timeout when using both settings.
- The lifecycle process is fire-and-forget. Failures are logged in the Ghost data directory and do not block the Stop hook.
- The unattended reflect path requires a real CLI harness; it does not silently use the offline fallback for an automatic rewrite.

### How often the chain runs

The Stop hook fires after **every** turn, so a second setting bounds how often a project's chain may start. Without it, a chatty session paid for a full `reflect → resolve → supersede` chain per turn:

```yaml
lifecycle:
  min_interval: 30m
```

`lifecycle.min_interval` is the shortest gap between two lifecycle **starts**, per project. It is a Go duration string (`"45s"`, `"30m"`, `"1h30m"`); the default is `30m`, and `0` disables the cooldown and restores the old every-turn behavior.

- The window is measured from when the last run **started**, not when it finished, so a long run cannot push the next one further away.
- The stamp is a per-project file in the data directory, `lifecycle-<project>.last`, beside the `lifecycle-<project>.pid` lock the two guards share. Its mtime is the record; the file's contents are diagnostic.
- The Stop hook only ever reads it. The detached `ghost lifecycle` process writes it, when the run it actually starts begins — so a spawn that never started, or one turned away because a run was already in progress, does not burn the window.
- Every unknown reads as "run": no stamp, an unreadable one, or a value that is not a regular file all mean the chain proceeds. A stamp dated slightly in the future (clock skew) counts as inside the window; one more than a full window ahead counts as stale and the chain proceeds. A cooldown that could silently stop maintenance is the worse failure.
- A value that is not a duration (`min_interval: soon`) is reported by name and falls back to the default, not to `0` — guessing `0` would switch off the guard you asked for.
- A skip leaves one line in `lifecycle.log` in the data directory. Nothing is printed to the host's stderr, which belongs to the editor and would otherwise get a line per turn.

## CLI harness paths and model pins

LLM-backed maintenance uses the calling session's CLI harness. Ghost resolves binaries from `PATH` unless a path is configured explicitly:

```yaml
cli:
  claude_binary: ""
  opencode_binary: ""
  codex_binary: ""
  goose_binary: ""
  model_reflect: ""
  model_resolve: ""
  model_supersede: ""
```

Set an explicit path when a binary is installed somewhere unavailable to the Stop hook process, for example:

```yaml
cli:
  opencode_binary: "/home/you/.opencode/bin/opencode"
```

The three `model_*` settings apply only to the opencode backend. Other harnesses do not receive a model flag. An empty value uses an inherited `GHOST_OPENCODE_MODEL` value, or Ghost's explicit `opencode/big-pickle` default when no environment pin is set. A configured phase pin overrides the inherited value for that phase.

Ghost does not contain a direct Anthropic HTTP client. Each selected CLI harness uses its own configured authentication and billing; Ghost does not add a second provider or API key.

## Obsidian

```yaml
obsidian:
  vault_dir: ""
  interval: "30s"
  auto_sync: false
```

- `vault_dir` defaults to `~/Documents/GhostVault` when empty.
- `interval` accepts a Go duration such as `30s`, `1m`, or `5m`.
- `auto_sync` starts a background `ghost obsidian sync` process from SessionStart. Leave it off unless you want that process and vault.

The mirror is one-way. See [the usage guide](usage.md#obsidian-vault-mirror).

## Default project routing

Sessions in a home directory or filesystem root may fall back to a configured project:

```yaml
routing:
  default_project: ""
```

Set this only when those sessions should receive a deliberate memory context. An empty value disables the fallback.

## Environment variables

Most keys use the generic mapping:

```text
GHOST_EMBEDDING_ENABLED=true
GHOST_REFLECTION_AUTO_REFLECT=true
```

The generic transformer replaces underscores with dots. Keys whose actual names contain underscores have explicit shortcuts, including:

| Variable | Key |
|---|---|
| `GHOST_OLLAMA_URL` | `embedding.ollama_url` |
| `GHOST_OBSIDIAN_VAULT_DIR` | `obsidian.vault_dir` |
| `GHOST_OBSIDIAN_AUTO_SYNC` | `obsidian.auto_sync` |
| `GHOST_CLI_CLAUDE_BINARY` | `cli.claude_binary` |
| `GHOST_CLI_OPENCODE_BINARY` | `cli.opencode_binary` |
| `GHOST_CLI_CODEX_BINARY` | `cli.codex_binary` |
| `GHOST_CLI_GOOSE_BINARY` | `cli.goose_binary` |
| `GHOST_CLI_MODEL_REFLECT` | `cli.model_reflect` |
| `GHOST_CLI_MODEL_RESOLVE` | `cli.model_resolve` |
| `GHOST_CLI_MODEL_SUPERSEDE` | `cli.model_supersede` |
| `GHOST_LINKING_DEMOTION_THRESHOLD` | `linking.demotion_threshold` |
| `GHOST_INJECTION_BEHAVIOR_FLOOR` | `injection.behavior_floor` |
| `GHOST_INJECTION_BEHAVIOR_CATEGORIES` | `injection.behavior_categories` |
| `GHOST_INJECTION_CATEGORY_WEIGHTS` | `injection.category_weights` |
| `GHOST_INJECTION_CATEGORY_CAPS` | `injection.category_caps` |
| `GHOST_INJECTION_SESSION_SCOPE` | `injection.session_scope` |
| `GHOST_SEARCH_MIN_SIMILARITY` | `search.min_similarity` |
| `GHOST_ROUTING_DEFAULT_PROJECT` | `routing.default_project` |
| `GHOST_LIFECYCLE_MIN_INTERVAL` | `lifecycle.min_interval` |

The five `injection.*` variables take structured values, so they use a comma-separated form rather than YAML syntax. Whitespace around the separators is ignored:

```bash
GHOST_INJECTION_BEHAVIOR_CATEGORIES="gotcha,decision"
GHOST_INJECTION_CATEGORY_WEIGHTS="gotcha=1.2,decision=1.5"
GHOST_INJECTION_CATEGORY_CAPS="gotcha=4,decision=2"
GHOST_INJECTION_SESSION_SCOPE="environment=development"
```

A value that cannot be read as its key's type is an error naming the variable, not a silently ignored setting.

Other useful variables:

- `GHOST_OPENCODE_MODEL` — inherited model pin for opencode-backed operations. If unset, Ghost passes `opencode/big-pickle` explicitly because the isolated child does not load the user's global OpenCode config.
- `GHOST_DEBUG` — enable debug logging.
- `GHOST_LOG_FILE` — redirect MCP logs to a file; useful when a client surfaces stderr as protocol noise.
- `GHOST_LIVE_TESTS=1` — opt into the live LLM tests; without it, `go test ./...` skips billable harness calls.
- `GHOST_TEST_SOURCE` — select the harness source used by opt-in live tests (`claude-code`, `opencode`, `codex`, or `goose`).

### Harness subprocess environment

Ghost gives each LLM harness child a case-insensitive environment allowlist. The common set contains process/home variables (including `USERPROFILE`, `APPDATA`, `LOCALAPPDATA`, `PATHEXT`, and `COMSPEC` on Windows), locale, proxy/CA settings, the owned scratch temp variables, and the documented Ghost configuration variables. It does not pass arbitrary credentials or an entire `GHOST_*` namespace. In particular, `GHOST_API_KEY`, `GHOST_DATABASE_URL`, unknown `GHOST_*_TOKEN`/`*_SECRET` names, and SSH agent/askpass variables are dropped by default.

Backend-specific configuration is selected only for the selected child:

- Claude receives `CLAUDE_CONFIG_DIR` for its configured authentication store.
- Codex receives `CODEX_HOME` for its configured state and credentials.
- Goose receives its documented safe `GOOSE_*` model/provider settings and `GOOSE_PATH_ROOT`.
- OpenCode receives `OPENCODE_API_KEY` when configured. If authentication is file-based, Ghost copies only the existing `auth.json` into the invocation-owned data directory; the child still uses an invocation-owned home/config tree and no MCP servers or plugins and, on opencode V2, an ask-every-tool policy (V2's non-interactive `run` declines every ask; a deny policy would strip tools from the request, which OpenCode's free tier rejects with 403) — V1 keeps a deny-all policy, since its handling of an ask is unverified — so Ghost does not load the user's OpenCode config or plugins. The invocation-owned tree covers `XDG_DATA_HOME` as well as `HOME`, so the child's sessions are written to a scratch store that dies with the invocation instead of entering the user's session list (#588); the sessions that predate that isolation are removed once by `ghost opencode cleanup-sessions`.

`GHOST_PASSTHROUGH_ENV=NAME1,NAME2` is an explicit escape hatch for an additional variable. Names are matched case-insensitively. Opting in can re-expose credentials to the selected harness; use it only for a value whose exposure you intend. `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, and `GOOSE_PROVIDER__API_KEY` are always removed, even when named in the hatch.

The harness commands also disable their tool surfaces explicitly: Claude runs restricted/safe mode with no built-in tools or MCP, Codex ignores user config/rules and disables shell, web, app, hook, and agent features, Goose uses `--no-profile --no-session`, and OpenCode receives a config that asks for every tool, which V2's non-interactive `run` declines (V1 receives a deny-all config). The allowlist is applied inside the shared spawn helper, including OpenCode's version probe.

## After changing configuration

Run the relevant health check:

```bash
ghost mcp status --client claude
```

For a new embedding model, stop and restart the MCP client after the model is available. See the [installation guide](installation.md#verify-the-installation) for client-specific status commands.
