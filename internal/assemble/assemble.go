// Package assemble owns what a context block is: selection, validity,
// predicates, provenance, conflicts, dedup, diversity, budget, abstention and
// the shared item renderer. internal/memory owns storage, retrieval legs, fusion
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
	SourceSearch        Source = "search"
	SourceProjectCtx    Source = "project_context"
	SourceSessionStart  Source = "session_start"
	SourceAllProjects   Source = "all_projects"
	SourceBench         Source = "bench"
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
type Slice struct {
	Bucket                string
	MaxItems              int // 0 = unbounded within this slice
	MaxBytes              int // item-content bytes; 0 = unbounded within this slice
	ClampBytes            int // 0 = no per-item presentation clamp
	DropDemotedLosers     bool
	DemoteOnlyWhenOverCap bool

	OverFetch          int
	Order              string
	IncludeGlobal      bool
	TwoPass            bool
	BehaviorFloor      int
	BehaviorCategories []string
	CategoryWeights    map[string]float64
	CategoryCaps       map[string]int
	DemotionThreshold  float64
}

// Budget is what a caller will accept. MaxItems is the total across buckets;
// Slices are the per-bucket caps, so search applies one limit across project
// and `_global` while injection applies independent caps. A budget that bounds
// neither rows nor bytes is rejected: an unbounded block is not a request, it is
// an omission. A MaxBytes cap alone is bounded, so it is honoured — the
// retrieval window falls back to its documented ceiling, stage 9 still trims by
// the per-slice item caps, and Run's response-fit post-pass brings the complete
// rendered response inside MaxBytes, except that the search envelope keeps its
// first row whole when that row alone is larger. MaxBytes is the RESPONSE's bytes, never
// item content: Slice.MaxBytes is the item-content cap, and applying one field to
// two units would make the effective limit depend on a caller's framing.
type Budget struct {
	MaxItems               int // 0 = unbounded total
	MaxBytes               int // complete response bytes; 0 = unbounded total
	MaxNoteBytes           int // per-note maximum; 0 = use the documented default
	MaxNotesBytes          int // total notes maximum; 0 = use the documented default
	Slices                 []Slice
	Measure                func(items []Item, trace *Trace) int
	FramingCeiling         int
	KeepPinned             bool
	ReserveByProject       map[string]int
	ReservePinnedByProject map[string]int
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
	ProjectID                    string
	Query                        string    // empty selects passive mode
	QueryVec                     []float32 // nil skips the vector leg; required for CondVectorOnly
	Scope                        map[string]string
	Category                     string
	Retention                    string
	Source                       Source
	Budget                       Budget
	Condition                    Condition
	Params                       *memory.SearchParams // unresolved caller params; nil uses defaults
	Now                          time.Time            // required and used end to end
	AsOf                         *time.Time
	AbstainCosine                float32
	RelevanceCutoff              float64
	NoAnswerCosine               float64
	NoAnswerBarNote              string
	Covered                      func(memory.Candidate) bool
	Explain                      bool
	Record                       RecordSink
	SuppressRecordWhenLegsFailed bool
	Logger                       *slog.Logger
	SessionID                    string
}

// Result is the assembled block. Run owns the whole response — listing, verdict
// sentence, filter caveat, diagnostics and the machine line — so Response is the
// text a caller returns verbatim and Bytes is its length, never 0 on a successful
// Run.
type Result struct {
	Items      []Item
	Outcome    Outcome
	Reason     string
	Notes      []string
	Qualifiers []string
	Trace      *Trace
	Explain    *memory.SearchExplain
	Bytes      int // complete rendered response, including framing and outcome
	Abstention string
	Machine    string
	Response   string
	Tokens     int
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

const predicateFetchWiden = 3

const maxRetrievalWindow = 100

const legDepthFactor = 2

// Run assembles one context block.
func Run(ctx context.Context, r Retriever, req Request) (Result, error) {
	if r == nil {
		return Result{}, errors.New("assemble: a retriever is required")
	}
	if err := validateRequest(req); err != nil {
		return Result{}, err
	}
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
		p.windowDisclosure = note
	}

	for _, name := range []string{"fts", "vector"} {
		leg := set.Legs[name]
		if leg.Applicable && leg.Attempted && !leg.Available {
			p.retrievalFailures = append(p.retrievalFailures, formatNote("retrieval_%s leg failed (%s): this search is incomplete, so an empty result does not mean nothing matched", name, leg.Err))
		}
	}
	for _, st := range stages {
		st.run(p)
	}

	res, err := p.fitResponse(Result{
		Items:      p.items,
		Trace:      p.trace,
		Notes:      p.notes(),
		Qualifiers: p.qualifiers,
	})
	relevance := res
	if err == nil {
		p.displayReplacements(&res)
	}
	if err == nil && req.Explain {
		res.Explain = p.explainProjection(relevance)
	}
	if err == nil && !req.Explain && !req.convertsToError(res) {
		emit(ctx, req.Record, req, relevance)
	}
	return res, err
}

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
		AsOf:      req.AsOf,
		Fetch:     memory.Fetch{FTSTopK: depth, VectorTopK: depth, Limit: window},
		Passive:   passivePolicies(req),
		Explain:   req.Explain,
	}
}

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

func windowIsACeiling(req Request) bool {
	if Passive(req) {
		return false
	}
	return itemBound(req) <= 0 && retrievalWindow(req) == maxRetrievalWindow
}

// RetrievalWindow is the window Run will ask the retriever for on this request.
// Exported so a caller whose own path has to describe the same window asks here
// instead of repeating the rule: explain mode reports the ranking of a window,
// and a diagnosis of a window the tool does not use describes nothing.
func RetrievalWindow(req Request) int { return retrievalWindow(req) }

func retrievalWindow(req Request) int {
	if Passive(req) {
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
		total = maxRetrievalWindow
	}
	if req.Category != "" || req.Retention != "" || len(req.Scope) > 0 {
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
	if req.AsOf != nil && req.Retention != "" {
		return fmt.Errorf("assemble: a retention filter cannot describe a historical read: memory_history records what a memory held, not its retention tier, so the only tier available for a version is the one it carries now. Drop the retention filter, or drop as_of")
	}
	if req.Budget.MaxItems < 0 || req.Budget.MaxBytes < 0 {
		return errors.New("assemble: a budget cannot be negative")
	}
	if v := float64(req.AbstainCosine); math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
		return fmt.Errorf("assemble: AbstainCosine is a cosine in [0,1], where 0 leaves the arm off, "+
			"got %v", req.AbstainCosine)
	}
	if v := req.RelevanceCutoff; math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
		return fmt.Errorf("assemble: RelevanceCutoff is a fraction in [0,1], where 0 is off, got %v", req.RelevanceCutoff)
	}
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
			return errors.New("assemble: a passive request (no query) cannot also be a historical (as_of) read: the bucket policies " +
				"select over live rows and a historical read selects over recorded versions — use one or the other")
		}
		if err := validatePassiveBudget(req); err != nil {
			return err
		}
	}
	return nil
}

func validatePassiveBudget(req Request) error {
	if len(req.Budget.Slices) == 0 {
		return errors.New("assemble: a request with no query carries no bucket policies, so there is nothing to retrieve by; " +
			"give it a slice per bucket, or a query")
	}
	mixesGlobal := false
	for _, s := range req.Budget.Slices {
		if s.IncludeGlobal {
			mixesGlobal = true
		}
	}
	for _, s := range req.Budget.Slices {
		if s.Bucket != req.ProjectID && s.Bucket != memory.GlobalProjectID {
			return fmt.Errorf("assemble: passive slice names bucket %q, which is neither the requested project %q nor %q; "+
				"the bucket IS the project predicate on this path, so a mismatched one would read a project the request "+
				"never named", s.Bucket, req.ProjectID, memory.GlobalProjectID)
		}
		if s.Bucket == memory.GlobalProjectID && mixesGlobal {
			return fmt.Errorf("assemble: a passive slice fetches %q while another admits it into a project bucket; "+
				"the two row sets overlap, so every global row would be admitted twice and capped under two different "+
				"slices. Read them as two requests", memory.GlobalProjectID)
		}
		if s.OverFetch <= 0 && s.MaxItems <= 0 {
			return fmt.Errorf("assemble: passive slice %q bounds the block's bytes but states no over-fetch and no item cap, "+
				"so its retrieval window is unbounded; this path runs at every session start. Name OverFetch, or MaxItems "+
				"if the over-fetch is the same number", s.Bucket)
		}
	}
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

func anySliceBound(slices []Slice) bool {
	for _, s := range slices {
		if s.MaxItems > 0 || s.MaxBytes > 0 {
			return true
		}
	}
	return false
}

type cosineReader interface {
	EmbeddingCosines(ctx context.Context, ids []string, queryVec []float32) (map[string]float32, error)
}

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
		if c.VectorScore == -1 {
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

func (p *pipeline) rowCosine(c memory.Candidate) (float64, bool) {
	if c.VectorScore != -1 {
		return c.VectorScore, true
	}
	if v, ok := p.exactCosine[c.ID]; ok {
		return v, true
	}
	return 0, false
}
