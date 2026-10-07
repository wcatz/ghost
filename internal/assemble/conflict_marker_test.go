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

// TestContradictsPairIsMarkedOnBothLines: both rows stay, neither is reordered,
// and each line names the other by the id it renders.
func TestContradictsPairIsMarkedOnBothLines(t *testing.T) {
	a := candidate("A1", "proj", "fact", "the database is postgres", 0.9)
	b := candidate("B1", "proj", "fact", "the database is mysql", 0.8)
	req := baseRequest()
	req.Budget.MaxItems = 10
	res := run(t, &fakeRetriever{set: contradictingSet(a, b)}, req)
	if got := itemIDs(res.Items); !eq(got, []string{"A1", "B1"}) {
		t.Fatalf("both rows must stay, in rank order: %v", got)
	}
	la, lb := lineOf(t, res.Response, "A1"), lineOf(t, res.Response, "B1")
	if !strings.Contains(la, "conflicts_with=`B1`") {
		t.Errorf("A1 does not say it conflicts with B1: %q", la)
	}
	if !strings.Contains(lb, "conflicts_with=`A1`") {
		t.Errorf("B1 does not say it conflicts with A1: %q", lb)
	}
}

// TestContradictsMarkerIsOrderIndependent: an edge may be stored either way round.
func TestContradictsMarkerIsOrderIndependent(t *testing.T) {
	a := candidate("A1", "proj", "fact", "one", 0.9)
	b := candidate("B1", "proj", "fact", "two", 0.8)
	set := contradictingSet(a, b)
	set.Edges[0].From, set.Edges[0].To = "B1", "A1"
	req := baseRequest()
	req.Budget.MaxItems = 10
	res := run(t, &fakeRetriever{set: set}, req)
	if !strings.Contains(lineOf(t, res.Response, "A1"), "conflicts_with=`B1`") ||
		!strings.Contains(lineOf(t, res.Response, "B1"), "conflicts_with=`A1`") {
		t.Errorf("a reversed edge must mark both lines:\n%s", res.Response)
	}
}

// TestContradictsMarkerNamesEveryPartner: one row in two pairs names both.
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
	if la := lineOf(t, res.Response, "A1"); !strings.Contains(la, "conflicts_with=`B1`,`C1`") {
		t.Errorf("A1 must name both partners: %q", la)
	}
	if lc := lineOf(t, res.Response, "C1"); strings.Contains(lc, "`B1`") {
		t.Errorf("C1 does not conflict with B1: %q", lc)
	}
}

// TestContradictsMarkerNeedsBothSidesRendered: an expired endpoint is withheld,
// a budget-cut endpoint is not rendered; either way the survivor says nothing.
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
		t.Errorf("a pair with one side withheld is marked: %q", l)
	}

	a := candidate("A1", "proj", "fact", "one", 0.9)
	b := candidate("B1", "proj", "fact", "two", 0.8)
	cutReq := baseRequest()
	cutReq.Budget.MaxItems = 1
	cut := run(t, &fakeRetriever{set: contradictingSet(a, b)}, cutReq)
	if strings.Contains(cut.Response, "conflicts_with") {
		t.Errorf("a pair cut by the budget is marked:\n%s", cut.Response)
	}
}

// TestContradictsMarkerSurvivesResponseFit: the fit pass drops the lowest row
// when the byte cap bites, and the survivor must not keep naming it.
func TestContradictsMarkerSurvivesResponseFit(t *testing.T) {
	a := candidate("A1", "proj", "fact", "one", 0.9)
	b := candidate("B1", "proj", "fact", "two", 0.8)
	req := baseRequest()
	req.Budget.MaxItems = 10
	full := run(t, &fakeRetriever{set: contradictingSet(a, b)}, req)
	la := lineOf(t, full.Response, "A1")

	req.Budget.MaxBytes = len(full.Response) - 1
	tight := run(t, &fakeRetriever{set: contradictingSet(a, b)}, req)
	if len(tight.Items) != 1 {
		t.Skipf("precondition: the cap did not drop a row (%d bytes, line %d)", len(tight.Response), len(la))
	}
	if strings.Contains(tight.Response, "conflicts_with") {
		t.Errorf("the surviving row still names a dropped partner:\n%s", tight.Response)
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
