package main

import (
	"os"
	"strings"
	"testing"
)

// TestQualifyNamesTheMissingEvidence: a run is qualified against what its arm
// claims to measure. The Ghost arm has to have used Ghost, received a block and
// reached every stage and probe; the first thing missing is the reason it cannot
// be scored.
func TestQualifyNamesTheMissingEvidence(t *testing.T) {
	story := ReversedDecision()
	short := &Result{Story: story, SaveOutputs: map[string]string{"seed": "id"}}
	short.Sessions = []Session{{Index: 0, Block: "## Ghost context"}}
	cases := []struct {
		name string
		res  *Result
		want string
	}{
		{"no ghost activity", &Result{Story: story}, "no ghost tool call"},
		{"no injection", &Result{Story: story, SaveOutputs: map[string]string{"seed": "id"}}, "session-start block"},
		{"short run", short, "ended before the probe"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason := qualify(tc.res)
			if reason == "" {
				t.Fatal("a run that never exercised the mechanism qualified")
			}
			if !strings.Contains(reason, tc.want) {
				t.Fatalf("reason %q does not name %q", reason, tc.want)
			}
		})
	}
}

// TestQualifyAcceptsACompleteGhostArmRun: the clean fixture is the control — a
// run that used Ghost, carried a block and reached every stage is counted.
func TestQualifyAcceptsACompleteGhostArmRun(t *testing.T) {
	if reason := qualify(gradedResult(t)); reason != "" {
		t.Fatalf("a complete run was disqualified: %s", reason)
	}
}

// TestDisqualifiedRunsAreKeptOutOfTheArmScore: a disqualified run is reported
// with its reason, counted in qualified/total, and never folded into a score or
// an inconclusive verdict the arm's other runs might have earned.
func TestDisqualifiedRunsAreKeptOutOfTheArmScore(t *testing.T) {
	story := CorrectionReplay()
	good := answersResult(story, []string{"", "Send X-Acme-Dedupe-Token.", "Send X-Acme-Dedupe-Token."})
	bad := answersResult(story, []string{"", "Send Idempotency-Key.", "Send Idempotency-Key."})
	bad.Disqualified = "the run ended before the probe session"
	cells := []cell{
		{story: story, arm: armWithoutGhost, run: 1, res: good},
		{story: story, arm: armWithoutGhost, run: 2, res: bad},
	}
	out := formatSummary(cells, 2, defaultMinQualified)
	for _, want := range []string{
		"| without-ghost | 2 | 1/2 (inconclusive) | 1/1 | 0/1 |",
		"run 2: the run ended before the probe session",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "| without-ghost | 2 | 1/2 (inconclusive) | 1/2 |") {
		t.Errorf("a disqualified run was folded into the score:\n%s", out)
	}
}

// TestInconclusiveGhostArmDoesNotFailTheRun: the threshold that marks an arm
// inconclusive in the summary also makes it neutral at exit. One qualified
// failure in ten is below the default threshold, so its failed checks must not
// make run() return an error; with no threshold the same run does count.
func TestInconclusiveGhostArmDoesNotFailTheRun(t *testing.T) {
	story := OpsFact()
	qualifiedFailure := answersResult(story, []string{"", "no idea", "no idea"})
	qualifiedFailure.WithoutGhost = false
	if qualifiedFailure.Passed() {
		t.Fatal("fixture should fail")
	}
	cells := []cell{{story: story, arm: armWithGhost, run: 1, res: qualifiedFailure}}
	for n := 2; n <= 10; n++ {
		cells = append(cells, cell{
			story: story, arm: armWithGhost, run: n,
			res: &Result{Story: story, Disqualified: "no ghost tool call was observed"},
		})
	}
	if err := exitError(cells, defaultMinQualified); err != nil {
		t.Fatalf("an inconclusive arm failed the run: %v", err)
	}
	if err := exitError(cells, 0); err == nil {
		t.Fatal("the same failing run counted with no qualification threshold")
	}
}

// TestRunRejectsAnOutOfRangeMinQualified: the fraction that decides whether an
// arm is inconclusive has to be a fraction; the check runs before any work.
func TestRunRejectsAnOutOfRangeMinQualified(t *testing.T) {
	for _, v := range []float64{-0.1, 1.1} {
		if err := run(config{minQualified: v}); err == nil || !strings.Contains(err.Error(), "min-qualified") {
			t.Fatalf("run accepted -min-qualified %v: %v", v, err)
		}
	}
}

// TestWriteReportMarksADisqualifiedRun: the per-run artifact says why the run
// was dropped instead of presenting an empty grade as a quiet failure.
func TestWriteReportMarksADisqualifiedRun(t *testing.T) {
	res := &Result{Story: ReversedDecision(), Disqualified: "no ghost tool call was observed"}
	path, err := writeReportAs(t.TempDir(), "run.md", res, "model")
	if err != nil {
		t.Fatalf("write report: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	for _, want := range []string{"DISQUALIFIED", "no ghost tool call was observed"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("report is missing %q:\n%s", want, b)
		}
	}
}
