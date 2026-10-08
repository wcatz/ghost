package assemble

import (
	"strings"
	"testing"
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
