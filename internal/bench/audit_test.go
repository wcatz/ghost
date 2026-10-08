package bench

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
)

// auditGoldenPath is the committed report: the numbers the retrieval audit scores
// today against the labelled offline session. Regenerate with
// GHOST_UPDATE_GOLDEN=1 go test ./internal/bench -run TestAuditBaseline.
const auditGoldenPath = "testdata/audit_report.golden"

func auditRun(t *testing.T) AuditReport {
	t.Helper()
	rep, err := RunAuditTemp(context.Background())
	if err != nil {
		t.Fatalf("RunAuditTemp: %v", err)
	}
	return rep
}

// TestAuditBaseline pins the report to its committed text. A change to the
// comparison, a threshold or a scanner that moves any figure fails here and has to
// say so in the golden, which is the review surface.
func TestAuditBaseline(t *testing.T) {
	got := FormatAudit(auditRun(t))
	if os.Getenv("GHOST_UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(auditGoldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(auditGoldenPath)
	if err != nil {
		t.Fatalf("read golden: %v (generate it with GHOST_UPDATE_GOLDEN=1)", err)
	}
	if got != string(want) {
		t.Errorf("audit report moved.\n--- want\n%s\n--- got\n%s", want, got)
	}
}

// TestAuditReportIsStableAcrossRuns: two runs print the same bytes, which is what
// makes the golden a property of the code and not of a machine or a second.
func TestAuditReportIsStableAcrossRuns(t *testing.T) {
	if a, b := FormatAudit(auditRun(t)), FormatAudit(auditRun(t)); a != b {
		t.Errorf("two runs differ\n--- first\n%s\n--- second\n%s", a, b)
	}
}

// localWords is a local lower-cased word split, deliberately NOT the product's
// hasher: the label check is a sanity check on the corpus, not a second
// implementation of the comparison.
func localWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
}

func distinctSet(words []string) map[string]bool {
	m := map[string]bool{}
	for _, w := range words {
		m[w] = true
	}
	return m
}

var auditDenialCues = []string{"is wrong", "is incorrect", "is false", "is obsolete", "is outdated", "is stale", "is deprecated", "is superseded", "no longer", "disregard", "ignore"}

func TestAuditCorpusLabelsAreConsistent(t *testing.T) {
	c := NewAuditCorpus()
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
	if len(c.Memories) != 40 {
		t.Fatalf("%d memories, want the forty sentences of the audit's own fixtures", len(c.Memories))
	}

	var cites, restates, denies, saves, plain int
	for i, turn := range c.Turns {
		lower := strings.ToLower(turn.Text)
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
			if shared < 3 || shared*3 < len(mw) {
				t.Errorf("turn %d is labelled a restatement of %s but shares %d of its %d words", i, id, shared, len(mw))
			}
		}
		for _, id := range turn.Denies {
			denies++
			labelled[id] = true
			cue := false
			for _, w := range auditDenialCues {
				cue = cue || strings.Contains(lower, w)
			}
			if !cue {
				t.Errorf("turn %d is labelled a denial of %s and holds no cue word", i, id)
			}
		}
		for _, id := range turn.Saves {
			saves++
			labelled[id] = true
			if turn.Kind != AuditSaveArgs {
				t.Errorf("turn %d saves %s but is kind %q", i, id, turn.Kind)
			}
		}
		if len(labelled) == 0 {
			plain++
			for id := range mem {
				if strings.Contains(strings.ToUpper(turn.Text), id) {
					t.Errorf("unlabelled turn %d holds the full id %s", i, id)
				}
			}
		}
		for id := range labelled {
			if _, ok := mem[id]; !ok {
				t.Errorf("turn %d labels unknown memory %s", i, id)
			}
		}
	}
	if cites < 3 || restates < 4 || denies < 2 || saves < 1 || plain < 20 {
		t.Errorf("labels: %d cites, %d restates, %d denies, %d saves, %d unlabelled; want at least 3, 4, 2, 1, 20", cites, restates, denies, saves, plain)
	}
}

func TestAuditBenchIdentifierArmIsExact(t *testing.T) {
	for _, sc := range auditRun(t).Scenarios {
		p := sc.Score.Class[ClassUsedIdentifier].Precision
		if p.Num != p.Den {
			t.Errorf("%s: identifier-arm precision %s, want 1.000: the id arm names the memory and nothing else", sc.Name, p)
		}
	}
}

func TestAuditBenchACallAfterTheLastTurnIsNeverUsed(t *testing.T) {
	found := false
	for _, sc := range auditRun(t).Scenarios {
		if sc.Name != "end" {
			continue
		}
		found = true
		if len(sc.Pairs) == 0 {
			t.Fatal("the end scenario judged nothing, so zero `used` proves nothing")
		}
		for _, p := range sc.Pairs {
			if p.Got == ClassUsedIdentifier || p.Got == ClassUsedToken {
				t.Errorf("end: %s judged %s by a call placed after every turn", p.MemoryID, p.Got)
			}
		}
	}
	if !found {
		t.Fatal("no end scenario")
	}
}

// TestAuditScorerFlagsWhatItIsGiven proves the scorer is not vacuous: handed a
// verdict list that ignores everything it must report zero recall on every
// positive outcome, and handed one that calls everything used it must report a
// precision below the product's.
func TestAuditScorerFlagsWhatItIsGiven(t *testing.T) {
	rep := auditRun(t)
	var start AuditScenario
	for _, sc := range rep.Scenarios {
		if sc.Name == "start" {
			start = sc
		}
	}
	if len(start.Pairs) == 0 {
		t.Fatal("no start scenario")
	}
	with := func(got AuditClass) []AuditPair {
		out := make([]AuditPair, len(start.Pairs))
		for i, p := range start.Pairs {
			p.Got = got
			out[i] = p
		}
		return out
	}

	ignored := ScoreAudit(with(ClassIgnored))
	for _, cl := range []AuditClass{ClassUsedIdentifier, ClassUsedToken, ClassSuperseded, ClassContradicted} {
		r := ignored.Class[cl].Recall
		if r.Den == 0 {
			t.Errorf("%s: the corpus expects none, so recall is untested", cl)
		}
		if r.Num != 0 {
			t.Errorf("%s: all-ignored recall %s, want 0", cl, r)
		}
	}

	used := ScoreAudit(with(ClassUsedToken))
	base := start.Score.Class[ClassUsedToken].Precision
	got := used.Class[ClassUsedToken].Precision
	if got.Den == 0 || got.Num*base.Den >= base.Num*got.Den {
		t.Errorf("all-used token precision %s is not below the baseline %s", got, base)
	}
}

func TestAuditHeadlinesArePrinted(t *testing.T) {
	out := FormatAudit(auditRun(t))
	for _, re := range []string{
		`(?m)^same-domain used at session start: \d+ of 20$`,
		`(?m)^cited ids caught: \d+ of \d+$`,
		`(?m)^restatements caught: \d+ of \d+$`,
	} {
		if !regexp.MustCompile(re).MatchString(out) {
			t.Errorf("report lacks a line matching %s:\n%s", re, out)
		}
	}
}
