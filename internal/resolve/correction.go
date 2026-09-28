// Correction-aware demotion: resolve walks two deterministic, free signals —
// existing 'supersedes' edges and explicit correction text — that single-memory
// LLM classification cannot see, and demotes the OLDER memory those signals
// invalidate.
//
// The KEEP-bias stays intact: every deterministic demotion is either the older
// endpoint of a link supersede's LLM already judged, or a prefilter-passing
// candidate whose content the correction's rare subject tokens tie to an
// explicit "already fixed / withdrawn / retracted" statement by a newer memory.
// A false resolve buries a useful memory; a missed one leaves the status quo.
package resolve

import (
	"regexp"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)

var (
	// subjectTokenRe matches identifier-ish tokens (lowercased): bare words plus
	// the interior punctuation (_ . / -) that survives FTS tokenization, so
	// "ledgerstate/imported_reward_inputs.go" and "d646e680" stay single terms.
	subjectTokenRe = regexp.MustCompile(`[a-z0-9_./-]{4,}`)

	// subjectStopwords are too common across memories to carry subject identity;
	// excluded before document-frequency counting.
	subjectStopwords = map[string]bool{
		"that": true, "with": true, "this": true, "from": true, "being": true,
		"have": true, "been": true, "were": true, "will": true, "there": true,
		"also": true, "into": true, "which": true, "they": true, "each": true,
		"their": true, "about": true, "after": true, "before": true, "these": true,
		"those": true, "should": true, "could": true, "would": true, "within": true,
		"while": true, "every": true, "through": true, "during": true, "between": true,
		"because": true, "memory": true, "memories": true, "note": true, "notes": true,
		"2026": true, "09": true,
		"the": true, "and": true, "was": true, "for": true, "are": true, "but": true,
		"not": true, "you": true, "all": true, "can": true, "had": true, "her": true,
		"one": true, "our": true, "out": true, "has": true, "his": true, "via": true,
		"than": true, "only": true, "make": true, "made": true, "just": true,
		"then": true, "more": true, "most": true, "some": true, "any": true,
		"less": true, "does": true, "over": true, "very": true, "when": true,
		"what": true, "where": true, "who": true, "whom": true, "why": true,
		"how": true, "both": true,
	}

	// correctionMarkers are text signals that a memory is an explicit correction
	// of an earlier claim about the same subject. The correction itself is
	// authoritative and stays KEEP (a terminal conclusion); the older claims it
	// corrects are resolved evidence.
	correctionMarkers = []string{
		"correction", "correction/resolution", "corrects the",
		"is already fixed", "already fixed on main", "no pr is needed",
		"do not open a", "withdrawn", "retracted", "supersedes the",
	}

	// correctionRareDF caps a token's document frequency across the eligible
	// pool for it to count as carrying subject identity (df > correctionRareDF
	// means the term is too common to distinguish subjects).
	correctionRareDF = 4

	// correctionMinSharedRare gates correction-pairing: the older memory must
	// share at least this many rare subject tokens with the correction.
	// Calibrated on the live dingo pool: random same-project pairs share >=3
	// rare tokens in ~2% of cases (the demotion false-positive pool), while the
	// evidence pair under test shares 8.
	correctionMinSharedRare = 3
)

// subjectTermSet returns a memory's subject tokens: lowercased, stopword- and
// length-filtered, deduplicated.
func subjectTermSet(content string) map[string]bool {
	s := strings.ToLower(content)
	out := make(map[string]bool)
	for _, m := range subjectTokenRe.FindAllString(s, -1) {
		if !subjectStopwords[m] {
			out[m] = true
		}
	}
	return out
}

// isCorrection reports whether content explicitly corrects an earlier memory.
func isCorrection(content string) bool {
	lc := strings.ToLower(content)
	for _, m := range correctionMarkers {
		if strings.Contains(lc, m) {
			return true
		}
	}
	return false
}

// isOlder reports whether a is older than b by updated_at (SQLite
// 'YYYY-MM-DD HH:MM:SS' compares lexicographically), ties broken by ID — the
// same deterministic freshness order supersede uses to orient pairs.
func isOlder(a, b memory.Memory) bool {
	if a.UpdatedAt != b.UpdatedAt {
		return a.UpdatedAt < b.UpdatedAt
	}
	return a.ID < b.ID
}

// correctionPairTargets returns the OLDER, prefilter-passing eligible memories
// that at least one correction-marked memory demotes: they share
// >= correctionMinSharedRare rare subject tokens with a NEWER correction. The
// correction memory itself is never demoted. Deterministic and free — no LLM
// call — because the correction's text ("already fixed on main", "no PR is
// needed from us") is itself the resolution verdict.
//
// The frequency pool and the correction pool are both `loaded`, which is what
// Run passes. The repair pass needs them chosen separately and calls
// correctionPairTargetsFrom directly.
func correctionPairTargets(loaded, cands []memory.Memory) []memory.Memory {
	return correctionPairTargetsFrom(loaded, cands, loaded)
}

// correctionPairTargetsFrom is correctionPairTargets with the correction pool
// separable from the frequency pool. `loaded` counts subject-token document
// frequency — Run's pool, so the rare-token set is identical — while `cands` may
// be rows outside it (the repair pass passes the RESOLVED pool, which
// ResolveCandidates never returns), and `corrections` is where a demoting
// correction is looked for. Keeping those three apart is what lets the repair
// pass mirror Run exactly instead of approximately: a correction only asserts
// anything while it is in the pool Run searches, i.e. while it is unresolved.
//
// It is correctionPairingsFrom with the corrections dropped, and it deduplicates
// the targets, because a target paired by two corrections is still one demoted
// row (Run counts Corrected per row) and the order a target first appears in is
// the order the same loop has always produced.
func correctionPairTargetsFrom(loaded, cands, corrections []memory.Memory) []memory.Memory {
	seen := make(map[string]bool, len(cands))
	var out []memory.Memory
	for _, p := range correctionPairingsFrom(loaded, cands, corrections) {
		if seen[p.Target.ID] {
			continue
		}
		seen[p.Target.ID] = true
		out = append(out, p.Target)
	}
	return out
}

// Pairing is one correction and the older, prefilter-passing candidate it
// demotes. The pair is the unit the repair pass reports, not the target: a row
// the pass leaves resolved has to say WHICH correction is asserting it, because
// the two reasons a row stays resolved need different remedies — a supersedes
// edge can be withdrawn, a correction pairing cannot be, it can only be read.
type Pairing struct {
	Correction memory.Memory
	Target     memory.Memory
}

// correctionPairingsFrom is correctionPairTargetsFrom with the corrections kept.
// One entry per (correction, target) pair rather than per target: two
// corrections can pair the same row, and naming only the first would send an
// operator to check a pairing that is not the only thing holding the row.
// Deterministic and free, for correctionPairTargets' reasons — and the ONE
// implementation of the rare-token rule, so a caller that wants the correction
// and one that wants only the targets cannot disagree about which pairs exist.
func correctionPairingsFrom(loaded, cands, corrections []memory.Memory) []Pairing {
	if len(cands) == 0 {
		return nil
	}

	// Rare terms per memory: subject tokens with document frequency
	// <= correctionRareDF across the frequency pool. Terms are also collected
	// for cands and for corrections outside that pool, because both are
	// compared against each other; only `loaded` raises a token's frequency, so
	// Run's rare-token set is unchanged.
	termSets := make(map[string]map[string]bool, len(loaded)+len(cands)+len(corrections))
	for _, m := range cands {
		termSets[m.ID] = subjectTermSet(m.Content)
	}
	for _, m := range corrections {
		if _, ok := termSets[m.ID]; !ok {
			termSets[m.ID] = subjectTermSet(m.Content)
		}
	}
	df := make(map[string]int)
	for _, m := range loaded {
		terms, ok := termSets[m.ID]
		if !ok {
			terms = subjectTermSet(m.Content)
			termSets[m.ID] = terms
		}
		for t := range terms {
			df[t]++
		}
	}
	rareFor := func(id string) map[string]bool {
		r := make(map[string]bool)
		for t := range termSets[id] {
			if df[t] <= correctionRareDF {
				r[t] = true
			}
		}
		return r
	}

	// Precompute rare sets for the candidate pool once (not per correction).
	candRare := make(map[string]map[string]bool, len(cands))
	for _, m := range cands {
		candRare[m.ID] = rareFor(m.ID)
	}

	// Only an explicitly correction-marked memory demotes; the pool it came
	// from is otherwise irrelevant to the decision.
	var correctionMems []memory.Memory
	for _, m := range corrections {
		if isCorrection(m.Content) {
			correctionMems = append(correctionMems, m)
		}
	}
	if len(correctionMems) == 0 {
		return nil
	}

	// One entry per (correction, target) pair, in correction order then candidate
	// order. The target is not deduplicated here: which corrections pair a row is
	// exactly the question a repair report has to answer about it.
	var out []Pairing
	for _, c := range correctionMems {
		cRare := rareFor(c.ID)
		for _, m := range cands {
			if m.ID == c.ID {
				continue
			}
			// Only demote OLDER candidates: the correction invalidates the
			// claims that came before it.
			if !isOlder(m, c) {
				continue
			}
			shared := 0
			for t := range cRare {
				if candRare[m.ID][t] {
					shared++
				}
			}
			if shared >= correctionMinSharedRare {
				out = append(out, Pairing{Correction: c, Target: m})
			}
		}
	}
	return out
}
