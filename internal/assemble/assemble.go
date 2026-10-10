// Package assemble owns what a context block is: selection, validity,
// predicates, provenance, conflicts, dedup, diversity, budget, abstention and
// the shared item renderer. internal/memory owns storage, retrieval legs, fusion
// and decay; this package owns every decision made after retrieval returns.
//
// The dependency is one-way. internal/memory never imports this package, which
// is why the retriever DTOs are declared there: a method implemented by
// *memory.Store cannot name a type from a package that imports memory.
//
// One entry point, Run, follows the repository's existing convention (bench.Run,
// mcpinit.Run, resolve.Run, supersede.Run). The ordered stages are one []stage
// in pipeline.go, so a future reranker or a different retrieval pass changes
// that list rather than scattering ranking logic through callers.
package assemble

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// Source names the surface a request came from. It decides the project mode,
// the budget and which near-duplicate policy applies — the two surfaces
// disagree about all three, which is why the request cannot leave it implicit.
type Source string

const (
	SourceSearch       Source = "search"
	SourceProjectCtx   Source = "project_context"
	SourceSessionStart Source = "session_start"
	SourceAllProjects  Source = "all_projects"
	SourceBench        Source = "bench"
	// SourceWorkingMoment is a block delivered at the moment of work: next to a
	// user message or to an edit's tool result, by a host hook. It is its own
	// source so the audit counts it apart from a session start and a search.
	SourceWorkingMoment Source = "working_moment"
)

// Outcome is the relevance verdict for a result. It is derived from the trace
// and the admitted rows, never from a second query.
type Outcome string

const (
	OutcomeAnswerable Outcome = "answerable"
	OutcomeWeak       Outcome = "weak"
	OutcomeEmpty      Outcome = "empty"
)

// Slice is a per-bucket membership budget. MaxBytes bounds item content and
// never includes response framing. It bounds the RAW, pre-fold content: the
// line-break fold (Data, #911) is a display step after selection, so a rendered
// line can differ in length from the bytes the budget counted. The same holds
// for the agent and source_ref display caps (MaxRenderedAgentLen,
// MaxRenderedSourceRefLen), which are applied to the raw value before folding.
//
// The second half of the struct is the RETRIEVAL policy for the bucket, and it
// is here rather than in a caller because a passive retrieval has no query to
// shape it: the store cannot know how deep to fetch, in what order, or which
// categories are behavioural unless the budget says. A passive slice that
// states no OverFetch and no cap bounds nothing and is refused, because a
// passive fetch runs on every session start and an unbounded one is a store
// scan.
type Slice struct {
	Bucket     string
	MaxItems   int // 0 = unbounded within this slice
	MaxBytes   int // item-content bytes; 0 = unbounded within this slice
	ClampBytes int // 0 = no per-item presentation clamp
	// DropDemotedLosers asks the RETRIEVER to remove a near-duplicate loser rather
	// than rank it last. It is honoured for any PASSIVE request — passive is keyed
	// on the absence of a query, not on Source, so a source that arrives without
	// one is passive whatever it calls itself.
	//
	// INERT on a query-mode request, and that is worth knowing rather than
	// discovering: a query's policies are never sent (passivePolicies returns nil
	// for a non-empty query) and the fusion path only reorders, so a slice that
	// sets this on a search request changes nothing. Stage 6 reads the flag to
	// describe the note, and is gated on the passive shape for the same reason —
	// otherwise it would report a removal the retriever never performed.
	DropDemotedLosers bool
	// DemoteOnlyWhenOverCap gates the retriever's near-duplicate demotion on the
	// selected set being wider than MaxItems. A demotion is a REORDER, so on a set
	// that fits under the cap it only shuffles rows the answer shows in full, and
	// the shipped session-start loaders skip it there.
	DemoteOnlyWhenOverCap bool

	// OverFetch is how many rows the passive fetch reads for this bucket before
	// any selection. It is the window: a policy's own bound, not the caller's
	// item budget, because the selection stages below run over what it returns.
	// 0 means "the same as this slice's MaxItems", which is the query-mode
	// reading of the field and the only one that shape has.
	OverFetch int
	// Order is the passive SQL order: "decay" (the composite score, then
	// importance, created_at and id) or "pinned_importance_updated". The two
	// buckets disagree, and the disagreement is policy rather than drift.
	Order string
	// IncludeGlobal admits `_global` rows into this bucket's read, so the cap on
	// MaxItems is a cap over the UNION rather than over the project alone. It is
	// what every whole-project listing reads and what two slices cannot express:
	// a project slice at 20 plus a `_global` slice at 20 admits 40 rows where the
	// caller asked for 20.
	//
	// False by default, which is the shipped session-start shape — those two
	// buckets are disjoint on purpose. A request that mixes `_global` into one
	// bucket and fetches it in another is refused (validatePassiveBudget): the
	// rows would come back twice.
	IncludeGlobal bool
	// TwoPass enables the behavioral reservation: BehaviorFloor slots are filled
	// from BehaviorCategories first, ordered by score × CategoryWeights and
	// capped per category by CategoryCaps, and the rest of the window is filled
	// from every candidate by plain score.
	TwoPass            bool
	BehaviorFloor      int
	BehaviorCategories []string
	CategoryWeights    map[string]float64
	CategoryCaps       map[string]int
	// DemotionThreshold is the near-duplicate similarity above which a row is a
	// loser. It is per bucket because the buckets genuinely differ: the global
	// policy is lower, and one number for both would be one bucket's threshold
	// silently applied to the other. Zero — the Go zero value, and what a caller
	// that states none sends — means "use the store's configured
	// linking.demotion_threshold", NOT "treat every edge as a near-duplicate": the
	// retriever binds this as `strength >= ?`, so a literal zero would demote over
	// a 0.1 edge and, on a bucket that drops losers, delete the row outright.
	DemotionThreshold float64
}

// Budget is what a caller will accept. MaxItems is the total across buckets;
// Slices are the per-bucket caps, so search applies one limit across project
// and `_global` while injection applies independent caps. A budget that bounds
// neither rows nor bytes is rejected: an unbounded block is not a request, it is
// an omission. A MaxBytes cap alone is bounded, so it is honoured — the
// retrieval window falls back to its documented ceiling, stage 9 still trims by
// the per-slice item caps, and Run's response-fit post-pass brings the complete
// rendered response inside MaxBytes. MaxBytes is the RESPONSE's bytes, never
// item content: Slice.MaxBytes is the item-content cap, and applying one field to
// two units would make the effective limit depend on a caller's framing.
//
// A budget that bounds nothing at all, including one whose slices carry only a
// ClampBytes presentation cap, is refused before this is reached.
type Budget struct {
	MaxItems      int // 0 = unbounded total
	MaxBytes      int // complete response bytes; 0 = unbounded total
	MaxNoteBytes  int // per-note maximum; 0 = use the documented default
	MaxNotesBytes int // total notes maximum; 0 = use the documented default
	Slices        []Slice
	// Measure is the caller's OWN render of the block, for a surface whose
	// delivered bytes are not Result.Response (session start frames its block
	// itself). When set together with MaxBytes, the response-fit post-pass
	// measures MaxBytes against Measure(items, trace) instead of the search
	// envelope, so the cap bounds the bytes the caller actually emits and each
	// cut is recorded in this run's trace and retrieval record like any other
	// stage-8 decision. The trace passed in already holds the cuts made so far.
	// Nil keeps the search envelope. Measure must be pure and cheap: it runs at
	// least once per dropped row and may be called more than once per drop. The
	// items slice may be EMPTY (nil) — that is how the caller's framing alone is
	// measured — so Measure must not assume a row.
	Measure func(items []Item, trace *Trace) int
	// FramingCeiling, with Measure, is the size at which cutting rows cannot help:
	// when the framing alone (Measure of no rows) is at or over it, the rows are
	// kept rather than all cut for nothing. Below it, rows are cut down to none if
	// that is what it takes to reach MaxBytes. 0 means rows are always cut.
	FramingCeiling int
	// KeepPinned makes the response-fit post-pass cut the lowest-ranked UNPINNED
	// row first, and a pinned row only when none other is left. Without it the
	// pass cuts the bottom of the ranking, which is not where a pinned row is
	// guaranteed to be (a behavioral reservation and a supersede reorder both
	// place unpinned rows above it).
	KeepPinned bool
}

// Condition is the retrieval condition. It is an alias so a request and the
// candidate request it maps to cannot disagree about the type.
type Condition = memory.Condition

const (
	CondHybrid     = memory.CondHybrid
	CondFTSOnly    = memory.CondFTSOnly
	CondVectorOnly = memory.CondVectorOnly
)

// Request is one assembled context block. Now is required and is used end to
// end: the candidate path never reads the wall clock, so a row's age, its
// validity and the trace describing it cannot disagree about what time it was.
type Request struct {
	ProjectID string
	Query     string    // empty selects passive mode
	QueryVec  []float32 // nil skips the vector leg; required for CondVectorOnly
	Scope     map[string]string
	Category  string
	// Retention is the tier filter (memory's session|project|persistent). Empty is
	// no filter, and the two are the same thing here: a filter is applied to the
	// widened candidate set before the window closes, exactly as Category is, so
	// a row of the requested tier the window cut is still reachable.
	Retention string
	Source    Source
	Budget    Budget
	Condition Condition
	Params    *memory.SearchParams // unresolved caller params; nil uses defaults
	Now       time.Time            // required and used end to end
	// AsOf asks for the block as it stood at that instant, and it is a binding
	// rather than a filter: Run moves Now to it, so every time-dependent decision
	// downstream — a row's age, its validity window, the trace that describes it —
	// is made against T rather than against the wall clock. Binding it here is
	// what keeps a historical block self-consistent; passing T down beside Now and
	// hoping each stage picks the right one is the failure this prevents.
	//
	// nil is a current read, which is the default and the only shape a caller
	// before #647 could produce. A request that names an instant cannot also ask
	// for vector-only retrieval, because an embedding records the text a memory
	// holds now; the store refuses that combination rather than answering it with
	// the keyword leg.
	AsOf *time.Time
	// AbstainCosine is the caller-resolved cfg.Context.AbstainCosine: the
	// vector arm's floor, and 0 for OFF. It ships off because a threshold
	// nobody measured is a relevance verdict, and the bench no-answer report
	// shows the answerable and no-answer cosine distributions overlap.
	AbstainCosine float32
	// RelevanceCutoff is the caller-resolved cfg.Context.RelevanceCutoff: the
	// relative-to-top cutoff the assembler applies to a QUERY-mode block after
	// dedup and before the budget (#954). Once a row's fused Base falls below
	// this fraction of the top row's Base (never the age-decayed Score), that row
	// is dropped with its own reason (§954). 0 — the value every
	// caller sends until a default is chosen — leaves it OFF, and a passive
	// request ignores it whatever its value, because a digest is not a relevance
	// answer. It can only SHORTEN a block: the top row is always kept, `limit`
	// stays the maximum, and a pinned or keyword-reserved row is never cut.
	//
	// It is validated against the same [0,1] range a fraction occupies, where 0
	// is off and 1 keeps only rows that tie the top. See the stage's own comment
	// in pipeline.go for the rule and for why it sits where it does.
	RelevanceCutoff float64
	// NoAnswerCosine is the caller-resolved cfg.Context.NoAnswerCosine: the
	// absolute bar the assembler applies to a QUERY-mode block after the relevance
	// cutoff and before the budget (#955). When the best vector cosine among the
	// block's rows is strictly below it, the block is withheld and the answer says
	// nothing cleared the bar. 0 leaves it OFF, and a passive request ignores it
	// whatever its value. A pinned row is never withheld, a block with no
	// comparable cosine (no vector leg) is never judged, and it only removes rows.
	// It is validated against a cosine's [0,1] range. See runNoAnswer.
	NoAnswerCosine float64
	// NoAnswerBarNote, when set with NoAnswerCosine 0, says why the configured bar
	// is off (the embedding model is not the one the default was measured on). It
	// is recorded in the trace only and never changes a block.
	NoAnswerBarNote string
	// Explain asks Run to project the trace of THIS run into Result.Explain, the
	// ghost_memory_search explain payload. It is a request for a second reading
	// of the same run and never for a second run: the candidate request, the
	// stages, the budget and the response are the ones the same Request would get
	// with Explain false, so the explanation describes the answer the caller would
	// have been given rather than a neighbouring search. The one thing it asks of
	// the retriever is to record the facts that decided the ranking
	// (memory.CandidateRequest.Explain), which changes what the retriever RETURNS
	// and nothing about how it ranks, scopes or windows.
	//
	// It is refused with AsOf (a historical read ranks nothing, so there is no
	// ranking to explain) and without a query (a passive retrieval scores no
	// candidate). Run writes no retrieval record for it, because the record
	// counts answers delivered to a caller and an explanation is not one; see
	// docs/invariants.md.
	Explain bool
	// Record receives one retrieval record per SUCCESSFUL Run: the query as a
	// digest, the ids this call judged, and each one's kept/dropped verdict with
	// the stage and reason the stages gave it (#646).
	//
	// nil is the ordinary case — the bench, every test in this package, and any
	// caller that wants no audit — and it is checked per Request rather than
	// through a package global, so two Runs cannot record into each other's
	// store. A failure to record never fails the search; see emit in record.go.
	Record RecordSink
	// SuppressRecordWhenLegsFailed states that this caller returns a result with
	// a FAILED leg and nothing admitted as an ERROR rather than as an answer, so
	// such a call is not a retrieval the audit should count.
	//
	// It exists because "a successful Run" is not the same as "a call the caller
	// received an answer for", and the difference is a surface's to make: the MCP
	// handler turns exactly that case into a retryable error so an agent never
	// reads "nothing matched" as a fact. A record written anyway would put a row
	// in the audit's denominator for a call that returned no memories at all —
	// the same error TestNoRecordIsWrittenForACallThatReturnedNothing exists to
	// prevent, one layer out, where Run cannot see it.
	//
	// false (the default) records such a result normally, which is right for a
	// caller that renders it. The caller keeps its own predicate; all this states
	// is which of Run's results that caller is going to convert.
	SuppressRecordWhenLegsFailed bool
	// Logger receives this Run's own diagnostics — today, only a record that could
	// not be written — and nil is the process default.
	//
	// It is on the Request because NO Ghost process configures the default: a
	// repo-wide search finds slog.SetDefault only in tests, in `ghost bench`, and
	// in the two bench mains, all of which install a DISCARD handler. `ghost mcp`
	// builds its own logger in bootstrap and passes it around, so a diagnostic
	// sent to slog.Default() from a live server reaches a handler nobody reads —
	// which would make "a failed record is logged, not silent" true only in tests.
	Logger *slog.Logger
	// SessionID names the caller's session, and reaches nothing but the record.
	// It is on the Request rather than stamped by a decorator in the caller so
	// the row is built in exactly one place — a record assembled from two
	// packages is a record whose fields can disagree about which call it is.
	//
	// Empty over the stdio transport Ghost ships, whose connection reports no
	// session; that is why Request.Source is a first-class column of the record
	// rather than something a reader infers from this being empty.
	SessionID string
}

// Result is the assembled block. Run owns the whole response — listing, verdict
// sentence, filter caveat, diagnostics and the machine line — so Response is the
// text a caller returns verbatim and Bytes is its length, never 0 on a successful
// Run.
//
// The framing is the SEARCH framing, for every Source, because that is the only
// one this version knows: nothing here branches on Source. A caller whose surface
// frames differently must not read Response, and must leave Budget.MaxBytes at 0
// until it supplies a render of its own (Budget.Measure) — otherwise the
// post-pass measures this envelope against a budget that was stated for another
// one, and drops rows against a cap the caller never described. Session-start is
// the passive surface (#581): it frames its own block around Line() and states
// its row bounds per bucket, and it states its BYTE bound through Budget.Measure,
// so the cap is on the bytes the host receives and not on this envelope.
type Result struct {
	Items   []Item
	Outcome Outcome
	Reason  string
	Notes   []string
	// Qualifiers are the statements that change what the ANSWER MEANS rather than
	// what it contains: that it is a historical read, that a leg did not run, that
	// some memories have no recorded version to place. Response already renders
	// them, leading the block, because they are bytes the caller receives and the
	// response-fit post-pass has to measure them to cap it honestly — a surface
	// that prepended its own copy would ship text the cap never saw. The field is
	// what a caller reads when it needs the statements apart from the answer.
	// Notes cannot carry them: Notes is a diagnostic list a surface shows for the
	// empty case and bounds from the end, where a qualifier is the first to go.
	Qualifiers []string
	Trace      *Trace
	// Explain is the explain payload, a projection of Trace and of the retriever's
	// recorded ranking facts for this one run. nil unless Request.Explain was set.
	Explain *memory.SearchExplain
	Bytes   int // complete rendered response, including framing and outcome
	// Abstention is the human sentence for a non-answerable result: what the
	// caller should do about it. "" for an answerable one, which withholds
	// nothing and needs no caveat.
	Abstention string
	// Machine is the machine-readable verdict line, one trailing line of the
	// response. It is a field rather than something the caller composes so the
	// response-fit post-pass can measure the line it is trimming to fit.
	Machine string
	// Response is the complete rendered text for the search surface, which is
	// the framing this version owns. Bytes is its length.
	Response string
	// Tokens is the token ESTIMATE for the admitted rows: bytes/4, rounded up
	// per item. Bytes remain the budget unit (there is no tokenizer here), so
	// this is reported for the caller's own budgeting and is never a cap.
	Tokens int
}

// ErrResponseBudgetExceeded is returned when even the empty envelope exceeds
// Budget.MaxBytes. It is an error rather than an outcome: no rows and a
// response that cannot be rendered are different facts, and reporting the
// second as `empty` would claim the store had nothing to say.
var ErrResponseBudgetExceeded = errors.New("response budget exceeded")

// Retriever is the one method this package depends on. It is an interface so
// the assembler never holds a *sql.DB, and so a test can supply a candidate set
// without a database.
type Retriever interface {
	Candidates(context.Context, memory.CandidateRequest) (*memory.CandidateSet, error)
}

// predicateFetchWiden is the multiple applied to the retrieval window when a
// predicate is present. A predicate is applied before the window closes, so it
// needs room beyond the caller's limit for a matching row to be reached from —
// the same widening the tool used to apply by hand before this seam existed.
// Without a predicate the window is the caller's limit, so a plain search
// retrieves and returns exactly what it always did.
//
// It is named for the predicate rather than for the category because the tier
// filter uses the same widening for the same reason: a session row ranked just
// below the cut is exactly the row a `retention: session` search was asking for,
// and a narrower window would answer the request with a silence.
const predicateFetchWiden = 3

// maxRetrievalWindow bounds how far a predicate's widening may go. The tool
// clamped its category fetch at 100 rows, and the ceiling is kept deliberately:
// reachability past the window comes from the retriever's discarded tail, so a
// deeper window buys rows that are hydrated, edge-loaded and then trimmed away.
// The cost is real — three times the hydration, and three times the edge
// queries, on a live tool surface — while the answer is unchanged. It bounds
// the widening only: a caller whose own budget exceeds it still gets a window
// that can fill that budget.
const maxRetrievalWindow = 100

// legDepthFactor is how deep each leg fetches relative to the window. Two is
// the historical production depth: it is what gives fusion a pool to rank
// inside rather than a list already trimmed to the answer.
const legDepthFactor = 2

// Run assembles one context block.
//
// The order is fixed and every filter that can affect membership runs before
// the final window closure: retrieve, validity, predicates, provenance,
// conflicts, dedup, diversity, budget, render. A retrieval failure is returned
// as an error rather than an empty outcome, because "the leg failed" and
// "nothing matched" must never render the same way.
func Run(ctx context.Context, r Retriever, req Request) (Result, error) {
	if r == nil {
		return Result{}, errors.New("assemble: a retriever is required")
	}
	if err := validateRequest(req); err != nil {
		return Result{}, err
	}
	// The historical binding, in one place and before anything reads a clock. A
	// stage that decided on Now and a trace that reported on the wall clock would
	// describe two different days, and nothing downstream could tell.
	if req.AsOf != nil {
		req.Now = *req.AsOf
	}
	set, err := r.Candidates(ctx, candidateRequest(req))
	if err != nil {
		return Result{}, err
	}

	p := &pipeline{
		req:            req,
		mode:           projectMode(req),
		set:            set,
		trace:          newTrace(req, set),
		passive:        Passive(req),
		rows:           set.Rows,
		droppedBy:      map[string]int{},
		droppedByBound: map[string]int{},
		dropped:        map[string]string{},
		qualifiers:     qualifiersFor(req, set),
	}
	p.loadExactCosines(ctx, r)
	// A window that had to fall back to the ceiling is disclosed, because the
	// block's size is then decided by a number the caller did not state. The note
	// also says what does bound the block, and that half is derived from the
	// request rather than asserted: a total byte cap bounds every row, while a
	// per-slice byte cap bounds only the buckets it names — leaving a row in a
	// bucket with no slice bounded by nothing but this window.
	if windowIsACeiling(req) {
		bound := "a row in a bucket with no slice is bounded by nothing but this window"
		if req.Budget.MaxBytes > 0 {
			bound = "the block's own size is bounded by the byte cap, not by a row count"
		} else if len(req.Budget.Slices) > 0 {
			bound = "the block is bounded only by the byte caps on the slices you named, and " + bound
		}
		note := formatNote(
			"retrieval_window_capped: this budget states no item bound, so the retrieval window is the documented ceiling of %d rows; %s",
			maxRetrievalWindow, bound)
		p.retrievalFailures = append(p.retrievalFailures, note)
		// The trace gets it too. Trace.Limit reports the window, so a consumer
		// building a projection from the trace — explain, which reports this as a payload note — would
		// otherwise read a 100 with nothing beside it and no way to tell it from a
		// caller's 100.
		p.windowDisclosure = note
	}

	// A leg that errored is named before the stages run, so every later
	// statement about the result carries the reason it is partial. `retrieval_
	// <leg> failed` is the note form Decision 3's reasons are drawn from.
	for _, name := range []string{"fts", "vector"} {
		leg := set.Legs[name]
		if leg.Applicable && leg.Attempted && !leg.Available {
			p.retrievalFailures = append(p.retrievalFailures, formatNote("retrieval_%s leg failed (%s): this search is incomplete, so an empty result does not mean nothing matched", name, leg.Err))
		}
	}
	for _, st := range stages {
		st.run(p)
	}

	// The verdict and the complete envelope are produced together, after the
	// stages, because the outcome is a function of what they admitted and the
	// envelope is measured from the outcome. The post-pass fitResponse runs is
	// what brings that envelope inside a byte cap, and it needs both.
	//
	// Qualifiers (#683) are carried through untouched: they state what the answer
	// MEANS, where Outcome and Reason are derived here. fitResponse sets both of
	// those itself, which is why this no longer calls outcome()/reason() —
	// outcome.go's verdict() supersedes both.
	res, err := p.fitResponse(Result{
		Items:      p.items,
		Trace:      p.trace,
		Notes:      p.notes(),
		Qualifiers: p.qualifiers,
	})
	// AFTER fitResponse, and only when it succeeded (#646). The post-pass drops
	// rows from the bottom of the ranking and records each as a `response_fit`
	// decision, so a record written before it would name a row the caller never
	// received as kept — a false "used" verdict, in the direction that flatters
	// Ghost. And the error paths write nothing: a retrieval that failed, a
	// request Run refused and an envelope that cannot fit all return an error
	// instead of a Result, and a record for any of them would put a denominator
	// in the audit for calls that returned no memories at all.
	//
	// SuppressRecordWhenLegsFailed covers the one case Run cannot see from
	// inside: a result that is perfectly valid, and that the CALLER then turns
	// into an error because a leg failed and nothing was admitted. Recording that
	// would count a call that delivered no answer.
	//
	// An explain run writes nothing. The record counts retrievals a caller was
	// ANSWERED with — it is the audit's denominator — and an explanation is a
	// diagnostic of one, so counting it would put calls that delivered no answer
	// in that denominator. It matches what explain did before it became a
	// projection of this run: it never reached the sink.
	if err == nil && req.Explain {
		res.Explain = p.explainProjection(res)
	}
	if err == nil && !req.Explain && !req.convertsToError(res) {
		emit(ctx, req.Record, req, res)
	}
	return res, err
}

// convertsToError reports whether this caller will turn this result into an
// error instead of returning it — the one case where a successful Run did not
// reach the caller as an answer.
//
// It mirrors the MCP handler's rule rather than sharing its helper on purpose:
// the handler's failedLegs is also what it puts in the error message, and tying
// a message to a recording decision would couple two things that should be able
// to differ. The two agree today, and this side is deliberately the weaker: a
// failed leg with rows ADMITTED is still an answer, degraded, and the handler
// returns it.
func (r Request) convertsToError(res Result) bool {
	if !r.SuppressRecordWhenLegsFailed || res.Outcome != OutcomeEmpty || res.Trace == nil {
		return false
	}
	for _, leg := range res.Trace.Legs {
		if leg.Applicable && leg.Attempted && !leg.Available {
			return true
		}
	}
	return false
}

// candidateRequest maps a Request onto the store's retriever request without
// changing the caller's semantics: the clock is passed through, the condition
// picks the legs, and the caller's unresolved params reach the store, which
// applies its configured vector floor to them.
func candidateRequest(req Request) memory.CandidateRequest {
	window := retrievalWindow(req)
	depth := window * legDepthFactor
	return memory.CandidateRequest{
		ProjectID: req.ProjectID,
		Mode:      projectMode(req),
		Query:     req.Query,
		QueryVec:  req.QueryVec,
		Scope:     req.Scope,
		Category:  req.Category,
		Condition: req.Condition,
		Params:    resolvedParams(req),
		Now:       req.Now,
		// The store treats AsOf as authoritative, so it is carried rather than
		// re-derived: Run has already bound Now to the same instant, and passing
		// only one of the two would leave the store guessing which is the clock.
		AsOf:    req.AsOf,
		Fetch:   memory.Fetch{FTSTopK: depth, VectorTopK: depth, Limit: window},
		Passive: passivePolicies(req),
		// A request to RECORD, not to change: the store ranks, scopes and windows
		// identically with it on or off, and hands back the facts the stages
		// stamped as they decided.
		Explain: req.Explain,
	}
}

// passivePolicies maps the caller's budget onto the store's selection policies.
// It is empty for a query-mode request, which is the point: a query retrieval
// has no bucket policy, because the query and the budget are the whole of it.
//
// The over-fetch default is the slice's own item cap, which is the only reading
// a query-mode slice has: "fetch what I can admit" is bounded by construction,
// and only a passive slice needs a wider fetch than it admits, because the
// selection stages drop rows before stage 9 would.
func passivePolicies(req Request) []memory.SlicePolicy {
	if req.Query != "" {
		return nil
	}
	out := make([]memory.SlicePolicy, 0, len(req.Budget.Slices))
	for _, s := range req.Budget.Slices {
		over := s.OverFetch
		if over <= 0 {
			over = s.MaxItems
		}
		out = append(out, memory.SlicePolicy{
			Bucket:                s.Bucket,
			Order:                 s.Order,
			TwoPass:               s.TwoPass,
			BehaviorFloor:         s.BehaviorFloor,
			BehaviorCategories:    s.BehaviorCategories,
			CategoryWeights:       s.CategoryWeights,
			CategoryCaps:          s.CategoryCaps,
			OverFetch:             over,
			ItemCap:               s.MaxItems,
			DemotionThreshold:     s.DemotionThreshold,
			DemoteOnlyWhenOverCap: s.DemoteOnlyWhenOverCap,
			DropDemotedLosers:     s.DropDemotedLosers,
			IncludeGlobal:         s.IncludeGlobal,
		})
	}
	return out
}

func resolvedParams(req Request) memory.SearchParams {
	if req.Params != nil {
		return *req.Params
	}
	return memory.DefaultSearchParams()
}

// itemBound is the caller's bound on row count: the total, or the sum of the
// per-slice caps when there is no total, which is the shape injection uses for
// independent per-bucket caps. It is 0 when the caller bounded no rows at all,
// which is the only case in which the window is the pipeline's choice rather than
// the caller's.
func itemBound(req Request) int {
	if req.Budget.MaxItems > 0 {
		return req.Budget.MaxItems
	}
	total := 0
	for _, s := range req.Budget.Slices {
		total += s.MaxItems
	}
	return total
}

// windowIsACeiling reports whether the retrieval window is the documented
// maximum rather than something the caller asked for. It reads the same bound the
// window does, because a disclosure about a window that disagrees with the window
// is worse than no disclosure: the two would have to be kept in step by hand.
func windowIsACeiling(req Request) bool {
	if Passive(req) {
		// Never, on a passive request. The ceiling is the pipeline's choice for a
		// window nothing stated; a passive window is stated, per bucket, by the
		// over-fetch — and the note would tell a reader the block is bounded by a
		// 100-row window that no passive read ever asked for. A passive slice
		// bounded only by MaxBytes is bounded by that byte cap, which stage 9
		// applies; the note is not where that fact is stated.
		return false
	}
	// ==, not >=: the fallback sets the window to exactly the ceiling, and a
	// looser comparison would let a future ceiling of twice the size keep the note
	// claiming the old number — the drift this function exists to prevent.
	return itemBound(req) <= 0 && retrievalWindow(req) == maxRetrievalWindow
}

// RetrievalWindow is the window Run will ask the retriever for on this request.
// Exported so a caller whose own path has to describe the same window asks here
// instead of repeating the rule: explain mode reports the ranking of a window,
// and a diagnosis of a window the tool does not use describes nothing.
//
// It is never 0 for a request Run accepts. A byte cap alone is a coherent request
// — "as many rows as fit" — and validateRequest admits it, so the window falls
// back to maxRetrievalWindow rather than handing the store a fetch limit it
// refuses with a message about a limit instead of about the budget. A budget that
// bounds nothing at all, including one whose slices carry only a ClampBytes
// presentation cap, is refused before this is reached.
func RetrievalWindow(req Request) int { return retrievalWindow(req) }

// retrievalWindow is how wide the retrieval window is. It is the caller's total
// item budget, widened for a category predicate, and never smaller than the
// budget: the retriever has to be able to return at least as many rows as the
// caller will accept, or a closure could never be filled.
func retrievalWindow(req Request) int {
	if Passive(req) {
		// A passive window is the policies' own over-fetches and nothing else.
		// The caller's item cap does not size it (the cap bounds MEMBERSHIP, and
		// the window has to be wider than the cap for a demoted row to be
		// backfilled), and a category predicate does not widen it either, because
		// the passive fetch has no category in its SQL: reporting a tripled
		// number here would describe a window nothing read.
		total := 0
		for _, s := range req.Budget.Slices {
			over := s.OverFetch
			if over <= 0 {
				over = s.MaxItems
			}
			total += over
		}
		return total
	}
	total := itemBound(req)
	if total <= 0 {
		// No item bound anywhere, so nothing sizes a window from the budget.
		// The ceiling is the honest default: stage 9 still trims by whatever
		// bytes the request bounds, so the caller gets a block its own limits
		// decide, and a fetch limit of 0 would be refused by the store before
		// any of that.
		total = maxRetrievalWindow
	}
	if req.Category != "" || req.Retention != "" {
		widened := total * predicateFetchWiden
		if widened > maxRetrievalWindow {
			widened = maxRetrievalWindow
		}
		if widened > total {
			total = widened
		}
	}
	return total
}

// projectMode maps the source and project to the storage-level distinction the
// retrieval SQL requires. An empty project id is not an error: an unresolved
// search still reaches the global rows, and a projectless session start is a
// supported state.
func projectMode(req Request) memory.ProjectMode {
	switch req.Source {
	case SourceAllProjects, SourceBench:
		if req.ProjectID == "" {
			return memory.AllProjects
		}
		return memory.ProjectScoped
	default:
		if req.ProjectID == "" {
			return memory.GlobalOnly
		}
		return memory.ProjectScoped
	}
}

// validateRequest rejects a request Run cannot serve honestly, before it asks
// the store for anything. An invalid request is an error rather than an empty
// result: a caller that asked for nothing usable must be told, not shown an
// absence it can misread.
func validateRequest(req Request) error {
	switch req.Source {
	case SourceSearch, SourceProjectCtx, SourceSessionStart, SourceAllProjects, SourceBench, SourceWorkingMoment:
	default:
		return fmt.Errorf("assemble: unknown source %q", req.Source)
	}
	switch req.Condition {
	case CondHybrid, CondFTSOnly, CondVectorOnly:
	case "":
		return errors.New("assemble: a retrieval condition is required")
	default:
		return fmt.Errorf("assemble: unknown condition %q", req.Condition)
	}
	if req.Now.IsZero() {
		return errors.New("assemble: Now is required: the candidate path does not read the wall clock")
	}
	if req.AsOf != nil && req.AsOf.IsZero() {
		return errors.New("assemble: AsOf must name an instant; the zero time is not one")
	}
	if req.Condition == CondVectorOnly && len(req.QueryVec) == 0 {
		return errors.New("assemble: vector-only retrieval requires a query vector")
	}
	if req.Explain && req.AsOf != nil {
		return errors.New("assemble: explain is a projection of the current ranking and cannot describe a historical (as_of) read, " +
			"whose versions were never ranked — ask for one or the other")
	}
	if req.Explain && req.Query == "" {
		return errors.New("assemble: explain requires a query: a passive retrieval scores no candidate, so there is no ranking to explain")
	}
	if req.Source == SourceProjectCtx && req.ProjectID == "" {
		return errors.New("assemble: project context requires a project")
	}
	// A tier filter over a historical read, refused rather than answered -- the
	// same reasoning as vector-only over an as_of request, one field over. The
	// change log records the state a memory HELD: its wording, its category, its
	// importance, its pin. It does not record the tier, so the only value a
	// historical row can carry is the one the row holds NOW, and applying that
	// would answer "what did Ghost know at T, in the tier it is in today" without
	// saying the second half. A filter the caller believes was applied and that
	// was not is the wrong answer; this is the same combination
	// ghost_memory_search already refuses for explain.
	if req.AsOf != nil && req.Retention != "" {
		return fmt.Errorf("assemble: a retention filter cannot describe a historical read: memory_history records what a memory held, not its retention tier, so the only tier available for a version is the one it carries now. Drop the retention filter, or drop as_of")
	}
	if req.Budget.MaxItems < 0 || req.Budget.MaxBytes < 0 {
		return errors.New("assemble: a budget cannot be negative")
	}
	// The cosine is checked HERE rather than only in config, because Run is the
	// exported entry point and the four unusable values fail silently rather than
	// loudly: NaN and a negative both compare false against 0, so the arm reads as
	// OFF and the line tells a user who set a floor that they have none; an
	// infinite or above-one value arms a threshold no cosine can clear, so every
	// result outside the keyword arm comes back weak with nothing on the line to
	// separate it from a measured verdict. config.cosineValue already refuses all
	// four on both of its paths — this makes every future caller inherit that
	// instead of having to remember it.
	if v := float64(req.AbstainCosine); math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
		return fmt.Errorf("assemble: AbstainCosine is a cosine in [0,1], where 0 leaves the arm off, "+
			"got %v", req.AbstainCosine)
	}
	// The cutoff is a fraction of the top row's fused Base, so it inhabits the
	// same [0,1] as a cosine and is refused the same way. NaN compares false
	// against both bounds and would read as OFF, telling a caller who set a
	// cutoff that there is none; an infinite or above-one value is a threshold
	// above the top Base, which admits nothing the answer would not already
	// have. 0 is a valid and meaningful value — the cutoff is disabled — so it is
	// not refused here; the stage reads it rather than rejecting it.
	if v := req.RelevanceCutoff; math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
		return fmt.Errorf("assemble: RelevanceCutoff is a fraction in [0,1], where 0 is off, got %v", req.RelevanceCutoff)
	}
	// The no-answer bar is a cosine, so it is refused the way a cosine and the
	// cutoff are: NaN would read as OFF and tell a caller who set a bar there is
	// none, and a value above 1 would withhold every answer.
	if v := req.NoAnswerCosine; math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
		return fmt.Errorf("assemble: NoAnswerCosine is a cosine in [0,1], where 0 is off, got %v", req.NoAnswerCosine)
	}
	for _, s := range req.Budget.Slices {
		if s.MaxItems < 0 || s.MaxBytes < 0 || s.ClampBytes < 0 {
			return fmt.Errorf("assemble: slice %q cannot have a negative bound", s.Bucket)
		}
	}
	if buckets := sliceBuckets(req.Budget.Slices); len(buckets) > 0 {
		return fmt.Errorf("assemble: two slices name the same bucket (%s): itemBound would sum both caps "+
			"while stage 9 honours only the first, so the window would not be the block's", strings.Join(buckets, ", "))
	}
	if req.Budget.MaxItems == 0 && req.Budget.MaxBytes == 0 && !anySliceBound(req.Budget.Slices) {
		return errors.New("assemble: this budget states no bound on the block: set MaxItems or MaxBytes, or give a slice an item or byte cap — a ClampBytes clamp alone bounds neither, since a clamped item is shorter")
	}
	if req.Query == "" {
		if req.AsOf != nil {
			// Refused here as well as in the store, for the same reason the store
			// refuses it: the passive policies select over LIVE rows and a
			// historical read selects over RECORDED versions, and Run dispatches
			// on AsOf first — so a request carrying both would be answered by the
			// historical path with its policies silently discarded and a window its
			// caller never stated, while Run still reported `not_applicable` and
			// added the historical qualifier. The store's own guard is not enough
			// here because Run never reaches it on this shape.
			return errors.New("assemble: a passive request (no query) cannot also be a historical (as_of) read: the bucket policies " +
				"select over live rows and a historical read selects over recorded versions — use one or the other")
		}
		if err := validatePassiveBudget(req); err != nil {
			return err
		}
	}
	return nil
}

// validatePassiveBudget refuses a passive request the retriever could not serve
// honestly. Two things are checked, and both are about the FETCH rather than
// about the block:
//
//   - there must be a policy at all. A request with no query and no slices is
//     the one shape the store cannot answer: it has nothing to retrieve by, and
//     an empty set would read as a store that holds nothing. A search request
//     that arrives with an empty query is refused here for the same reason,
//     which is why the check is on the query's absence and not on Source.
//   - every slice must bound its own window. The query path gets one from the
//     budget's item bound, so a fetch limit is always implied; here the window
//     IS the policy, and a slice stating neither an over-fetch nor a cap would
//     leave the store to choose — which on a path that runs at every session
//     start is a full-store scan.
func validatePassiveBudget(req Request) error {
	if len(req.Budget.Slices) == 0 {
		return errors.New("assemble: a request with no query carries no bucket policies, so there is nothing to retrieve by; " +
			"give it a slice per bucket, or a query")
	}
	// Resolved BEFORE the loop, not during it. The overlap is a property of the
	// whole budget, so a check that sets the flag as it walks is order-dependent:
	// it refuses `[mixing, _global]` and serves `[_global, mixing]`, which is a
	// refusal a caller can talk its way past by sorting its slices.
	mixesGlobal := false
	for _, s := range req.Budget.Slices {
		if s.IncludeGlobal {
			mixesGlobal = true
		}
	}
	for _, s := range req.Budget.Slices {
		// The bucket IS the project predicate on a passive read: the store binds
		// SlicePolicy.Bucket as the WHERE clause and does not consult Mode at all,
		// because one policy per bucket is the whole shape of a passive retrieval.
		// So a slice naming some other project would read — and inject — rows the
		// request never named, and stage 3 only RECORDS that as
		// Signals[id].ProjectMatch=false, which nothing refuses. Refusing the
		// mismatch here is the same decision `sliceBuckets` makes for a repeated
		// bucket: one named bucket per slice, and it has to be a bucket this
		// request is about.
		if s.Bucket != req.ProjectID && s.Bucket != memory.GlobalProjectID {
			return fmt.Errorf("assemble: passive slice names bucket %q, which is neither the requested project %q nor %q; "+
				"the bucket IS the project predicate on this path, so a mismatched one would read a project the request "+
				"never named", s.Bucket, req.ProjectID, memory.GlobalProjectID)
		}
		// Overlapping row sets under distinct bucket NAMES, which `sliceBuckets`
		// cannot see: one slice admits `_global` into the project read and another
		// fetches `_global` in its own right. The two sets overlap, so every
		// global row is admitted twice — and stage 9 would cap them under two
		// different slices, so neither slice's cap would describe the block. The
		// store refuses the same shape; this seam has to, because it is where a
		// caller states its budget and `Run` is the exported entry point.
		//
		// A caller that wants both — the project-context surface does, for its
		// `## Global` section — runs them as two REQUESTS, which is also what
		// keeps their two verdicts and two traces separate.
		if s.Bucket == memory.GlobalProjectID && mixesGlobal {
			return fmt.Errorf("assemble: a passive slice fetches %q while another admits it into a project bucket; "+
				"the two row sets overlap, so every global row would be admitted twice and capped under two different "+
				"slices. Read them as two requests", memory.GlobalProjectID)
		}
		// The bound that matters here is the FETCH, not the block. A slice bounded
		// only by MaxBytes says how many bytes the answer may occupy, which bounds
		// membership but says nothing about how much is READ — and on this path the
		// read is the thing that must not be unbounded. Accepting it would also be
		// incoherent downstream: `passivePolicies` falls back to MaxItems for the
		// over-fetch, so a MaxBytes-only slice would reach the store asking for a
		// window of 0 and be refused there, with a message about the store's
		// contract for a request this seam had already called valid.
		if s.OverFetch <= 0 && s.MaxItems <= 0 {
			return fmt.Errorf("assemble: passive slice %q bounds the block's bytes but states no over-fetch and no item cap, "+
				"so its retrieval window is unbounded; this path runs at every session start. Name OverFetch, or MaxItems "+
				"if the over-fetch is the same number", s.Bucket)
		}
	}
	// A category or tier filter cannot be honoured here, and the seam would rather
	// refuse it than serve a confident wrong answer.
	//
	// `passiveFetchSQL` binds `WHERE project_id = ? AND resolved_at IS NULL` plus
	// scope — there is no category and no retention in its SQL at all, because a
	// passive block is selected by importance, decay and pin rather than by a
	// predicate. The query path can afford the same filter because it WIDENS the
	// window when one is set (`predicateFetchWiden`, what closed #573), so a
	// matching row ranked below the cut stays reachable; the passive branch of
	// `retrievalWindow` returns before that widening, because the window there is
	// the policies' own over-fetches.
	//
	// So a passive request carrying a category is not "filtered, then assembled" —
	// it is assembled from rows the filter never touched, and stage 3 then drops
	// every non-matching one out of a window nothing widened. The caller gets
	// `all_out_of_category`, or a short block, while the store holds exactly the
	// rows it asked for just below the over-fetch cut. That is a false negative
	// with a confident reason attached, and it is worse than a refusal: a caller
	// that sees the reason concludes its category is absent from the project.
	//
	// Refusing also keeps the number honest. `RetrievalWindow` reporting a tripled
	// window nothing read is a small lie, but the one that does damage is the
	// filter: it decides membership, and membership is what the caller came for.
	if req.Category != "" {
		return fmt.Errorf("assemble: a passive request cannot carry a Category filter (%q): the passive fetch binds no "+
			"category in SQL and its window is the policies' over-fetches, so nothing widens the read for one — the rows "+
			"would be dropped after selection rather than fetched by it. Drop the filter, or send a query", req.Category)
	}
	if req.Retention != "" {
		return fmt.Errorf("assemble: a passive request cannot carry a Retention filter (%q): the passive fetch binds no "+
			"tier in SQL and its window is the policies' over-fetches, so nothing widens the read for one — the rows "+
			"would be dropped after selection rather than fetched by it. Drop the filter, or send a query", req.Retention)
	}
	return nil
}

// Passive reports whether this request is a passive retrieval: no query, so the
// block is selected by importance, decay and pin rather than by relevance to
// anything the caller asked.
//
// It is a function of the query alone, not of the Source, because the shape is
// what decides the consequences. A passive block cannot receive a relevance
// verdict (there is no query to be relevant to) and its empty reason describes
// a window rather than the store, and both of those follow from the absence of
// a query — so a source that arrives with one is a query-mode request whatever
// it calls itself, and a source that arrives without one is passive.
func Passive(req Request) bool { return req.Query == "" }

// ReasonNoMemories is the empty reason for a passive block: the over-fetched
// window came back empty. It is exported because a surface that frames its own
// block has to tell this case from a block that was EMPTIED, and only the reason
// knows which — the two call for opposite sentences, one of which is a census and
// the other a report of rows found and withheld. Comparing against the string
// literal would work until the vocabulary moved.
const ReasonNoMemories = reasonNoMemories

// ReasonAllInvalid is the empty reason for a block whose every row was withheld
// as out of date, exported for the same reason as ReasonNoMemories and beside it
// so a caller comparing reasons reads them from one place.
const ReasonAllInvalid = reasonAllInvalid

// sliceBuckets lists the bucket names that appear more than once. The two halves
// of a slice budget disagree about a repeat: the window sums every cap, and
// sliceFor honours the first slice for a bucket, so the window is the wider of the
// two and the disagreement over-fetches rather than dropping a row. A caller who
// meant one cap per bucket has written something else, and it is cheap to refuse.
func sliceBuckets(slices []Slice) []string {
	seen := make(map[string]bool, len(slices))
	dupes := make(map[string]bool, len(slices))
	var order []string
	for _, s := range slices {
		if !seen[s.Bucket] {
			seen[s.Bucket] = true
			continue
		}
		if !dupes[s.Bucket] {
			dupes[s.Bucket] = true
			order = append(order, s.Bucket)
		}
	}
	return order
}

// anySliceBound reports whether a slice list bounds the block at all. ClampBytes
// does not: it is a per-item presentation cap, and a clamped item is shorter, so
// it can only let more rows fit. A budget whose slices name only clamps therefore
// bounds neither the row count nor the bytes, which is the omission the
// all-zero check refuses — treating it as a bound would leave the block's size
// decided by a ceiling the caller never asked for.
func anySliceBound(slices []Slice) bool {
	for _, s := range slices {
		if s.MaxItems > 0 || s.MaxBytes > 0 {
			return true
		}
	}
	return false
}

// cosineReader is the optional capability the no-answer bar uses to judge a row
// the vector leg's own list did not carry. *memory.Store has it; a retriever that
// does not leaves such rows unjudged, which the bar never withholds.
type cosineReader interface {
	EmbeddingCosines(ctx context.Context, ids []string, queryVec []float32) (map[string]float32, error)
}

// loadExactCosines reads, for the rows that carry no vector-leg cosine (VectorScore
// -1), the cosine of their own stored embedding. A hybrid window admits keyword
// hits whatever their cosine, so such a row can be embedded and weakly similar
// (judged by the bar) or have no embedding at all (absent from the answer, and
// never judged). It runs only for a query-mode request with the bar on, and a
// failed read leaves every such row unjudged, which only ever keeps rows.
func (p *pipeline) loadExactCosines(ctx context.Context, r Retriever) {
	if p.passive || p.req.NoAnswerCosine <= 0 || len(p.req.QueryVec) == 0 {
		return
	}
	cr, ok := r.(cosineReader)
	if !ok {
		return
	}
	var ids []string
	for _, c := range p.rows {
		if c.VectorScore < 0 {
			ids = append(ids, c.ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	got, err := cr.EmbeddingCosines(ctx, ids, p.req.QueryVec)
	if err != nil || len(got) == 0 {
		return
	}
	p.exactCosine = make(map[string]float64, len(got))
	for id, v := range got {
		p.exactCosine[id] = float64(v)
	}
}

// rowCosine is the cosine the bar judges a row by: the vector leg's own score
// when it carried one, else the cosine of the row's stored embedding, else -1
// (no cosine exists, so the row is never judged).
func (p *pipeline) rowCosine(c memory.Candidate) float64 {
	if c.VectorScore >= 0 {
		return c.VectorScore
	}
	if v, ok := p.exactCosine[c.ID]; ok {
		return v
	}
	return -1
}
