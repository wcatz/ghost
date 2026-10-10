package assemble

import (
	"strings"
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
	// ceiling when the budget states no item bound at all. The stage 9 record
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
	// PinnedCut is, per project (the row's own), how many pinned rows the cap left
	// out: the pinned rows stage 9 cut plus the ones the retriever reported past
	// its window. A pin is a slot guarantee, so a non-zero count means the pinned
	// rows alone exceeded the cap, and it is a subset of what the bucket ranked out.
	// Empty for a query-mode read, which promises a pin nothing.
	PinnedCut map[string]int
	// WindowExtra is the retriever's per-bucket count of rows carried beyond the
	// over-fetch (CandidateSet.WindowExtra), so a caller deriving the rows past the
	// window does not count them twice.
	WindowExtra map[string]int
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
//
// There is deliberately no "did this stage reorder" field, and the absence is a
// decision rather than a gap: a reader who needs to know which rows a stage
// MOVED reads that stage's own per-row decisions, which say it in more detail
// than a bool could. Stages 2-6 and 8-9 filter or record and move nothing, so
// they file no decisions and the question does not arise for them.
//
// Stage 7 (diversity) is the one exception, and it is the one stage whose
// reordering is recorded per row rather than by a flag: it permutes the
// candidate order so that the rows behind the window are the ones the ranking
// put there, and every row it moved has a decision naming it (Kept for one it
// moved and then readmitted, dropped for one left behind the window). Its In and
// Out are therefore the SAME count — it removed nothing — and its DroppedIDs
// names the rows it moved behind the window rather than rows it removed from the
// set. The In - Out = len(DroppedIDs) identity the other stages hold is broken
// here on purpose: a stage that deferred a row and a stage that deleted one are
// different events, and a count that had to satisfy the identity would have to
// lie about one of them. A row the budget then cuts keeps the verdict stage 7
// gave it (see trim), so In - Out still equals the number of rows the whole
// pipeline judged. All of this is on PASSIVE reads only: the share is a
// passive-read rule, so a query-mode request records stage 7 as a pass-through
// and holds no decision for it at all.
//
// In and Out are what the stage saw and left, and they chain from one stage to
// the next EXCEPT at stage 6: its In is the rows it was handed plus the
// near-duplicate losers the retriever removed before the pipeline began, which
// are counted In and dropped here (so In - Out equals len(DroppedIDs)) but were
// never in stage 5's Out. Stage 6 is the first stage that can report a drop made
// upstream of it, and counting them In is what keeps In - Out = dropped true.
type StageTrace struct {
	Stage      string
	In, Out    int
	DroppedIDs []string
	Notes      []string
}

// Decision is one row's fate at one stage: whether it was kept or dropped, the
// stage that decided it, the reason that stage gave, and the score the row held
// when the decision was made.
//
// It records no score AFTER the decision: no stage re-scores a row, so there is
// no "after" to record — stage 4's weight is pinned at 1.0 and both the
// contributions it writes are zero, so Before is still the row's final score. A
// stage that starts re-scoring has to add the field, because a Decision that
// carries an unset one is indistinguishable from a stage that judged it.
//
// The row a decision was made AGAINST is Against, and two stages set it. Stage 6
// is the retriever's verdict, made over a window this pipeline never saw the
// edges of, so stage 6 does not decide it: it records what the retriever reported
// (CandidateSet.DroppedLosers), the winner ids included. Stage 5 decides its own:
// a row withheld from a `contradicts` pair names the kept row or rows it
// directly contradicts. Every other stage leaves it empty.
//
// ProjectID is the row's own project, recorded because a trace is read per
// bucket: the session-start block keys its "N shown of M total" line on it, and
// a decision that named no project could not be told apart from another bucket's
// row (a `_global` row dropped beside this project's is that bucket's fate, not
// this one's).
type Decision struct {
	ID        string
	ProjectID string
	Stage     string
	Reason    string
	Kept      bool
	Before    float64
	// Against is the ids of the rows this one lost to. Set by a stage 6
	// near_duplicate drop and by a stage 5 contradiction separation; empty on
	// every other decision.
	Against []string
}

// stage names, in pipeline order.
//
// Retrieval is NOT one of them, and the reason is the seam's shape rather than an
// omission: it is the one step that needs the Retriever, so Run performs it
// before the pipeline starts and has no In/Out pair to record — the store was
// asked a question and it answered. The trace describes it instead through
// Trace.Limit (the window asked for) and Trace.Legs (what each leg did), and a
// `retrieve` name with no stage behind it is exactly the kind of declaration a
// reader cannot tell from a stage that ran and removed nothing.
const (
	stageValidity   = "validity"
	stagePredicates = "predicates"
	stageProvenance = "provenance"
	stageConflicts  = "conflicts"
	stageDedup      = "dedup"
	stageDiversity  = "diversity"
	// stageCutoff is the relative-to-top relevance cutoff (#954), a query-mode
	// reduction that runs between diversity and the budget. It is the query-mode
	// dual of stage 7: diversity divides a PASSIVE window by category, and this
	// stops a QUERY answer where relevance falls off, and they never both act on
	// one request because each is gated on the opposite mode.
	stageCutoff = "cutoff"
	// stageNoAnswer is the absolute no-answer bar (#955), a query-mode step that
	// runs right after the relevance cutoff and before the budget: it withholds a
	// block whose best vector cosine is below the bar, so a question nothing in
	// the store answers is told so rather than handed plausible rows.
	stageNoAnswer    = "no_answer"
	stageBudget      = "budget"
	stageRender      = "render"
	stageResponseFit = "response_fit"
)

// The three caps stage 9 applies, as the sentence names them. They are recorded
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
	t.WindowExtra = set.WindowExtra
	for bucket, n := range set.PinnedBeyond {
		if n > 0 {
			t.addPinnedCut(bucket, n)
		}
	}
	return t
}

// record appends one stage's entry, always. In and Out are what the stage saw
// and what it left, so a reader can tell a stage that changed nothing from one
// that was never reached. It takes no "reordered" argument because no stage
// reorders — see StageTrace.
func (t *Trace) record(stage string, in, out int, dropped []string, notes ...string) {
	t.Stages = append(t.Stages, StageTrace{
		Stage: stage, In: in, Out: out, DroppedIDs: dropped, Notes: notes,
	})
}

// decide records one row's exclusion, with the project the row belongs to and
// the reason the stage gives.
func (t *Trace) decide(id, projectID, stage, reason string, before float64) {
	t.Decisions = append(t.Decisions, Decision{
		ID: id, ProjectID: projectID, Stage: stage, Reason: reason, Kept: false, Before: before,
	})
}

// keep records that a row survived a stage, with the reason the stage noted
// while letting it through. An unreadable validity value is the case v1 has: the
// value is reported, and the row is kept, so the two facts have to be recorded
// separately. Recording a kept row through decide would tell a consumer of the
// trace that a row in the answer was excluded from it.
func (t *Trace) keep(id, projectID, stage, reason string, before float64) {
	t.Decisions = append(t.Decisions, Decision{
		ID: id, ProjectID: projectID, Stage: stage, Reason: reason, Kept: true, Before: before,
	})
}

// withdrawDeferral turns a stage-7 deferral decision for id into the keep it
// turned out to be, and reports whether it found one. It also takes the id out of
// stage 7's `DroppedIDs` and rewrites that stage's summary note, so the stage
// record keeps agreeing with the verdicts rather than naming a deferral the trace
// no longer holds.
//
// WHY IT EXISTS, AND WHEN IT RUNS. A deferral moves a row behind a window, so the
// rows in front of it are what normally cut it. A slice's `MaxItems` does exactly
// that. A slice's `MaxBytes` cuts by CONTENT while the item count is still under
// its cap, so it can remove every row in front of a deferred one and leave the
// deferred rows — which the budget stage then admits. That is a shipped shape
// (`MaxItems` and `MaxBytes` on one slice), and the failure it would otherwise
// produce is a row the answer RENDERS carrying a `diversity_deferred` verdict:
// counted as withheld by the bucket tally the passive header reads, and listed as
// dropped by the stage record. `trim` is where membership is decided, so this is
// where the reversal belongs.
//
// `TestTrimWithdrawsADeferralForEveryRowItKeeps` pins the shipped path and
// `TestTrimWithdrawsADeferralForARowItKeeps` pins the reversal itself, so the
// window arithmetic above it cannot break this quietly.
func (t *Trace) withdrawDeferral(id string) bool {
	found := false
	for i := range t.Decisions {
		d := &t.Decisions[i]
		if d.ID != id || d.Stage != stageDiversity || d.Kept {
			continue
		}
		d.Kept = true
		d.Reason = reasonDiversityBackfilled
		found = true
		break
	}
	if !found {
		return false
	}
	for i := range t.Stages {
		st := &t.Stages[i]
		if st.Stage != stageDiversity {
			continue
		}
		st.DroppedIDs = removeID(st.DroppedIDs, id)
		// The summary line is the one note naming the row count; the per-row
		// notes name the rows themselves and are left alone.
		for j, note := range st.Notes {
			if strings.HasPrefix(note, "diversity deferred ") && strings.Contains(note, "candidates behind") {
				st.Notes[j] = diversitySummaryNote(len(st.DroppedIDs))
			}
		}
		return true
	}
	return true
}

// removeID drops one id from a slice, keeping the order of the rest.
func removeID(ids []string, id string) []string {
	out := ids[:0:0]
	for _, have := range ids {
		if have != id {
			out = append(out, have)
		}
	}
	return out
}

// addPinnedCut counts n pinned rows cut from a bucket.
func (t *Trace) addPinnedCut(bucket string, n int) {
	if t.PinnedCut == nil {
		t.PinnedCut = map[string]int{}
	}
	t.PinnedCut[bucket] += n
}
