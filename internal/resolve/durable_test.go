package resolve

import (
	"testing"
)

// #674: resolve's own rubric already asked the fresh-session question, and
// that question answers a note which only restates the repository: an agent
// starting fresh reads the code, not the note. These tests pin that the rubric
// now says so out loud — and, just as importantly, that nothing deterministic
// was added alongside it.

// TestNoDeterministicVetoForRepositoryFacts: the issue says the resolve rubric
// already covers this and asks for NO new veto, and a new pattern here would be
// a second vocabulary deciding what a memory is for — one that would be
// mistaken for judgement. The cost is not symmetric either: in `Run` a false
// veto costs a wasted injection slot, but `Reassess` runs the same veto over
// already-resolved rows, where one that fires un-hides a note permanently
// (#702). A repository fact has to stay a question for the classifier.
func TestNoDeterministicVetoForRepositoryFacts(t *testing.T) {
	for _, content := range []string{
		"foo.go contains HandleFoo()",
		"internal/reflection/prompt.go defines the drop guard.",
		"The changelog for v0.9.3 notes the connection leak fix.",
	} {
		if reason, vetoed := VetoKeep(content); vetoed {
			t.Errorf("VetoKeep(%q) = %q, vetoed — #674 adds rubric guidance, not a new veto pattern", content, reason)
		}
	}

	// The standing KEEP signals are untouched: an imperative is still settled
	// free, with no harness call.
	if reason, vetoed := VetoKeep("Never store a credential in a memory; production schema changes require explicit approval."); !vetoed {
		t.Error("the imperative veto stopped firing — the new rule must not have displaced it")
	} else if reason != "never" {
		t.Errorf("imperative veto reason = %q, want %q", reason, "never")
	}
}
