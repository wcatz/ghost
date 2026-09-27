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
// never includes response framing.
type Slice struct {
	Bucket            string
	MaxItems          int  // 0 = unbounded within this slice
	MaxBytes          int  // item-content bytes; 0 = unbounded within this slice
	ClampBytes        int  // 0 = no per-item presentation clamp
	DropDemotedLosers bool // honored only for session-start
}

// Budget is what a caller will accept. MaxItems is the total across buckets;
// Slices are the per-bucket caps, so search applies one limit across project
// and `_global` while injection applies independent caps. A budget that bounds
// neither rows nor bytes is rejected: an unbounded block is not a request, it is
// an omission. A MaxBytes cap alone is bounded, so it is honoured — the
// retrieval window falls back to its documented ceiling, stage 8 still trims by
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
	Explain       bool
}

// Result is the assembled block. Run owns the whole response — listing, verdict
// sentence, filter caveat, diagnostics and the machine line — so Response is the
// text a caller returns verbatim and Bytes is its length, never 0 on a successful
// Run.
//
// The framing is the SEARCH framing, for every Source, because that is the only
// one this version knows: nothing here branches on Source. A caller whose surface
// frames differently must not read Response, and must leave Budget.MaxBytes at 0
// until it supplies a render of its own — otherwise the post-pass measures this
// envelope against a budget that was stated for another one, and drops rows
// against a cap the caller never described. Session-start is the next surface
// (#577) and inherits that constraint, not an exemption.
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
	Bytes      int // complete rendered response, including framing and outcome
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

// categoryFetchWiden is the multiple applied to the retrieval window when a
// category predicate is present. Category is applied before the window closes,
// so it needs room beyond the caller's limit for a matching row to be reached
// from — the same widening the tool used to apply by hand before this seam
// existed. Without the predicate the window is the caller's limit, so a plain
// search retrieves and returns exactly what it always did.
const categoryFetchWiden = 3

// maxRetrievalWindow bounds how far the category widening may go. The tool
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
		rows:           set.Rows,
		droppedBy:      map[string]int{},
		droppedByBound: map[string]int{},
		dropped:        map[string]string{},
		qualifiers:     qualifiersFor(req, set),
	}
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
		// building a projection from the trace — explain, in the next PR — would
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
	return p.fitResponse(Result{
		Items:      p.items,
		Trace:      p.trace,
		Notes:      p.notes(),
		Qualifiers: p.qualifiers,
	})
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
		AsOf: req.AsOf,
		Fetch: memory.Fetch{
			FTSTopK:    depth,
			VectorTopK: depth,
			Limit:      window,
		},
	}
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
	total := itemBound(req)
	if total <= 0 {
		// No item bound anywhere, so nothing sizes a window from the budget.
		// The ceiling is the honest default: stage 8 still trims by whatever
		// bytes the request bounds, so the caller gets a block its own limits
		// decide, and a fetch limit of 0 would be refused by the store before
		// any of that.
		total = maxRetrievalWindow
	}
	if req.Category != "" {
		widened := total * categoryFetchWiden
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
	case SourceSearch, SourceProjectCtx, SourceSessionStart, SourceAllProjects, SourceBench:
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
	if req.Source == SourceProjectCtx && req.ProjectID == "" {
		return errors.New("assemble: project context requires a project")
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
	for _, s := range req.Budget.Slices {
		if s.MaxItems < 0 || s.MaxBytes < 0 || s.ClampBytes < 0 {
			return fmt.Errorf("assemble: slice %q cannot have a negative bound", s.Bucket)
		}
	}
	if buckets := sliceBuckets(req.Budget.Slices); len(buckets) > 0 {
		return fmt.Errorf("assemble: two slices name the same bucket (%s): itemBound would sum both caps "+
			"while stage 8 honours only the first, so the window would not be the block's", strings.Join(buckets, ", "))
	}
	if req.Budget.MaxItems == 0 && req.Budget.MaxBytes == 0 && !anySliceBound(req.Budget.Slices) {
		return errors.New("assemble: this budget states no bound on the block: set MaxItems or MaxBytes, or give a slice an item or byte cap — a ClampBytes clamp alone bounds neither, since a clamped item is shorter")
	}
	if req.Query == "" {
		// An empty query selects passive retrieval, whose bucket policies are
		// specification until the session-start surface moves onto this seam.
		// Rejecting it here keeps the failure an error the caller can see,
		// rather than an empty block that reads as an empty store.
		return errors.New("assemble: passive retrieval (an empty query) is not served yet; it arrives with the session-start migration")
	}
	return nil
}

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
