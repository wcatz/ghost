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
(`embeddings: 120/551 memories (431 awaiting re-embed: 45 stale, 386 unembedded)`),
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

Both apply before the result window is chosen, so they affect which memories are returned as well as their order. Neither removes a memory from the candidate pool, but both can drop it from the window: a demoted row is cut as soon as enough candidates outscore its halved score, or loses its slot to the keyword reservation's score-blind eviction when a top-`limit/5` keyword hit is admitted — an exchange that can put a strictly lower-scoring row in its place; and the reservation never admits a row whose factor is below 1, so a demoted keyword hit must make that cut on its demoted score. With RRF k=60 the factors effectively rank a demoted row below every live candidate in the fetched pool: the best fused score any row can earn is 1/61 ≈ 0.0164, so a halved row sits at ≈ 0.0082 — under the 0.7/80 ≈ 0.0088 the deepest vector-leg row in a default `limit`-10 window still scores, and under the 1/80 = 0.0125 of the deepest row in a keyword-only search. They only reorder what the legs fetched: each leg pulls `limit*2` rows from the project plus `_global`, and `_global` rows count against that budget, so a project that matches fewer rows than the limit still has demoted `_global` rows fill the remainder. The passive surfaces never see either factor: `loadSessionPassive` (`internal/mcpinit/session_passive.go`) assembles the session-start digest and `projectContextBudget` (`internal/mcpserver/project_context.go`) backs `ghost_project_context` with the `ghost://project/{id}/context` resource and the `recall_project` prompt — both rank in SQL without reaching fusion, and both queries already exclude resolved rows. `ghost_memory_search` with `explain: true` reports them per row as `status_factor`.

## Abstention

`ghost_memory_search` reports whether its answer can be relied on. Every formatted `ghost_memory_search` answer ends with a machine line (`explain: true` returns a JSON scoring breakdown and carries no verdict; `ghost_search_all` is a different tool, answers a different question, and carries no verdict line — this setting does not reach it), `[ghost:outcome=answerable|weak|empty reason=...]`, and a human sentence for the two non-answerable cases:

- `answerable` — nothing was withheld as weak. Read the reason before relying on the rows, because two of the reasons this tool can produce mean **no floor could be applied at all**, and the rows are then unjudged: `no_floor_arm` (no arm had a value to compare — neither a keyword rank nor a cosine reached these rows, which is what a paraphrase sharing no words with the corpus produces) and `retrieval_partial` (a leg ran and broke, so no verdict was possible). Judge those rows yourself. Every other `answerable` reason means a floor did clear a row, and a machine with no embedder is not one of them: the keyword arm still judges a keyword-only result.
- `weak` — the memories are listed but **none** cleared the relevance floor. Treat them as leads and verify before relying on them.
- `empty` — nothing was returned, and the reason says why. `all_out_of_scope`, `all_out_of_category`, `all_out_of_retention` and `all_invalid` mean rows were found and then withheld (by the scope, category or retention filter, or as out of date); `nothing_cleared_the_bar` means the no-answer bar (`context.no_answer_cosine`, below) judged that no returned row was a close enough match and withheld the block, with the best score in the sentence; `all_over_budget` means the answer was too large to return; `vector_backend_unavailable` means the vector leg never ran, so the keyword leg was all that searched, and a leg that ran and *broke* is a tool error rather than a verdict. `no_candidates` is the only reason that may claim the store has nothing matching, and it may only do so over complete coverage; where the search was windowed the answer says "no match within the searched window" instead, because that is not the same claim.

The vector arm of that floor is configurable and ships off:

```yaml
context:
  abstain_cosine: 0.0   # 0 = arm disabled
  relevance_cutoff: 0.63  # fraction of the top row's fused Base; 0 = off
  no_answer_cosine: 0.62  # absolute bar on the best vector cosine; 0 = off
```

A returned memory clears the vector arm when its cosine is **at least** this value. The value must be between 0 and 1: a negative one is satisfied by every row including the worst match, an infinite one by none, and a NaN reads as no threshold at all, so all three are refused as the load error they are rather than accepted as settings. The assembler refuses them again on the request itself, so a caller that builds a `Request` without going through this file inherits the guard instead of a silently wrong floor. The default is 0 — meaning *disabled*, not "a threshold of zero", which every row clears. The verdict line renders three states, because a threshold and a threshold that ran are different facts: `abstain_cosine=off` (nobody configured one), `abstain_cosine=not_applied` (one is configured and no cosine could be compared — the vector leg never ran, the ordinary state on a machine with no working embedder, or it ran and failed; the verdict line's `reason=` and `legs=` say which, so the threshold you set is never silently doing nothing without also saying why), and the number itself, printed when the vector leg executed — which is not the same as a row being compared, because an empty result reads no cosine at all and a result whose first row cleared the keyword arm never reads one.

The default is off deliberately, and the measurement behind it is about the distributions rather than about any one number. `ghost bench` reports that the answerable and no-answer cosine distributions overlap (mean top cosine 0.740 against 0.584, and the no-answer maximum of 0.697 would separate the two only by also refusing 52 of the 220 answerable queries), so no constant separates them. That 0.697 is a property of *this* corpus measured with the strict-above convention `search.min_similarity` uses, not a threshold for this key: this arm clears ties, so a value of 0.697 here would admit every query the bench counted as refused at that level. Set it only after measuring against your own corpus, and read the number off your own distribution rather than off that one.

The keyword arm needs no key: a result within the top four keyword ranks clears it. This is also why an unavailable embedder is never a reason to call a match weak — a result whose rows carry keyword ranks is judged by that arm whether or not a vector leg ran, and the line reports the vector arm separately (`abstain_cosine=not_applied`, `legs=vector:not_run`). What is reported as `no_floor_arm` is the opposite case: no arm held a value to compare, because no row carried a keyword rank and no cosine reached them. That is `answerable` rather than below a floor nobody applied.

`context.abstain_cosine` is unrelated to `search.min_similarity` above: that floor runs inside the vector leg *before* fusion and so never sees a keyword-only result, which is the case this one exists to judge.

`context.relevance_cutoff` is the query-mode relevance cutoff ([#954](https://github.com/wcatz/ghost/issues/954)): a block stops where relevance falls off, once a row's fused Base is below this **fraction of the top row's fused Base**. It is ONE rule with ONE parameter, applied in the assembler after dedup and before the budget, and it can only **shorten** an answer — `limit` stays the maximum, the top row is always kept (so the result rate never drops below 1.000 on an answerable query) a pinned row is never cut and a keyword-reserved row is never cut. The comparison is on the fused Base, the score before the age decay, so a relevant month-old decision is not cut for being old. It is a **query-mode** rule: the passive surfaces (session start, project context) are a digest and keep their slices whatever this is set to. The default **0.63** is chosen from the `ghost bench --cutoff-sweep` gradient in [`benchmarks.md`](benchmarks.md#the-relevance-cutoff-ghost-bench---cutoff-sweep) — it admits 302 of the 304 baseline graded-relevant rows while raising context precision 0.138 → 0.145 and lowering estimated tokens per answer 296.6 → 280.3. The value is a fraction in [0, 1]; **0 is off** (the pre-cutoff block, byte-identical to a machine that never had the cutoff), and a value of 1 keeps only rows that tie the top. Setting `relevance_cutoff: 0` disables it, which is the state a machine with no config runs. Unlike `abstain_cosine` it does not need to be measured against your own distribution before use — the sweep below ships a default — but you can raise or lower it from the sweep's own reasoning.

`context.no_answer_cosine` is the query-mode no-answer bar ([#955](https://github.com/wcatz/ghost/issues/955)): a question nothing in the store answers still fills its window, and a model handed ten plausible rows will often use them, so when the **best vector cosine among the rows a block would carry is strictly below this bar** the block is withheld and the answer says so. It is ONE rule with ONE parameter, applied in the assembler right after the relevance cutoff and before the budget. A withheld block is `outcome=empty reason=nothing_cleared_the_bar` with the sentence "No memory answers this: nothing cleared the bar", the best score and the bar, so the caller can see it was a judgement and not an absence claim about the store; each withheld row is recorded with the same reason in the trace, the retrieval record and the `explain: true` payload, like any other cut. Rows are shown again by lowering the bar or setting it to 0. It is a **query-mode** rule (the passive surfaces never see it), a **pinned** row is never withheld (and is not judged), a keyword-reserved row is not exempt, and a block that cannot be judged is never refused: with no vector leg in play (no embedder, or the leg failed) or no row carrying a cosine the block passes through, because refusing every answer on a machine with no embedder would be the rule misreading its own blind spot. It only removes rows, so `limit` stays the maximum. The default **0.62** is chosen from the `ghost bench --no-answer-sweep` table in [`benchmarks.md`](benchmarks.md#the-no-answer-bar-ghost-bench---no-answer-sweep): at that bar 19 of the 24 no-answer queries are refused (false-positive rate 0.208) at a cost of 2 of the 220 answerable ones, inside the gate of at most half and at most 7. The value is a cosine in [0, 1]; **0 is off** (the pre-#955 block, byte-identical). It is a property of the embedding model the bench uses, so read your own distribution before relying on it with a different model; it is unrelated to `search.min_similarity` (applied inside the vector leg before fusion) and to `abstain_cosine` (which only labels a block weak and still shows its rows).

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

It is a presentation default, not an access control. The block's counts come from the same retrieval that produced its rows rather than from a second census of the store: "N shown of M total" divides by every eligible row the project holds (one count over the same predicates the retrieval's window uses, so the over-fetch's size never stands in for the store), and the sentence after it says why the difference is there. "not shown, ranked by a composite score" is the caps' cut; "withheld rather than ranked out" is a stage refusing a row the window held (a closed validity window, a resolved row); when every row was withheld the block prints the abstention sentence in place of any count. A row this filter excluded never enters the window — it is filtered at the fetch, so it is no part of either number — and the MCP tools are not narrowed by it, so an agent that wants a row from another scope can still ask `ghost_memory_search` for it. `ghost context` and every host that renders the session-start block read the same key, because they are the same loaders.

## Lifecycle and reflection

Automatic lifecycle phases are off by default:

```yaml
reflection:
  auto_reflect: false
  auto_resolve: false
  auto_supersede: false
  supersede_consensus: 3
  lifecycle_timeout_minutes: 60
  consolidation_timeout_minutes: 10
```

**Leave `auto_supersede: false` for now, and run the phase by hand.** The
supersede pass was measured at 43% precision on a real store
([#686](https://github.com/wcatz/ghost/issues/686)): most of the edges it
proposed joined two notes that were both still true, and an edge is not an
annotation — it demotes the older note in ranking and `ghost resolve` then stamps
`resolved_at` on it, which takes a live memory out of every later session. The
pass is KEEP-biased (the rubric asks whether the older claim is still false, a
`supersedes` answer must name the claim in a `replaced:` field, and a standing
rule the newer note never retires is vetoed before any harness call) and its
labeled eval read 1.00 precision / 1.00 recall against the free
`opencode/big-pickle` — a figure from the 28-pair set, measured before the four
fixtures below existed, so it does not cover them. The eval is synthetic and the
corpus it came from is not.

A second measurement over three more real stores
([#779](https://github.com/wcatz/ghost/issues/779)) read **55% precision** over
108 distinct proposals, with the wrong edges in four classes the rubric now
names outright: a newer note retiring one claim of a many-claim older note, a
release or status log treated as a chain of replacements, a recurring defect
treated as a fix chain, and parallel investigation notes treated as a linear
one. A `supersedes` now has to retire **every** claim of the older note, and a
log entry, a recurring defect and a parallel investigation are not chains. The
same measurement found the wrong edges are an *unstable* classifier as much as a
wrong one — 79% precision on edges proposed in all three passes, 33% on those
proposed in one — which is what `supersede_consensus` is for.

### `supersede_consensus`

**Read only when `auto_supersede: true`.** The automatic supersede phase
classifies its candidate set this many times and writes **only what every pass
proposed, in the same direction**; a pair the passes split on is reported as "not
agreed" with its count and ids, and nothing is written for it. Unanimity rather
than a majority, because a 2-of-3 majority writes exactly the 0.56 row of that
measurement. Default `3`, the number the measurement used. Raising the count
makes the gate **harder**, not easier — a pair that split 2-1 at 3 has to satisfy
one more pass at 4 — so a higher value suppresses more and costs more. It is
worth setting for a corpus you want to be conservative about, not as a way to
settle a specific pair.

The cost is real and worth naming: a gated phase asks the classifier **N times**
what an ungated one asks once, and the harness bills every call. Two things do
*not* scale with N, and both are why the multiplier is affordable: a pair
`skip-if-unchanged` or the NEITHER cache would have skipped is skipped **once**,
not N times, and an unchanged live edge is not re-asked at all — so on a
converged project the gate costs almost nothing, and the cost is concentrated on
the passes that were about to be asked anyway.

**The phase's wall time scales with N and its deadline does not.**
`lifecycle_timeout_minutes` still bounds the whole supersede phase, unchanged, and
it is a hang detector rather than a work budget, so it is deliberately not
multiplied: raising it to fit N would make it N times longer to notice a model
that never answers, which is how a hang detector stops being one. Turning the gate
on at the default 3 therefore multiplies the work against the same 60 minutes —
on a large project, raise `lifecycle_timeout_minutes` or set the count to 2.

Know what an expired deadline costs under `--apply`. The bound is enforced by
`SIGTERM` to the phase's process group, and that signal can land **inside the
apply block**, whose per-pair link writes are separate transactions — so an
expired deadline leaves a **partial write**, **no report at all** (the CLI prints
nothing on the error path) and a `lifecycle-last-failure` marker the next
session-start turns into an alert. Nothing is corrupted: the writes that landed
are correct and a re-run converges from the graph's own state. But there is no
partial report to read either, which is the reason to raise the bound rather than
rely on the retry.

A value below `2` runs the **ungated** pass, and the phase prints no
`--consensus` at all: a value under the minimum is not a smaller gate but no
gate, and the flag is omitted rather than emitted as a number the parser refuses.
Nothing on the phase's own output says so — an ungated pass prints no gate line —
so `ghost lifecycle` announces it on stderr, beside its reflect-skip notice, and
that is where the unattended path collects it (the phase tail, and
`lifecycle.log` beneath it). Understand what an ungated phase is choosing: the
measurement that motivates the gate says those edges are 0.55 correct, and a
wrong one buries a live memory. A gated run's own report says which number ran,
so the two are told apart by that notice rather than by the phase's stdout.

The flag `ghost supersede <project> --apply --consensus N` is the same gate by
hand, and it is **off unless typed** — a hand-run pass is never silently tripled.
It also gates `--reassess`, the only command left that deletes a live
`supersedes` edge, so `--reassess --consensus 3 --apply` withdraws an edge the
**classifier decided** only when all three passes agree. It does not gate the
deterministic veto, which is settled from the two note bodies before the gate with
no classify call at all, so a vetoed edge is withdrawn on the first pass exactly
as before. It is still refused with `--withdraw`, which asks no model: N passes
would buy no agreement about a decision you already made by naming the edge.

Turning the phase on still means writing real edges from a model that has not
been measured on your notes; running `ghost supersede <project>` by hand and
reading the list is free of that, and it is dry-run by default.


**Upgrading from before #779: run `--reassess --consensus 3` once.** The tightened rubric
applies to pairs judged from here on — the NEITHER cache's key prefix moved with
the rubric, so every cached verdict is re-asked. Edges **already in the graph**
are a different thing: `skip-if-unchanged` holds a live `supersedes` edge quiet
until one of its endpoints changes, so a passing pass never re-judges one written
under the old rules. `ghost supersede <project> --reassess` re-judges every live
edge under the current rules and, with `--apply`, withdraws the ones they no
longer support — every edge the classifier decided, and only where all three
passes agreed on it, when you pass `--consensus 3`:

```bash
ghost supersede <project> --reassess --consensus 3            # dry run
ghost supersede <project> --reassess --consensus 3 --apply    # withdraw, and print the resolve repair
```

It is worth doing once after the upgrade and then not routinely. The repair pass
is where the rubric's error argument does not apply — a false veto there deletes
a correct edge, and being deterministic it re-fires until a note changes — so
read the dry run before applying, and prefer the gated form: an ungated
`--reassess --apply` withdraws on one classifier verdict per edge, which is what
[#845](https://github.com/wcatz/ghost/issues/845) measured at 6 wrong withdrawals
in 11.

When enabled, the Stop hook spawns one detached lifecycle process and runs the phases in this order:

```text
reflect → resolve → supersede
```

- `consolidation_timeout_minutes` bounds one `ghost reflect` consolidation call, harness calls included: a run that needs the LLM tier's one repair turn (a rejected answer is re-read once with the reader's complaint attached) spends what is left of this budget rather than getting a deadline of its own.
- `lifecycle_timeout_minutes` bounds each lifecycle phase. Set it to `0` to remove the bound, but keep it above the consolidation timeout when using both settings.
- The lifecycle process is fire-and-forget. Failures are logged in the Ghost data directory and do not block the Stop hook.
- The unattended reflect path requires a real CLI harness; it does not silently use the offline fallback for an automatic rewrite.
- The unattended reflect path also runs through the consolidation quality gate, which is what bounds how hard it may compress with nobody watching. The gate applies from 6 consolidatable memories up, and `--require-llm` (which the lifecycle phase passes, and which omits the mechanical SQLite fallback) is what makes failing it a failed run rather than a fall-through: a corpus of up to 60 must retain 30% of itself, a corpus of 200 or more must come back with at least 5 memories, and the minimum in between interpolates. Those are the shipped values — the backlog figure is the absolute floor `gateBacklogMinOutput` in `internal/reflection/consolidator.go`, not a fraction, because the prompt asks for a corpus of high-quality memories rather than a fixed count and a percentage would demand more output than it ever asks for on a large backlog. `--tier cli` and `--tier opencode` are held to the same floor and, having no fallback tier, report a too-small answer as a failure. A round of 6 consolidatable memories or more that keeps less than half of them prints a `>50% reduction` warning, counted as the memories the project ends up holding. A pin on a memory (`ghost_memory_save` with `pin: true`, or `ghost_memory_pin`) is the way to keep one specific memory out of a rewrite entirely.

### Repairing what the resolve and supersede passes got wrong

Both passes are re-runnable and both have a repair flag, because neither decision
can be undone by re-running the pass that made it. Both are dry-run by default;
`--apply` writes.

```text
ghost resolve <project> --reassess [--apply]
ghost supersede <project> --reassess [--apply]
```

- `ghost resolve --reassess` re-judges the memories already stamped `resolved_at` and, with `--apply`, clears the stamp on the ones that now come back KEEP. It does not re-run the keyword prefilter, so every already-resolved memory in the project is re-judged and the report has no silent gap.
- `ghost supersede --reassess --consensus 3` re-judges every `supersedes` link already in the graph — the edges, not the candidate pairs, so `--threshold` does not apply — and with `--apply` **withdraws** the ones that no longer hold: a pair that comes back `neither`, a `causes` or `reversed` verdict, and — **only when all three passes agreed** — every one of those; a pair whose older note states a rule the newer note never retires is **not** gated, because the veto settles it with no classify call at all. Each withdrawn edge is printed with the rule that withdrew it and with `[veto, no harness call]` or `[classifier]`, so the rows no model looked at are visible as such; a withdrawal that also drops the pair's `causes` edge says so on the row (`[+1 causes edge]`) and on the summary line — predicted in a dry run, and read back from the store under `--apply`, so a concurrent pass that took that edge first reports `0` rather than claiming a deletion. A sweep that *fails* is reported as unknown, not as `0`: the count is not knowable after a failed write. The withdrawal records an `unsupersede` row in the memory history, and a write that fails after some have landed still reports those, because each is its own transaction and a later pass will not see them again. A scope-conflicting edge is left alone: scope says which pairs may be related at all, not that one of them was a supersession. Note the one asymmetry with the ordinary pass: withdrawing a **vetoed** edge deletes a correct edge if the veto is wrong, and the ordinary pass will not re-create it, because the veto is deterministic on the same two notes — so read that row before applying.
- **Order matters if a memory was buried by a wrong edge.** `ghost resolve` stamps `resolved_at` on the older endpoint of a live `supersedes` edge for free, and `ghost resolve --reassess` deliberately treats a live edge as a floor — so run `ghost supersede --reassess --consensus 3 --apply` **first**, then `ghost resolve --reassess --apply`. Withdrawing the edge is what makes the resolution it justified clearable.
- Neither flag is ever emitted by the Stop hook's lifecycle chain. They are operator commands, and both spend harness calls on every pair they judge.
- `ghost resolve --reassess` also honours a correction pairing as a floor, and holds back any row whose correction the same run is repairing — so a repair that the next ordinary pass would undo is reported as still asserted rather than done.

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
| `GHOST_REFLECTION_AUTO_REFLECT` | `reflection.auto_reflect` |
| `GHOST_REFLECTION_AUTO_RESOLVE` | `reflection.auto_resolve` |
| `GHOST_REFLECTION_AUTO_SUPERSEDE` | `reflection.auto_supersede` |
| `GHOST_REFLECTION_SUPERSEDE_CONSENSUS` | `reflection.supersede_consensus` |
| `GHOST_REFLECTION_LIFECYCLE_TIMEOUT_MINUTES` | `reflection.lifecycle_timeout_minutes` |
| `GHOST_REFLECTION_CONSOLIDATION_TIMEOUT_MINUTES` | `reflection.consolidation_timeout_minutes` |
| `GHOST_LINKING_DEMOTION_THRESHOLD` | `linking.demotion_threshold` |
| `GHOST_INJECTION_BEHAVIOR_FLOOR` | `injection.behavior_floor` |
| `GHOST_INJECTION_BEHAVIOR_CATEGORIES` | `injection.behavior_categories` |
| `GHOST_INJECTION_CATEGORY_WEIGHTS` | `injection.category_weights` |
| `GHOST_INJECTION_CATEGORY_CAPS` | `injection.category_caps` |
| `GHOST_INJECTION_SESSION_SCOPE` | `injection.session_scope` |
| `GHOST_SEARCH_MIN_SIMILARITY` | `search.min_similarity` |
| `GHOST_CONTEXT_ABSTAIN_COSINE` | `context.abstain_cosine` |
| `GHOST_CONTEXT_RELEVANCE_CUTOFF` | `context.relevance_cutoff` |
| `GHOST_CONTEXT_NO_ANSWER_COSINE` | `context.no_answer_cosine` |
| `GHOST_SCRATCH_MAX_BYTES` | `scratch.max_bytes` |
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
- `GHOST_DEV_FORBID_DATA_DIR` — a list of data directories (separated by the platform's list separator) that a build that is **not a release** refuses to open, so a development build cannot migrate a real store. A release build ignores it entirely, which is what makes it safe to export in a development shell. Unlike every other variable here it is not a config key and never reaches a config file; the full rules, including the canonicalization and the hook paths' fail-open behaviour, are in [`cli.md`](cli.md#development-builds-refusing-a-real-store).
- `GHOST_TEST_SOURCE` — select the harness source used by opt-in live tests (`claude-code`, `opencode`, `codex`, or `goose`).

### Harness subprocess environment

Ghost gives each LLM harness child a case-insensitive environment allowlist. The common set contains process/home variables (including `USERPROFILE`, `APPDATA`, `LOCALAPPDATA`, `PATHEXT`, and `COMSPEC` on Windows), locale, proxy/CA settings, the owned scratch temp variables, and the documented Ghost configuration variables. It does not pass arbitrary credentials or an entire `GHOST_*` namespace. In particular, `GHOST_API_KEY`, `GHOST_DATABASE_URL`, unknown `GHOST_*_TOKEN`/`*_SECRET` names, and SSH agent/askpass variables are dropped by default.

Backend-specific configuration is selected only for the selected child:

- Claude receives `CLAUDE_CONFIG_DIR` for its configured authentication store.
- Codex receives `CODEX_HOME` for its configured state and credentials.
- Goose receives its documented safe `GOOSE_*` model/provider settings and `GOOSE_PATH_ROOT`. `GOOSE_MODE` is not among them: Ghost sets it to `chat` on the child rather than passing the parent's value through, because it is the only control goose offers over tool execution (see the harness policy below). Goose also runs with an invocation-owned `HOME`, because goose discovers user-scope Agent Plugins under `~/.agents/plugins/` — a path that is home-relative by specification, so redirecting `XDG_CONFIG_HOME` alone does not move it. `ghost mcp init --client goose` installs a package there that registers the Ghost MCP server and runs `ghost hook` through a shell, so a child that kept the real home could start Ghost's own server from inside a reflect, resolve, or supersede call. `--no-profile` does not cover this — it governs the configured profile, not plugin discovery, and goose documents no variable that disables discovery.

  The child still reaches the configuration it authenticates from. Ghost does not tell goose where its config lives, because goose resolves that differently per platform and per build. Instead the invocation-owned `HOME` is an empty directory that carries the configuration with it. When `XDG_CONFIG_HOME` is set, the child reads that inherited absolute path directly and nothing has to be reproduced. Otherwise Ghost reproduces each home-relative root goose may use — `~/.config/goose` and `~/Library/Application Support/goose` — inside that home, so the child resolves whichever one its own goose would have resolved before and `config.yaml`, `secrets.yaml`, and `settings.json` are unaffected. Only the plugin root moves. On Windows the config comes from `%APPDATA%`, an absolute path the allowlist already passes through, so nothing is reproduced and the home move cannot affect it.

  Where the platform refuses a symlink — Windows without Developer Mode — Ghost copies the configuration files into the isolated home instead, so a normal unelevated `ghost reflect` is not made to depend on a privileged filesystem operation. When no invocation-owned home can be created, or when the parent named no home at all, the call fails rather than running with the real home: a degraded reflection is better than one that starts a second Ghost server.

  `GOOSE_PATH_ROOT` overrides the home entirely. Goose checks it before any home variable — plugins resolve to `$GOOSE_PATH_ROOT/.agents/plugins`, and the home is never consulted — so isolating the home achieves nothing while it is set. Ghost therefore repoints it at a root inside the invocation-owned tree and carries the real `config/` directory (with `secrets.yaml`) into it, so the child still authenticates. A relative value is left alone, because goose ignores one anyway and the home variables it falls back to are already confined. A candidate configuration root that cannot be examined fails the call rather than being skipped, even if another root was already carried: any of them can hold a plugin directory on some platform, so continuing on partial information is how a boundary quietly stops being one.
- OpenCode receives `OPENCODE_API_KEY` when configured. If authentication is file-based, Ghost copies only the existing `auth.json` into the invocation-owned data directory; the child still uses an invocation-owned home/config tree and no MCP servers or plugins and, on opencode V2, an ask-every-tool policy (V2's non-interactive `run` declines every ask; a deny policy would strip tools from the request, which OpenCode's free tier rejects with 403) — V1 keeps a deny-all policy, since its handling of an ask is unverified — so Ghost does not load the user's OpenCode config or plugins. The invocation-owned tree covers `XDG_DATA_HOME` as well as `HOME`, so the child's sessions are written to a scratch store that dies with the invocation instead of entering the user's session list (#588); the sessions that predate that isolation are removed once by `ghost opencode cleanup-sessions`.

`GHOST_PASSTHROUGH_ENV=NAME1,NAME2` is an explicit escape hatch for an additional variable. Names are matched case-insensitively. Opting in can re-expose credentials to the selected harness; use it only for a value whose exposure you intend. `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, and `GOOSE_PROVIDER__API_KEY` are always removed, even when named in the hatch.

The harness commands also disable their tool surfaces explicitly, and the mechanism differs per harness because the tools are not configured the same way. The child receives untrusted memory text, so this is a boundary and not a preference:

- **Claude** runs restricted/safe mode with no built-in tools or MCP: `--safe-mode --restricted --strict-mcp-config --tools "" --disallowedTools mcp__*`. `--tools ""` is claude's own "disable all tools" spelling. Ghost probes the installed binary's `--help` first and **refuses to run** if any of those flags is missing, rather than falling back to a weaker policy.
- **Codex** has no "no tools" flag — `--sandbox read-only` bounds what a tool may do, not which tools exist — so the restriction is a list of `-c features.<key>=false` overrides: `shell_tool` and `unified_exec` (codex registers either the unified exec tool or the one-shot exec, so both are named), `view_image` (a local file read, which the sandbox's write policy does not cover), the plugin and connector group `apps`/`plugins`/`tool_suggest`/`skill_mcp_dependency_install` (installing an MCP server on demand runs a command), plus `remote_plugin`, `hooks`, `multi_agent`, `agents.enabled`, and the top-level `web_search="disabled"`. It also ignores user config and rules, and runs with a read-only sandbox. One residual: codex provides no way to disable `apply_patch`, so the read-only sandbox is what bounds it.
- **Goose has no flag for this at all.** Its extension options only *add* extensions, and `--no-profile --no-session` govern the profile and the session, not which extensions load. The restriction is therefore `GOOSE_MODE=chat` on the child environment — the mode goose documents as "chat only, no tool calls", against a default of `auto` (which approves them). Ghost **sets** it rather than inheriting it, and `GOOSE_MODE` is deliberately absent from the goose allowlist, because an inherited value could only weaken it while dropping it returns the child to goose's autonomous default. Two things about this are verified against the real binary only, under `GHOST_LIVE_TESTS=1`: that a given goose accepts `chat` as a mode name and ranks it above `config.yaml`, and that a headless `goose run` still completes a turn under it. Whether `chat` suppresses *extensions* in a given build is not verified — `goose info -v` does not report it, so that one is documented as unverified rather than claimed.
- **OpenCode** receives a config that asks for every tool, which V2's non-interactive `run` declines (V1 receives a deny-all config). `opencode run` has no tool-disabling flag — its only permission flag, `--auto`, moves the other way — and Ghost never passes it. The wildcard permission is what makes the policy total, because opencode defaults every unset permission to `allow`.

The allowlist is applied inside the shared spawn helper, including OpenCode's version probe.

## After changing configuration

Run the relevant health check:

```bash
ghost mcp status --client claude
```

For a new embedding model, stop and restart the MCP client after the model is available. See the [installation guide](installation.md#verify-the-installation) for client-specific status commands.
