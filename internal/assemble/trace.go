package assemble

import (
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// Trace records what each stage saw and decided.
type Trace struct {
	ProjectID, Query string
	AsOf             string
	Limit            int
	VectorAvailable  bool
	Notes            []string
	Mode             string
	Legs             map[string]memory.LegStatus
	Signals          map[string]Signals
	Stages           []StageTrace
	Decisions        []Decision
	Floors           Floors
	PinnedCut        map[string]int
	WindowExtra      map[string]int
}

// Floors records the thresholds a relevance floor used.
type Floors struct {
	FTSRankMax    int
	FTSApplied    bool
	VectorCosine  float32
	VectorArmOn   bool
	VectorApplied bool
}

// Signals holds one candidate's recorded contributions.
type Signals struct {
	FTSRank, VectorRank    int
	VectorScore            float64
	Base, DecayFactor      float64
	AgeDays                float64
	CreatedAt              time.Time
	ProjectMatch           bool
	ScopeMatched           bool
	ScopeKeysCompared      []string
	ValidityState          string
	ValidityPenalty        float64
	Confidence             *float64
	ConfidenceContribution float64
	ProvenanceWeight       string
	ProvenanceContribution float64
	Evidence               memory.EvidenceCounts
}

// StageTrace records one stage's counts and removed rows.
type StageTrace struct {
	Stage      string
	In, Out    int
	DroppedIDs []string
	Notes      []string
}

// Decision records one row's fate at one stage.
type Decision struct {
	ID        string
	ProjectID string
	Stage     string
	Reason    string
	Kept      bool
	Before    float64
	Against   []string
}

const (
	stageValidity     = "validity"
	stagePredicates   = "predicates"
	stageProvenance   = "provenance"
	stageConflicts    = "conflicts"
	stageDedup        = "dedup"
	stageDiversity    = "diversity"
	stageLegConsensus = "leg_consensus"
	stageCutoff       = "cutoff"
	stageNoAnswer     = "no_answer"
	stageBudget       = "budget"
	stageRender       = "render"
	stageResponseFit  = "response_fit"
)

const (
	boundSliceItems = "slice_item_cap"
	boundSliceBytes = "slice_byte_cap"
	boundTotalItems = "item_cap"
)

func newTrace(req Request, set *memory.CandidateSet) *Trace {
	t := &Trace{
		ProjectID:       req.ProjectID,
		Query:           req.Query,
		Limit:           retrievalWindow(req),
		VectorAvailable: len(req.QueryVec) > 0,
		Mode:            string(projectMode(req)),
		Legs:            set.Legs,
		Signals:         map[string]Signals{},
		Floors:          floorsOf(req),
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

func (t *Trace) record(stage string, in, out int, dropped []string, notes ...string) {
	t.Stages = append(t.Stages, StageTrace{
		Stage: stage, In: in, Out: out, DroppedIDs: dropped, Notes: notes,
	})
}

func (t *Trace) decide(id, projectID, stage, reason string, before float64) {
	t.Decisions = append(t.Decisions, Decision{
		ID: id, ProjectID: projectID, Stage: stage, Reason: reason, Kept: false, Before: before,
	})
}

func (t *Trace) keep(id, projectID, stage, reason string, before float64) {
	t.Decisions = append(t.Decisions, Decision{
		ID: id, ProjectID: projectID, Stage: stage, Reason: reason, Kept: true, Before: before,
	})
}

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
		for j, note := range st.Notes {
			if strings.HasPrefix(note, "diversity deferred ") && strings.Contains(note, "candidates behind") {
				st.Notes[j] = diversitySummaryNote(len(st.DroppedIDs))
			}
		}
		return true
	}
	return true
}

func removeID(ids []string, id string) []string {
	out := ids[:0:0]
	for _, have := range ids {
		if have != id {
			out = append(out, have)
		}
	}
	return out
}

func (t *Trace) addPinnedCut(bucket string, n int) {
	if t.PinnedCut == nil {
		t.PinnedCut = map[string]int{}
	}
	t.PinnedCut[bucket] += n
}
