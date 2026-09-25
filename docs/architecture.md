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
ghost mcp status                  Check client and store health
ghost hook <event> --source <host> Normalize a host lifecycle event
ghost reflect <project>           Consolidate memories
ghost resolve <project>           Mark resolved evidence
ghost supersede <project>         Classify replacement relationships
ghost lifecycle <project>         Run the detached maintenance phases
ghost project delete|merge        Manage project records
ghost obsidian export|sync        Mirror the store to Markdown
ghost context                     Render passive session context
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
  codex_client.go                  Codex adapter
  goose_client.go                  Goose adapter
  scratch.go                       Harness scratch/temp handling

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
internal/procstat/                  Cross-platform process liveness/start time
internal/provider/                  MemoryStore and LLMProvider interfaces
internal/reflection/                Tiered memory consolidation
internal/resolve/                   Resolved-evidence classifier and cache
internal/scratch/                   Ghost-owned scratch root cleanup
internal/selfupdate/                Checksum-verified GitHub release updater
internal/supersede/                 Directed supersession relation classifier
```

The database schema is an embedded Go string constant in `internal/memory/schema.go`; it is the single source of truth for the store schema.

## Host integration

`internal/mcpinit` owns the host-specific setup paths:

- Claude Code: MCP registration, permissions, SessionStart/Stop hooks, file-memory migration, and redirects.
- opencode: one lifecycle TypeScript adapter that registers MCP and bridges idle events.
- Codex: `config.toml` registration plus `hooks.json` entries that require user trust.
- Goose: an Agent Plugins package with MCP and Open Plugins hooks.

All hook paths converge on `internal/hostevent`, which parses the versioned event envelope and dispatches normalized events. The `scratch` and `procstat` packages keep harness scratch and detached-process liveness handling separate from host adapters.

If a host reports an unknown source, `internal/ai` does not cascade to a default harness. The caller must provide a source or the operation fails with an actionable error. This prevents an opencode or Claude session from silently using the wrong harness or billing path. OpenCode children receive an explicit `opencode/big-pickle` model unless a per-phase pin or `GHOST_OPENCODE_MODEL` override is supplied; the child config is intentionally scrubbed, so the user's global OpenCode model is not inherited.

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
  → optional detached ghost lifecycle <project>
       → reflect
       → resolve
       → supersede
```

The lifecycle is opt-in. A phase failure is logged and does not prevent later phases from running. The reflect phase can use a source-matched CLI harness or an explicitly selected offline tier; the autonomous path requires a real harness when it is configured to rewrite memories.

## Persistence and search

`internal/memory` owns:

- Project and memory CRUD
- FTS5 indexing and query sanitization
- Optional vector storage and cosine similarity
- Reciprocal Rank Fusion for hybrid results, with the result window chosen by
  `FuseAndSelectWindow` (see [Hybrid fusion and window selection](#hybrid-fusion-and-window-selection))
- Category-aware time-decay ordering
- Pinned and near-duplicate handling
- Directed memory links
- Snapshots, audit history, tasks, decisions, and usage data

### Hybrid fusion and window selection

`FuseAndSelectWindow` (`internal/memory/vector.go`) owns both halves of hybrid
retrieval: it fuses the FTS5 and vector legs into one ranking, and it decides
which memories form the result window. They live in one function because
window selection is not separable from fusion — the rule that keeps a keyword
hit has to be stated in terms of the scores fusion produced.

Fusion is Reciprocal Rank Fusion, weighted 0.3 FTS / 0.7 vector with k=60.
A memory retrieved by both legs accumulates both contributions, so a two-leg
match always outranks a single-leg one.

Window selection reserves real estate for the keyword leg. A plain cut on the
fused score could not admit a keyword-only hit at all: the keyword leg's rank-1
row scores 0.3/61 ≈ 0.0049, while the vector leg's 20th row — still well
inside the fetched window — scores 0.7/80 ≈ 0.0088. A full vector leg
therefore outranked the best keyword match every time, and an exact identifier
match could never reach the results, which is the case FTS exists for.

So the best `limit/5` keyword hits the vector leg did **not** retrieve are
guaranteed a place in the window, evicting the weakest admitted rows for them.
Position is left to the fused score: admission is the defect, and the stronger
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
strongest *in-scope* candidate rather than shortening the result. Category is a
separate tool-level post-filter and therefore uses a wider store fetch. Explain
mode calls the same scoped selection entry point, so its included rows and
scope-exclusion reasons describe the store result rather than an unscoped
ranking. Ordering is deterministic (ties broken by ID), because the demotion
penalties applied downstream depend on order.

The main schema tables are:

| Table | Purpose |
|---|---|
| `projects` | Project names, IDs, and paths |
| `memories` | Core memory content, category, importance, tags, and state |
| `memories_fts` | FTS5 virtual table |
| `memory_embeddings` | Float32 embedding vectors |
| `memory_links` | Related, supersedes, causes, and other graph edges |
| `tasks` | Cross-session work items |
| `decisions` | Decisions, rationale, alternatives, and status |
| `ghost_state` | Per-project learned context and interaction state |
| `memory_snapshots` | Reflection rollback snapshots |
| `token_usage` | Reserved schema for future harness usage and cost records; current CLI adapters report zero token counts |
| `audit_log` | Destructive and consolidation operations |

The linking worker skips cosine `related` edges whose endpoint scopes conflict,
and `DemotionPenalties` ignores scope-conflicting `related` and `duplicate` edges
even when a legacy or manual edge already exists. Unscoped or one-sided scopes
remain compatible under `ScopesConflict`. The worker reaches its candidates
through `SearchVectorScoped`, which applies that rule *before* its candidate
limit, so the limit counts neighbours the source may link to. Filtering at the
call site instead — or widening the fetch by a fixed factor — only moves the
cutoff: enough conflicting rows above a compatible one still hide it, and the
source is marked scanned once its sweep succeeds, so it is never reconsidered.

### Retrieval

When embeddings are available, search combines:

- SQLite FTS5 for lexical and exact-identifier matches
- Cosine-similarity vector candidates for paraphrases
- Reciprocal Rank Fusion with the shipped 70% vector / 30% FTS weighting
- Category-aware decay applied to the surviving result window
- Targeted demotion when a present memory is superseded by another present memory

Without Ollama, the same API remains available with FTS5-only results. Search membership is not discarded solely because of age; decay changes ordering.

### Memory lifecycle

`reflect` replaces non-manual memories through a tiered consolidator. It snapshots before replacement, rejects empty results, preserves manual memories, and can restore the latest snapshot. `resolve` stamps resolved evidence so it leaves injection but remains searchable. `supersede` creates directed replacement links after source-matched classification.

## Memory axes

A memory is described along four independent axes. The axes are orthogonal: a row can be live, in-date, contradicted, and low-confidence at the same time, and each of those facts is stored and judged separately. This section is the normative definition of the axes; the gaps listed against each one are tracked as issues and are the plan in [`ROADMAP.md`](ROADMAP.md#part-9--architecture-direction-memory-axes-and-context-assembly-p0p3).

| Axis | Question it answers | Storage today | Status |
|---|---|---|---|
| **Lifecycle** | Is this memory still current, and what replaced it? | `memories.resolved_at`, `memories.pinned`, the relation CHECK in `internal/memory/schema.go` (`duplicate`, `contradicts`, `supersedes`, `elaborates`, `causes`), `memory_snapshots`, `audit_log` | Partial — no retention/ownership tiers ([#587](https://github.com/wcatz/ghost/issues/587)) and no documented transition model ([#579](https://github.com/wcatz/ghost/issues/579)) |
| **Validity** | Is this memory true *now*, and when was it last checked? | `memories.valid_from`, `valid_until`, `verified_at` | Partial — snapshot replacement and restore preserve these fields, but normal reads and ranking do not expose or consult them ([#575](https://github.com/wcatz/ghost/issues/575)); wiring them into retrieval is part of the assembler ([#581](https://github.com/wcatz/ghost/issues/581)) |
| **Relationships** | What does this memory connect to, contradict, or replace? | `memory_links` (directed), near-duplicate links created by `Upsert`, scope-conflict exemption ([#563](https://github.com/wcatz/ghost/pull/563)) | Partial — the linker's `related` edges bypass the scope exemption ([#574](https://github.com/wcatz/ghost/issues/574)) |
| **Confidence** | How much should a caller trust this, and why is it here? | `memories.confidence` plus write-time provenance columns `agent`, `session_id`, `source_ref` | Inert — written on some paths, never read by ranking ([#575](https://github.com/wcatz/ghost/issues/575)); history is missing entirely ([#578](https://github.com/wcatz/ghost/issues/578)) |

Axis interaction rules:

- **Supersede wins over time.** A memory that has a live replacement is demoted regardless of a later `verified_at` or higher confidence on the old row.
- **Resolved leaves injection, not the database.** `resolved_at` removes a row from ranked session injection ([#559](https://github.com/wcatz/ghost/issues/559)) but keeps it searchable and auditable.
- **Contradiction is symmetric, duplicate is directional.** A `contradicts` pair must never appear together in one assembled block; a `duplicate` edge points at the row that survives, and folding must not cross a scope conflict ([#574](https://github.com/wcatz/ghost/issues/574)).
- **Scope and project membership are not axes.** They are access predicates applied before scoring ([#577](https://github.com/wcatz/ghost/issues/577)); a memory that fails them is out of scope regardless of its other axes.
- **Provenance is history, not a weight.** `agent`/`session_id`/`source_ref` describe who wrote a row; append-only history of those changes is [#578](https://github.com/wcatz/ghost/issues/578).

## Context assembly (target design)

> **Target design, not current behavior.** Today there is no assembler: `ghost_memory_search` (`internal/mcpserver`) and the session-start injector (`internal/mcpinit`) each run their own ad-hoc retrieve → filter → rank → trim sequence, which is why the two surfaces disagree about scope ([#577](https://github.com/wcatz/ghost/issues/577)) and why the injector still filters scope after the result window closes. `ghost_memory_search` no longer does: it narrows scope inside hybrid window selection, before the cut ([#573](https://github.com/wcatz/ghost/issues/573)), which leaves category as its only post-filter. The plan to converge the surfaces is [#581](https://github.com/wcatz/ghost/issues/581).

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
- **One renderer, one field set.** Scope, validity state, and confidence appear identically in `ghost_memory_search` output and in the injected session-start block.
- **The trace is the explain payload.** `explain:true` ([#583](https://github.com/wcatz/ghost/issues/583)) reports the stages above, so explain and ranking cannot disagree.
- **Abstention is an outcome.** If no row clears the relevance floor, the assembler returns `weak` or `empty` with a reason rather than passing stale candidates through ([#580](https://github.com/wcatz/ghost/issues/580)).
- **The budget is a hard boundary.** Stage 8 trims deterministically and is tested at, just under, and just over the limit; injection and search use different budgets but the same code.
- **The pipeline is measurable.** Bench gains context precision, contamination rate, budget adherence, diversity, and token cost ([#582](https://github.com/wcatz/ghost/issues/582)), and contamination classification reuses the production exclusion reasons so the two cannot drift.

## Concurrency contract

**Multiple Ghost processes may open the same database concurrently, and SQLite is the synchronization layer.** This is a supported mode, not an accident: a CLI command, a live MCP server, a hook-spawned lifecycle subprocess, and a maintenance run routinely overlap.

Ghost does not run a single owning daemon that other commands route through. Each process opens its own handle and relies on the database for isolation.

The guarantees rest on four settings:

| Setting | Where | Why |
|---|---|---|
| `journal_mode(WAL)` | `memory.OpenDB` | Readers never block on a writer for their snapshot, so a hook read cannot be stalled by a reflection write. WAL is persisted in the database file, so it applies to every connection to that file. |
| `busy_timeout(5000)` | `memory.OpenDB`, `mcpinit.rwDSN` | A write arriving mid-contention retries for up to 5 seconds instead of failing on the first collision. Without it, concurrent writes return `SQLITE_BUSY` and the memory is silently lost. This is a bound, not a guarantee: a transaction held longer than 5 seconds still fails the writer with `SQLITE_BUSY`, and callers that treat extended contention as recoverable (`bumpSessionCount`, for one) must handle that error rather than assume the write landed. |
| `busy_timeout(1000)` | `mcpinit.roDSN`, CLI read paths | Read-only connections are not exposed to write-lock contention under WAL, so a short timeout is enough to catch real problems without hanging a hook. |
| `SetMaxOpenConns(1)` | `memory.OpenDB` | Pins each handle to one connection so `PRAGMA data_version` polls compare against a stable baseline. `obsidian sync` uses that counter to detect commits from other processes; an unpinned pool would compare connection-local counters instead of points in database history. |
| `_txlock=immediate` | `memory.OpenDB` | `BeginTx` issues `BEGIN IMMEDIATE`, taking the write lock at transaction start. A deferred transaction that reads first and writes later holds a WAL read snapshot, and the read-to-write upgrade fails with `SQLITE_BUSY_SNAPSHOT` if another process committed in between — an error `busy_timeout` does not retry. Without this, a read-then-write transaction such as `UpdateMemory` fails outright under concurrent handles instead of waiting. |

A read-only connection deliberately sets no `journal_mode`: setting it writes the database header, which a read-only connection cannot do.

### Boundaries

- **Writers are serialized by SQLite, not by Ghost.** There is no application-level writer lock for ordinary memory operations. The per-project lifecycle PID file (`AcquireLifecycleLock`) prevents two *maintenance runs* from overlapping; it does not govern memory reads or writes.
- **A handle is one connection.** Process-level concurrency is the number of open handles, not the number of goroutines. Goroutines within one process contend with each other for that single connection.
- **Holding a pinned connection blocks the pool.** Code that pins `db.Conn(ctx)` must not then issue a `db.*` call on the same handle: with `MaxOpenConns(1)` that call waits for a connection only the pinning code can release, and blocks forever.
- **This contract is solo mode.** It bounds one machine and one database file. A networked multi-writer backend is a separate deployment mode, not a relaxation of these settings; see `ROADMAP.md`.

### Tests

`TestConcurrentProcessesMixedReadWrite` opens several handles against one file and runs two phases against each other: concurrent inserts with FTS readers, then concurrent content rewrites with FTS readers. It asserts no `SQLITE_BUSY` failures, no dropped writes, and no row returned by a search whose stored content does not contain the searched terms. The rewrite phase exists because `memories_au`, the trigger keeping the index in step with content, fires only `WHEN old.content != new.content` — inserts alone never exercise it. `TestOpenDBPinsPoolToSingleConnection` pins the pool setting directly. Both are contract guards: they pass while the contract holds and fail if a setting that provides it is removed.

## Configuration and filesystem layout

`internal/config` loads compiled defaults, `/etc/ghost/config.yaml`, the user config file, and `GHOST_*` environment variables. Commands apply supported flag overrides after loading. The data directory is resolved from `XDG_DATA_HOME` or the user's home directory and contains `ghost.db`.

See [`configuration.md`](configuration.md) for the user-facing contract and [`internal/config/config.example.yaml`](../internal/config/config.example.yaml) for the annotated template.

## Build and release

Ghost is built as a static binary with CGO disabled:

```bash
CGO_ENABLED=0 go build -o ghost ./cmd/ghost
```

GoReleaser produces Linux, macOS, and Windows binaries for amd64 and arm64, with checksums. The Docker build uses a Go Alpine builder and an Alpine runtime, also with `CGO_ENABLED=0`. CI runs tests, race tests, vetting, linting, vulnerability scanning, and workflow validation.

## Historical design records

The `docs/superpowers/` tree contains archived specifications, plans, and reports. It explains how the architecture reached its current shape but is not the canonical source for current behavior. Start with this page, the source, and the current user documentation.
