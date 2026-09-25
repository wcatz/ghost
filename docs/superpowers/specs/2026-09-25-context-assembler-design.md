# Context Assembler Design (with Abstention)

> **Status:** Proposed — spec only, no implementation. This document defines the
> seam for [#581](https://github.com/wcatz/ghost/issues/581) and
> [#580](https://github.com/wcatz/ghost/issues/580), and the contracts that
> [#583](https://github.com/wcatz/ghost/issues/583) and
> [#582](https://github.com/wcatz/ghost/issues/582) build on.
> **Related:** the target-design vocabulary in
> [`architecture.md`](../../architecture.md) and the current hybrid retrieval
> primitives in `internal/memory`.

## Problem

Search and session-start injection implement separate retrieval and rendering
sequences. The search surface widens the retrieval limit for category, applies
category and scope after the window has closed, returns either an unannotated
list or a generic empty string, and has no result-level verdict. The session
hook uses a small `sessionMemory` shape, ranks passive rows itself, applies a
two-pass category policy, and renders a different line format.

`memory.Memory` carries `Scope` and `Confidence`. The `valid_from`,
`valid_until`, and `verified_at` columns exist in the schema, but those fields
are not on `Memory`, are not bound by the current scan, and have no product
reader. The two surfaces therefore disagree about what a memory is, filters can
run after a candidate has been discarded, and callers cannot distinguish an
empty store from a weak result or an incomplete search. The missing boundary is
one function that decides what a context block is and records why each row was
kept or removed.

## Decision 1 — Boundary: one package, `internal/assemble`

`internal/memory` owns storage, retrieval legs, fusion, decay, schema, and
retrieval DTOs. `internal/assemble` owns selection, predicates, validity,
provenance weighting, conflicts, dedup, diversity, budget, abstention, the
trace, and the shared item renderer. `internal/mcpserver` and `internal/mcpinit`
are callers. The dependency is one-way: `internal/memory` never imports
`internal/assemble`.

The package is named `internal/assemble`, and its entry point is `assemble.Run`.
A name that collides with the standard-library `context` package is rejected
because it forces aliases in every caller. `Run` follows the repository's
existing one-entry-point convention (`bench.Run`, `mcpinit.Run`, `resolve.Run`,
and `supersede.Run`).

The alternatives are rejected: growing `internal/memory` would mix presentation
with storage and increase conflicts in hot files; a provider interface with
one implementation would add indirection without providing a second policy.

### Input and output

The following declarations are the complete public contract. `Condition` is a
memory-owned type exposed as an alias so the request and candidate request have
the same type.

```go
package assemble

type Source string
const (
    SourceSearch       Source = "search"
    SourceProjectCtx   Source = "project_context"
    SourceSessionStart Source = "session_start"
    SourceAllProjects  Source = "all_projects"
    SourceBench        Source = "bench"
)

type Outcome string
const (
    OutcomeAnswerable Outcome = "answerable"
    OutcomeWeak       Outcome = "weak"
    OutcomeEmpty      Outcome = "empty"
)

type Slice struct {
    Bucket              string
    MaxItems            int // 0 = unbounded within this slice
    MaxBytes            int // 0 = unbounded within this slice
    ClampBytes          int // 0 = no per-item presentation clamp
    DropDemotedLosers   bool // honored only for session-start
}

type Budget struct {
    MaxItems int     // 0 = unbounded total
    MaxBytes int     // 0 = unbounded total
    Slices   []Slice
}

type Condition = memory.Condition
const (
    CondHybrid     = memory.CondHybrid
    CondFTSOnly    = memory.CondFTSOnly
    CondVectorOnly = memory.CondVectorOnly
)

type Request struct {
    ProjectID string
    Query     string            // empty selects passive mode
    QueryVec  []float32         // required unless CondFTSOnly
    Scope     map[string]string
    Category  string
    Source    Source
    Budget    Budget
    Condition Condition
    Params    *memory.SearchParams // unresolved caller params; nil uses defaults
    Now       time.Time            // required and used end to end
    Explain   bool
}

type Result struct {
    Items   []Item
    Outcome Outcome
    Reason  string
    Notes   []string
    Trace   *Trace
    Bytes   int
}
```

`Slice` is a per-bucket budget; `Budget` also has a total because search applies
one limit across project and `_global`, while injection has independent
project and global caps. A zero budget is rejected rather than treated as an
implicit unbounded request. Stage 8 applies slice caps first and the total
second. `MaxItems: 0` is unbounded only within the scope named by the field;
callers set every total and slice budget explicitly.

`Item` is the shared output type for rendering, explanation, and bench metrics.
It carries the fields needed to reproduce both existing renderers and to
measure the axes named in Decision 5.

```go
type Item struct {
    ID, Category, Content string
    Tags                  []string
    Importance            float64
    Pinned                bool
    CreatedAt             time.Time
    Bytes                 int
    Bucket                string
    ProjectID             string
    Scope                 map[string]string
    ResolvedAt            *time.Time
    ValidFrom, ValidUntil *time.Time
    VerifiedAt            *time.Time
    Confidence            *float64
    Agent                 string
    Score                 float64
}
```

The four timestamps on `Item` are parsed output values. A validity value that
cannot be parsed is `nil` on `Item` and is recorded as
`validity_unparseable`; nil therefore means no readable validity claim, not a
validity assertion.

### Project and source validation

`memory.ProjectMode` is the storage-level distinction required by the SQL
shape:

```go
type ProjectMode string
const (
    ProjectScoped ProjectMode = "scoped"        // project_id = ? OR '_global'
    GlobalOnly    ProjectMode = "global_only"   // project_id = '_global'
    AllProjects   ProjectMode = "all_projects"  // no project predicate
)
```

`Run` validates the source, project mode, query mode, condition, query vector,
clock, and non-zero budget before calling `Candidates`.

| Source | `ProjectID` | `Query` | Retrieval mode |
|---|---|---|---|
| `SourceSearch` | required by the tool; an unresolved name may become `""` | required by the tool | `ProjectScoped`; empty resolved ID preserves today's globals-only result |
| `SourceProjectCtx` | required non-empty | empty | `ProjectScoped` |
| `SourceSessionStart` | non-empty or `""` | empty | `ProjectScoped`, or `GlobalOnly` when empty |
| `SourceAllProjects` | ignored, may be empty | optional | `AllProjects` |
| `SourceBench` | non-empty, or empty for an all-projects condition | optional | follows the condition |

A projectless session start is a supported rendered state: global memories are
loaded on a separate path and are still shown when no project matches. The
empty project ID therefore means `GlobalOnly` only for session start and
`AllProjects` for the all-projects source. Search's unknown-project fallback is
preserved rather than converted to an error; the test suite covers that
behavior. The inconsistency with update and delete remains outside this seam.

### Retriever contract and DTOs

The assembler depends on one method, not on `*sql.DB`:

```go
type Retriever interface {
    Candidates(context.Context, memory.CandidateRequest) (*memory.CandidateSet, error)
}

func Run(context.Context, Retriever, Request) (Result, error)
```

The DTOs are declared in `internal/memory`, because a method implemented by
`*memory.Store` cannot name a type from a package that imports memory. This
preserves the one-way boundary without a leaf DTO package.

```go
// package memory
type Condition string
const (
    CondHybrid     Condition = "hybrid"
    CondFTSOnly    Condition = "fts_only"
    CondVectorOnly Condition = "vector_only"
)

type CandidateRequest struct {
    ProjectID string
    Mode      ProjectMode
    Query     string
    QueryVec  []float32
    Scope     map[string]string
    Category  string
    Condition Condition
    Params    SearchParams // unresolved; the Store applies its vector floor
    Now       time.Time
    Fetch     Fetch
    Passive   []SlicePolicy
}

type Fetch struct {
    FTSTopK, VectorTopK int
    Limit               int // Candidates returns a wider untrimmed set
}

type SlicePolicy struct {
    Bucket             string
    Order              string // decay or pinned_importance_updated
    TwoPass            bool
    BehaviorFloor      int
    BehaviorCategories []string
    CategoryWeights    map[string]float64
    CategoryCaps       map[string]int
    OverFetch          int
    DemotionThreshold  float64
    DropDemotedLosers  bool
}

type CandidateSet struct {
    Rows        []Candidate
    Edges       []LinkEdge
    EdgesStatus EdgeStatus
    Legs        map[string]LegStatus
    Widened     bool
}

type Candidate struct {
    Memory
    FTSRank, VectorRank int
    VectorScore         float64
    Base, Decay, Score  float64
    AgeDays             float64
}

type LinkEdge struct { From, To, Relation string; Strength float64 }
type LegStatus struct {
    Attempted, Available bool
    Err                  string
    Truncated            bool
    Applicable           bool
    Indexed, Eligible    int
    DimMismatch          int
}
type EdgeStatus struct { Status, Err string } // ok, unavailable, or err
```

`Run` maps its request to `CandidateRequest` without changing the caller's
semantics:

- `SourceSessionStart` with an empty project maps to `GlobalOnly`; the
  all-projects source maps to `AllProjects`; other sources map to
  `ProjectScoped`.
- `Now` is passed unchanged. The candidate path never calls the wall clock.
- `Condition` selects the legs. `CondVectorOnly` runs the vector leg alone.
- `Fetch.Limit` is the requested window. `Candidates` returns a wider,
  untrimmed set for stages 2–8 to filter before stage 8 closes the window.
- `Passive` is populated only for an empty query. Each policy carries its own
  bucket width and selection rules.
- `Params` contains the caller's unresolved search parameters. `Candidates`
  applies the Store's configured vector floor immediately before vector
  filtering, so a non-zero configured floor cannot be bypassed.

`mcpserver` stores a `provider.MemoryStore` interface rather than
`*memory.Store`. The migration uses one capability assertion equivalent to
`resolveCapableStore`; an unsupported provider returns a structured error.
`provider.MemoryStore` is not widened for this feature.

### Validity, snapshots, and read handles

The schema already stores `valid_from`, `valid_until`, and `verified_at`, but
`Memory` and its scan path do not expose them. PR 1 adds the three `*string`
fields to `Memory`, binds them with `sql.NullString`, and adds them to the
relevant SELECT lists. The fields are declared only on `Memory`; `Candidate`
embeds `Memory` and does not redeclare them. A reflection test rejects a
future field-name collision.

SQLite stores these values as unconstrained text. Stage 2 parses the layouts
`2006-01-02 15:04:05` and `2006-01-02`; an unparseable value is treated as
unset and recorded as `validity_unparseable`, rather than failing retrieval or
silently becoming valid.

`Candidates` executes the legs, hydration, edge load, and penalty lookups
through one read transaction. Existing methods that receive a `*sql.DB` must
accept a queryer so the transaction is used directly; otherwise a transaction
would hold the Store's single connection while those methods wait for another
connection. The refactor includes `SearchHybridAll` and the passive global
query so the all-projects path has the same snapshot guarantee. The transaction
contains no embedding, LLM, or rendering work.

`OpenReadDB` is the single read-only database constructor, matching `OpenDB`.
`NewStoreWithRead(db, readDB, logger)` injects the read handle. `Store` holds
an optional `readDB` and selects it for `Candidates` when present. The
constructor name and hook wiring are in PR 1 because the one-snapshot contract
applies to session-start retrieval; the hook's existing read-only `Store` is
migrated to `OpenReadDB` plus `NewStoreWithRead`, with no separate read-only
Store constructor.

Production file stores use a second handle without `_txlock=immediate`, so the
transaction is read-only. A `NewStore` without a read handle uses its primary
handle and logs a warning for a file-backed store because the production DSN
issues `BEGIN IMMEDIATE` and takes a write lock. `OpenReadDB` rejects
`:memory:` because a second connection cannot see a private in-memory
database. Bench and unit tests therefore use the original handle and execute
through the transaction; the deferred-read guarantee is limited to file
stores. Those tests and bench runs are short-lived and have no concurrent
writer.

Edge lookup failure is non-fatal and explicit. `EdgesStatus == "ok"` means
stages 5 and 6 ran; `"err"` leaves the edges empty, marks both stages skipped
with `edges_unavailable`, and adds the error to notes; `"unavailable"` means a
successful query returned no edges. `Candidates` returns an error only when
all retrieval paths fail, never an empty set that could be mistaken for a
successful negative result.

## Decision 2 — The nine stages

The order is fixed. Every filter and every decision that can affect membership
runs before the final window closure.

```text
Request
  ├─1 retrieve     Candidates() -> widened, untrimmed rows
  ├─2 validity     drop expired or not-yet-valid rows
  ├─3 predicates   project, category, and scope verdicts
  ├─4 provenance   bounded multiplier
  ├─5 conflicts    supersede reorders; contradicts is recorded
  ├─6 dedup        duplicate and near-duplicate ordering
  ├─7 diversity    per-bucket quota
  ├─8 budget       final order and hard trim
  ├─9 render       shared item rendering
  └─ outcome       answerable | weak | empty
```

| Stage | Existing behavior | Assembler behavior |
|---|---|---|
| 1 retrieve | Search legs and passive policies are separate | `memory.Candidates` returns a wider set, scoring facts, statuses, and edges; it supports query and passive modes |
| 2 validity | columns are not exposed by `Memory` | parse and drop `valid_until < Now` or `valid_from > Now`; `verified_at` is a flag only |
| 3 predicates | category and scope post-filter the closed window | apply category and scope before closure; project membership remains in SQL and is recorded as a per-row verdict, not a second filter |
| 4 provenance | no consumer | ship the stage with multiplier `1.0`; define NULL and non-NULL confidence semantics before any multiplier changes |
| 5 conflicts | supersede and demotion helpers exist | supersede reorders; `contradicts` pairs are recorded and not acted on in v1; `elaborates` is non-removing |
| 6 dedup | demotion helpers exist | duplicate and near-duplicate edges reorder; session-start global slices retain their explicit drop policy |
| 7 diversity | none | per-bucket quota, off by default until measured |
| 8 budget | separate search and hook trims | UTF-8 item clamp plus whole-item byte/count trim, with total and slice caps |
| 9 render | two renderers | one `Item.Line()` for the shared item prefix; each surface keeps its framing and field order |

### Stage 1: query and passive retrieval

Query mode combines FTS and vector retrieval through
`FuseAndSelectWindow`, retains the discarded tail, and returns decay ordering
and the facts needed by the trace. The configured vector floor is applied by
`Candidates`; the caller's `Now` is used for age and decay. The shared
`DecayRankingSQL` path receives a bound `Now` parameter, so ranking and trace
timestamps cannot use different instants.

Passive mode is not `GetTopMemories`. The hook has three policies that the
candidate path must reproduce:

| Bucket | Fetch and order | Selection and cap | Near-duplicate policy |
|---|---|---|---|
| project | `resolved_at IS NULL`, over-fetch `sessionMemoriesCap*3` (45), `DecayRankingSQL` then importance/time/id | two-pass behavioral floor with category weights and caps, then decay fill; cap 15 | configured `linking.demotion_threshold`, default `0.90`; project relies on the cap |
| `_global` | `resolved_at IS NULL`, over-fetch `globalsCap*2` (16), pinned/importance/updated order | no decay or two-pass selection; cap 8 | threshold `0.85`; global losers are removed |

`injection.behavior_categories` is explicit in `SlicePolicy`; it cannot be
inferred from `CategoryWeights`, which is nil by default. `DemotionThreshold`
is populated per bucket from `linking.demotion_threshold` and the global
`0.85` policy. The global order and threshold are separate policies, not
accidental variations of project ranking. The passive paths are therefore
specification of existing behavior, not a redesign. PR 2 verifies the
rendered block before changing rendering.

### Stages 2–4: validity and provenance

Stage 2 drops a row when either validity boundary is outside the request
clock. A null boundary is unset. An unparseable value is not a valid claim
and is surfaced in the trace. Stage 3 applies category and `ScopeMatches` over
the widened set. Project membership is enforced by the SQL legs; stage 3
records the project verdict for every row but does not turn an unexpected
bucket into a second drop predicate.

Stage 4 is present but inert in v1. `Confidence` is writable through the store
and may be non-NULL in existing rows, so the contract distinguishes NULL from
a recorded value while the multiplier remains `1.0`. `agent` is recorded on
save and is not independently scored. A seeded confidence value must produce
the same order while the multiplier is pinned.

### Stages 5–7: conflicts, dedup, and diversity

**Conflicts: supersede reorders; `contradicts` is recorded, not acted on, in
v1.** The existing negative-retrieval contract requires a contradicted row to
survive while a duplicate restatement sinks, and the Obsidian renderer emits
the contradiction relation. Removing the weaker endpoint would reverse a
tested product contract. The trace records co-occurring contradiction pairs
without changing membership or order. PR 6 therefore adds no contradiction
fixture; any removing behavior requires a separate contract change and
migration.

Near-duplicate handling is a reorder, not a second storage fold. The policy is
scoped by source before bucket:

| Source | Bucket | `DropDemotedLosers` |
|---|---|---|
| `SourceSearch` | project and `_global` | false; search reorders |
| `SourceSessionStart` | `_global` | true; the hook removes global losers |
| `SourceSessionStart` | project | false; the cap performs the drop |
| `SourceProjectCtx` | project and `_global` | false |
| `SourceAllProjects`, `SourceBench` | all | false |

A setting outside the session-start policy is a configuration error. The
trace records the collapse set, including rows removed by the global policy;
an all-global dedup case is the regression test for `all_dedup_dropped`.
Diversity is a per-bucket quota, not MMR. It is a membership change when
enabled, remains off by default, and receives its own bench comparison.

### Stages 8–9: budget and rendering

`Slice.ClampBytes` is a presentation clamp that preserves UTF-8 boundaries.
`Slice.MaxBytes` and `MaxItems` are hard membership trims. Stage 8 applies
slice caps and then the total. Stage 9 shares the item line's scope label and
quote escaping, while preserving the distinct search and session-start
framing and field order. Session-start output may gain a scope label for a row
that already carries scope; that visible change is intentional.

## Decision 3 — Abstention

The outcome is derived from the trace and admitted rows, never from a second
query. `Candidates` errors are returned as errors. Otherwise the rules are:

```text
QUERY MODE
  stage 1 produced no candidates                         -> empty
  candidates existed but stage 8 admitted none            -> empty
  at least one admitted row satisfies a floor arm          -> answerable
  admitted rows satisfy no floor arm                       -> weak

PASSIVE MODE
  stage 1 produced no rows                                -> empty/no_memories
  rows existed but stage 8 admitted none                  -> empty/stage reason
  at least one row admitted                               -> answerable/not_applicable
```

Passive retrieval is windowed, so `no_memories` describes an empty over-fetched
set, not proof that a store has no memories. Passive sources make no relevance
claim and never receive `weak`.

### Reasons

The empty reason set is closed. The first matching stage supplies the reason;
`window_exhausted` is a note modifier rather than a peer reason.

| Reason | Stage | Meaning |
|---|---:|---|
| `no_candidates` | 1 | query retrieval returned no candidates over applicable, complete coverage |
| `retrieval_failed` | 1 | all usable paths failed and no rows survived |
| `vector_backend_unavailable` | 1 | an applicable vector leg could not run |
| `no_memories` | 1 | passive retrieval returned no rows |
| `all_invalid` | 2 | every row was expired or not yet valid |
| `all_out_of_category` | 3 | every row failed category |
| `all_out_of_scope` | 3 | every row failed scope |
| `all_dedup_dropped` | 6 | every row was removed by a source policy |
| `all_diversity_capped` | 7 | every row was cut by a diversity quota |
| `all_over_budget` | 8 | every row was cut by the hard budget |

There is no `all_out_of_project` reason: project membership is enforced in
SQL. There is no `all_resolved` reason: query mode deliberately admits resolved
rows, while passive fetch excludes them in SQL. Supersede and dedup do not
empty a set except through the named global drop policy.

### Floor arms

Arm A is the keyword arm and is enabled by default. A row satisfies it when
`0 <= FTSRank && FTSRank <= 3`; `-1` means the keyword leg did not retrieve the
row. The floor is based on BM25 rank, not RRF score, because rank remains
meaningful when retrieval depth changes.

Arm B is the vector arm and is disabled by default:
`VectorScore >= cfg.Context.AbstainCosine`, with a default threshold of `0.0`.
PR 4 introduces the key; PR 7 supplies a measured threshold when comparable
LongMemEval data is available.

The vector-leg attempt, not its result count, determines whether Arm A is a
floor:

```text
vector leg Attempted, whether or not it returned neighbours -> both arms apply
vector leg Attempted == false                              -> no_vector_leg
```

When the vector leg was not attempted, any admitted row is answerable with
`no_vector_leg`; a keyword hit is never labelled `below_floor` merely because
an embedder was unavailable. When the vector leg was attempted and returned no
neighbours, both arms still apply and `below_floor` remains reachable. A
partial leg failure suppresses `below_floor` because no floor verdict can be
made from the failed path; survivors are answerable with a
`retrieval_partial` note. A rank-0 FTS hit is always answerable when the
vector leg was not applicable or did not attempt.

Regression coverage includes an empty explicit `CondFTSOnly` request yielding
`no_candidates` rather than `vector_backend_unavailable`, a rank-4 FTS hit with
no attempted vector leg yielding `answerable`/`no_vector_leg`, and a
fault-injected FTS error with vector survivors yielding `answerable` with a
`retrieval_partial` note and no `below_floor` reason.

`weak` annotates the returned items and abstention line. It withholds no row,
which keeps the outcome decision orthogonal to ranking and bench results.

### Output and absence

The result remains an ordinary text payload. Search adds one trailing machine
line:

```text
- [gotcha] `A1B2…` (0.9) «session injection bypasses the debug build»
[ghost:outcome=weak reason=below_floor floor_fts_rank=3 abstain_cosine=off candidates=7 admitted=2]
```

The machine line includes applicable leg status and any `retrieval_partial`
modifier. `abstain_cosine=off` distinguishes a disabled threshold from a zero
cosine result. A human sentence is shown for `weak`; passive session-start
blocks remain free of a relevance banner. Empty copy is reason-specific:

| Reason | Copy principle |
|---|---|
| `no_candidates` | may say that no stored memory matches, only under complete applicable-leg coverage |
| `no_memories` | describes the empty passive window, not store-wide absence |
| `all_invalid` | says the found rows were withheld as out of date |
| `all_dedup_dropped`, `all_diversity_capped`, `all_over_budget` | identifies the limit and suggests raising it |
| `retrieval_failed` | explicitly says search was incomplete |
| `vector_backend_unavailable` | says keyword-only retrieval may be less complete |
| category/scope reasons | names the filter that excluded the rows |

A bounded search always receives the single note:

```text
Ghost memory: no match within the searched window — widen the limit or drop the
scope filter. This is not evidence that nothing exists.
```

The note suppresses an absence claim. `LegStatus` makes absence possible only
when every applicable leg is available, error-free, and untruncated, with
index coverage and eligible counts sufficient for the selected mode. A vector
leg that skips dimension-mismatched rows or cannot see unembedded rows is not
complete coverage. A non-applicable leg is not a failure. Partial failure is
reported in notes and the machine payload rather than converted into a
relevance verdict.

The existing `maybeIncomplete` caveat moves into this single note path. A
bounded result, a failed leg, and an unscoped store must not all render as
“nothing exists.”

## Decision 4 — Explainability: the trace is the explain payload

`internal/memory/explain.go` currently retrieves membership and recomputes
ranking facts locally. The assembler removes that second implementation:
`Candidate` facts are copied into the trace while they exist, and
`SearchExplain` becomes a projection of the trace.

```go
type Trace struct {
    Mode      string
    Signals   map[string]Signals
    Stages    []StageTrace
    Decisions []Decision
    Floors    Floors
}

type Signals struct {
    FTSRank, VectorRank int
    VectorScore         float64
    Base, DecayFactor   float64
    AgeDays             float64
    CreatedAt           time.Time
}

type StageTrace struct {
    Stage      string
    In, Out    int
    DroppedIDs []string
    Reordered  bool
    Notes      []string
}

type Decision struct {
    ID, Stage, Reason, AgainstID string
    Kept                          bool
    Before, After                 float64
}
```

The trace is always recorded. Only the explain projection is gated by
`Request.Explain`, because recording is bounded and a flag-dependent second
ranking path would reintroduce drift. `Signals` replaces re-derived RRF, decay,
age, and rank values. `StageTrace` supplies per-stage counts and dropped IDs;
`Decision` supplies per-row attribution; `Floors` records the exact threshold
values.

The projection has three invariants:

1. `ExplainRow` contains no independent ranking arithmetic; its score is the
   score carried by the corresponding `Item` and decisions.
2. The scope decision in the trace is the only scope filter.
3. Recording is unconditional; the explain-row allocation is conditional.

The payload is capped at 32KB. Complete rows are removed from the lowest-ranked
end and the payload records `truncated.dropped_rows` and
`truncated.reason=payload_budget`; rows are never cut mid-item. The migration
adapter preserves the existing explain fields and reads supersede and
near-duplicate penalties from the conflict and dedup decisions.

`ghost context --explain` does not exist in the current command parser. PR 5
adds the flag and the trace-to-payload projection. Until then, `Explain: true`
on `ghost_memory_search` is the only trace surface.

## Decision 5 — The metrics hook for #582

Bench calls `assemble.Run` with `SourceBench` and `Explain: true` for a new
context-quality condition. Existing FTS-only, vector-only, and hybrid tables
retain their direct store calls: `SearchFTS` and `SearchVector` return from
SQL, while every `SearchHybridParams` exit runs `demoteSuperseded` and
`demoteNearDuplicates`. The conditions are therefore intentionally asymmetric;
routing them through the assembler would redefine existing regression floors.

Rerouting hybrid through the assembler has these effects:

| Stage | Effect on hybrid |
|---|---|
| 1 retrieve | changes IDs and order because the candidate pool is wider than today's closed window |
| 2 validity | no effect on the current corpus; validity fixtures make it a membership change |
| 3 predicates | no effect without category or scope predicates |
| 4 provenance | no effect while the multiplier is `1.0` |
| 5 conflicts | widens the set on which supersede reordering occurs; a reorder before the trim can change final membership |
| 6 dedup | has the same wider-reorder effect; `DropDemotedLosers` is false for search |
| 7 diversity | no effect while off |
| 8 budget | matches only when the bench request sets the current total limit explicitly |
| 9 render | no metric effect |

The bench request therefore sets its current limit. A nil or all-zero budget is
an error, not a way to compare an unbounded tail with a trimmed baseline.
`Condition` remains in the contract because a nil query vector means FTS-only,
not vector-only, and zeroing FTS weight still leaves fused FTS candidates in
the pool.

The six context metrics are:

| Metric | Computation | Contract |
|---|---|---|
| Context precision | `sum(relevance(Item.ID)) / len(Items)` | scored over admitted items only |
| Contamination rate | any contamination predicate over admitted items | disjunction of the five arms below |
| Budget adherence | `Result.Bytes` and stage-8 `DroppedIDs` against the matching slice | exposes relevant rows discarded by a trim |
| Diversity | `max_b count(Item.Bucket) / len(Items)` | `_global` is its own bucket |
| Result rate | `count(Outcome != empty) / count(queries)` | reported separately so abstention cannot look like quality |
| Token cost | `sum(Item.Bytes)` per answered question | bytes, matching existing content and injection budgets |

Empty results contribute to result rate only. They are excluded from precision,
contamination, and diversity rather than counted as zero. Ratios report their
numerator and denominator; an undefined ratio is `n/a` and is never averaged.
A non-empty block with no relevant item remains a genuine zero precision.

Contamination is the following disjunction, evaluated over admitted items:

```go
Item.ResolvedAt != nil ||
ExpiredAt(Item.ValidUntil, req.Now) ||
NotYetValidAt(Item.ValidFrom, req.Now) ||
ScopeContradicts(Item.Scope, req.Scope) ||
BucketUnexpected(Item.Bucket, req.ProjectID)
```

`_global` is never contamination because both legs and the passive global
path deliberately include it. Project membership remains in SQL, so
`BucketUnexpected` is a metric predicate only and is not a stage-3 drop
condition. The production stages and the metric share leaf definitions, but
the metric composes them independently:

```go
func ExpiredAt(*time.Time, time.Time) bool
func NotYetValidAt(*time.Time, time.Time) bool
func ScopeContradicts(map[string]string, map[string]string) bool
func BucketUnexpected(string, string) bool
```

Stage 2 uses the two validity leaves. Stage 3 uses `ScopeContradicts` only;
project buckets are already constrained by SQL. The metric composes all four
leaves with `ResolvedAt` so a leaked row remains measurable. `Decision.Reason`
corroborates a leak; it is not the source of the metric because exclusion
reasons describe rows that were removed.

The contamination fixture contains four rows: `future_scheduled`, `expired_policy`,
`valid_window_open`, and `valid_current`. The first two must be withheld and,
if admitted, classified as contaminated; the last two must be admitted and
unflagged. The open-window row prevents an over-broad “any validity column”
predicate, while the all-null row prevents an always-flag predicate. Removing
the not-yet-valid arm must fail the fixture.

## Decision 6 — Migration

Seven independently revertible PRs implement the contract. The current
`FuseAndSelectWindow` primitive is the fusion boundary, and the built-in bench
fixture has 220 scored queries. Ranking-affecting work runs the bench delta
gate against `origin/main`; the gate is NDCG@10 and
R@5 within `0.005`, with every difference explained. PR 1 uses that same numeric
gate because the widened candidate pool can move IDs and order. PR 3 uses new
validity fixtures because the existing corpus does not exercise stage 2.

| # | Branch / title | Closes | Bench expectation |
|---:|---|---|---|
| 1 | `feat(assemble): internal/assemble seam; scope and category filter before window closure` | #573 | Run origin/main and branch; stay within 0.005 on NDCG@10 and R@5, explaining any diff. Cover the configured vector floor, bound `Now`, widened rows, and existing negative retrieval. |
| 2 | `feat(mcpinit): render and apply scope on the session-start surface` | #577 | Rendered block comparison; selection and 15/8 caps unchanged, with scope labels appearing on scoped rows. |
| 3 | `feat(memory): write validity and provenance from the tools` | #575 | New validity fixtures; run the same delta gate because writers activate stage 2 and change membership. |
| 4 | `feat(assemble): abstention is an outcome, not an empty list` | #580 | Weak results annotate rather than withhold; run the delta gate. Arm B is initially disabled. |
| 5 | `feat(memory): explain reports the assembler's decisions and `ghost context --explain` exists` | #583 | No scored-result change; mutation-test the no-recomputation invariant and add the CLI flag. |
| 6 | `feat(assemble): conflict, dedup, diversity and budget stages` | #581 | Run the delta gate; verify the global drop policy and rendered session-start block. |
| 7 | `feat(bench): context-quality metrics` | #582 | Add context mode beside existing direct-call ablations; do not redefine their baseline. |

The order is deliberate: PR 1 establishes the seam and the transaction/read
contract; PR 3 ships validity writers with the stage that consumes them; PR 4
establishes outcome and trace fields before PR 5 projects them; PR 6 adds the
remaining membership stages; PR 7 adds metrics after the pipeline is complete.
`ghost_search_all`, project-context, and the bench corpora migrate later as
separate changes.

The ordered stages are implemented as one `[]stage` in
`internal/assemble/pipeline.go`; `assemble.Run` owns orchestration. A future
LLM reranker or different retrieval pass changes that list rather than scattering
ranking logic through callers.

## Decision 7 — Risks and non-goals

### Risks

- **Trace cost:** always-on recording is bounded; if the benchmark exceeds the
  agreed cost, reduce non-scoring fields before considering conditional
  recording.
- **Snapshot contention:** the read transaction must stay short and read-only.
  File stores use the injected read handle; in-memory bench stores accept the
  `BEGIN IMMEDIATE` limitation because they have no concurrent writer.
- **Clock drift:** `DecayRankingSQL` receives the bound request clock; ranking
  and trace timestamps must use the same instant.
- **Validity fields:** the schema permits non-NULL values, so corpus-wide
  neutrality is not assumed. PR 1 adds fields and scan bindings; PR 3 gates
  the writer on fixtures.
- **Category behavior:** moving category before closure can turn a previously
  empty result into a correct match; PR 1 records the before/after.
- **Renderer convergence:** the shared item line must preserve importance,
  tags, and each surface's framing while adding scope labels where needed.
- **Demotion and trimming:** supersede and dedup reordering is not membership
  neutral when a wider pool is trimmed later. This is why the hybrid baseline
  is not rerouted.
- **Abstention thresholds:** Arm B remains off until measured data supports a
  threshold; an unmeasured default is not introduced.
- **Hot files:** `internal/memory/store.go`,
  `internal/mcpserver/mcpserver.go`, `internal/mcpinit/hook.go`, and
  `internal/mcpserver/mcpserver.go` call sites stay surgical and are rebased
  onto current `origin/main` before final push.

### Non-goals

- No LLM reranking, summarization, or model call in the assembler.
- No tokenizer-based budget; bytes remain the unit.
- No retention tiers (#587), export/import (#586), or provenance history (#578).
- No new `confidence=` product-tool parameter in these seven PRs.
- No big-bang rewrite and no change to the session-leak or linker-scope issues.
- No automatic unification of existing bench ablations; context metrics are an
  additional condition until a separately gated change proves equivalence.
