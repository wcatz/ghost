package assemble

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)

// ExplainFromTrace projects the assembler's Trace into the memory.SearchExplain
// format used by the MCP tool's explain:true response. This ensures explain and
// the formatted answer can never disagree about which rows were included, because
// both are derived from the same assembler Run.
func ExplainFromTrace(trace *Trace, req Request, items []Item) (memory.SearchExplain, error) {
	if trace == nil {
		return memory.SearchExplain{}, fmt.Errorf("explain: trace is nil")
	}

	ex := memory.SearchExplain{
		ProjectID:       trace.ProjectID,
		Query:           trace.Query,
		Limit:           RetrievalWindow(req),
		VectorAvailable: trace.VectorAvailable,
		Scope:           req.Scope,
		Notes:           make([]string, 0, len(trace.Notes)+len(trace.Stages)*2),
		Rows:            make([]memory.ExplainRow, 0, len(items)+len(trace.Decisions)),
	}

	// Copy the trace notes
	ex.Notes = append(ex.Notes, trace.Notes...)

	// Add stage summary notes
	for _, st := range trace.Stages {
		if st.In > 0 || st.Out > 0 || len(st.DroppedIDs) > 0 {
			note := fmt.Sprintf("stage %s: in=%d out=%d dropped=%d", st.Stage, st.In, st.Out, len(st.DroppedIDs))
			if len(st.Notes) > 0 {
				note += "; " + strings.Join(st.Notes, "; ")
			}
			ex.Notes = append(ex.Notes, note)
		}
	}

	// Add filter disclosure notes (matching the old ExplainSearchScoped behavior)
	if len(req.Scope) > 0 {
		ex.Notes = append(ex.Notes, "scope is applied inside hybrid window selection; included membership below matches the scoped search")
	}
	if req.Category != "" {
		ex.Notes = append(ex.Notes, "a category filter is not applied to these rows: they are the retrieval window the formatted path searches, before the category filter runs, so a row marked included may not be in that answer")
	}
	if req.Retention != "" {
		ex.Notes = append(ex.Notes, "a retention filter is not applied to these rows either: they are the retrieval window the formatted path searches, before the tier filter runs, so a row marked included may not be in that answer")
	}

	// Build a map of admitted item contents for quick lookup
	admitted := make(map[string]bool, len(items))
	itemByID := make(map[string]Item, len(items))
	for _, it := range items {
		admitted[it.ID] = true
		itemByID[it.ID] = it
	}

	// Build a map of decisions by ID for quick lookup
	decisionsByID := make(map[string][]Decision, len(trace.Decisions))
	for _, d := range trace.Decisions {
		decisionsByID[d.ID] = append(decisionsByID[d.ID], d)
	}

	// Build a map of signals by ID
	signalsByID := make(map[string]Signals, len(trace.Signals))
	for id, sig := range trace.Signals {
		signalsByID[id] = sig
	}

	// For each admitted item, create an ExplainRow with Included=true
	for _, it := range items {
		sig := signalsByID[it.ID]
		row := memory.ExplainRow{
			ID:          it.ID,
			Category:    it.Category,
			Content:     it.Content,
			Included:    true,
			Rank:        0, // Will be set below
			FTSRank:     sig.FTSRank,
			VectorRank:  sig.VectorRank,
			VectorScore: sig.VectorScore,
			RRFScore:    sig.Base,
			StatusFactor: func() float64 {
				if sig.Base != 0 && sig.DecayFactor != 0 {
					return 1.0 // The status factor is embedded in the decay path; we don't track it separately in v1
				}
				return 1.0
			}(),
			DecayFactor:  sig.DecayFactor,
			AgeDays:      sig.AgeDays,
			ProjectMatch: sig.ProjectMatch,
			ScopeMatched: sig.ScopeMatched,
			RowProject:   sig.RowProject,
			ScopeKeysCompared: func() []string {
				if len(sig.ScopeKeysCompared) == 0 {
					return nil
				}
				return sig.ScopeKeysCompared
			}(),
			ScopeKeysComparedTotal: len(req.Scope),
			ValidityState:          it.ValidityState,
			ValidityPenalty:        0,
			Confidence:             sig.Confidence,
			ConfidenceContribution: 0,
			ProvenanceWeight:       "off",
			ProvenanceContribution: 0,
			Retention:              it.Retention,
			RetentionFactor:        1.0, // Not tracked in v1
		}
		// Set rank from the item's position in the admitted list
		// We'll set this after collecting all admitted rows
		ex.Rows = append(ex.Rows, row)
	}

	// Now assign 1-based ranks to admitted rows
	for i := range ex.Rows {
		if ex.Rows[i].Included {
			ex.Rows[i].Rank = i + 1
		}
	}

	// For each dropped decision, create an ExplainRow with Included=false and the reason
	for _, d := range trace.Decisions {
		if d.Kept {
			continue // Already handled above
		}
		sig := signalsByID[d.ID]
		row := memory.ExplainRow{
			ID:          d.ID,
			Category:    "", // Not available for dropped rows without the item
			Content:     sig.Content,
			Included:    false,
			Rank:        0,
			FTSRank:     sig.FTSRank,
			VectorRank:  sig.VectorRank,
			VectorScore: sig.VectorScore,
			RRFScore:    d.Before,
			StatusFactor: func() float64 {
				if d.Before != 0 {
					return 1.0
				}
				return 1.0
			}(),
			DecayFactor:  sig.DecayFactor,
			AgeDays:      sig.AgeDays,
			ProjectMatch: sig.ProjectMatch,
			ScopeMatched: sig.ScopeMatched,
			RowProject:   sig.RowProject,
			ScopeKeysCompared: func() []string {
				if len(sig.ScopeKeysCompared) == 0 {
					return nil
				}
				return sig.ScopeKeysCompared
			}(),
			ScopeKeysComparedTotal: len(req.Scope),
			ValidityState:          sig.ValidityState,
			ValidityPenalty:        0,
			Confidence:             sig.Confidence,
			ConfidenceContribution: 0,
			ProvenanceWeight:       "off",
			ProvenanceContribution: 0,
			Reason:                 fmt.Sprintf("dropped at %s: %s", d.Stage, d.Reason),
		}
		ex.Rows = append(ex.Rows, row)
	}

	// Add floor information
	if trace.Floors.FTSApplied {
		ex.Notes = append(ex.Notes, fmt.Sprintf("FTS rank floor: max rank %d (applied)", trace.Floors.FTSRankMax))
	}
	if trace.Floors.VectorArmOn {
		if trace.Floors.VectorApplied {
			ex.Notes = append(ex.Notes, fmt.Sprintf("vector cosine floor: %.4f (applied)", trace.Floors.VectorCosine))
		} else {
			ex.Notes = append(ex.Notes, fmt.Sprintf("vector cosine floor: %.4f (configured but not applied — vector leg did not run or failed)", trace.Floors.VectorCosine))
		}
	}

	// Add abstention cosine info
	if req.AbstainCosine > 0 {
		ex.Notes = append(ex.Notes, fmt.Sprintf("abstain cosine floor: %.4f (configured)", req.AbstainCosine))
	} else {
		ex.Notes = append(ex.Notes, "abstain cosine floor: off")
	}

	// Add leg status
	legs := make([]string, 0, len(trace.Legs))
	for name, leg := range trace.Legs {
		status := "ok"
		if leg.Applicable && leg.Attempted && !leg.Available {
			status = fmt.Sprintf("failed: %s", leg.Err)
		} else if !leg.Applicable {
			status = "not applicable"
		} else if !leg.Attempted {
			status = "not attempted"
		}
		legs = append(legs, fmt.Sprintf("%s: %s", name, status))
	}
	if len(legs) > 0 {
		ex.Notes = append(ex.Notes, "legs: "+strings.Join(legs, ", "))
	}

	// Add retrieval_partial if any leg failed
	for _, leg := range trace.Legs {
		if leg.Applicable && leg.Attempted && !leg.Available {
			ex.Notes = append(ex.Notes, "retrieval_partial: a retrieval leg failed and the answer is incomplete")
			break
		}
	}

	// Add as_of qualifier
	if trace.AsOf != "" {
		ex.Notes = append(ex.Notes, fmt.Sprintf("historical read at %s", trace.AsOf))
	}

	return ex, nil
}

// ExplainFromTraceJSON is a convenience that returns the JSON bytes directly.
func ExplainFromTraceJSON(trace *Trace, req Request, items []Item) ([]byte, error) {
	ex, err := ExplainFromTrace(trace, req, items)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(ex, "", "  ")
}