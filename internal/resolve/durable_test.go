package resolve

import (
	"strings"
	"testing"
)

// #674: resolve's own rubric already asked the fresh-session question, and
// that question answers a note which only restates the repository: an agent
// starting fresh reads the code, not the note. These tests pin that the rubric
// now says so out loud — and, just as importantly, that nothing deterministic
// was added alongside it.

// TestClassifyRubricStatesTheRepositoryFactRule pins the rule on both prompts.
// Single-note and batch prompts carry the rubric verbatim, so a verdict means
// the same thing whichever call it came from, and a rubric that stated the new
// ground in only one of them would judge the same note two ways.
func TestClassifyRubricStatesTheRepositoryFactRule(t *testing.T) {
	for name, prompt := range map[string]string{
		"single": classifySystemPrompt,
		"batch":  classifyBatchSystemPrompt,
	} {
		for _, want := range []string{
			// the fresh-session question the issue says to KEEP, not replace
			"starting a fresh session make a mistake, repeat work, or break a rule",
			// the new ground, with the issue's own examples
			"foo.go contains HandleFoo()",
			"Production schema changes require explicit approval.",
			"the repository is authoritative",
			// the closing restatement, which is what tells the model the
			// QUESTION is the unit and not the path: without it a rubric that
			// names a repository ground reads as "any note citing a file".
			"Judge the note, not the mention",
		} {
			if !strings.Contains(prompt, want) {
				t.Errorf("%s prompt missing %q:\n%s", name, want, prompt)
			}
		}
	}
}

// TestClassifyRubricKeepsDurableKnowledgeWithPaths: the boundary is the whole
// safety of the rule. A note that cites a file and adds a reason the code does
// not state is durable knowledge, and the fresh-session question KEEPs it; a
// rubric that let the path alone decide would bury architecture, gotchas and
// decisions — the categories #640 measured being buried already.
func TestClassifyRubricKeepsDurableKnowledgeWithPaths(t *testing.T) {
	prompt := classifySystemPrompt

	// Per-SENTENCE, not per-prompt, and that is the whole point. This side has
	// no backstop: a wrongly-RESOLVED note leaves ranked injection permanently,
	// and unlike the reflect half there is no verbatim re-add to undo it. So the
	// boundary that makes the rule safe has to be read to APPLY the rule, which
	// means the same sentence as the ground it limits — a boundary parked in a
	// later paragraph is a second thing to reconcile, and reword the two halves
	// together or the model reconciles them apart. The reflect half already reads
	// "is an obsolete-drop candidate WHEN the note is a location or a definition…".
	ground := sentenceContaining(t, prompt, "is RESOLVED on this ground")
	if !strings.Contains(ground, "only when it is a location or a definition") {
		t.Errorf("the RESOLVED ground does not carry its boundary in the same sentence:\n%s", ground)
	}
	if !strings.Contains(ground, "nothing in it says something the file does not") {
		t.Errorf("the RESOLVED ground does not exclude a note that says more than the file:\n%s", ground)
	}
	// The durable case still needs naming, and with a path in it: a rule stated
	// only as an abstract "a reason the code does not state" is what let a
	// reviewer re-read this rule and find it unguarded.
	if !strings.Contains(prompt, "whatever files or paths it mentions") {
		t.Errorf("the rubric does not protect a durable note that names a path:\n%s", prompt)
	}
	// The KEEP bias is the pass's standing answer to uncertainty, and a second
	// verdict that is not a KEEP must not quietly displace it.
	if !strings.Contains(prompt, "When uncertain, answer KEEP.") {
		t.Errorf("the rubric lost its uncertainty rule:\n%s", prompt)
	}
}

// sentenceContaining returns the one sentence of text holding marker. It fails
// rather than returning "" because a missing sentence and an empty one are the
// same bug here: the rule is gone, or the rule is there and the assertion is
// pointed at nothing.
func sentenceContaining(t *testing.T, text, marker string) string {
	t.Helper()
	// Split on a full stop followed by whitespace, the same boundary the
	// heuristic uses, so a path like internal/memory/store.go never splits.
	for _, s := range strings.Split(text, ". ") {
		if strings.Contains(s, marker) {
			return s
		}
	}
	t.Fatalf("no sentence holds %q", marker)
	return ""
}

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
