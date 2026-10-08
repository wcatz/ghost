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

// TestContradictsMarkerIsOrderIndependent: an edge may be stored either way
// round. Both orders describe one fact, so both keep A1 (rank) and mark it with
// the withheld B1.
func TestContradictsMarkerIsOrderIndependent(t *testing.T) {
	a := candidate("A1", "proj", "fact", "one", 0.9)
	b := candidate("B1", "proj", "fact", "two", 0.8)
	for _, order := range [][2]string{{"A1", "B1"}, {"B1", "A1"}} {
		set := setOf(a, b)
		set.Edges = []memory.LinkEdge{{From: order[0], To: order[1], Relation: "contradicts", Strength: 1}}
		set.EdgesStatus = memory.EdgeStatus{Status: "ok"}
		req := baseRequest()
		req.Budget.MaxItems = 10
		res := run(t, &fakeRetriever{set: set}, req)
		if got := itemIDs(res.Items); !eq(got, []string{"A1"}) {
			t.Errorf("edge %v: items = %v, want only A1", order, got)
			continue
		}
		if la := lineOf(t, res.Response, "A1"); !strings.Contains(la, "conflicts_with=`B1`") {
			t.Errorf("edge %v: A1 must name the withheld B1: %q", order, la)
		}
	}
}

// TestContradictsMarkerNamesEveryPartner: one survivor names every row it was
// separated from, so a reader can look each one up.
func TestContradictsMarkerNamesEveryPartner(t *testing.T) {
	a := candidate("A1", "proj", "fact", "one", 0.9)
	b := candidate("B1", "proj", "fact", "two", 0.8)
	c := candidate("C1", "proj", "fact", "three", 0.7)
	set := setOf(a, b, c)
	set.Edges = []memory.LinkEdge{
		{From: "A1", To: "B1", Relation: "contradicts", Strength: 1},
		{From: "C1", To: "A1", Relation: "contradicts", Strength: 1},
	}
	set.EdgesStatus = memory.EdgeStatus{Status: "ok"}
	req := baseRequest()
	req.Budget.MaxItems = 10
	res := run(t, &fakeRetriever{set: set}, req)
	if got := itemIDs(res.Items); !eq(got, []string{"A1"}) {
		t.Fatalf("the component must keep one row: %v", got)
	}
	if la := lineOf(t, res.Response, "A1"); !strings.Contains(la, "conflicts_with=`B1`,`C1`") {
		t.Errorf("A1 must name both withheld partners: %q", la)
	}
}

// TestContradictsMarkerNeedsBothSidesRendered: a side an earlier stage withheld
// means the pair never reached stage 5, so the survivor says nothing. A side the
// final budget cut DID reach stage 5, and the survivor names it.
func TestContradictsMarkerNeedsBothSidesRendered(t *testing.T) {
	expired := "2020-01-01 00:00:00"
	kept := candidate("KEEP", "proj", "fact", "admitted row", 0.9)
	gone := candidate("GONE", "proj", "fact", "expired row", 0.8)
	gone.ValidUntil = &expired
	req := baseRequest()
	req.Budget.MaxItems = 10
	res := run(t, &fakeRetriever{set: contradictingSet(kept, gone)}, req)
	if got := itemIDs(res.Items); !eq(got, []string{"KEEP"}) {
		t.Fatalf("precondition: one admitted row, got %v", got)
	}
	if l := lineOf(t, res.Response, "KEEP"); strings.Contains(l, "conflicts_with") {
		t.Errorf("a pair whose other side was withheld before stage 5 is marked: %q", l)
	}

	a := candidate("A1", "proj", "fact", "one", 0.9)
	b := candidate("B1", "proj", "fact", "two", 0.8)
	cutReq := baseRequest()
	cutReq.Budget.MaxItems = 1
	cut := run(t, &fakeRetriever{set: contradictingSet(a, b)}, cutReq)
	if l := lineOf(t, cut.Response, "A1"); !strings.Contains(l, "conflicts_with=`B1`") {
		t.Errorf("the survivor must name the row stage 5 withheld: %q", l)
	}
}

// TestContradictsMarkerSurvivesResponseFit: the response-fit pass drops a row
// after stage 5, and the surviving winner keeps naming the side stage 5
// withheld — the marker is rebuilt against the final item list.
func TestContradictsMarkerSurvivesResponseFit(t *testing.T) {
	a := candidate("A1", "proj", "fact", "database configuration A1", 0.9)
	b := candidate("B1", "proj", "fact", "database configuration B1", 0.8)
	c := candidate("C1", "proj", "fact", "database configuration C1 an unrelated longer line", 0.7)
	set := setOf(a, b, c)
	set.Edges = []memory.LinkEdge{{From: "A1", To: "B1", Relation: "contradicts", Strength: 1}}
	set.EdgesStatus = memory.EdgeStatus{Status: "ok"}

	req := baseRequest()
	req.Budget.MaxItems = 10
	full := run(t, &fakeRetriever{set: set}, req)
	if got := itemIDs(full.Items); !eq(got, []string{"A1", "C1"}) {
		t.Fatalf("precondition: the pair separates and leaves A1 and C1, got %v", got)
	}
	if la := lineOf(t, full.Response, "A1"); !strings.Contains(la, "conflicts_with=`B1`") {
		t.Fatalf("precondition: A1 must name the withheld B1: %q", la)
	}

	req.Budget.MaxBytes = len(full.Response) - 1
	tight := run(t, &fakeRetriever{set: set}, req)
	if got := itemIDs(tight.Items); !eq(got, []string{"A1"}) {
		t.Fatalf("precondition: the cap must drop the lowest row C1, got %v", got)
	}
	if la := lineOf(t, tight.Response, "A1"); !strings.Contains(la, "conflicts_with=`B1`") {
		t.Errorf("the surviving winner stopped naming the withheld B1: %q", la)
	}
}

// TestNoContradictionLeavesLinesUnchanged: other relations and no edges mark nothing.
func TestNoContradictionLeavesLinesUnchanged(t *testing.T) {
	a := candidate("A1", "proj", "fact", "one", 0.9)
	b := candidate("B1", "proj", "fact", "two", 0.8)
	set := setOf(a, b)
	set.Edges = []memory.LinkEdge{{From: "A1", To: "B1", Relation: "supersedes", Strength: 1}}
	set.EdgesStatus = memory.EdgeStatus{Status: "ok"}
	req := baseRequest()
	req.Budget.MaxItems = 10
	res := run(t, &fakeRetriever{set: set}, req)
	plain := run(t, &fakeRetriever{set: setOf(a, b)}, req)
	if strings.Contains(res.Response, "conflicts_with") {
		t.Errorf("a supersedes edge was marked as a conflict:\n%s", res.Response)
	}
	for _, id := range []string{"A1", "B1"} {
		if got, want := lineOf(t, res.Response, id), lineOf(t, plain.Response, id); got != want {
			t.Errorf("%s changed: %q vs %q", id, got, want)
		}
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

// TestScopeConflictingPairIsNeitherMarkedNorNoted: production and development
// rows are two true claims about two places, so a contradicts edge between them
// is the one every other reader already ignores (memory.ScopesConflict).
func TestScopeConflictingPairIsNeitherMarkedNorNoted(t *testing.T) {
	a := candidate("A1", "proj", "fact", "the database is postgres", 0.9)
	a.Scope = map[string]string{"environment": "production"}
	b := candidate("B1", "proj", "fact", "the database is sqlite", 0.8)
	b.Scope = map[string]string{"environment": "development"}
	req := baseRequest()
	req.Budget.MaxItems = 10
	res := run(t, &fakeRetriever{set: contradictingSet(a, b)}, req)
	if got := itemIDs(res.Items); !eq(got, []string{"A1", "B1"}) {
		t.Fatalf("precondition: both rows rendered, got %v", got)
	}
	if strings.Contains(res.Response, "conflicts_with") {
		t.Errorf("a scope-conflicting pair is marked:\n%s", res.Response)
	}
	if hasNote(res.Notes, "contradicts pair recorded") {
		t.Errorf("a scope-conflicting pair is noted: %v", res.Notes)
	}

	// Compatible scopes (one silent) still conflict.
	b.Scope = nil
	res = run(t, &fakeRetriever{set: contradictingSet(a, b)}, req)
	if !strings.Contains(lineOf(t, res.Response, "A1"), "conflicts_with=`B1`") {
		t.Errorf("a scoped row against an unscoped one must still be marked:\n%s", res.Response)
	}
}
