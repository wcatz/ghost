package main

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
)

// saveAll pushes a storyline's records through the fake's save path in order and
// returns the fake, whose ids are what the graded state is expressed in.
func saveAll(t *testing.T, s Storyline) *fakeGhost {
	t.Helper()
	g := &fakeGhost{}
	for _, r := range s.Order() {
		if _, err := g.Save(context.Background(), s.Project, r); err != nil {
			t.Fatalf("save %s: %v", r.Key, err)
		}
	}
	return g
}

// gradedResult is a reversed-decision run whose arc came out clean: the reversal
// was carried into the final session, the superseded claim was not injected
// again, supersede linked the two, and the reversal is still live. Every table
// case below breaks exactly one of those, so the check that fails is the one the
// case is about.
func gradedResult(t *testing.T) *Result {
	t.Helper()
	s := ReversedDecision()
	g := saveAll(t, s)
	res := &Result{
		Story:   s,
		WorkDir: "/scratch/work/acme",
		State: State{
			Links: []Link{{
				Source:   g.idOf("session-store-postgres"),
				Target:   g.idOf("session-store-redis"),
				Relation: "supersedes",
			}},
			Stamps: g.stamps(),
		},
	}
	// Session N's block holds the project's opening notes plus everything
	// sessions 1..N-1 left behind — which is what the runner renders, because a
	// record stays in the store for every later session. Counting DISTINCT keys
	// rather than rendered lines is what makes the assertion the one it claims to
	// be: a record injected twice is still one record that carried forward.
	rendered := map[string]bool{}
	// A claim is omitted once its replacement has been WRITTEN: that is what a
	// coherent store does with the older claim — the block stops quoting it rather
	// than quoting it unmarked. A store that keeps quoting it is exactly what the
	// stale-original checks exist to catch, so the CLEAN fixture has to model the
	// clean behaviour or it would be grading the failure it is the control for.
	live := func(r Record, stages int) bool {
		if r.SupersededBy == "" {
			return true
		}
		at, ok := s.stageOf(r.SupersededBy)
		return !ok || at >= stages
	}
	block := func(stages int) string {
		var b strings.Builder
		b.WriteString("## Ghost context: " + s.Project + "\n")
		for _, r := range s.Opening {
			if live(r, stages) {
				fmtLine(&b, r)
				rendered[r.Key] = true
			}
		}
		for j := 0; j < stages && j < len(s.Stages); j++ {
			for _, r := range s.Stages[j].Records {
				if live(r, stages) {
					fmtLine(&b, r)
					rendered[r.Key] = true
				}
			}
		}
		return b.String()
	}
	for i := range s.Stages {
		// Saved carries the ids the save path returned, which is how the grade
		// reaches the store's rows: a fixture that omitted them would be grading
		// checks that could never resolve an id, and a run that loses them fails
		// for a reason unrelated to the arc.
		saved := map[string]string{}
		for _, r := range s.Stages[i].Records {
			saved[r.Key] = g.idOf(r.Key)
		}
		// Provide answers that contain the expected marks for the answer-carries checks
		var answer string
		switch i {
		case 0: // Session 1: no Expect
			answer = "We will use Redis for session state."
		case 1: // Session 2: Expects session-store-redis
			answer = "The previous choice was Redis (SESSION_STORE=redis), but now we use Postgres."
		case 2: // Session 3: Expects session-store-postgres
			answer = "We store sessions in Postgres; the bootstrap sets SESSION_STORE=postgres."
		}
		res.Sessions = append(res.Sessions, Session{Index: i, Block: block(i), Saved: saved, Answer: answer})
	}
	for _, r := range s.Order() {
		if !rendered[r.Key] {
			t.Fatalf("fixture never injected %s, so no check grades it", r.Key)
		}
	}
	// The final block is what a session starting after the arc sees: everything the
	// project holds, including the last stage's own records.
	res.FinalBlock = block(len(s.Stages))
	return res
}

func fmtLine(b *strings.Builder, r Record) {
	b.WriteString("- [" + r.Category + "] «" + r.Content + "»\n")
}

func checkByName(t *testing.T, res *Result, name string) Check {
	t.Helper()
	for _, c := range res.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check named %q in %v", name, checkNames(res))
	return Check{}
}

func checkNames(res *Result) []string {
	var out []string
	for _, c := range res.Checks {
		out = append(out, c.Name)
	}
	return out
}

func TestGradeAcceptsACoherentArc(t *testing.T) {
	res := gradedResult(t)
	res.Checks = grade(res)
	if !res.Passed() {
		for _, c := range res.Checks {
			if !c.Passed {
				t.Errorf("%s: %s", c.Name, c.Detail)
			}
		}
	}
	for _, name := range []string{
		"injection-present:session-1",
		"delivery:block-carries:session-2:session-store-redis",
		"answer-carries:session-2:session-store-redis",
		"delivery:block-carries:session-3:session-store-postgres",
		"delivery:stale-absent:session-3:session-store-redis",
		"answer-carries:session-3:session-store-postgres",
		"delivery:stale-absent:final-block:session-store-redis",
		"answer-carries:session-3:session-store-postgres",
		"supersede-edge:session-store-postgres",
		"reversal-live:session-store-postgres",
		"final-block-carries:session-store-postgres",
	} {
		checkByName(t, res, name)
	}
}

func TestGradeFailsWhenTheReversalWasNotInjected(t *testing.T) {
	res := gradedResult(t)
	res.Sessions[2].Block = "## Ghost context: acme\n- [decision] «We will store sessions in Redis.»\n"
	// Also modify the answer to not contain the mark
	res.Sessions[2].Answer = "We store sessions in Redis; the bootstrap sets SESSION_STORE=redis."
	res.Checks = grade(res)
	// The delivery check should fail (block doesn't carry the mark)
	c := checkByName(t, res, "delivery:block-carries:session-3:session-store-postgres")
	if c.Passed {
		t.Fatal("a block without the reversal passed the delivery check")
	}
	if !strings.Contains(c.Detail, "session-store-postgres") {
		t.Fatalf("detail does not name the missing record: %q", c.Detail)
	}
	// The answer check should also fail (answer doesn't carry the mark)
	c = checkByName(t, res, "answer-carries:session-3:session-store-postgres")
	if c.Passed {
		t.Fatal("an answer without the reversal passed the answer check")
	}
}

func TestGradeFailsWhileTheSupersededClaimIsStillInjected(t *testing.T) {
	res := gradedResult(t)
	// Re-quote the superseded row VERBATIM, which is what the block does when it
	// carries no marker: the stored content, unmodified, with nothing saying it is
	// old. A reworded version would not be the finding — only the verbatim form is
	// something the store's own render produces.
	staleRec, ok := res.Story.RecordByKey("session-store-redis")
	if !ok {
		t.Fatal("reversed-decision no longer names the superseded record")
	}
	var b strings.Builder
	fmtLine(&b, staleRec)
	post := res.Sessions[2].Block + b.String()
	res.Sessions[2].Block = post
	res.FinalBlock = post
	res.Checks = grade(res)
	for _, name := range []string{"delivery:stale-absent:session-3:session-store-redis", "delivery:stale-absent:final-block:session-store-redis"} {
		c := checkByName(t, res, name)
		if c.Passed {
			t.Errorf("%s passed while the superseded claim was still injected", name)
		}
		if !strings.Contains(c.Detail, "session-store-redis") {
			t.Errorf("%s detail does not name the stale record: %q", name, c.Detail)
		}
	}
}

func TestGradeSupersedeEdgeIsDirectionAware(t *testing.T) {
	cases := []struct {
		name     string
		relation string
		flip     bool // write the edge older → newer
		dropped  bool // an invalidated edge stands for nothing
		// stamps rewrites the store's own chronology after the edge is written.
		// It is the case an id comparison cannot see at all: the edge joins exactly
		// the two ids the storyline says, in exactly the annotated order, and it is
		// still not a supersede — the store believes the source is the older row.
		stamps func(res *Result)
		passes bool
	}{
		{"newer supersedes older", "supersedes", false, false, nil, true},
		{"older supersedes newer", "supersedes", true, false, nil, false},
		{"a causes edge is not a supersede", "causes", false, false, nil, false},
		{"an invalidated edge stands for nothing", "supersedes", false, true, nil, false},
		{"a source the store says is older", "supersedes", false, false, func(res *Result) {
			older, newer := res.State.Links[0].Target, res.State.Links[0].Source
			res.State.Stamps[older] = Stamp{CreatedAt: "2027-01-01 00:00:00"}
			res.State.Stamps[newer] = Stamp{CreatedAt: "2026-01-01 00:00:00"}
		}, false},
		{"an unreadable chronology", "supersedes", false, false, func(res *Result) {
			older := res.State.Links[0].Target
			delete(res.State.Stamps, older)
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := gradedResult(t)
			older, newer := res.State.Links[0].Target, res.State.Links[0].Source
			link := Link{Source: newer, Target: older, Relation: tc.relation}
			if tc.flip {
				link = Link{Source: older, Target: newer, Relation: tc.relation}
			}
			link.Invalidated = tc.dropped
			res.State.Links = []Link{link}
			if tc.stamps != nil {
				tc.stamps(res)
			}
			res.Checks = grade(res)
			c := checkByName(t, res, "supersede-edge:session-store-postgres")
			if c.Passed != tc.passes {
				t.Fatalf("check passed = %v, want %v (%s)", c.Passed, tc.passes, c.Detail)
			}
		})
	}
}

func TestGradeFailsWhenTheReversalWasResolvedAway(t *testing.T) {
	res := gradedResult(t)
	g := saveAll(t, res.Story)
	res.State.Stamps[g.idOf("session-store-postgres")] = Stamp{
		CreatedAt: "2026-01-01 00:05:00", ResolvedAt: "2026-01-02 00:00:00",
	}
	res.Checks = grade(res)
	if checkByName(t, res, "reversal-live:session-store-postgres").Passed {
		t.Fatal("a resolved reversal passed the liveness check")
	}
}

func TestGradeFailsAnEmptyInjection(t *testing.T) {
	res := gradedResult(t)
	res.Sessions[1].Block = ""
	res.Checks = grade(res)
	if checkByName(t, res, "injection-present:session-2").Passed {
		t.Fatal("an empty block passed")
	}
}

func TestParseJudgeVerdict(t *testing.T) {
	cases := []struct {
		answer string
		want   bool
		errs   bool
	}{
		{"yes", true, false},
		{"YES — the answer names the new backend.", true, false},
		{"  yes\n", true, false},
		{"no", false, false},
		{"No, the session still described the original decision.", false, false},
		// A judge that answers something else is neither a pass nor a fail: an
		// unreadable verdict is a broken measurement, and counting it as either
		// would put a number in the report that nothing observed.
		{"maybe", false, true},
		{"", false, true},
		// A PREFIX match turns the first word of an ordinary sentence
		// into a verdict: "Yesterday's block..." is not a yes and "Nothing in
		// the block names the new store" is not a no. The rule is the first WORD
		// being exactly yes or no, so a sentence that starts with one and then
		// keeps talking is still a verdict while a sentence that merely begins
		// with the letters is not.
		{"Yesterday the session named the old store.", false, true},
		{"Notably, nothing in the block marks the reversal.", false, true},
		{"Nonsense, but it starts with N.", false, true},
		// The verdict and its justification are separated by WHITESPACE, not by
		// one particular byte: a judge that puts the answer on its own line is
		// still answering the question, and refusing it would discard a
		// three-session run's whole report over line wrapping.
		{"Yes\nThe session named the new store.", true, false},
		{"no\tit still described the original decision.", false, false},
		{"Yes.\nThe session named the new store.", true, false},
		// Punctuation around the word is not part of the verdict either.
		{`"Yes."`, true, false},
		{"(no)", false, false},
	}
	for _, tc := range cases {
		got, err := judgeVerdict(tc.answer)
		if tc.errs {
			if err == nil {
				t.Errorf("judgeVerdict(%q) = %v, want an error", tc.answer, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("judgeVerdict(%q): %v", tc.answer, err)
			continue
		}
		if got != tc.want {
			t.Errorf("judgeVerdict(%q) = %v, want %v", tc.answer, got, tc.want)
		}
	}
}

// TestJudgeIsOptIn: the deterministic checks are the grade, and a run with no
// judge says so rather than reporting a check nothing measured.
func TestJudgeIsOptIn(t *testing.T) {
	res := gradedResult(t)
	res.Checks = grade(res)
	for _, c := range res.Checks {
		if strings.Contains(c.Name, "followed-reversal") {
			t.Fatalf("grade invented a judge check: %s", c.Name)
		}
	}
	if res.Judged {
		t.Fatal("a run with no judge reported itself judged")
	}
}

func TestWriteReportNamesEveryCheckAndTheSessionAnswers(t *testing.T) {
	res := gradedResult(t)
	res.Checks = grade(res)
	res.Sessions[2].Answer = "We store sessions in Postgres; the Redis sentinel is gone."
	res.Supersede = "acme: 1 live supersedes edge(s)"
	path, err := writeReport(t.TempDir(), res, "opencode/big-pickle")
	if err != nil {
		t.Fatalf("writeReport: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	for _, want := range []string{
		"reversed-decision", "opencode/big-pickle", "answer-carries:session-3:session-store-postgres",
		"supersede-edge:session-store-postgres", "Postgres",
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("report does not mention %q", want)
		}
	}
}

func TestWriteReportSaysAFailedCheckFailed(t *testing.T) {
	res := gradedResult(t)
	res.Sessions[2].Block = "## Ghost context: acme\n"
	res.FinalBlock = res.Sessions[2].Block
	res.Checks = grade(res)
	path, err := writeReport(t.TempDir(), res, "opencode/big-pickle")
	if err != nil {
		t.Fatalf("writeReport: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "FAIL") {
		t.Error("a report of a failed run does not say so")
	}
}

// TestAnswerCarriesPassAndFail tests the new answer-carries check with canned answers
// for each arc type (pass and fail for each arc).
func TestAnswerCarriesPassAndFail(t *testing.T) {
	arcs := []struct {
		name      string
		story     Storyline
		pass      string // answer that should pass
		fail      string // answer that should fail
		expectKey string // the record key that is expected
		stage     int
	}{
		{
			name:      "reversed-decision",
			story:     ReversedDecision(),
			pass:      "We store sessions in Postgres with SESSION_STORE=postgres.",
			fail:      "We store sessions in Redis with SESSION_STORE=redis.",
			expectKey: "session-store-postgres",
			stage:     2,
		},
		{
			name:      "correction-replay",
			story:     CorrectionReplay(),
			pass:      "The handler returns HTTP 201 as required by the payment provider.",
			fail:      "The handler returns HTTP 200.",
			expectKey: "correct-status",
			stage:     2,
		},
		{
			name:      "ops-fact",
			story:     OpsFact(),
			pass:      "The database host is db-primary.internal:5432.",
			fail:      "The database host is localhost on port 5432.",
			expectKey: "global-db-host",
			stage:     1,
		},
		{
			name:      "stale-fact",
			story:     StaleFact(),
			pass:      "The current API endpoint is https://api.new.example.com/v2.",
			fail:      "The legacy API endpoint is https://api.old.example.com/v1.",
			expectKey: "new-api-endpoint",
			stage:     1,
		},
	}

	for _, arc := range arcs {
		t.Run(arc.name+"/pass", func(t *testing.T) {
			s := arc.story
			g := saveAll(t, s)
			res := &Result{
				Story:   s,
				WorkDir: "/scratch/work/test",
				State:   State{Links: []Link{}, Stamps: g.stamps()},
			}
			// Build blocks with the expected marks delivered
			for i := range s.Stages {
				var b strings.Builder
				b.WriteString("## Ghost context: " + s.Project + "\n")
				for _, r := range s.Opening {
					fmtLine(&b, r)
				}
				for j := 0; j < i; j++ {
					for _, r := range s.Stages[j].Records {
						fmtLine(&b, r)
					}
				}
				answer := arc.pass
				if i != arc.stage {
					answer = "some answer"
				}
				saved := map[string]string{}
				for _, r := range s.Stages[i].Records {
					saved[r.Key] = g.idOf(r.Key)
				}
				res.Sessions = append(res.Sessions, Session{Index: i, Block: b.String(), Saved: saved, Answer: answer})
			}
			res.FinalBlock = res.Sessions[len(res.Sessions)-1].Block
			res.Checks = grade(res)
			checkName := "answer-carries:session-" + strconv.Itoa(arc.stage+1) + ":" + arc.expectKey
			c := checkByName(t, res, checkName)
			if !c.Passed {
				t.Errorf("expected pass for %s: %s", checkName, c.Detail)
			}
		})

		t.Run(arc.name+"/fail", func(t *testing.T) {
			s := arc.story
			g := saveAll(t, s)
			res := &Result{
				Story:   s,
				WorkDir: "/scratch/work/test",
				State:   State{Links: []Link{}, Stamps: g.stamps()},
			}
			for i := range s.Stages {
				var b strings.Builder
				b.WriteString("## Ghost context: " + s.Project + "\n")
				for _, r := range s.Opening {
					fmtLine(&b, r)
				}
				for j := 0; j < i; j++ {
					for _, r := range s.Stages[j].Records {
						fmtLine(&b, r)
					}
				}
				answer := arc.fail
				if i != arc.stage {
					answer = "some answer"
				}
				saved := map[string]string{}
				for _, r := range s.Stages[i].Records {
					saved[r.Key] = g.idOf(r.Key)
				}
				res.Sessions = append(res.Sessions, Session{Index: i, Block: b.String(), Saved: saved, Answer: answer})
			}
			res.FinalBlock = res.Sessions[len(res.Sessions)-1].Block
			res.Checks = grade(res)
			checkName := "answer-carries:session-" + strconv.Itoa(arc.stage+1) + ":" + arc.expectKey
			c := checkByName(t, res, checkName)
			if c.Passed {
				t.Errorf("expected fail for %s: %s", checkName, c.Detail)
			}
		})
	}
}
