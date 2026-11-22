package main

import (
	"strings"
	"testing"
)

// TestCompactionHandoffWinsContinuity is the measurement behind shipping #2164:
// the same mid-task arc runs twice against the deterministic fake harness, once
// with the pre-compact handoff injected and once without it. The handoff is the
// only difference, and the signal is whether the in-flight state — which lives
// in the transcript, not in a saved memory — reaches the post-compaction
// session. Without a win here the feature has not earned its place.
func TestCompactionHandoffWinsContinuity(t *testing.T) {
	story := CompactionContinuity()
	if err := story.Validate(); err != nil {
		t.Fatalf("validate storyline: %v", err)
	}
	const inFlight = "warehouse cutover is tomorrow"

	withHandoff := runFixtureStory(t, story, &fakeGhost{}, &fakeAgent{answers: []string{"stage one", "tomorrow"}})
	withoutHandoff := runFixtureStory(t, story,
		&fakeGhost{compactDisabled: true},
		&fakeAgent{answers: []string{"stage one", "I do not know"}})

	withBlock := withHandoff.Sessions[1].Block
	withoutBlock := withoutHandoff.Sessions[1].Block
	if !strings.Contains(withBlock, inFlight) {
		t.Errorf("with the handoff, the in-flight state did not reach the post-compaction session:\n%s", withBlock)
	}
	if strings.Contains(withoutBlock, inFlight) {
		t.Errorf("without the handoff, the in-flight state still reached the post-compaction session — no measured win:\n%s", withoutBlock)
	}
	if !strings.Contains(withBlock, "Handoff from the session before compaction") {
		t.Errorf("the post-compaction injection has no handoff block:\n%s", withBlock)
	}
}
