package assemble

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// The recategorised-replacement storyline's pair, word for word: the lookup
// cache saved as a fact, and the team's change to it saved as a decision.
const (
	cacheFact     = "Lookup responses are cached in Memcached: internal/cache dials memcached-01.corp.example:11211 and every entry lives 300s."
	cacheDecision = "Decision: lookup responses are cached in-process with ristretto instead of Memcached; internal/cache keeps a 64 MB budget per pod, entries still live 300s, and memcached-01 is being shut down."
)

// TestRunPassiveOrdersARecategorisedReplacementFirstAndMarksTheOlderRow: a
// replacement filed under another category than the claim it replaces (a
// decision replacing a fact) was never paired, so the session start showed the
// replaced fact unmarked. It is now ordered ahead and the fact is marked, never
// withheld, as the same texts in one category are.
func TestRunPassiveOrdersARecategorisedReplacementFirstAndMarksTheOlderRow(t *testing.T) {
	r := newReplacementStore(t)
	older := r.add(memory.Memory{Category: "fact", Content: cacheFact, Importance: 0.8}, 20)
	newer := r.add(memory.Memory{Category: "decision", Content: cacheDecision, Importance: 0.7}, 24)

	res := run(t, r.s, replacementPassiveRequest())
	oi, ni := indexOf(res.Items, older), indexOf(res.Items, newer)
	if oi < 0 || ni < 0 {
		t.Fatalf("answer %v must hold both rows (older %s, newer %s): the older row is marked, not withheld", itemIDs(res.Items), older, newer)
	}
	if ni > oi {
		t.Errorf("answer %v puts the replaced fact %s (index %d) ahead of the decision %s (index %d)", itemIDs(res.Items), older, oi, newer, ni)
	}
	if old := res.Items[oi]; len(old.MaybeReplacedBy) != 1 || old.MaybeReplacedBy[0] != newer || !strings.Contains(old.Line(), MaybeReplacedMarker) {
		t.Errorf("older row MaybeReplacedBy = %v, want [%s] and the marker: %s", old.MaybeReplacedBy, newer, old.Line())
	}
}

// TestRunQueryOrdersARecategorisedReplacementAheadOfTheRowItReplaced: the
// storyline's session-3 search ranked the replaced fact first and the decision
// second, unmarked. The pair is named on the query path too, so the decision is
// shown directly ahead of the fact, which is marked.
func TestRunQueryOrdersARecategorisedReplacementAheadOfTheRowItReplaced(t *testing.T) {
	r := newReplacementStore(t)
	older, newer := addPair(r,
		memory.Memory{Category: "fact", Content: cacheFact, Importance: 0.8},
		memory.Memory{Category: "decision", Content: cacheDecision, Importance: 0.7})
	req := baseRequest()
	req.Query = "where lookup responses are cached"
	req.Budget = Budget{MaxItems: 5}

	res := run(t, r.s, req)
	oi, ni := indexOf(res.Items, older), indexOf(res.Items, newer)
	if oi < 0 || ni < 0 {
		t.Fatalf("answer %v must hold both rows (older %s, newer %s)", itemIDs(res.Items), older, newer)
	}
	if ni > oi {
		t.Errorf("answer %v: the decision %s must be shown ahead of the fact %s it replaced", itemIDs(res.Items), newer, older)
	}
	if old := itemByID(t, res.Items, older); len(old.MaybeReplacedBy) != 1 || old.MaybeReplacedBy[0] != newer {
		t.Errorf("older row MaybeReplacedBy = %v, want [%s]", old.MaybeReplacedBy, newer)
	}
}
