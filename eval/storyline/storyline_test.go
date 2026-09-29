package main

import (
	"strings"
	"testing"
)

// goodStory is a two-stage storyline whose single reversal is ANNOTATED on the
// older record (the one that names its own SupersededBy), because that is the
// direction the store's edges point: newer supersedes older, so the older row is
// the one that can say what replaced it.
//
// Every Mark is a verbatim substring of its own Content, and that is not
// decoration: the carry-forward and stale-original checks match a Mark against the
// text the block renders, so a Mark absent from the content could never be graded
// either way. A fixture that ignored this rule would fail Validate on its first
// record and every table case would report the wrong error.
func goodStory() Storyline {
	return Storyline{
		Key:     "test-reversal",
		Title:   "test reversal",
		Project: "acme",
		Opening: []Record{{
			Key: "opening", Category: "fact", Mark: "acme-api",
			Content: "The service is called acme-api.",
		}},
		Stages: []Stage{
			{
				Script: "stage one script",
				Records: []Record{{
					Key: "original", Category: "decision", Mark: "sessions in Redis",
					Content: "We will store sessions in Redis.",
				}},
			},
			{
				Script: "stage two script",
				Records: []Record{{
					Key: "reversal", Category: "decision", Mark: "sessions in Postgres",
					Content: "We will store sessions in Postgres, not Redis.",
				}},
				Expect: []string{"original"},
			},
		},
	}
}

func TestStorylineValidateAcceptsAWellFormedReversal(t *testing.T) {
	if err := goodStory().Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestStorylineValidateRejectsUnusableShapes(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(s *Storyline)
		wants string
	}{
		{"no key", func(s *Storyline) { s.Key = "" }, "key"},
		{"no project", func(s *Storyline) { s.Project = "" }, "project"},
		{"one stage is not a storyline", func(s *Storyline) { s.Stages = s.Stages[:1] }, "2 stages"},
		{"duplicate record key", func(s *Storyline) {
			s.Stages[1].Records[0].Key = "original"
		}, "duplicate"},
		{"empty content", func(s *Storyline) { s.Stages[0].Records[0].Content = "" }, "content"},
		{"bad category", func(s *Storyline) { s.Stages[0].Records[0].Category = "note" }, "category"},
		{"mark is not in the content", func(s *Storyline) {
			s.Stages[0].Records[0].Mark = "SOMETHING-ELSE"
		}, "mark"},
		{"expect names no record", func(s *Storyline) {
			s.Stages[1].Expect = []string{"nope"}
		}, "expect"},
		// A session is graded on what it was INJECTED, and it is injected before
		// it records anything, so expecting its own record is a contradiction the
		// grade could only satisfy by leaking the stage's own writes backwards.
		{"expect names this stage's own record", func(s *Storyline) {
			s.Stages[1].Expect = []string{"reversal"}
		}, "earlier stage"},
		// SupersededBy sits on the OLDER record and names what replaced it, so it
		// may only name a later stage's record. An annotation pointing at its own
		// stage's other record, or at an earlier one, inverts the arc silently:
		// every supersede-edge check would then grade the wrong direction.
		{"superseded_by names its own stage's record", func(s *Storyline) {
			s.Stages[0].Records = append(s.Stages[0].Records, Record{
				Key: "same-stage", Category: "gotcha", Mark: "one session",
				Content: "Two records from one session.",
			})
			s.Stages[0].Records[1].SupersededBy = "same-stage"
		}, "later stage"},
		{"superseded_by names an earlier record", func(s *Storyline) {
			s.Stages[1].Records[0].SupersededBy = "opening"
		}, "later stage"},
		{"superseded_by names no record", func(s *Storyline) {
			s.Stages[0].Records[0].SupersededBy = "nope"
		}, "superseded_by"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := goodStory()
			tc.mut(&s)
			err := s.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Fatalf("Validate error %q does not mention %q", err, tc.wants)
			}
		})
	}
}

// TestStorylineOrderIsChronological pins the one order the rest of the runner
// depends on: opening records, then each stage's records in stage order. It is
// what the chronology restamp walks and what makes a supersedes direction a
// fact about the run rather than about save timing.
func TestStorylineOrderIsChronological(t *testing.T) {
	s := goodStory()
	s.Stages[0].Records = append(s.Stages[0].Records, Record{
		Key: "second-opening", Category: "fact", Mark: "SECOND",
		Content: "The service listens on 8080.",
	})
	var keys []string
	for _, r := range s.Order() {
		keys = append(keys, r.Key)
	}
	want := "opening,original,second-opening,reversal"
	if got := strings.Join(keys, ","); got != want {
		t.Fatalf("Order = %s, want %s", got, want)
	}
}

// TestShippedStorylinesAreValid keeps the shipped scenarios honest: a storyline
// that fails its own validation is a run that dies at the flag boundary, and the
// only tests that would notice are the ones below.
func TestShippedStorylinesAreValid(t *testing.T) {
	for _, s := range storylines() {
		if err := s.Validate(); err != nil {
			t.Fatalf("%s: Validate: %v", s.Key, err)
		}
		if s.Title == "" {
			t.Fatalf("%s: no title", s.Key)
		}
	}
}

func TestStorylineByKeyNamesTheOnesItDoesNotHave(t *testing.T) {
	if _, err := StorylineByKey("no-such-storyline"); err == nil {
		t.Fatal("StorylineByKey accepted an unknown key")
	}
	s, err := StorylineByKey(ReversedDecision().Key)
	if err != nil {
		t.Fatalf("StorylineByKey: %v", err)
	}
	if s.Key != ReversedDecision().Key {
		t.Fatalf("StorylineByKey returned %q", s.Key)
	}
}

// TestReversedDecisionHasThreeSessionsAndOneReversal states the shape of the one
// shipped storyline in a test rather than only in prose: a reversal needs a
// session that made the original, a session that reversed it, and a session that
// has to act on the reversal.
func TestReversedDecisionHasThreeSessionsAndOneReversal(t *testing.T) {
	s := ReversedDecision()
	if len(s.Stages) != 3 {
		t.Fatalf("reversed-decision has %d stages, want 3", len(s.Stages))
	}
	reversals := 0
	for _, r := range s.Order() {
		if r.SupersededBy != "" {
			reversals++
		}
	}
	if reversals != 1 {
		t.Fatalf("reversed-decision annotates %d reversals, want 1", reversals)
	}
	// The last stage is the one the storyline exists for: it must be graded on
	// carrying the reversal, and it must name it in Expect.
	last := s.Stages[len(s.Stages)-1]
	if len(last.Expect) == 0 {
		t.Fatal("the final stage expects nothing to be carried forward")
	}
	carried, ok := s.RecordByKey(last.Expect[0])
	if !ok {
		t.Fatalf("final stage expects unknown key %q", last.Expect[0])
	}
	// The record the final session must be handed is the NEWER side of the
	// reversal: an older record is what the stale-original checks are for, so a
	// final stage carrying one would be graded on the arc's failure mode.
	replaces := 0
	for _, r := range s.Order() {
		if r.SupersededBy == carried.Key {
			replaces++
		}
	}
	if replaces == 0 {
		t.Fatalf("final stage carries %q, which replaces no record in the arc", carried.Key)
	}
}
