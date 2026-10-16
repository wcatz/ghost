package bench

import "testing"

// nearDuplicatePairs are the corpus row pairs that restate ONE claim. #965 kept
// them as deliberate near-duplicate fixtures rather than dropping a row, and its
// labelling rule followed from that: because the two answer the same question,
// they must be graded the same. Three of the corpus's pairs are one claim
// written twice; the fourth row of each pair exists only as a distractor and the
// relation is not lexical, so the pairs are named here rather than derived from
// a similarity threshold.
var nearDuplicatePairs = [][2]string{
	{"grafana_port", "grafana_ingress_port"},
	{"arc_runners", "arc_self_hosted"},
	{"time_decay_categories", "ghost_decay"},
}

// TestTwinOfATopGradedAnswerIsGraded pins the #965 labelling rule where it
// matters to the score: when a query grades one member of a near-duplicate pair
// at the query's maximum gain, the other member must carry that same gain. A
// query's best answer being one half of a pair while the other half is left
// ungraded is the defect #1400 reports — a correct row the ranking returns in
// the top five still counts as a miss, so the numbers move for a labelling
// reason rather than a retrieval one.
//
// Scoped to the maximum gain on purpose: a pair member graded BELOW the top gain
// is a distractor for that query, and #965's rule does not require its twin to
// appear on a query neither one answers for. The check runs over the committed
// dataset, so the same edit that adds a query or renames a key has to keep the
// two halves in step.
func TestTwinOfATopGradedAnswerIsGraded(t *testing.T) {
	ds, _ := loadTestdataDataset(t)

	have := make(map[string]bool, len(ds.Memories))
	for _, m := range ds.Memories {
		have[m.Key] = true
	}
	// Anti-vacuity: a renamed key would make every check below pass by finding
	// neither half, which is the way a fixture guard silently stops guarding.
	for _, p := range nearDuplicatePairs {
		if p[0] == p[1] {
			t.Fatalf("near-duplicate pair names one key twice: %q", p[0])
		}
		for _, k := range p {
			if !have[k] {
				t.Fatalf("near-duplicate pair names the unknown memory key %q", k)
			}
		}
	}

	for _, q := range ds.Queries {
		top := 0
		for _, g := range q.Rel {
			if g > top {
				top = g
			}
		}
		if top == 0 {
			continue // a no-answer query grades nothing, by construction
		}
		for _, p := range nearDuplicatePairs {
			lo, hi := p[0], p[1]
			if q.Rel[lo] != top && q.Rel[hi] == top {
				lo, hi = hi, lo
			}
			if q.Rel[lo] != top {
				continue // neither half answers this query at the top gain
			}
			if q.Rel[hi] != top {
				t.Errorf("query %q grades %q at the top gain %d but leaves its near-duplicate %q at gain %d",
					q.Name, lo, top, hi, q.Rel[hi])
			}
		}
	}
}
