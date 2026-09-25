# Context Assembler Design (with Abstention)

> **Status:** Proposed — spec only, no implementation. Closes nothing; it is the
> plan for [#581](https://github.com/wcatz/ghost/issues/581) and
> [#580](https://github.com/wcatz/ghost/issues/580), and the seam that
> [#583](https://github.com/wcatz/ghost/issues/583) and
> [#582](https://github.com/wcatz/ghost/issues/582) build on.
> **Related:** the axes and target-design sections in
> [`architecture.md`](../../architecture.md) as drafted in
> [#589](https://github.com/wcatz/ghost/pull/589) (open; this spec assumes its
> normative vocabulary), and `FuseAndSelectWindow` from
> [#591](https://github.com/wcatz/ghost/pull/591) (open; referenced as merged).

## Problem

Two surfaces consume retrieval, and each runs its own sequence.

```text
ghost_memory_search                    session-start injection
internal/mcpserver/mcpserver.go        internal/mcpinit/hook.go
──────────────────────────────────     ─────────────────────────────────────
584 searchLimit=limit, ×3 if Category  400 sessionMemory{ID,Cat,Content,Pinned}
    ← the only widening                   ← no Scope, no validity
595 if Explain: return                  459 SELECT … WHERE project_id=?
    ← scope never consulted (#571)           AND resolved_at IS NULL
611 SearchHybrid(…, searchLimit)             ORDER BY DecayRankingSQL
623 fullWindow = len == searchLimit         LIMIT 45  ← rank, not relevance
627 post-filter category                491 truncateUTF8(content, 200)
652 post-filter scope  ← unwidened (#573)    ← the budget lives here
671 truncate to limit  ← once, after both  515 load injection.* config
678 empty ⇒ "No matching memories found." 551/584 two-pass selection, then
    ← no outcome, no reason (#580)           609/623 demote, then [:15]
684 formatMemories + incomplete caveat  238 render "- [cat] «content»"
                                            ← scopeLabel absent (#577)
```

Three consequences, each an issue: **the surfaces disagree about what a memory
*is*** (`memory.Memory` carries `Scope`, `Confidence`, `VerifiedAt`;
`sessionMemory` carries four fields, and `formatMemories` prints
`scope{environment=production}` where `formatSessionContext` cannot — #577);
**filters run on the wrong side of the window** (`searchLimit` is only widened
for `category`, so a scoped search can report absence while a match exists —
#573); and **the result carries no verdict** (`""` and `"No matching memories
found."` are the only two non-answer shapes, so a harness cannot tell "nothing
resembles this" from "the best match is weak", and Ghost's only abstention logic
lives in `bench/longmemeval/main.go` — #580).

The root cause is not any one of those defects. It is that no single function
decides *what a context block is*, so every surface re-derives it.

## Decision 1 — Boundary: one new package, `internal/assemble`

`internal/memory` keeps **storage and the retrieval legs**. A new
`internal/assemble` owns **selection, penalties, budget, abstention, the trace,
and the shared item renderer**. `internal/mcpserver` and `internal/mcpinit`
become callers. The layering is one-way — `internal/memory` must not import
`internal/assemble`.

```text
internal/memory    primitives: SearchFTS, SearchVector, FuseAndSelectWindow,
                   DecayFactor, DecayRankingSQL, ScopeMatches,
                   SupersedePenalties, DemotionPenalties, StableDemote, schema
internal/assemble  orchestration: the nine stages, the trace, Item, Budget,
                   the outcome, the shared item renderer
callers            mcpinit (session_start), mcpserver (search, project
                   context, search_all), bench
```

Those primitives are **already exported** — `DemotionPenalties`,
`SupersedePenalties`, `StableDemote`, `DecayFactor` and `DecayRankingSQL` were
deliberately made shareable so injection and search could agree. This design
finishes that move rather than inventing a new axis.

### Alternatives considered

**(a) New `internal/assemble` package — chosen.**

**(b) Grow `internal/memory` into the assembler.** Rejected on three counts.
The renderer cannot live there — `scopeLabel` and `quoteData` are presentation,
and `internal/memory` is the storage layer with a deliberately narrow dependency
set. `store.go` and `vector.go` are named hot files in the team rules, and nine
stages stacked on the retrieval legs maximises merge conflict with the five
other in-flight branches. And trace types as storage-adjacent data with no owner
is exactly how `SearchExplain` ended up re-deriving scoring arithmetic locally
(`explain.go:180-210` re-adds RRF terms the search already computed) — the drift
#583 exists to close.

**(c) A `ContextAssembler` interface in `internal/provider` with swappable
impls.** Rejected: the surfaces need *different configurations* of one
implementation, not different implementations. It puts a one-implementation
interface in front of code that must be fast.

**(d) Name it `internal/context`, as the first draft did — rejected on the
name, not the boundary.** The boundary above is right; the identifier was not.
`package context` shares its name with the standard library's `context`, and
the two collide in a **caller's import block** — which is a compile error, not
a style preference. A minimal reproduction:

```text
import (
    "context"              // stdlib
    "wcatz/ghost/internal/context"
)
```

```text
context redeclared in this block
    "context" redeclared in this block: other declaration of context
    "wcatz/ghost/internal/context" imported and not used
```

So the tax is **guaranteed, not hypothetical**: all three callers — `mcpinit`'s
`hook.go`, `internal/mcpserver` and `internal/bench` — already import `"context"`
and take a `context.Context`, so each needs an alias at its own call sites, and
the alias then has to be carried through every call path that reaches the
assembler. That is a tax on the highest-traffic call sites in the repo, paid to
buy a stutter: `assemble.Run` reads better than `context.Assemble` anyway.

Worth being precise about the scope of the problem, because it is narrower than
it first looks. **The package itself is legal** — a package's own name is not in
file scope, so `package context` importing `"context"` and writing
`ctx context.Context` compiles without an alias. I verified both halves: that
form builds, and the *caller* form above is what fails. So this is not "this
cannot be written". It is that the callers pay, that the failure lands as a
build break in three packages rather than in the one being added, and that every
reader of `context.Context` inside `package context` has to stop and work out
which `context` they are looking at.

**Why `assemble.Run` and not `assemble.Assemble`.** `Assemble` in package
`assemble` is a stutter, and this repo already has a settled convention for the
one exported entry point a package exists to call: `bench.Run`
(`internal/bench/runner.go:45`), `mcpinit.Run` (`internal/mcpinit/init.go:24`),
`resolve.Run` (`internal/resolve/resolve.go:124`), `supersede.Run`
(`internal/supersede/supersede.go:255`). `assemble.Run` joins that set. The
`bench.Run` / `assemble.Run` overlap is not a conflict — bench always qualifies
the call, and the two have different arities, so `assemble.Run(ctx, r, req)`
against `bench.Run(ctx, store, queries)` is unambiguous at every call site.
"Assemble" stays the name of the *process* in prose throughout this document;
only the exported function is `Run`.

### Input / output

```go
package assemble

// Source names the surface asking for a block. It selects the budget and the
// caller's framing text; it never selects which stages run.
type Source string
const (
    SourceSearch       Source = "search"          // ghost_memory_search
    SourceProjectCtx   Source = "project_context" // ghost://project/{id}/context
    SourceSessionStart Source = "session_start"   // SessionStart hook, opencode plugin
    SourceAllProjects  Source = "all_projects"    // ghost_search_all
    SourceBench        Source = "bench"           // internal/bench context mode (#582)
)

type Outcome string
const (
    OutcomeAnswerable Outcome = "answerable"
    OutcomeWeak       Outcome = "weak"
    OutcomeEmpty      Outcome = "empty"
)

// Slice is one bucket's budget. Injection has two — 15 project memories at a
// 200-byte clamp and 8 globals at 300 — and one scalar trio cannot hold both
// policies without silently changing one of them. ClampBytes is the existing
// UTF-8-safe per-item content truncation (presentation); MaxBytes/MaxItems are
// the whole-item hard trim (membership). They are separate mechanisms.
type Slice struct {
    Bucket     string // project id, "_global", or "*" for the fallback slice
    MaxItems   int    // 0 = unbounded by count
    MaxBytes   int    // 0 = unbounded
    ClampBytes int    // 0 = no clamp
    DropDemotedLosers bool // true for _global today; false elsewhere — see §2
}
type Budget []Slice // first matching Bucket wins; "*" is the fallback

// Condition is which retrieval legs run. bench needs all three; zeroing
// FTSWeight is NOT the vector-only case, because fused FTS candidates still
// enter the union with a zero base score (runner.go:46-60).
type Condition string
const (
    CondHybrid   Condition = "hybrid"
    CondFTSOnly  Condition = "fts_only"  // the default: nil QueryVec
    CondVectorOnly Condition = "vector_only" // bench's second ablation
)

type Request struct {
    ProjectID string               // "" for SourceAllProjects AND for a projectless
                                  // session_start — see "Projectless session start"
    Query     string               // "" = passive mode, see §2 stage 1
    Scope     map[string]string    // nil = no scope predicate
    Category  string               // "" = any; stage 3 membership, never post-closure
    Source    Source
    Budget    Budget
    Condition Condition            // which legs run; see §5 and runner.go:46-60
    QueryVec  []float32            // required unless CondFTSOnly
    Explain   bool
    Params    *memory.SearchParams // nil → DefaultSearchParams() with the store's
                                   // configured MinSimilarity re-applied on top
    Now       time.Time            // required, and honoured end to end — see below
}

type Result struct {
    Items   []Item
    Outcome Outcome
    Reason  string   // stable reason code, see §3
    Notes   []string // human-readable caveats (window, vector backend)
    Trace   *Trace   // always recorded; only the projection is gated
    Bytes   int
}
```

`Item` is the assembler's currency — what the renderer prints, what `explain`
reports per row, and what bench scores. It carries the axis fields (#582's
contamination metric is defined over them, §5) *and* every field the existing
renderers print, because a shared renderer that drops one is not shared:
`formatMemories` emits importance and tags today (`mcpserver.go:2156-2180`) and
`Score` cannot stand in for `Importance` — one is a ranking position, the other
a user-set weight.

```go
type Item struct {
    ID, Category, Content string
    Tags                  []string // formatMemories prints these
    Importance            float64  // ditto; NOT interchangeable with Score
    Pinned                bool
    CreatedAt             time.Time // AgeDays and DecayFactor derive from it
    Bytes                 int
    Bucket                string    // project id or "_global" — bench's diversity unit
    ProjectID             string
    Scope                 map[string]string
    ResolvedAt, ValidFrom, ValidUntil, VerifiedAt *time.Time // parsed by stage 2
    Confidence            *float64
    Agent                 string
    Score                 float64   // final, after every scoring stage
}
```

The four timestamps on `Item` are `*time.Time` even though `memory.Memory` will
carry the validity three as `*string`, because `Item` is the assembler's *output*
type rather than a storage type: it holds the parsed form the renderer and the
metrics need. A value that failed to parse is nil on `Item` **and** recorded in
the trace as `validity_unparseable`, so nil unambiguously means "no validity claim
we can read" — which is why the contamination predicate in §5 needs its own nil
check rather than trusting a bare comparison.

### Projectless session start is a real, rendered state

My first draft wrote `ProjectID string // "" only with SourceAllProjects`, which
is false about the product. Session-start **already renders a globals-only block
for an unmatched project**, and it does so through a path that bypasses project
retrieval entirely:

- `loadSessionContext` resolves the project, and on no match returns immediately
  with an empty `projectID` (`hook.go:428-430`) — so no project memories, and no
  `ghost_state` lookups either.
- Globals are loaded on a **separate** path (`loadGlobals`, `hook.go:177`/`:301`)
  that never consults `projectID`.
- The render guard is `if projectID == "" && len(globals) == 0` (`hook.go:303`) —
  projectless **with** globals falls through and calls `formatSessionContext`
  (`hook.go:306`) with an empty project id and a populated global list.

So the existing behaviour is "no project match → still inject your global
preferences", and it is deliberate enough to have its own guard clause rather
than an accident of ordering. Forcing it through a contract that forbids an
empty `ProjectID` would mean either breaking that rendering or keeping a second
retrieval path, both of which defeat the point of the seam. So:

- `ProjectID == ""` is legal for `SourceSessionStart` **and** `SourceAllProjects`.
- For `SourceSessionStart` it selects a **globals-only passive mode**: stage 1
  fetches the `_global` bucket and nothing else, and stages 2–8 run over that
  block normally. `Item.Bucket` is `"_global"` for every row, which is already
  what the diversity metric buckets on (§5).
- For `SourceAllProjects` it keeps its current meaning (no project filter at
  all), so the two are distinguished by `Source`, not by the emptiness of the
  field.
- The invariant that *is* kept: `ProjectID == ""` with a non-empty `Query` is
  rejected, because a query against no project has no meaning the legs can
  express. `Run` returns an error rather than silently degrading to a global-only
  keyword search that the caller did not ask for.

The regression test is the rendered block for a projectless session: project
bucket absent, `_global` bucket present and capped by the existing `globalsCap`,
and no `all_out_of_project` reason — which is what the next subsection needs to
make reachable at all.

### The hook's read-only constraint is load-bearing — but not for the reason I first gave

`internal/mcpinit/hook.go:400-406` says the memory query "deliberately queries its
own lightweight `*sql.DB` connection instead of depending on Store", and the
function opens that handle through `roDSN` (`hook.go:417`).

My first draft justified `Retriever` by claiming the hook has no `Store` at all.
**That was wrong**: `loadSessionContext` builds one at `hook.go:426` and passes it
to `resolveSessionProject`. The real invariant is narrower and better: *the handle
is read-only*, so a `Store` built on it acquires nothing writable. `Retriever` is
still the right shape, for the two reasons that actually hold:

- It keeps `internal/assemble` unable to reach a `*sql.DB` at all, which is what
  makes the one-method interface sufficient (§ the `CandidateSet` edges).
- It does not force the hook to widen its own dependency for a convenience it does
  not need, and it leaves room for a future read-only `memory` constructor without
  the hook having to be the reason one exists.

A follow-up may narrow `resolveSessionProject` to a narrower interface too, but
that is not a prerequisite for this design and is not claimed as one.

```go
// Retriever is what the assembler needs from storage. memory.Store satisfies it;
// memory.NewReadOnly(ro *sql.DB) satisfies it for the hook.
//
// The parameter and result types are memory's, NOT assemble's — see "Where the
// DTOs live" below, which is load-bearing rather than cosmetic.
type Retriever interface {
    Candidates(ctx context.Context, q memory.CandidateRequest) (*memory.CandidateSet, error)
}

func Run(ctx context.Context, r Retriever, req Request) (Result, error)
```

**Where the DTOs live: `internal/memory`, not `internal/assemble`.** My first
draft declared `CandidateRequest`, `CandidateSet`, `Candidate` and `LinkEdge` in
`assemble` and had `*memory.Store` implement the interface. **That cannot
compile.** `assemble` imports `internal/memory` (for `SearchParams`, `Memory`,
`ScopeMatches`, `DecayFactor`), so for `*memory.Store` to implement a method whose
signature mentions an `assemble` type, `internal/memory` would have to import
`internal/assemble` — an import cycle, and a direct violation of the one-way
layering this decision exists to establish. The giveaway is `Candidate` embedding
`memory.Memory`: a `memory` type that embeds another `memory` type is coherent; an
`assemble` type that embeds `memory.Memory` forces `memory` to name `assemble`.

Declaring the four DTOs in `internal/memory` resolves it with no new package:

- `memory` gains no dependency — it only names its own types.
- `assemble` keeps importing `memory` one-way, so the layering holds.
- `Candidate` embeds `Memory` **directly** (not `memory.Memory`), which also
  dissolves the duplicate-field problem the "Candidate validity fields will be
  duplicated" round raised: there is now exactly one home for the validity
  fields, and no shadowing outer copy.
- A third option — a leaf `internal/candidatetype` package importing neither —
  was rejected as a package that exists only to dodge a cycle, adding an import
  to every reader of the DTOs for no behaviour.

The cost is that `memory` exposes a few retrieval-shaped types. That is the
correct home for them: they describe a *read* of the store, and the store is
what produces them.

`CandidateSet` is the widened set stages 2 onward filter over, and it carries
more than rows. Stages 5–6 read link edges while `SupersedePenalties` and
`DemotionPenalties` need a `*sql.DB` the assembler never holds, and today's legs
*discard* individual errors (`SearchHybridParams` swallows a failing leg as
non-fatal, `vector.go:442/454`) — so "both legs failed" and "the vector backend
was never configured" are currently indistinguishable, which is the distinction
#580 asks for:

```go
// package memory
type CandidateSet struct {
    Rows    []Candidate          // hydrated, fused, decay-ordered, untrimmed
    Edges   []LinkEdge           // active edges whose BOTH endpoints are in Rows
    EdgesStatus EdgeStatus       // ok | unavailable | err — see "Edge failures"
    Legs    map[string]LegStatus // "fts","vector": attempted/available/err/truncated
    Widened bool                 // the fetch window was full — see §3
}

type Candidate struct {
    Memory                        // the hydrated row, carrying validity once extended
    FTSRank, VectorRank   int     // -1 when that leg did not retrieve it
    VectorScore           float64 // cosine; -1 when absent
    Base, Decay, Score    float64 // fused base, DecayFactor, base×decay
    AgeDays               float64
}
type LinkEdge struct{ From, To, Relation string; Strength float64 }
type LegStatus struct{ Attempted, Available bool; Err string; Truncated bool }
type EdgeStatus struct{ Status string; Err string } // "ok" | "unavailable" | "err"
```

**One snapshot per `Candidates` call — which means refactoring the legs, not
just wrapping them.** The legs, the hydration and the edge load must run inside a
**single read transaction**, not as today's separate autocommit queries.
`SearchFTS` (`store.go:1159`), `SearchVector` (`vector.go:83`), `GetByIDs`
(`store.go:482`) and the penalty/edge lookups each open their own implicit
transaction today, so a concurrent write landing between them can produce an FTS
rank computed over old content, a body hydrated *after* the write, and an edge
set that lost a cascade-deleted link — three views of one store that disagree.
That is not a theoretical hazard here: the session hook and the MCP server write
through `ghost_memory_save` while a search is in flight. It also corrupts the
trace, which is presented as the *authoritative* record of why a result looks as
it does (#583).

The obvious plan — `BEGIN DEFERRED` and call the existing methods inside it —
**deadlocks, and I nearly specified it.** `memory.OpenDB` sets
`db.SetMaxOpenConns(1)` (`schema.go:310`), so the handle has exactly one
connection. The open transaction holds it, and every `s.db.QueryContext` inside
`SearchFTS`/`SearchVector`/`GetByIDs` then waits for a connection that cannot be
handed out until the transaction closes. That is a self-deadlock, not a slow
path, and no `busy_timeout` covers it because no lock is being waited on — the
pool is.

So the legs have to be refactored to take a queryer and be executed *through* it:

```go
type Queryer interface {
    QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
    QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}
```

`Candidates` opens one `*sql.Tx` and passes the `Tx` to `SearchFTS`,
`SearchVector`, `GetByIDs`, `SupersedePenalties`, `DemotionPenalties` and the new
edge query. That is a mechanical signature change on six functions and is the
real cost of this section — worth stating plainly, because it is a prerequisite
in PR 1, not a refinement of a later one.

Two consequences that follow from `SetMaxOpenConns(1)` and are not optional:

- The transaction must be **read-only for its whole life**. It is `BEGIN DEFERRED`
  and never writes, so no read→write upgrade is ever attempted. That also disposes
  of the caveat I first wrote here: `SQLITE_BUSY_SNAPSHOT` is a *read-to-write
  upgrade* failure — it is returned when a deferred transaction that has already
  read tries to become a writer after another connection committed — so a
  read-only transaction cannot produce it. The real contention risk is plain
  `SQLITE_BUSY` from a concurrent writer, which `busy_timeout=5000` already
  covers and which must stay a non-fatal leg error, never a panic.
- Holding the single connection **serialises every other database user** for the
  duration of the call — including `ghost_memory_save` on another goroutine. That
  is already true of any single query on this handle, but a four-leg transaction
  makes the window wider and the consequence measurable. The transaction must
  therefore stay as short as possible: no LLM calls, no embedding generation, and
  no rendering inside it. Embeddings are fetched by `SearchVector` rather than
  computed, which keeps the embed call outside the transaction by construction.

**Edge failures are non-fatal, and visibly so.** The edge load introduces a
failure path that `CandidateSet` must model rather than hide, because the trace
must never claim stages 5 and 6 ran when they did not. The precedent is
`demoteResults`: on a failed penalty lookup it logs at debug and returns the
results unchanged (`demotion.go:176-178`). `Candidates` follows that, with one
addition the current code cannot make — it is a one-way door, so it has to say
so:

- `EdgesStatus.Status == "ok"` — edges loaded; stages 5 and 6 are authoritative.
- `"err"` — the lookup failed. `Edges` is empty, stages 5 and 6 are recorded in
  the trace as **skipped** with `Decision.Reason = "edges_unavailable"`, and
  `Result.Notes` carries the error. Ranking degrades to the pre-demotion order,
  which is the same output today's failure path produces.
- `"unavailable"` — the query succeeded but returned no edges (a store with no
  `memory_links` rows, e.g. a fresh import). Not an error, not a note.

The alternative — failing the whole search — is rejected: conflict and dedup are
*reorder* stages, and today's code treats a failure there as non-fatal. Making
them fatal would be a behaviour regression in the error direction.

**`memory.Memory` cannot carry stage 2 today, and PR 1 extends it.** `Memory` has
`ResolvedAt *string` and `Confidence *float64` but **no** `ValidFrom`,
`ValidUntil` or `VerifiedAt`, and `scanMemories` does not bind those three columns
at all — the columns exist in the DDL, are copied between `memories` and
`memory_snapshots`, and are unreachable from Go (#575). PR 1 adds the three fields
to `Memory`, binds them in `scanMemories`, and adds them to the `SELECT` lists in
`store.go` and `vector.go:481-487`. They are declared **only** on `Memory`, not
also on `Candidate`: a duplicate declaration on the outer struct would shadow the
promoted embedded field and leave `Candidate.ValidUntil` and
`Candidate.Memory.ValidUntil` as two independent values, of which stage 2 would
read the wrong one. Moving the DTOs into `memory` (§ "Where the DTOs live") does
**not** fix that by itself, and I claimed it did: an outer field shadowing a
promoted one at a shallower depth is **legal Go**, not a compile error (only a
redeclaration at the *same* depth is). The move's real benefit is different and
weaker — it puts `Candidate` and `Memory` in one package so a reviewer can see
the embedding and the fields together — so the prohibition has to be a **review
invariant plus a test**, not something the compiler enforces:

```go
// Candidate must not redeclare these; memory_test asserts it by reflection,
// so a future edit that adds one fails the suite rather than silently
// shadowing the promoted field.
```

The reflection test walks `Candidate`'s fields and fails if any name collides
with a promoted `Memory` field. That is the check that makes the claim true, and
it is a behavioural assertion about the type's field set, not a source-text
assertion.

**They are `*string`, not `*time.Time`, and stage 2 owns the parsing.** Three
reasons, all from the existing code rather than preference:

- `Memory.ResolvedAt` is already `*string` (`store.go:34`) — the established
  pattern for a nullable SQLite `TEXT` timestamp in this struct, and validity
  should not be the odd one out.
- The columns are unconstrained `TEXT` with **no** format enforcement, and the
  values already in the tree are not all the same shape: SQLite's own
  `datetime('now')` writes `2026-01-02 03:04:05`, while
  `provenance_validity_test.go:174` inserts a bare date, `'2026-09-24'`.
- `database/sql` will not scan a `TEXT` driver value into `*time.Time`; the
  driver would have to be taught, and hand-rolling that is worse than parsing.

So `scanMemories` scans `sql.NullString` and the fields stay `*string`, and stage
2 parses them against a documented, closed layout set — `"2006-01-02 15:04:05"`
(the SQLite writer's shape) and `"2006-01-02"` — in that order. **A value matching
neither layout is treated as unset, not as an error**, and the row is recorded in
the trace with `Reason: "validity_unparseable"`. The reasoning: a malformed
timestamp must not be able to fail a whole retrieval, and must not silently read
as "valid" either — surfacing it is the only honest third option. The regression
test seeds both accepted shapes plus a malformed one and asserts none of them
errors the query.

Returning `Edges` from the same call keeps `Retriever` a one-method interface —
nothing in `internal/assemble` can reach a `*sql.DB` — at the cost of one batched
query instead of a round trip per stage. `Candidates` returns an **error** when
every retrieval path failed, rather than an empty set that reads as "no memory
resembles this".

`memory.Store` gets a `Candidates` method wrapping the existing
`SearchFTS`/`SearchVector`/`filterVectorFloor`/`FuseAndSelectWindow`/`decayRank`
calls and returning the window **plus** the discarded tail; the hook gets a
read-only variant over its own handle. Four consequences:

- **Scoring stays inside `memory`.** `decayRank` (`vector.go:332`) is
  unexported and stays that way; `internal/assemble` never calls it. Stage 1's
  ordering, decay and trim are *inside* `Candidates`, and stages 2–8 re-sort by
  the `Candidate.Score` it hands back. That removes a cross-package call to an
  unexported symbol rather than exporting it.
- **#591 is a hard prerequisite.** `FuseAndSelectWindow` does not exist on
  `main`. PR 1 of the migration cannot land before it, because the widened
  window is what `Candidates` returns.
- **The configured floor survives.** `SearchHybrid` sets
  `p.MinSimilarity = s.vectorMinSimilarityFloor()` before searching
  (`vector.go:433`) and `search.min_similarity` is user-settable. `Candidates`
  must do the same, so a nil `Request.Params` means
  `DefaultSearchParams() *with* the store's configured floor re-applied* — not
  bare defaults, which would silently disable every non-zero user setting. The
  regression test sets a non-zero `search.min_similarity` and asserts rows below
  it are still dropped.
- **`Request.Now` is honoured everywhere, and that costs a small storage change.**
  A required clock is meaningless if the primitives below call `time.Now()`
  themselves, and they do — five times in `vector.go` (lines 425, 448, 460, 567,
  577) — while `DecayRankingSQL` hard-codes `julianday('now')` (`store.go:1089`,
  `1091`). A `Candidates` that delegates to them would rank against the wall
  clock and then stamp the trace with `Request.Now`, and the two would disagree.
  So `Candidates` passes `Request.Now` down: `decayRank` already takes a `now`
  parameter, so the Go path is already bound-able; `DecayRankingSQL` needs its two
  `julianday('now')` calls replaced by a bound `?` parameter, which changes that
  exported constant's signature. The hook shows the pattern it wants —
  `hook.go:470-473` captures `now` once and re-scores in Go *specifically* to
  avoid clock drift between the SQL rank and the Go rank. `Candidates` makes that
  unconditional, and the regression test is that two `assemble.Run` calls with
  different `Now` values and identical data produce different `AgeDays` and
  `DecayFactor` and identical `Base` scores.

### Passive mode is three policies, not one function

My first draft equated passive mode with `GetTopMemories` (`store.go:1097`) plus
`DecayRankingSQL`. **That is wrong about session-start**, and getting it wrong
would have quietly broken PR 2's no-op gate. `GetTopMemories` exists and shares
the ranking SQL, but the hook does not call it — `loadSessionContext` issues its
own query against its read-only handle (`hook.go:459-464`), reusing
`memory.DecayRankingSQL` for the `ORDER BY` precisely so the two cannot drift
apart, and then applies selection logic that exists nowhere in `memory`. Three
distinct policies are in play:

| Policy | Project bucket | `_global` bucket |
|---|---|---|
| Fetch | own SQL, `resolved_at IS NULL`, over-fetch `sessionMemoriesCap*3` (45) — `hook.go:459-464` | own SQL, `resolved_at IS NULL`, over-fetch `globalsCap*2` (16) — `hook.go:339-344` |
| Order | `DecayRankingSQL` DESC, then `importance`, `created_at`, `id` | `pinned DESC, importance DESC, updated_at DESC` (`hook.go:342`) — **not** decay-ranked at all |
| Selection | **two-pass**: pass 1 reserves up to `injection.behavior_floor` slots for behavioural categories scored by `importance × DecayFactor × category_weight` under per-category caps; pass 2 fills the rest by plain decay score (`hook.go:507-571`) | no two-pass, no weights — a flat `globalsCap` = 8 cut |
| Cap | `sessionMemoriesCap` = 15 (`:323`) | `globalsCap` = 8 (`:311`) |
| Near-dup | demoted before the cap, at the threshold from `linking.demotion_threshold` (default 0.90, `config.go:194`), pushed into the store by `SetDemotionThreshold` (`store.go:105`) (`hook.go:604`) | `globalsDemotionThreshold` = 0.85 (`:317`), lower on purpose — a live global pair linked at 0.8857 |

Three consequences the pipeline has to respect:

1. **Pass 1 is a selection stage, not a ranking stage.** It reorders *then cuts*,
   and it cuts by category. That is not stage 1 (retrieve), not stage 4
   (provenance multiplier) and not stage 8 (byte/count trim) — it is a
   *membership* decision driven by category semantics. It becomes an explicit
   **passive selection policy** on `Slice` (or a `PassivePolicy` beside it), and
   it is the one place where `injection.behavior_floor`, `category_weights` and
   `category_caps` must keep their exact current meaning.
2. **`_global` is not decay-ranked.** Stage 1's `DecayFactor` is correct for
   project memories and wrong for globals; applying it uniformly would reorder
   every global by age, which today it is not. The bucket therefore selects the
   scoring function, exactly as it already selects the cap and the demotion
   threshold. This is the single most load-bearing row in the table above.
3. **The demotion threshold is per-bucket** (0.90 from
   `linking.demotion_threshold` vs 0.85 for globals), so it belongs on `Slice`
   next to `DropDemotedLosers` rather than in a global config default. Note the
   key is under `linking`, not `injection` — the two injection knobs in this
   table (`behavior_floor`, `category_weights`) live in a different config
   section from the threshold, and conflating them would make the globals
   override look like a documented injection setting when it is a hardcoded
   constant with a comment explaining why (`hook.go:313-317`).

**How this stays a no-op for PR 2.** PR 2 only changes the *rendering* and adds
one opt-in scope predicate. The passive policy table is therefore a
**specification of existing behaviour that `Candidates` must reproduce**, not a
redesign: the three policies are ported as-is, and PR 2's gate is a
before/after of the rendered block byte-for-byte. Any divergence found while
porting — a different `ORDER BY` tiebreak, a floor applied one slot early — is a
bug in the port, fixed to match the hook, not a new preference. The
configuration keys (`injection.behavior_floor`, `category_weights`,
`category_caps`) are read by `Candidates`, not re-implemented in `assemble`, so
there is exactly one place that knows their meaning.

## Decision 2 — The nine stages

Order is fixed and load-bearing: **every filter precedes window closure**, which
is the invariant #573 and #581 are really about. That applies to `category` just
as much as to `scope` — today's category post-filter has the same defect #573
found in the scope one, and the pipeline must not import it.

```text
  Request
    │
    ├─1 retrieve      Candidates() → widened, untrimmed set
    ├─2 validity      drop expired / not-yet-valid            ── membership
    ├─3 predicates    project membership, category, scope     ── membership
    ├─4 provenance    bounded multiplier                      ── score
    ├─5 conflicts     sink superseded; split contradicts      ── membership+order
    ├─6 dedup         collapse duplicate/near-dup             ── order
    ├─7 diversity     per-bucket quota                        ── membership
    ├─8 budget        final order, hard byte trim             ── membership
    ├─9 render        one item renderer
    └─ outcome        answerable | weak | empty               ← §3
```

| # | Stage | Exists today | Where | Action |
|---|---|---|---|---|
| 1 | retrieve | yes, but query-only and clock-unbound | `SearchHybridParams` `vector.go:439`; `fuseAndRank:400`; `decayRank:332`; `FuseAndSelectWindow` (#591). **Passive mode is *not* `GetTopMemories`** — see "Passive mode is three policies", below. | **Keep all of it in `memory`.** New `Candidates` returns the widened set *with* scoring already applied and `Request.Now` honoured (see the four consequences above). **Two modes**: `Request.Query != ""` is the hybrid path; `Request.Query == ""` is the passive path, which is what session-start and project-context need and what no current unified function serves. |
| 2 | validity | **no** | columns `valid_from`/`valid_until`/`verified_at` are inert (#575) | **New.** `valid_until < now` → drop; `valid_from > now` → drop; both NULL → valid. `verified_at` NULL sets an `unverified` *flag only*, no penalty in v1 (see the last decision below). |
| 3 | predicates (project, category, scope) | half | `memory.ScopeMatches` is already an exported pure predicate; the *loops* are `mcpserver.go:627-639` (category) and `652-663` (scope) | **Move both loops** into stage 3, over the widened set, and **carry `Request.Category` into `CandidateRequest`** so the SQL legs can over-fetch on it. Neither `SearchFTS` nor `SearchVector` accepts a category today, so a `Candidates` that ignored it would either drop `ghost_memory_search`'s category filter or reintroduce the post-closure defect #573 found in the scope one. Project membership stays in SQL (both legs already do `project_id = ? OR '_global'`); the stage records all three verdicts per row. |
| 4 | provenance | **no consumer** | the column *is* writable — `Store.Create` persists `Memory.Confidence` (`store.go:767`) and `UpsertWithOptions` persists `Provenance.Confidence` (`store.go:1019`, `1056`), and a test stores 0.9 — but no **product tool** sets it (#575), and `agent` is written by `ghost_memory_save` and read by nothing | **New stage, identity by default.** Ship the stage and the bounded multiplier with the multiplier pinned to 1.0 until #575 has writers. Note the column is not *empty* on every store: rows written by import, by a direct `Store.Create` caller, or by a test can already carry a value, so stage 4 must define what a non-NULL `confidence` means **before** it is allowed to move any score — the test for that is a seeded row with `confidence = 0.1` that must rank identically to `NULL` while the multiplier is 1.0. |
| 5 | conflicts | partially | `demoteSuperseded` `vector.go:262` + `SupersedePenalties` `demotion.go:106`; `contradicts` appears **only** in the schema CHECK (`schema.go:240`, `migrate.go:273`) — no code writes or reads it | **Move the reorder** into stage 5, fed from `CandidateSet.Edges`. **Add** the contradicts-pair rule, gated on a fixture that actually contains such edges (see below). |
| 6 | dedup | yes | `demoteNearDuplicates` `demotion.go:163` + `DemotionPenalties:34` + `StableDemote:90` | **Move** into stage 6, fed from `CandidateSet.Edges`. Reorder stays membership-preserving; the trace records the collapse set. |
| 7 | diversity | **no** | — | **New**, default no-op (§ below). |
| 8 | budget | fragmented | `args.Limit` `mcpserver.go:671`; `truncateUTF8(200/300)` `hook.go:491`; `sessionMemoriesCap=15` (`hook.go:323`); `globalsCap=8` (`hook.go:311`) | **New.** Two mechanisms, not one: a UTF-8-safe per-item content **clamp** (`Slice.ClampBytes`, the existing `truncateUTF8` behaviour) and a whole-item **hard trim** (`Slice.MaxBytes`/`MaxItems`). Conflating them was wrong — the clamp is presentation, the trim is membership. |
| 9 | render | twice | `formatMemories` `mcpserver.go:2156`; `formatSessionContext` `hook.go:198`; `scopeLabel:2190`; `quoteData:2214` | **Converge the item line only.** `scopeLabel` and `quoteData` move to `internal/assemble` and one `Item.Line()` renders them; each surface keeps its own framing *and* its own field order, because search prints `id`/`importance`/`pin`/`tags` and injection prints a bare bullet. A shared prefix of the two, not a shared whole line. |


### Four stage-level decisions worth defending

**Conflicts: supersede reorders, and `contradicts` also reorders in v1.** My
first draft had `contradicts` *remove* the weaker endpoint. That reverses a
deliberate, tested product contract, so it is rejected.
`TestNegativeRetrieval` (`internal/memory/negative_retrieval_test.go:179-196`)
has a case named **"contradiction stays while duplication sinks"** that seeds
`db_postgres` ⟷ `db_mysql` as `contradicts` and `db_postgres` ⟷
`db_postgres_dup` as `duplicate`, and requires the *lowest-importance* row
(`db_mysql`, 0.1) to survive at full rank while the restatement sinks. The
comment states the intent outright: "a system that treats both as 'similar' would
drop the row carrying the actual disagreement." Dropping it would fail that test
and lose the signal it was written to protect.

And `contradicts` is not unread. `internal/obsidian/render.go:171` lists it as a
directed relation and emits it in the vault, so both members are load-bearing
downstream too. So in v1: supersede → reorder (existing behaviour, moved);
`contradicts` → **record the pair, reorder nothing** (`elaborates` → group, also
non-removing). The trace still reports which pairs co-occur, so #583 can answer
"does this block contain a disagreement?" without changing membership. Making it
removing is a **separate, deliberate contract reversal** that needs its own
issue, its own fixture change, and a stated migration note — not a stage in this
design. That also means PR 6 has no `contradicts` fixture to add, and the
unmeasurability problem I flagged earlier disappears with the removal.

**Dedup: reorder, not fold — and the per-bucket policy is not uniform.** Two
rejections, one of them a real behavioural difference I had flattened.

*Reorder, not fold.* `Upsert` already folds the *storage*; a second fold at
assembly time would make "which row won and why" unanswerable — #583's entire
subject — and would change the ID set the graded bench scores. Reorder plus a
recorded collapse set gives #582 its duplicate count without changing membership.

*But globals already drop their losers, and stage 6 must not quietly un-drop
them.* `hook.go:364-387` filters a near-duplicate loser out **outright** for
globals, independent of the cap, with the reasoning in the code: "unlike project
memories (where `StableDemote` only reorders and relies on the 15-item cap to
actually drop the loser), globals are capped much tighter (`globalsCap=8`) and
near-duplicates must not survive merely because the set is small." Search reorders;
global injection removes. Converging them on membership-preserving reorder would
retain duplicate globals in the default session-start result and contradict PR 2's
no-op gate.

So the policy is explicit and per-slice, not global:

```go
type Slice struct {
    ...
    DropDemotedLosers bool // true for _global today (hook.go:364-387);
                        // false for project memories and for search, where
                        // reorder + the cap is the established behaviour
}
```

The decision is traced either way (`Decision.Stage == "dedup"`, `Kept: false`,
`Reason: "dedup_dropped_by_slice_policy"`), so §5's duplicate count works and PR
2's before/after rendered blocks are the check.

**Diversity: a quota, not MMR.** MMR over embeddings is the textbook answer and
is rejected: it re-scores (so it needs a `--sweep` to justify), it is
order-sensitive in a way that makes the trace harder to replay, and it competes
with `DecayFactor` for the same signal. v1 is a per-`Bucket` cap — *at most N
items from any one project or `_global`* — applied after stage 6, defaulting to
**off** until #582 has numbers to set it from, which is then a one-line follow-up
PR rather than a redesign. It is a membership change even when off-by-default, so
it is the stage most likely to regress the bench and gets its own PR (§6, PR 6).

**Validity and provenance ship inert, then get their writers.** Defining stages
2 and 4 before #575 lands would be two no-ops pretending to be features. Stage 2
drops rows only when a `valid_until`/`valid_from` is actually non-NULL — never,
today, so it is provably bench-neutral — and stage 4's multiplier is 1.0. #575
makes them bite. The pipeline shape lands first, the inputs second.

## Decision 3 — Abstention

### The rule

The outcome is computed **from the trace**, never by a separate query. A row
that reached stage 8 satisfying a floor signal makes the outcome not `empty` —
a definitional guarantee that the verdict cannot disagree with the result.

```text
every retrieval path failed (Candidates returned an error)  → error, not a Result
QUERY MODE (Request.Query != "")
   stage 1 produced 0 candidates                          → empty
   candidates existed, 0 reached stage 8                   → empty
   ≥1 reached stage 8, ≥1 satisfies a floor arm            → answerable
   ≥1 reached stage 8,  0 satisfies any floor arm          → weak
PASSIVE MODE (Request.Query == "" — session_start, project_context)
   0 rows admitted                                         → empty,    reason: no_memories
   ≥1 row admitted                                         → answerable, reason: not_applicable
```

### The `empty` reason set is closed, in actual stage order

An open-ended reason list cannot be a machine-readable contract, so the codes are
exhaustive and the first match wins. The order is **the stage number**, because
the earliest stage to empty the set is the one that actually explains it — later
stages are membership-preserving by design and so cannot be the cause unless
nothing earlier did it. My first draft listed these by habit rather than by stage
and got the order wrong in one place: validity is **stage 2** and was listed after
the stage-3 category reason, which contradicts the precedence the same paragraph
claims.

| # | Reason | Stage | Fires when |
|---|---|---|---|
| 1 | `no_candidates` | 1 | retrieval returned zero rows, every leg `Attempted && !Err` (query mode) |
| 2 | `retrieval_failed` | 1 | ≥1 leg errored and the survivors yielded nothing (partial leg failure) |
| 3 | `vector_backend_unavailable` | 1 | no embedder, or the embed call failed, so only FTS ran and matched nothing |
| 4 | `no_memories` | 1 | retrieval returned zero rows (passive mode) — the passive counterpart of 1, and **windowed**: it means the over-fetched set was empty, not that the store is empty, so it never licenses an absence claim (§ Output shape) |
| 5 | `all_invalid` | 2 | every candidate failed validity — expired, **or** not yet valid |
| 6 | `all_out_of_category` | 3 | every candidate failed the category predicate |
| 7 | `all_out_of_scope` | 3 | every candidate failed `ScopeMatches` |
| 8 | `all_dedup_dropped` | 6 | every candidate was removed by `Slice.DropDemotedLosers` (reachable only for the `_global` slice today) |
| 9 | `all_diversity_capped` | 7 | every candidate was cut by a per-bucket quota |
| 10 | `all_over_budget` | 8 | every candidate was cut by the byte/count trim |

Stages 4 and 5 contribute **no** reasons, and that is a statement about them
rather than an omission: stage 4's multiplier is 1.0 so nothing is dropped, and
stage 5 is membership-preserving in v1 because `contradicts` is non-removing and
`supersede` only reorders.

Three codes from my first draft are **removed because they are unobservable**, not
because they are inconvenient:

- **`all_out_of_project`** — project filtering stays in both SQL legs
  (`store.go:1165`, `vector.go:87`), so a row from another project never becomes a
  `Candidate` and stage 3 has nothing to reject. The one place an empty project
  *is* reachable is projectless session start, and that is a deliberate
  globals-only mode with its own outcome, not a filtering accident.
- **`all_resolved`** — there is no dropping stage for `resolved_at`. In query mode
  resolved memories stay searchable by design, and in passive mode
  `resolved_at IS NULL` is in the fetch SQL (`hook.go:461`, `:335`), so a resolved
  row never reaches the pipeline. Note the asymmetry that matters for §5: a
  resolved row *can* still be admitted in query mode, so the contamination
  metric's `Item.ResolvedAt != nil` arm stays; it is only the *empty* reason that
  is unreachable.
- **`all_demoted`** — supersede and dedup reorder rather than empty, except via
  `DropDemotedLosers`, which is reason 8 and now carries that name in the table
  rather than being invented in prose.

`window_exhausted` is deliberately **not** in the table. It is a modifier, not a
peer: `CandidateSet.Widened` is set, the specific cause (5–10) is what `Reason`
carries, and `Result.Notes` records that the window was also exhausted — the same
way today's `maybeIncomplete` caveat works, so a caller can tell "nothing matches"
from "nothing matched in what I looked at". Listing it as reason 11 would let a
reader conclude the answer is absent when it is merely bounded, which is the exact
error §4's empty-message copy also has to avoid.

The passive branch is not a special case bolted on. `weak` means "these rows
resemble the question less than I would like" — a claim that only means anything
when there *was* a question. Passive sources have no `Query` and no `QueryVec`
(§2 stage 1), a relevance floor has nothing to measure, and applying the query
table to them would mark every successful session start `weak`, which is worse
than useless. `not_applicable` is the honest value: no relevance claim was made,
so none is qualified. `Trace.Mode` records which rule ran, so a reader of an
explain payload is never guessing.

### Thresholds, and their inputs

Two arms, both reading signals stage 1 already produced, so neither needs a new
query.

**Arm A — keyword, on by default.** `0 <= FTSRank && FTSRank <= 3`. The lower
bound is load-bearing and my first draft got it wrong: `FTSRank` is `-1` when the
leg did not retrieve the row, and a bare `<= 3` test lets every vector-only
candidate satisfy the keyword arm, so a purely semantic match would be reported
`answerable` on the strength of a leg that never saw it. `0 <=` excludes the
sentinel, which is the same reason `ExplainRow` uses `-1` rather than `0`
("rank 0 is a real first place — an absent leg and a top-ranked one must not look
alike").

The floor is on the *FTS leg's rank*, not an RRF score: RRF values here are
0.005–0.01 (`FuseAndSelectWindow` does this arithmetic in its own doc comment) and
are a function of *leg depth*, not relevance, so an absolute RRF floor moves every
time the retrieval width changes while BM25 rank is stable and means what it
says. The default `3` is deliberately narrow — a rank-0 exact match can never be
withheld, which is the regression test #580 asks for and the same class of hole
#543/#591 just fixed from the other direction. Regression test: a vector-only
candidate (FTS leg not run) must never satisfy arm A.

**Arm B — vector, off by default.** `VectorScore >= cfg.Context.AbstainCosine`,
default `0.0`, i.e. disabled. It cannot default on: `search.min_similarity` is
`0.0` today (`internal/config/config.go:198`) — Ghost has never run a vector
cosine floor, so there is no measured value to inherit, and inventing one would
be exactly the unevidenced number #561 exists to eliminate. PR 4 ships the key;
PR 7 sets it from the LongMemEval abstention subset, before/after in the body.

**Arm A is a floor only when a vector leg actually ran.** This closes a hole the
first draft of this section contradicted itself on. With `QueryVec == nil` — no
embedder, or an embedding failure — arm B has nothing to measure, so an FTS hit
at rank 4 would fall through both arms and be labelled `weak` with reason
`below_floor`. That is precisely what #580 forbids: "when the vector backend is
unavailable, FTS-only hits remain `answerable` and the reason never claims below
floor." So:

```text
vector leg attempted and produced results  → both arms apply; below_floor is reachable
vector leg did not run (QueryVec nil, or  → arm A is a relevance NOTE, not a gate.
  embedder unavailable/failed)              Any admitted row is answerable, with
                                           reason no_vector_leg. Never below_floor.
```

`LegStatus.Attempted` distinguishes "the backend was never there" from "the
backend answered and found nothing", and the second case *does* leave `below_floor`
reachable — an FTS rank-4 hit plus a vector leg that returned nothing is a
genuinely weak result and saying so is the product working. The regression test is
a strong FTS hit at rank ≥ 4 with `QueryVec == nil`, which must come back
`answerable` / `no_vector_leg`.

**`weak` annotates; it does not withhold.** A `weak` result returns its items
*and* the abstention line. Withholding would be a ranking change, and
`FuseAndSelectWindow`'s documented history is the cautionary tale: a floor wide
enough to be safe is too weak to help, and one narrow enough to help (measured:
hybrid R@1 0.507 → 0.366) destroys the metric. A `weak` block is "here is the
best I have, and here is why you should check it" — the caller decides. This
also makes PR 4 bench-neutral by construction, which is what lets it ship early.

### Output shape

Not an empty list, not a bare string. Two carriers, because there are two
consumers with different needs.

**Machine-readable** — one trailing line in the existing text payload, so no
response shape changes and no protocol capability is assumed:

```text
- [gotcha] `A1B2…` (0.9) «session injection bypasses the debug build»
[ghost:outcome=weak reason=below_floor floor_fts_rank=3 abstain_cosine=off candidates=7 admitted=2]
```

`abstain_cosine=off` is deliberate: a caller reading `cosine=0.0000` cannot tell
"no vector threshold is configured" from "your query had no vector neighbours at
all", and #580's AC requires those to stay distinguishable.

**Human-readable** — a sentence above the items for `weak`, replacing the block
for `empty`:

```text
Ghost memory: the best match here is weak. Treat the items below as leads, not
established facts, and verify before acting on them.
```

**The `empty` sentence is reason-specific, and only one reason may claim
absence.** My first draft used a single sentence — "no sufficiently relevant
memory for this question" — for every `empty`. That is wrong for most of the
reason set, and wrong in the one direction that matters: it tells an agent that
prior context is *absent* when the truth is often that retrieval was
*incomplete*. A bounded window or a failed leg is not evidence of absence, and an
agent that reads "nothing relevant exists" will stop looking and save something
that was already known.

So the copy is a function of `Reason`, with the *absence* claim reserved for the
one case that actually establishes it:

| Reason | Copy |
|---|---|
| `no_candidates` | "no stored memory matches this question" — **absence**, the only case entitled to claim it: every leg completed with no error, and the fetch was exhaustive over the store rather than windowed |
| `no_memories` | "no memories recorded for this project yet" — **not** absence. Passive fetch is capped (`sessionMemoriesCap*3` / `globalsCap*2`), so an empty passive block means the *window* was empty, not the store. It is a "nothing recorded yet" statement about the over-fetched set |
| `all_invalid` | "everything I found for this is out of date; it is not being suggested" — the rows exist, they were correctly withheld |
| `all_dedup_dropped`, `all_diversity_capped`, `all_over_budget` | "found N, all withheld by the <dedup / diversity / budget> limit; raise the limit to see more" — a knob, not a claim |
| `retrieval_failed` | "search could not complete (<leg> failed); this is not evidence that nothing exists" — explicitly **not** absence |
| `vector_backend_unavailable` | "keyword-only search, no vector index available; this may be less complete than usual" |
| `all_out_of_category`, `all_out_of_scope` | "nothing matches the <category / scope> filter you set" |

`no_memories` is the row worth dwelling on, because I initially put it in the
absence row alongside `no_candidates` and that was wrong. Passive retrieval is
**windowed by construction** — 45 rows for the project bucket, 16 for globals —
so an empty passive result cannot distinguish "this project has no memories" from
"this project has more than 45 and none of the first 45 matched". The honest copy
claims only what the fetch supports. If a caller needs true absence semantics for
a project, that is a different query with a different contract, and faking it here
would put a claim in the payload that the retrieval never made.

**One rendering of the bounded case, not two.** My first pass had the copy table
append "in what I searched" while the *Honest absence* section (§ below) quoted a
second, different sentence for the same condition. Two renderings of one state is
a bug in the making, so there is now exactly one:

```text
Ghost memory: no match within the searched window — widen the limit or drop the
scope filter. This is not evidence that nothing exists.
```

It is appended as a `Note` whenever `CandidateSet.Widened` is true, regardless of
the stage reason, and it **suppresses the absence claim** — so it is only ever
combined with a non-absence reason, since the absence case requires
`Widened == false` by definition.

The rule that makes all of this checkable: **the absence claim is permitted only
when every leg reports `Attempted && Err == ""` and `Widened` is false.** That is
a one-line assertion in the renderer and three tests — set `Widened = true`, set
`Legs["fts"].Err`, and set `Source: SourceSessionStart`, and assert the absence
sentence does not appear in any of them.

Each sentence is byte-bounded by the same `Slice.MaxBytes` as the items — #580's
last AC — and each includes the `Result.Notes` for its reason, so a truncated
render still says *why* rather than just stopping mid-sentence. **Session-start
prints none of them**, for the same reason the passive rule has no `weak`: passive
rows make no relevance claim to qualify, and a "this might be wrong" banner on
every session start trains the agent to ignore it. The hook's empty block stays
*absent*, not apologetic; the outcome lives in `Trace`, and the block's existing
"N of M shown" line is where a partial block announces itself.

The trace is inspected via a `ghost context --explain` flag that **does not exist
today**: `runContext` (`cmd/ghost/session.go:18-30`) parses only `--cwd` and
silently ignores anything else, so the diagnostic this section promises would
otherwise be a documentation of a command that quietly does nothing. Adding the
flag is part of migration PR 5, alongside the `Trace` → payload projection. Until
it lands, `explain:true` on `ghost_memory_search` is the only trace surface.

### Honest absence (#573)

`empty` and a *bounded* search are different claims. When stage 1's widened set
was itself truncated (`CandidateSet.Widened == true`) and a later stage then
emptied it, absence is not provable. Consistent with the reason table, the
**specific stage reason stays in `Reason`** and the boundedness is carried in
`Notes`, which renders the single bounded sentence quoted in § Output shape —
*"no match within the searched window — widen the limit or drop the scope
filter. This is not evidence that nothing exists."* There is deliberately no
second rendering of this state; an earlier draft had one here and one in the copy
table, and two sentences for one condition is a defect regardless of which is
better. `LegStatus.Truncated` distinguishes "the backend answered" from "the
backend was cut off", and a failing leg is reported as a `Note` rather than
silently narrowing the set — the current `SearchHybridParams` (`vector.go:442/454`)
discards a leg error, which is how #580's "FTS-only hits must not be reported as
below-floor" requirement currently has nowhere to be expressed. The existing
`maybeIncomplete` caveat (`mcpserver.go:674-676`, 685+) moves into `Notes` and is
rendered from one place, so #573's zero-result half and its non-empty half can no
longer disagree.

## Decision 4 — Explainability: the trace *is* the explain payload

`internal/memory/explain.go` today calls the real `SearchHybrid` for membership
(line 67) and then **re-derives** the scores locally (lines 180-210: it re-adds
`p.FTSWeight/(RRFK+r+1)` and `p.VecWeight/(RRFK+r+1)`, re-runs `DecayFactor`,
and re-queries `SupersedePenalties`/`DemotionPenalties` over the final window).
That is a second implementation of the ranking, and #571 is what it costs: the
two disagree whenever the tool post-filters. This design removes the second
implementation.

```go
type Trace struct {
    Mode      string             // "query" or "passive" — which outcome rule ran
    Signals   map[string]Signals // per candidate ID, the retrieval/scoring facts
    Stages    []StageTrace
    Decisions []Decision         // one per row per stage that touched it
    Floors    Floors             // the abstention thresholds actually applied
}

// Signals are copied out of Candidate in stage 1, while the numbers still exist.
// Without them the projection has nothing to read: CandidateSet is gone by the
// time a Result exists, and recomputing is the drift this design removes.
type Signals struct {
    FTSRank, VectorRank   int       // -1 when that leg did not retrieve it
    VectorScore           float64   // cosine; -1 when absent
    Base, DecayFactor     float64
    AgeDays               float64
    CreatedAt             time.Time
}

type StageTrace struct {
    Stage      string
    In, Out    int
    DroppedIDs []string // every stage: which rows left, for §5's budget metric
    Reordered  bool
    Notes      []string
}

type Decision struct {
    ID        string
    Stage     string // "retrieve","validity","scope","provenance","conflict",
                    // "dedup","diversity","budget"
    Kept      bool
    Reason    string // stable code, e.g. "scope_excluded:environment=production"
    AgainstID string // the row that displaced or pair-grouped this one
    Before, After float64 // score before/after, when the stage re-scored
}
```

`Signals` is what makes the projection possible at all: a one-time copy out of
`Candidate` in stage 1 that replaces `explain.go`'s re-derivation of `FTSRank`,
`VectorRank`, `VectorScore`, `RRFScore`, `DecayFactor` and `AgeDays`. It cannot
be derived later without recomputation, because `CandidateSet` no longer exists
by then. `StageTrace` gives #581's per-stage counts, `Decision` gives #583's
per-row attribution, `Floors` gives #580's reason-and-floor. Three invariants,
each with the test that pins it:

1. **Projection, not recomputation.** `ExplainRow` becomes a projection of
   `Trace`; the RRF/decay arithmetic in `explain.go` is deleted, not ported.
   Test: `explain_rrf_equals_ordering_score` — for every included row,
   `Item.Score` equals the value replayed from that row's `Decision`s. This is
   #583's second AC, and it can only hold if explain reads the trace.
2. **Membership is decided once.** The scope verdict in `Decision.Stage ==
   "scope"` *is* the scope filter; `included` cannot disagree with the tool
   because there is no separate tool-side filter. Closes the class in #571.
3. **The trace is always recorded, the projection is gated.** Recording is a
   slice append per row per stage over a ~20–30 row pool, and gating it on
   `Explain` would reintroduce exactly the flag-dependent second path this
   design removes. Only the `ExplainRow` slice — which allocates per row and is
   what the 32KB budget applies to — is built when `Explain` is set. A
   `BenchmarkAssemble` guard asserts the trace costs < 40µs on the built-in
   fixture (219 scored queries on `main`, 220 once #591 lands); the fallback if
   it exceeds that is to drop `Before`/`After` from non-scoring stages, not to
   gate recording.

**Payload budget.** 32KB, per #583. When exceeded, whole `ExplainRow`s are
dropped from the **lowest-ranked** end and the payload gains
`"truncated": {"dropped_rows": N, "reason": "payload_budget"}` — never a
mid-row cut, because a silently shortened diagnosis is worse than an absent one.

**Migration of `ExplainSearch`.** A thin adapter: `assemble.Run` with
`Explain: true`, then project `Trace` → `SearchExplain`. Every existing field
(`FTSRank`, `VectorRank`, `VectorScore`, `RRFScore`, `DecayFactor`, `AgeDays`,
`SupersedePenalty`, `NearDuplicatePenalty`, `Reason`) is preserved — #583
requires an additive change. `SupersedePenalty` and `NearDuplicatePenalty` are
read off the stage-5/6 `Decision`s, which is *more* faithful than today's
post-hoc re-query: the re-query sees only penalties among the final window,
while the stage saw the whole candidate set.

## Decision 5 — The metrics hook for #582

`#582` needs six numbers, and all six are computable from `Result` + `Trace` — the
sixth, result rate, exists because the other five can all be gamed by abstaining
(§ "Empty blocks").
Bench calls the same `assemble.Run` with `Source: SourceBench, Explain: true` and
`Condition` set per ablation.

**One honest limit on "zero bench-side retrieval code":** it applies to the
**context mode only**, and the existing ablation tables keep their direct store
calls. `internal/bench.Run` (`runner.go:45-62`) scores `fts-only`, `vector-only`
and `hybrid` by calling `store.SearchFTS` / `store.SearchVector` /
`store.SearchHybrid` directly, so the three conditions are **not symmetric** — and
an earlier draft of this section treated them as if they were, which was wrong.

**Corrected: only `fts-only` and `vector-only` bypass conflicts and dedup.**
`SearchHybrid` (`vector.go:431`) is a thin wrapper over `SearchHybridParams`
(`:439`), and **all three of that function's exits run `demoteResults`** — the
nil-`QueryVec` return (`:448`), the empty-vector-leg return (`:460`), and the
fused path through `fuseAndRank` (`:463` → `:426`). `demoteResults`
(`demotion.go:154`) is `demoteSuperseded` + `demoteNearDuplicates`.
`SearchFTS` (`store.go:1155-1175`) and `SearchVector` (`vector.go:79`) return
straight from their SQL with no demotion at all, so for those two the bypass is
real.

**Which stages would actually change `hybrid` if it were rerouted through
`assemble.Run`?** Not the two this section used to name — those already run:

| Stage | Effect on `hybrid` if rerouted |
|---|---|
| 1 retrieve | **Moves the IDs and the order.** `Candidates` returns a widened, untruncated pool; today `decayRank` ranks then truncates to `limit` (`vector.go:425`). More candidates compete for the same window slots, so which rows survive changes. This is the real metric mover. |
| 2 validity | No change today (the columns are inert); becomes a membership change the day #575 writes them. |
| 3 predicates | No change on the bench fixture, which is unscoped and uncategorised. The ordering fix — predicates ahead of window closure — is real but a no-op when no predicate is set. |
| 4 provenance | No change; the multiplier is pinned to 1.0. |
| 5 conflicts | **No change.** `demoteSuperseded` already runs. The only addition is the `contradicts` pair rule, which is non-removing and reorders nothing in v1. |
| 6 dedup | **No change for project rows** — `demoteNearDuplicates` already runs. The addition is `Slice.DropDemotedLosers`, which is `true` for `_global` only, so it changes membership only for `_global` rows in the block. |
| 7 diversity | No change; default off. |
| 8 budget | No change, provided bench sets `MaxItems`/`MaxBytes` to the current limit — and unset is equally a no-op. |
| 9 render | No change to the metrics; bench scores IDs, not rendered text. |
| outcome | No change; `weak` annotates and withholds no row. |

So the risk is real but **narrower** than "three new stages land on every
condition". Rerouting `hybrid` would not newly apply conflicts and dedup; it
would widen the candidate pool and pick up the `_global` drop policy. The
baseline argument still stands — `regression_test.go`'s floors (`ndcg10`,
`recall10`) are measured against exactly these numbers, so silently redefining
them would invalidate the regression baseline rather than test it — but it is a
*widening* risk, not a wholesale redefinition. The three ablations therefore
stay as they are, and #582's context metrics are reported as an additional
condition.

`Condition` is in the contract anyway, for the reason it is needed *at all*:
vector-only is not expressible otherwise. A nil `QueryVec` means FTS-only, and
zeroing `FTSWeight` is not a substitute, because fused FTS candidates still enter
the candidate union with a zero base score and would occupy window slots — the
ablation would not be measuring vector-only retrieval. With `Condition` available,
a later PR can unify the ablations on purpose, as its own bench-gated change.

| Metric | Computed from | Note |
|---|---|---|
| **Context precision** | `Σ relevance(Item.ID) / len(Items)` | bench's existing `Query.Rel` grades. The denominator is the *assembled* block, not a ranked list — that is the whole difference from NDCG. Empty blocks are **excluded** from the ratio and counted separately — see "Empty blocks" below. |
| **Contamination rate** | admitted `Item`s for which `ExpiredAt‖NotYetValidAt‖ScopeContradicts‖BucketUnexpected` all hold, composed by the metric from the shared leaf predicates | `Item.ResolvedAt != nil` (no stage drops resolved rows in query mode, so this arm is metric-only), **or** `Item.ValidUntil != nil && Item.ValidUntil.Before(req.Now)` (expired), **or** `Item.ValidFrom != nil && Item.ValidFrom.After(req.Now)` (not yet valid), **or** `Item.Scope` contradicts the request, **or** `Item.Bucket` is neither the requested project nor `_global`. The nil checks are explicit because the fields are pointers — a bare `<`/`>` against `req.Now` does not compile, and a bare dereference can panic. **Both validity arms are required**: stage 2 drops on `valid_until < now` *and* on `valid_from > now` (Decision 2, stage 2), and an earlier draft checked only the first. A half-mirrored predicate is worse than a re-implementation, because it reports 0% contamination for a not-yet-valid row stage 2 would have dropped, reading as "the filter works" when the metric never looked. Hence the fixture below. `_global` is *never* contamination: both retrieval legs and `GetTopMemories` include it deliberately (`store.go:1106`, `1165`), so a global preference is the product working. The metric composes the leaf predicates itself rather than calling a shared drop function — see the next paragraph for why that distinction is what keeps the metric non-vacuous. |
| **Budget adherence** | `Result.Bytes` vs. the matching `Slice.MaxBytes`; dropped IDs from the stage-8 `StageTrace.DroppedIDs` | `DroppedIDs` is what lets bench intersect the trim with `Query.Rel` and measure *relevant rows the trim discarded* — the "low-relevance memory crowding out a relevant one" regression, otherwise invisible. |
| **Diversity** | `max_b count(Item.Bucket) / len(Items)` | One histogram over `Item.Bucket`, with `_global` as its own bucket. Empty blocks excluded, as above. |
| **Result rate** | `count(Outcome != empty) / count(queries)` | Reported **separately**, and it is the metric that makes the others readable. Because empty blocks are excluded from precision and contamination, an assembler that returns nothing has *no* precision sample rather than a bad one — so on its own it would post a clean scoreboard. Result rate is the number that makes that visible. |
| **Token cost** | `Σ Item.Bytes` per answered question, reported next to accuracy | Bytes, not a tokenizer: the injection budget is already in bytes and `MaxContentLen` is 8,000 bytes. A tokenizer would be a second, disagreeing notion of size. |

**Empty blocks are excluded from every ratio, and never aggregated as zero.**
Context precision and diversity both divide by `len(Items)`, and the design
deliberately admits empty results — that is what `OutcomeEmpty` and `weak` are
for. An undefined policy here is not a style gap: `0/0` is `NaN` in Go, and
`NaN` propagates silently through a mean, so a single empty block poisons the
aggregate with no visible error. Worse, *skipping* and *counting as zero* are
opposite incentives — counting as zero makes an assembler that abstains on
everything score perfectly on contamination and diversity.

So the policy is fixed and stated in the metric table:

- A query with `Outcome == empty` contributes to **result rate** and to **nothing
  else**. It is not a 0, and it is not a 1.
- Each ratio reports `(numerator, denominator)` as well as the value, so a reader
  can see the scored set shrank. A ratio over 0 queries is reported as `n/a`, and
  `n/a` is never averaged.
- Precision, diversity and contamination are computed **only** over blocks with
  ≥1 admitted item, and the count of excluded queries is printed next to them.
- The one place a zero *is* meaningful is a per-query numerator: a non-empty block
  with no relevant item is genuinely 0.0 precision, and that must drag the mean.

The regression test is arithmetic, not behavioural: one query with an empty
`Result`, one with a single relevant item, and one with a single irrelevant item
assert the reported triple is `(result rate 2/3, precision 1/2, n/a never
averaged)`. A change that counts empty blocks as 0.0 fails it on precision; a
change that averages `NaN` fails it on the report.

**Corroborated by the trace; shared at the leaf, not at the top.** #582's AC says
the metric must be derived from production code rather than re-implemented in
bench. Two things about my first two attempts were wrong, and the second is worse
than the first.

**Corroborated by the trace; shared at the leaf, not at the top.** #582's AC says
the metric must be derived from production code rather than re-implemented in
bench. Two things about my first two attempts were wrong, and the second is worse
than the first.

*Attempt 1* claimed the classifier reads the production **exclusion** codes in
`Decision.Reason`. It cannot. `Decision.Reason` describes rows a stage
*excluded*, and contamination is measured over rows that were **admitted** — so
every correctly filtered contaminant is invisible to the metric by construction,
and a row that leaks through is recorded as `Kept: true`, carrying no exclusion
code to classify. The two populations are disjoint. A metric built that way would
report 0% contamination on a store where every filter is broken.

*Attempt 2* made `assemble.Contaminating(item, req)` the drop predicate that the
stages call. That is worse, because it makes the metric **vacuous**: if stages 2
and 3 drop every row the classifier flags, then by construction no admitted item
can be flagged, so contamination is 0% on a healthy store *and* on a store where
every filter is silently broken. The metric would be a tautology wearing a
percentage sign.

The resolution is to share the **leaf predicates** and compose them separately:

```go
// Leaf predicates: each is the single definition of one filter, called by the
// production stage that owns it AND by the metric's own composition.
func ExpiredAt(validUntil *time.Time, now time.Time) bool
func NotYetValidAt(validFrom *time.Time, now time.Time) bool
func ScopeContradicts(scope, want map[string]string) bool
func BucketUnexpected(bucket, projectID string) bool

// stage 2 drops when ExpiredAt(..) || NotYetValidAt(..)
// stage 3 drops when ScopeContradicts(..) || BucketUnexpected(..)
// the metric flags when all four hold — its own composition, not a call
// into a shared drop function
```

So a stage that forgets to call `NotYetValidAt` is caught by the metric, and a
change to what "expired" means is still made in exactly one place. That is what
honours the AC — the field-level definitions are single-sourced and cannot drift
— while the metric stays an *independent composition*, which is the only shape in
which it can report anything other than zero. It is also why the
`future_scheduled` fixture row is load-bearing: with a shared drop function that
row could never be flagged, so the test would be asserting a tautology.

`Decision.Reason` is then used for what it actually supports, as
**corroboration**: bench cross-checks that a row the classifier flags as
contaminated was *not* excluded, which is exactly the leak case the exclusion
codes can express. `Decision` therefore needs no change — a `Kept: true` row is
enough to detect the leak — and the trace gains a real use rather than a
ceremonial one.

**The design constraint this imposes on `Item`:** contamination is a property of
an *included* row, so `Item` must carry `Scope`, `ResolvedAt`, `ValidFrom`,
`ValidUntil`, `ProjectID` and `Bucket` — the same fields the renderer prints.
`ValidFrom` is not optional padding: the metric has a *not-yet-valid* arm, so
an `Item` without it cannot express half of stage 2's predicate. A content-only
`Item` would force #582 to re-query the store and drift from the product, which
is why §1 defines it with the axis fields. Reported, not gated, until two
independent changes have been measured — #582's own rule.

**The regression fixture this makes mandatory (both validity arms).** The
contamination metric is only trustworthy if it is fed a block that *contains* the
things it claims to catch, and a fixture whose validity columns are all NULL
cannot tell a correct predicate from a vacuous one. So the contamination test
seeds four rows against a fixed `Request.Now`, all in the project's retrieval set
and all otherwise relevant to the query:

| Seeded row | `valid_from` | `valid_until` | Expected |
|---|---|---|---|
| `future_scheduled` | `Now + 24h` | `nil` | dropped by stage 2; must be classified contaminating if it ever reaches a block |
| `expired_policy` | `nil` | `Now - 24h` | dropped by stage 2; same expectation |
| `valid_window_open` | `Now - 24h` | `Now + 24h` | **admitted, must not be counted** — both columns non-NULL, both inside the window |
| `valid_current` | `nil` | `nil` | **admitted, must not be counted** — the no-columns case |

The first two are the arms the assertion exists to exercise. `future_scheduled`
is the row this finding turned on: **delete the `ValidFrom` arm from the metric
and this test must fail.** Before the fix, a not-yet-valid row reported 0%
contamination because the metric never compared `ValidFrom` to `Request.Now` —
the symptom was a metric that looked correct precisely when it was blind.

`valid_window_open` and `valid_current` are the two negative controls, and they
are not interchangeable. `valid_window_open` kills the naive over-broad predicate
that flags any row merely for *having* a validity column set — that mutant still
passes both dropped rows, because it flags them for the wrong reason, and only
this row exposes it. `valid_current` kills the degenerate "flag every included
row" classifier, which is otherwise indistinguishable from a real one when every
seeded row is supposed to be contaminated. `valid_current` alone would *not*
catch the over-broad mutant, because both of its columns are NULL; that is the
whole reason for the fourth row.

The test drives `assemble.Run` with a **relaxed** budget so the block is not
trimmed before the metric sees it, then asserts both halves: that neither dropped
row appears in `Result.Items`, and that a hand-built `[]Item` containing each of
the four *is* classified correctly. The second half matters — otherwise the
classifier is never exercised, because stage 2 already removed the only rows
that should trip it. Drop either validity column from `Item` and the test does
not compile, which is the intended coupling between the metric and the type.

## Decision 6 — Migration

Seven PRs, each shippable alone, each naming its issue and its bench
expectation. The bench delta gate (origin/main vs. branch, NDCG@10 and R@5
within 0.005) applies to PRs 1, 4 and 6; PRs 2, 3, 5 and 7 cannot move ranking
and say so with the reason. **PR 1 is additionally blocked on #591**, since the
widened window it returns does not exist on `main`.

| # | Branch / title | Closes | Bench expectation |
|---|---|---|---|
| 1 | `feat(assemble): internal/assemble seam; scope and category filter before window closure` | **#573** (and category's half) | **Neutral by construction.** The seam delegates to the same legs; both post-filters move ahead of the truncation and the window widens for either. The scored path is byte-identical for unscoped, uncategorised queries (the fixture's queries are both), so both tables must be *equal*, not merely within 0.005. Stage 2 reads validity columns that are always NULL today, which is what keeps it a no-op. Carries four named regression tests: the configured floor with a non-zero `search.min_similarity`, `Request.Now` bounding `AgeDays`/`DecayFactor`, `Candidates` returning strictly more rows than the current path, and `TestNegativeRetrieval` still green (the contradicts contract is unchanged). |
| 2 | `feat(mcpinit): render and apply scope on the session-start surface` | **#577** | Not applicable — injection is not scored by `ghost bench`. Gate is behavioural: the new `injection.session_scope` key defaults to unset, so the default is a no-op and the change is rendering plus one opt-in predicate. |
| 3 | `feat(memory): write validity, confidence and provenance from the tools` | **#575** | **Not neutral, and I had this wrong.** PR 1 installs stage 2, which *reads* `valid_from`/`valid_until`; the moment this PR starts writing them, that stage changes membership. So the writers and the consumption are split honestly: this PR ships the writers **and** the retrieval change together, and is bench-gated on the new validity fixtures. It is not "writers only" and the PR body must not claim to be. Stage 4's multiplier stays 1.0, so `confidence` remains unread for ranking. |
| 4 | `feat(assemble): abstention is an outcome, not an empty list` | **#580** | **Neutral by construction** — `weak` annotates, so no row is withheld. Arm A is a rank ≤ 3 gate that applies *only* when the vector leg ran; passive sources get `answerable`/`not_applicable` and never `weak`. Arm B ships disabled. Regression tests: a rank-≥4 FTS hit with `QueryVec == nil` is `answerable`/`no_vector_leg`; a rank-0 FTS hit is `answerable` with no abstention line. |
| 5 | `feat(memory): explain reports the assembler's own decisions, and `ghost context --explain` exists` | **#583** (and the class in #571) | Not applicable — explain is a read-only diagnostic and never fed a scored result. The mutation check is on the invariant test: restore the local re-derivation in `explain.go` and `explain_rrf_equals_ordering_score` must fail. Also adds the `--explain` flag to `runContext` (`cmd/ghost/session.go:18`), which today parses only `--cwd` and silently ignores everything else. |
| 6 | `feat(assemble): conflict, dedup, diversity and budget stages` | **#581** (remaining ACs) | **The gated one.** The only membership changes are the diversity quota (default off, so a no-op until #582 sets a number) and `Slice.DropDemotedLosers` for `_global`, which must *preserve* today's removal behaviour rather than converge it to reorder — so the gate is a before/after of the rendered session-start block, not a metric movement. `contradicts` is non-removing in v1, so it contributes nothing here. Paste both tables and the rendered-block diff. |
| 7 | `feat(bench): context-quality metrics` | **#582** | **Measurement only, and PR 7 keeps it that way by *not* rerouting the existing ablations.** `bench.Run` (`runner.go:45-62`) calls `store.SearchFTS` / `store.SearchVector` / `store.SearchHybrid` directly today, so the conditions are asymmetric: the two single-leg conditions bypass conflicts and dedup entirely (neither function demotes), while `hybrid` already runs `demoteSuperseded` + `demoteNearDuplicates` through `demoteResults` (`vector.go:426`/`:448`/`:460`, `demotion.go:154`). Rerouting `hybrid` through `assemble.Run` would therefore **not** newly apply stages 5 and 6 — what it would change is stage 1's widened candidate pool and stage 6's `_global`-only `DropDemotedLosers` (§5 has the per-stage table). Either way it would change the IDs, ordering and metrics, silently redefining the baseline that `regression_test.go`'s floors (`ndcg10`, `recall10`) have been measured against for the life of the suite. So the historical ablation tables **keep their direct store calls**, and #582's context mode is added as a **new** condition alongside them, with `Condition` available if a later PR wants to unify. §5's claim is scoped to the context mode alone. |

**Why this order.** #575 (3) lands the validity *writers* together with the
retrieval change they enable, because a stage that reads columns nothing writes is
a no-op that cannot be tested — and splitting them would mean PR 3 claims to be
neutral while silently changing membership. #580 (4) before #583 (5), so the
outcome is already a `Trace` field when explain starts projecting. #583 (5) before
the new stages (6), so conflicts/diversity/budget get traces for free instead of
each inventing reporting. #582 (7) last because its own AC says the metrics "only
make sense once stages 2-8 are a single pipeline".

Note what this ordering does *not* claim: PR 1 ships stage 2 **reading** validity
against columns that are still always NULL, which is provably a no-op today and is
the reason PR 1's gate is "equal tables, not within 0.005". The first PR whose
numbers can move is PR 3, and it is the one PR in the sequence that is explicitly
bench-gated on new fixtures rather than on the existing corpus.

**Not in these seven**, deliberately: `ghost_search_all`
(`SearchHybridAll`, `mcpserver.go:1077`), `ghost://project/{id}/context`
(`buildProjectContext`, `mcpserver.go:1834-1855`) and the bench corpora
(`bench/locomo`, `bench/memoryagentbench`) each have their own retrieval calls.
They migrate to `assemble.Run` after the seven land, as their own small PRs — the
seam is additive and they are not on the critical path for the defects this
spec closes.

**Where the logic lives** (required by the team rules): one function,
`assemble.Run`, with the nine stages as an ordered `[]stage` literal in
`internal/assemble/pipeline.go`, so the order is data rather than control flow
spread across nine functions. A future assembler wanting a different stage list —
an LLM reranker, a two-pass retrieve — edits one slice.

## Decision 7 — Risks and non-goals

### Risks

- **Trace cost on the hot path.** §4 mitigates by always recording and
  measuring; the fallback is dropping `Before`/`After` on non-scoring stages,
  not gating recording.
- **Making `contradicts` removing is a contract reversal, and it stays out of
  v1.** `TestNegativeRetrieval` ("contradiction stays while duplication sinks")
  and `internal/obsidian/render.go:171` both depend on the weaker member
  surviving. My first draft removed it and would have failed that test. The
  reversal may well be the right long-term design — an agent should not read a
  contradiction as a fact — but it needs its own issue, its own fixture change and
  a stated migration note, because it changes a tested guarantee rather than
  adding a filter.
- **PR 1 depends on #591 landing.** `FuseAndSelectWindow` is not on `main`, and
  the widened window is the thing PR 1 exists to consume. If #591 slips, PR 1
  slips; there is no partial version that is still worth landing, because the
  whole point of PR 1 is the un-truncated candidate set.
- **PR 1 must extend `memory.Memory`, which is a wider diff than it looks.**
  `ValidFrom`, `ValidUntil` and `VerifiedAt` do not exist on the struct and are
  not bound in `scanMemories`, so stage 2 has nothing to read until they are
  added, along with the `SELECT` lists in `store.go` and `vector.go:481-487`.
  That touches the two hot files plus a 17-destination scan, so it belongs in PR 1
  with its own mutation-checked test rather than arriving piecemeal in PR 3. The
  parsing rules above matter as much as the fields: the columns are unconstrained
  `TEXT` and already hold two different shapes, so a bare `*time.Time` scan would
  fail at runtime rather than at compile time.
- **Converging the renderer cuts both ways.** `formatMemories` gains nothing
  (it already prints scope) but must not *lose* importance or tags, and
  session-start gains a `scope{…}` suffix on every scoped row — a visible product
  change. PR 2's body carries before/after rendered blocks for both surfaces, and
  `docs/architecture.md` and the tool descriptions are updated in that PR. The
  convergence is scoped to the shared prefix precisely so this stays reviewable.
- **The `Candidates` widening changes what stage 1 returns.** Today
  `SearchHybrid` trims to `limit`; `Candidates` must not. Getting this wrong
  re-creates #573 inside the new package. The test is that `Candidates` returns
  strictly more rows than the current path for the same inputs, and PR 1's body
  shows the counts.
- **Bounding the clock changes an exported constant.** `DecayRankingSQL` is
  exported and shared with the hook's `ORDER BY`, so replacing its two
  `julianday('now')` calls with a bound parameter is a signature change to a
  shared primitive mid-migration. That is deliberate — an unbounded clock means
  the trace and the ranking can disagree, which is the failure this whole design
  exists to prevent — but it is a hot-file edit and it must land in PR 1, not
  drift into a later one.
- **Moving `category` into the pipeline is a behaviour change users can see.**
  Today the category filter is lossy over an unwidened window (#573's mechanism,
  applied to a different argument) and its zero result drops the incompleteness
  caveat. Fixing it means a previously-empty scoped-category search can start
  returning rows. That is the correct direction and still a visible change, so
  PR 1's body should show one before/after.
- **Abstention thresholds without comparability.** #580 records that the
  LongMemEval abstention subset is only comparable after #322/#561, so arm B
  ships off. If arm A alone proves insufficient, the honest response is a
  measured threshold from PR 7, not a larger default.
- **Two hot files.** `internal/memory/store.go` and
  `internal/mcpserver/mcpserver.go` are touched by PRs 1–5. Each diff in them
  stays the call-site change plus the smallest possible extraction, and each
  rebases onto `origin/main` immediately before its final push.

### Non-goals

- **Not a re-scoring engine.** The assembler orchestrates existing primitives.
  The only new scoring is stage 4's bounded multiplier, default 1.0, and it does
  not move until #575 has writers *and* a bench run justifies turning it on.
  No LLM reranking or summarization either — the pipeline has no model in it.
- **Not a tokenizer.** The budget unit is bytes, matching the hook's existing
  200/300-byte per-item and 8,000-byte content clamps.
- **Not retention tiers (#587), export/import (#586), or provenance history
  (#578).** Stage 4 is a *weight*; #578 is what would make it auditable, and
  this spec does not pre-empt it. `confidence` stays Ghost-written — no tool
  gains a `confidence=` parameter in these seven PRs.
- **Not one PR.** The big-bang rewrite is the failure mode this plan exists to
  avoid; §6 is seven independently revertible, independently bench-gated steps.
- **Not a fix for the `[ghost]` session leak (#588) or the linker scope bypass
  (#574).** Both are separate P0s with their own PRs; #574 decides which edges
  stage 5 will see and should land before PR 6.
