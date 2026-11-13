package memory

import (
	"context"
	"testing"
)

func retentionTierFixture(t *testing.T, s *Store, content, tier string) string {
	t.Helper()
	id, _, _, err := s.UpsertWithOptions(context.Background(), testProject, "fact", content, "mcp", 0.6, nil,
		UpsertOptions{Retention: tier})
	if err != nil {
		t.Fatalf("UpsertWithOptions(%s): %v", tier, err)
	}
	return id
}

func TestConsolidationLeavesAPersistentRowAlone(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	keep := retentionTierFixture(t, s, "the bastion is reached on port 2222, never on 22", RetentionPersistent)
	consolidatable := retentionTierFixture(t, s, "an ordinary durable fact the consolidator will replace", RetentionProject)

	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "the corpus after consolidation", Importance: 0.6, Tags: []string{}},
	}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	mems, err := s.GetByIDs(ctx, []string{keep})
	if err != nil || len(mems) != 1 {
		t.Fatalf("the persistent row is gone (err=%v, rows=%d): a keep-forever memory was deleted by a consolidation", err, len(mems))
	}
	if got := mems[0].Content; got != "the bastion is reached on port 2222, never on 22" {
		t.Errorf("the persistent row now says %q, want its own text unchanged", got)
	}
	if got, err := s.GetByIDs(ctx, []string{consolidatable}); err == nil && len(got) == 1 && got[0].ID == consolidatable {
		t.Error("the ordinary row survived consolidation — the test is not exercising the replace path")
	}

	var snapshotted int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM memory_snapshots WHERE memory_id = ?`, keep).Scan(&snapshotted); err != nil {
		t.Fatalf("count snapshots of the persistent row: %v", err)
	}
	if snapshotted != 0 {
		t.Errorf("the persistent row was snapshotted %d time(s); a restore would rewrite it", snapshotted)
	}
}

func TestResolveNeverOffersOrStampsAPersistentRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	keep := retentionTierFixture(t, s, "a keep-forever fact resolve must not demote", RetentionPersistent)
	ordinary := retentionTierFixture(t, s, "a changelog note resolve is allowed to demote", RetentionProject)

	candidates, err := s.ResolveCandidates(ctx, testProject)
	if err != nil {
		t.Fatalf("ResolveCandidates: %v", err)
	}
	for _, c := range candidates {
		if c.ID == keep {
			t.Error("ResolveCandidates offered a persistent row to the classifier")
		}
	}
	foundOrdinary := false
	for _, c := range candidates {
		if c.ID == ordinary {
			foundOrdinary = true
		}
	}
	if !foundOrdinary {
		t.Error("ResolveCandidates offered neither row: the test is not exercising the candidate query")
	}

	n, err := s.SetResolved(ctx, []string{keep, ordinary})
	if err != nil {
		t.Fatalf("SetResolved: %v", err)
	}
	if n != 1 {
		t.Errorf("SetResolved reported %d rows, want 1 — only the ordinary row may be stamped", n)
	}
	if got := getOne(t, s, keep); got.ResolvedAt != nil {
		t.Errorf("the persistent row was stamped resolved_at = %q", *got.ResolvedAt)
	}
	if got := getOne(t, s, ordinary); got.ResolvedAt == nil {
		t.Error("the ordinary row was not stamped — the guard fired on the wrong side")
	}

	resolved, err := s.ResolvedCandidates(ctx, testProject)
	if err != nil {
		t.Fatalf("ResolvedCandidates: %v", err)
	}
	for _, r := range resolved {
		if r.ID == keep {
			t.Error("ResolvedCandidates offered a persistent row to the repair pass")
		}
	}
}

func TestASupersedesEdgeDoesNotSinkAPersistentRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	newer := retentionTierFixture(t, s, "the decision that replaced the older claim", RetentionProject)
	older := retentionTierFixture(t, s, "a claim the newer note superseded", RetentionProject)
	if err := s.CreateLink(ctx, newer, older, "supersedes", 0.9, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	before, err := SupersedePenalties(ctx, s.queryDB(), []string{newer, older}, protectionOf(t, s, newer, older))
	if err != nil {
		t.Fatalf("SupersedePenalties: %v", err)
	}
	if before[older] != 1 {
		t.Fatalf("fixture did not sink the superseded row: penalties = %v, want one on %s", before, older)
	}

	if _, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"a claim the newer note superseded, declared keep-forever afterwards", "mcp", 0.6, nil,
		UpsertOptions{Retention: RetentionPersistent}); err != nil {
		t.Fatalf("raise the superseded row: %v", err)
	}
	ids := []string{newer, older}
	keepID := ""
	for _, m := range []Memory{getOne(t, s, newer), getOne(t, s, older)} {
		if m.Retention == RetentionPersistent {
			keepID = m.ID
		}
	}
	if keepID == "" {
		t.Fatalf("neither row is persistent: %s=%q %s=%q", newer, getOne(t, s, newer).Retention, older, getOne(t, s, older).Retention)
	}
	after, err := SupersedePenalties(ctx, s.queryDB(), ids, protectionOf(t, s, ids...))
	if err != nil {
		t.Fatalf("SupersedePenalties after: %v", err)
	}
	if after[keepID] != 0 {
		t.Errorf("the persistent row still carries a supersede penalty of %d: an edge sinks it despite the exemption", after[keepID])
	}
}

func TestRetentionExemptIsTheOneExemptionPredicate(t *testing.T) {
	if !RetentionExempt(Memory{Retention: RetentionPersistent}) {
		t.Error("a persistent row is not reported exempt")
	}
	for _, tier := range []string{RetentionSession, RetentionProject, ""} {
		if RetentionExempt(Memory{Retention: tier}) {
			t.Errorf("a %q row is reported exempt", tier)
		}
	}
}

func protectionOf(t *testing.T, s *Store, ids ...string) map[string]bool {
	t.Helper()
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = RetentionExempt(getOne(t, s, id))
	}
	return out
}

func TestTheDemotionLookupsDoNotNameTheTierColumn(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	newer := retentionTierFixture(t, s, "the decision that replaced the older claim here", RetentionProject)
	older := retentionTierFixture(t, s, "a claim the newer note superseded entirely", RetentionProject)
	near := retentionTierFixture(t, s, "the pool timeout is 30 seconds in production", RetentionProject)
	nearDup := retentionTierFixture(t, s, "the pool timeout is 30 seconds in prod", RetentionProject)
	if err := s.CreateLink(ctx, newer, older, "supersedes", 0.9, "llm"); err != nil {
		t.Fatalf("CreateLink(supersedes): %v", err)
	}
	if err := s.CreateLink(ctx, near, nearDup, "duplicate", 0.99, "auto"); err != nil {
		t.Fatalf("CreateLink(duplicate): %v", err)
	}

	if _, err := s.db.ExecContext(ctx, `DROP INDEX IF EXISTS idx_memories_session_expiry`); err != nil {
		t.Fatalf("drop the v19 index: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE memories DROP COLUMN retention`); err != nil {
		t.Fatalf("drop the tier column: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE memories DROP COLUMN expires_at`); err != nil {
		t.Fatalf("drop the expiry column: %v", err)
	}

	ids := []string{newer, older, near, nearDup}
	protected := map[string]bool{newer: false, older: false, near: false, nearDup: false}
	penalty, err := SupersedePenalties(ctx, s.queryDB(), ids, protected)
	if err != nil {
		t.Fatalf("SupersedePenalties named a column a v18 store lacks: %v", err)
	}
	if penalty[older] != 1 {
		t.Errorf("the superseded row carries no penalty on a v18 store: %v — the lookup failed open", penalty)
	}
	dup, err := DemotionPenalties(ctx, s.queryDB(), ids, protected, s.demotionThreshold)
	if err != nil {
		t.Fatalf("DemotionPenalties named a column a v18 store lacks: %v", err)
	}
	if dup[near] == 0 && dup[nearDup] == 0 {
		t.Errorf("no near-duplicate was sunk on a v18 store: %v — the lookup failed open", dup)
	}
}

// TestAPinDoesNotStopASupersedesEdge: a pin is not a tier, and this test pins the
// boundary between the two. The near-duplicate demotion spares a pinned row — that
// is what DemotionPenalties did before tiers existed, and the tier merely extends
// it — but a `supersedes` edge does not spare one: the edge states that one claim
// replaced another, and keeping a row on screen is not the same as declaring its
// claim current. So on every supersede ranking path a pinned target sinks below its
// replacement exactly as it did before #587, for the same reason a project-tier
// target does.
//
// The control is in the same test: with the pin cleared and no tier, the same edge
// sinks the same row; the assertion is that the pin changes NOTHING. A future
// follow-up that wants to exempt pins from supersede demotion (sparing a pinned
// row the way this test's predecessor did) must flip this test and the callers'
// maps together — issue #739 documents the variant-follow-up.
func TestAPinDoesNotStopASupersedesEdge(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	newer := retentionTierFixture(t, s, "the block producer reads its slot-leader schedule from the ledger-state directory", RetentionProject)
	target := retentionTierFixture(t, s, "the KES watermark lives under /var/lib/cardano/kes with private permissions", RetentionProject)
	if err := s.CreateLink(ctx, newer, target, "supersedes", 0.9, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	ids := []string{newer, target}

	tierOnly := func() map[string]bool {
		out := make(map[string]bool, len(ids))
		for _, id := range ids {
			out[id] = RetentionExempt(getOne(t, s, id))
		}
		return out
	}

	// The control first, on the ordinary row: this edge DOES sink its target, so
	// the assertion below is about the pin and not about a broken fixture.
	before, err := SupersedePenalties(ctx, s.queryDB(), ids, tierOnly())
	if err != nil {
		t.Fatalf("SupersedePenalties control: %v", err)
	}
	if before[target] != 1 {
		t.Fatalf("the edge did not sink its target unpinned: %v", before)
	}

	if _, err := s.db.ExecContext(ctx, `UPDATE memories SET pinned = 1 WHERE id = ?`, target); err != nil {
		t.Fatalf("pin the superseded target: %v", err)
	}
	after, err := SupersedePenalties(ctx, s.queryDB(), ids, tierOnly())
	if err != nil {
		t.Fatalf("SupersedePenalties after the pin: %v", err)
	}
	if after[target] != 1 {
		t.Errorf("the pinned superseded target carries a penalty of %d, want 1: a pin changed what a `supersedes` edge does", after[target])
	}
	if after[newer] != 0 {
		t.Errorf("the superseder carries a penalty of %d: the edge points the wrong way", after[newer])
	}

	// And through the ranking surface that runs the demotion, not just the helper:
	// the helper is where the rule now lives, `demoteResults` is what a search
	// calls, and it is handed the caller's map through the same path a user's
	// query takes.
	ranked := s.demoteResults(ctx, []Memory{getOne(t, s, target), getOne(t, s, newer)}, SearchParams{SupersedeDemote: true})
	if ranked[0].ID != newer {
		t.Errorf("the search window after the demotion is %s, %s — the pinned superseded row outranks its replacement", ranked[0].ID, ranked[1].ID)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE memories SET pinned = 0 WHERE id = ?`, target); err != nil {
		t.Fatalf("clear the pin: %v", err)
	}
	ranked = s.demoteResults(ctx, []Memory{getOne(t, s, target), getOne(t, s, newer)}, SearchParams{SupersedeDemote: true})
	if ranked[0].ID != newer {
		t.Errorf("with the pin cleared the window is %s, %s, want the replacement first — clearing the pin changed the ranking", ranked[0].ID, ranked[1].ID)
	}
}
