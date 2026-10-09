package assemble

import (
	"github.com/wcatz/ghost/internal/memory"
	"strings"
	"testing"
)

// TestMeasureCapsTheCallersOwnRenderAndKeepsPinned: with Budget.Measure the cap is
// on the caller's render, not the search envelope, a cut is recorded as
// response_fit, and with KeepPinned a pinned row ranked LAST outlives unpinned
// rows ranked above it.
func TestMeasureCapsTheCallersOwnRenderAndKeepsPinned(t *testing.T) {
	rows := []string{"A1", "A2", "A3", "PIN"}
	build := func() Request {
		req := baseRequest()
		req.Budget.MaxItems = 10
		return req
	}
	mk := func() *fakeRetriever {
		var cands = make([]memory.Candidate, 0, len(rows))
		for i, id := range rows {
			c := ranked(id, i, strings.Repeat("x", 100))
			c.Pinned = id == "PIN"
			cands = append(cands, c)
		}
		return &fakeRetriever{set: ftsOnlySet(cands...)}
	}

	// The measure counts only content bytes: 100 per row. Cap 250 keeps two rows.
	req := build()
	req.Budget.MaxBytes = 250
	req.Budget.KeepPinned = true
	req.Budget.Measure = func(items []Item, _ *Trace) int {
		n := 0
		for _, it := range items {
			n += len(it.Content)
		}
		return n
	}
	res := run(t, mk(), req)

	got := strings.Join(itemIDs(res.Items), ",")
	if got != "A1,PIN" && got != "PIN,A1" {
		t.Fatalf("kept %q, want the top unpinned row and the pinned one", got)
	}
	cut := 0
	for _, d := range res.Trace.Decisions {
		if d.Stage == stageResponseFit && !d.Kept {
			cut++
			if d.Reason == "" || d.ID == "PIN" {
				t.Errorf("bad cut decision %+v", d)
			}
		}
	}
	if cut != 2 {
		t.Errorf("response_fit cuts = %d, want 2", cut)
	}
	if tally := CountsFor(res.Trace, "proj", len(res.Items)); tally.ByteCut != 2 || tally.RankedOut != 2 {
		t.Errorf("tally ByteCut=%d RankedOut=%d, want 2 and 2", tally.ByteCut, tally.RankedOut)
	}

	// Framing alone over the cap: every row goes and Run still answers.
	req = build()
	req.Budget.MaxBytes = 10
	req.Budget.Measure = func([]Item, *Trace) int { return 50 }
	res = run(t, mk(), req)
	if len(res.Items) != 0 {
		t.Errorf("kept %d rows with the framing alone over the cap", len(res.Items))
	}
}
