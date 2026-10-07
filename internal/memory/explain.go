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
//
// Ranks are 0-based within their leg (the same convention RRF uses) so the
// arithmetic shown matches the arithmetic performed. A leg that did not match
// reports -1 rather than 0, because rank 0 is a real first place — an absent
// leg and a top-ranked one must not look alike.
//
// Every SCORING number here is read from the ranking path's own record of the
// candidate (RankFact, written by the stages as they ran), not recomputed from the
// legs after the fact. A score that explain rebuilt is a score that could disagree
// with the one that ordered the results. The fields that name a signal the ranking
// path does NOT apply say so with a zero or "off" rather than carrying an invented
// contribution; the payload's notes say which is which. The payload is built by
// internal/assemble from the same run as the formatted answer, and every stored
// string in it is rendered by assemble.Data or assemble.Token.
//
// The one deliberate exception is decay_factor and age_days for a candidate the
// window cut before decayRank ordered it, which carry the factor the ranking would
// have used, measured against the ranking's OWN clock, because a field is better
// than a gap. The fields that decide
// membership carry no such exception: a row that did not win a slot has no
// status_factor, no scope verdict and no base read on its behalf, only sentinels
// saying the ranking never reached it.
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
	// StatusFactor is the multiplicative resolved/_global demotion the ranking
	// APPLIED to rrf_score before the cut — 1.0, 0.5 or 0.25, and 1.0 when neither
	// applies.
	//
	// 1.0 also covers a row the ranking never scored: the vector floor removed it
	// before fusion, so no demotion ran on it at all. The field means one thing
	// everywhere for that reason — reporting the factor a never-scored row WOULD
	// carry puts a multiplier beside an rrf_score of 0, and the payload's own
	// contract says every number in it is one the ranking used. Whether such a row
	// is a shared one is carried by row_project beside it, and floor_dropped is
	// the field that says nothing was applied.
	StatusFactor         float64 `json:"status_factor"`
	DecayFactor          float64 `json:"decay_factor"`           // category/age AND TIER multiplier the ranking applied to the base (a `session` row carries both halves — see RetentionFactor); for a candidate the window cut before decayRank ordered it, the one it WOULD have applied, measured against the ranking's own clock
	AgeDays              float64 `json:"age_days"`               // as at that same clock, for the same reason
	SupersedePenalty     int     `json:"supersede_penalty"`      // window-scoped as demoteResults applies it; 0 for rows outside the window
	NearDuplicatePenalty int     `json:"near_duplicate_penalty"` // window-scoped and order-sensitive, exactly as DemotionPenalties assigns it
	// Retention is the row's own tier (session, project, persistent) and
	// RetentionFactor is the part of decay_factor it contributed: 1.0 for every
	// tier but session, which is what makes a durable memory's score exactly what
	// it was before tiers existed. Reported as a separate field rather than folded
	// into decay_factor because it is a different question — "how old is this"
	// against "how long do we want this" — and a reader asking why a
	// conversation-scoped row sits low is asking the second one.
	Retention       string  `json:"retention"`
	RetentionFactor float64 `json:"retention_factor"` // bounded tier decay: tau 7d, floor 0.5
	Reason          string  `json:"reason,omitempty"` // why it is absent from the results

	// --- the eligibility axes, as the ranking path decided them ---

	// ProjectMatch is whether the row belongs to the searched project. A
	// _global row is admitted by the legs' project predicate and is NOT a match:
	// it is a shared row, and StatusFactor is what demotes it in a
	// project-scoped search. A cross-project search matches everything, since it
	// expresses no project of its own.
	ProjectMatch bool `json:"project_match"`
	// RowProject is the project the row belongs to, as the leg supplied it.
	// It is what makes ProjectMatch checkable: a reader that sees
	// project_match=false and row_project="_global" knows the row was admitted
	// by the shared-row predicate rather than by a project mismatch.
	RowProject string `json:"row_project,omitempty"`
	// ScopeMatched is the verdict scope narrowing reached, and it agrees with
	// membership by construction because it IS the verdict that decided it — the
	// structural fix for #571, where explain reported rows the tool would exclude
	// because the filter ran after the ranking. Silence is not disagreement: a
	// row that does not mention a requested key is reported matched.
	ScopeMatched bool `json:"scope_matched"`
	// ScopeKeysCompared is the scope key set the narrowing compared — the
	// request's keys, identical on every row, so a reader can see WHICH axis
	// decided rather than only that something did. A row whose own scope names
	// none of them had nothing to compare against and was not excluded by scope;
	// scope_matched says that per row, and this says what the axis was.
	//
	// The list is capped, because the scope object is caller-supplied and as
	// unbounded as a JSON object, and ScopeKeysComparedTotal says how long the
	// list really was. The count is what makes the cap honest: a payload naming
	// sixteen keys of a forty-key scope would otherwise report scope_matched as
	// the verdict the narrowing reached over all forty, and the reader could not
	// tell. The attribution lists beside superseded_by and near_duplicate_of are
	// capped the same way and are counted the same way, for the same reason.
	ScopeKeysCompared      []string `json:"scope_keys_compared,omitempty"`
	ScopeKeysComparedTotal int      `json:"scope_keys_compared_total,omitempty"`
	// KeywordReserved says the row entered the window through the keyword
	// reservation rather than the score cut. Its score is below the cut BY
	// CONSTRUCTION, so this is the only thing that explains its presence, and it
	// is invisible from the numbers otherwise.
	KeywordReserved bool `json:"keyword_reserved,omitempty"`
	// TookSlotFrom names the row whose window slot KeywordReserved took, and
	// DisplacedBy names the reserved row that took this row's slot. Both sides
	// are recorded because the exchange is the interesting fact: neither row's
	// score explains it.
	TookSlotFrom string `json:"took_slot_from,omitempty"`
	DisplacedBy  string `json:"displaced_by,omitempty"`
	// FloorDropped says the vector similarity floor removed this row's VECTOR
	// CONTRIBUTION, and FloorScore is the cosine that did it.
	//
	// Read it as being about the vector leg, not about the row's fate: a row the
	// keyword leg also retrieved is floor-dropped and still IN THE ANSWER, with
	// a non-zero rrf_score, because only its vector term was cut. Check FTSRank to
	// tell the two apart — under 0 means the row is out entirely, and that is the
	// only case where rrf_score is 0. Distinct from VectorRank == -1, which is
	// also what a row the vector leg never matched reports.
	FloorDropped bool    `json:"floor_dropped,omitempty"`
	FloorScore   float64 `json:"floor_score,omitempty"`
	// SupersededBy names the present superseders that sank this row, and
	// NearDuplicateOf the rows it sank below as a near-duplicate loser. A count
	// alone says the row moved; the counterpart is the row a reader has to look
	// at next. Attribution is READ FROM the same edge set the penalty was
	// decided on, so the named row is the one that actually decided it.
	SupersededBy    []string `json:"superseded_by,omitempty"`
	NearDuplicateOf []string `json:"near_duplicate_of,omitempty"`

	// --- signals the ranking path does not apply ---

	// ValidityState is the row's own currency against the search clock, by the
	// one shared rule (memory.ValidityState) the context assembler's validity
	// stage also uses: valid, future, expired, unverified or unset. An expired or
	// not-yet-valid row is withheld by that stage, so it is excluded here with its
	// reason rather than ranked lower.
	ValidityState string `json:"validity_state,omitempty"`
	// ValidityPenalty is 0 on every row, and that is the honest value: no stage
	// multiplies by validity. Stage 2 of the assembler DROPS an out-of-window row
	// rather than ranking it lower, so there is no factor for a score to carry. It
	// is read from the stage's own recorded signal, which is where a future
	// non-zero value would arrive.
	ValidityPenalty float64 `json:"validity_penalty"`
	// Confidence is the stored provenance confidence, and
	// ConfidenceContribution is the score delta a bounded multiplier applied —
	// which is 0, because no such multiplier exists. Both are reported: the
	// column is readable and a reader comparing rows needs to see that two rows
	// with different confidence ranked identically, rather than inferring it
	// from a field that is absent.
	Confidence             *float64 `json:"confidence,omitempty"`
	ConfidenceContribution float64  `json:"confidence_contribution"`
	// ProvenanceWeight is the weight the assembler's provenance stage recorded
	// ("1.0", pinned: no score is multiplied by it), or "off" for a row withheld
	// before that stage ran. See ExplainProvenanceOff.
	ProvenanceWeight string `json:"provenance_weight"`
	// ProvenanceContribution is 0 for the same reason as
	// ConfidenceContribution.
	ProvenanceContribution float64 `json:"provenance_contribution"`
}

// SearchExplain is the diagnosis for one search: which legs ran, what each
// candidate scored, and why anything was left out.
//
// It is a projection of the assembler's run (assemble.Result.Explain), never a
// second search. Rows are marked included exactly when the formatted answer for
// the same request lists them, so an explanation describes the answer Ghost
// actually produced; recomputing membership anywhere else would let it drift
// from the behavior it explains.
type SearchExplain struct {
	ProjectID       string            `json:"project_id"`
	Query           string            `json:"query"`
	Limit           int               `json:"limit"`
	VectorAvailable bool              `json:"vector_available"`
	Scope           map[string]string `json:"scope,omitempty"` // requested scope applied to membership
	Notes           []string          `json:"notes,omitempty"`
	Rows            []ExplainRow      `json:"rows"`
	// Truncation is set when the payload hit its size budget. It is a field
	// rather than a note because a note is prose a reader may skip, and a short
	// candidate list that LOOKS complete is the failure this prevents: an agent
	// checking a row that "was not even a candidate" has to be able to tell that
	// the explanation never looked.
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
//
// The candidate pool is the union of what both legs returned, so it grows with
// the caller's limit rather than with the corpus: each leg fetches limit*2, so a
// hybrid search over a well-matched corpus produces a candidate set four times
// the window. The window itself is capped (the tool's retrieval-window ceiling
// is 100 rows), which is what makes a fixed row budget possible at all: 150
// holds every row a search can return, plus the candidates nearest the cut, and
// cuts only the far tail of the union beyond that. At the default limit of ten
// the union is at most 40 rows and the budget never engages.
//
// It spends itself on the CANDIDATES and never on the answer: rows the search
// returned are always kept, in their own order, and the rows dropped are the ones
// that were already excluded. A caller diagnosing a missing result is looking
// for a row the search did not return, and a budget that could drop those would
// spend itself exactly where it is needed least.
const ExplainMaxRows = 150

// ExplainSnippetRunes bounds the content a row carries: the identifying opening
// words matter more than the full text, and the answer itself holds the rest.
const ExplainSnippetRunes = 120

// BoundExplainRows applies the documented size budget, and records the cut when
// it applies one.
//
// The budget spends itself on candidates, never on the answer. Every included row
// is kept — regardless of where it falls in the list, which is not a reliable
// proxy for it, because the list runs leg by leg and a vector-leg rank-0 row sits
// after every keyword-leg row however it ranked. The rows dropped are excluded
// candidates, in list order, which is NOT a rank order: the list is every
// keyword-leg id, then every vector-leg id, then the final ids, so a positional
// cut takes a keyword-leg exclusion before a vector-leg one regardless of which
// came closer to the cut. Nothing in the payload claims otherwise, and no ordering
// claim survives that the list's construction does not support. The surviving rows
// keep their original order, so the payload looks the same with a budget as
// without one, only shorter.
//
// A caller that asks for a window wider than the budget gets an OVER-budget
// payload rather than a truncated answer, and MaxRows reports the true row count
// in that case. The tool cannot reach that state — it clamps the window to 100 —
// but returning a marker claiming a 150-row cap on a 400-row payload would be the
// one way this could mislead, and the alternative (dropping a returned row) is the
// guarantee the budget exists not to break.
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
	// The tool's window ceiling is 100 rows, so the answer always fits inside a
	// 150-row budget and the first pass never has to drop one. The clamp is
	// stated rather than assumed, so a future larger window cannot quietly start
	// losing returned rows — it makes the payload over budget and says so.
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
	// The reason names the true row count, and says which of the two states this
	// is: a cut that kept the whole answer, or an answer that was over budget on
	// its own and so had no room for a single excluded candidate.
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
	// MaxRows is what the payload ACTUALLY carries, not the budget it aimed for.
	// They differ only when the answer alone exceeds the budget, which the tool
	// cannot produce but an in-process caller can; reporting the budget there
	// would be a cap claim the payload violates on its face.
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
