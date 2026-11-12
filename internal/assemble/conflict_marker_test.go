package assemble

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// lineOf returns the one rendered line that names id, failing when there is not
// exactly one: a marker that spilled onto a second physical line would otherwise
// be found by a substring check and pass.
func lineOf(t *testing.T, response, id string) string {
	t.Helper()
	var found []string
	for _, l := range strings.Split(response, "\n") {
		if strings.HasPrefix(l, "- [") && strings.Contains(l, "`"+Token(id)+"` (") {
			found = append(found, l)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one line for %s, got %d in:\n%s", id, len(found), response)
	}
	return found[0]
}

func contradictingSet(rows ...memory.Candidate) *memory.CandidateSet {
	set := setOf(rows...)
	set.Edges = []memory.LinkEdge{{From: rows[0].ID, To: rows[1].ID, Relation: "contradicts", Strength: 1}}
	set.EdgesStatus = memory.EdgeStatus{Status: "ok"}
	return set
}

// TestContradictsPairRendersTheWinnerNamingTheWithheldPartner: the pair is
// separated, so one row renders and its line names the side stage 5 withheld.
func TestContradictsPairRendersTheWinnerNamingTheWithheldPartner(t *testing.T) {
	a := candidate("A1", "proj", "fact", "the database is postgres", 0.9)
	b := candidate("B1", "proj", "fact", "the database is mysql", 0.8)
	req := baseRequest()
	req.Budget.MaxItems = 10
	res := run(t, &fakeRetriever{set: contradictingSet(a, b)}, req)
	if got := itemIDs(res.Items); !eq(got, []string{"A1"}) {
		t.Fatalf("the winner alone must stay: %v", got)
	}
	if got := renderedLines(res.Response, "B1"); len(got) != 0 {
		t.Errorf("the withheld side rendered: %v", got)
	}
	if la := lineOf(t, res.Response, "A1"); !strings.Contains(la, "conflicts_with=`B1`") {
		t.Errorf("A1 does not name the withheld B1: %q", la)
	}
}

func TestItemLineRendersConflictsWith(t *testing.T) {
	it := Item{ID: "A1", Category: "fact", Content: "x", ConflictsWith: []string{"B1", "C1"}}
	if l := it.Line(); !strings.Contains(l, " conflicts_with=`B1`,`C1`)") || strings.Count(l, "\n") != 0 {
		t.Errorf("Line() = %q", l)
	}
	it.ConflictsWith = nil
	if strings.Contains(it.Line(), "conflicts_with") {
		t.Errorf("an unpaired row is marked: %q", it.Line())
	}
}
