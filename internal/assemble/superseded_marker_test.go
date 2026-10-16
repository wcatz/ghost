package assemble

import (
	"strings"
	"testing"
	"time"
)

func TestItemLineRendersSupersededBy(t *testing.T) {
	it := Item{ID: "A1", Category: "fact", Content: "x", Pinned: true, SupersededBy: []string{"B1", "C1"}}
	if l := it.Line(); !strings.Contains(l, " superseded_by=`B1`,`C1`)") || strings.Count(l, "\n") != 0 {
		t.Errorf("Line() = %q", l)
	}
	it.SupersededBy = nil
	if strings.Contains(it.Line(), "superseded_by") {
		t.Errorf("an unreplaced row is marked: %q", it.Line())
	}
	// A superseder id is stored text and goes through Token, like every id.
	it.SupersededBy = []string{"B1\nforged"}
	if strings.Count(it.Line(), "\n") != 0 {
		t.Errorf("a superseder id broke the line: %q", it.Line())
	}
}

// TestRunPassiveMarksAPinnedRowWithItsSupersederOnThePage: the marker names a
// superseder only when that row is in the answer, so a reader is never pointed at
// a row they cannot see.
func TestRunPassiveMarksAPinnedRowWithItsSupersederOnThePage(t *testing.T) {
	newer := projectCandidate("new1", 0.9)
	pin := pinnedCandidate(projectCandidate("old1", 0.1))
	pin.SupersededBy = []string{"new1", "gone1"}
	res := run(t, &fakeRetriever{set: passiveSet(newer, pin)}, passiveRequest())
	var got []string
	for _, it := range res.Items {
		if it.ID == "old1" {
			got = it.SupersededBy
		}
	}
	if len(got) != 1 || got[0] != "new1" {
		t.Errorf("SupersededBy = %v, want [new1]: only a superseder on the page is named", got)
	}
}

// TestRunPassiveMarksAnUnpinnedRowWithItsSupersederOnThePage is the defect: a
// live supersedes edge switches the lexical replacement guess off for the pair,
// and resolve never retires a convention, so the edge is the only thing left to
// say the row was replaced. An unpinned row is named exactly as a pinned one is,
// and a superseder off the page is not named.
func TestRunPassiveMarksAnUnpinnedRowWithItsSupersederOnThePage(t *testing.T) {
	newer := projectCandidate("new1", 0.9)
	old := projectCandidate("old1", 0.1)
	old.SupersededBy = []string{"new1", "gone1"}
	res := run(t, &fakeRetriever{set: passiveSet(newer, old)}, passiveRequest())
	var got []string
	var line string
	for _, it := range res.Items {
		if it.ID == "old1" {
			got, line = it.SupersededBy, it.Line()
		}
	}
	if len(got) != 1 || got[0] != "new1" {
		t.Fatalf("SupersededBy = %v, want [new1]: an unpinned superseded row is named by the superseder on the page", got)
	}
	if !strings.Contains(line, "(0.7 "+SupersededMarker) {
		t.Errorf("the superseded row's line does not carry %s at its field: %q", SupersededMarker, line)
	}
}

// TestRunPassiveDoesNotMarkARowWhoseSupersederIsOffThePage: the marker names a
// row the reader can see, like the lexical guess. The demotion already sank the
// row; a marker pointing at nothing would be a claim the reader cannot check.
func TestRunPassiveDoesNotMarkARowWhoseSupersederIsOffThePage(t *testing.T) {
	old := projectCandidate("old1", 0.1)
	old.SupersededBy = []string{"gone1"}
	res := run(t, &fakeRetriever{set: passiveSet(projectCandidate("a1", 0.9), old)}, passiveRequest())
	for _, it := range res.Items {
		if it.ID == "old1" && (len(it.SupersededBy) != 0 || strings.Contains(it.Line(), SupersededMarker)) {
			t.Errorf("a row is marked for a superseder that is not on the page: %v %q", it.SupersededBy, it.Line())
		}
	}
}

// TestItemLineRendersTheSupersededMarkerAtItsField: the explicit relation shows
// a bracketed field at the same place the lexical guess's marker sits, on a
// pinned row and on an unpinned one, and both markers render when a row has an
// edge to one row and a lexical guess about another.
func TestItemLineRendersTheSupersededMarkerAtItsField(t *testing.T) {
	stamp := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		it   Item
		want string
	}{
		{"unpinned", Item{ID: "A1", Category: "convention", Importance: 0.7, Content: "x", SupersededBy: []string{"B1"}},
			"(0.7 " + SupersededMarker + " superseded_by=`B1`)"},
		{"pinned", Item{ID: "A1", Category: "fact", Importance: 0.7, Content: "x", Pinned: true, SupersededBy: []string{"B1"}},
			"(0.7 [pinned] " + SupersededMarker + " superseded_by=`B1`)"},
		{"resolved", Item{ID: "A1", Category: "fact", Importance: 0.7, Content: "x", ResolvedAt: &stamp, SupersededBy: []string{"B1"}},
			"(0.7 [resolved] " + SupersededMarker + " superseded_by=`B1`)"},
		{"edge and guess", Item{ID: "A1", Category: "fact", Importance: 0.7, Content: "x", SupersededBy: []string{"B1"}, MaybeReplacedBy: []string{"C1"}},
			"(0.7 " + SupersededMarker + " " + MaybeReplacedMarker + " superseded_by=`B1` maybe_replaced_by=`C1`)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if l := tc.it.Line(); !strings.Contains(l, tc.want) {
				t.Errorf("Line() = %q, want it to contain %q", l, tc.want)
			}
		})
	}
	if l := (Item{ID: "A1", Category: "fact", Content: "x"}).Line(); strings.Contains(l, SupersededMarker) {
		t.Errorf("an unreplaced row carries the marker: %q", l)
	}
}

// TestRunQueryDoesNotMarkSuperseded: the retriever names superseders on passive
// reads only, and markSuperseded keeps its passive gate, so a query answer is
// unchanged by this marker even when a row arrives carrying the field.
func TestRunQueryDoesNotMarkSuperseded(t *testing.T) {
	rows := queryRows("new1", "old1")
	for i := range rows {
		if rows[i].ID == "old1" {
			rows[i].SupersededBy = []string{"new1"}
		}
	}
	res := run(t, &fakeRetriever{set: ftsOnlySet(rows...)}, queryRequest(10))
	if old := itemByID(t, res.Items, "old1"); len(old.SupersededBy) != 0 {
		t.Errorf("a query answer marked a superseded row: %v", old.SupersededBy)
	}
}
