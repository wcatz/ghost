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
	Limit            int
	VectorAvailable  bool
	Notes            []string
	// Mode is the storage-level project mode retrieval ran in.
	Mode      string
	Legs      map[string]memory.LegStatus
	Signals   map[string]Signals
	Stages    []StageTrace
	Decisions []Decision
	Floors    Floors
}

// Floors records the exact thresholds a relevance floor used. It is zero while
// no floor is applied — the arms arrive with abstention — and a non-zero value
// here always names a threshold that was actually evaluated.
type Floors struct {
	FTSRankMax   int
	VectorCosine float32
	VectorArmOn  bool
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

// newTrace seeds the trace from the request and the candidate set. Recording
// starts here and is unconditional.
func newTrace(req Request, set *memory.CandidateSet) *Trace {
	return &Trace{
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
	}
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
