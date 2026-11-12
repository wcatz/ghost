package reflection

// #648 slice 2: a flag reaches reflect as a COUNT on its own memory's line, and
// its reason never leaves the store.
//
// The prompt is sent to a third-party model, so this is the surface where a
// leaked reason does the most damage and the one where the count has to appear
// — a consolidation that ignored an agent's objection entirely would be a
// different failure with the same cause.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// flagReasonMarker is unmistakable text: its presence anywhere in the prompt is
// the failure, asserted as a measurement rather than as a claim.
const flagReasonMarker = "ZZREASONNEVERLEAVESSTOREZZ"

// reflectPromptMems is a two-memory corpus where both notes pass any prefilter a
// caller might apply, so both are rendered and the two can be told apart by their
// content.
func reflectPromptMems() []memory.Memory {
	return []memory.Memory{
		{ID: "M1", Category: "fact", Importance: 0.5, Source: "mcp",
			Content: "the deploy failure was a stale hash, per the postmortem"},
		{ID: "M2", Category: "fact", Importance: 0.5, Source: "mcp",
			Content: "the graph bonus is gone as of PR #210"},
	}
}

func reflectBaseInput(mems []memory.Memory) ReflectionInput {
	return ReflectionInput{
		ExistingMemories: mems,
		ProjectName:      "proj",
		ProjectLanguage:  "go",
	}
}

// memoryLine returns the single rendered record for the id whose line starts with
// prefix.
func memoryLine(t *testing.T, prompt, prefix string) string {
	t.Helper()
	for _, l := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	t.Fatalf("no record starting with %q in the prompt:\n%s", prefix, prompt)
	return ""
}

// diffTail returns a short, readable account of where two prompts diverge, so a
// byte-inequality failure names the change rather than dumping two documents.
func diffTail(want, got string) string {
	wl, gl := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := range wl {
		if i >= len(gl) {
			return fmt.Sprintf("the new prompt ends after %d lines; last old line: %q", len(gl), wl[i])
		}
		if wl[i] != gl[i] {
			return fmt.Sprintf("line %d:\n  want: %q\n   got: %q", i+1, wl[i], gl[i])
		}
	}
	return fmt.Sprintf("the new prompt carries %d extra line(s): %q", len(gl)-len(wl), gl[len(wl):])
}

// TestTheFlagCountRidesTheLineAndItsReasonDoesNot: the feature and its bound, in
// one prompt.
func TestTheFlagCountRidesTheLineAndItsReasonDoesNot(t *testing.T) {
	in := reflectBaseInput(reflectPromptMems())
	in.Flags = map[string]memory.FlagEvidence{
		"M1": {Flagged: 2},
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
// which every existing byte-pinned prompt test rests on. No flags — no
// annotation, no change.
func TestAFlagOnItsOwnIsStillAStablePromptWhenThereIsNothingToSay(t *testing.T) {
	mems := reflectPromptMems()
	want := BuildReflectionPrompt(reflectBaseInput(mems))

	in := reflectBaseInput(mems)
	in.Flags = map[string]memory.FlagEvidence{
		"M9": {Flagged: 3}, // a memory not in the corpus
	}
	if got := BuildReflectionPrompt(in); got != want {
		t.Errorf("the prompt changed by %d bytes when the only flag names a memory not in the corpus:\n%s",
			len(got)-len(want), diffTail(want, got))
	}

	in2 := reflectBaseInput(mems)
	in2.Flags = map[string]memory.FlagEvidence{"M1": {}}
	if got := BuildReflectionPrompt(in2); got != want {
		t.Errorf("the prompt changed by %d bytes for an entry carrying no evidence at all:\n%s",
			len(got)-len(want), diffTail(want, got))
	}
}
