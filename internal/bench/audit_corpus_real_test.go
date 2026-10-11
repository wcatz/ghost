package bench

import (
	"context"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// realAuditGoldenPath is the committed real-set report: the numbers the judge
// scores today against the hand-labelled corpus. Regenerate with
// GHOST_UPDATE_GOLDEN=1 go test ./internal/bench -run TestRealAuditBaseline.
const realAuditGoldenPath = "testdata/audit_report_real.golden"

// The real-set floors: the measured figures, held so a judge change that regresses
// on hand-labelled real work fails CI rather than being found by the next reader.
// Every one is a MEASURED value and not a target. The set has three positives, so a
// change that moves them is a hypothesis about the judge, not a result.
//
// The cited-id floor is 1 since #1010 (an id in a save's arguments is a citation).
// The restatement floor stays 0, and that is a finding: the labelled restatements are
// clipped excerpts of long memories, and in two of three the identical excerpt sits in
// neighbouring turns the labels call NOT_USED or UNCLEAR, so no overlap statistic can
// admit one without admitting the other (see docs/benchmarks.md).
const (
	realFloorCites        = 1
	realFloorRestatements = 0
	realCeilingFalseUsed  = 0
	// The populations those figures are fractions of, pinned so a corpus quietly
	// trimmed of its labels cannot meet every floor above by shrinking.
	realCitesTotal        = 1
	realRestatementsTotal = 2
	realFalseTotal        = 33
	realTokenArmPairs     = 0
)

// realAuditRun is the real-set report, built once and shared: it opens one scratch
// store per call (twenty-three), each seeded with the corpus's thirty-six memories,
// and the baseline, the floors and the headline-shape check all read this one run.
// The report is read-only to its callers.
func realAuditRun(t *testing.T) AuditReport {
	t.Helper()
	realAuditOnce.Do(func() { realAuditShared, realAuditSharedErr = RunRealAuditTemp(context.Background()) })
	if realAuditSharedErr != nil {
		t.Fatalf("RunRealAuditTemp: %v", realAuditSharedErr)
	}
	return realAuditShared
}

var (
	realAuditOnce      sync.Once
	realAuditShared    AuditReport
	realAuditSharedErr error
)

// TestRealAuditCorpusLabelsAreConsistent checks the real-work corpus's own shape,
// in the spirit of the synthetic corpus's test: ids well formed and unique, every
// labelled turn naming a memory the corpus holds, and the counts pinned so a
// corpus edit cannot pass as an unchanged measurement.
//
// What it deliberately does NOT carry over from the synthetic test is the
// restatement's word-share bar. There a turn restates a whole memory; here a turn
// is a clipped excerpt of the agent's own text, so the share of a long memory's
// words it can hold is a property of the clip and not of the label. The bar this
// test uses is the comparison's own absolute floor — at least three distinct
// shared words — which is the fewest the token arm will accept of anything.
func TestRealAuditCorpusLabelsAreConsistent(t *testing.T) {
	c := NewRealAuditCorpus()
	mem := map[string]AuditMemory{}
	for _, m := range c.Memories {
		if !regexp.MustCompile(`^[0-9A-F]{32}$`).MatchString(m.ID) {
			t.Errorf("memory id %q is not 32 upper-case hex", m.ID)
		}
		if mem[m.ID].ID != "" {
			t.Errorf("duplicate memory id %s", m.ID)
		}
		mem[m.ID] = m
	}
	if len(c.Memories) != 36 {
		t.Errorf("%d memories, want the thirty-six notes the study hand-labelled", len(c.Memories))
	}
	if len(c.Turns) != 94 {
		t.Errorf("%d turns, want the ninety-four scripted turn items", len(c.Turns))
	}
	if len(c.Calls) != 23 {
		t.Errorf("%d calls, want the twenty-three recorded retrievals", len(c.Calls))
	}

	var cites, restates, plain int
	for i, turn := range c.Turns {
		labelled := map[string]bool{}
		for _, id := range turn.Cites {
			cites++
			labelled[id] = true
			if !strings.Contains(strings.ToUpper(turn.Text), id) {
				t.Errorf("turn %d cites %s but its text does not hold the full id", i, id)
			}
		}
		for _, id := range turn.Restates {
			restates++
			labelled[id] = true
			m, ok := mem[id]
			if !ok {
				t.Errorf("turn %d restates unknown memory %s", i, id)
				continue
			}
			mw, tw := distinctSet(localWords(m.Content)), distinctSet(localWords(turn.Text))
			shared := 0
			for w := range mw {
				if tw[w] {
					shared++
				}
			}
			if shared < 3 {
				t.Errorf("turn %d is labelled a restatement of %s but shares %d of its %d words, under the token arm's floor of three", i, id, shared, len(mw))
			}
		}
		for id := range labelled {
			if _, ok := mem[id]; !ok {
				t.Errorf("turn %d labels unknown memory %s", i, id)
			}
		}
		if len(labelled) == 0 {
			plain++
		}
	}
	if cites != 1 || restates != 3 {
		t.Errorf("labels: %d cites, %d restates; want the one cited id and the three restatements the study labelled USED", cites, restates)
	}
	// The population the false-"used" figure is a fraction of: every turn that
	// carries no label slice is scored as expected-ignored. That is the study's
	// NOT_USED turns and also its UNCLEAR ones, so the figure covers undecided
	// pairs as well as settled negatives.
	if plain != len(c.Turns)-cites-restates {
		t.Errorf("%d unlabelled turns, want %d", plain, len(c.Turns)-cites-restates)
	}

	// Every call names a domain, and the domains that hold nothing are named too:
	// the study recorded calls whose window kept no memory at all, and they are
	// part of the measurement rather than a gap in it. A call placed outside the
	// session would have no turns after it, and then every figure it carries would
	// be the same zero.
	last := 0
	for _, t := range c.Turns {
		if t.Minute > last {
			last = t.Minute
		}
	}
	named := map[string]bool{}
	for _, call := range c.Calls {
		if len(call.Domains) == 0 {
			t.Errorf("call %s names no domain", call.Name)
		}
		for _, d := range call.Domains {
			named[d] = true
		}
		if call.Before < 0 || call.Before > last {
			t.Errorf("call %s is placed before turn %d, outside the session's %d minutes", call.Name, call.Before, last)
		}
	}
	kept := 0
	for _, m := range c.Memories {
		if named[m.Domain] {
			kept++
		}
	}
	if kept != len(c.Memories) {
		t.Errorf("%d of %d memories are in a domain some call names, so the rest are seeded and never kept", kept, len(c.Memories))
	}
}

// TestRealAuditBaseline pins the real-set report to its committed text. A change to
// the comparison, a threshold or a scanner that moves any figure fails here and
// has to say so in the golden, which is the review surface.
func TestRealAuditBaseline(t *testing.T) {
	got := FormatAudit(realAuditRun(t))
	if os.Getenv("GHOST_UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(realAuditGoldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(realAuditGoldenPath)
	if err != nil {
		t.Fatalf("read golden: %v (generate it with GHOST_UPDATE_GOLDEN=1)", err)
	}
	if got != string(want) {
		t.Errorf("real-set audit report moved.\n--- want\n%s\n--- got\n%s", want, got)
	}
}

// TestRealAuditFloors is the gate the real set exists for: the judge's numbers on
// hand-labelled real work cannot fall below what they are today without CI going
// red.
//
// Read the floors honestly: the restatement floor is zero and gates nothing. Token-arm
// precision is still undefined because the arm files nothing on this set. The cited-id
// floor of 1 is a real gate on the single labelled citation. The false-"used" ceiling
// is the number the real set exists to protect.
func TestRealAuditFloors(t *testing.T) {
	h := realAuditRun(t).Headlines()
	// The two recall-side floors; the restatement one is zero today (see above) and
	// is written as a comparison against the constant so that raising it is a
	// one-constant edit.
	if h.CitesCaught < realFloorCites {
		t.Errorf("cited ids caught %d of %d, floor %d", h.CitesCaught, h.CitesTotal, realFloorCites)
	}
	if h.RestatementsCaught < realFloorRestatements {
		t.Errorf("restatements caught %d of %d, floor %d", h.RestatementsCaught, h.RestatementsTotal, realFloorRestatements)
	}
	if h.FalseUsed > realCeilingFalseUsed {
		t.Errorf("false \"used\" %d of %d, ceiling %d: a judge that calls more unlabelled real-work pairs used has regressed", h.FalseUsed, h.FalseTotal, realCeilingFalseUsed)
	}
	if h.CitesTotal != realCitesTotal || h.RestatementsTotal != realRestatementsTotal || h.FalseTotal != realFalseTotal {
		t.Errorf("populations moved: cited %d (want %d), restated %d (want %d), expected-ignored %d (want %d)",
			h.CitesTotal, realCitesTotal, h.RestatementsTotal, realRestatementsTotal, h.FalseTotal, realFalseTotal)
	}
	if h.TokenPrecision.Den != realTokenArmPairs {
		t.Errorf("the token arm filed %s, want the %d pairs it files today: a real-set precision floor can only be written once the arm has a population", h.TokenPrecision, realTokenArmPairs)
	}
	if h.SessionStart {
		t.Error("the real-work corpus has no scenario named start; a start scenario here would read its headline off a population the corpus does not have")
	}
}
