package bench

import (
	"context"
	"strings"
	"testing"
)

// loadNegativeFixture loads the committed no-answer queries.
func loadNegativeFixture(t *testing.T) []NegativeQuery {
	t.Helper()
	negs, err := loadFile("testdata/negative_queries.jsonl", LoadNegatives)
	if err != nil {
		t.Fatalf("load no-answer queries: %v", err)
	}
	return negs
}

// TestFalsePositiveReport runs the no-answer set through the production search
// path and logs the baseline. Report-only, for the same reason the maintenance
// suite is: the numbers are the starting point the abstention work (#580) moves,
// and a floor here would be a gate on that work rather than on this change.
//
// What IS enforced is that the set can measure something at all — every flavor
// present, every query embedded, and results actually returned for queries
// nothing answers. A suite where search returned nothing for any of them would
// report a beautiful 0.000 and mean nothing.
func TestFalsePositiveReport(t *testing.T) {
	ds, vecs, err := BuiltinDataset()
	if err != nil {
		t.Fatalf("BuiltinDataset: %v", err)
	}
	negs := ds.Negatives
	if len(negs) < 20 {
		t.Fatalf("fixture has %d no-answer queries, want >= 20", len(negs))
	}
	flavors := map[string]int{}
	for _, n := range negs {
		flavors[n.Flavor]++
		if len(n.Rel) != 0 {
			t.Errorf("no-answer query %q grades %d memories", n.Name, len(n.Rel))
		}
	}
	for _, flavor := range []string{"off_domain", "near_miss"} {
		if flavors[flavor] == 0 {
			t.Errorf("no %s no-answer queries: one flavor alone cannot show what an abstain rule has to survive", flavor)
		}
	}

	store := newBenchStore(t)
	ctx := context.Background()
	graded, err := Seed(ctx, store, ds, vecs)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	noAnswer, err := NegativeQueries(ds, vecs)
	if err != nil {
		t.Fatalf("NegativeQueries: %v", err)
	}
	rep, err := FalsePositives(ctx, store, noAnswer, graded)
	if err != nil {
		t.Fatalf("FalsePositives: %v", err)
	}
	if rep.Queries != len(noAnswer) || rep.Answerable != len(graded) {
		t.Errorf("report counts %d/%d queries, want %d/%d", rep.Queries, rep.Answerable, len(noAnswer), len(graded))
	}
	if len(rep.Floors) != len(FalsePositiveFloors) {
		t.Errorf("report has %d floor rows, want %d", len(rep.Floors), len(FalsePositiveFloors))
	}
	if rep.MeanResults < float64(scoreK) {
		t.Errorf("mean results per no-answer query is %.1f, but the search returns a full window of %d when nothing is filtered: "+
			"the false-positive count is not being measured", rep.MeanResults, scoreK)
	}
	if rep.MeanTop <= 0 {
		t.Error("no-answer queries scored a mean top cosine of 0, so nothing was measured")
	}
	if rep.Unseparable < 0 || rep.Unseparable > rep.Answerable {
		t.Errorf("unseparable count %d is outside 0..%d", rep.Unseparable, rep.Answerable)
	}
	// The one behavioural claim worth enforcing: a no-answer query must not
	// look better than an answerable one. If the corpus's own questions scored
	// no higher than questions it cannot answer, no threshold could ever abstain
	// and the fix would have to be something other than a score.
	if rep.MeanTop >= rep.AnswerableTop {
		t.Errorf("no-answer queries score %.3f on average against %.3f for answerable ones: "+
			"the score cannot tell them apart at all", rep.MeanTop, rep.AnswerableTop)
	}
	if rep.NoAnswerMax < rep.MeanTop {
		t.Errorf("no-answer maximum %.3f is below the mean %.3f", rep.NoAnswerMax, rep.MeanTop)
	}
	t.Logf("no-answer queries against the graded corpus:\n%s", FormatFalsePositives(rep))
}

func TestLoadNegativesRejectsGradedQueries(t *testing.T) {
	cases := []struct {
		name, line, want string
	}{
		{"no name", `{"text":"t","flavor":"off_domain","rel":{}}`, "empty name"},
		{"no text", `{"name":"n","flavor":"off_domain","rel":{}}`, "no text"},
		{"no flavor", `{"name":"n","text":"t","rel":{}}`, "no flavor"},
		{"grades a memory", `{"name":"n","text":"t","flavor":"off_domain","rel":{"a":1}}`, "rel must stay empty"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := LoadNegatives(strings.NewReader(c.line)); err == nil {
				t.Fatalf("LoadNegatives accepted %s", c.line)
			} else if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

func TestNegativeQueriesRequireVectors(t *testing.T) {
	ds := Dataset{Project: "p", Negatives: []NegativeQuery{{
		QuerySpec: QuerySpec{Name: "n_missing", Text: "t"},
		Flavor:    "off_domain",
	}}}
	if _, err := NegativeQueries(ds, Vectors{}); err == nil {
		t.Fatal("a no-answer query with no fixture vector was accepted")
	} else if !strings.Contains(err.Error(), "no fixture vector") {
		t.Errorf("error %q does not name the missing vector", err)
	}
}
