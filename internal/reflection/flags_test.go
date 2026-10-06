package reflection

// #648 slice 2: a flag reaches reflect as a COUNT on its own memory's line, and
// its reason never leaves the store.
//
// The prompt is sent to a third-party model, so this is the surface where a
// leaked reason does the most damage and the one where the count has to appear
// — a consolidation that ignored an agent's objection entirely would be a
// different failure with the same cause.

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// flagReasonMarker is unmistakable text: its presence anywhere in the prompt is
// the failure, asserted as a measurement rather than as a claim.
const flagReasonMarker = "ZZREASONNEVERLEAVESSTOREZZ"

// TestTheFlagCountRidesTheLineAndItsReasonDoesNot: the feature and its bound, in
// one prompt.
func TestTheFlagCountRidesTheLineAndItsReasonDoesNot(t *testing.T) {
	in := reflectBaseInput(reflectPromptMems())
	in.Usefulness = map[string]memory.UsefulnessEvidence{
		"M1": {Flagged: 2, LastAt: "2026-10-06 09:00:00"},
	}
	prompt := BuildReflectionPrompt(in)

	line := memoryLine(t, prompt, "- id:M1 ")
	if !strings.Contains(line, "[audit: verdicts flagged=2") {
		t.Errorf("M1's line does not carry its flag count:\n%s", line)
	}
	if idx := strings.Index(line, "[audit:"); idx < strings.Index(line, "»") {
		t.Errorf("the evidence is printed BEFORE M1's content, so it reads as belonging to the line above:\n%s",
			line)
	}
	// The reason is nowhere — not on M1's line, not anywhere in the document.
	// The store never puts it in the evidence struct, and this is what proves it
	// stayed out rather than merely being rendered elsewhere.
	if strings.Contains(prompt, flagReasonMarker) {
		t.Errorf("a flag's reason text reached the reflection prompt:\n%s", prompt)
	}
	// The memory with no flags is untouched in the SAME prompt, so the two are
	// one renderer conditioned on the evidence rather than a changed prompt.
	other := memoryLine(t, prompt, "- id:M2 ")
	if strings.Contains(other, "audit") {
		t.Errorf("a memory with no flags was annotated:\n%s", other)
	}
}

// TestAFlagOnItsOwnIsStillAStablePromptWhenThereIsNothingToSay: the empty case,
// which every existing byte-pinned prompt test rests on. No flags, no verdicts —
// no annotation, no change.
func TestAFlagOnItsOwnIsStillAStablePromptWhenThereIsNothingToSay(t *testing.T) {
	mems := reflectPromptMems()
	want := BuildReflectionPrompt(reflectBaseInput(mems))

	in := reflectBaseInput(mems)
	in.Usefulness = map[string]memory.UsefulnessEvidence{
		"M9": {Flagged: 3, LastAt: "2026-10-06 09:00:00"}, // a memory not in the corpus
	}
	if got := BuildReflectionPrompt(in); got != want {
		t.Errorf("the prompt changed by %d bytes when the only flag names a memory not in the corpus:\n%s",
			len(got)-len(want), diffTail(want, got))
	}

	in2 := reflectBaseInput(mems)
	in2.Usefulness = map[string]memory.UsefulnessEvidence{"M1": {}}
	if got := BuildReflectionPrompt(in2); got != want {
		t.Errorf("the prompt changed by %d bytes for an entry carrying no evidence at all:\n%s",
			len(got)-len(want), diffTail(want, got))
	}
}
