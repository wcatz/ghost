package memory

import (
	"context"
	"math"
	"testing"
	"time"
)

// retentionTierFixture writes one memory of the given tier, plus the ordinary
// project memory the maintenance passes would otherwise touch, so a test can
// compare the two in the same transaction rather than reasoning about a corpus of
// one.
func retentionTierFixture(t *testing.T, s *Store, content, tier string) string {
	t.Helper()
	id, _, _, err := s.UpsertWithOptions(context.Background(), testProject, "fact", content, "mcp", 0.6, nil,
		UpsertOptions{Retention: tier})
	if err != nil {
		t.Fatalf("UpsertWithOptions(%s): %v", tier, err)
	}
	return id
}

// TestConsolidationLeavesAPersistentRowAlone: ReplaceNonManual deletes every
// consolidatable row and re-inserts what the consolidator emitted, so a tier
// that is not in its predicate is a tier whose text the model never saw and
// cannot have re-emitted — the row goes, and no amount of drop-guard auditing
// downstream brings it back.
//
// The second half matters as much as the first: a persistent row must not be in
// the snapshot either, or `ghost reflect --restore` would have a copy of it and
// the exemption would depend on which direction the operator went.
func TestConsolidationLeavesAPersistentRowAlone(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	keep := retentionTierFixture(t, s, "the bastion is reached on port 2222, never on 22", RetentionPersistent)
	consolidatable := retentionTierFixture(t, s, "an ordinary durable fact the consolidator will replace", RetentionProject)

	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "the corpus after consolidation", Importance: 0.6, Tags: []string{}},
	}, ""); err != nil {
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

// TestRestoreCannotRewriteARowThatBecamePersistentAfterTheSnapshot: the
// snapshot is taken at the start of a consolidation, and the tier a row carries
// can change after it. A restore is supposed to undo the consolidation, not to
// re-apply an old version of a memory the user has since declared untouchable —
// so the restore's UPDATE has to check the tier as well as the two filters it
// inherited from #318.
//
// The fixture is a row the consolidator REUSED (same id, so the snapshot's
// id-based restore reaches it), then edited by hand — because a restore only
// rewrites a row whose text has moved since the snapshot, and without the edit
// the restore would be a no-op and the test would pass for the wrong reason.
func TestRestoreCannotRewriteARowThatBecamePersistentAfterTheSnapshot(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "alpha beta gamma", Source: "reflection", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "alpha beta gamma", Importance: 0.5, Tags: []string{}},
	}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	if got := getOne(t, s, id); got.Content != "alpha beta gamma" {
		t.Fatalf("the fixture row was not reused (content %q), so the snapshot does not name its id", got.Content)
	}

	edited := "alpha beta delta epsilon"
	if err := s.UpdateMemory(ctx, testProject, id, &edited, nil, nil, nil); err != nil {
		t.Fatalf("edit the row after the snapshot: %v", err)
	}
	// A near-duplicate save that declares the row keep-forever: the fold raises
	// the SURVIVING row's tier, which is the only way a tier changes after a
	// snapshot was taken.
	if _, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"alpha beta delta epsilon zeta", "mcp", 0.6, nil, UpsertOptions{Retention: RetentionPersistent}); err != nil {
		t.Fatalf("declare the row keep-forever: %v", err)
	}
	before := getOne(t, s, id)
	if before.Retention != RetentionPersistent {
		t.Fatalf("fixture did not reach the row: retention = %q", before.Retention)
	}

	restored, err := s.RestoreSnapshot(ctx, testProject)
	if err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	if restored == 0 {
		t.Skip("the store kept no snapshot to restore from, so this run proves nothing about the restore path")
	}
	after := getOne(t, s, id)
	if after.Content != before.Content {
		t.Errorf("a restore rewrote a persistent row: %q -> %q", before.Content, after.Content)
	}
}

// TestResolveNeverOffersOrStampsAPersistentRow: resolve marks a memory
// resolved_at, which drops it out of ranked injection — it stays searchable, but
// a fresh session no longer sees it. That is a demotion, and a keep-forever row
// the user can only protect from consolidation (ReplaceNonManual) while a later
// pass silently demotes it is a protection with a hole in it.
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

	// And at the write boundary, not only at the query: a caller that hands the
	// id over anyway must be refused the stamp, because the exemption is a
	// property of the row rather than of the reader that found it.
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

	// The repair pass reads the rows resolve already stamped. A persistent row
	// must not appear there either: --reassess exists to undo a wrong
	// resolution, and asking the classifier to re-judge a row it must not touch
	// is how a protection is lost one pass later.
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

// TestASupersedesEdgeDoesNotSinkAPersistentRow: the exemption has to reach the
// demotion, not only the edge. An edge written before the row was declared
// keep-forever still names it, and SupersedePenalties sinks the target on sight
// — so a row that gained its protection after the fact would lose it on the next
// search.
func TestASupersedesEdgeDoesNotSinkAPersistentRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	newer := retentionTierFixture(t, s, "the decision that replaced the older claim", RetentionProject)
	older := retentionTierFixture(t, s, "a claim the newer note superseded", RetentionProject)
	if err := s.CreateLink(ctx, newer, older, "supersedes", 0.9, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	before, err := SupersedePenalties(ctx, s.queryDB(), []string{newer, older})
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
	after, err := SupersedePenalties(ctx, s.queryDB(), ids)
	if err != nil {
		t.Fatalf("SupersedePenalties after: %v", err)
	}
	if after[keepID] != 0 {
		t.Errorf("the persistent row still carries a supersede penalty of %d: an edge sinks it despite the exemption", after[keepID])
	}
}

// TestANearDuplicateDoesNotSinkAPersistentRow: DemotionPenalties already gives
// a pinned row the same protection — the lower-ranked member of a pair loses,
// unless the loser is the pinned one, in which case the other does. A
// keep-forever row is the same claim in a stronger form, and it belongs in that
// map on the same terms: the map is not a list of pins, it is a list of rows the
// user asked to keep visible.
func TestANearDuplicateDoesNotSinkAPersistentRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	first := retentionTierFixture(t, s, "the near-duplicate that was stored first", RetentionPersistent)
	second := retentionTierFixture(t, s, "the near-duplicate that was stored second", RetentionProject)
	if err := s.CreateLink(ctx, second, first, "duplicate", 0.95, "auto"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	// The surviving mapping is built the way every production caller builds it:
	// from the hydrated rows, one entry per row.
	protection := map[string]bool{}
	rank := map[string]int{}
	for i, id := range []string{first, second} {
		m := getOne(t, s, id)
		protection[id] = m.Pinned || RetentionExempt(m)
		rank[id] = i
	}
	penalty, err := DemotionPenalties(ctx, s.db, []string{first, second}, protection, 0.90)
	if err != nil {
		t.Fatalf("DemotionPenalties: %v", err)
	}
	if penalty[first] != 0 {
		t.Errorf("the persistent row is the near-duplicate loser (penalty %d) while the other row is not", penalty[first])
	}
	if penalty[second] != 1 {
		t.Errorf("penalty[second] = %d, want 1 — the ordinary row is the one that should sink", penalty[second])
	}
	_ = rank
}

// TestRetentionExemptIsTheOneExemptionPredicate: every pass asks the same
// question — is this row beyond the automated passes — and the answer has to be
// the same in each of them. The function is what makes that checkable rather
// than four independent readings of a string.
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

// TestTheSessionDecayIsBoundedAndNamedInExplain: the acceptance criterion is
// two claims, and they are different kinds of claim. That a session row decays is
// arithmetic; that it decays to a BOUND (never better than half a durable row) is
// the property that makes it safe to ship as a default, because recency is the
// strongest signal in the composite score and an unbounded decay would one day
// make a memory nobody wants again the top hit. And explain has to name the
// signal, or a user asking "why is this ranked here" gets a number with no
// label.
func TestTheSessionDecayIsBoundedAndNamedInExplain(t *testing.T) {
	if got := RetentionDecayFactor(RetentionProject, 0); got != 1.0 {
		t.Errorf("a project row's tier factor at age 0 = %v, want 1.0 — a durable memory's score must be exactly what it was before tiers", got)
	}
	if got := RetentionDecayFactor(RetentionPersistent, 0); got != 1.0 {
		t.Errorf("a persistent row's tier factor = %v, want 1.0", got)
	}
	if got := RetentionDecayFactor(RetentionSession, 0); got != 1.0 {
		t.Errorf("a brand-new session row's tier factor = %v, want 1.0 — the decay is an age term, and a new row is not old", got)
	}
	// The bound, at every age. It has two ends and both matter: the factor never
	// EXCEEDS 1.0, so a session row can never outrank a durable row of the same
	// age, importance and relevance — recency is the strongest signal in the
	// composite score and this is what stops it from being the whole answer. And
	// it never falls below the floor, which is what keeps an old session memory
	// findable rather than quietly unfindable.
	for _, age := range []float64{0.01, 1, 7, 30, 100, 10000} {
		got := RetentionDecayFactor(RetentionSession, age)
		if got > 1.0 {
			t.Errorf("session tier factor at age %v = %v, above 1.0: a session row must never outrank its durable twin", age, got)
		}
		if got < sessionDecayFloor-1e-9 {
			t.Errorf("session tier factor at age %v = %v, below the %v floor that keeps the row findable", age, got, sessionDecayFloor)
		}
	}
	// And monotone downward, so age is the only thing that moves it.
	prev := 2.0
	for age := 0.0; age <= 60; age += 5 {
		got := RetentionDecayFactor(RetentionSession, age)
		if got > prev {
			t.Fatalf("session tier factor rose from %v to %v at age %v", prev, got, age)
		}
		prev = got
	}
	// The asymptote is the floor, and a row far older than the tau is there:
	// a bound nothing reaches is not a bound.
	if got := RetentionDecayFactor(RetentionSession, 100000); math.Abs(got-sessionDecayFloor) > 1e-9 {
		t.Errorf("session tier factor deep past the tau = %v, want the %v floor", got, sessionDecayFloor)
	}
	// And the composite consequence, which is the property an operator cares
	// about: identical in every other respect, a session row never beats the
	// durable row it duplicates.
	for _, age := range []float64{0, 3, 30, 365} {
		session := DecayFactor("fact", RetentionSession, false, age)
		durable := DecayFactor("fact", RetentionProject, false, age)
		if session > durable {
			t.Errorf("at age %v a session row scores %v, above the durable %v", age, session, durable)
		}
		if session < durable*0.49 {
			t.Errorf("at age %v a session row scores %v, more than half again below the durable %v", age, session, durable)
		}
	}
}

// TestTheSessionDecayMatchesTheSQLItRanks: DecayRankingSQL and DecayFactor are
// two spellings of one formula and the session half is the newest part of it, so
// it is the part most likely to be edited in one and not the other. This is the
// same parity guard the category decay has, over the tier.
func TestTheSessionDecayMatchesTheSQLItRanks(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const eps = 1e-4

	for _, age := range []int{0, 1, 7, 14, 60, 365, 5000} {
		for _, cat := range []string{"fact", "convention", "pattern", "decision"} {
			id, err := s.Create(ctx, testProject, Memory{
				Category: cat, Content: cat, Source: "manual", Importance: 1.0, Retention: RetentionSession,
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			setCreatedAtDaysAgo(t, s, id, age)

			sqlFactor := memoryScore(t, s, id)
			goAge := time.Since(memoryCreatedAtAsTime(t, s, id)).Hours() / 24.0
			if goAge < 0 {
				goAge = 0
			}
			goFactor := DecayFactor(cat, RetentionSession, false, goAge)
			if diff := sqlFactor - goFactor; diff > eps || diff < -eps {
				t.Errorf("category=%s age=%d: SQL=%v Go=%v", cat, age, sqlFactor, goFactor)
			}
		}
	}
}
