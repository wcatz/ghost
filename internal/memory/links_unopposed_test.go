package memory

import (
	"context"
	"strings"
	"testing"
)

// The store half of #806: CreateLinkUnopposed keeps two concurrent
// `ghost supersede --apply` passes from writing both directions of one pair. The
// guard is the contract of a named method so a cycle stays writable by callers
// that describe one (bench seeders, repair fixtures). The two-process proof lives
// in internal/supersede (TestTwoApplyPassesCannotWriteACycle).

func supersedeHistoryRows(t *testing.T, s *Store, id string) int {
	t.Helper()
	entries, err := s.MemoryHistory(context.Background(), id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	n := 0
	for _, e := range entries {
		if e.Phase == "supersede" {
			n++
		}
	}
	return n
}

// TestCreateLinkUnopposedWritesWhenNothingOpposesIt: the method writes, reports
// it, files the same supersede history row CreateLink files, and keeps the
// JUDGED stamp rather than the write clock. Re-confirming the live edge in its
// own direction (#792) re-stamps it, reports a write, and files no second row.
func TestCreateLinkUnopposedWritesWhenNothingOpposesIt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	newer := makeMemory(t, s, "the restore path on one spindle is safe")
	older := makeMemory(t, s, "a restore across two spindles took 41 minutes")

	wrote, err := s.CreateLinkUnopposed(ctx, newer, older, "supersedes", 0.95, "llm", "2026-09-01 00:00:00")
	if err != nil || !wrote {
		t.Fatalf("CreateLinkUnopposed = %v, %v, want a write", wrote, err)
	}
	edges, err := s.SupersedesWithin(ctx, []string{newer, older})
	if err != nil || len(edges) != 1 || edges[0] != [2]string{newer, older} {
		t.Fatalf("live supersedes edges = %v, %v, want exactly [%s %s]", edges, err, newer, older)
	}
	if n := supersedeHistoryRows(t, s, older); n != 1 {
		t.Errorf("supersede history rows for the target = %d, want 1", n)
	}
	links, _ := s.GetLinks(ctx, newer)
	if len(links) != 1 || links[0].CreatedAt != "2026-09-01 00:00:00" {
		t.Errorf("link = %+v, want created_at = the judged stamp", links)
	}

	wrote, err = s.CreateLinkUnopposed(ctx, newer, older, "supersedes", 0.95, "llm", "2026-09-20 12:00:00")
	if err != nil || !wrote {
		t.Fatalf("re-confirm = %v, %v, want a write: the stamp is what skip-if-unchanged reads", wrote, err)
	}
	links, _ = s.GetLinks(ctx, newer)
	if len(links) != 1 || links[0].CreatedAt != "2026-09-20 12:00:00" {
		t.Errorf("link = %+v, want created_at moved to the re-confirming stamp", links)
	}
	if n := supersedeHistoryRows(t, s, older); n != 1 {
		t.Errorf("supersede history rows = %d after re-confirm, want still 1", n)
	}
}

// TestCreateLinkUnopposedRefusesTheOppositeDirection: the refusal is a normal
// outcome, (false, nil), the first writer's edge stays, and a refused write
// files no history row. An INVALIDATED opposite edge claims nothing, so the pair
// is writable again.
func TestCreateLinkUnopposedRefusesTheOppositeDirection(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	newer := makeMemory(t, s, "the relay rebalance stall is fixed")
	stale := makeMemory(t, s, "bug: the relay stalls on every consumer rebalance")

	if err := s.CreateLink(ctx, stale, newer, "supersedes", 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	wrote, err := s.CreateLinkUnopposed(ctx, newer, stale, "supersedes", 0.95, "llm", "")
	if err != nil || wrote {
		t.Fatalf("CreateLinkUnopposed = %v, %v, want a refusal reported as (false, nil)", wrote, err)
	}
	edges, err := s.SupersedesWithin(ctx, []string{newer, stale})
	if err != nil || len(edges) != 1 || edges[0] != [2]string{stale, newer} {
		t.Errorf("live supersedes edges = %v, %v, want exactly the first writer's", edges, err)
	}
	if n := supersedeHistoryRows(t, s, stale); n != 0 {
		t.Errorf("a refused write filed %d supersede history row(s)", n)
	}

	if _, err := s.InvalidateLink(ctx, stale, newer, "supersedes"); err != nil {
		t.Fatalf("InvalidateLink: %v", err)
	}
	if wrote, err := s.CreateLinkUnopposed(ctx, newer, stale, "supersedes", 0.95, "llm", ""); err != nil || !wrote {
		t.Errorf("an INVALIDATED opposite edge refused the write: %v, %v", wrote, err)
	}
}

// TestCreateLinkUnopposedRefusesASymmetricRelationByName: a symmetric relation
// has no reverse to be opposed by, so the method refuses it with an error.
func TestCreateLinkUnopposedRefusesASymmetricRelationByName(t *testing.T) {
	s := testStore(t)
	a := makeMemory(t, s, "alpha about linker behaviour")
	b := makeMemory(t, s, "beta about linker behaviour")
	wrote, err := s.CreateLinkUnopposed(context.Background(), a, b, "related", 0.8, "auto", "")
	if err == nil || wrote || !strings.Contains(err.Error(), "symmetric") {
		t.Fatalf("CreateLinkUnopposed(related) = %v, %v, want a refusal naming the relation as symmetric", wrote, err)
	}
}

// TestCreateLinkUnopposedGuardsTheCausesRelation: the guard covers 'causes' too
// (#823) and is scoped to THAT relation's reverse: an agreeing 'supersedes'
// edge is the same claim in the other convention and must not refuse a
// re-affirmation, or the pair would be re-billed every pass.
func TestCreateLinkUnopposedGuardsTheCausesRelation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	cause := makeMemory(t, s, "the restore path on one spindle is safe and fast")
	effect := makeMemory(t, s, "the restore is being rewritten to run on one spindle")

	if wrote, err := s.CreateLinkUnopposed(ctx, cause, effect, "causes", 0.9, "llm", "2026-09-01 00:00:00"); err != nil || !wrote {
		t.Fatalf("CreateLinkUnopposed(causes) = %v, %v, want a write", wrote, err)
	}
	if wrote, err := s.CreateLinkUnopposed(ctx, effect, cause, "causes", 0.9, "llm", ""); err != nil || wrote {
		t.Fatalf("reverse causes = %v, %v, want a refusal reported as (false, nil)", wrote, err)
	}
	if _, err := s.CreateLinkUnopposed(ctx, effect, cause, "supersedes", 0.9, "llm", "2026-09-01 00:00:00"); err != nil {
		t.Fatalf("CreateLinkUnopposed(supersedes): %v", err)
	}
	if wrote, err := s.CreateLinkUnopposed(ctx, cause, effect, "causes", 0.9, "llm", "2026-10-01 00:00:00"); err != nil || !wrote {
		t.Fatalf("re-affirm causes = %v, %v, want a write beside an agreeing supersedes edge", wrote, err)
	}
	links, err := s.GetLinks(ctx, cause)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	var found Link
	for _, l := range links {
		if l.Relation == "causes" {
			found = l
		}
	}
	if found.SourceID != cause || found.TargetID != effect || found.CreatedAt != "2026-10-01 00:00:00" {
		t.Errorf("causes link = %+v, want %s -> %s re-stamped", found, cause, effect)
	}
}
