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

## Decision 1 — Boundary: one new package, `internal/context`

`internal/memory` keeps **storage and the retrieval legs**. A new
`internal/context` owns **selection, penalties, budget, abstention, the trace,
and the shared item renderer**. `internal/mcpserver` and `internal/mcpinit`
become callers. The layering is one-way — `internal/memory` must not import
`internal/context`.

```text
internal/memory    primitives: SearchFTS, SearchVector, FuseAndSelectWindow,
                   DecayFactor, DecayRankingSQL, ScopeMatches,
                   SupersedePenalties, DemotionPenalties, StableDemote, schema
internal/context   orchestration: the nine stages, the trace, Item, Budget,
                   the outcome, the shared item renderer
callers            mcpinit (session_start), mcpserver (search, project
                   context, search_all), bench
```

Those primitives are **already exported** — `DemotionPenalties`,
`SupersedePenalties`, `StableDemote`, `DecayFactor` and `DecayRankingSQL` were
deliberately made shareable so injection and search could agree. This design
finishes that move rather than inventing a new axis.

### Alternatives considered

**(a) New `internal/context` package — chosen.**

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

### Input / output

```go
package context

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
}
type Budget []Slice // first matching Bucket wins; "*" is the fallback

type Request struct {
    ProjectID string               // "" only with SourceAllProjects
    Query     string               // "" = passive mode, see §2 stage 1
    Scope     map[string]string    // nil = no scope predicate
    Category  string               // "" = any; stage 3 membership, never post-closure
    Source    Source
    Budget    Budget
    QueryVec  []float32            // nil → FTS-only; the caller embeds
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
    ResolvedAt, ValidFrom, ValidUntil, VerifiedAt *time.Time
    Confidence            *float64
    Agent                 string
    Score                 float64   // final, after every scoring stage
}
```

### The hook's read-only constraint is load-bearing

`internal/mcpinit/hook.go:400-406` is explicit: the hook queries its own
read-only `*sql.DB` and **deliberately does not depend on `Store`**. If
`Assemble` took a `*memory.Store`, that comment would become a lie and the hook
would acquire a read-write handle it has no business holding. So `Assemble` takes
a narrow interface, not the store:

```go
// Retriever is what the assembler needs from storage. memory.Store satisfies it;
// memory.NewReadOnly(ro *sql.DB) satisfies it for the hook.
type Retriever interface {
    Candidates(ctx context.Context, q CandidateRequest) (*CandidateSet, error)
}

func Assemble(ctx context.Context, r Retriever, req Request) (Result, error)
```

`CandidateSet` is the widened set stages 2 onward filter over, and it carries
more than rows. Stages 5–6 read link edges while `SupersedePenalties` and
`DemotionPenalties` need a `*sql.DB` the assembler never holds, and today's legs
*discard* individual errors (`SearchHybridParams` swallows a failing leg as
non-fatal, `vector.go:442/454`) — so "both legs failed" and "the vector backend
was never configured" are currently indistinguishable, which is the distinction
#580 asks for:

```go
type CandidateSet struct {
    Rows    []Candidate          // hydrated, fused, decay-ordered, untrimmed
    Edges   []LinkEdge           // active edges whose BOTH endpoints are in Rows
    Legs    map[string]LegStatus // "fts","vector": attempted/available/err/truncated
    Widened bool                 // the fetch window was full — see §3
}

type Candidate struct {
    memory.Memory                    // the hydrated row
    FTSRank, VectorRank   int        // -1 when that leg did not retrieve it
    VectorScore           float64    // cosine; -1 when absent
    Base, Decay, Score    float64    // fused base, DecayFactor, base×decay
    AgeDays               float64
}
type LinkEdge struct{ From, To, Relation string; Strength float64 }
type LegStatus struct{ Attempted, Available bool; Err string; Truncated bool }
```

Returning `Edges` from the same call keeps `Retriever` a one-method interface —
nothing in `internal/context` can reach a `*sql.DB` — at the cost of one batched
query instead of a round trip per stage. `Candidates` returns an **error** when
every retrieval path failed, rather than an empty set that reads as "no memory
resembles this".

`memory.Store` gets a `Candidates` method wrapping the existing
`SearchFTS`/`SearchVector`/`filterVectorFloor`/`FuseAndSelectWindow`/`decayRank`
calls and returning the window **plus** the discarded tail; the hook gets a
read-only variant over its own handle. Four consequences:

- **Scoring stays inside `memory`.** `decayRank` (`vector.go:332`) is
  unexported and stays that way; `internal/context` never calls it. Stage 1's
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
  unconditional, and the regression test is that two `Assemble` calls with
  different `Now` values and identical data produce different `AgeDays` and
  `DecayFactor` and identical `Base` scores.

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
| 1 | retrieve | yes, but query-only and clock-unbound | `SearchHybridParams` `vector.go:439`; `fuseAndRank:400`; `decayRank:332`; `FuseAndSelectWindow` (#591); passive rank path `GetTopMemories` `store.go:1097` + `DecayRankingSQL:1083` | **Keep all of it in `memory`.** New `Candidates` returns the widened set *with* scoring already applied and `Request.Now` honoured (see the four consequences above). **Two modes**: `Request.Query != ""` is the hybrid path; `Request.Query == ""` is the passive path, which is what session-start and project-context need and what no current unified function serves. |
| 2 | validity | **no** | columns `valid_from`/`valid_until`/`verified_at` are inert (#575) | **New.** `valid_until < now` → drop; `valid_from > now` → drop; both NULL → valid. `verified_at` NULL sets an `unverified` *flag only*, no penalty in v1 (see the last decision below). |
| 3 | predicates (project, category, scope) | half | `memory.ScopeMatches` is already an exported pure predicate; the *loops* are `mcpserver.go:627-639` (category) and `652-663` (scope) | **Move both loops** into stage 3, over the widened set, and **carry `Request.Category` into `CandidateRequest`** so the SQL legs can over-fetch on it. Neither `SearchFTS` nor `SearchVector` accepts a category today, so a `Candidates` that ignored it would either drop `ghost_memory_search`'s category filter or reintroduce the post-closure defect #573 found in the scope one. Project membership stays in SQL (both legs already do `project_id = ? OR '_global'`); the stage records all three verdicts per row. |
| 4 | provenance | **no consumer** | the column *is* writable — `Store.Create` persists `Memory.Confidence` (`store.go:767`) and `UpsertWithOptions` persists `Provenance.Confidence` (`store.go:1019`, `1056`), and a test stores 0.9 — but no **product tool** sets it (#575), and `agent` is written by `ghost_memory_save` and read by nothing | **New stage, identity by default.** Ship the stage and the bounded multiplier with the multiplier pinned to 1.0 until #575 has writers. Note the column is not *empty* on every store: rows written by import, by a direct `Store.Create` caller, or by a test can already carry a value, so stage 4 must define what a non-NULL `confidence` means **before** it is allowed to move any score — the test for that is a seeded row with `confidence = 0.1` that must rank identically to `NULL` while the multiplier is 1.0. |
| 5 | conflicts | partially | `demoteSuperseded` `vector.go:262` + `SupersedePenalties` `demotion.go:106`; `contradicts` appears **only** in the schema CHECK (`schema.go:240`, `migrate.go:273`) — no code writes or reads it | **Move the reorder** into stage 5, fed from `CandidateSet.Edges`. **Add** the contradicts-pair rule, gated on a fixture that actually contains such edges (see below). |
| 6 | dedup | yes | `demoteNearDuplicates` `demotion.go:163` + `DemotionPenalties:34` + `StableDemote:90` | **Move** into stage 6, fed from `CandidateSet.Edges`. Reorder stays membership-preserving; the trace records the collapse set. |
| 7 | diversity | **no** | — | **New**, default no-op (§ below). |
| 8 | budget | fragmented | `args.Limit` `mcpserver.go:671`; `truncateUTF8(200/300)` `hook.go:491`; `sessionMemoriesCap=15` (`hook.go:323`); `globalsCap=8` (`hook.go:311`) | **New.** Two mechanisms, not one: a UTF-8-safe per-item content **clamp** (`Slice.ClampBytes`, the existing `truncateUTF8` behaviour) and a whole-item **hard trim** (`Slice.MaxBytes`/`MaxItems`). Conflating them was wrong — the clamp is presentation, the trim is membership. |
| 9 | render | twice | `formatMemories` `mcpserver.go:2156`; `formatSessionContext` `hook.go:198`; `scopeLabel:2190`; `quoteData:2214` | **Converge the item line only.** `scopeLabel` and `quoteData` move to `internal/context` and one `Item.Line()` renders them; each surface keeps its own framing *and* its own field order, because search prints `id`/`importance`/`pin`/`tags` and injection prints a bare bullet. A shared prefix of the two, not a shared whole line. |


### Four stage-level decisions worth defending

**Conflicts: supersede reorders, `contradicts` removes.** Different operations,
kept different. A superseded row is still *true history* — `demoteSuperseded`
already sinks it under its replacement, and making that a removal would hide the
audit trail the axes section calls for. A `contradicts` pair is two incompatible
assertions; emitting both means the agent reads a contradiction as a fact. So
supersede → reorder (existing behaviour, moved), contradicts → drop the
weaker-ranked endpoint and record `AgainstID`, `elaborates` → group (parent and
child are both wanted), group recorded so #583 can attribute it.

**…and the contradicts rule is currently unmeasurable, which the spec says
rather than papers over.** `contradicts` appears in exactly two places in the
tree: the schema CHECK (`schema.go:240`, `migrate.go:273`). No code writes it, no
code reads it. The built-in bench fixture creates no `memory_links` at all, and
neither do the LongMemEval and LoCoMo runners — they ingest via `Store.Create`,
and the product classifier emits `supersedes` and `causes`. A rule that fires on
contradicts is therefore provably a no-op on every corpus Ghost can currently
score, and PR 7 cannot measure it either. PR 6 must **add a targeted fixture** to
the built-in dataset (two memories, one `contradicts` edge, a graded relevance
for each) and show the rule firing on it; without that fixture the rule ships
with an explicit "unmeasured on every available corpus" caveat, not a bench claim.

**Dedup: reorder, not fold.** `Upsert` already folds the *storage*; a second
fold at assembly time would make "which row won and why" unanswerable — #583's
entire subject — and would change the ID set the graded bench scores. Reorder
plus a recorded collapse set gives #582 its duplicate count without changing
membership.

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
      reason: no_candidates | vector_backend_unavailable
   candidates existed, 0 reached stage 8                   → empty
      reason: all_expired | all_out_of_scope | all_demoted | window_exhausted
   ≥1 reached stage 8, ≥1 satisfies a floor arm            → answerable
   ≥1 reached stage 8,  0 satisfies any floor arm          → weak
      reason: below_floor (with the floor values applied)
PASSIVE MODE (Request.Query == "" — session_start, project_context)
   0 rows admitted                                         → empty,  reason: no_memories
   ≥1 row admitted                                         → answerable, reason: not_applicable
```

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

**Arm A — keyword, on by default.** `FTSRank <= 3`. The floor is on the *FTS
leg's rank*, not an RRF score: RRF values here are 0.005–0.01 (`FuseAndSelectWindow`
does this arithmetic in its own doc comment) and are a function of *leg depth*,
not relevance, so an absolute RRF floor moves every time the retrieval width
changes while BM25 rank is stable and means what it says. The default `3` is
deliberately narrow — a rank-0 exact match can never be withheld, which is the
regression test #580 asks for and the same class of hole #543/#591 just fixed
from the other direction.

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
Ghost memory: no sufficiently relevant memory for this question. Do not rely on
prior context for it; save what you learn with ghost_memory_save.
```

The `empty` sentence **replaces** the bare `No matching memories found.`
(`mcpserver.go:678`) and is byte-bounded by the same `Slice.MaxBytes` as the
items — #580's last AC. **Session-start prints neither**, for the same reason the
passive rule has no `weak`: passive rows make no relevance claim to qualify, and
a "this might be wrong" banner on every session start trains the agent to ignore
it. The hook's empty block stays *absent*, not apologetic; the outcome lives in
`Trace`, and the block's existing "N of M shown" line is where a partial block
announces itself.

The trace is inspected via a `ghost context --explain` flag that **does not exist
today**: `runContext` (`cmd/ghost/session.go:18-30`) parses only `--cwd` and
silently ignores anything else, so the diagnostic this section promises would
otherwise be a documentation of a command that quietly does nothing. Adding the
flag is part of migration PR 5, alongside the `Trace` → payload projection. Until
it lands, `explain:true` on `ghost_memory_search` is the only trace surface.

### Honest absence (#573)

`empty` and a *bounded* search are different claims. When stage 1's widened set
was itself truncated (`CandidateSet.Widened == true`) and stages 2–3 then emptied
it, absence is not provable: the reason becomes `window_exhausted`, rendered as
*"no match within the searched window — widen limit or drop the scope filter"*
rather than *"no memory exists"*. `LegStatus.Truncated` distinguishes "the
backend answered" from "the backend was cut off", and a failing leg is reported
as a `Note` rather than silently narrowing the set — the current
`SearchHybridParams` (`vector.go:442/454`) discards a leg error, which is how
#580's "FTS-only hits must not be reported as below-floor" requirement currently
has nowhere to be expressed. The existing `maybeIncomplete` caveat
(`mcpserver.go:674-676`, 685+) moves into `Notes` and is rendered from one place,
so #573's zero-result half and its non-empty half can no longer disagree.

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

**Migration of `ExplainSearch`.** A thin adapter: `Assemble` with
`Explain: true`, then project `Trace` → `SearchExplain`. Every existing field
(`FTSRank`, `VectorRank`, `VectorScore`, `RRFScore`, `DecayFactor`, `AgeDays`,
`SupersedePenalty`, `NearDuplicatePenalty`, `Reason`) is preserved — #583
requires an additive change. `SupersedePenalty` and `NearDuplicatePenalty` are
read off the stage-5/6 `Decision`s, which is *more* faithful than today's
post-hoc re-query: the re-query sees only penalties among the final window,
while the stage saw the whole candidate set.

## Decision 5 — The metrics hook for #582

`#582` needs five numbers. All five are computable from `Result` + `Trace` with
**zero bench-side retrieval code**, because bench calls the same `Assemble` with
`Source: SourceBench, Explain: true`.

| Metric | Computed from | Note |
|---|---|---|
| **Context precision** | `Σ relevance(Item.ID) / len(Items)` | bench's existing `Query.Rel` grades. The denominator is the *assembled* block, not a ranked list — that is the whole difference from NDCG. |
| **Contamination rate** | items whose own `Item` fields are contaminating *at assembly time* | `Item.ResolvedAt != nil`, `Item.ValidUntil < Now`, `Item.Scope` contradicts the request, or `Item.Bucket` is **neither the requested project nor `_global`**. `_global` is *never* contamination: both retrieval legs and `GetTopMemories` include it deliberately (`store.go:1106`, `1165`), so a global preference in the block is the product working, and scoring it as contamination would report correct behaviour as a regression. **Classified from the production exclusion codes in `Decision.Reason`**, not a bench-only re-implementation — #582's second AC. |
| **Budget adherence** | `Result.Bytes` vs. the matching `Slice.MaxBytes`; dropped IDs from the stage-8 `StageTrace.DroppedIDs` | `DroppedIDs` is what lets bench intersect the trim with `Query.Rel` and measure *relevant rows the trim discarded* — the "low-relevance memory crowding out a relevant one" regression, otherwise invisible. |
| **Diversity** | `max_b count(Item.Bucket) / len(Items)` | One histogram over `Item.Bucket`, with `_global` as its own bucket. |
| **Token cost** | `Σ Item.Bytes` per answered question, reported next to accuracy | Bytes, not a tokenizer: the injection budget is already in bytes and `MaxContentLen` is 8,000 bytes. A tokenizer would be a second, disagreeing notion of size. |

**The design constraint this imposes on `Item`:** contamination is a property of
an *included* row, so `Item` must carry `Scope`, `ResolvedAt`, `ValidUntil`,
`ProjectID` and `Bucket` — the same fields the renderer prints. A content-only
`Item` would force #582 to re-query the store and drift from the product, which
is why §1 defines it with the axis fields. Reported, not gated, until two
independent changes have been measured — #582's own rule.

## Decision 6 — Migration

Seven PRs, each shippable alone, each naming its issue and its bench
expectation. The bench delta gate (origin/main vs. branch, NDCG@10 and R@5
within 0.005) applies to PRs 1, 4 and 6; PRs 2, 3, 5 and 7 cannot move ranking
and say so with the reason. **PR 1 is additionally blocked on #591**, since the
widened window it returns does not exist on `main`.

| # | Branch / title | Closes | Bench expectation |
|---|---|---|---|
| 1 | `feat(context): internal/context seam; scope and category filter before window closure` | **#573** (and category's half) | **Neutral by construction.** The seam delegates to the same legs; both post-filters move ahead of the truncation and the window widens for either. The scored path is byte-identical for unscoped, uncategorised queries (the fixture's queries are both), so both tables must be *equal*, not merely within 0.005. Carries three named regression tests: the configured floor with a non-zero `search.min_similarity`, `Request.Now` bounding `AgeDays`/`DecayFactor`, and `Candidates` returning strictly more rows than the current path. |
| 2 | `feat(mcpinit): render and apply scope on the session-start surface` | **#577** | Not applicable — injection is not scored by `ghost bench`. Gate is behavioural: the new `injection.session_scope` key defaults to unset, so the default is a no-op and the change is rendering plus one opt-in predicate. |
| 3 | `feat(memory): write validity, confidence and provenance from the tools` | **#575** | **Neutral.** Writers only. Nothing reads `confidence` for ranking until PR 6's stage 4 is enabled, which it is not. Includes `Item`/`formatMemories` gaining the fields so they are *visible* before they are *weighted*. |
| 4 | `feat(context): abstention is an outcome, not an empty list` | **#580** | **Neutral by construction** — `weak` annotates, so no row is withheld. Arm A is a rank ≤ 3 gate that applies *only* when the vector leg ran; passive sources get `answerable`/`not_applicable` and never `weak`. Arm B ships disabled. Regression tests: a rank-≥4 FTS hit with `QueryVec == nil` is `answerable`/`no_vector_leg`; a rank-0 FTS hit is `answerable` with no abstention line. |
| 5 | `feat(memory): explain reports the assembler's own decisions, and `ghost context --explain` exists` | **#583** (and the class in #571) | Not applicable — explain is a read-only diagnostic and never fed a scored result. The mutation check is on the invariant test: restore the local re-derivation in `explain.go` and `explain_rrf_equals_ordering_score` must fail. Also adds the `--explain` flag to `runContext` (`cmd/ghost/session.go:18`), which today parses only `--cwd` and silently ignores everything else. |
| 6 | `feat(context): conflict, dedup, diversity and budget stages` | **#581** (remaining ACs) | **At risk — the gated one, and partly unmeasurable.** The `contradicts` rule is the only membership change, and no corpus Ghost can score today contains such an edge (§2). So PR 6 must **first add a contradicts fixture** to the built-in dataset, show the rule firing on it, and only then claim neutrality on the rest. The diversity quota defaults to off. Paste both tables and the fixture diff. |
| 7 | `feat(bench): context-quality metrics` | **#582** | Not applicable — measurement only, reported not gating. This is the PR that sets `context.abstain_cosine` from the abstention subset, with the before/after in the body. It cannot validate the contradicts rule either: LongMemEval and LoCoMo ingest via `Store.Create` and create no links, so PR 6's fixture is the only contradicts evidence that will exist. |

**Why this order.** #575 (3) before the stages it feeds (2, 4), or those stages
are no-ops that cannot be tested. #580 (4) before #583 (5), so the outcome is
already a `Trace` field when explain starts projecting. #583 (5) before the new
stages (6), so conflicts/diversity/budget get traces for free instead of each
inventing reporting. #582 (7) last because its own AC says the metrics "only
make sense once stages 2-8 are a single pipeline".

**Not in these seven**, deliberately: `ghost_search_all`
(`SearchHybridAll`, `mcpserver.go:1077`), `ghost://project/{id}/context`
(`buildProjectContext`, `mcpserver.go:1834-1855`) and the bench corpora
(`bench/locomo`, `bench/memoryagentbench`) each have their own retrieval calls.
They migrate to `Assemble` after the seven land, as their own small PRs — the
seam is additive and they are not on the critical path for the defects this
spec closes.

**Where the logic lives** (required by the team rules): one function,
`context.Assemble`, with the nine stages as an ordered `[]stage` literal in
`internal/context/pipeline.go`, so the order is data rather than control flow
spread across nine functions. A future assembler wanting a different stage list —
an LLM reranker, a two-pass retrieve — edits one slice.

## Decision 7 — Risks and non-goals

### Risks

- **Trace cost on the hot path.** §4 mitigates by always recording and
  measuring; the fallback is dropping `Before`/`After` on non-scoring stages,
  not gating recording.
- **The `contradicts` rule is unmeasurable today, and shipping it as if it were
  measured would be the worst outcome.** `contradicts` exists only in the schema
  CHECK; no code writes or reads it, the built-in fixture creates no
  `memory_links`, and neither do the LongMemEval or LoCoMo runners. A rule that
  can never fire is indistinguishable from a rule that is wrong. The mitigation
  is PR 6's targeted fixture (§2) and an explicit "no corpus evidence" line in
  the PR body if that fixture is deferred. The alternative — shipping the rule
  with a synthetic unit test and a clean bench table — would let a green gate
  vouch for behaviour no gate has observed.
- **PR 1 depends on #591 landing.** `FuseAndSelectWindow` is not on `main`, and
  the widened window is the thing PR 1 exists to consume. If #591 slips, PR 1
  slips; there is no partial version that is still worth landing, because the
  whole point of PR 1 is the un-truncated candidate set.
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
