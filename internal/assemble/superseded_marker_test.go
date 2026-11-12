package assemble

import (
	"strings"
	"testing"
)

// TestRunPassiveMarksAnUnpinnedRowWithItsSupersederOnThePage is the defect: a live
// supersedes edge switches the lexical replacement guess off for the pair, and
// resolve never retires a convention.
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

// queryIDsAndRanks is the order of a query answer and each row's explain rank,
// the two things the marker must not move.
func queryIDsAndRanks(t *testing.T, res Result) (ids []string, ranks map[string]int) {
	t.Helper()
	ids = itemIDs(res.Items)
	if res.Explain == nil {
		t.Fatal("no explain payload")
	}
	ranks = map[string]int{}
	for _, r := range res.Explain.Rows {
		ranks[r.ID] = r.Rank
	}
	return ids, ranks
}

// TestRunQueryMarksASupersededRowWithItsSupersederOnThePage is the defect: a live
// supersedes edge switches the lexical guess off for its pair on a query read too.
func TestRunQueryMarksASupersededRowWithItsSupersederOnThePage(t *testing.T) {
	req := queryRequest(10)
	req.Explain = true
	plain := run(t, &fakeRetriever{set: ftsOnlySet(queryRows("new1", "old1", "a1")...)}, req)

	rows := queryRows("new1", "old1", "a1")
	for i := range rows {
		if rows[i].ID == "old1" {
			rows[i].SupersededBy = []string{"new1", "gone1"}
		}
	}
	res := run(t, &fakeRetriever{set: ftsOnlySet(rows...)}, req)
	old := itemByID(t, res.Items, "old1")
	if len(old.SupersededBy) != 1 || old.SupersededBy[0] != "new1" {
		t.Fatalf("SupersededBy = %v, want [new1]: a query answer marks a row its superseder on the page replaced", old.SupersededBy)
	}
	if l := old.Line(); !strings.Contains(l, SupersededMarker) || !strings.Contains(l, "superseded_by=`new1`)") {
		t.Errorf("the superseded row's line does not carry the marker and superseded_by=: %q", l)
	}
	for _, it := range res.Items {
		if it.ID != "old1" && len(it.SupersededBy) != 0 {
			t.Errorf("%s is marked superseded: %v", it.ID, it.SupersededBy)
		}
	}
	wantIDs, wantRanks := queryIDsAndRanks(t, plain)
	gotIDs, gotRanks := queryIDsAndRanks(t, res)
	if strings.Join(gotIDs, ",") != strings.Join(wantIDs, ",") {
		t.Errorf("the marker moved the answer: %v, want %v", gotIDs, wantIDs)
	}
	for id, r := range wantRanks {
		if gotRanks[id] != r {
			t.Errorf("explain rank of %s = %d, want %d: rank stays the relevance rank", id, gotRanks[id], r)
		}
	}
	if !strings.Contains(res.Response, SupersededMarker) {
		t.Errorf("the rendered answer does not carry the marker:\n%s", res.Response)
	}
}

// TestRunQueryDoesNotMarkARowWhoseSupersederIsOffThePage: as on the session
// start, the marker points at a row the reader can see or at nothing.
func TestRunQueryDoesNotMarkARowWhoseSupersederIsOffThePage(t *testing.T) {
	rows := queryRows("a1", "old1")
	for i := range rows {
		if rows[i].ID == "old1" {
			rows[i].SupersededBy = []string{"gone1"}
		}
	}
	res := run(t, &fakeRetriever{set: ftsOnlySet(rows...)}, queryRequest(10))
	if old := itemByID(t, res.Items, "old1"); len(old.SupersededBy) != 0 || strings.Contains(old.Line(), SupersededMarker) {
		t.Errorf("a row is marked for a superseder that is not on the page: %v %q", old.SupersededBy, old.Line())
	}
}
