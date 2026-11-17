package assemble

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// passiveRows lays out a passive page in the order the retriever hands it over,
// with the row named "old" naming "new" as the row that replaced it. The second
// form chains two replacements, oldest last in relevance order.
func passiveRows(ids ...string) []memory.Candidate {
	rows := make([]memory.Candidate, len(ids))
	for i, id := range ids {
		c := projectCandidate(id, 0.9-0.1*float64(i))
		c.FetchedBy = "proj"
		rows[i] = c
	}
	for i := range rows {
		switch rows[i].ID {
		case "old":
			rows[i].MaybeReplacedBy = []string{"new"}
		case "mid":
			rows[i].MaybeReplacedBy = []string{"new"}
		case "oldest":
			rows[i].MaybeReplacedBy = []string{"mid"}
		}
	}
	return rows
}

// TestRunPassiveShowsAReplacementPairInTimeOrder is the repeated-tuning defect:
// a session-start block that puts the newer row first makes a reader take it for
// the change a question asks "before", so the answer comes back one version too
// old. Showing the older row first, still marked as maybe replaced, leaves the
// reader a timeline from which the newest recorded value is the last one.
func TestRunPassiveShowsAReplacementPairInTimeOrder(t *testing.T) {
	res := run(t, &fakeRetriever{set: passiveSet(passiveRows("new", "old")...)}, passiveRequest())
	if got := itemIDs(res.Items); !eq(got, []string{"old", "new"}) {
		t.Errorf("answer %v, want [old new]: the replaced row comes first in time order", got)
	}
	old := itemByID(t, res.Items, "old")
	if len(old.MaybeReplacedBy) != 1 || old.MaybeReplacedBy[0] != "new" {
		t.Errorf("older row MaybeReplacedBy = %v, want [new]", old.MaybeReplacedBy)
	}
	if !strings.Contains(old.Line(), MaybeReplacedMarker) {
		t.Errorf("older row lost its marker: %s", old.Line())
	}
	if newer := itemByID(t, res.Items, "new"); strings.Contains(newer.Line(), MaybeReplacedMarker) {
		t.Errorf("newer row is marked as replaced: %s", newer.Line())
	}
}

// TestRunPassiveShowsAReplacementChainOldestFirst: the same rule across a chain
// (oldest replaced by mid, mid replaced by new) keeps each link in time order,
// so the chain reads as a timeline rather than the reverse of one.
func TestRunPassiveShowsAReplacementChainOldestFirst(t *testing.T) {
	res := run(t, &fakeRetriever{set: passiveSet(passiveRows("new", "mid", "oldest")...)}, passiveRequest())
	if got := itemIDs(res.Items); !eq(got, []string{"oldest", "mid", "new"}) {
		t.Errorf("answer %v, want [oldest mid new] in time order", got)
	}
	for _, id := range []string{"oldest", "mid"} {
		if it := itemByID(t, res.Items, id); !strings.Contains(it.Line(), MaybeReplacedMarker) {
			t.Errorf("%s lost its marker: %s", id, it.Line())
		}
	}
	if it := itemByID(t, res.Items, "new"); strings.Contains(it.Line(), MaybeReplacedMarker) {
		t.Errorf("the newest row is marked as replaced: %s", it.Line())
	}
}
