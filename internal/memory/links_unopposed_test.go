package memory

import (
	"context"
	"strings"
	"testing"
)

// This file is the store half of #806: `CreateLinkUnopposed` is the write that
// keeps two concurrent `ghost supersede --apply` passes from writing both
// directions of one pair, and the reason it is a SEPARATE method is that a cycle
// has to remain writable by the callers that describe one — the bench seeders and
// the repair pass's own fixtures. So the guard is a contract of a named method,
// and this file is what holds it to that contract.
//
// The two-process proof lives in internal/supersede
// (TestTwoApplyPassesCannotWriteACycle): what is tested here is the property that
// makes it hold, which is that the reverse edge is read INSIDE the transaction
// that writes this one. Everything a caller can observe about the refusal is
// below.

// TestCreateLinkUnopposedWritesWhenNothingOpposesIt: the ordinary case. The
// method has to write, report that it wrote, and file the same `supersede`
// history row `CreateLink` files — a guard that refused everything, or one that
// cost a write its audit row, would pass a test that only ever looked at cycles.
func TestCreateLinkUnopposedWritesWhenNothingOpposesIt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	newer := makeMemory(t, s, "the restore path on one spindle is safe")
	older := makeMemory(t, s, "a restore across two spindles took 41 minutes")

	wrote, err := s.CreateLinkUnopposed(ctx, newer, older, "supersedes", 0.95, "llm", "2026-09-01 00:00:00")
	if err != nil {
		t.Fatalf("CreateLinkUnopposed: %v", err)
	}
	if !wrote {
		t.Fatal("CreateLinkUnopposed reported no write for a pair nothing opposed")
	}
	edges, err := s.SupersedesWithin(ctx, []string{newer, older})
	if err != nil {
		t.Fatalf("SupersedesWithin: %v", err)
	}
	if len(edges) != 1 || edges[0] != [2]string{newer, older} {
		t.Fatalf("live supersedes edges = %v, want exactly [%s %s]", edges, newer, older)
	}
	// The audit row, because a supersedes edge with no record of it is a state
	// CreateLinkJudged's transaction exists to make impossible — and the guarded
	// writer shares that transaction rather than re-implementing it.
	entries, err := s.MemoryHistory(ctx, older, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Phase == "supersede" {
			found = true
		}
	}
	if !found {
		var phases []string
		for _, e := range entries {
			phases = append(phases, e.Phase)
		}
		t.Errorf("no supersede history row for the target: %v", phases)
	}
	// And the stamp is the JUDGED one, not the write clock: the whole reason
	// CreateLinkJudged exists is that these are a classify call apart.
	links, err := s.GetLinks(ctx, newer)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	if len(links) != 1 || links[0].CreatedAt != "2026-09-01 00:00:00" {
		t.Errorf("link = %+v, want created_at = the judged stamp 2026-09-01 00:00:00", links)
	}
}

// TestCreateLinkUnopposedRefusesTheOppositeDirection: the refusal itself, and it
// is a normal outcome rather than an error. The edge that exists is the earlier
// writer's, so the pass that lost the race has something to go on — and an error
// would abort the whole pass over a write that was declined on purpose.
func TestCreateLinkUnopposedRefusesTheOppositeDirection(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	newer := makeMemory(t, s, "the relay rebalance stall is fixed")
	stale := makeMemory(t, s, "bug: the relay stalls on every consumer rebalance")

	// The concurrent pass got there first, in the OPPOSITE direction.
	if err := s.CreateLink(ctx, stale, newer, "supersedes", 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	wrote, err := s.CreateLinkUnopposed(ctx, newer, stale, "supersedes", 0.95, "llm", "")
	if err != nil {
		t.Fatalf("CreateLinkUnopposed: %v, want a refusal reported as (false, nil)", err)
	}
	if wrote {
		t.Error("CreateLinkUnopposed wrote the second direction of a pair already claimed the other way: the two edges demote BOTH endpoints and neither withdraws the other")
	}
	edges, err := s.SupersedesWithin(ctx, []string{newer, stale})
	if err != nil {
		t.Fatalf("SupersedesWithin: %v", err)
	}
	if len(edges) != 1 || edges[0] != [2]string{stale, newer} {
		t.Errorf("live supersedes edges = %v, want exactly the first writer's [%s %s]", edges, stale, newer)
	}
	// No history row for a write that did not happen: a history row records a
	// write, and this is the endpoint the refused edge would have demoted. (The
	// first writer's own row is on `newer`, which is ITS target, so the two are
	// not confused by reading the wrong memory here.)
	entries, err := s.MemoryHistory(ctx, stale, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	for _, e := range entries {
		if e.Phase == "supersede" {
			t.Errorf("a refused write filed a supersede history row: %+v", e)
		}
	}
}

// TestCreateLinkUnopposedReStampsTheDirectionItAlreadyHolds: #792's path must be
// untouched. A live edge whose endpoints moved is re-judged in its OWN direction
// every pass, and the verdict moves the stamp the skip test reads — so a guard
// that treated "an edge for this pair is already live" as opposition would freeze
// every re-confirmation and re-bill the pair forever.
func TestCreateLinkUnopposedReStampsTheDirectionItAlreadyHolds(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	newer := makeMemory(t, s, "the ingest service now runs Redis 7.2")
	older := makeMemory(t, s, "the ingest service runs Redis 6.2")

	if _, err := s.CreateLinkUnopposed(ctx, newer, older, "supersedes", 0.95, "llm", "2026-06-01 00:00:00"); err != nil {
		t.Fatalf("CreateLinkUnopposed: %v", err)
	}
	wrote, err := s.CreateLinkUnopposed(ctx, newer, older, "supersedes", 0.95, "llm", "2026-09-20 12:00:00")
	if err != nil {
		t.Fatalf("re-confirm: %v", err)
	}
	if !wrote {
		t.Error("re-confirming a live edge in its own direction reported no write: the edge's stamp is what skip-if-unchanged reads, so a pass that could not move it would re-bill the pair forever")
	}
	links, err := s.GetLinks(ctx, newer)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	if len(links) != 1 || links[0].CreatedAt != "2026-09-20 12:00:00" {
		t.Errorf("link = %+v, want created_at moved to the re-confirming verdict's stamp", links)
	}
	// Re-confirming a live edge files no second history row: the edge's validity
	// did not change, so there is no new state to record.
	entries, err := s.MemoryHistory(ctx, older, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	n := 0
	for _, e := range entries {
		if e.Phase == "supersede" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("supersede history rows = %d, want 1: re-writing a live edge changes nothing about either memory", n)
	}
}

// TestCreateLinkUnopposedIgnoresAnInvalidatedOpposite: the guard reads LIVE
// edges, so withdrawing the opposite direction frees the pair again. A guard that
// looked at invalidated rows would refuse every write to a pair that ever held a
// cycle, which is exactly the pair an operator runs `--reassess` to unblock.
func TestCreateLinkUnopposedIgnoresAnInvalidatedOpposite(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	newer := makeMemory(t, s, "the cache expiry gap is fixed")
	stale := makeMemory(t, s, "bug: the cache never expires between restarts")

	if err := s.CreateLink(ctx, stale, newer, "supersedes", 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	if _, err := s.InvalidateLink(ctx, stale, newer, "supersedes"); err != nil {
		t.Fatalf("InvalidateLink: %v", err)
	}
	wrote, err := s.CreateLinkUnopposed(ctx, newer, stale, "supersedes", 0.95, "llm", "")
	if err != nil {
		t.Fatalf("CreateLinkUnopposed: %v", err)
	}
	if !wrote {
		t.Error("an INVALIDATED opposite edge refused the write: a withdrawn edge claims nothing, and the pair an operator just repaired has to be writable again")
	}
}

// TestCreateLinkUnopposedRefusesASymmetricRelationByName: the guard is about a
// directed claim, and a symmetric relation has no reverse to be opposed by — it
// is stored in normalized order, so its two directions are one row. Answering
// anyway would be a guess about a question the relation cannot raise.
func TestCreateLinkUnopposedRefusesASymmetricRelationByName(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := makeMemory(t, s, "alpha about linker behaviour")
	b := makeMemory(t, s, "beta about linker behaviour")

	wrote, err := s.CreateLinkUnopposed(ctx, a, b, "related", 0.8, "auto", "")
	if err == nil {
		t.Fatalf("CreateLinkUnopposed(related) = (%v, nil), want a refusal by name", wrote)
	}
	if wrote {
		t.Error("CreateLinkUnopposed reported a write alongside its error")
	}
	if !strings.Contains(err.Error(), "symmetric") {
		t.Errorf("error = %v, want it to name the relation as symmetric", err)
	}
}

// TestCreateLinkUnopposedGuardsTheCausesRelation: the guard is about a PAIR, not
// about a relation, and 'causes' reached it in #823.
//
// The pass only ever reached the guarded writer for 'supersedes' until then, and
// that asymmetry was a decision rather than an oversight — with no direction
// override for the second relation, a pair whose 'causes' edge ran against the
// timestamps was re-proposed flipped on every pass, so a guard would have refused
// it on every pass, forever. The override now covers both relations, so a 'causes'
// write is in the live edge's own direction and the refusal is a one-off race
// again. This test holds the store half of that: the method guards whatever
// relation it is handed, and it is scoped to THAT relation's reverse rather than
// to the pair's any edge.
//
// The last case is the scope, and it is why the guard is not simply "is the pair
// linked in the other direction": a 'causes' edge and a 'supersedes' edge that
// agree about the pair are the same claim in two conventions, and the pass's own
// verdict is what reconciles them. A guard that refused a 'causes' write beside
// an agreeing 'supersedes' edge would refuse every re-affirmation on such a pair
// and re-bill it forever.
func TestCreateLinkUnopposedGuardsTheCausesRelation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	cause := makeMemory(t, s, "the restore path on one spindle is safe and fast")
	effect := makeMemory(t, s, "the restore is being rewritten to run on one spindle")

	// Nothing opposed: the guard writes.
	wrote, err := s.CreateLinkUnopposed(ctx, cause, effect, "causes", 0.9, "llm", "2026-09-01 00:00:00")
	if err != nil {
		t.Fatalf("CreateLinkUnopposed(causes): %v", err)
	}
	if !wrote {
		t.Fatal("CreateLinkUnopposed reported no write for a 'causes' pair nothing opposed")
	}
	if links, err := s.GetLinks(ctx, cause); err != nil || len(links) != 1 || links[0].TargetID != effect {
		t.Fatalf("GetLinks(cause) = %+v (err %v), want the one causes edge -> %s", links, err, effect)
	}

	// The opposite direction, live: refused, and it is (false, nil) rather than an
	// error, because nothing went wrong and the graph is left in a state the next
	// pass can act on.
	wrote, err = s.CreateLinkUnopposed(ctx, effect, cause, "causes", 0.9, "llm", "")
	if err != nil {
		t.Fatalf("CreateLinkUnopposed(reverse causes): %v, want a refusal reported as (false, nil)", err)
	}
	if wrote {
		t.Error("CreateLinkUnopposed wrote the second direction of a 'causes' pair already claimed the other way round")
	}
	if links, err := s.GetLinks(ctx, cause); err != nil || len(links) != 1 {
		t.Fatalf("GetLinks(cause) = %+v (err %v), want the FIRST writer's edge alone", links, err)
	}

	// An AGREEING 'supersedes' edge is not opposition. 'causes' runs older→newer
	// and 'supersedes' newer→older, so these two say the same thing about the
	// pair, and the pass's verdict is what reconciles them.
	if _, err := s.CreateLinkUnopposed(ctx, effect, cause, "supersedes", 0.9, "llm", "2026-09-01 00:00:00"); err != nil {
		t.Fatalf("CreateLinkUnopposed(supersedes): %v", err)
	}
	wrote, err = s.CreateLinkUnopposed(ctx, cause, effect, "causes", 0.9, "llm", "2026-10-01 00:00:00")
	if err != nil {
		t.Fatalf("re-confirm causes: %v", err)
	}
	if !wrote {
		t.Error("a 'causes' re-affirmation was refused because an AGREEING 'supersedes' edge exists: the two relations say the same thing about the pair, so refusing here would re-bill it every pass")
	}

	// And the re-affirmation moved the stamp, which is what skip-if-unchanged
	// reads on the next pass. A 'causes' edge that could not be re-stamped would
	// be re-judged forever.
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
		t.Errorf("causes link = %+v, want %s -> %s with created_at moved to the re-confirming verdict's stamp", found, cause, effect)
	}
}
