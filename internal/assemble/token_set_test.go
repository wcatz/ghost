package assemble

// The bare-token set moved from here to memory.IsSafeTokenRune, because the
// retrieval audit's usefulness line renders a session id into a prompt from
// internal/memory and the dependency runs assemble -> memory: a reader in
// internal/memory cannot call a function in the package that imports it.
//
// This file holds what the move has to keep true, which is not "Token still
// behaves" — Token is a one-line delegation, so a behavioural test of it is a test
// of memory.SafeToken wearing another name. What can actually go wrong is DRIFT:
// isTokenRune is still consulted a rune at a time by three renderers in this
// package (Label's escape test, and the two pipeline decisions about what an id
// may be shown as), so it can disagree with the whole-string renderer it sits
// beside without anything failing. A divergence means one surface decides "bare is
// safe" on a different rule than the renderer that writes it.

import (
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestTheSharedTokenRuneSetIsOneSet: the local predicate and the shared one agree
// on every rune, not merely on the ones this repository happens to use.
//
// Exhaustive over all 0x110000 code points rather than table-driven over a sample,
// because a sample is the shape of the bug: the two functions are the same today
// by delegation, so any test that passes now passes forever — UNLESS someone later
// gives the local one a body of its own, and then the question is whether the test
// notices on the characters nobody happened to list. Exhaustion makes the answer
// unconditional. It costs a few milliseconds and it is the only version of this
// test that cannot rot into a decoration.
func TestTheSharedTokenRuneSetIsOneSet(t *testing.T) {
	for r := rune(0); r <= 0x10FFFF; r++ {
		if isTokenRune(r) != memory.IsSafeTokenRune(r) {
			t.Fatalf("isTokenRune and memory.IsSafeTokenRune disagree at %U (%q): the renderer "+
				"Token delegates writes this rune %s while this package's rune-at-a-time "+
				"callers decide the opposite, and two answers to one rule is a rule nobody holds",
				r, r, bareOrNot(isTokenRune(r)))
		}
	}
	// And the set is not empty, nor everything: a delegation that returned a
	// constant would satisfy the loop above exactly.
	if !isTokenRune('a') || isTokenRune('\n') {
		t.Errorf("the shared set is degenerate: 'a' bare = %v, newline bare = %v. Token's whole "+
			"guarantee rests on this set being the narrow one it documents",
			isTokenRune('a'), isTokenRune('\n'))
	}
}

// TestTokenIsMemorySafeTokenRatherThanACopy: Token must remain a delegation, not a
// re-implementation. This asserts it over the same exhaustive sweep for the reason
// above — the failure it guards against is someone writing the loop back out
// beside the call, and a copy is invisible to every other test in this package.
func TestTokenIsMemorySafeTokenRatherThanACopy(t *testing.T) {
	for r := rune(0); r <= 0x10FFFF; r++ {
		s := string(r)
		if got, want := Token(s), memory.SafeToken(s); got != want {
			t.Fatalf("Token(%q) = %q but memory.SafeToken(%q) = %q: Token has become a second "+
				"implementation of the rule rather than a name for it", s, got, s, want)
		}
	}
}

func bareOrNot(bare bool) string {
	if bare {
		return "bare"
	}
	return "quoted"
}
