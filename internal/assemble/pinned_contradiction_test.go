package assemble

import (
	"fmt"
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

func TestPinnedRowContradictedByOlderRowIsNotMarkedContradicted(t *testing.T) {
	pin := pinnedStamped("PIN", 0.5, "2026-06-01 00:00:00")
	older := stamped("OLD", 0.9, "2026-01-01 00:00:00")
	res := run(t, &fakeRetriever{set: contradicting([][2]string{{"OLD", "PIN"}}, older, pin)}, separationRequest())

	assertSeparated(t, res, "PIN", "OLD")
	if line := lineOf(t, res.Response, "PIN"); strings.Contains(line, "contradicted_by=") {
		t.Errorf("an older contradicting row marked the pinned row: %q", line)
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

func TestPinnedMarkerListsAPairStoredBothWaysOnce(t *testing.T) {
	pin := pinnedStamped("PIN", 0.5, "2026-01-01 00:00:00")
	newer := stamped("NEW", 0.9, "2026-06-01 00:00:00")
	res := run(t, &fakeRetriever{set: contradicting([][2]string{{"NEW", "PIN"}, {"PIN", "NEW"}}, newer, pin)}, separationRequest())

	if line := lineOf(t, res.Response, "PIN"); strings.Count(line, "`NEW`") != 2 { // once per label
		t.Errorf("a pair stored both ways is named more than once per label: %q", line)
	}
}

func TestPinnedMarkerIsBounded(t *testing.T) {
	rows := []memory.Candidate{pinnedStamped("PIN", 0.5, "2026-01-01 00:00:00")}
	var edges [][2]string
	for i := 0; i < maxRenderedConflictPartners+2; i++ {
		id := fmt.Sprintf("N%02d", i)
		rows = append(rows, stamped(id, 0.9-float64(i)/100, "2026-06-01 00:00:00"))
		edges = append(edges, [2]string{id, "PIN"})
	}
	req := separationRequest()
	req.Budget.MaxItems = 20
	res := run(t, &fakeRetriever{set: contradicting(edges, rows...)}, req)

	line := lineOf(t, res.Response, "PIN")
	if got := strings.Count(line, "(+2 more)"); got != 2 { // conflicts_with and contradicted_by
		t.Errorf("both labels must bound their ids with (+2 more): %q", line)
	}
	if !strings.Contains(line, "contradicted_by=`N00`,") {
		t.Errorf("contradicted_by= missing or out of rank order: %q", line)
	}
}

func TestNoContradictionMarkerWithoutAPinnedContradiction(t *testing.T) {
	res := run(t, &fakeRetriever{set: contradictingSet(stamped("A1", 0.9, ""), stamped("B1", 0.8, ""))}, separationRequest())
	if strings.Contains(res.Response, "contradicted_by=") {
		t.Errorf("an unpinned pair carries the pinned marker:\n%s", res.Response)
	}
}
