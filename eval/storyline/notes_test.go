package main

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// TestFlatNotesArmHandsEverySavedRecordInOrder: the notes arm saves the same
// records as the Ghost arm, hands each session every record saved before it as
// a notes file in save order inside the same prompt framing, never asks Ghost
// for a block, and runs no arc stage.
func TestFlatNotesArmHandsEverySavedRecordInOrder(t *testing.T) {
	story := CorrectionReplay()
	withG, notesG := &fakeGhost{}, &fakeGhost{}
	notesA := &fakeAgent{}
	runFixtureStory(t, story, withG, &fakeAgent{})
	r := &Run{Story: story, WorkDir: "/scratch/work/acme", Ghost: notesG, Agent: notesA, FlatNotes: true, Out: testWriter{t}}
	res, err := r.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	savedKeys := func(g *fakeGhost) []string {
		var ks []string
		for _, s := range g.saved {
			ks = append(ks, s.key)
		}
		return ks
	}
	if !slices.Equal(savedKeys(withG), savedKeys(notesG)) {
		t.Errorf("seeds differ: %v vs %v", savedKeys(withG), savedKeys(notesG))
	}

	before := append([]Record(nil), story.Opening...)
	for i, st := range story.Stages {
		block := res.Sessions[i].Block
		if want := notesBlock(before); block != want {
			t.Errorf("session %d block = %q, want the notes of the %d records saved before it", i+1, block, len(before))
		}
		last := -1
		for _, rec := range before {
			at := strings.Index(block, strings.TrimSpace(rec.Content))
			if at < 0 {
				t.Errorf("session %d is missing %s", i+1, rec.Key)
				continue
			}
			if at < last {
				t.Errorf("session %d lists %s out of save order", i+1, rec.Key)
			}
			last = at
		}
		if !strings.HasPrefix(notesA.prompts[i], st.Script) || !strings.Contains(notesA.prompts[i], "session-start injection follows") {
			t.Errorf("session %d lost its script or the injection framing", i+1)
		}
		before = append(before, st.Records...)
	}

	for _, call := range notesG.calls {
		if strings.HasPrefix(call, "context") || strings.HasPrefix(call, "supersede") ||
			strings.HasPrefix(call, "resolve") || call == "settle" || call == "state" || call == "restamp" {
			t.Errorf("the notes arm called %q", call)
		}
	}
	if res.Arm() != armFlatNotes {
		t.Errorf("arm = %q", res.Arm())
	}
	for _, c := range res.Checks {
		if !strings.HasPrefix(c.Name, "answer-") {
			t.Errorf("the notes arm graded %q, which is not an answer line", c.Name)
		}
	}
}

// TestNotesBlockAnnotatesValidityScopeAndGlobal: a notes file filters nothing,
// so an expired and a scoped record are present, each annotated the way a
// person keeping notes would; a global record goes under the user-level heading;
// Ghost's own work (ids, categories, replacement marks) is absent.
func TestNotesBlockAnnotatesValidityScopeAndGlobal(t *testing.T) {
	got := notesBlock([]Record{
		{Key: "old", Category: "fact", Content: "The API is at api-v1.example.", ValidUntil: "2025-01-31"},
		{Key: "stg", Category: "fact", Content: "Workers: 2.", Scope: map[string]string{"region": "eu", "environment": "staging"}},
		{Key: "broker", Category: "fact", Content: "Broker is at mq-01.example:5672.", Global: true},
		{Key: "new", Category: "decision", Content: "The API is at api-v2.example.", SupersededBy: ""},
	})
	want := "# Project notes (oldest first)\n\n" +
		"- «The API is at api-v1.example.» (valid until 2025-01-31)\n" +
		"- «Workers: 2.» (applies to environment=staging, region=eu)\n" +
		"- «The API is at api-v2.example.»\n" +
		"\n# User notes, all projects (oldest first)\n\n" +
		"- «Broker is at mq-01.example:5672.»\n"
	if got != want {
		t.Errorf("notes block:\n%s\nwant:\n%s", got, want)
	}
	for _, leak := range []string{"decision", "[fact]", "replaced", "superseded"} {
		if strings.Contains(got, leak) {
			t.Errorf("the notes block carries Ghost's own %q", leak)
		}
	}
	if notesBlock(nil) != "" {
		t.Error("an empty store rendered a non-empty notes file")
	}
}

func TestFlatNotesAndWithoutGhostAreExclusive(t *testing.T) {
	r := &Run{Story: CorrectionReplay(), WorkDir: "/w", Ghost: &fakeGhost{}, Agent: &fakeAgent{}, WithoutGhost: true, FlatNotes: true, Out: testWriter{t}}
	if _, err := r.Execute(context.Background()); err == nil || !strings.Contains(err.Error(), "exclusive") {
		t.Fatalf("a run with both baselines set was accepted: %v", err)
	}
}

// TestSummaryAndExitWithTheNotesArm: the summary prints the notes arm as its
// own row, and a notes-arm miss is a measurement that never decides the exit
// code, while a notes-arm error still does.
func TestSummaryAndExitWithTheNotesArm(t *testing.T) {
	story := OpsFact()
	miss := answersResult(story, []string{"", "no idea", "no idea"})
	miss.WithoutGhost, miss.FlatNotes = false, true
	if miss.Passed() {
		t.Fatal("fixture should fail")
	}
	ok := answersResult(story, []string{"", "pg-queue-03.corp.example:6432", "pg-queue-03.corp.example:6432"})
	ok.WithoutGhost = false
	cells := []cell{
		{story: story, arm: armWithGhost, run: 1, res: ok},
		{story: story, arm: armFlatNotes, run: 1, res: miss},
	}
	if got := exitFailures(cells); len(got) != 0 {
		t.Errorf("a notes-arm miss counted: %v", got)
	}
	if got := exitFailures([]cell{{story: story, arm: armFlatNotes, run: 1, err: context.Canceled}}); len(got) != 1 {
		t.Errorf("an errored notes run did not count: %v", got)
	}
	out := formatSummary(cells, 1)
	if !strings.Contains(out, "| flat-notes | 1 |") || !strings.Contains(out, "n/a (every note)") {
		t.Errorf("summary has no notes row:\n%s", out)
	}
	if strings.Index(out, "| with-ghost") > strings.Index(out, "| flat-notes") {
		t.Errorf("the notes row is not after the Ghost row:\n%s", out)
	}
}
