package reflection

import (
	"fmt"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// decisionLinks is n distinct decision links joined the way a consolidation
// successor stores them, starting at from so two rows can carry disjoint sets.
func decisionLinks(from, n int) string {
	links := make([]string, 0, n)
	for i := from; i < from+n; i++ {
		links = append(links, fmt.Sprintf("decision:%032X", i))
	}
	return strings.Join(links, ",")
}

// A merge whose sources fold more decision links than one source_ref can hold is
// refused at the write with every source kept, so the accounting lists it in its
// own bucket with the link count and the bound, and not under Merges. The
// replace asks the overflow before the scopes, so a merge that would fail both
// is listed as an overflow and only as one; a merge under the bound is still a
// merge. A row the merge does not name whose text is already the merge's text is
// kept by the replace too (memory.EmissionClaimsRow), so the refusal line owns
// it even though the response dropped it.
func TestAccountInputsListsALinkOverflowAsTheApplyRefusesIt(t *testing.T) {
	const (
		overText = "the consumer and the retry path both use the measured option"
		okText   = "the queue drains on shutdown"
	)
	rows := []memory.Memory{
		{ID: "CAFE0000000000000000000000000001", Category: "fact", Content: "the consumer uses the measured option",
			SourceRef: decisionLinks(0, 7), Scope: map[string]string{"environment": "production"}},
		{ID: "CAFE0000000000000000000000000002", Category: "fact", Content: "the retry path uses the measured option",
			SourceRef: decisionLinks(7, 7), Scope: map[string]string{"environment": "staging"}},
		{ID: "CAFE0000000000000000000000000003", Category: "fact", Content: "the queue drains on shutdown, always",
			SourceRef: decisionLinks(14, 1)},
		{ID: "CAFE0000000000000000000000000004", Category: "fact", Content: "the queue drains before it stops",
			SourceRef: decisionLinks(15, 1)},
		{ID: "CAFE0000000000000000000000000005", Category: "fact", Content: overText},
	}
	input := ReflectionInput{ProjectName: "queue", ExistingMemories: rows}
	result := ReflectionResult{
		Memories: []ReflectMemory{
			{Category: "fact", Content: overText, Importance: 0.6},
			{Category: "fact", Content: okText, Importance: 0.6},
		},
		Merges: []Merge{
			{IDs: []string{rows[0].ID, rows[1].ID}, Text: overText},
			{IDs: []string{rows[2].ID, rows[3].ID}, Text: okText},
		},
		Drops: []Drop{{ID: rows[4].ID, Reason: "obsolete"}},
	}

	// The fixture has to be the case under test by the write's own rule, or the
	// assertions below are about a merge the apply would write.
	links, over := memory.DecisionLinksOverflow([]string{rows[0].SourceRef, rows[1].SourceRef})
	if !over || links != 14 {
		t.Fatalf("fixture: the first merge folds %d links, over=%v; want 14 and over the bound", links, over)
	}
	if _, err := memory.FoldSourceScopes([]memory.SourceScope{{ID: rows[0].ID, Scope: rows[0].Scope}, {ID: rows[1].ID, Scope: rows[1].Scope}}); !memory.IsScopeConflict(err) {
		t.Fatalf("fixture: the first merge's scopes agree (%v), so it does not test the order", err)
	}

	acc := AccountInputs(input, result, nil)
	if len(acc.LinkOverflows) != 1 {
		t.Fatalf("LinkOverflows = %+v, want the one overflowing merge", acc.LinkOverflows)
	}
	got := acc.LinkOverflows[0]
	if strings.Join(got.IDs, ",") != rows[0].ID+","+rows[1].ID || got.Links != 14 || got.Bound != memory.MaxSourceRefLen {
		t.Errorf("LinkOverflows[0] = %+v, want both sources, 14 links and the %d-byte bound", got, memory.MaxSourceRefLen)
	}
	if strings.Join(got.Carriers, ",") != rows[4].ID {
		t.Errorf("LinkOverflows[0].Carriers = %v, want the text twin %s the replace keeps", got.Carriers, rows[4].ID)
	}
	if len(acc.Drops) != 0 {
		t.Errorf("Drops = %+v: the replace keeps the twin when it drops the merge, so no drop line describes it", acc.Drops)
	}
	if len(acc.ScopeRefusals) != 0 {
		t.Errorf("ScopeRefusals = %+v: the replace drops the overflow before it folds the scopes, so the merge is listed once", acc.ScopeRefusals)
	}
	if len(acc.Merges) != 1 || strings.Join(acc.Merges[0].IDs, ",") != rows[2].ID+","+rows[3].ID {
		t.Errorf("Merges = %+v, want only the merge under the bound", acc.Merges)
	}
	if len(acc.Kept)+len(acc.Passed)+len(acc.Deleted) != 0 {
		t.Errorf("kept %v, passed %v, deleted %+v: every input is owned by a line", acc.Kept, acc.Passed, acc.Deleted)
	}
}
