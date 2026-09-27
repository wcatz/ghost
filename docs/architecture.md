# Ghost architecture

This document is for contributors and maintainers. For installation and everyday use, start with [`installation.md`](installation.md), [`usage.md`](usage.md), and [`cli.md`](cli.md).

## Design goals

Ghost is intentionally:

- **Multi-process and local-first:** one SQLite database that any number of Ghost processes may open concurrently; SQLite is the synchronization layer. See [Concurrency contract](#concurrency-contract).
- **Pull-based during normal MCP use:** the server exposes tools and resources; it does not inject an LLM call into ordinary memory reads.
- **CGO-free:** `modernc.org/sqlite` supplies SQLite and FTS5 without a C toolchain.
- **Host-aware:** lifecycle events are normalized into a small contract shared by Claude Code, opencode, Codex, and Goose.
- **Source-aware for maintenance:** reflect, resolve, and supersede route through the calling session's CLI harness rather than silently selecting another harness or billing path.

## Runtime modes

The same binary provides several modes:

```text
ghost mcp                         MCP server over stdio
ghost mcp init                    Configure MCP clients
ghost mcp status                  Check client and store health, and list
                                   projects with no bound checkout
ghost hook <event> --source <host> Normalize a host lifecycle event
ghost reflect <project>           Consolidate memories
ghost resolve <project>           Mark resolved evidence
ghost supersede <project>         Classify replacement relationships
ghost lifecycle <project>         Run the detached maintenance phases
ghost project delete|merge        Manage project records
ghost project bind <id> <path>     Record a project checkout so a session in
                                   that directory resolves it
ghost obsidian export|sync        Mirror the store to Markdown
ghost opencode cleanup-sessions   One-shot cleanup of lifecycle sessions
                                   titled exactly "[ghost]"
ghost backup                      Snapshot the live database (VACUUM INTO)
ghost export                      Write the store as a portable JSONL artifact
ghost import <file> [--apply]     Load a JSONL artifact (dry-run by default)
ghost context                     Render passive session context
ghost history <memory-id>         Print one memory's append-only history
ghost history purge <memory-id>   Erase a memory and every recorded version of it
ghost bench [--sweep]             Run the built-in benchmark
ghost upgrade                     Update a standalone binary
ghost version                     Print the version
```

The MCP server is the primary mode. It starts embedding and linking workers when enabled, but those workers do not make LLM calls.

## Package map

```text
cmd/ghost/                         CLI entrypoint and command dispatch

internal/ai/                        Source-aware CLI harness adapters
  provider.go                      Provider and TokenUsage contracts
  source_detect.go                 Environment/process source detection
  source_provider.go               Routes to the caller's harness
  cli_client.go                    Claude-compatible CLI adapter
  opencode_client.go               OpenCode V1/V2 adapter
  opencode_cleanup.go              One-shot "[ghost]" session backlog cleanup
  codex_client.go                  Codex adapter
  goose_client.go                  Goose adapter
  scratch.go                       Harness scratch/temp handling

internal/adversarial/               Test-only: shared hostile-input corpus and
                                     the inert-content invariant (#585)
internal/bench/                     Built-in retrieval benchmark and sweeps
internal/claudeimport/              One-time Claude Code memory import
internal/config/                    Layered YAML/environment configuration
internal/embedding/                 Optional Ollama embedding client/worker
internal/hostevent/                 Normalized host-event contract and scanners
internal/linking/                   Background related-memory linking worker
internal/mcpinit/                   Client installers, status checks, hooks
internal/mcpserver/                 MCP server, tools, resources, prompts
internal/memory/                    SQLite store, FTS5, vectors, links, schema
internal/obsidian/                  One-way Markdown vault exporter/sync
internal/portable/                  Portable JSONL artifact: read (export) and
                                    write (import) of the portable record format
internal/procstat/                  Cross-platform process liveness/start time
internal/provider/                  MemoryStore and LLMProvider interfaces
internal/reflection/                Tiered memory consolidation
internal/resolve/                   Resolved-evidence classifier and cache
internal/scratch/                   Ghost-owned scratch root cleanup
internal/secret/                    Credential-shape detector for stored text
internal/selfupdate/                Checksum-verified GitHub release updater
internal/supersede/                 Directed supersession relation classifier
```

The database schema is an embedded Go string constant in `internal/memory/schema.go`; it is the single source of truth for the store schema. `OpenDB` reads `PRAGMA user_version` and refuses a store from a newer Ghost **before** it runs any DDL against it, not after: `initSQL` is `CREATE … IF NOT EXISTS`, so on a store that merely has more than this build knows it is nearly a no-op — which is exactly what hid the ordering — but any object the newer build renamed, replaced or dropped is recreated here, in a store the same call then declares unreadable. A refusal that has already written is not a refusal. A fresh store has no stamp to read and is initialized and stamped instead; a store behind is backed up, then migrated.

## Host integration

`internal/mcpinit` owns the host-specific setup paths:

- Claude Code: MCP registration, permissions, SessionStart/Stop hooks, file-memory migration, and redirects.
- opencode: one lifecycle TypeScript adapter that registers MCP and bridges idle events.
- Codex: `config.toml` registration plus `hooks.json` entries that require user trust.
- Goose: an Agent Plugins package with MCP and Open Plugins hooks.

All hook paths converge on `internal/hostevent`, which parses the versioned event envelope and dispatches normalized events. The `scratch` and `procstat` packages keep harness scratch and detached-process liveness handling separate from host adapters.

Every user-owned config write — the Claude `settings.json`, the codex `config.toml` and `hooks.json`, the opencode plugin, the goose package and the MEMORY.md redirect — goes through one atomic path (temp file in the same directory, then rename, under a random hidden name), so an interrupted write cannot leave a half-written host config, and a host reading a config while init repairs it reads a whole document rather than a truncated one. The rename alone only orders that for a *reader*; a *power cut* is answered by the two flushes around it: the temp file is fsynced (its data and its mode together) before the rename, and the parent directory is fsynced after it, because a rename that reaches the disk before the bytes it points at leaves a config that reads as empty or truncated, and a directory entry that never reaches the disk leaves the file gone even though its contents were written. A temp-file flush that fails stops the write and is reported — nothing has been published yet, so the user's config is still the one that was there. A directory flush that fails is not reported: the rename already published the document, there is nothing left to refuse, and the alternative is failing an init that did its job on a filesystem that cannot flush a directory at all (Windows has no equivalent — `FlushFileBuffers` on a directory handle fails and NTFS orders that metadata itself — so the directory flush is a no-op there). A symlinked config is written through to its target rather than replaced by it; a link whose target does not exist yet is the one case that cannot be, and is replaced by a regular file. A rename swaps in a new inode, so the permission bits are copied across but the owner, POSIX ACLs, xattrs and hardlinks are not: a config owned by another account changes owner, its ACL entries are dropped for the directory's defaults, and a config hardlinked into a dotfiles repo instead of symlinked silently diverges from its repo copy. An existing file keeps its own permissions, so a repair never widens or narrows a mode the user chose, except on the Claude side: `settings.json` is clamped so it can never be group- or world-readable whatever mode the user's copy had. That clamp is backwards compatibility, not a credential policy — it is the ceiling every save enforced before the refactor, and a copy narrower than `0600` is left at its own narrower mode. The other files keep a wider mode on purpose, including a codex `config.toml` whose `[mcp_servers.ghost.env]` sub-table can hold a credential. The Claude `settings.json.bak` backup is captured once, by the first save that finds a file to back up, so repeated `init` runs never overwrite the pristine original with ghost's own output. No other user-owned config is backed up: a `hooks.json` repair that merged wrongly can only be repaired by hand, and a once-only `.bak` for the merged hosts is still open.

If a host reports an unknown source, `internal/ai` does not cascade to a default harness. The caller must provide a source or the operation fails with an actionable error. This prevents an opencode or Claude session from silently using the wrong harness or billing path. OpenCode children receive an explicit `opencode/big-pickle` model unless a per-phase pin or `GHOST_OPENCODE_MODEL` override is supplied; the child home/config tree is invocation-owned, only a configured `auth.json` is carried into its data root, and its tool/MCP policy is deny-all, so the user's global OpenCode model, plugins, and MCP servers are not inherited.

**The prompt travels on stdin, never as an argv element.** Every adapter spawns its harness with the prompt on the child's standard input and no positional message, because the kernel caps a single argument at 32 pages — 128 KiB on a 4 KiB-page x86, 512 KiB on a 16 KiB-page one — and a reflect prompt is assembled from up to 2000 memories of 8000 bytes, so a large project produced a prompt that failed the spawn outright with `E2BIG` rather than an answer. Each harness reads it the same way: `claude -p` and `opencode run` take the message from a pipe when no positional message is given, and codex and goose are given their explicit stdin sentinels (`codex exec -`, `goose run -i -`). A flag argument stays an argument where it is small, fixed, and must not be confusable with user content: `claude --system-prompt` carries the classify rubric while the untrusted user content is piped.

## Data flow

### Normal MCP session

```text
MCP client
  → stdio JSON-RPC
  → internal/mcpserver
  → provider.MemoryStore
  → SQLite / FTS5
```

The MCP server registers 20 tools, 4 resources, and 2 prompts. Core memory CRUD and search tools do not invoke an LLM; the maintenance-oriented `ghost_resolve` and lifecycle paths can invoke the selected CLI harness. Resources expose project context, decisions, tasks, and global memories for clients that support resource pinning.

### Session start

```text
host SessionStart
  → ghost hook session-start --source <host>
  → resolve project by longest path prefix/name
  → rank and bound the context digest
  → write the digest to the host
```

opencode uses `ghost context` because it cannot consume the hook's stdout injection directly. The OpenCode adapter injects that rendered block as instructions instead.

### Stop and maintenance

```text
host Stop
  → ghost hook stop --source <host>
  → save reminder / bounded host behavior
  → cooldown check (lifecycle.min_interval vs lifecycle-<project>.last mtime)
  → optional detached ghost lifecycle <project>
       → writes lifecycle-<project>.last
       → reflect
       → resolve
       → supersede
```

The lifecycle is opt-in. A phase failure is logged and does not prevent later phases from running. The reflect phase can use a source-matched CLI harness or an explicitly selected offline tier; the autonomous path requires a real harness when it is configured to rewrite memories.

The cooldown exists because the hook fires once per turn, so a chain per turn is a chain per turn. Two independent guards bound it: the per-project PID file (`AcquireLifecycleLock` — a run is not already in progress) and `lifecycle.min_interval` (a run did not start too recently). The second is a file's mtime rather than a row, because the hook's synchronous path must not open the database to ask the question. The `ghost lifecycle` process writes it, once it has passed every precondition that can exit non-zero — so a run that dies before doing any work does not burn the window, and does not add to the silent-death class the failure marker exists to catch. A foreground run writes it too, which is what a user retrying the alert's command wants.

## Persistence and search

`internal/memory` owns:

- Project and memory CRUD
- FTS5 indexing and query sanitization
- Optional vector storage and cosine similarity
- Reciprocal Rank Fusion for hybrid results, with the result window chosen by
  `Store.fuseAndRank` delegating to `selectWindow` (see
  [Hybrid fusion and window selection](#hybrid-fusion-and-window-selection))
- Category-aware time-decay ordering
- Pinned and near-duplicate handling
- Directed memory links
- Snapshots, audit history, tasks, decisions, and usage data

### The vector scan

Both vector legs are brute force over the project's embeddings, and they run as
two phases in `internal/memory/vector_scan.go`. `snapshotVectors` copies every
candidate row out of SQLite under the store's read lock and its single
connection, and returns; `vectorRows.search` then scores the copy with both
released. The copy is the only part that needs either — the cosine pass is
O(corpus × dims) of float arithmetic over rows the store has already handed over,
so holding a read lock across it blocked a writer on the store for the length of
the whole corpus. It matters most for `ghost supersede`, which runs one search
per memory.

A writer is still kept out for the duration of the copy, and releasing the mutex
is not what fixes that: `OpenDB` pins the pool at one connection, so a write
cannot start while a query is streaming whether or not a mutex says so. The
connection, not the lock, is what a corpus scan contends on — which is why the
next change here is a cache that stops asking SQLite for the bytes, not finer
lock work.

The copy keeps the bytes as the database handed them over rather than decoding
them, and the score loop reads the little-endian `float32`s back out of the
blob in place (`cosineFromBytes`, bit-identical to decoding first). The scope
column is kept as text and parsed only for a row that wins a slot. A search
decides scope eligibility per row with a `scopeProbe`, which skips the
`json.Unmarshal` for a row whose stored text cannot mention a requested key —
`ScopeMatches` only ever excludes a row that names a requested key and disagrees
with it, so the substring test can save a parse but never decides membership.

Ranking holds a bounded window rather than the corpus: candidates are offered to
a min-heap whose root is the worst row held, so a row that cannot win a slot
costs one comparison, and the order is imposed once at the end over at most
`limit` rows. The order is total — cosine descending, exact ties broken by scan
position — so the answer is a function of the stored data *and* of the order the
scan yielded it in, rather than of the heap's internals. That is not independence
from the scan order and does not claim to be: the query has no `ORDER BY`, so a
`VACUUM` or an index rebuild can reorder a tie. What it does buy is that two
searches over the same rows in the same order return the same rows, which the
unstable `sort.Slice` it replaces did not promise either.

The scratch is recycled through a `sync.Pool` on the store, so a steady-state
search refills buffers it grew last time; an oversized embedding buffer is
dropped on return rather than retained for a query that may not return. What a
query still allocates is the database driver: `modernc.org/sqlite` copies every
BLOB and every TEXT column into a fresh Go allocation per row, which is the
floor for any `database/sql` scan and is the reason the next step here is a
decoded-vector cache invalidated by `PRAGMA data_version` rather than more work
on the scoring loop ([#556](https://github.com/wcatz/ghost/issues/556)).

### Hybrid fusion and window selection

Window selection lives in `internal/memory/vector.go` as three steps:
`fuseCandidatePool` fuses the FTS5 and vector legs into one ranking,
`scopeEligiblePool` narrows it, and `selectWindow` decides which memories
form the result window. They stay together because window selection is not
separable from fusion — the rule that keeps a keyword hit has to be stated in
terms of the scores fusion produced. Every production search reaches them
through `Store.fuseAndRank`, which fuses once and then cuts;
`FuseAndSelectWindow` is the exported entry point over the same seam, called
only from the fusion tests.

Fusion is Reciprocal Rank Fusion, weighted 0.3 FTS / 0.7 vector with k=60.
A memory retrieved by both legs accumulates both contributions, so a two-leg
match always outranks a single-leg one.

Before that fused score is sorted and cut, `demoteStatus`
(`internal/memory/demotion.go`) multiplies it by a status factor: a resolved
row, and a `_global` row when a specific project is being searched, score
`× 0.5`. The factor runs inside `fuseCandidatePool`, so it decides membership
too — a live project memory a raw-score cut would have lost to a demoted row
takes that slot. It only ever scales, so the demotion itself never excludes a
row: whether a demoted row comes back is the window's ordinary question of
rank, not a filter. With RRF k=60 the factor effectively ranks a demoted row
below every live candidate in the pool rather than below one comparable
neighbour: every fused score lives in a band of a few thousandths — the best
any row can earn is a rank-1 hit in both legs, 0.3/61 + 0.7/61 ≈ 0.0164 — so
halving one lands at ≈ 0.0082, under the ≈ 0.0088 that the deepest vector-leg
row in a default `limit`-10 window still scores and under the 0.0125 of the
deepest row in a keyword-only search. The only live rows a demoted leader can
still outrank are ones the vector leg never fetched (no embedding yet), whose
keyword-only score is capped at 0.3/61 ≈ 0.0049. That is what keeps resolved
memories and shared rules findable while stopping them from leading every
result.

The demotion only reorders what the legs already fetched: each leg pulls
`limit*2` rows from the project *plus* `_global`, so `_global` rows count
against that budget and neither leg drops them. When a project matches fewer
rows than the limit, the demoted `_global` rows are the only candidates left
and they fill the remainder — demotion decides which fetched rows lead, never
which rows are eligible. Session-start injection is outside all of this: it
ranks in SQL on two separate paths — `loadSessionContext`
(`internal/mcpinit/hook.go`) builds the session-start digest and
`Store.GetTopMemories` backs the MCP tool surface — neither reaches fusion,
and both queries already filter `resolved_at IS NULL`, so no status factor
changes what is injected.

A cross-project
search leaves `_global` undemoted (there is no project whose own memories it
could be padding), and explain mode reports the factor per row
as `status_factor`, computed by the same `statusDemotionFactor` the ranking
used.

Window selection reserves real estate for the keyword leg. A plain cut on the
fused score could not admit a keyword-only hit at all: the keyword leg's rank-1
row scores 0.3/61 ≈ 0.0049, while the vector leg's 20th row — still well
inside the fetched window — scores 0.7/80 ≈ 0.0088. A full vector leg
therefore outranked the best keyword match every time, and an exact identifier
match could never reach the results, which is the case FTS exists for.

So the best `limit/5` keyword hits the vector leg did **not** retrieve are
guaranteed a place in the window, evicting the weakest admitted rows for them
— unless the hit's status factor is below 1. The reservation reads raw
keyword rank while the demotion writes the fused score, so reserving a
demoted `_global` or resolved rank-1 hit would hand back exactly the slot the
factor had just taken from it; such a row still enters on its demoted score
like any other candidate — it comes back when the window has room and its
demoted score clears the cut, and drops out of a full window by either of the
window's two ordinary paths: enough candidates outscore it (at default
limits, any full window of live candidates the weights already put above a
halved keyword score), or the reservation's score-blind eviction hands its
slot to a top-`limit/5` keyword hit that scored below it — eviction replaces
the weakest admitted row that is not itself reserved, and a demoted row is
never reserved, so it is always an eviction target. Position is left to the
fused score: admission is
the defect, and the stronger
interventions were built and measured against the built-in dataset first.
Reordering the selected slice does nothing beyond admission, because
`decayRank` re-sorts by score on the way out. Flooring a reserved hit's score
does work, but the floor cannot be made safe — at the top of the window it
costs hybrid R@1 0.507 → 0.366 and NDCG@10 0.812 → 0.738, and at the median it
sits close enough to the row below that decay, which multiplies scores by a
category- and age-dependent factor, reorders it and breaks the invariant that
uniform timestamps leave the graded ranking untouched.

The window's width is `limit`, or twice that under `DecayReselect`, where decay
still has to narrow the set afterwards. Scope constraints are narrowed from the
combined candidate pool before this cut, including when one leg is unavailable,
so an out-of-scope row cannot consume a result slot and force the tool to report
absence for an eligible row that was retrieved but not selected. The hydration
backfill after the cut draws from that same narrowed pool, so a row that
disappears between the leg queries and hydration is replaced by the next
strongest *in-scope* candidate rather than shortening the result. Explain
mode calls the same scoped selection entry point, so its included rows and
scope-exclusion reasons describe the store result rather than an unscoped
ranking. Ordering is deterministic (ties broken by ID), because the demotion
penalties applied downstream depend on order.

The formatted `ghost_memory_search` path does not call that entry point
directly. It goes through `assemble.Run` (see [Context assembly](#context-assembly-target-design)),
which asks `Store.Candidates` for the same legs, parameters and configured
vector floor, and then applies the caller's category filter to the widened
candidate set before the window closes. Retrieval is unchanged; what moved is
which set the filter sees.

The main schema tables are:

| Table | Purpose |
|---|---|
| `projects` | Project names, IDs, and paths |
| `memories` | Core memory content, category, importance, tags, and state |
| `memories_fts` | FTS5 virtual table |
| `memory_embeddings` | Float32 embedding vectors, each stamped with the identity of the space that produced it (model, dimensions, task prefix), so a model change retires the old vectors instead of comparing across spaces |
| `memory_links` | Related, supersedes, causes, and other graph edges |
| `tasks` | Cross-session work items |
| `decisions` | Decisions, rationale, alternatives, and status |
| `ghost_state` | Per-project learned context and interaction state |
| `memory_snapshots` | Reflection rollback snapshots, including `scope` and its `scope_captured` marker (schema v14) so a restore can put scope back — and leave a live scope alone when the snapshot predates scope |
| `memory_history` | Append-only per-memory CHANGE LOG (schema v17, [#578](https://github.com/wcatz/ghost/issues/578)) — the name `memory_provenance` is reserved for evidence records, which are a different concept: one row per write, each holding the content, category, importance, `resolved_at` and source the memory had once that write landed, plus the other memory the event is about (`related_id`) and the text a merge folded in (`merged_content`) |
| `token_usage` | Reserved schema for future harness usage and cost records; current CLI adapters report zero token counts |
| `audit_log` | Destructive and consolidation operations |

### Retrieval

When embeddings are available, search combines:

- SQLite FTS5 for lexical and exact-identifier matches
- Cosine-similarity vector candidates for paraphrases
- Reciprocal Rank Fusion with the shipped 70% vector / 30% FTS weighting
- Category-aware decay applied to the surviving result window
- Targeted demotion when a present memory is superseded by another present memory

Without Ollama, the same API remains available with FTS5-only results. Search membership is not discarded solely because of age; decay changes ordering.

### Memory history

`memory_history` is the append-only CHANGE LOG of how a memory reached its
current state ([#578](https://github.com/wcatz/ghost/issues/578)). `memories`
holds only the last value of everything, so "which reflection run changed this",
"what did it say before" and "why is this here with this confidence" were all
unanswerable from the database. Every write that changes a memory's state appends one row, in
the **same transaction** as the write, so history cannot diverge from state:

**A history row's content is written through a redaction filter, and it is a replacement rather than a refusal.** The history is the one place Ghost keeps text it holds nowhere else — a memory row is overwritten by the next edit and gone by the next delete — so a credential redacted from the row by an edit or a delete has to be redacted from here too, or the redaction leaves a LONGER-LIVED copy than the row it was made to remove. `ghost_history_content` is a registered SQLite scalar function that every statement writing a history row reaches its content through (the batched append and the baseline's `NOT EXISTS` insert; they cannot each decide for themselves, because one of them forgetting is exactly how an unredacted credential lands while the append path claims to redact it). The filter is installed by `internal/memory/history_redactor.go` and the append path's gate is DERIVED from the redactor rather than kept beside it, so the two cannot disagree.

The redaction is whole-content, not a span, and the reason is worth stating because it looks like laziness and is not. `secret.Finding` carries a rule name and a human-readable label and no offsets, deliberately — its other consumer is a refusal message that must not quote the value — so a span-precise redactor would need a capture group per rule across the whole rules table plus offsets from all three post-table passes. The honest options today are "replace the content" and "keep the credential", and only one of those is a redaction. The trade is that a **false positive now costs one history entry's text**, where before the guard landed it cost nothing: the live row still holds the current text, and the entry still records that a change happened, when and by whom. And it is defence in depth rather than a live control — every writer refuses a credential on the way in, so a credential can only be in a history row if it was stored before that guard existed, which means the filter fires on nothing at all in a store that has never held one.

The filter runs on **every** appended row whether or not anything is redacted, because the only sound way to know is to look, and that cost is on the write path's critical section: measured at **~1.1 ms per 2 KB row and ~3.6 ms at the 8 KB content cap**, inside the transaction, so a batched append pays it once per id. Skipping the detector on rows that look clean is not available — a prefilter with a false negative is a silent leak, which is the same lesson `internal/secret` already teaches once in its own comments.

**The name is a reservation, not a description.** This is a change log — one
row per write, holding the state the memory had once that write landed. Evidence
provenance is a separate concept and is not this table: that is a future
`memory_provenance` holding SEVERAL evidence records per memory (kind, agent,
`session_id`, `source_ref`, confidence, `observed_at`, `verified_at`), answering
"who or what supports this memory" rather than "how did this row change". The two
are easy to confuse in prose and unrelated in fact, and a schema name is
permanent once released, so this table is named for what it is.
A development store built from a pre-rename commit of #664 can hold a
`memory_provenance` table from that build, which Ghost neither reads nor purges.
No release ever created it, and Ghost does not drop it because the name is
reserved: remove it by hand with `sqlite3 <db> 'DROP TABLE memory_provenance'`.

| Phase | Appended by | What the row records |
|---|---|---|
| `save` | `Create`, `Upsert` (new row or linked copy), the decision companion memory, the shipped seeds | the row as inserted |
| `merge` | `Upsert`'s near-duplicate fold | the target after its importance and access count were raised |
| `update` | `UpdateMemory` | the edited row (a content change may also clear `resolved_at`) |
| `reflect` | `ReplaceNonManual` — reuse, rewrite and fresh insert alike, including a verbatim re-emission that leaves every recorded column identical (see below) | the row as the consolidation left it |
| `resolve` / `unresolve` | `SetResolved` / `ClearResolved` | the row with the new `resolved_at`, or without it |
| `supersede` | `CreateLink` with a `supersedes` edge, when the edge becomes active | the **target**'s state, `related_id` naming the superseding memory |
| `unsupersede` | `InvalidateLink` on a `supersedes` edge, when a live edge is withdrawn | the target's state again — a withdrawal is a change, and a history that shows a claim and no withdrawal reads as though it is still live |
| `baseline` | the first write to a memory that predates the table, before that write | what the memory said when this build had never seen it |
| `restore` | `RestoreSnapshot` | the row as the snapshot put it back |
| `import` | `ImportMemory` | the imported row, attributed to the artifact's agent |
| `delete` | `Delete`, the replace's bulk delete, the restore's cleanup | the state the row held immediately before it went |

Three properties are deliberate:

- **Each row is a version, not a diff.** It holds the content, category,
  importance, `resolved_at` and source the memory *had once that write landed*.
  The prior content of any write is therefore the previous row's content, and
  "what did Ghost know at time T" is the newest row at or before T — a query
  `memories` cannot answer at all. Recording the state *after* the write is also
  the only shape an insert can have: an inserted row has no prior state.
- **`agent` and `session_id` are the performer, not the memory's own stored
  provenance.** The save, merge and import paths carry a `Provenance`; the
  lifecycle passes (`reflect`, `resolve`, `supersede`) and `Delete` know no
  session and leave both empty. An empty field is an admission, not a claim that
  nobody acted.
- **A `delete` row names its successor.** A consolidation rewrite or merge gives
  the row a new id, so without a pointer the old id's history simply stops, and a
  reader following one memory — what [#648](https://github.com/wcatz/ghost/issues/648)
  will do with a usefulness verdict — never learns the memory it holds a verdict
  about is now a different row. `ReplaceNonManual` stamps it from
  `Memory.ReplacesIDs`, which the reflection operations (#659) fill in; a row
  deleted for any other reason is left without one rather than given a fabricated
  successor.
- **`memory_id` has no foreign key.** A hard `DELETE` takes the row with it, so
  a cascading history table would be empty exactly when the audit is asked — the
  `delete` row is the tombstone, and it carries the text the memory held.
  `project_id` *does* cascade: deleting a project is meant to take its corpus
  with it. That makes `project_id` reassignment on a project *merge* load-bearing
  rather than cosmetic — a merge keeps the memories and deletes only the
  `projects` row, so a `memory_history` row left behind would be taken by that
  cascade while the memory it describes survived with no recorded past. Both of
  main's merge reassignment lists (there are two, one per implementation) carry
  the table.

A write that changes none of a memory's own recorded columns appends no row: one
repeating the previous state would record that nothing happened, at the cost of a
row per recall. `Touch` (`access_count`, `last_accessed`), `TogglePin` and the
resolve KEEP cache are in that class.

Two writers deliberately break that rule, for the same reason: the row is not a
state change but a record that a *pass* ran over this memory, or that a *claim*
now stands against it, and neither is visible in the row's own columns. A
consolidation that re-emits a memory verbatim — retention rather than
consolidation, see `reusePreservesAge` — appends a `reflect` row byte-identical
to the previous version, because "which reflection run touched this" is the
question the table exists to answer, and a memory no reflection has confirmed is
indistinguishable from one that has. A `supersedes` edge moves none of the
target's columns but does change its standing — "this is no longer current" —
and an audit blind to that is blind to the corpus's main staleness signal. Both
cost a slot under the cap below, which is what bounds them; neither is a licence
to repeat. The `supersede` row in particular is written when the edge *becomes*
active and not on every re-link, because `ghost supersede` re-writes a pair on
every pass whose endpoint moved, and a re-write of a live edge records no run and
asserts no new claim.

Two more writers can produce a row byte-identical to the previous version
without meaning to, and neither earns a special case: `UpdateMemory` called with
the values a row already holds (the MCP tool passes the caller's arguments
straight through, and its "changed" list is which arguments were supplied, not
what differed), and a fold whose `importance` increment has saturated at 1.0.
Both cost a slot under the cap, which is what bounds them, and both record a write
somebody asked for.

`MergeProject` and `PromoteToGlobal` are in the no-append class — both change
only `project_id` — and they are the writers that show the other half: appending
nothing is not the same as ignoring them. A write that moves a memory between
projects has to carry its history rows' `project_id` with it, because that column
is what the project-delete cascade follows, and both do. Without it, deleting the
project a memory was promoted out of takes the recorded past of a memory that is
still live in `_global`, and `ghost history <id>` reports that it was never
written.

**Redaction.** The history is the one place Ghost keeps text it no longer holds
anywhere else, which makes it a liability as well as an asset: a memory row is
overwritten by the next edit and gone by the next delete, while its earlier
versions sit here. So a secret that was "removed" by deleting its memory is not
removed — it is in a table with a longer life than the row, and `ghost history`
prints it. Three things follow:

- **A purge path, in both directions.** `ghost history purge <id>` and
  `ghost_memory_delete`'s `purge_history` argument delete a live row and every
  history row for it in one transaction, so a memory and its history cannot come
  apart. A memory that is ALREADY deleted has its history erased on its own
  (`Store.PurgeMemoryHistory`, reachable from both entry points), which is the
  case a delete-time purge cannot cover: the tombstone is the feature, so a
  redaction asked after the delete would otherwise report the memory as not found
  and leave the text on disk. A plain delete deliberately keeps the history —
  that is what makes the table worth having — and an id whose history still exists
  is one `ghost import` refuses to write into, because the artifact's ids are
  verbatim and the two records would splice under one id. The second direction is
  `Store.PurgeMemoryHistory` alone, and it differs in kind from the two entry
  points above: it erases recorded text and leaves a live memory row exactly as it
  was, because "erase the history of this memory" and "delete this memory" are
  different requests. The entry points do not have that choice — they are a
  delete, and the live row goes with its history.
- **A purge reaches every copy this database holds**, not just the history table.
  `memory_snapshots` is the one that matters: every applied reflection copies each
  non-manual memory's full content into a snapshot, the column has no foreign key,
  and `ghost reflect --restore` re-inserts the row from it under the memory's
  original id — so a purge that left the snapshot behind would report success on a
  secret one `ghost reflect --restore` away from being readable again, with no
  history to show it came back. A FoldOnly fold's discarded wording is the other
  copy: it sits in `merged_content` on the *target's* row, so those cells are
  redacted rather than their rows deleted (the event is worth keeping; the text is
  what the purge is for). Snapshots are matched by content as well as by id, because a
  pre-v13 snapshot recorded no id and `ghost reflect --restore` matches those by
  content — an id-keyed delete would have left the one snapshot that can resurrect
  the row. What a purge still cannot reach is a backup taken before it, or another
  machine's copy of the store, so the command reports what its transaction covered
  rather than that the text has ceased to exist.
- **A redaction seam on the way in.** `ghost_history_content` is a SQLite
  function called by the append statement itself, so content is rewritten inside
  the one statement that copies the state out of `memories` rather than in a
  second pass that would leave an unredacted copy on disk. It is wired to
  `internal/secret`'s `Detect` when [#656](https://github.com/wcatz/ghost/pull/656)
  lands; until then it is the identity, and the plumbing is tested.
- **The pre-v17 gap is not a purge's job.** `migrateV17` records no starting row,
  so a memory that predates the table has no history until something writes one.
  The first write that would destroy its text files a `baseline` row first
  (`recordBaselineHistoryTx`), and a backfill is the documented alternative that
  was not taken: it would assert what every existing row said at the moment of
  the upgrade, which nobody recorded, on a store with a large corpus.

**Growth policy.** The table is append-only, not unbounded, and both bounds are
applied in the appending transaction — so neither needs a background job or a
clock, and neither can be lost to a crash between the write and its cleanup:

1. **Per memory**, only the newest 50 versions survive
   (`historyVersionsPerMemory`), ranked by `rowid` rather than `recorded_at` —
   which is second-precision, so every write one reflection makes in a pass shares
   a timestamp and the order among them would be a tie-break. Ranking by `rowid`
   also means the newest row can never be taken, so a memory always has one
   statement of what it says now, and a memory under the cap keeps everything —
   which is why a rarely-changed memory never loses its `baseline`. One
   `ROW_NUMBER()`-windowed `DELETE` covers the whole batch, not one statement per
   id.
2. **Across the store**, only the newest 20 000 rows survive (`historyRowsCap`).
   The per-memory bound cannot do this job — every created-then-dropped memory is
   a distinct `memory_id` with a couple of rows of its own, and one applied
   reflection can churn the whole non-manual corpus. The cap is a rowid window
   rather than an `ORDER BY` over the table: an implicit rowid is `max(rowid)+1`
   and is never reused, so rowid order *is* insertion order.

Both questions are asked in **one** statement, and each `DELETE` behind them runs
only when the answer says a cap is actually exceeded (`pruneHistoryTx`). That is
not tidiness. This runs inside the caller's write transaction, so everything it
adds is time the write lock is held, and the write lock is the one resource
concurrent writers queue for — see the concurrency contract above. A first
version that ran the windowed `DELETE` and a separate `max(rowid)` probe on every
append added three statements to every save, and `TestConcurrentProcessesMixedReadWrite`
began failing with `SQLITE_BUSY` at `BEGIN IMMEDIATE`: `busy_timeout(5000)` is a
bound, not a guarantee, and a per-upsert statement that looks free in a
one-process test is what reaches it. A memory under the cap and a table under the
global cap are the normal state, and then the only statement is the probe.

Two more decisions on that same critical section, both measured rather than
assumed:

- **One index, not two.** `idx_history_memory(memory_id, recorded_at)` is the
  only one, and it serves every reader this build has: the per-memory cap ranks
  by rowid (the implicit index, free), `MemoryHistory` filters on `memory_id`, and
  an `as_of` read (#647) — "the newest row for this memory at or before T" — is
  the same index with a bound. A standalone `recorded_at` index would have no
  reader, and every append would pay a second b-tree insert to keep it current.
  The migration test asserts the *absence*, so adding one back "just in case"
  takes an edit rather than happening quietly.
- **The content filter is called only when one is installed.**
  `ghost_history_content` is a Go function reached through the driver, so calling
  it unconditionally means a cross-language call per appended row to do nothing
  while #656 — the redactor it exists for — is not on main. With a redactor
  installed the call is back, and that is the only case that pays.

`Store.MemoryHistory` reads one memory's history oldest first — a changelog, not
a log tail — and `ghost history <memory-id>` prints it.

A vector candidate is only scored when its stored vector belongs to the vector
space this process embeds into. Every embedding records that identity — model,
dimensions, and whether the model needs a task prefix (`nomic-embed-text` takes
`search_document: ` on stored text and `search_query: ` on queries, which is why
both halves have to move together) — as
`<model>[:<dimensions>][+prefix]` in `memory_embeddings.model`. A row recorded
under a different identity is a vector from another space, so it is excluded
from the leg and handed back to the embedding worker as unembedded rather than
compared with a query it cannot be compared to. `SetEmbeddingIdentity` supplies
the configured side from `embedding.VectorIdentity`, and the rules that read it
are four, deliberately separate by caller:

| Rule | Site | Applies to |
|---|---|---|
| may this stored vector enter a search? | `loadVectorRows` | both vector scans (`SearchVector`, `SearchVectorAll`), which share one snapshot and one scoring pass |
| may this stored vector act as a *query* vector? | `GetEmbedding` (returns nil) | the link worker and `ghost supersede`, which both search from a stored vector |
| may this memory be compared at all yet? | `UnscannedEmbeddedMemoryIDs` | the link worker's queue, so a foreign row is neither paired across spaces nor marked scanned |
| is this memory covered? | `EmbeddingStats` | `ghost mcp status` and `ghost_health`, which must not report full coverage mid-re-embed — and split the uncovered rows into stale (a vector under a retired identity) and unembedded (no vector at all), since only the first kind has something to rewrite |

Rewriting a row under a new identity also clears the `link_scans` slot the
memory earned in the old space (in `StoreEmbedding`, before the upsert), so the
memory is re-queued for linking and the linker compares it again in the new
space. The re-scan adds current-space edges alongside the ones the old space
produced — nothing deletes the old `related` rows (only `supersede` calls
`InvalidateLink`), and `CreateLink` upserts with `MAX(strength, ...)`, so an
edge whose new-space similarity is lower keeps the strength the old space gave
it; retiring those edges is a separate decision this change does not make. The
failure ordering of the delete and the upsert fails safe in both directions, so
they need no transaction. The foreign-vector warning in `loadVectorRows` is
likewise logged once per retired identity rather than once per search — a
process that reconfigures twice warns about both retirements.

The query-side rules matter because the filter only guards the *rows*: a stale
vector used as a query would be a cosine between two spaces, and the number it
produces becomes a `related` edge or a `supersedes` candidate. The derived store
`ExplainSearch` runs on inherits the identity too, or the trace would report a
ranking the search did not produce. A retired vector never hides its memory: the
text stays in the keyword leg until the row is rewritten.

### Memory lifecycle

`reflect` replaces non-manual/non-builtin memories through a tiered consolidator. It snapshots before replacement, rejects empty results, preserves manual and Ghost-shipped builtin memories, and can restore the latest snapshot.

The LLM tier does not return a rewritten memory list. It returns operations on the ids the prompt renders: `keep <id>`, `merge <id>,<id> -> <text>`, `rewrite <id> -> <text>`, `drop <id> reason: obsolete | superseded by <id>`. A memory the harness has nothing to change is carried through by `keep`, which re-emits the stored row byte for byte, so the row is updated in place and keeps its id, embedding, link graph, age and source. That is the whole point: under the previous free-text contract only a byte-identical re-emission kept an identity, so retyping — including retyping in order to improve — gave the memory a fresh id and cascaded its embedding and links away with it, which a maintenance benchmark over 865 real memories measured as 12 of 13 new ids in one project and 15 of 17 in another. A merge derives its category from the first id it names, its importance from the strongest source, its tags from their union and its scope from the same heuristic the SQLite tier uses: a consolidation run does not get to relabel what a memory is, least of all unattended and into `_global`. A merge or rewrite whose text carries a path, hash, version, hostname or number appearing in none of its sources is rejected and its sources are kept unchanged — the model was measured turning `2.BeXIAhbj.js` into `2.BeXIAhbq.js` while only copying, and Ghost cannot know which side is true, so it declines to write either. That is also why a `rewrite` corrects a claim and never a specific: the corrected value would be an identifier the source does not contain, so the prompt asks for `keep` (or `drop`) instead, and the check is never asked a question it has to refuse. Anything Ghost cannot read — an unreadable operation, an id it did not supply, an id claimed by two operations, a supersession whose target the same response drops, a response carrying no operations — fails the LLM result outright and falls through to the deterministic tier, because applying the readable half of an operation list is a partial consolidation.

A memory leaves the corpus only when its id is named as a merge source, named for a rewrite, or named in a drop with a reason — **and** a surviving output accounts for it, which is the drop guard's 45% token containment with a merge source measured against its own merge — or `--allow-drops` accepts the deletion. Naming an id is necessary but not sufficient: a rewrite whose replacement does not carry the old row's substance leaves that row in the corpus verbatim, and a `superseded by` drop naming a successor that shares nothing with the row disposes of nothing. Every other input is carried through verbatim, and that pass-through is what makes the tier zero-loss rather than merely careful. It is also what an earlier draft got wrong: an unnamed memory used to be emitted by nobody, so it survived only if the token guard *failed* to recognise a survivor — and a guard false positive in the "absorbed" direction is a silent deletion. A measured run lost "SSH to the bastion goes through port 2222, not 22" that way, with no warning and no `--allow-drops`, because an unrelated survivor shared 45% of its tokens. A guard can only re-add what it flags, so an id the harness never mentioned needs no inference at all. An operation may echo the `id:` label the prompt prints in front of each memory; the label is stripped rather than failing the pass.


An LLM tier's answer is additionally bounded by a scale-aware quality gate, from six memories up: a small input must retain 30% of itself, and a large backlog must return at least an absolute handful — `gateBacklogMinOutput`, value 5. The floor is per TIER, so it only bounds a run with nowhere else to go: the `auto` default drops to the mechanical SQLite tier, which is exempt because Jaccard dedup cannot truncate or hallucinate, while `--require-llm`, which omits that fallback, reports a rejection as a failed run. It is absolute rather than a fraction because the prompt asks for a corpus of high-quality memories rather than a fixed count, and a percentage floor would demand more output than that ever asks for on a large backlog. `--tier cli` and `--tier opencode` are held to it too: naming a backend used to hand the bare LLM tier straight to the apply path, where the gate does not live, so those invocations had no floor at all; they now run through the same one-tier wrapper, which adds no fallback and so keeps "exactly this backend" meaning exactly that. The wrapper does not change the reported tier name, so `Consolidator: cli` still prints `cli` rather than `tiered:cli`.

Before replacement, every input memory is audited for a surviving merge target, in all eight categories: an input under 45% token containment in the output it is compared against is re-added verbatim rather than deleted, and `--allow-drops` accepts the deletions instead. With the pass-through above the guard no longer has omissions to rescue, and what remains is narrow. A **merge's** sources are measured against the text of **that merge**, which is the only witness available: a merge source is consumed by its merge, so its own text is never in the result, and an id claimed by two operations is rejected, so a sibling cannot be there either. It used to be measured against the union of the outputs, which was right when the union was a handful of survivors and which the pass-through turned into the whole project's vocabulary — every id the response never named is emitted verbatim — so a source whose substance its merge discarded passed containment on the strength of an unrelated memory that happened to share its words. That is the same class of loss the pass-through exists to remove, reintroduced through the merge branch. A merge that is not in the result has no witness at all, and its sources fall back to the strict per-output test. Everything else — an explicit `obsolete` drop, and every input of the offline SQLite tier, which names no ids — is measured against a **single** output, because there the guard is the only thing between a claim and a deletion and it has to be answered by one survivor. **There is no exemption.** An input the response **disposed of** — named for a rewrite, or named in a `superseded by` drop — is audited exactly like an `obsolete` drop: the corpus has to be able to show the row is gone. An unattended reflect never deletes a memory on the model's say-so alone. The result does still *record* the ids a response disposed of and the text it says took their place, and `ghost reflect` prints each of them under `Disposed of (model's claim)`, so a person deciding whether to pass `--apply` can see that the model tried to drop something; the guarded-drop report beside it says what was actually retained or deleted. The record is not consulted to decide anything, and the report deliberately does not predict the outcome — a second opinion about it would be a second implementation of the guard's decision waiting to disagree with the first. The carve-out that used to exist was the hole: a `superseded by` drop disposed of a row whenever the named successor's text was in the result, and the parser's only rule is that the target survives the response — so naming a neighbour was a valid operation, and two ordinary deployment notes in the same project, paired because they sat adjacent in the prompt, was a silent deletion with no warning, no `--allow-drops` and a zero exit status. Requiring the witness to be *related* closed that and then turned out to be redundant, because the witness text is itself an output and a row at 45% containment against it also passes the per-output scan; the whole branch was dead code. The rule is **keep-biased** on purpose, and the reason is repairability: a stale row that is kept can be demoted, because the lifecycle runs `resolve` and `supersede` immediately after `reflect` and demoting a genuinely stale row is their job, while a deleted row is gone. The failure direction is therefore a duplicate — the old text back beside its replacement — never a silent deletion. The cost is real and worth naming: a supersession whose successor is reworded below the containment bar leaves the stale row behind (this project's own superseded note about calling the Anthropic API client directly scores 0.429 against its replacement), and it stays visible until `resolve` or `supersede` demote it, so a corpus of those grows the input each pass has to read. That is the right way round for a command that rewrites a memory store with no human in the loop.

A round is also reported when it compresses hard: a corpus of six consolidatable memories or more that keeps less than half of them prints a `>50% reduction` warning, counted as the memories the project ends up **holding** rather than the ones emitted under the project scope. A cross-project candidate is counted as retained in two of the three promotion states and not in the third, which is why the count has to be taken where it is known. With promotion off every candidate is folded back into the project, so all of them are survivors; with promotion on, a candidate that could not become a `_global` row is written back into the project too and is likewise a survivor; but a candidate that *did* become a `_global` row has left the project deliberately and is not counted as retained, because counting it would overstate survival and hide a compression that did happen. Only that third state is knowable after the write, so an apply with promotion on reports after the write rather than before — a count built from the project-scoped memories alone would understate retention by the whole candidate set, and a run where every promotion failed would report a far larger reduction than actually occurred. A dry run has no write to wait for, so it reports before. With promotion on that is the optimistic count and the note says so, since a candidate it cannot yet place is counted as though it had left the project; with promotion off every candidate is folded back into the project either way, so the count there is already the final one. It is a warning, not a refusal: the number of operations a response carries says nothing about how much it folded, and on the unattended lifecycle path it is the only report a compression gets, on the stderr of a detached process, with the exit status still 0.

## Credential guard

**No write path stores a credential value.** Stored content is not private to the machine that wrote it: it is embedded, replayed into the context of every later session in the project, mirrored into the Obsidian vault, and quoted into the prompt of the next `reflect`, `resolve` or `supersede` call — which is a CLI harness talking to a third-party model. A credential that reaches the database has already been copied to all of those, and the database is a file the user syncs and backs up.

`internal/secret` decides whether a value looks like a credential, and the store layer enforces it. `memory.rejectSecret` is the single seam, called before any statement on every path that writes caller-supplied text: `UpsertWithOptions` (and therefore `Upsert`, `UpsertWithProvenance`, `ghost_memory_save`, `ghost_save_global`, and the first-contact import), `UpdateMemory`, `RecordDecision` (including each entry of `alternatives`), the three task writers, `UpdateLearnedContext`, and the four portable importers `ImportMemory`/`ImportTask`/`ImportDecision`/`ImportProject` (a project's name and path are replayed into every later session's digest too; `repo_remote` needs no guard because `NormalizeRepoRemote` strips the userinfo). The importers are the ones that matter most for where the text came from: they write with raw `INSERT`s rather than through `Upsert`, and their input is a JSONL artifact that arrived from somewhere.

The importers place the guard in a narrow window — after the `SELECT 1 FROM <table> WHERE id = ?` presence check and before the `apply=false` early return — and both edges are load-bearing. Above the presence check, a record already in the store would be *refused*, turning the portable format's "never overwrites an id that already exists, so re-running is always safe" into a hard failure over a row that is not being written; re-importing an artifact a pre-guard build exported would fail on records that were previously no-ops. Below the `apply=false` return, a dry run would classify a record differently from the apply run it previews, and that parity is the only reason a dry run is worth running. A refusal is a `*SecretContentError` unwrapping to `memory.ErrSecretContent`, naming the field and the credential format and never the value — the message itself reaches the log, the saving agent, and a model.

The detector works on the *shape* of a value, never on the words around it. That is the whole design, and it is what separates it from the keyword heuristic reflection already uses for a different question (`looksLikeSecret`, which decides whether text is safe to *widen* to every project and therefore must not reject a save). A memory store's ordinary vocabulary — "rotate the password quarterly", "the access token lives in the runner env", "the tokenizer keeps a 30k vocabulary" — must keep saving, so every rule needs a value: a fixed provider prefix at a realistic length, a complete PEM block, a labelled Cardano key field, an unbroken hex run long enough that it cannot be a digest and mixed enough that it cannot be padding, a whole 24-word BIP-39 mnemonic, or a credential-shaped name assigned a value no one documenting the setting would have written. `internal/secret`'s tests are half positive and half negative for that reason; the negative half is this repo's own database. Two of the rules need the matched text inspected rather than a boolean, so both walk *every* candidate in the save: a first-match implementation would let a `${DB_PASSWORD}` placeholder earlier in the text shadow a real credential later in it, which is not an adversarial ordering but what an incident note reads like.

Reflection output is filtered at the write boundary, and both halves of that position are load-bearing. `cmd/ghost`'s `applyReflection` is the single seam every proposal passes through on its way to `ApplyReflection`, and it runs *after* the drop guard's audit. `dropCredentialProposals` returns fresh slices rather than filtering in place, and `applyReflection` returns those **post-drop** slices to its caller: the `Applied: N memories consolidated` line counts them, so returning only a count left it counting the caller's pre-drop lists, and a partial drop reported 2 written when 1 was. It is a drop rather than a refusal because `ApplyReflection` replaces a project's corpus in one transaction, so a refusal would roll back an entire consolidation over one hallucinated value. It is after the audit because the audit asks which **inputs** the output failed to account for, and `executeOps` emits every unclaimed input verbatim — so a memory the tier carried forward is byte-identical to the output that carried it. Filtering before the audit makes that input look unaccounted for, and both outcomes are wrong: `RetainGuardedDrops` re-adds it verbatim and the credential is written back, making the drop a no-op *after* the audit has printed its content to stderr; or some other output happens to cover 45% of its tokens, the audit stays quiet, and `ReplaceNonManual` deletes the stored row with no `--allow-drops` — the one deletion path the drop guard exists to close. At the boundary the memory is still accounted for, so `RetainGuardedDrops` does not re-add it and the drop is not a no-op. It is, however, **deleted from the store**: `ReplaceNonManual` removes every replaceable row the emitted set does not account for, and this one no longer is. That is the correct outcome — the stored row IS the value, and leaving it is the leak — but it is a deletion, so the report describes the mechanism rather than counting anything — `dropped` counts *proposals*, and the rows the replace removed is a different number in both directions, since a fresh merge carried nothing and a `manual`/`builtin`/pinned/resolved row is not in the replace's candidate set — and it fires only when a project replace actually ran. A round the drop empties writes nothing, is reported as `Applied: nothing` with the surviving row named, and **records no skip fingerprint**: the corpus is byte-identical, and a fingerprint over it is what makes `--skip-unchanged` skip the project forever, so a stored credential would never be revisited. `applyReflection` returns an `applied` flag for exactly this, and it is false whenever nothing was written; a removal claim printed before anyone knows whether a replace runs closes the incident in the operator's head while the value sits in the database. `--allow-drops` does not gate it, because that flag is the operator authorising what the **model** chose to drop. Clearing a credential out of a database is therefore not only a report-first scan: an unattended `ghost reflect --apply` removes the rows it finds, and says so.

The per-drop report carries format, category, scope and length, never content — and that guarantee is enforced at every print site in `ghost reflect` (all three of them), not only at the write boundary. The scope is named because a grep for `.Content` in `cmd/ghost` finds three more sites in two other commands: `runResolve`'s listing uses `firstLine` at 70 characters, and its input is the stored corpus the write-boundary guard already refuses — a narrower claim than "resolve cannot print a credential", since a pre-guard database could still hold one; and `printHistoryEntry` prints history content raw, which is deliberate, because history is the one surface where a credential outlives the row it was removed from and the substitution belongs at write time (`ghost_history_content`) rather than at a print site. The consequence is that `ghost history` renders whatever is stored, so its guarantee is the filter's. `displayProposal` substitutes `<withheld: format, category, bytes>` for the content wherever `ghost reflect` echoes a proposal or a guarded drop, because the proposal listing and the drop-guard warning both printed 120 truncated characters and that stdout is the append-only `lifecycle.log` in the autonomous path. `previewContent` in `internal/reflection` does the same for the three log lines the tier writes — the fabrication, contamination and grounding rejections — because a 160-rune prefix is a copy of the content into an append-only log, and all three run *inside* the tier, before `cmd/ghost` sees anything and therefore before the boundary drop. A guard that has to be remembered at three call sites is one refactor away from not existing, so the check is in the one function they share. A boundary-only guarantee is not a guarantee about the command's report.


**The Cardano rules are discriminated on the CBOR header**, because the negative corpus for them is not prose — it is the key file formats themselves, and they collide by construction. A signing key and a *verification* key are the same 32 bytes, so both carry `5820`; an *extended* key is a 32-byte key plus a 32-byte chain code, so its private half and its **published** half both carry `5840`; the KES signing key carries a two-byte length (`5902 60`) that a Plutus script also carries. Only three things separate them: the CBOR length byte, the `"type"` envelope, and the surrounding line.

| CBOR length | shape | verdict |
|---|---|---|
| 32 (`5820`) | cold / payment / stake signing key, **and every verification key** | refused only on a line that also names a signing key |
| 64 (`5840`) | **extended** signing key, and the published extended verification key | refused unless the enclosing JSON object names a verification key and labels the value |
| 128 (`5880`) | no published counterpart | refused on the tag |
| 608 (`5902 60`) | the KES signing key — the one two-byte length that is a key | refused on the tag |
| 417+ (`5901 a1`…) | a Plutus script wrapper | stored |

A 64-byte value is refused **unless its enclosing JSON object publishes it** — it names a `VerificationKey` *and* labels something as key material — because a 64-byte value is an extended key and the verification half of that pair is published; refusing the tag unconditionally refuses a memory about a wallet. Both conditions, because either alone is satisfied by a document that merely mentions the other. The run floor is **68** hex characters, not 64: a 32-byte transaction hash is 64 and a key's `cborHex` adds a 4-byte CBOR header, and "signed with payment.skey, submitted tx <64 hex>" is one of the most common Cardano memories there is. The walk is a post-table pass, not a rule, because the decision needs the length *and* the envelope *and* the line.

**That exemption has had three scopes, and the sequence is the lesson.** It started as the whole text, which made it a property of the memory rather than of the value: one `ghost_memory_save` recording a pool's published `cc-hot-StakeExtendedVerificationKey` — a phrase a block producer writes constantly — also stored an extended **signing** key pasted anywhere else in the same content. Scoping it to the **line** fixed that and broke pretty-printed envelopes, where `"type"` and `"cborHex"` are on separate lines, which is what any indented document is. The scope that holds both is the **enclosing object**, bounded by its own braces: a JSON document's fields *are* the envelope, so a vkey name on the line above its own value describes that value, and a name in a different object does not. The backward scan is the part that carries it, and it stops at a closing brace as well as an opening one — a `}` above the value means the nearest object has already closed — which is what separates two objects in one memory from one object in one memory. A closing brace is required *after* the value too, so a truncated document is refused rather than exempted, and the backward read is fenced at 4 KB. The 32-byte branch still reads the **line**, and should: a 32-byte value is a signing key or a verification key with nothing in the envelope to tell them apart, so the line is the widest scope that can carry a decision. A `"type"` field alone is not a key either — `{"type": "StakePoolSigningKey_ed25519"}` in a runbook is documentation, and a pool operator's runbook names that type on its own, so the type rule requires a value beside the name.

**A URL userinfo password** is refused unless it is a template (`${VAR}`, `<db_password>`), not all capitals, not a placeholder word, not a product's default account name, and not equal to its own user — `postgres://postgres:postgres@localhost` is the most common DSN in a README and is not a secret by any reading. A field named after a **digest** of its value (`token_sha`, `fingerprint`, `checksum`) holds a digest, not the value, and storing a digest is the correct thing to do. A value that is a **dotted identifier path** names a place rather than holding a thing: every credential encoding Ghost knows excludes `.`, so `opts.OAuthClientSecretFromVault` and `secrets.GITHUB_TOKEN` are the same judgement arriving through different surfaces.

**The per-match cost is bounded by the next assignment, and the report is scanned in full.** Two post-table passes need the text after a value — the shell-variable test and the quoted-argument scan — and both used to recompute the rest of the *text* for every match, so a save holding *n* assignments cost *n* scans of everything after the first. There are now three bounds, each tightening the last: line ends are indexed once per `Detect` and found by binary search; a command's argument list ends at the next `;` or `|`, because a pipe is where a new command begins; and it also ends at the **next candidate**, which the walk already knows. So each lookup reads one command's worth of text and the lookups do not overlap. 256 KB measures in the low hundreds of milliseconds, pinned by `TestDetectIsLinearInLineLength`. The mnemonic pass also built a `[]string` of every word in the content before looking at the first, which was 67% of all allocations (~10 KB per 4 KB save; now one allocation per call).

Two things are recorded rather than fixed. A keyword prefilter in front of each rule is the obvious remaining win — the profile puts essentially all of the ~2 ms per 4 KB save in regexp's own machine, over about twenty backtracking scans — but Go's `Regexp.LiteralPrefix` returns empty for a pattern whose first instruction is a word boundary, which every rule here has, so a *derived* prefilter is useless and a hand-written one per rule is a silent false negative waiting to happen. **Both cost properties are mutation-verified, and finding out took a second fix.** The first attempt was not enough and the check that proved it was a fixture I had got wrong: the linearity test held many `$relay_addr` tokens but no `=`, so `assignmentRe` matched nothing, the loop never ran, and the quadratic code was never on the path. A performance test that does not execute what it bounds cannot fail. The fixture now plants complete `$aN = <22 characters>` assignments with keys that are deliberately not flagged, so the walk runs to the end of the line where the cost is paid — and it immediately failed, at 66x the time for 8x the input.

The line index had removed the newline scan and the statement cut had removed the scan to the next `;`, but a line with **no separators** — which is exactly what a wall of `$a = value` is — still left the flag regex reading to the end of the line for every candidate on it. The bound that holds is the **next candidate**: a command's argument list ends where the next assignment begins, which the walk already knows, so each lookup reads one command's worth of text and the lookups do not overlap. 2,000 candidates on a 52 KB line went from **1.83 s to 27 ms**, and 8x the candidates now costs 10x the time. The linearity assertion is the **median of per-round ratios over interleaved rounds**, and two earlier estimators failed in opposite directions, both of them the estimator rather than the property. A single shot per size passed locally and read 7.3x on CI (944 ms then 6.9 s) — a loaded runner, not a quadratic scan — which made a 6x bar a red check. Taking the minimum of five runs per size was **worse**: the small measurement benefits more from noise removal than the large one does (514 ms against 2.28 s), so min-of-N raises the ratio. Timing both sizes inside one round is the robust form, because a GC pause or a stolen timeslice inflates both and cancels. The bar is **8x**, which is not a round number but the geometric midpoint between the linear prediction (4x) and the quadratic one (16x) — the tightest bar that treats the two failure directions symmetrically. Measured: 3.4-4.5x here, and **16.04x with the per-match rescan restored**, so the separation is roughly 2x on each side.

**Nothing bounded `detectQuotedArgument` until this round, and the gap was real.** Both cost fixtures have no flag after the value, so `valueIsCommand` is false — and the quoted scan sits inside the same `if shellVar && valueIsCommand` — so neither reached it, while a comment claimed one of them did. `TestDetectBoundsTheQuotedArgumentScan` closes it, and its fixture has to satisfy **four** things at once: a flag after the value, no quoted argument that looks like a credential, a value `isCommandWord` accepts (so the loop `continue`s instead of falling through to a finding), and **a quote character on the line**, because the scan returns before its regex when the line has none. That last one is what made the first version of this fixture useless: the mutation that made the scan read the whole line instead of the window passed at 7.85x, identical to the honest reading, because the changed line was never executed. `ConvertTo-SecureString` is what makes the third reachable — it is a real cmdlet of 22 characters, which clears the 20-character value floor `assignmentRe` requires, where `Get-Credential` at 14 matches nothing at all. With a quoted `-Label "pool"` on the line, the same mutation reads **100x** against a 7.9x honest reading.

Its bar is 20x, and the arithmetic is recorded because I got it wrong first: with 8x the input the **linear** prediction is 8x, so a bar of 8x sits exactly on an honest measurement — this one read 7.85x and would have failed on a quiet machine. Quadratic here is 64x, so the geometric midpoint is sqrt(8·64) = 22.6.

That fixture also had to change, and it is the second round of a lesson this work has now taught three times. It carried a `-asPlainText` flag after each value, added so that "each candidate reaches the flag test" — and it did, and the test went on **passing with the quadratic code in place at 4.02x**, because a flag makes the flag test return true, the walk returns on the *first* candidate, and the per-match cost is never paid. Reaching an expensive line is not what exercises it; scanning the rest of the line to decide is, and that happens whether the answer is yes or no. Without the flag the same mutation reads 16.04x and fails. A fixture that returns early cannot bound anything.

The mnemonic splitter is checked by **allocated bytes scaled against content length** rather than by allocation count, because `strings.FieldsFunc` returns one slice however many words it finds — a count cannot see the regression at all. Nor can an absolute bound: race instrumentation alone moves the same code by two and a half orders of magnitude, so a bound tight enough to be meaningful locally fails on a loaded runner. The bar is a **ratio** — 8× the content must not cost more than 1.5× the bytes per call — set from the four numbers the two forms actually measure, in both instrumentations, with all four written down beside it: the walking form 1.0× plain and 0.3× under `-race`, the collecting form 10.6× and 2.7×.

Getting an instrument that could carry that took three attempts, and the two that failed are worth recording because each looked like a flake and neither was. `testing.Benchmark`'s `AllocsPerOp` is an average over a GC-paced loop, so it measures the whole process's allocation state: run it inside a package where other tests are also allocating and it measures them too, and the byte ratio failed intermittently at 0.8× against a 2.0× bar while passing every time it ran alone. Minimising over five runs narrowed that and did not remove it. `MemStats.TotalAlloc` with GC off for the window is **exact** rather than sampled — 129 B/op at 4 KB and 129 B/op at 32 KB, 1.0×, identical across eight consecutive runs. The residual noise was then the process's own floor: eight calls of a 500-byte fixture is a 4 KB signal that one background allocation doubles, so the 4 KB figure read 569, 8975, 3582, 569, 8975 across six runs. And the figure stayed bimodal at 421 or 4,203 B/op until the cause was `runtime.GC` clearing every `sync.Pool` — Go's regexp keeps its match machine in one, so the first call after a collection rebuilds it — which is a **one-time** cost measured as a per-call one. The window now warms up after the collection. Both assertions are ratios rather than timings, and both were confirmed RED against the code they name under `-race` as well as plain.

A refusal never prints the value it refused, and that reaches the *report* paths, not only the error. `ghost import` echoes a record's own words back in its per-record line and in its rejected list, so `portable.safeDetail` substitutes the record's id whenever the **whole** of the field holds a credential — not the 60-rune prefix the label is cut from, since a token is longer than the cut and `ghp_Ab12Cd34Ef5…` scans as clean and prints half a live credential on a record being printed for an unrelated reason. The prefix is still what gets printed, so a record whose first line is clean keeps its preview, and `labelOrID` and `printRecordLine` both refuse the detail outright for a `*SecretContentError`. The substitution is where the detail is built rather than where it is printed, because a record can be rejected for an unrelated reason — a missing task status, an unknown project — and still carry a credential, and because `RecordResult.Detail` is consumed from another package.

The rule is measured against a real corpus as well as against hand-written cases, because a memory about credentials is full of things that look like one. Running the detector over longmemeval-s (500 records, ~25,000 conversation turns) found eleven distinct lines it flagged; **three are genuine leaked credentials** — a `dckr_pat_` Docker Hub token, a `gho_` GitHub OAuth token, and a 40-character hex SparkPost key — and the other eight were pasted code and pasted documentation, each of which fixed a gate: a value that is an expression (`PasswordResetTokenGenerator().make_token(user)`), a key that is a shell variable (`$domainAdminPassword = ConvertTo-SecureString …`), a value that contains its own key's name (`accessToken: req.body.accessToken`), and a PEM rule that matched a Snowflake help page naming both markers in one sentence. A backslash is deliberately *not* an expression character, because a markdown-escaped `dckr\_pat\_…` is a real token.

Three boundaries remain deliberate. `CreateFromCorpus` is outside it, and exists because of that measurement: the corpus cannot be ingested if the detector is right about those three credentials, and a retrieval benchmark that refuses its dataset measures nothing. It is a named function rather than a flag, it is not on `provider.MemoryStore`, it is not in the MCP surface, and its only callers are the three `bench/` dataset seeders. The claim justifying it is deliberately the narrow one: the row lands in a scratch database that dies with the run, and is never injected into a session's context, never mirrored to the vault, and never quoted into a harness prompt. It is **not** "never embedded" — the two seeders that run a non-fts condition embed every ingested turn and send it to Ollama — and the narrow claim is the one quoted because the wide one is what a future author would cite to widen the carve-out to a bench run against a real store, where the vault and reflect exposures are real. `RestoreSnapshot` and `SeedGlobalMemories` are outside the guard — they write byte-exact data into a restored store, and they are outside the `MaxContentLen` contract for the same reason; a restore that silently dropped rows would be worse than the leak, because a restore is a user's own database coming back. `ReplaceNonManual` is outside it because filtering there would be actively destructive: it deletes every replaceable row the snapshot does not account for, so dropping a credential-shaped memory from the emitted set deletes the stored row, and an unattended nightly reflect would silently remove a credential from a pre-guard database with no report and no way to recover it. That is the same reason the reflection drop guard requires an explicit `--allow-drops` before deleting anything. `applyReflection` drops a credential-shaped proposal before the store sees it — after the audit, for the reason above — and clearing a credential out of an existing database remains a separate report-first scan rather than a side effect of consolidation. Both exclusions are exported on `provider.MemoryStore`, and the honest form of the claim is that each has exactly one production caller and each of those filters upstream.

## Memory axes

A memory is described along four independent axes. The axes are orthogonal: a row can be live, in-date, contradicted, and low-confidence at the same time, and each of those facts is stored and judged separately. This section is the normative definition of the axes; the gaps listed against each one are tracked as issues and are the plan in [`ROADMAP.md`](ROADMAP.md#part-9--architecture-direction-memory-axes-and-context-assembly-p0p3).

| Axis | Question it answers | Storage today | Status |
|---|---|---|---|
| **Lifecycle** | Is this memory still current, and what replaced it? | `memories.resolved_at`, `memories.pinned`, the relation CHECK in `internal/memory/schema.go` (`duplicate`, `contradicts`, `supersedes`, `elaborates`, `causes`), `memory_snapshots`, `memory_history`, `audit_log` | Partial — `memory_history` now records every state change and who made it, but nothing reads it during retrieval; there are still no retention/ownership tiers ([#587](https://github.com/wcatz/ghost/issues/587)) and no documented transition model ([#579](https://github.com/wcatz/ghost/issues/579)) |
| **Validity** | Is this memory true *now*, and when was it last checked? | `memories.valid_from`, `valid_until`, `verified_at` | Partial — the columns are read into `memory.Memory` and stage 2 of the assembler evaluates them against the request clock, so a row with a closed window is withheld rather than ranked ([#581](https://github.com/wcatz/ghost/issues/581)). No MCP writer exists yet (PR 3), so a store nobody has restored or imported reads every row as unset and the evaluation is corpus-neutral; `Store.RestoreSnapshot` and `Store.ImportMemory` both carry the triple, which is where a non-NULL window first comes from ([#575](https://github.com/wcatz/ghost/issues/575)) |
| **Relationships** | What does this memory connect to, contradict, or replace? | `memory_links` (directed), near-duplicate links created by `Upsert`, scope-conflict exemption ([#563](https://github.com/wcatz/ghost/pull/563)) | Every writer refuses a scope-conflicting pair and every reader ignores one already stored ([#563](https://github.com/wcatz/ghost/pull/563), [#574](https://github.com/wcatz/ghost/issues/574)). Writers: `Upsert`'s two `duplicate` dedup probes (at save time), the linker's `related` edges, and `ghost supersede`'s `supersedes`/`causes` candidates. Readers: `DemotionPenalties` and `SupersedePenalties` (ranking), `ghost resolve`'s supersedes piggyback and the repair pass's matching floor (which would otherwise stamp `resolved_at` on the older endpoint), and the two fold-target liveness checks that decide whether a row may be folded into (which would otherwise turn every re-save of that row into a duplicate) |
| **Confidence** | How much should a caller trust this, and why is it here? | `memories.confidence` plus write-time provenance columns `agent`, `session_id`, `source_ref`; `memory_history` records who performed each write | Inert — `confidence` is written on some paths and never read by ranking ([#575](https://github.com/wcatz/ghost/issues/575)), and the history has no reader yet: `ghost history <memory-id>` and `Store.MemoryHistory` are the only two, and no retrieval path consults them |

Axis interaction rules:
- **Supersede wins over time.** A memory that has a live replacement is demoted regardless of a later `verified_at` or higher confidence on the old row.
- **Resolved leaves injection, not the database.** `resolved_at` removes a row from ranked session injection ([#559](https://github.com/wcatz/ghost/issues/559)) but keeps it searchable and auditable.
- **Contradiction is symmetric, duplicate is directional.** A `contradicts` pair must never appear together in one assembled block; a `duplicate` edge points at the row that survives, and folding must not cross a scope conflict ([#574](https://github.com/wcatz/ghost/issues/574)). Not yet enforced: stage 5 of the assembler *records* a co-occurring `contradicts` pair and leaves both rows in place, because the existing contract requires a contradicted row to survive while a duplicate restatement sinks. Separating the pair needs its own contract change ([#581](https://github.com/wcatz/ghost/issues/581)).
- **A scope conflict blocks the relation, and is not repaired by deleting it.** Two memories naming `environment=production` and `environment=development` are two claims about two places, so no relation may be proposed or created between them — not `duplicate` (at save time), not `related` (at link time), not `supersedes` (at supersede time), because each of those writers is the only one that can see both scopes at the moment it decides. An edge that already exists stays in the graph and is exempt at read time instead: every reader ignores it, and none deletes a row to reach that verdict. The exemption is stated where the decision is made, and every writer that chooses between candidates states it *inside* the query, because the same `LIMIT` chooses them: a conflict decided after the cut spends the budget on rows the caller may not use and misses a compatible candidate ranked just below ([#665](https://github.com/wcatz/ghost/issues/665)). The two cosine writers narrow before their window (`SearchVectorScoped`, in Go over the scan rather than inside a statement, so its top-k is the limit and there is no statement to fold the rule into), so a neighbour budget counts only rows a memory may relate to. `Upsert`'s two dedup probes and `foldTargetStillLive` each carry a second, SQL statement of the rule (`scopesConflictSQL`, held to the Go one by a test that runs both over the same table), for two different reasons: the probes because their own `LIMIT 15` chooses the candidates — so a save whose fifteen best FTS matches all name another environment still finds the compatible duplicate at rank 16 — and `foldTargetStillLive` because it has no window to protect, names one row by id, and carries the rule to keep a scope-conflicting `supersedes` edge from being read as a verdict on it.
- **Scope and project membership are not axes.** They are access predicates applied before scoring ([#577](https://github.com/wcatz/ghost/issues/577)); a memory that fails them is out of scope regardless of its other axes. The rule is decided where the rows are read, in whichever form that read allows. Search applies it in Go over the widened pool `Store.Candidates` returns — both legs have already run by then, and fusion narrows the fused pool, so a SQL form there would narrow nothing it still had to decide about — and the assembler keeps it in Go over the same set. A reader whose candidate set *is* a statement's own `LIMIT` has no rows left to decide over once the cut is taken, and no corpus scan to decide them with, so it carries a SQL statement instead: `memory.ScopeMatchesSQL`, in the two session-start loaders. A test runs that form and the Go one over the same rows, and a second test pins the single divergence between them — a stored scope value that is not a string. The cosine writers in the bullet above are that difference made concrete: a brute-force pass over the corpus can decide before its own top-k, in Go, and backfill to fill it.
- **Who wrote it is not how much to trust it.** `agent`/`session_id`/`source_ref` describe who wrote a row, and `memory_history` ([#578](https://github.com/wcatz/ghost/issues/578)) records who performed each write since — but neither is a ranking input, and a confidence value is not a verdict anything computes. (Write-time authorship and evidence provenance are different things; see [Memory history](#memory-history).)

## Context assembly (target design)

> **Partly built.** The seam exists (`internal/assemble`, `assemble.Run`, and
> `Store.Candidates` behind it), the formatted `ghost_memory_search` path runs on
> it with both filters applied before the window closes, and the session-start
> surface renders and applies `memories.scope` from the shared label and the
> shared rule ([#577](https://github.com/wcatz/ghost/issues/577)). What does not
> exist yet: the session-start injector still runs its own ad-hoc pipeline rather
> than `assemble.Run` (passive retrieval is not served by the seam yet), the
> conflict, dedup, diversity and budget stages are pass-throughs, abstention is
> not derived
> ([#580](https://github.com/wcatz/ghost/issues/580)), and `explain: true` still
> calls the store's own diagnosis rather than projecting the assembler's trace
> ([#583](https://github.com/wcatz/ghost/issues/583), [#571](https://github.com/wcatz/ghost/issues/571)). The plan to converge the surfaces is [#581](https://github.com/wcatz/ghost/issues/581), staged in
> [`2026-09-25-context-assembler-design.md`](superpowers/specs/2026-09-25-context-assembler-design.md).

What exists now:

- **`internal/assemble`** owns selection, validity, predicates, provenance, the
  stage list, budget closure, the outcome and the shared item renderer. It
  depends on one method, `Candidates(context.Context, memory.CandidateRequest)`,
  never on `*sql.DB`. `internal/memory` never imports it; the retriever DTOs
  live in `internal/memory` because a method on `*memory.Store` cannot name a
  type from a package that imports it.
- **`Store.Candidates`** returns a *widened, untrimmed* set: the window selected
  exactly as production search selects it (fusion, status demotion, the keyword
  reservation, hydration with its deleted-row backfill, decay over the window,
  then the supersede and near-duplicate demotions), followed by the rows that
  window cut, in the same decay order. With no predicate, closing the set to the
  window reproduces the production search row for row; with a predicate, rows
  beyond the window are reachable. Re-deriving the window's decisions from the
  wider pool would drop the reservation and widen the demotions, so it is not
  done.
- **One snapshot per retrieval.** Legs, hydration, the edge load and the penalty
  lookups run in one read transaction on an injected read-only handle
  (`memory.OpenReadDB` + `memory.NewStoreWithRead`), whose DSN has no
  `_txlock`, so the transaction is a plain deferred read and does not take the
  write lock the primary handle's `BEGIN IMMEDIATE` would. A store with no read
  handle falls back to the primary connection and logs that cost once.
  `OpenReadDB` refuses `:memory:`; in-memory and bench stores run the snapshot on
  the handle they already hold, which has no concurrent writer.
- **Stage 3 is where category and scope verdicts live**, and both run over the
  widened set. That is [#573](https://github.com/wcatz/ghost/issues/573)'s
  mechanism closed: a post-filter over a closed window can only remove from the
  answer, so a matching row the window cut was invisible and the tool reported
  absence while the memory existed. Project membership stays in SQL and is
  recorded as a per-row verdict, never applied as a second drop.
- **The three validity columns are readable.** `Memory` now carries
  `valid_from`, `valid_until` and `verified_at` as `*string`, bound on every
  retrieval scan, and stage 2 interprets them against the request clock
  (`valid`, `future`, `expired`, `unverified`, `unset`; an unreadable value is
  reported as `validity_unparseable` rather than read as valid). No MCP writer
  exists yet, so a store nobody has restored or imported reads every row as
  unset and stage 2 is corpus-neutral; `Store.RestoreSnapshot` and `Store.ImportMemory`
  do write the triple, which is where a non-NULL window first comes from. The
  writer contract is the next change.
- **The session-start surface shows and applies scope.** The two session-start
  loaders select `memories.scope`, print it with `assemble.ScopeLabel` — the same
  label a search line carries, so the two cannot spell one scope differently —
  and narrow their own fetch when `injection.session_scope` is set
  ([#577](https://github.com/wcatz/ghost/issues/577)). The filter is
  `memory.ScopeMatchesSQL`, the SQL statement of the rule stage 3 applies in Go
  through `assemble.ScopeContradicts`, and it is held to the Go form by a test
  that runs both over the same rows. It has to be decided in SQL for the reason
  `scopesConflictSQL` gives: a statement that chooses its own candidate set with a
  `LIMIT` cannot check scope after that cut. Both loaders are that statement — the
  45-row and 16-row over-fetches are their candidate sets — so a check applied
  afterwards would spend the budget on rows the session excluded and never reach
  an eligible one ranked below the cut. With the key unset the clause is absent,
  so the query, the ranking and the selection are the ones that shipped — the
  label is not part of that, and is deliberately new: a row that carries scope is
  labelled on its line whether or not a session scope is configured, which is
  half of what [#577](https://github.com/wcatz/ghost/issues/577) asked for. The
  column itself is read only from a store at or past the version that added it
  (`migrateV12`), because these loaders run on a read-only handle that migrates
  nothing: naming `memories.scope` on a store below that version fails the query
  with "no such column", which a loader reads as no rows — a digest with its
  header, its tasks and its decisions and no memories, and nothing saying why. A
  store below the floor selects the column as a NULL literal instead, which is what
  every row in it carries by definition, so the block is the one that store
  produced before scope was read. The loaders are callers of the assembler's label
  and rule, not of `Run`: passive retrieval is not served by the seam, so moving
  the digest onto it is its own change.
- **The trace is recorded unconditionally**, with per-stage counts, dropped ids
  and per-row decisions. `explain: true` does not read it yet.

What the remaining stages will add, in pipeline order: conflict recording and
dedup reordering (stage 5-6, where `contradicts` is recorded and not acted on),
diversity (7, off by default), the budget byte caps and the `response_fit`
post-pass (8), and abstention as an outcome (Decision 3,
[#580](https://github.com/wcatz/ghost/issues/580)).

Both consumers should call one assembler with an explicit budget, so every surface applies the same predicates in the same order and every stage is testable in isolation:

```text
query
  1. retrieve       hybrid FTS + vector candidates, widened window (0.3 FTS / 0.7 vector RRF)
  2. validity       drop or bound rows outside valid_from/valid_until, flag unverified
  3. scope          machine-readable memories.scope match, project membership
  4. provenance     bounded penalty for unattributed or low-confidence rows
  5. conflicts      suppress superseded rows; never emit a contradicts pair together
  6. dedup          collapse duplicate/near-duplicate links to one representative
  7. diversity      cap per-source share so one project cannot crowd out the rest
  8. budget         final ordering, then a hard byte/token trim
  9. render         one renderer shared by search output and injected context
       → Trace      per-stage row counts and per-row exclusion reasons
```

Rules the pipeline must hold:

- **Filters precede window closure.** Stages 2-4 run over the widened candidate set from stage 1, never over an already-truncated list.
- **One renderer, one field set.** Scope, validity state, and confidence appear identically in `ghost_memory_search` output and in the injected session-start block. Scope does today: both surfaces print the label from `assemble.ScopeLabel`, and validity state and confidence arrive with the writer that can set them.
- **The trace is the explain payload.** `explain:true` ([#583](https://github.com/wcatz/ghost/issues/583)) reports the stages above, so explain and ranking cannot disagree.
- **Abstention is an outcome.** If no row clears the relevance floor, the assembler returns `weak` or `empty` with a reason rather than passing stale candidates through ([#580](https://github.com/wcatz/ghost/issues/580)).
- **The budget is a hard boundary.** Stage 8 trims deterministically and is tested at, just under, and just over the limit; injection and search use different budgets but the same code.
- **The pipeline is measurable.** Bench gains context precision, contamination rate, budget adherence, diversity, and token cost ([#582](https://github.com/wcatz/ghost/issues/582)), and contamination classification reuses the production exclusion reasons so the two cannot drift.

## Concurrency contract

**Multiple Ghost processes may open the same database concurrently, and SQLite is the synchronization layer.** This is a supported mode, not an accident: a CLI command, a live MCP server, a hook-spawned lifecycle subprocess, and a maintenance run routinely overlap.

Ghost does not run a single owning daemon that other commands route through. Each process opens its own handle and relies on the database for isolation.

The guarantees rest on these settings:

| Setting | Where | Why |
|---|---|---|
| `journal_mode(WAL)` | `memory.OpenDB` | Readers never block on a writer for their snapshot, so a hook read cannot be stalled by a reflection write. WAL is persisted in the database file, so it applies to every connection to that file. |
| `busy_timeout(5000)` | `memory.OpenDB`, `mcpinit.rwDSN` | A write arriving mid-contention retries for up to 5 seconds instead of failing on the first collision. Without it, concurrent writes return `SQLITE_BUSY` and the memory is silently lost. This is a bound, not a guarantee: a transaction held longer than 5 seconds still fails the writer with `SQLITE_BUSY`, and callers that treat extended contention as recoverable (`bumpSessionCount`, for one) must handle that error rather than assume the write landed. |
| `busy_timeout(1000)` | `mcpinit.roDSN`, CLI read paths | Read-only connections are not exposed to write-lock contention under WAL, so a short timeout is enough to catch real problems without hanging a hook. |
| `SetMaxOpenConns(1)` | `memory.OpenDB` | Pins each handle to one connection so `PRAGMA data_version` polls compare against a stable baseline. `obsidian sync` uses that counter to detect commits from other processes; an unpinned pool would compare connection-local counters instead of points in database history. |
| `foreign_keys(ON)` | `memory.OpenDB` | Enforces the `memories.project_id` and `memory_links` foreign keys, so a write cannot insert a memory for a project that does not exist or leave a link pointing at a row that is gone. A connection without it accepts both, and the row-level damage is invisible until a later read cascades or a project listing disagrees with the memories attributed to it. |
| no `_txlock` | `memory.OpenReadDB` | The read-only handle exists so a snapshot read can be a plain deferred `BEGIN`. On the primary handle every `BeginTx` is `BEGIN IMMEDIATE`, so a read transaction there would hold the write lock for its whole lifetime — long enough to block every concurrent writer in the machine. |
| `_txlock=immediate` | `memory.OpenDB` | `BeginTx` issues `BEGIN IMMEDIATE`, taking the write lock at transaction start. A deferred transaction that reads first and writes later holds a WAL read snapshot, and the read-to-write upgrade fails with `SQLITE_BUSY_SNAPSHOT` if another process committed in between — an error `busy_timeout` does not retry. Without this, a read-then-write transaction such as `UpdateMemory` fails outright under concurrent handles instead of waiting. |

A read-only connection deliberately sets no `journal_mode`: setting it writes the database header, which a read-only connection cannot do.

### Boundaries

- **Writers are serialized by SQLite, not by Ghost.** There is no application-level writer lock for ordinary memory operations. The per-project lifecycle PID file (`AcquireLifecycleLock`) prevents two *maintenance runs* from overlapping; it does not govern memory reads or writes.
- **A read that decides a write belongs inside the write transaction.** `Upsert` is the worked example: it probes for a duplicate, then strengthens the row it found or inserts a new one. Probed outside the transaction, the gap between probe and write is a window any other handle can use — a save that strengthened the same row in that window had its increment overwritten by arithmetic on a value read before the fact, and a row committed in that window was invisible to the probe, so two saves of one fact became two unlinked rows. Both are cross-process races that a per-`Store` mutex cannot close. The transaction is therefore opened before the probe and `_txlock=immediate` holds the write lock across it, and the strengthen is `SET importance = MIN(1.0, importance + ?)` so the value it adds to is the one the row holds when the statement runs. A read-modify-write outside a transaction is the same bug with a different statement.
- **A handle is one connection.** Process-level concurrency is the number of open handles, not the number of goroutines. Goroutines within one process contend with each other for that single connection.
- **Holding a pinned connection blocks the pool.** Code that pins `db.Conn(ctx)` must not then issue a `db.*` call on the same handle: with `MaxOpenConns(1)` that call waits for a connection only the pinning code can release, and blocks forever.
- **This contract is solo mode.** It bounds one machine and one database file. A networked multi-writer backend is a separate deployment mode, not a relaxation of these settings; see `ROADMAP.md`.

### Tests

`TestConcurrentProcessesMixedReadWrite` opens several handles against one file and runs two phases against each other: concurrent inserts with FTS readers, then concurrent content rewrites with FTS readers. It asserts no `SQLITE_BUSY` failures, no dropped writes, and no row returned by a search whose stored content does not contain the searched terms. The rewrite phase exists because `memories_au`, the trigger keeping the index in step with content, fires only `WHEN old.content != new.content` — inserts alone never exercise it. `TestOpenDBPinsPoolToSingleConnection` pins the pool setting directly. Both are contract guards: they pass while the contract holds and fail if a setting that provides it is removed.

Every handle in that test comes from the same binary and the same DSN builder, so it cannot see a setting that only one open path carries. `TestMultiProcessSharedDatabase` closes that gap with real processes: it builds a helper program (`internal/memory/testdata/multiproc`) once and runs the entry points the contract names against one database file — two MCP servers driving the real `ghost_memory_save` and `ghost_memory_search` over the real stdio transport, two CLI children doing `Upsert` and `UpdateMemory`, two read-only handles searching the rows those children are rewriting, and one lifecycle writer applying a single `ApplyReflection` batch plus a `maintenance_runs` row. Ten processes, ten handles, one file.

What it asserts, beyond "nothing crashed":

- every process exits successfully and no `SQLITE_BUSY` or `SQLITE_BUSY_SNAPSHOT` reaches any of them. Nothing in the test retries a database error, so a pass is evidence the 5s busy timeout carried the contention on its own rather than that a backoff hid it. What keeps that from becoming a measurement of how long a transaction may be held is the trigger described below, not a cap on the writers: the batch comes as soon as every writer has made its share of writes, so the corpus is still small when it arrives, and a run in which something *else* decided the timing would be the one that grew it.
- the load was still writing when the batch committed. Two barriers say so. The batch waits for every writer to report that it has completed its share of writes, so it is triggered by writer progress rather than by a clock; then it announces itself and waits for every writer to report a write issued *after* that announcement, which it cannot do until each has come back around its loop. The batch therefore cannot commit until every writer has written across the announcement. The measurement has to be a file: the batch holds SQLite's write lock for its whole transaction, so a writer cannot commit *during* it, a row count taken either side would measure the two gaps around the lock, and a writer testing a flag on both sides of its own write cannot tell a write that straddled the instant from one that ran entirely after it. Without this the run could pass with the batch landing on an idle file, which is the vacuous case the guard exists to prevent.
- every row id a process reported creating is present afterwards, read back through a fresh handle. A writer that gave up under contention is a lost memory.
- a read transaction opened before the batch commits still describes the pre-batch state afterwards, and a new snapshot then sees the whole batch. The transaction is opened the way `ExplainSearchScoped` opens its diagnostic snapshot — `BeginTx` with `ReadOnly`, which issues a plain `BEGIN` even under `_txlock=immediate` and so pins a WAL read snapshot without taking the write lock.
- a second reader, taking a fresh snapshot on every sample straight across the commit, observes the pre-batch state and the post-batch state and nothing between them. A pinned snapshot cannot show this — it shows the pre-batch state whatever the writer did — which is why it takes two readers. It announces its first sample as a barrier, so the batch cannot commit before a pre-commit reading exists, and its tail is counted from that signal rather than from its first sample, because a read that begins while the writer is still finishing its commit can legitimately describe the pre-batch snapshot.
- no reader sees a row whose content does not contain a term its FTS match claimed, in the CLI and read-only readers, which hold `[]memory.Memory` and check the terms. The MCP readers are not part of this: `ghost_memory_search` returns formatted text, so a row rewritten between its FTS match and the read that hydrates the result is indistinguishable from a stale index entry, and a search is two queries. Their reads establish that the tools answer under contention.
- `journal_mode`, `foreign_keys`, `busy_timeout`, `SetMaxOpenConns(1)` and a read-only handle that refuses writes are read back from a live connection rather than from the DSN that produced it. The fleet exercises two of the tree's DSN builders: `memory.OpenDB` (the six read-write roles) and `memory.readOnlyDSN` (the four read-only ones), with `busy_timeout` 5000 and 1000 respectively. `foreign_keys` is asserted on the read-write shape only, because that is the shape whose DSN sets it; the contract table above now says so too. It opens neither `mcpinit.rwDSN` nor `mcpinit.roDSN`, nor `cmd/ghost`'s own read-only spelling, so a regression in one of those would still pass this test. `_txlock=immediate` cannot be read back at all — it is a driver parameter — so it is asserted behaviourally, by showing that a write from a second connection is refused while a read-then-write transaction is open on the first, which is what a deferred `BEGIN` would not do.
- `user_version` is unchanged and `PRAGMA integrity_check` is `ok`. All six read-write processes ran `OpenDB`'s open path concurrently, which is `initSQL`'s `CREATE ... IF NOT EXISTS` pass plus its `CREATE UNIQUE INDEX IF NOT EXISTS`; `migrate()` itself is not entered, because the parent creates and stamps the database at the current schema and `migrate` only runs when the file is behind. The four read-only processes opened through `OpenDBReadOnly`, which runs no DDL at all.

Ordering between the processes is carried by barrier files, not sleeps: each announces the state it has reached and waits for the state it depends on — a writer announces the writes it has completed, a sampler announces its first reading, the maintenance process announces its commit — so a slow machine makes the run slower rather than wrong. The test skips under `-short` (it builds a binary and starts ten processes) and makes no LLM call, so it needs no live-test gate. What it does not cover: a schema-changing migration committing against live readers. `migrate()` is not entered at all here — see the `user_version` bullet — so a real ALTER TABLE racing the load and the readers is untested.

## Backup and portable transfer

Two independent mechanisms move the store out of a machine, and they answer different questions.

**A backup is a restorable copy of the database.** `memory.Store.Backup` (`internal/memory/backup.go`) writes one with SQLite's `VACUUM INTO`, which reads a single snapshot of the source and builds the destination from it. That is what makes it correct against a live WAL database, where copying files cannot be: a writer committing during the copy is reflected in the result whole or not at all, where a `cp` of `ghost.db` alone loses whatever the `-wal` held and a `cp` of the pair can capture a torn page. It is also why no extra transaction wraps it — the write lock is held for the length of the vacuum and nothing longer, so a running MCP server keeps serving.

`memory.vacuumInto` is the single implementation of "put a restorable copy of this database over there", and both callers use it: the pre-migration copy `OpenDB` takes before any migration step, and `ghost backup`. One implementation means the refusal to replace an existing file, the mode the copy is created at, and the `TightenPermissions` pass cannot drift apart between the two — the pre-migration copy is the same width as the database it came from, and so is a `--out` destination outside the data directory, which has no 0700 parent to shield it.

The copy is created at 0600 *before* the vacuum rather than chmod'ed after it, and the path is claimed with `O_EXCL`. `VACUUM INTO` names no mode for the file it creates, so a copy it creates lands at SQLite's default minus the umask (measured 0644 under umask 000) and a create-then-chmod leaves a window in which a full copy of the memory database is group- and world-readable. `O_EXCL` is also the atomic claim, so there is no gap between the `Lstat` that classifies the destination and the create that takes it, and a dangling symlink at that path is refused rather than written through. SQLite accepts an existing *empty* file as a `VACUUM INTO` destination and keeps its mode, and refuses a non-empty one — so the emptiness `reserveBackupPath` guarantees is the emptiness SQLite checks. The `TightenPermissions` call after the vacuum is a second line rather than the only one: the open mode is advisory, and setgid directories, ACLs and some network mounts can leave a file wider than the mode asked for.

`BackupResult.Counts` is counted by opening the *written* file read-only, never the live one. The counts a restore is checked against have to describe the file that was written: a writer that committed after the vacuum would otherwise make the printed numbers and the snapshot's contents disagree, and the reader has no way to tell which is right.

**An export is an inspectable artifact.** `internal/portable` writes the store as JSON Lines — one self-describing object per line behind a schema-version header — and reads one back. It is a wire format with its own version, independent of `schemaVersion`: an artifact is a file that outlives the ghost that wrote it, so its meaning is the shape of the records, not the tables behind them. Ordering is fixed (projects, then memories, tasks and decisions, each by id) and the header carries no timestamp, so two exports of an unchanged store are byte-identical and a diff of two artifacts shows only what changed in the store. The line-oriented shape is also why a damaged file is survivable: a line that is not JSON is rejected on its own, by line number, and the records around it still import — which is what the reader gets from a truncated or hand-mangled artifact. Whole-file refusals are reserved for the problems that are not one line's: a missing or unreadable schema version, a second header, an unknown record type. Those mean the file is not this format, or not one this build can interpret, and applying part of it would be importing data whose meaning is a guess.

The reads and writes are separate store methods rather than reuse, and each for a stated reason. `PortableProjects` exists because `ListProjects` has no `repo_remote` column — the field that makes two checkouts of one repository one project, and that another machine needs to resolve a project from its own directory. `PortableMemories` exists because the ordinary list readers drop the validity triple, and restoring a memory with a fresh `created_at` would age it out of injection immediately. The `Import*` methods exist because no existing writer can preserve an imported id, an imported `created_at` or an imported pin: `Create` generates its own id and `Upsert` is deliberately a dedup probe, which is the wrong operation for a restore. Each takes an `apply` flag, so a dry run runs the same validation as the run it previews rather than a second code path that can disagree with it.

Four things are deliberately outside the artifact, and the first three are there for one shared reason — they are derived rather than stored knowledge. `memory_embeddings` and `memory_links` are rebuilt by the embedding and linking workers, and `memories.resolve_kept_hash` is recomputed by the resolve pass; shipping them would mean shipping a model's output as if it were the memory, importing edges ahead of their endpoints, and marking a memory the classifier has never seen as reviewed.

The fourth is Ghost's own `_global` `builtin` seeds, and they are excluded because no import could ever reconcile them. `SeedGlobalMemories` writes each seed under the schema's default `hex(randomblob(16))` id, which is per-install, so an artifact's copy never equals the destination's own — and every importer dedups by id alone. Importing one machine's artifact into a store Ghost had already bootstrapped therefore inserted a second row with byte-identical content: a second pinned `builtin` copy of a shipped rule, or, after the default provenance downgrade, an `onboarding` copy that has silently lost the "this is Ghost's own" label. Nothing repairs it, because re-seeding skips by content and no path deletes memories.

The exclusion is at the export read rather than in the import: it keeps the import rule simple, and the destination already re-creates exactly those rows by content on every open, so a restored store ends up with one copy — written by Ghost, not two. It is on the seed (`project_id` **and** `source`) rather than on the project, because a user's own memory filed under `_global` is the only copy of itself and has no other way in.

Provenance is a per-run policy, not a per-record one, and it lives in `memory.ImportOptions` because the store is what enforces it. A dry run opens the database read-only, so the preview cannot migrate a store whose schema is behind or seed the builtin rows while reporting "nothing written" — the import's own writes all return before their INSERT when `apply` is false, so the read-only connection loses no capability the preview needs.

Read-only therefore carries two obligations the read-write path does not, and both are in `openReadOnlyTransferStore` because it is the only read-only open the transfer commands use. It must not create the database, and it must not read a schema it cannot query: `PRAGMA user_version` is read off the open connection through `memory.DBUserVersion` and compared with `memory.SchemaVersion()`, and a store behind or ahead is refused before the first query. Without that check the failure is SQLite's `no such column: repo_remote`, which names a column rather than the version mismatch and lands on exactly the machine the docs tell a user to prepare. The check reads and never writes, so a version check cannot itself become the migration it is reporting the absence of.

Strict on both sides — equal and only equal passes — which is stricter than the failure it replaces needs to be. The only post-v10 columns these readers select are `projects.repo_remote` (v11) and `memories.scope` (v12), so a v12-v16 store is perfectly queryable and a floor would have kept `ghost export` working on one. A floor is not chosen because it hardcodes an assumption about which columns exist at which version, nothing enforces that assumption, and the day a reader selects a column added in a later migration the floor admits a store that fails with `no such column` again — with nothing in the suite asserting about the old versions to notice. Equality is self-maintaining: the store is refused unless it provably carries every column this build selects. The cost is one read-write open for a v12-v16 user before an export works. It can be `ghost mcp init`, any session, or `ghost backup` — and `ghost backup` is reached through `bootstrap()` itself, so it migrates and seeds the store it copies, and `OpenDB` refuses a store from a newer Ghost exactly as it refuses one to any read-write opener. It runs no version check of its own, but that makes it a way to *pay* this cost rather than a way around it: there is no read-only way in this build to get a copy, and describing `ghost backup` as one would tell a user that a command which writes has read their store. Read-write is kept for it deliberately, because a store behind the current schema is exactly the store a user most wants a restorable copy of, and a read-only open would refuse it. `openReadOnlyTransferStoreUnchecked` exists only so a test can prove the strictness is about the version: it opens a behind store, shows it queries fine, and shows the checked opener refusing it anyway.

Import never overwrites a record whose id already exists, and that is the property the rest of its behaviour rests on. The artifact is the older of the two copies by construction, so an overwrite would restore stale data over live data — and making re-running a no-op is what lets a rejected record be repaired by fixing the file and running the import again. A record that cannot be imported is rejected individually and the run continues, because one hand-edited line must not abandon ten thousand good ones; the rejections are counted and the command exits non-zero, so a partial import is never reported as a complete one.

Three decisions belong to the caller rather than to the store, and they are the same shape: the store owns what a row may be, the importer owns what a run means.

The first is provenance. An artifact is a file that arrived from somewhere, and on its own authority it would be able to plant rows that read as the user's own words (`manual`) or as Ghost's shipped rules (`builtin`) — both excluded from consolidation by name — or a pinned row, which is excluded whatever its source. So `ImportOptions.TrustProvenance` is off by default and every imported memory is stamped `onboarding` (the source `internal/claudeimport` already uses for memories brought in from outside Ghost) and unpinned; the flag restores the artifact's own values for a user restoring their own database. That asymmetry is the deliberate part: the cost of the default being wrong is planted provenance, and the cost of the flag being wrong is a user passing it once.

The second is project identity. Project ids are per-install, so an artifact names a project the destination has never seen while the destination usually has its own for the same checkout or repository — and `projects.path` and `repo_remote` are both UNIQUE, so inserting the artifact's project would collide, and one collision would take every child record with it because they all name the artifact's id. `resolveProjectMapping` therefore maps each artifact project onto the project that already holds the checkout before any record runs, and the store's own `projectCollision` check catches whatever survives that as a sentence naming the project it collides with, in the dry run as well as the apply. The third is which fields fall back to a column default. A record that states no `created_at` must not be stored with the empty string, because `julianday('')` is NULL and NULL sorts the row out of every ranked read; a record that states no `importance` must not be stored as 0, because `Store.Create` binds a `Memory` saved without one as 0 and an artifact carrying `"importance":0` would be silently promoted to the column's 0.5 on every re-import. Both fallbacks therefore live in the SQL, where the column default can still apply — a value bound for a column never lets its default — and the importance field is a pointer so that an absent key and a stated 0 stay distinguishable.

## Untrusted parse surfaces

Three packages read text somebody else wrote, and none of that text can be trusted to be well-formed, to be the size it claims, or to be a name. `internal/claudeimport` reads Claude Code's auto-memory files, which a session derived from a checkout wrote; `internal/hostevent` reads a host hook's stdin, including values the host itself copied out of a transcript; `internal/obsidian` reads the store, which a portable artifact can seed with ids, names, tags and bodies Ghost never generated. The invariant is one sentence and it is stated once, in `internal/adversarial`: **a value parsed from an untrusted surface is data** — stored exactly as it was parsed, naming at most one path component inside the directory its surface owns, and never deciding the parse. `internal/adversarial` is test-only support; the fixtures in all three packages drive the same corpus and the same assertions over it, so relaxing the invariant fails all three at once instead of drifting in whichever file was read last. The MCP-side suite (`internal/mcpserver/adversarial_test.go`, #538) proves the same invariant one layer out, over the wire.

The interesting failures were all silent ones, which is why each of these is a fixture rather than a rule in prose:

- **A ceiling that discards instead of refusing.** `parseFrontmatter` scanned front-matter lines with `bufio.Scanner` at its 64 KiB default, and a line past the default stops the scan with an error the loop never read — so a memory file with a long description lost its *type*, and with it its category, while the unparsed fences were stored as the body. A line now gets its own bound (`maxFrontmatterLine`), and a block that could not be read whole is not front matter at all: the file imports as body text, which is the same fallback an unterminated block already got. A file past `maxMemoryFileBytes` is skipped, because a memory this import keeps is capped at 8 KB and a file orders of magnitude past that is not one.
- **A name that decides a path.** A note's filename is `slug(content) + "-" + id fragment + ".md"`, and the id fragment used to be a bare prefix of the record's id. Ghost mints hex ids, so that held for everything the store wrote itself — but `Store.ImportMemory` writes an artifact's ids verbatim, and an id can hold a separator, a NUL or a backslash. A separator put the note outside the `Memories/` subtree prune manages, and the NUL failed the write with `EINVAL`, which failed the *whole* export on every run and every retry, taking every other project's notes with it. `idToken` keeps the prefix when it names a single path component — a dash, a dot or a space is left exactly as it was, so no real note is renamed — and hashes the whole id otherwise, because hashing keeps two different hostile ids apart where replacing the offending bytes would collapse them onto one filename. `folderNames` bounds a project folder for the same reason, since a project name is whatever a caller sent.
- **A reader that does not agree with its writer.** `fm` writes `ghost_id` through `yamlScalar`, which quotes any value a YAML reader would not take as a plain scalar; `hasGhostID` read the line back raw, so it returned the *quoted* text for every id that needed quoting. Prune then looked that quoted text up in a keep-set keyed by the real id, did not find it, and deleted the note it had just written — on every export, for good. Only the double-quoted form `yamlScalar` emits is now unquoted: this function reports which files are Ghost's to prune, so a single-quoted value in a hand-written note must keep reading as its own text.
- **A key that has to be recoverable, chosen so it does not have to be.** The same failure has a second and quieter form, and it is why the keep-set is a set of canonical *filenames* (`keepSet`) rather than a map from `ghost_id` to filename. This is only the retention half of prune's decision and the other half is unchanged: the closed front-matter block is what makes a file Ghost's to touch at all, so a note without one is never removed, whatever it is called. What moved is the second question — *which* note this is — from the id parsed out of the note to the name. `yamlScalar` flattens a tab, a newline and a carriage return to a space, which is what keeps every key on one line, and it is lossy: `tab<TAB>id`, `tab<CR><LF>id` and `tab id` are three records and one `ghost_id` line. Keyed on that line the three notes collide on one key, the other two match nothing, and prune deletes them: the export writes three notes and leaves one, silently, every run. Keyed on the filename there is nothing to collide, because the filename is unique per record. The general rule this follows: a renderer and its reader have to agree on an encoding, and a key that decides a *delete* should be one the system holds rather than one it has to parse back out of a file it wrote for someone else to read.

Two properties are the opposite of a filter, and the fixtures say so explicitly. A planted payload must still be **retrievable**: dropping it would hide the tampering from the user, which is worse than returning it as data, and quotation into the data block happens on the way out (`quoteData`, #538) rather than on the way in. And `hostevent` **fails open** on all of it — oversized, deeply nested, invalid UTF-8, a NUL, an unknown event name — because "allow the stop" is the only response the hook contract emits. A ceiling (`maxPayloadBytes`) was the one thing missing there, because nothing on the path bounded the read: the hook read stdin with an unbounded `io.ReadAll` and `Parse` retains the payload twice. The hook now reads through `hostevent.ReadPayload`, which stops one byte past the ceiling, so the ceiling bounds the allocation as well as the parse.

## Configuration and filesystem layout

`internal/config` loads compiled defaults, `/etc/ghost/config.yaml`, the user config file, and `GHOST_*` environment variables. Commands apply supported flag overrides after loading. The data directory is resolved from `XDG_DATA_HOME` or the user's home directory and contains `ghost.db`.

`memory.TightenPermissions` (`internal/memory/perms_unix.go`, a no-op on Windows) strips the group and other bits from the configured data directory and from `ghost.db`, `ghost.db-wal` and `ghost.db-shm`. The pass is subtractive by construction and cannot widen a mode, it `Lstat`s each path and chmods only regular files so a symlink is skipped rather than followed, and it is scoped to the configured data directory so a database opened in a scratch or eval tree does not have its parent chmod'ed. A chmod that fails logs a warning and does not fail the caller.

There are exactly **two** read-write *open functions* in the tree, and both call it. `memory.OpenDB` creates the database or migrates it and calls the pass once, after its first successful query — the point at which the database and the `-wal` and `-shm` files beside it all exist. `mcpinit.bumpSessionCount` is the session hook's single deliberate write, over its own short-lived `rwDSN` connection, and calls it twice: after its existence check, and again after the write. The second call is not redundant. A clean close deletes the `-wal` and `-shm`, so when no other process holds the database, the hook's own INSERT is what recreates them, at whatever mode SQLite gives a new file, and the counter it just wrote is in them until the deferred `Close`. The first call cannot see files that do not exist yet, and the second cannot run before they do.

Every other open is read-only and deliberately stays that way: a diagnostic must be able to report on a database it cannot modify, and a read-only connection cannot create one. That is `memory.OpenReadDB` — the read-only constructor this tree prefers, which refuses a missing file and an in-memory path — at `cmd/ghost/project.go`'s `openDiagnosticStore`, `cmd/ghost/transfer_store.go`'s `openReadOnlyTransferStore` (and `internal/memory/backup.go`'s `countBackupContents`, which counts a written backup), the MCP server's injected read handle in `bootstrap()`, and the session hook's store and global-memory read in `internal/mcpinit/hook.go`. Two package-local `roDSN` families predate it and are not `OpenReadDB` calls: the one in `internal/mcpinit` (`stophook.go`, `lifecyclelock.go`, `lifecyclemarker.go`) and its separate namesake in `cmd/ghost/obsidian.go`, which opens through `sql.Open` directly. `eval/cycle` is mixed: its `state.go` and `inject.go` open read-write through `OpenDB` and write, while `inject.go` also has one `mode=ro` read — so an eval run tightens the three files in its scratch tree and not the directory holding them, since `isDataDir` does not match a scratch path.

"Read-only" is a property of the open, not of the command, and a read-write open tightens wherever it is called. `cmd/ghost`'s shared `bootstrap()` is the usual route, and it already ran migrations and stamped `user_version` before any command-specific work, so most commands tighten simply by starting up. Three paths open read-write without it, and each was already writing or migrating for its own reasons: `mcpinit.checkStoreHealth` (behind `ghost mcp status`, which must not bootstrap — a status check that created the database would turn the next run's "no Ghost database" line into a healthy one), `runMaintenanceStatus`, and three paths under `ghost hook` / `ghost context`:
- `mcpinit.importMemories`, reached by `finalizePlugin` before any of the session-start gates when a Claude Code plugin install is finalizing for the first time. It opens read-write to import memory files, so that one fire tightens regardless of source or whether it is a subagent.
- `runSessionStart`, which returns early for a subagent, a `resume` and a `compact`, and gates the bump on `projectID != ""` and on the source being empty or `startup` — so a `clear` does not bump either.
- `RenderSessionContext`, which opencode's plugin reaches by spawning `ghost context` at start, because opencode has no context-injection surface of its own — it returns before `runSessionStart` on the `InjectContext` gate, and its plugin spawns the context render instead. It has no source or subagent gate at all, and is gated only on `projectID != ""`.

So a genuine new session in a directory that resolves to a project tightens; on the `ghost hook` path a resumed, cleared or compacted one does not, while on the opencode path any start does. That is the same gate that keeps the session counter honest. Goose's session start tightens nothing at all, for the same `InjectContext` reason. A session stop can reach a read-write open indirectly when the stop hook spawns `ghost lifecycle` for a reflection pass.

That is why the pass is a named exported function rather than a line inside `OpenDB`: a read-only path is not a place to add a side effect to, but a read-write one is, and there are two of them.

The contract guards are `internal/memory/perms_test.go` and `internal/mcpinit/hookperms_test.go` (the latter `!windows`, since the assertions are about POSIX mode bits); see [`configuration.md`](configuration.md#data-directory-permissions) for the user-facing description.

See [`configuration.md`](configuration.md) for the user-facing contract and [`internal/config/config.example.yaml`](../internal/config/config.example.yaml) for the annotated template.

## Build and release

Ghost is built as a static binary with CGO disabled:

```bash
CGO_ENABLED=0 go build -o ghost ./cmd/ghost
```

GoReleaser produces Linux, macOS, and Windows binaries for amd64 and arm64, with checksums. The Docker build uses a Go Alpine builder and an Alpine runtime, also with `CGO_ENABLED=0`. CI runs tests, race tests, vetting, linting, vulnerability scanning, and workflow validation.

## Historical design records

The `docs/superpowers/` tree contains archived specifications, plans, and reports. It explains how the architecture reached its current shape but is not the canonical source for current behavior. Start with this page, the source, and the current user documentation.
