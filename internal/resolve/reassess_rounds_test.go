package resolve

import (
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestHoldBackPairingsAreNotMonotoneAcrossRounds is the review finding on #737.
//
// The comment on maxHoldBackRounds claimed the re-check "can only reveal MORE
// pairings" round on round, and that this is why the loop terminates. Only the
// first half of that chain was true. A held-back row leaves the pool the
// re-check reads, and that pool is BOTH the frequency pool and the correction
// pool, so dropping a row does two opposite things at once: it lowers the
// document frequency of the tokens the row carries, which can reveal a pairing a
// round earlier was blind to, AND — if the row is correction-marked — it removes
// every pairing that row was asserting. A later round can therefore report FEWER
// pairings than the round before it.
//
// The loop still terminates. It is not the pairings that guarantee that; it is
// `kept` strictly shrinking on any round that drops anything, which bounds the
// loop by len(repair) rounds whatever the pairings do. But a comment naming the
// wrong invariant is one a future change reads as licence to raise or drop the
// bound on the strength of a guarantee the loop does not have.
//
// This is the evidence for the corrected comment, so the claim is checked rather
// than argued: round one finds two pairings and round two finds NONE.
func TestHoldBackPairingsAreNotMonotoneAcrossRounds(t *testing.T) {
	// c-old is a correction that pairs target, and carries a second token set
	// that the live c-new pairs it with. c-new is unresolved, so it is never a
	// hold-back target and never leaves the pool.
	cNew := memory.Memory{ID: "c-new", UpdatedAt: "2026-09-03 00:00:00",
		Content: "CORRECTION/RESOLUTION: investigation note oldone oldtwo oldthree"}
	cOld := memory.Memory{ID: "c-old", UpdatedAt: "2026-09-02 00:00:00",
		Content: "CORRECTION/RESOLUTION: postmortem targone targtwo targthree oldone oldtwo oldthree"}
	target := memory.Memory{ID: "target", UpdatedAt: "2026-09-01 00:00:00",
		Content: "postmortem: targone targtwo targthree"}
	live := []memory.Memory{cNew}
	repair := []memory.Memory{cOld, target}

	// One round: c-old is paired by the live c-new, and target is paired by
	// c-old while c-old is still in the correction pool. Two pairings.
	one := holdBack(repair, live, 1)
	if len(one.holds) != 2 {
		t.Fatalf("round one holds = %v, want two pairings", one.holds)
	}
	if one.holds["c-old"][0].Holder != cNew.ID || one.holds["target"][0].Holder != cOld.ID {
		t.Fatalf("round one holds = %v, want c-old held by c-new and target held by c-old", one.holds)
	}

	// The whole re-check. It takes a second round — and that round finds NOTHING,
	// because holding c-old took it out of the correction pool and with it every
	// pairing it asserted. Two pairings then zero is the non-monotonicity: a
	// reader who took "can only reveal MORE pairings" from the comment would
	// expect the second round to hold at least the first round's two.
	all := holdBack(repair, live, maxHoldBackRounds)
	if all.rounds != 2 {
		t.Errorf("rounds = %d, want 2: the second round runs, finds nothing, and stops", all.rounds)
	}
	if all.boundHit {
		t.Error("a two-row repair set cannot reach a bound of 8")
	}
	if len(all.holds) != 2 {
		t.Errorf("the whole re-check holds %v, want round one's two and nothing more", all.holds)
	}
	// The dropped set is one-way, which is what makes the answer well defined
	// despite the pairings moving: target stays held even though round two no
	// longer finds the correction asserting it.
	if _, ok := all.holds["target"]; !ok {
		t.Error("target must stay held: dropping is one-way, so a later round that cannot see the pairing does not free it")
	}
	if len(all.kept) != 0 {
		t.Errorf("kept = %v, want both repair rows held", ids(all.kept))
	}
}
