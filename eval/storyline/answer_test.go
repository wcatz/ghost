package main

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// answersResult is a run of a shipped arc graded on its ANSWERS alone: the
// without-Ghost shape, in which no block or store line exists, so every check in
// the result is an answer line and a canned answer is the only input.
func answersResult(s Storyline, answers []string) *Result {
	res := &Result{Story: s, WithoutGhost: true}
	for i, a := range answers {
		res.Sessions = append(res.Sessions, Session{Index: i, Answer: a})
	}
	res.Checks = grade(res)
	return res
}

// TestAnswerGradePassAndFailPerArc drives every shipped arc through the answer
// grade with canned answers: one that does what the arc wants, and one per way of
// failing it. A pass and a fail for each arc is what shows the grade reads the
// answer rather than being satisfied by any text, and each failing case names the
// check that must be the one to fail.
func TestAnswerGradePassAndFailPerArc(t *testing.T) {
	const blank = "I do not have that information."
	cases := []struct {
		story   string
		answers []string
		// failing is the exact set of failing check names; empty is a clean pass.
		failing []string
	}{
		{"reversed-decision", []string{blank, blank,
			"Postgres; set SESSION_STORE=postgres. Someone on the old choice would set SESSION_STORE=redis, which is wrong."}, nil},
		{"reversed-decision", []string{blank, blank, "It goes in Redis; set SESSION_STORE=redis."},
			[]string{"answer-carries:session-3:current-store", "answer-avoids:session-3:superseded-store"}},
		{"reversed-decision", []string{blank, blank, "It goes in a SQL database; set the store variable accordingly."},
			[]string{"answer-carries:session-3:current-store"}},

		{"correction-replay", []string{blank,
			"Send X-Acme-Dedupe-Token, not Idempotency-Key.",
			"The retry test sets X-Acme-Dedupe-Token; the standard header is ignored by this gateway."}, nil},
		{"correction-replay", []string{blank,
			"Send the Idempotency-Key header.",
			"Send X-Acme-Dedupe-Token."},
			[]string{"answer-carries:session-2:corrected-header", "answer-avoids:session-2:repeated-mistake"}},
		{"correction-replay", []string{blank,
			"Send X-Acme-Dedupe-Token.",
			"Set the Idempotency-Key header on the request. It carries the key."},
			[]string{"answer-carries:session-3:corrected-header", "answer-avoids:session-3:repeated-mistake"}},

		{"ops-fact", []string{blank,
			"Connect to pg-queue-03.corp.example on port 6432.",
			"The probe dials pg-queue-03.corp.example:6432 over TLS."}, nil},
		{"ops-fact", []string{blank,
			"Connect to localhost on port 5672.",
			"The probe dials pg-queue-03.corp.example:6432 over TLS."},
			[]string{"answer-carries:session-2:queue-host", "answer-carries:session-2:queue-port"}},
		{"ops-fact", []string{blank,
			"Connect to pg-queue-03.corp.example on port 5671.",
			"Connect to pg-queue-03.corp.example."},
			[]string{"answer-carries:session-2:queue-port", "answer-carries:session-3:queue-port"}},

		{"stale-fact", []string{blank,
			"The client calls https://ledger-v2.corp.example/api.",
			"The fixture points at https://ledger-v2.corp.example/api, not the expired ledger-v1.corp.example host."}, nil},
		{"stale-fact", []string{blank,
			"The client calls https://ledger-v1.corp.example/api.",
			"The fixture points at https://ledger-v2.corp.example/api."},
			[]string{"answer-carries:session-2:current-endpoint", "answer-avoids:session-2:expired-endpoint"}},
		{"stale-fact", []string{blank,
			"The client calls https://ledger-v2.corp.example/api.",
			"Use https://ledger-v2.corp.example/api. Fall back to https://ledger-v1.corp.example/api if it is down."},
			[]string{"answer-avoids:session-3:expired-endpoint"}},
	}
	for _, tc := range cases {
		s, err := StorylineByKey(tc.story)
		if err != nil {
			t.Fatal(err)
		}
		res := answersResult(s, tc.answers)
		var failing []string
		for _, c := range res.Checks {
			if !c.Passed {
				failing = append(failing, c.Name)
			}
		}
		slices.Sort(failing)
		want := slices.Clone(tc.failing)
		slices.Sort(want)
		if !slices.Equal(failing, want) {
			t.Errorf("%s %q: failing checks = %v, want %v\n%v", tc.story, tc.answers, failing, want, res.Checks)
		}
		if res.Passed() != (len(tc.failing) == 0) {
			t.Errorf("%s: Passed() = %v with failing %v", tc.story, res.Passed(), tc.failing)
		}
	}
}

// TestEveryShippedArcGradesTheAnswer guards the table above against a storyline
// that quietly lost its answer grade: each shipped arc must carry at least one
// answer-carries check, and the arcs that claim a stale or mistaken claim must
// carry an avoid check.
func TestEveryShippedArcGradesTheAnswer(t *testing.T) {
	wantAvoids := map[string]bool{"reversed-decision": true, "correction-replay": true, "stale-fact": true}
	for _, s := range storylines() {
		var carries, avoids int
		for _, st := range s.Stages {
			carries += len(st.Carries)
			avoids += len(st.Avoids)
		}
		if carries == 0 {
			t.Errorf("%s has no answer-carries check", s.Key)
		}
		if wantAvoids[s.Key] && avoids == 0 {
			t.Errorf("%s has no answer-avoids check", s.Key)
		}
	}
}

func TestAnswerAvoidsReadsMarkersAndTokens(t *testing.T) {
	avoid := AnswerCheck{Name: "m", Any: []string{"Idempotency-Key"}}
	cases := []struct {
		answer string
		used   bool
	}{
		{"Set the Idempotency-Key header.", true},
		{"set the idempotency-key header", true},
		{"Use X-Acme-Dedupe-Token instead of Idempotency-Key.", false},
		{"Never send Idempotency-Key here.", false},
		// A different identifier that merely starts with the spelling is not a use.
		{"Send the Idempotency-Keys table.", false},
		{"Send X-Idempotency-Key.", false},
		// A later sentence is judged on its own: the marker in the first does not
		// excuse the second.
		{"The token is not optional. Set the Idempotency-Key header.", true},
		// A dot inside a hostname does not end a sentence.
		{"The host is old.example.test and Idempotency-Key.", false},
	}
	for _, tc := range cases {
		c := answerAvoids(Session{Answer: tc.answer}, avoid)
		if c.Passed == tc.used {
			t.Errorf("%q: used = %v, check passed = %v (%s)", tc.answer, tc.used, c.Passed, c.Detail)
		}
	}
	if c := answerAvoids(Session{Answer: "Set the Idempotency-Key header."}, avoid); !strings.Contains(c.Detail, "set the idempotency-key header") {
		t.Errorf("detail does not quote the offending sentence: %q", c.Detail)
	}
}

func TestAnswerCarriesIsCaseInsensitiveAndAcceptsAnySpelling(t *testing.T) {
	c := AnswerCheck{Name: "x", Any: []string{"Alpha-One", "beta"}}
	for _, a := range []string{"use ALPHA-ONE", "it is Beta."} {
		if !answerCarries(Session{Answer: a}, c).Passed {
			t.Errorf("%q did not carry", a)
		}
	}
	if answerCarries(Session{Answer: "gamma"}, c).Passed {
		t.Error("an unrelated answer carried")
	}
}

// TestValidateRefusesAnAnswerThatCouldLeakOrNeverCarry is the arcs' anti-leak
// rule: a Carries spelling in the script of the stage graded on it, or in no
// earlier record, would make a passing answer mean nothing.
func TestValidateRefusesAnAnswerThatCouldLeakOrNeverCarry(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(s *Storyline)
		wants string
	}{
		{"leaked in the script", func(s *Storyline) {
			s.Stages[1].Script = "We keep sessions in Redis for now."
			s.Stages[1].Carries = []AnswerCheck{{Name: "c", Any: []string{"sessions in redis"}}}
		}, "script contains"},
		{"no earlier record", func(s *Storyline) {
			s.Stages[1].Carries = []AnswerCheck{{Name: "c", Any: []string{"memcached"}}}
		}, "no earlier record"},
		{"a record of its own stage", func(s *Storyline) {
			s.Stages[1].Carries = []AnswerCheck{{Name: "c", Any: []string{"sessions in postgres"}}}
		}, "no earlier record"},
		{"no name", func(s *Storyline) {
			s.Stages[1].Carries = []AnswerCheck{{Any: []string{"redis"}}}
		}, "no name"},
		{"empty spelling", func(s *Storyline) {
			s.Stages[1].Carries = []AnswerCheck{{Name: "c", Any: []string{" "}}}
		}, "empty spelling"},
		{"avoid with no spelling", func(s *Storyline) {
			s.Stages[1].Avoids = []AnswerCheck{{Name: "a"}}
		}, "no name or no spelling"},
	}
	for _, tc := range cases {
		s := goodStory()
		tc.mut(&s)
		err := s.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.wants) {
			t.Errorf("%s: Validate = %v, want an error containing %q", tc.name, err, tc.wants)
		}
	}
	// And the well-formed case passes, so the refusals above are not a blanket.
	s := goodStory()
	s.Stages[1].Carries = []AnswerCheck{{Name: "c", Any: []string{"sessions in Redis"}}}
	if err := s.Validate(); err != nil {
		t.Fatalf("a carried spelling from an earlier record was refused: %v", err)
	}
}

func TestValidateRefusesUnsoundScopeAndValidity(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(s *Storyline)
		wants string
	}{
		{"global-only", func(s *Storyline) {
			s.Opening[0].Global = true
			s.Stages[0].Records[0].Global = true
			s.Stages[1].Records[0].Global = true
		}, "every record is global"},
		{"global record superseded", func(s *Storyline) {
			s.Stages[0].Records[0].Global = true
			s.Stages[0].Records[0].SupersededBy = "reversal"
		}, "global record original cannot be superseded"},
		{"global record supersedes", func(s *Storyline) {
			s.Stages[0].Records[0].SupersededBy = "reversal"
			s.Stages[1].Records[0].Global = true
		}, "cannot supersede"},
		{"unreadable valid_until", func(s *Storyline) {
			s.Opening[0].ValidUntil = "next tuesday"
		}, "valid_until"},
	}
	for _, tc := range cases {
		s := goodStory()
		tc.mut(&s)
		err := s.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.wants) {
			t.Errorf("%s: Validate = %v, want an error containing %q", tc.name, err, tc.wants)
		}
	}
}

// TestShippedArcsKeepTheirAnswersOutOfTheScripts re-reads the property each arc's
// doc comment asserts, against the shipped literals rather than a fixture: no
// graded stage's script holds a spelling it grades the answer on.
func TestShippedArcsKeepTheirAnswersOutOfTheScripts(t *testing.T) {
	for _, s := range storylines() {
		for i, st := range s.Stages {
			for _, c := range st.Carries {
				for _, a := range c.Any {
					if strings.Contains(strings.ToLower(st.Script), strings.ToLower(a)) {
						t.Errorf("%s stage %d script contains %q", s.Key, i+1, a)
					}
				}
			}
		}
	}
}

func TestExpiredRecordIsGradedOnTheBlock(t *testing.T) {
	s := StaleFact()
	old, ok := s.RecordByKey("old-endpoint")
	if !ok {
		t.Fatal("stale-fact lost its expired record")
	}
	if got := expiredBefore(s, 0); len(got) != 1 || got[0].Key != "old-endpoint" {
		t.Fatalf("expiredBefore(0) = %v, want the opening's expired record", got)
	}
	if c := expiredWithheld("session-2", "## Ghost context\n- «The ledger API base URL is https://ledger-v2.corp.example/api.»\n", old); !c.Passed {
		t.Errorf("a block without the expired record failed: %s", c.Detail)
	}
	c := expiredWithheld("session-2", "- «"+old.Content+"»\n", old)
	if c.Passed || !strings.Contains(c.Detail, "expired on 2025-01-31") {
		t.Errorf("a block quoting the expired record passed or hid the date: %+v", c)
	}
	// A record not yet expired is not graded.
	future := s
	future.Opening = slices.Clone(s.Opening)
	future.Opening[0].ValidUntil = "2999-01-01"
	if got := expiredBefore(future, 3); len(got) != 0 {
		t.Errorf("an unexpired record was graded as expired: %v", got)
	}
}

func TestPassedIgnoresAdvisoryChecks(t *testing.T) {
	res := &Result{Checks: []Check{
		{Name: "answer-carries:session-2:x", Passed: true},
		{Name: "judge:followed-reversal", Passed: false, Advisory: true},
	}}
	if !res.Passed() {
		t.Error("a failing advisory check flipped the verdict")
	}
	if got := res.FailedNames(); len(got) != 0 {
		t.Errorf("an advisory failure is listed as a failed check: %v", got)
	}
	res.Checks = append(res.Checks, Check{Name: "answer-carries:session-3:x"})
	if res.Passed() || !slices.Equal(res.FailedNames(), []string{"answer-carries:session-3:x"}) {
		t.Errorf("a failing gating check did not decide: %v", res.FailedNames())
	}
	if c := judgedCheck(false, "no"); !c.Advisory {
		t.Error("the judge's check is not advisory")
	}
}

// TestWithoutGhostArmRunsTheSameSeedsWithAnEmptyBlock: the control arm saves the
// same records in the same order under the same scripts, hands each session an
// empty block inside the same prompt framing, and runs no arc stage.
func TestWithoutGhostArmRunsTheSameSeedsWithAnEmptyBlock(t *testing.T) {
	story := CorrectionReplay()
	withG, withoutG := &fakeGhost{}, &fakeGhost{}
	withA, withoutA := &fakeAgent{}, &fakeAgent{}
	with := runFixtureStory(t, story, withG, withA)
	r := &Run{Story: story, WorkDir: "/scratch/work/acme", Ghost: withoutG, Agent: withoutA, WithoutGhost: true, Out: testWriter{t}}
	without, err := r.Execute(context.Background())
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
	if !slices.Equal(savedKeys(withG), savedKeys(withoutG)) {
		t.Errorf("seeds differ: %v vs %v", savedKeys(withG), savedKeys(withoutG))
	}
	for i, st := range story.Stages {
		if !strings.HasPrefix(withA.prompts[i], st.Script) || !strings.HasPrefix(withoutA.prompts[i], st.Script) {
			t.Errorf("session %d did not get its own script first", i+1)
		}
		if without.Sessions[i].Block != "" {
			t.Errorf("session %d was handed a block: %q", i+1, without.Sessions[i].Block)
		}
		if with.Sessions[i].Block == "" {
			t.Errorf("session %d of the Ghost arm had no block", i+1)
		}
		// The framing survives the empty block: the arm is an empty block, not a
		// missing hook.
		if !strings.Contains(withoutA.prompts[i], "session-start injection follows") {
			t.Errorf("session %d lost the injection framing", i+1)
		}
		// Session 1's script is where the correction is made; the graded sessions
		// must not repeat it.
		if i > 0 && strings.Contains(withoutA.prompts[i], "X-Acme-Dedupe-Token") {
			t.Errorf("session %d's prompt carries the corrected header", i+1)
		}
	}
	for _, call := range withoutG.calls {
		if strings.HasPrefix(call, "context") || strings.HasPrefix(call, "supersede") ||
			strings.HasPrefix(call, "resolve") || call == "settle" || call == "state" || call == "restamp" {
			t.Errorf("the control arm called %q", call)
		}
	}
	if without.Arm() != armWithoutGhost || with.Arm() != armWithGhost {
		t.Errorf("arms are %q and %q", with.Arm(), without.Arm())
	}
	for _, c := range without.Checks {
		if !strings.HasPrefix(c.Name, "answer-") {
			t.Errorf("the control arm graded %q, which is not an answer line", c.Name)
		}
	}
}

func TestSaveCallRoutesGlobalAndValidity(t *testing.T) {
	tool, args := saveCall("proj", Record{Content: "c", Category: "fact", ValidUntil: "2025-01-31"})
	if tool != "ghost_memory_save" || args["project_id"] != "proj" || args["valid_until"] != "2025-01-31" {
		t.Errorf("project save = %s %v", tool, args)
	}
	tool, args = saveCall("proj", Record{Content: "c", Category: "fact", Global: true})
	if tool != "ghost_save_global" {
		t.Errorf("global save used %s", tool)
	}
	if _, has := args["project_id"]; has {
		t.Error("a global save carries a project id")
	}
	if _, has := args["valid_until"]; has {
		t.Error("an unset valid_until was sent")
	}
}

func TestSavedIDRegexpReadsBothSaveTools(t *testing.T) {
	for _, out := range []string{
		"Memory saved (id: 0a1b2c)",
		"Global memory saved (id: 0a1b2c), linked as a likely duplicate of x (score 0.91)",
	} {
		if m := savedIDRe.FindStringSubmatch(out); m == nil || m[1] != "0a1b2c" {
			t.Errorf("%q: %v", out, m)
		}
	}
}

func TestSelectStorylines(t *testing.T) {
	all, err := selectStorylines("all")
	if err != nil || len(all) != len(storylines()) {
		t.Fatalf("all: %d, %v", len(all), err)
	}
	two, err := selectStorylines("ops-fact, stale-fact")
	if err != nil || len(two) != 2 || two[0].Key != "ops-fact" || two[1].Key != "stale-fact" {
		t.Fatalf("list: %v, %v", two, err)
	}
	if _, err := selectStorylines("ops-fact,nope"); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("an unknown key was accepted: %v", err)
	}
}

// TestSummaryReportsBothArmsWithBlockSizes: the side-by-side table counts runs
// per arm and names each session's block size, and an errored run is counted
// apart rather than as a miss.
func TestSummaryReportsBothArmsWithBlockSizes(t *testing.T) {
	story := CorrectionReplay()
	mk := func(answers []string, blocks []int) *Result {
		res := answersResult(story, answers)
		for i := range res.Sessions {
			res.Sessions[i].Block = strings.Repeat("x", blocks[i])
		}
		return res
	}
	good := []string{"", "Send X-Acme-Dedupe-Token.", "Send X-Acme-Dedupe-Token."}
	bad := []string{"", "Send Idempotency-Key.", "Send Idempotency-Key."}
	cells := []cell{
		{story: story, arm: armWithGhost, run: 1, res: mk(good, []int{100, 200, 300})},
		{story: story, arm: armWithGhost, run: 2, res: mk(good, []int{100, 220, 300})},
		{story: story, arm: armWithoutGhost, run: 1, res: mk(bad, []int{0, 0, 0})},
		{story: story, arm: armWithoutGhost, run: 2, res: mk(good, []int{0, 0, 0})},
		{story: story, arm: armWithoutGhost, run: 3, err: context.Canceled},
	}
	out := formatSummary(cells, 3)
	for _, want := range []string{
		"## correction-replay",
		"| with-ghost | 2 | 2/2 | 0/2 |",
		"100B, 200-220B, 300B",
		"| without-ghost | 2 (+1 errored) | 1/2 | 1/2 |",
		"0B, 0B, 0B",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary is missing %q:\n%s", want, out)
		}
	}
}

// TestUnreadableJudgeVerdictDoesNotDiscardTheRun: the judge is an advisory
// column, so a reply that is neither yes nor no is recorded as a failed advisory
// line and the deterministic grade survives.
func TestUnreadableJudgeVerdictDoesNotDiscardTheRun(t *testing.T) {
	story := OpsFact()
	r := &Run{
		Story: story, WorkDir: "/scratch/work/acme", Ghost: &fakeGhost{},
		Agent: &fakeAgent{answers: []string{"ok", "pg-queue-03.corp.example:6432", "pg-queue-03.corp.example:6432"}},
		Judge: &fakeAgent{answers: []string{"The answer matches. So yes.</think>yes"}},
		Out:   testWriter{t},
	}
	res, err := r.Execute(context.Background())
	if err != nil {
		t.Fatalf("an unreadable verdict ended the run: %v", err)
	}
	var found bool
	for _, c := range res.Checks {
		if c.Name == "judge:unreadable" {
			found = c.Advisory && !c.Passed
		}
	}
	if !found {
		t.Fatalf("no failed advisory judge:unreadable check in %v", checkNames(res))
	}
	if !res.Judged || !strings.Contains(res.Verdict, "So yes") {
		t.Errorf("the raw verdict was not kept: %+v", res.Verdict)
	}
	for _, c := range res.Checks {
		if strings.HasPrefix(c.Name, "answer-") && !c.Passed {
			t.Errorf("%s failed", c.Name)
		}
	}
	// The judge was asked the arc's own question, not the reversal's.
	if q := r.Judge.(*fakeAgent).prompts[0]; !strings.Contains(q, "stored address") {
		t.Errorf("the judge was not asked the arc's question: %q", q)
	}
}

// TestSummaryKeepsARunWhoseReportFailedToWrite: a finished, graded run whose
// markdown could not be written stays in every tally; only a run that did not
// finish is counted as errored.
func TestSummaryKeepsARunWhoseReportFailedToWrite(t *testing.T) {
	story := OpsFact()
	res := answersResult(story, []string{"", "pg-queue-03.corp.example:6432", "pg-queue-03.corp.example:6432"})
	out := formatSummary([]cell{{story: story, arm: armWithoutGhost, run: 1, res: res, reportErr: context.DeadlineExceeded}}, 1)
	if !strings.Contains(out, "| without-ghost | 1 (1 report not written) | 1/1 |") || strings.Contains(out, "errored") {
		t.Errorf("a graded run was dropped from the summary:\n%s", out)
	}
}

// TestAnswerLinesReadOnlyTheAnswer: the block is evidence the session was handed,
// never what it said. A block holding the mark with a blank answer must fail
// answer-carries, and a block holding the stale string with a clean answer must
// pass answer-avoids.
func TestAnswerLinesReadOnlyTheAnswer(t *testing.T) {
	story := CorrectionReplay()
	st := story.Stages[1]
	block := "- [gotcha] «The gateway ignores the Idempotency-Key header; use X-Acme-Dedupe-Token.»\n"
	if c := answerCarries(Session{Index: 1, Block: block, Answer: ""}, st.Carries[0]); c.Passed {
		t.Errorf("answer-carries read the block: %+v", c)
	}
	if c := answerAvoids(Session{Index: 1, Block: block, Answer: "I am not sure."}, AnswerCheck{Name: "m", Any: []string{"Idempotency-Key"}}); !c.Passed {
		t.Errorf("answer-avoids read the block: %+v", c)
	}
	// And through grade, in the Ghost-arm shape.
	res := &Result{Story: story, Sessions: []Session{{Index: 0}, {Index: 1, Block: block, Answer: ""}, {Index: 2, Block: block, Answer: ""}}}
	res.Checks = grade(res)
	if checkByName(t, res, "answer-carries:session-2:corrected-header").Passed {
		t.Error("grade passed answer-carries on a blank answer")
	}
	if !checkByName(t, res, "answer-avoids:session-2:repeated-mistake").Passed {
		t.Error("grade failed answer-avoids on an answer that names nothing")
	}
}

// TestCorrectionReplaySentences table-tests every sentence the reviews named,
// against correction-replay's own checks. "mistake" rows are answers that
// repeat the mistake and must not pass both lines; "correct" rows must pass both.
// A "known" row is a documented misgrade (docs/benchmarks.md): the table pins the
// grade the rule actually gives, and the comment says which way it errs.
func TestCorrectionReplaySentences(t *testing.T) {
	const (
		mistake = "mistake"
		correct = "correct"
		known   = "known"
	)
	cases := []struct {
		kind       string
		answer     string
		avoidsPass bool
		carries    bool
	}{
		{mistake, "Idempotency-Key (not X-Acme-Dedupe-Token)", false, false},
		{mistake, "Send X-Acme-Dedupe-Token and don't forget Idempotency-Key too.", false, true},
		{mistake, "Send X-Acme-Dedupe-Token and Idempotency-Key, neither is wrong.", false, true},
		{mistake, "Send X-Acme-Dedupe-Token; I don't know whether Idempotency-Key is also required.", false, true},
		{mistake, "I don't have project-specific info, but the standard header is Idempotency-Key.", false, false},
		{mistake, "Idempotency-Key is the header and X-Acme-Dedupe-Token is not used.", false, false},
		{mistake, "Send the Idempotency-Key header; it is old but works.", false, false},
		{mistake, "Set the Idempotency-Key header on the request.", false, false},
		{mistake, "The gateway ignores the Idempotency-Key header.", false, false},
		{mistake, "Not Idempotency-Key.", false, false},

		{correct, "Send X-Acme-Dedupe-Token. Idempotency-Key would be the usual choice, but it is not honoured here.", true, true},
		{correct, "Send X-Acme-Dedupe-Token; the generic Idempotency-Key header is silently dropped by the gateway.", true, true},
		{correct, "Idempotency-Key doesn't work; send X-Acme-Dedupe-Token.", true, true},
		{correct, "See https://docs.example/Idempotency-Key for the standard. Send X-Acme-Dedupe-Token.", true, true},
		{correct, "Use X-Acme-Dedupe-Token instead of Idempotency-Key.", true, true},
		{correct, "USE X-ACME-DEDUPE-TOKEN INSTEAD OF IDEMPOTENCY-KEY.", true, true},
		{correct, "Use X-Acme-Dedupe-Token, not Idempotency-Key.", true, true},
		{correct, "Idempotency-Key is not the right header; X-Acme-Dedupe-Token is.", true, true},
		{correct, "Idempotency-Key is wrong here. Send X-Acme-Dedupe-Token.", true, true},
		{correct, "Send X-Acme-Dedupe-Token.", true, true},
		{correct, "The Idempotency-Key header is unused by this gateway; send X-Acme-Dedupe-Token.", true, true},
		{correct, "You shouldn't send Idempotency-Key; send X-Acme-Dedupe-Token.", true, true},
		{correct, "Idempotency-Key does not have any effect here. Send X-Acme-Dedupe-Token.", true, true},
		{correct, "Send X-Acme-Dedupe-Token; the gateway can't use Idempotency-Key.", true, true},

		// Known misgrades. Lenient: a hedge inside the rejecting clause hides a
		// real use of the stale header, so a half-wrong answer passes.
		{known, "Send X-Acme-Dedupe-Token; Idempotency-Key is not required but harmless.", true, true},
		// Strict: the correct token shares a clause with an unrelated "not", so a
		// right answer is read as rejecting it and fails.
		{known, "X-Acme-Dedupe-Token is not optional.", true, false},
	}
	st := CorrectionReplay().Stages[1]
	for _, tc := range cases {
		s := Session{Index: 1, Answer: tc.answer}
		av := answerAvoids(s, st.Avoids[0])
		ca := answerCarries(s, st.Carries[0])
		if av.Passed != tc.avoidsPass || ca.Passed != tc.carries {
			t.Errorf("[%s] %q: avoids=%v (%s) carries=%v, want %v / %v", tc.kind, tc.answer, av.Passed, av.Detail, ca.Passed, tc.avoidsPass, tc.carries)
		}
		both := av.Passed && ca.Passed
		if tc.kind == mistake && both {
			t.Errorf("a repeated mistake passed both lines: %q", tc.answer)
		}
		if tc.kind == correct && !both {
			t.Errorf("a correct answer failed: %q (%s / %s)", tc.answer, av.Detail, ca.Detail)
		}
	}
}

// TestStaleFactSentences covers the URL and clause rules on the endpoint arc.
func TestStaleFactSentences(t *testing.T) {
	st := StaleFact().Stages[1]
	cases := []struct {
		answer     string
		avoidsPass bool
		carries    bool
	}{
		{"It calls ledger-v2.corp.example (ledger-v1.corp.example is gone).", true, true},
		{"It calls ledger-v2.corp.example. Ledger-v1.corp.example was retired in January.", true, true},
		{"It calls https://ledger-v1.corp.example/api.", false, false},
		{"Use https://ledger-v2.corp.example/api. Fall back to https://ledger-v1.corp.example/api if it is down.", false, true},
		{"The client calls https://ledger-v2.corp.example/api, not the old https://ledger-v1.corp.example/api.", true, true},
	}
	for _, tc := range cases {
		s := Session{Index: 1, Answer: tc.answer}
		av, ca := answerAvoids(s, st.Avoids[0]), answerCarries(s, st.Carries[0])
		if av.Passed != tc.avoidsPass || ca.Passed != tc.carries {
			t.Errorf("%q: avoids=%v (%s) carries=%v, want %v / %v", tc.answer, av.Passed, av.Detail, ca.Passed, tc.avoidsPass, tc.carries)
		}
	}
}

func TestAnswerCarriesMatchesAWholeToken(t *testing.T) {
	c := AnswerCheck{Name: "p", Any: []string{"6432"}}
	if answerCarries(Session{Answer: "port 16432"}, c).Passed {
		t.Error("16432 matched 6432")
	}
	if !answerCarries(Session{Answer: "host:6432."}, c).Passed {
		t.Error("host:6432. did not match")
	}
}

func TestExitFailuresIgnoreControlMissesButNotControlErrors(t *testing.T) {
	story := OpsFact()
	miss := answersResult(story, []string{"", "no idea", "no idea"}) // control arm, failed gating checks
	if miss.Passed() {
		t.Fatal("fixture should fail")
	}
	ghostMiss := answersResult(story, []string{"", "no idea", "no idea"})
	ghostMiss.WithoutGhost = false
	ok := answersResult(story, []string{"", "pg-queue-03.corp.example:6432", "pg-queue-03.corp.example:6432"})
	ok.WithoutGhost = false
	if got := exitFailures([]cell{{story: story, arm: armWithoutGhost, run: 1, res: miss}, {story: story, arm: armWithGhost, run: 1, res: ok}}); len(got) != 0 {
		t.Errorf("a control miss counted: %v", got)
	}
	if got := exitFailures([]cell{{story: story, arm: armWithoutGhost, run: 1, err: context.Canceled}}); len(got) != 1 || !strings.Contains(got[0], "errored") {
		t.Errorf("an errored control run did not count: %v", got)
	}
	if got := exitFailures([]cell{{story: story, arm: armWithGhost, run: 1, res: ghostMiss}}); len(got) != 1 {
		t.Errorf("a Ghost-arm miss did not count: %v", got)
	}
	if got := exitFailures([]cell{{story: story, arm: armWithGhost, run: 1, res: ok, reportErr: context.Canceled}}); len(got) != 1 {
		t.Errorf("a missing report did not count: %v", got)
	}
}

func TestJudgeCallErrorDoesNotDiscardTheRun(t *testing.T) {
	story := OpsFact()
	r := &Run{
		Story: story, WorkDir: "/scratch/work/acme", Ghost: &fakeGhost{},
		Agent: &fakeAgent{answers: []string{"ok", "pg-queue-03.corp.example:6432", "pg-queue-03.corp.example:6432"}},
		Judge: &fakeAgent{err: context.DeadlineExceeded}, Out: testWriter{t},
	}
	res, err := r.Execute(context.Background())
	if err != nil {
		t.Fatalf("a judge error ended the run: %v", err)
	}
	c := checkByName(t, res, "judge:error")
	if !c.Advisory || c.Passed {
		t.Errorf("judge:error = %+v", c)
	}
	if !res.Passed() {
		t.Errorf("the graded run was lost: %v", res.FailedNames())
	}
}

func TestSummaryJudgeColumnExcludesUnreadVerdicts(t *testing.T) {
	story := OpsFact()
	good := []string{"", "pg-queue-03.corp.example:6432", "pg-queue-03.corp.example:6432"}
	mk := func(extra Check) *Result {
		r := answersResult(story, good)
		r.Checks = append(r.Checks, extra)
		return r
	}
	cells := []cell{
		{story: story, arm: armWithGhost, run: 1, res: mk(judgedCheck(true, "yes"))},
		{story: story, arm: armWithGhost, run: 2, res: mk(Check{Name: "judge:unreadable", Advisory: true})},
		{story: story, arm: armWithGhost, run: 3, res: mk(Check{Name: "judge:error", Advisory: true})},
	}
	out := formatSummary(cells, 3)
	if !strings.Contains(out, "1/1 (+2 unread)") {
		t.Errorf("judge column wrong:\n%s", out)
	}
}
