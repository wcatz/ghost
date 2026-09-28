package assemble

import (
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// Trace is the record of what each stage saw and decided. It is always
// recorded: only the explain projection is gated, because a flag-dependent
// second ranking path is how an explanation starts drifting from the ranking it
// explains.
type Trace struct {
	ProjectID, Query string
	// AsOf is the instant a historical read was assembled at, in RFC 3339, and
	// empty for a current read. It is the trace's own record of the binding Run
	// performed on Now, so a reader of the trace can tell a block that was
	// assembled against the wall clock from one assembled against a past instant
	// without having to know that both carry a Now.
	AsOf string
	// Limit is the window Run asked the retriever for, not the caller's budget:
	// the caller's item bound widened for a category predicate, or the documented
	// ceiling when the budget states no item bound at all. The stage 8 record
	// carries a note when that ceiling was the pipeline's choice, so a projection
	// built from this trace can tell an invented window from a requested one.
	Limit           int
	VectorAvailable bool
	Notes           []string
	// Mode is the storage-level project mode retrieval ran in.
	Mode      string
	Legs      map[string]memory.LegStatus
	Signals   map[string]Signals
	Stages    []StageTrace
	Decisions []Decision
	Floors    Floors
}

// Floors records the exact thresholds a relevance floor used. FTSRankMax is the
// keyword arm and is always set, because a request with no vector leg still
// judges on keyword rank.
//
// FTSApplied is the keyword arm's counterpart to VectorApplied, and it exists for
// the same reason: a threshold and a threshold that ran are different facts, and
// the keyword arm is not always run. A retriever that never ranked a row by
// keyword leaves the -1 "this leg did not retrieve it" sentinel on every row, and
// a threshold printed next to that verdict asserts a judgement nobody made — the
// ordinary state of a semantic query whose words share nothing with the memory.
// It is a fact about the RUN, not the request, so Run sets it beside
// VectorApplied rather than newTrace, which only knows what was asked for.
type Floors struct {
	FTSRankMax    int
	FTSApplied    bool
	VectorCosine  float32
	VectorArmOn   bool
	VectorApplied bool
}

// Signals is one candidate's contribution, copied from the retriever rather than
// recomputed. ScopeMatched, ValidityState and the two contributions are the
// facts the later stages produced, so a reader can see why a row is in the
// block without re-running any arithmetic.
type Signals struct {
	FTSRank, VectorRank int
	VectorScore         float64
	Base, DecayFactor   float64
	AgeDays             float64
	CreatedAt           time.Time
	ProjectMatch        bool
	ScopeMatched        bool
	ScopeKeysCompared   []string
	ValidityState       string
	// ValidityPenalty is 0 in v1: stage 2 drops a row outside its window
	// rather than ranking it lower, so no row carries one.
	ValidityPenalty        float64
	Confidence             *float64
	ConfidenceContribution float64
	// ProvenanceWeight is the weight stage 4 recorded, as text so the trace
	// shows "1.0" rather than a float that could be mistaken for a tuned value.
	// It is inert in v1: no score is multiplied by it, and both contributions
	// below are zero. A measured multiplier changes this field's value and the
	// two assignments that compute the contributions together.
	ProvenanceWeight       string
	ProvenanceContribution float64
	// Evidence is what supports the memory: how many observations the store holds
	// for it, and how many of them carry a verification. It is RECORDED, never
	// acted on -- the weight above is what would act, and it is pinned at 1.0 --
	// so the trace can already say "supported by 2 observations, 1 verified" for a
	// reader who asks what a memory rests on. Render it with
	// memory.EvidenceCounts.Label; nothing here ranks on it.
	Evidence memory.EvidenceCounts
}

// StageTrace is one stage's counts and the rows it removed.
type StageTrace struct {
	Stage      string
	In, Out    int
	DroppedIDs []string
	Reordered  bool
	Notes      []string
}

// Decision is one row's fate at one stage, with the row it was decided against
// where there was one.
type Decision struct {
	ID, Stage, Reason, AgainstID string
	Kept                         bool
	Before, After                float64
}

// stage names, in pipeline order.
const (
	stageRetrieve    = "retrieve"
	stageValidity    = "validity"
	stagePredicates  = "predicates"
	stageProvenance  = "provenance"
	stageConflicts   = "conflicts"
	stageDedup       = "dedup"
	stageDiversity   = "diversity"
	stageBudget      = "budget"
	stageRender      = "render"
	stageResponseFit = "response_fit"
)

// The three caps stage 8 applies, as the sentence names them. They are recorded
// per removal because their remedies differ: a row count is raised and a content
// byte budget is not, so advice that fits one is wrong advice for the other.
const (
	boundSliceItems = "slice_item_cap"
	boundSliceBytes = "slice_byte_cap"
	boundTotalItems = "item_cap"
)

// newTrace seeds the trace from the request and the candidate set. Recording
// starts here and is unconditional.
func newTrace(req Request, set *memory.CandidateSet) *Trace {
	t := &Trace{
		ProjectID: req.ProjectID,
		Query:     req.Query,
		// The window the retriever is asked for, not the caller's item budget:
		// they differ for a category predicate (the window is widened) and for a
		// budget that names no item bound at all (the window falls back to the
		// ceiling), and a trace that reported the budget beside the real window
		// would explain neither.
		Limit:           retrievalWindow(req),
		VectorAvailable: len(req.QueryVec) > 0,
		Mode:            string(projectMode(req)),
		Legs:            set.Legs,
		Signals:         map[string]Signals{},
		// The thresholds this run will judge against, recorded before the stages
		// so a projection of the trace cannot report a floor nobody evaluated.
		Floors: floorsOf(req),
	}
	if req.AsOf != nil {
		t.AsOf = req.AsOf.UTC().Format(time.RFC3339)
	}
	return t
}

// record appends one stage's entry, always. In and Out are what the stage saw
// and what it left, so a reader can tell a stage that changed nothing from one
// that was never reached.
func (t *Trace) record(stage string, in, out int, dropped []string, reordered bool, notes ...string) {
	t.Stages = append(t.Stages, StageTrace{
		Stage: stage, In: in, Out: out, DroppedIDs: dropped, Reordered: reordered, Notes: notes,
	})
}

// decide records one row's exclusion, with the reason the stage gives.
func (t *Trace) decide(id, stage, reason string, before float64) {
	t.Decisions = append(t.Decisions, Decision{
		ID: id, Stage: stage, Reason: reason, Kept: false, Before: before,
	})
}

// keep records that a row survived a stage, with the reason the stage noted
// while letting it through. An unreadable validity value is the case v1 has: the
// value is reported, and the row is kept, so the two facts have to be recorded
// separately. Recording a kept row through decide would tell a consumer of the
// trace that a row in the answer was excluded from it.
func (t *Trace) keep(id, stage, reason string, before float64) {
	t.Decisions = append(t.Decisions, Decision{
		ID: id, Stage: stage, Reason: reason, Kept: true, Before: before,
	})
}
