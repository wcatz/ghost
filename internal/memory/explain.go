package memory

import "fmt"

// ExplainProvenanceOff is the value provenance_weight carries when no
// provenance multiplier is applied. It is the string "off" rather than a number
// on purpose: a multiplier of 1.0 is a claim that the ranking considered
// provenance and decided it did not matter, which is not what happens — nothing
// reads provenance at all. A reader seeing "off" cannot mistake a missing
// implementation for a neutral one.
const ExplainProvenanceOff = "off"

// ExplainRow is one candidate's contribution to a search, with every signal
// that moved it stated separately.
type ExplainRow struct {
	ID          string  `json:"memory_id"`
	Category    string  `json:"category"`
	Content     string  `json:"content"`
	Included    bool    `json:"included"`
	Rank        int     `json:"rank"`         // 1-based; 0 when excluded
	FTSRank     int     `json:"fts_rank"`     // -1 when the FTS leg had no match
	VectorRank  int     `json:"vector_rank"`  // -1 when the vector leg had no match
	VectorScore float64 `json:"vector_score"` // cosine; -1 when absent
	RRFScore    float64 `json:"rrf_score"`    // base before status demotion and decay: the sum over the legs that retrieved the row of weight/(K+rank+1); the notes name the weights and K

	StatusFactor         float64 `json:"status_factor"`
	DecayFactor          float64 `json:"decay_factor"`           // category/age AND TIER multiplier the ranking applied to the base (a `session` row carries both halves — see RetentionFactor); for a candidate the window cut before decayRank ordered it, the one it WOULD have applied, measured against the ranking's own clock
	AgeDays              float64 `json:"age_days"`               // as at that same clock, for the same reason
	SupersedePenalty     int     `json:"supersede_penalty"`      // window-scoped as demoteResults applies it; 0 for rows outside the window
	NearDuplicatePenalty int     `json:"near_duplicate_penalty"` // window-scoped and order-sensitive, exactly as DemotionPenalties assigns it

	Retention       string  `json:"retention"`
	RetentionFactor float64 `json:"retention_factor"` // bounded tier decay: tau 7d, floor 0.5
	Reason          string  `json:"reason,omitempty"` // why it is absent from the results

	ProjectMatch bool `json:"project_match"`

	RowProject string `json:"row_project,omitempty"`

	ScopeMatched bool `json:"scope_matched"`

	ScopeKeysCompared      []string `json:"scope_keys_compared,omitempty"`
	ScopeKeysComparedTotal int      `json:"scope_keys_compared_total,omitempty"`

	KeywordReserved bool `json:"keyword_reserved,omitempty"`

	TookSlotFrom string `json:"took_slot_from,omitempty"`
	DisplacedBy  string `json:"displaced_by,omitempty"`

	MovedAheadOf string `json:"moved_ahead_of,omitempty"`

	FloorDropped bool    `json:"floor_dropped,omitempty"`
	FloorScore   float64 `json:"floor_score,omitempty"`

	SupersededBy    []string `json:"superseded_by,omitempty"`
	NearDuplicateOf []string `json:"near_duplicate_of,omitempty"`

	ValidityState string `json:"validity_state,omitempty"`

	ValidityPenalty float64 `json:"validity_penalty"`

	Confidence             *float64 `json:"confidence,omitempty"`
	ConfidenceContribution float64  `json:"confidence_contribution"`

	ProvenanceWeight string `json:"provenance_weight"`

	ProvenanceContribution float64 `json:"provenance_contribution"`
}

// SearchExplain is the diagnosis for one search: which legs ran, what each
// candidate scored, and why anything was left out.
type SearchExplain struct {
	ProjectID       string            `json:"project_id"`
	Query           string            `json:"query"`
	Limit           int               `json:"limit"`
	VectorAvailable bool              `json:"vector_available"`
	Scope           map[string]string `json:"scope,omitempty"` // requested scope applied to membership
	Notes           []string          `json:"notes,omitempty"`
	Rows            []ExplainRow      `json:"rows"`

	Truncation *ExplainTruncation `json:"truncation,omitempty"`
}

// ExplainTruncation records that the payload was cut to fit the documented
// budget, and says what was cut and why. RowsOmitted counts candidate rows the
// budget would not carry; MaxRows is the budget itself, published so a reader can
// judge whether the cut could plausibly have hidden the row it cares about.
type ExplainTruncation struct {
	RowsOmitted int    `json:"rows_omitted"`
	MaxRows     int    `json:"max_rows"`
	Reason      string `json:"reason"`
}

// ExplainMaxRows is the documented size budget for one explanation, in rows.
const ExplainMaxRows = 150

// ExplainSnippetRunes bounds the content a row carries: the identifying opening
// words matter more than the full text, and the answer itself holds the rest.
const ExplainSnippetRunes = 120

// BoundExplainRows applies the documented size budget, and records the cut when
// it applies one.
func BoundExplainRows(rows []ExplainRow) ([]ExplainRow, *ExplainTruncation) {
	if len(rows) <= ExplainMaxRows {
		return rows, nil
	}
	included := make([]bool, len(rows))
	includedCount := 0
	for i, r := range rows {
		if r.Included {
			included[i] = true
			includedCount++
		}
	}

	room := ExplainMaxRows - min(includedCount, ExplainMaxRows)
	keep := make([]bool, len(rows))
	for i := range rows {
		if included[i] {
			keep[i] = true
		}
	}
	omitted := 0
	for i := range rows {
		if keep[i] || room == 0 {
			continue
		}
		keep[i] = true
		room--
	}
	kept := make([]ExplainRow, 0, len(rows))
	keptCount := 0
	for i, r := range rows {
		if keep[i] {
			kept = append(kept, r)
			keptCount++
			continue
		}
		omitted++
	}

	reason := fmt.Sprintf(
		"the explanation was truncated to its documented %d-row budget: %d excluded candidates were omitted "+
			"and all %d rows the search returned were kept, so a row absent from this payload was either never a "+
			"candidate or was cut by the budget",
		ExplainMaxRows, omitted, includedCount)
	if keptCount > ExplainMaxRows {
		reason = fmt.Sprintf(
			"the %d rows the search returned exceed the documented %d-row budget on their own, so this payload "+
				"is %d rows long and every excluded candidate (%d of them) was dropped: the budget is not applied to "+
				"rows in the answer, because a caller that asked for this window needs the window back",
			includedCount, ExplainMaxRows, keptCount, omitted)
	}

	trunc := &ExplainTruncation{
		RowsOmitted: omitted,
		MaxRows:     max(keptCount, ExplainMaxRows),
		Reason:      reason,
	}
	return kept, trunc
}

// ExplainSnippet shortens content for a diagnostic listing. Explanations are
// read by a human or an agent debugging a query, so the identifying opening
// words matter more than the full text.
func ExplainSnippet(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
