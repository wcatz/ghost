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

	// Framing alone over the cap: cutting rows cannot help, so they are kept.
	req = build()
	req.Budget.MaxBytes = 10
	req.Budget.FramingCeiling = 40
	req.Budget.Measure = func(items []Item, _ *Trace) int { return 50 + len(items) }
	res = run(t, mk(), req)
	if len(res.Items) != len(rows) {
		t.Errorf("kept %d rows, want all %d: the framing alone is over the cap", len(res.Items), len(rows))
	}

	// Framing over the cap but under the ceiling: cutting still helps, so rows go.
	req = build()
	req.Budget.MaxBytes = 10
	req.Budget.FramingCeiling = 100
	req.Budget.Measure = func(items []Item, _ *Trace) int { return 20 + len(items)*5 }
	res = run(t, mk(), req)
	if len(res.Items) != 0 {
		t.Errorf("kept %d rows, want 0: the framing (20) is over the cap (10) but under the ceiling (100), so cutting runs to none", len(res.Items))
	}

	// Only pinned rows left: the last resort cuts a pinned row from the bottom.
	req = build()
	req.Budget.MaxBytes = 250
	req.Budget.KeepPinned = true
	req.Budget.Measure = func(items []Item, _ *Trace) int {
		n := 0
		for _, it := range items {
			n += len(it.Content)
		}
		return n
	}
	var pinnedOnly []memory.Candidate
	for i, id := range []string{"P1", "P2", "P3", "P4"} {
		c := ranked(id, i, strings.Repeat("x", 100))
		c.Pinned = true
		pinnedOnly = append(pinnedOnly, c)
	}
	res = run(t, &fakeRetriever{set: ftsOnlySet(pinnedOnly...)}, req)
	if got := strings.Join(itemIDs(res.Items), ","); got != "P1,P2" {
		t.Errorf("kept %q, want P1,P2: with only pinned rows left the bottom one goes", got)
	}
}

// TestTheFramingCeilingIsDecidedFromTheAllCutFraming: a caller's framing that
// grows with the cuts (a count line that names how many rows were cut) can sit
// just under the ceiling with no cut and over it after the first. Reading the
// framing on every iteration cut rows for that and then stopped, leaving the
// render over the cap with rows recorded as cut for nothing. The decision is
// made ONCE, against the framing as it stands with every row cut, so such a block
// keeps every row.
func TestTheFramingCeilingIsDecidedFromTheAllCutFraming(t *testing.T) {
	rows := []string{"A1", "A2", "A3", "A4"}
	var cands []memory.Candidate
	for i, id := range rows {
		cands = append(cands, ranked(id, i, strings.Repeat("x", 100)))
	}
	req := baseRequest()
	req.Budget.MaxItems = 10
	req.Budget.MaxBytes = 9000
	req.Budget.FramingCeiling = 10000
	// 9,900 with no cut and 10,100 with one: the clause a cut earns is what
	// crosses the ceiling, and the block is over the 9,000 cap either way.
	mostCuts := 0
	req.Budget.Measure = func(items []Item, trace *Trace) int {
		cuts := 0
		for _, d := range trace.Decisions {
			if d.Stage == stageResponseFit && !d.Kept {
				cuts++
			}
		}
		if cuts > mostCuts {
			mostCuts = cuts
		}
		if cuts == 0 {
			return 9900 + 10*len(items)
		}
		return 10100 + 10*len(items)
	}
	res := run(t, &fakeRetriever{set: ftsOnlySet(cands...)}, req)

	if len(res.Items) != len(rows) {
		t.Errorf("kept %d rows, want %d: the framing cannot reach the cap, so no row is cut", len(res.Items), len(rows))
	}
	// The measure it decided against, not merely that it decided once: a version
	// that read the framing with only the cuts made so far would pass this on the
	// first row and stop on the second.
	if mostCuts != len(rows) {
		t.Errorf("the ceiling was decided against a trace holding %d cut rows, want %d: the framing is measured "+
			"at its longest, with every row cut", mostCuts, len(rows))
	}
	for _, d := range res.Trace.Decisions {
		if d.Stage == stageResponseFit && !d.Kept {
			t.Errorf("row %s recorded as cut for nothing", d.ID)
		}
	}
}

// TestTheFramingCeilingStillCutsWhenTheFramingCannotReachIt is the other half of
// the once-only decision: a caller whose framing does NOT grow with the cuts, and
// cannot reach the ceiling, still loses rows down to the cap. A decision made
// once is not a decision never to cut.
func TestTheFramingCeilingStillCutsWhenTheFramingCannotReachIt(t *testing.T) {
	rows := []string{"A1", "A2", "A3", "A4"}
	var cands []memory.Candidate
	for i, id := range rows {
		cands = append(cands, ranked(id, i, strings.Repeat("x", 100)))
	}
	req := baseRequest()
	req.Budget.MaxItems = 10
	req.Budget.MaxBytes = 9000
	req.Budget.FramingCeiling = 10000
	// 8,000 plus 500 a row: two of the four rows have to go, and with every row
	// cut the framing is still under the ceiling.
	req.Budget.Measure = func(items []Item, _ *Trace) int { return 8000 + 500*len(items) }
	res := run(t, &fakeRetriever{set: ftsOnlySet(cands...)}, req)

	if len(res.Items) != 2 {
		t.Fatalf("kept %d rows, want 2: the framing (8,000) is under the ceiling and the rows reach the cap", len(res.Items))
	}
	cut := 0
	for _, d := range res.Trace.Decisions {
		if d.Stage == stageResponseFit && !d.Kept {
			cut++
		}
	}
	if cut != len(rows)-2 {
		t.Errorf("response_fit cuts = %d, want %d", cut, len(rows)-2)
	}
}

// TestKeepPinnedCutsUnpinnedRowsFirst: with KeepPinned the cut order is the
// lowest-ranked unpinned row each time, so a pinned row ranked above unpinned
// ones outlives them and is the last to go.
func TestKeepPinnedCutsUnpinnedRowsFirst(t *testing.T) {
	var cands []memory.Candidate
	for i, id := range []string{"U1", "P1", "U2", "U3"} {
		c := ranked(id, i, strings.Repeat("x", 100))
		c.Pinned = id == "P1"
		cands = append(cands, c)
	}
	req := baseRequest()
	req.Budget.MaxItems = 10
	req.Budget.KeepPinned = true
	req.Budget.MaxBytes = 100
	req.Budget.Measure = func(items []Item, _ *Trace) int { return 100 * len(items) }
	res := run(t, &fakeRetriever{set: ftsOnlySet(cands...)}, req)
	if got := strings.Join(itemIDs(res.Items), ","); got != "P1" {
		t.Fatalf("kept %q, want only the pinned row", got)
	}
	var order []string
	for _, d := range res.Trace.Decisions {
		if d.Stage == stageResponseFit && !d.Kept {
			order = append(order, d.ID)
		}
	}
	if got := strings.Join(order, ","); got != "U3,U2,U1" {
		t.Errorf("cut order %q, want U3,U2,U1: lowest-ranked unpinned first", got)
	}
}
