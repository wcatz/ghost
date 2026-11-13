package assemble

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// A pin is a promise of delivery, so a pinned row that newer rows contradict is
// kept and marked, never withheld: its line names the newer rows in
// contradicted_by= beside the conflicts_with= that already names every withheld
// partner.

func pinnedStamped(id string, score float64, updatedAt string) memory.Candidate {
	c := stamped(id, score, updatedAt)
	c.Pinned = true
	return c
}

func contradicting(edges [][2]string, rows ...memory.Candidate) *memory.CandidateSet {
	set := setOf(rows...)
	for _, e := range edges {
		set.Edges = append(set.Edges, memory.LinkEdge{From: e[0], To: e[1], Relation: "contradicts", Strength: 1})
	}
	set.EdgesStatus = memory.EdgeStatus{Status: "ok"}
	return set
}

func TestPinnedRowContradictedByNewerRowIsKeptAndMarked(t *testing.T) {
	for _, edge := range [][2]string{{"NEW", "PIN"}, {"PIN", "NEW"}} {
		pin := pinnedStamped("PIN", 0.5, "2026-01-01 00:00:00")
		newer := stamped("NEW", 0.9, "2026-06-01 00:00:00")
		res := run(t, &fakeRetriever{set: contradicting([][2]string{edge}, newer, pin)}, separationRequest())

		assertSeparated(t, res, "PIN", "NEW")
		line := lineOf(t, res.Response, "PIN")
		if !strings.Contains(line, "contradicted_by=`NEW`") {
			t.Errorf("edge %v: the pinned line does not name the newer row: %q", edge, line)
		}
		if !strings.Contains(line, "conflicts_with=`NEW`") {
			t.Errorf("edge %v: conflicts_with= was displaced: %q", edge, line)
		}
	}
}

func TestPinnedRowIsNeverWithheldByAnotherPinnedRow(t *testing.T) {
	older := pinnedStamped("OLD", 0.9, "2026-01-01 00:00:00")
	newer := pinnedStamped("NEW", 0.5, "2026-06-01 00:00:00")
	res := run(t, &fakeRetriever{set: contradicting([][2]string{{"NEW", "OLD"}}, older, newer)}, separationRequest())

	if got := itemIDs(res.Items); !eq(got, []string{"OLD", "NEW"}) {
		t.Fatalf("items = %v, want both pinned rows delivered", got)
	}
	if line := lineOf(t, res.Response, "OLD"); !strings.Contains(line, "contradicted_by=`NEW`") {
		t.Errorf("the older pinned row does not name the newer one: %q", line)
	}
	if line := lineOf(t, res.Response, "NEW"); strings.Contains(line, "contradicted_by=") {
		t.Errorf("the newer pinned row is marked contradicted: %q", line)
	}
	for _, d := range res.Trace.Decisions {
		if !d.Kept {
			t.Errorf("a pinned row was dropped: %+v", d)
		}
	}
}

func TestPinnedMarkerSkipsAScopeConflictingEdge(t *testing.T) {
	pin := pinnedStamped("PIN", 0.5, "2026-01-01 00:00:00")
	pin.Scope = map[string]string{"environment": "production"}
	dev := stamped("DEV", 0.9, "2026-06-01 00:00:00")
	dev.Scope = map[string]string{"environment": "development"}
	res := run(t, &fakeRetriever{set: contradicting([][2]string{{"DEV", "PIN"}}, dev, pin)}, separationRequest())

	if got := itemIDs(res.Items); len(got) != 2 {
		t.Fatalf("items = %v, want both rows (the edge is exempt)", got)
	}
	if line := lineOf(t, res.Response, "PIN"); strings.Contains(line, "contradicted_by=") {
		t.Errorf("a scope-conflicting edge marked the pinned row: %q", line)
	}
}

// The marker names a kept pinned partner only while that partner is still in the
// answer: the response-fit pass can cut it after stage 5, and the surviving row
// must not point at a row the reader no longer has.
func TestPinnedMarkerDropsAKeptPartnerTheResponseFitCut(t *testing.T) {
	older := pinnedStamped("OLD", 0.9, "2026-01-01 00:00:00")
	newer := pinnedStamped("NEW", 0.5, "2026-06-01 00:00:00")
	// NEW is the row the cap cuts and is deliberately LARGE: the trim's cut notice is
	// counted against the cap.
	newer.Content = "database configuration NEW " + strings.Repeat("newer detail ", 20) + "NEW"
	set := contradicting([][2]string{{"NEW", "OLD"}}, older, newer)
	req := separationRequest()
	full := run(t, &fakeRetriever{set: set}, req)
	if got := itemIDs(full.Items); !eq(got, []string{"OLD", "NEW"}) {
		t.Fatalf("precondition: both pinned rows are delivered, got %v", got)
	}
	if line := lineOf(t, full.Response, "OLD"); !strings.Contains(line, "contradicted_by=`NEW`") {
		t.Fatalf("precondition: OLD names NEW while NEW is in the answer: %q", line)
	}

	req.Budget.MaxBytes = len(full.Response) - 1
	tight := run(t, &fakeRetriever{set: set}, req)
	if got := itemIDs(tight.Items); !eq(got, []string{"OLD"}) {
		t.Fatalf("precondition: the cap must cut the lowest row NEW, got %v", got)
	}
	if line := lineOf(t, tight.Response, "OLD"); strings.Contains(line, "contradicted_by=") {
		t.Errorf("OLD still names the cut NEW: %q", line)
	}
}
