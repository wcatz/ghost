package supersede

import (
	"context"
	"strings"
	"testing"
)

// The live eval (live_test.go) measures the prompt against a real harness, and
// this file measures the two deterministic halves of the same decision over the
// same fixtures, with canned replies: the parser (does a verdict line become the
// right Relation) and the veto (does a pair never reach the prompt). A green live
// run on a free model is worth little without these, because both halves decide
// pairs before the model is ever asked — and both are free.

// TestSupersedeEvalSetCoversBothClasses is the fixture's own contract: the eval
// is only a measurement of #686's failure if it holds at least a dozen pairs of
// each class, and a set that drifts to one class silently reports a number that
// says nothing. Both the count and the mix are pinned here rather than in the
// live test, which is skipped in CI.
func TestSupersedeEvalSetCoversBothClasses(t *testing.T) {
	supersessions, bothTrue, vetoed := 0, 0, 0
	seen := make(map[string]bool, len(liveSupersedeCases))
	for _, c := range liveSupersedeCases {
		if c.name == "" || c.newer == "" || c.older == "" {
			t.Fatalf("fixture with a missing field: %+v", c)
		}
		if seen[c.name] {
			t.Errorf("duplicate fixture name %q", c.name)
		}
		seen[c.name] = true
		if c.class == "" {
			t.Errorf("%s: a fixture with no class label cannot be read back as one of the issue's cases", c.name)
		}
		switch c.want {
		case RelationSupersedes:
			supersessions++
		case RelationNeither:
			bothTrue++
		default:
			t.Errorf("%s: the eval scores SUPERSEDES against NEITHER only, got %q", c.name, c.want)
		}
		if c.veto {
			vetoed++
		}
	}
	if supersessions < 12 {
		t.Errorf("%d labeled supersession(s), want at least 12", supersessions)
	}
	if bothTrue < 12 {
		t.Errorf("%d both-true pair(s), want at least 12", bothTrue)
	}
	if vetoed == 0 {
		t.Error("no fixture exercises the deterministic veto, so the free half of the decision is unscored here")
	}
	if vetoed > bothTrue {
		t.Errorf("%d fixture(s) marked vetoed but only %d are both-true pairs: the veto may never settle a real supersession", vetoed, bothTrue)
	}
}

// TestSupersedeEvalFixturesAgreeWithTheVeto holds the fixtures' veto labels to
// the production function rather than to the test's own reading of them. A label
// that drifts from the code would make the live eval quietly misreport what the
// veto caught, and the drift is invisible in a skipped test.
func TestSupersedeEvalFixturesAgreeWithTheVeto(t *testing.T) {
	for _, c := range liveSupersedeCases {
		_, vetoed := VetoSupersede(evalCandidate(c))
		if vetoed != c.veto {
			t.Errorf("%s: VetoSupersede = %v, fixture says %v", c.name, vetoed, c.veto)
		}
		if c.veto && c.want == RelationSupersedes {
			t.Errorf("%s: a fixture cannot be labeled both vetoed and a true supersession — the veto would lose a real edge", c.name)
		}
	}
}

// TestSupersedeEvalParserOverTheFixtures drives every labeled pair's canned
// verdict through the parser, on both reply shapes the pass accepts, and asserts
// the decision the shipped path would reach. The canned replies are the ones a
// correct model is being asked for:
//
//   - a true supersession answers SUPERSEDES with the older note's claim named,
//     and stays SUPERSEDES;
//   - a both-true pair answers NEITHER, and stays NEITHER;
//   - and the reasonless shape — SUPERSEDES with no `replaced:` claim, which is
//     what a model that only half follows the format produces — reads NEITHER
//     even for a true supersession. That last one is the KEEP-bias: the pair
//     keeps its stale note ranked instead of demoting a live one, and it is
//     scored as a miss here so the recall cost stays visible.
func TestSupersedeEvalParserOverTheFixtures(t *testing.T) {
	claims := map[Relation]string{
		RelationSupersedes: "it runs Redis 6.2",
		RelationNeither:    "",
	}
	for _, shape := range []struct {
		name string
		// reply renders the line for pair 1 of a two-pair call, so the batched
		// shape really goes through parseBatchVerdict — a "1: SUPERSEDES | …"
		// string handed to Classify would be read by parseRelation and the
		// batched parser would never run.
		reply func(Relation, string) string
		// batched says which parser the shape must reach.
		batched bool
	}{
		{
			name:    "single pair",
			batched: false,
			reply: func(v Relation, claim string) string {
				if v != RelationSupersedes {
					return string(v)
				}
				return "SUPERSEDES | replaced: " + claim
			},
		},
		{
			name:    "batched pairs",
			batched: true,
			reply: func(v Relation, claim string) string {
				if v != RelationSupersedes {
					return "1: " + string(v) + "\n2: NEITHER"
				}
				return "1: SUPERSEDES | replaced: " + claim + "\n2: NEITHER"
			},
		},
		{
			name:    "reasonless",
			batched: false,
			reply: func(v Relation, _ string) string {
				if v != RelationSupersedes {
					return string(v)
				}
				return "SUPERSEDES"
			},
		},
	} {
		for _, c := range liveSupersedeCases {
			// A reasonless answer is NEITHER for every pair, whichever class the
			// fixture is; a named claim is read as the answer the fixture labels.
			want := c.want
			if shape.name == "reasonless" && c.want == RelationSupersedes {
				want = RelationNeither
			}
			if _, vetoed := VetoSupersede(evalCandidate(c)); vetoed {
				// A vetoed pair never reaches the parser at all, so its reply is
				// the NEITHER the veto stands for.
				want = RelationNeither
			}
			reply := shape.reply(c.want, claims[c.want])
			cls := NewRelationClassifier(&fakeProvider{resp: reply})
			var got Relation
			var err error
			if shape.batched {
				// Two pairs, so the chunked path is the one a real pass takes.
				var got1 []Relation
				got1, err = cls.ClassifyBatch(context.Background(), []Candidate{
					evalCandidate(c),
					{NewerID: "n2", NewerContent: "n2", OlderID: "o2", OlderContent: "o2"},
				})
				if err == nil {
					if len(got1) != 2 {
						t.Fatalf("%s/%s: got %d verdicts for 2 pairs", shape.name, c.name, len(got1))
					}
					got = got1[0]
				}
			} else {
				got, err = cls.Classify(context.Background(), evalCandidate(c))
			}
			if err != nil {
				t.Fatalf("%s/%s: classify %q: %v", shape.name, c.name, reply, err)
			}
			if got != want {
				t.Errorf("%s/%s: verdict = %q, want %q (reply %q)", shape.name, c.name, got, want, reply)
			}
		}
	}
}

// TestSupersedeEvalFixturesCarryNothingReal: the eval's text is invented, and a
// real memory pasted into a fixture would put stored note content — and whatever
// it was about — into the repository, permanently and publicly. Nothing can
// prove a fixture is invented, but the shapes a pasted note almost always
// carries can be refused: a filesystem path, a URL, an email address, a private
// address, and the prompt's own data delimiters (which quoteData would have to
// rewrite). Each of those is a plausible line in a real note about a real
// deployment and impossible in a sentence written to be generic.
func TestSupersedeEvalFixturesCarryNothingReal(t *testing.T) {
	forbidden := []struct {
		what  string
		shape string
	}{
		{"a filesystem path", "/home/"},
		{"a macOS path", "/Users/"},
		{"a Windows path", `C:\\`},
		{"a URL", "http://"},
		{"a secure URL", "https://"},
		{"an email address", "@"},
		{"a private IPv4 address", "192.168."},
		{"a link-local IPv4 address", "169.254."},
		{"a prompt data delimiter", "«"},
		{"a prompt data delimiter", "»"},
	}
	for _, c := range liveSupersedeCases {
		for _, field := range []struct{ what, text string }{{"older", c.older}, {"newer", c.newer}} {
			for _, f := range forbidden {
				if strings.Contains(field.text, f.shape) {
					t.Errorf("%s: the %s note carries %s (%q) — a real note pasted into a fixture leaks whatever it was about",
						c.name, field.what, f.what, f.shape)
				}
			}
		}
	}
}
