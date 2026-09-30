package supersede

import (
	"context"
	"database/sql"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// This file is #823: what a live 'causes' edge does to the pass, and what it does
// NOT do.
//
// `ghost supersede` writes two directed relations and reconciled only one of
// them. A 'supersedes' edge is reconciled on the UNORDERED pair and its direction
// beats the scan's, skip-if-unchanged holds a pair whose endpoints have not moved
// since the edge was written, a pair a live edge names is never cache-skipped,
// and a write that would create the reverse of a live 'supersedes' edge is
// refused inside the write transaction (memory.Store.CreateLinkUnopposed, #806).
// A 'causes' edge got NONE of that: it is written older→newer, and the pair it
// belongs to was reconciled by the same `pairKey` with no direction override, so
// the direction came from the scan — from `orient`, from `updated_at`.
//
// So a pair carrying a live 'causes' edge whose direction disagrees with the
// timestamps today:
//
//  1. is re-proposed by the scan every pass, in the direction the timestamps
//     give, because nothing about a 'causes' edge holds the pair quiet — only a
//     'supersedes' pair is ever stamped and only a 'supersedes' pair is ever
//     cache-skipped;
//  2. pays a classify call for that re-ask, on every pass, for as long as the
//     edge lives;
//  3. and answers CAUSES again, so the pass writes a 'causes' edge in the NEW
//     direction — leaving the pair live in BOTH directions, a 'causes' cycle the
//     store can produce and nothing demotes.
//
// Point 2 is the re-bill. Point 3 is the contradiction, and #819 refused to guard
// the 'causes' write precisely because of point 2: a guard alone, with no
// override, is a refusal that never converges, which costs a call per pass
// forever. The two have to land TOGETHER, in this order — with the override in
// place the pair is judged in the live edge's own direction, so the guarded
// write is in that direction too and the refusal becomes the one-off race it is
// for 'supersedes', rather than a state the pass re-enters. That ordering is the
// whole fix, and it is why TestTheCausesWriteIsNotGuarded had to CHANGE rather
// than be deleted: it pinned the asymmetry, and the asymmetry was the trap.

// recordingCauses answers CAUSES to every pair and records every orientation it
// was asked about, so a test can ask both questions at once: was the pair asked
// about at all, and in which direction.
//
// CAUSES is the verdict that writes a 'causes' edge — the relation whose
// direction is under test — so a fake answering anything else would settle the
// question by never asking it.
type recordingCauses struct {
	judged [][2]string
}

func (c *recordingCauses) ClassifyBatch(_ context.Context, pairs []Candidate) ([]Relation, error) {
	out := make([]Relation, len(pairs))
	for i, p := range pairs {
		c.judged = append(c.judged, [2]string{p.NewerID, p.OlderID})
		out[i] = RelationCauses
	}
	return out, nil
}

// retagBoth moves both endpoints' updated_at through the REAL writer, which is
// what an operator's edit through a live `ghost mcp` does and what re-arms
// skip-if-unchanged. A fixture that poked the column directly would be asserting
// something about the column rather than about the pass's freshness test — and
// the tag is a real change, so the write is a real one.
func retagBoth(t *testing.T, store *memory.Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := store.UpdateMemory(context.Background(), "p", id, nil, nil, nil, []string{"retagged-mid-pass"}); err != nil {
			t.Fatalf("retag %s: %v", id, err)
		}
	}
}

// TestALiveCausesEdgeHoldsItsPairQuiet is the ordinary pass's half, and it is the
// re-bill #823 is about: a live 'causes' edge whose direction disagrees with the
// scan's must produce NO classify call, because neither endpoint has moved since
// the edge was written.
//
// The observable is the call count, not the graph. A graph assertion alone would
// be satisfied by a pass that asked the question and then declined to act on the
// answer — which is the state #819 documented, and the one that costs a call per
// pass forever.
//
// The edge's STAMP is what buys the quiet and nothing else. It is not a
// direction the pair is judged in — see TestACausesEdgeCannotDecideTheDirectionASupersedesEdgeIsWrittenIn
// for what reading it as one costs — so `OppositeLive` is 0 here where it was 1:
// there is no refused orientation any more, because the pair is never proposed in
// one that opposes the edge. A counted refusal was the wrong instrument for a
// quiet, and the call count is the right one.
func TestALiveCausesEdgeHoldsItsPairQuiet(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	newer := add(t, store, db, "the restore is being rewritten to run on one spindle", []float32{1, 0, 0}, "2026-09-01 00:00:00")
	older := add(t, store, db, "the restore path on one spindle is safe and fast", []float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")
	// A live 'causes' edge running NEWER→OLDER, which is the reverse of the
	// direction 'causes' is written in and so the reverse of what the timestamps
	// propose. Stamped at the freshness of what a verdict was made against, which
	// is what lets skip-if-unchanged hold the pair at all.
	if err := store.CreateLinkJudged(ctx, newer, older, string(RelationCauses), 0.9, "llm", "2026-09-01 00:00:00"); err != nil {
		t.Fatal(err)
	}

	cls := &recordingCauses{}
	res, classified, err := Run(ctx, store, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(cls.judged) != 0 {
		t.Errorf("the pass asked about %d pair-orientation(s) %v, want none: neither endpoint has moved since the edge was written, and a pair a live 'causes' edge names is held by skip-if-unchanged exactly as a live 'supersedes' edge is",
			len(cls.judged), cls.judged)
	}
	if len(classified) != 0 {
		t.Errorf("classified = %+v, want no rows", classified)
	}
	if res.Candidates != 0 {
		t.Errorf("Candidates = %d, want 0: the pair's edge stamp is the freshness of the text it was judged against", res.Candidates)
	}
	// The graph, read from the store rather than from the pass's counters: the
	// reverse of the live edge was never written, so the pair is NOT live in both
	// directions. Before the fix a second edge appears here.
	edges := liveCausesEdges(t, store, newer, older)
	if len(edges) != 1 || edges[0] != [2]string{newer, older} {
		t.Errorf("live 'causes' edges = %v, want exactly [%s %s]: the pass must never write the reverse of a live 'causes' edge, and nothing was re-decided here so nothing was written at all",
			edges, newer, older)
	}
	// And the refusal is COUNTED, because a pass that declined to re-ask has to
	// say so rather than reporting the totals of a pass that found nothing.
	if res.OppositeLive != 0 {
		t.Errorf("OppositeLive = %d, want 0: a 'causes' edge's direction does not decide the question, so there is no proposal opposing it and nothing to refuse",
			res.OppositeLive)
	}
	if res.Bidirectional != 0 {
		t.Errorf("Bidirectional = %d, want 0: the pair is claimed in ONE direction, so there is no cycle to refuse", res.Bidirectional)
	}
}

// TestACausesVerdictCorrectsALiveCausesEdgeThatDisagreesWithTheTimestamps is the
// half that cannot be reached through quiet: the endpoints HAVE moved, so
// skip-if-unchanged releases the pair and the pass really does spend a classify
// call.
//
// What it writes is the point. The pair is asked in the direction `orient` gives,
// and a CAUSES verdict therefore writes `causes older→newer` — which is the
// REVERSE of the edge a disagreeing store holds. It converges precisely because
// the apply block drops what the pass read before it writes: the verdict
// invalidates the live edge and then writes its own, so the pair ends up with ONE
// 'causes' edge in the direction the model just chose. Asking the pair and then
// refusing the write — a guard alone, which is what #819 shipped on purpose —
// would converge on nothing and would re-ask forever, and writing the live edge's
// direction instead would have meant asking a question the edge cannot answer,
// since the edge is not a supersession (see
// TestACausesEdgeCannotDecideTheDirectionASupersedesEdgeIsWrittenIn).
func TestACausesVerdictCorrectsALiveCausesEdgeThatDisagreesWithTheTimestamps(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	newer := add(t, store, db, "the restore is being rewritten to run on one spindle", []float32{1, 0, 0}, "2026-09-01 00:00:00")
	older := add(t, store, db, "the restore path on one spindle is safe and fast", []float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")
	// A live 'causes' edge, and STAMPED OLD: the edge claims a judgement made
	// long ago, so both endpoints have since moved and the pair is re-judged.
	if err := store.CreateLinkJudged(ctx, newer, older, string(RelationCauses), 0.9, "llm", "2020-01-01 00:00:00"); err != nil {
		t.Fatal(err)
	}
	// Both endpoints move, so nothing about the quiet can be the reason the
	// assertions below hold.
	retagBoth(t, store, newer, older)

	cls := &recordingCauses{}
	res, classified, err := Run(ctx, store, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(cls.judged) != 1 {
		t.Fatalf("the pass asked about %d pair-orientation(s) %v, want exactly 1: both endpoints moved, so skip-if-unchanged cannot hold this pair and the re-ask is the point of the test",
			len(cls.judged), cls.judged)
	}
	// The direction asked is the TIMESTAMPS'. The live edge runs newer→older,
	// which asserts that the September note is the cause and the January one its
	// effect, so a question in the edge's own direction would carry the edge's
	// claim rather than the two notes' chronology — and a supersedes answer to it
	// writes a demotion of the memory that is current.
	if cls.judged[0] != [2]string{newer, older} {
		t.Errorf("the pair was asked about as %v, want [%s %s]: the timestamps' direction, and a 'causes' edge is a claim about which note CAUSED which, not about which one replaces the other",
			cls.judged[0], newer, older)
	}
	if res.OppositeLive != 0 {
		t.Errorf("OppositeLive = %d, want 0: the pair is not proposed in a direction that opposes the live edge, it is asked in the direction it is written in", res.OppositeLive)
	}
	if len(classified) != 1 || !classified[0].Reclassified {
		t.Fatalf("classified = %+v, want one row marked reclassified: this is a verdict about an edge in the store, not about a proposal", classified)
	}
	// The write landed in the direction asked, and the edge it contradicts is
	// gone: ONE live 'causes' edge, and the pair is not a cycle. The reverse is
	// never left in place beside the write, which is the property the store-level
	// guard exists to make impossible for a concurrent pass.
	edges := liveCausesEdges(t, store, newer, older)
	if len(edges) != 1 || edges[0] != [2]string{older, newer} {
		t.Errorf("live 'causes' edges = %v, want exactly [%s %s]: the verdict's own direction was written and the edge it contradicted was dropped, so the pair converged on one edge rather than becoming a cycle",
			edges, older, newer)
	}
	// The row names the relation it acted on, so a report can tell a
	// re-affirmation from a change of relation — which is the difference between
	// "the edge held" and "the edge became something else".
	if got := classified[0].ReclassifiedFrom; got != RelationCauses {
		t.Errorf("ReclassifiedFrom = %q, want %q: the pair carried a live 'causes' edge, and that is the fact a report needs to know whether the verdict changed the graph's relation",
			string(got), string(RelationCauses))
	}
	if res.ReverseLive != 0 {
		t.Errorf("ReverseLive = %d, want 0: the write was in the live edge's own direction, so nothing opposed it", res.ReverseLive)
	}
	if res.Reclassified != 1 {
		t.Errorf("Reclassified = %d, want 1: the relation is unchanged, but a live 'causes' edge was DROPPED, so this row moved the graph and must not be reported as a quiet re-affirmation", res.Reclassified)
	}
}

// TestACausesVerdictReAffirmsALiveCausesEdgeThatAgreesWithTheTimestamps is the
// other half of the correction above, and the reason `Reclassified` is decided by
// whether a ROW moved rather than by whether the relation changed. Here the live
// edge already runs the way a CAUSES verdict writes, so the verdict re-stamps it
// and the graph is unchanged — which is the one CAUSES outcome that is not a
// reclassification.
func TestACausesVerdictReAffirmsALiveCausesEdgeThatAgreesWithTheTimestamps(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	newer := add(t, store, db, "the restore is being rewritten to run on one spindle", []float32{1, 0, 0}, "2026-09-01 00:00:00")
	older := add(t, store, db, "the restore path on one spindle is safe and fast", []float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")
	// The live edge already runs the way `causes` is written — the CAUSE is the
	// older note and the EFFECT the newer one — and it is stamped old so the pair
	// is re-judged.
	if err := store.CreateLinkJudged(ctx, older, newer, string(RelationCauses), 0.9, "llm", "2020-01-01 00:00:00"); err != nil {
		t.Fatal(err)
	}
	retagBoth(t, store, newer, older)

	cls := &recordingCauses{}
	res, classified, err := Run(ctx, store, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(cls.judged) != 1 || cls.judged[0] != [2]string{newer, older} {
		t.Fatalf("asked about %v, want exactly [%s %s]", cls.judged, newer, older)
	}
	edges := liveCausesEdges(t, store, newer, older)
	if len(edges) != 1 || edges[0] != [2]string{older, newer} {
		t.Errorf("live 'causes' edges = %v, want exactly [%s %s]: a verdict that agrees with the live edge must leave it where it is, and one edge is not a cycle",
			edges, older, newer)
	}
	if len(classified) != 1 || classified[0].ReclassifiedFrom != RelationCauses {
		t.Errorf("classified = %+v, want one row that names the 'causes' edge it was judged against", classified)
	}
	if res.Reclassified != 0 {
		t.Errorf("Reclassified = %d, want 0: nothing was dropped and the relation is unchanged, so the graph is byte-for-byte what it was and the row is not a reclassification", res.Reclassified)
	}
	if classified[0].CausesDropped != 0 {
		t.Errorf("CausesDropped = %d, want 0: the edge the verdict re-affirmed is not dropped by its own verdict", classified[0].CausesDropped)
	}
}

// guardedWriterSpy records which of the two link writers the pass reached for.
// It is a behavioural probe, not a source check: the defect this file is about is
// WHICH writer a verdict uses, and the two writers differ in nothing a caller
// can otherwise observe except whether the graph can end up holding a pair in
// both directions.
type guardedWriterSpy struct {
	*memory.Store
	unopposed []string
	judged    []string
}

func (s *guardedWriterSpy) CreateLinkUnopposed(ctx context.Context, sourceID, targetID, relation string, strength float32, source, judgedAt string) (bool, error) {
	s.unopposed = append(s.unopposed, relation)
	return s.Store.CreateLinkUnopposed(ctx, sourceID, targetID, relation, strength, source, judgedAt)
}

func (s *guardedWriterSpy) CreateLinkJudged(ctx context.Context, sourceID, targetID, relation string, strength float32, source, judgedAt string) error {
	s.judged = append(s.judged, relation)
	return s.Store.CreateLinkJudged(ctx, sourceID, targetID, relation, strength, source, judgedAt)
}

// TestTheCausesWriteIsNowGuarded replaces TestTheCausesWriteIsNotGuarded, and
// it changes its mind for the reason #823 gives rather than for tidiness.
//
// #819 shipped the 'causes' write unguarded and said why: the direction override
// was built from live SUPERSEDES edges, so a pair whose 'causes' edge ran against
// the timestamps was re-proposed flipped on every pass, answered CAUSES, and
// would have been refused on every one — a refusal that never converges, at a
// classify call per pass, for as long as the edge lives. That trade was worse
// than the contradiction it prevented, and the asymmetry was pinned from the
// pass's side so a later change could not "tidy" the guard onto both relations
// for the sake of symmetry.
//
// The override is now built from BOTH relations, so a pair is judged in the live
// edge's own direction and the write is in that direction too. The refusal is
// therefore the same one 'supersedes' has: a cross-process race that costs one
// call, not a state the pass re-enters. And the write has to go through the
// guarded writer, because the state it closes is exactly one the pass's own
// reads cannot see — two passes, or a pass and a pre-#823 store.
func TestTheCausesWriteIsNowGuarded(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	spy := &guardedWriterSpy{Store: store}

	older := add(t, store, db, "the restore path on one spindle is safe and fast", []float32{1, 0, 0}, "2026-01-01 00:00:00")
	newer := add(t, store, db, "the restore is being rewritten to run on one spindle", []float32{0.98, 0.02, 0}, "2026-09-01 00:00:00")

	res, _, err := Run(ctx, spy, &recordingCauses{}, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.CausesCreated != 1 {
		t.Errorf("CausesCreated = %d, want 1: nothing opposed this write, and a guard that refuses an unopposed pair is not a guard", res.CausesCreated)
	}
	// The pair is unclaimed, so the guarded writer is the one that has to be
	// reached for — and it wrote.
	if len(spy.unopposed) != 1 || spy.unopposed[0] != string(RelationCauses) {
		t.Errorf("guarded writer saw %v, want exactly [%q]: a 'causes' edge is a directed claim about a pair, and the second direction of one another writer has already claimed is the state this fix exists to prevent",
			spy.unopposed, string(RelationCauses))
	}
	if len(spy.judged) != 0 {
		t.Errorf("unguarded writer saw %v, want none: the unguarded spell is CreateLink's, and it is what the linker's 'related' edges and the bench seeders use",
			spy.judged)
	}
	if edges := liveCausesEdges(t, store, older, newer); len(edges) != 1 || edges[0] != [2]string{older, newer} {
		t.Errorf("live 'causes' edges = %v, want exactly [%s %s]: 'causes' is written older→newer", edges, older, newer)
	}
}

// TestACausesVerdictReportsTheRelationItReplaced is the report's half, and it is
// the reason Classified carries ReclassifiedFrom.
//
// A live 'causes' edge re-judged into SUPERSEDES is a change of relation: the
// pair is now linked in the other relation, and the 'causes' row is gone. The
// pass's own counter has to say so (Result.Reclassified) and the row has to carry
// which relation it replaced, or a report prints the new 'supersedes' edge and
// says nothing about the graph row that was removed — "0 reclassified" over a
// pair whose relation changed is the same misleading line in a new place.
func TestACausesVerdictReportsTheRelationItReplaced(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	newer := add(t, store, db, "kubernetes runs on 1.31 everywhere", []float32{1, 0, 0}, "2026-09-01 00:00:00")
	older := add(t, store, db, "kubernetes cluster runs 1.27", []float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")
	// A live 'causes' edge in the direction the timestamps agree with, stamped
	// old so the pair is re-judged. The verdict is SUPERSEDES, so the relation
	// changes.
	if err := store.CreateLinkJudged(ctx, older, newer, string(RelationCauses), 0.9, "llm", "2020-01-01 00:00:00"); err != nil {
		t.Fatal(err)
	}
	retagBoth(t, store, newer, older)

	res, classified, err := Run(ctx, store, &supersedesEverything{}, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(classified) != 1 {
		t.Fatalf("classified = %+v, want one row", classified)
	}
	c := classified[0]
	if c.ReclassifiedFrom != RelationCauses {
		t.Errorf("ReclassifiedFrom = %q, want %q", string(c.ReclassifiedFrom), string(RelationCauses))
	}
	if c.Relation != RelationSupersedes {
		t.Fatalf("Relation = %q, want %q", string(c.Relation), string(RelationSupersedes))
	}
	if res.Reclassified != 1 {
		t.Errorf("Reclassified = %d, want 1: a 'causes' edge became a 'supersedes' edge, which is a change to what the graph holds", res.Reclassified)
	}
	if res.ReclassifiedNoWrite != 0 {
		t.Errorf("ReclassifiedNoWrite = %d, want 0: this pair's --apply effect was a WRITE (the new edge), not purely destructive", res.ReclassifiedNoWrite)
	}
	// The graph, not the counters: the 'causes' edge is gone and the
	// 'supersedes' one is in its place. Nothing demotes on a 'causes' edge, so
	// this is a contradiction the pass settles rather than harm in flight.
	if liveRelations(t, store, older)[string(RelationCauses)] {
		t.Error("the 'causes' edge is still live after a SUPERSEDES verdict re-judged that pair")
	}
	if got := liveSupersedesEdges(t, store, newer, older); len(got) != 1 || got[0] != [2]string{newer, older} {
		t.Errorf("live supersedes edges = %v, want exactly [%s %s]", got, newer, older)
	}
}

// TestALiveCausesEdgeIsNeverCacheSkipped is the NEITHER cache's half, and it is
// the third thing a live 'causes' edge did not get.
//
// A cache skip is treated as equivalent to a NEITHER verdict — a pair whose
// endpoints' text still matches a stored verdict is not re-asked. For a pair a
// live 'supersedes' edge names that is impossible, and for a pair a live
// 'causes' edge names it was a live hole until #823: the cache skip asserted the
// edge was not there, and the edge's direction is what decides how the pair is
// judged, so a skipped pair is a pair the graph and the pass disagree about while
// the pass reports itself quiet.
//
// The fixture is the one state where the two rules collide: a stored NEITHER
// verdict for the SCAN's orientation (which is what a previous pass would have
// cached, before it could see the edge), a live 'causes' edge in the other
// direction, and endpoints that have moved since both — so skip-if-unchanged
// releases the pair and the cache is the only thing that could skip it.
func TestALiveCausesEdgeIsNeverCacheSkipped(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	const newerText = "the restore is being rewritten to run on one spindle"
	const olderText = "the restore path on one spindle is safe and fast"
	newer := add(t, store, db, newerText, []float32{1, 0, 0}, "2026-09-01 00:00:00")
	older := add(t, store, db, olderText, []float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")
	// The live 'causes' edge runs the other way, stamped old.
	if err := store.CreateLinkJudged(ctx, newer, older, string(RelationCauses), 0.9, "llm", "2020-01-01 00:00:00"); err != nil {
		t.Fatal(err)
	}
	// The cache rows a previous pass would have left. BOTH orientations, and that
	// is what makes this a test of the rule rather than of a lookup that happens
	// to miss: the cache is keyed by the ORDERED pair, and a store that has been
	// judged in either orientation holds a row under each — #823 asked contested
	// pairs in the live edge's direction, which for this fixture is older-first.
	// Only the one the pass will look up is load-bearing, so seeding just the other
	// one would let a regression hide behind a cache MISS.
	//
	// Each row carries the hashes of the notes ITS OWN key names — a row keyed
	// (older, newer) records the older note's text under NewerHash — because the
	// lookup reads them against the candidate's own orientation. Both rows are
	// real hits: the retag below moves updated_at and changes no text, which is
	// the whole point of a content-keyed cache and the reason a tag edit is the
	// write that reaches it.
	if err := store.MarkSupersedeNeither(ctx, "p", map[[2]string]memory.SupersedeCheck{
		{newer, older}: {NewerHash: contentHash(newerText), OlderHash: contentHash(olderText)},
		{older, newer}: {NewerHash: contentHash(olderText), OlderHash: contentHash(newerText)},
	}); err != nil {
		t.Fatalf("MarkSupersedeNeither: %v", err)
	}
	// Both endpoints have moved since, so nothing but the cache could hold this
	// pair quiet.
	retagBoth(t, store, newer, older)

	cls := &recordingCauses{}
	res, _, err := Run(ctx, store, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Skipped != 0 {
		t.Errorf("Skipped = %d, want 0: a pair a live 'causes' edge names is never cache-skipped, because a cache skip is a NEITHER verdict and this pair is linked", res.Skipped)
	}
	if len(cls.judged) != 1 {
		t.Fatalf("the pass asked about %d pair(s) %v, want exactly 1", len(cls.judged), cls.judged)
	}
	if cls.judged[0] != [2]string{newer, older} {
		t.Errorf("asked about %v, want [%s %s]: the direction the pair is asked in, which is the timestamps' and NOT the one the cache row keyed by the older-first pair names",
			cls.judged[0], newer, older)
	}
}

// TestASupersedesVerdictDropsTheCausesEdgeThatContradictsIt is the sweep a
// verdict owes in BOTH directions, and it is a shape the pass used to leave
// behind.
//
// A live 'supersedes' edge A→B says A is the newer note. A 'causes' edge A→B
// says the opposite — its source is the CAUSE, so A is the older one and B the
// effect. The two cannot both be true of the pair, and a SUPERSEDES verdict
// denies the 'causes' claim whichever end it is written from: a pair whose newer
// note retires the older one is not one note having caused the other.
//
// The pass has always swept the 'causes' edge in the direction a supersession
// implies (older→newer), and the contradicting one was left in place — invisible
// until the 'causes' edges became load-bearing and this shape became a pair the
// reconciliation had to read rather than a row nobody looked at.
func TestASupersedesVerdictDropsTheCausesEdgeThatContradictsIt(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	newer := add(t, store, db, "kubernetes runs on 1.31 everywhere", []float32{1, 0, 0}, "2026-09-01 00:00:00")
	older := add(t, store, db, "kubernetes cluster runs 1.27", []float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")
	// Both edges named by the same two ids, in the two relations' conventions, so
	// they contradict. Stamped old so the pair is re-judged.
	if err := store.CreateLinkJudged(ctx, newer, older, string(RelationSupersedes), 0.9, "llm", "2020-01-01 00:00:00"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateLinkJudged(ctx, newer, older, string(RelationCauses), 0.9, "llm", "2020-01-01 00:00:00"); err != nil {
		t.Fatal(err)
	}

	cls := &supersedesEverything{}
	_, classified, err := Run(ctx, store, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(cls.judged) != 1 || cls.judged[0] != [2]string{newer, older} {
		t.Fatalf("asked about %v, want exactly [%s %s]: the 'supersedes' edge decides the pair's direction, and it is the edge a contradiction has to be settled against",
			cls.judged, newer, older)
	}
	if len(classified) != 1 {
		t.Fatalf("classified = %+v, want one row", classified)
	}
	if got := classified[0].CausesDropped; got != 1 {
		t.Errorf("CausesDropped = %d, want 1: the SUPERSEDES verdict denies the contradicting 'causes' claim, and the row has to say the run moved a second graph row", got)
	}
	if liveRelations(t, store, older)[string(RelationCauses)] {
		t.Error("the contradicting 'causes' edge is still live after the verdict that denies it")
	}
	if got := liveSupersedesEdges(t, store, newer, older); len(got) != 1 || got[0] != [2]string{newer, older} {
		t.Errorf("live supersedes edges = %v, want exactly [%s %s]: the edge the verdict re-affirmed is not the one it drops", got, newer, older)
	}
	// And the row is a RE-AFFIRMATION, not a withdrawal: the edge the pair was
	// judged around survived, so a report must not call it withdrawn.
	if classified[0].ReclassifiedFrom != RelationSupersedes || classified[0].Withdrawn {
		t.Errorf("row = %+v, want a reclassified supersedes pair that was NOT withdrawn: %q vs %q, withdrawn %v",
			classified[0], string(classified[0].ReclassifiedFrom), string(RelationSupersedes), classified[0].Withdrawn)
	}
}

// TestACausesCycleIsSettledRatherThanFrozen is the case the pass does NOT
// refuse, and the reason is in Result.Bidirectional.
//
// Two 'causes' edges in opposite directions assert that each of the pair's notes
// caused the other, which is a contradiction rather than a cycle with harm in
// flight: a 'supersedes' cycle is refused because both its edges demote one of
// the pair's two memories, and no orientation of it can be judged into a state
// worth keeping. Nothing demotes on a 'causes' edge.
//
// So this pair is judged ONCE, in the direction the timestamps give — the only
// direction a pair whose two edges disagree has — and the ordinary verdict
// converges it: an affirming verdict keeps the direction it was asked about and
// drops the edge asserting the other, a denying one drops both. The alternative,
// refusing it, would leave a contradiction no repair can reach, because
// `ghost supersede --reassess` loads live 'supersedes'/'llm' edges and cannot see
// a 'causes' one at all.
func TestACausesCycleIsSettledRatherThanFrozen(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	// Two notes whose timestamps DO order them, so the pair is judgeable.
	first := add(t, store, db, "the restore path on one spindle is safe and fast", []float32{1, 0, 0}, "2026-09-01 00:00:00")
	second := add(t, store, db, "the restore is being rewritten to run on one spindle", []float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")
	// Both directions, the state a pass before #823 could leave.
	for _, dir := range [][2]string{{first, second}, {second, first}} {
		if err := store.CreateLinkJudged(ctx, dir[0], dir[1], string(RelationCauses), 0.9, "llm", "2020-01-01 00:00:00"); err != nil {
			t.Fatal(err)
		}
	}

	cls := &recordingCauses{}
	res, classified, err := Run(ctx, store, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Bidirectional != 0 {
		t.Errorf("Bidirectional = %d, want 0: this is a 'causes' cycle, and the refusal is for a 'supersedes' one — refusing this would leave a contradiction `--reassess` cannot see",
			res.Bidirectional)
	}
	if len(cls.judged) != 1 {
		t.Fatalf("asked about %d pair(s) %v, want exactly 1: a cycle is ONE question", len(cls.judged), cls.judged)
	}
	// Asked in the direction the timestamps give, since neither stored edge is
	// preferred over the other.
	if cls.judged[0] != [2]string{first, second} {
		t.Errorf("asked about %v, want [%s %s]: the two 'causes' edges disagree, so the only direction available is the one the timestamps give",
			cls.judged[0], first, second)
	}
	edges := liveCausesEdges(t, store, first, second)
	if len(edges) != 1 || edges[0] != [2]string{second, first} {
		t.Errorf("live 'causes' edges = %v, want exactly [%s %s]: the verdict keeps the direction it was asked about and drops the edge asserting the other",
			edges, second, first)
	}
	if len(classified) != 1 || !classified[0].Withdrawn {
		t.Errorf("classified = %+v, want one row marked withdrawn: the run dropped a live graph row and the row has to say so", classified)
	}
	// The standing edge carries THIS verdict's stamp, and that is the ordering
	// inside the apply block rather than an incidental detail: settled by writing
	// first and sweeping after, the guarded write finds the cycle's other half
	// live, refuses (correctly — it is the reverse of what it is about to write),
	// and the sweep then drops that half, leaving the OLD edge with its OLD stamp.
	// The graph looks the same and the pair is re-billed on the next pass for ever,
	// which is the re-bill this whole change is about arriving through the back
	// door.
	var standing memory.Link
	for _, l := range mustGetLinks(t, store, second) {
		if l.Relation == string(RelationCauses) {
			standing = l
		}
	}
	if standing.CreatedAt != "2026-09-01 00:00:00" {
		t.Errorf("the standing 'causes' edge is stamped %q, want this verdict's judgement stamp 2026-09-01 00:00:00: an edge left holding an old stamp is a pair the next pass asks about again",
			standing.CreatedAt)
	}

	// And it CONVERGES: a second pass finds the pair in one direction, holds it
	// quiet, and spends nothing. A rule that asked the question every pass would
	// pass every assertion above and still be the re-bill #823 is about.
	quiet := &recordingCauses{}
	second_res, _, err := Run(ctx, store, quiet, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run (second): %v", err)
	}
	if len(quiet.judged) != 0 {
		t.Errorf("the second pass asked about %v, want nothing: the pair is now claimed in ONE direction with neither endpoint moved since the verdict, which is what skip-if-unchanged reads",
			quiet.judged)
	}
	if second_res.Bidirectional != 0 || second_res.Unoriented != 0 {
		t.Errorf("the second pass reported Bidirectional=%d Unoriented=%d, want 0/0: a settled pair is an ordinary quiet one",
			second_res.Bidirectional, second_res.Unoriented)
	}
}

// TestADryRunForecastsTheCausesCycleHalfItsVerdictWouldDrop is the preview half
// of the test above, and it is the last thing a dry run got wrong: the verdict
// deletes a live graph row, the applied run says so on the row, and the dry run
// said nothing at all — because CausesDropped counts what --apply actually
// invalidated, so a preview of a two-mutation verdict showed one of them.
//
// The forecast costs no read. It is counted off the same edges this pass loaded,
// from the same claimsHold predicates the apply block's sweeps are gated on, so
// the number the preview prints and the number the applied run reports are
// computed from one snapshot and cannot disagree about whether there IS a row to
// move. A prediction QUERY would have been the alternative and would have put a
// second read's failure into a pass whose only fatal error is a write error.
func TestADryRunForecastsTheCausesCycleHalfItsVerdictWouldDrop(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	newer, older := seedCausesCycle(t, store, db)

	// apply=false, so the pass reads and judges and writes NOTHING.
	cls := &recordingCauses{}
	res, classified, err := Run(ctx, store, cls, "p", 0.9, false, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reclassified != 1 {
		t.Errorf("Reclassified = %d, want 1: the counter is set from the same prediction the row carries, so a dry run's tally and its rows cannot disagree",
			res.Reclassified)
	}
	if len(classified) != 1 {
		t.Fatalf("classified = %+v, want exactly one row", classified)
	}
	row := classified[0]
	if row.CausesDroppable != 1 {
		t.Errorf("CausesDroppable = %d, want 1: the pair holds a 'causes' edge in the direction this verdict is about to write, and the other one is what the verdict drops",
			row.CausesDroppable)
	}
	if row.CausesDropped != 0 {
		t.Errorf("CausesDropped = %d, want 0: a dry run performed no invalidation, and the observed count must not claim one", row.CausesDropped)
	}
	if row.Withdrawn {
		t.Error("Withdrawn = true, want false: a dry run withdrew nothing")
	}
	// The forecast is a forecast: the cycle is still intact, which is the half a
	// number with no graph behind it gets wrong.
	edges := liveCausesEdges(t, store, newer, older)
	if len(edges) != 2 {
		t.Errorf("live 'causes' edges = %v, want both directions: a dry run must not change the graph", edges)
	}
	// And it is the same verdict the applied run reaches, so the preview is a
	// preview: the applied pass on the untouched cycle drops exactly one.
	applied := &recordingCauses{}
	ares, aclassified, err := Run(ctx, store, applied, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run (apply): %v", err)
	}
	if len(aclassified) != 1 || aclassified[0].CausesDropped != 1 {
		t.Errorf("the applied run reported %+v, want one row with CausesDropped=1: the forecast has to name the same number the run moves, or it is not forecasting it",
			aclassified)
	}
	if ares.Reclassified != 1 {
		t.Errorf("Reclassified = %d, want 1: a 'causes' cycle answered CAUSES drops a live edge, and a row moved", ares.Reclassified)
	}
}

// mustGetLinks reads one memory's live links or fails the test, so an assertion
// about which edge survived a pass does not quietly pass against an empty read.
func mustGetLinks(t *testing.T, store *memory.Store, id string) []memory.Link {
	t.Helper()
	links, err := store.GetLinks(context.Background(), id)
	if err != nil {
		t.Fatalf("GetLinks(%s): %v", id, err)
	}
	return links
}

// seedCausesCycle writes a pair live in BOTH directions of 'causes', which is the
// state a pass before #823 could leave, and returns the two notes' ids with the
// newer one first (their timestamps are a month apart, so the pair IS
// judgeable — the shape whose repair #823 settled).
func seedCausesCycle(t *testing.T, store *memory.Store, db *sql.DB) (newer, older string) {
	t.Helper()
	newer = add(t, store, db, "the restore is being rewritten to run on one spindle", []float32{1, 0, 0}, "2026-09-01 00:00:00")
	older = add(t, store, db, "the restore path on one spindle is safe and fast", []float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")
	for _, dir := range [][2]string{{older, newer}, {newer, older}} {
		if err := store.CreateLinkJudged(context.Background(), dir[0], dir[1], string(RelationCauses), 0.9, "llm", "2020-01-01 00:00:00"); err != nil {
			t.Fatal(err)
		}
	}
	return newer, older
}

// TestADenyingVerdictDropsBothOfACausesCyclesEdges is the review finding that the
// first cut of this change missed: the both-directions sweep existed only on the
// AFFIRMING branch.
//
// A 'causes' cycle is settled by an affirming verdict because the guarded write
// in front of it closes the cycle. A DENYING verdict writes nothing, so the sweep
// is the only thing that can, and stopping it at one direction left the pair's
// other edge live: the graph went on asserting that each of the pair's notes
// caused the other, while `Result.ReclassifiedNoWrite` and the row's `already
// gone` marker both said the pair was unlinked.
func TestADenyingVerdictDropsBothOfACausesCyclesEdges(t *testing.T) {
	for _, tc := range []struct {
		name    string
		verdict Relation
		// live is what the graph must hold afterwards: a NEITHER or a REVERSED
		// leaves nothing, and both are the reason the count matters.
		live int
	}{
		{name: "neither", verdict: RelationNeither, live: 0},
		{name: "reversed", verdict: RelationReversed, live: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, db := seed(t)
			ctx := context.Background()
			newer, older := seedCausesCycle(t, store, db)

			cls := &scriptedRelation{relation: tc.verdict}
			res, classified, err := Run(ctx, store, cls, "p", 0.9, true, nil)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(classified) != 1 {
				t.Fatalf("classified = %+v, want one row", classified)
			}
			if got := liveCausesEdges(t, store, newer, older); len(got) != tc.live {
				t.Errorf("live 'causes' edges = %v, want %d: a denying verdict says the pair is no relation at all, and one edge left is a cycle the graph still asserts",
					got, tc.live)
			}
			if got := classified[0].CausesDropped; got != 2 {
				t.Errorf("CausesDropped = %d, want 2: the row has to name both rows the run moved, or the `[+N causes edge dropped]` clause describes half the change", got)
			}
			if !classified[0].Withdrawn {
				t.Error("Withdrawn = false over a run that removed two live edges: the marker above this row is a claim about the graph, and `already gone` is the wrong one")
			}
			if res.Reclassified != 1 || res.ReclassifiedNoWrite != 1 {
				t.Errorf("Reclassified=%d ReclassifiedNoWrite=%d, want 1/1", res.Reclassified, res.ReclassifiedNoWrite)
			}
		})
	}
}

// TestACausesCycleOnAPairHoldingASupersedesEdgeIsJudgedInTheEdgesDirection is the
// second review finding, and it is the #641 shape wearing a 'causes' costume.
//
// `pairDirection` reported "no single stored direction" as soon as a pair's
// 'causes' edges disagreed — before the rule that a 'supersedes' edge's direction
// wins. A store a pre-#823 pass wrote is exactly that: a live 'supersedes' edge
// with a 'causes' cycle beside it, because that writer shipped unguarded. The pair
// then fell into the causes-cycle path, was judged in the TIMESTAMPS' direction,
// and a NEITHER verdict invalidated 'supersedes' in the timestamps' direction —
// which is not where the live edge is, so nothing was withdrawn while the report
// said it was.
func TestACausesCycleOnAPairHoldingASupersedesEdgeIsJudgedInTheEdgesDirection(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	newer, older := seedCausesCycle(t, store, db)
	// The 'supersedes' edge, running the OTHER way round from the timestamps —
	// so the pair has a stored direction that disagrees with them, and a verdict
	// read in the timestamps' direction is asked about an edge that is not there.
	if err := store.CreateLinkJudged(ctx, older, newer, string(RelationSupersedes), 0.9, "llm", "2020-01-01 00:00:00"); err != nil {
		t.Fatal(err)
	}

	cls := &supersedesEverything{}
	_, classified, err := Run(ctx, store, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(cls.judged) != 1 || cls.judged[0] != [2]string{older, newer} {
		t.Fatalf("asked about %v, want exactly [%s %s]: the live 'supersedes' edge decides the pair's direction even when its 'causes' edges disagree, because that is the edge the pass, `--reassess` and resolve's piggyback can all reach",
			cls.judged, older, newer)
	}
	if len(classified) != 1 || classified[0].ReclassifiedFrom != RelationSupersedes {
		t.Fatalf("classified = %+v, want one row whose live edge was 'supersedes'", classified)
	}
	// The verdict re-AFFIRMED that edge and still took the pair's two 'causes'
	// rows, which is the whole of this finding: a supersession denies a 'causes'
	// claim whichever end it is written from, so neither half of the cycle
	// survives a pass that had every reason to end it.
	if got := liveCausesEdges(t, store, newer, older); len(got) != 0 {
		t.Errorf("live 'causes' edges = %v, want none: the pair is now a supersession, and a 'causes' claim in either direction is what that verdict denies", got)
	}
	if got := liveSupersedesEdges(t, store, older, newer); len(got) != 1 || got[0] != [2]string{older, newer} {
		t.Errorf("live supersedes edges = %v, want exactly [%s %s]: the edge the verdict re-affirmed is not the one it drops", got, older, newer)
	}
	if got := classified[0].CausesDropped; got != 2 {
		t.Errorf("CausesDropped = %d, want 2: the row has to name both rows the run moved", got)
	}
}

// TestACausesCycleSettledByACausesVerdictIsCountedAndPrinted is the third
// finding: the row set `CausesDropped` and `Withdrawn`, and the report printed
// it as an ordinary `causes` line.
//
// The relation did not change — the verdict came back CAUSES on a pair whose live
// edge was a 'causes' one — so nothing counted it, and the one graph row the run
// actually deleted was invisible on every surface while the summary read
// "0 reclassified".
func TestACausesCycleSettledByACausesVerdictIsCountedAndPrinted(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	_, _ = seedCausesCycle(t, store, db)

	cls := &recordingCauses{}
	res, classified, err := Run(ctx, store, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(classified) != 1 {
		t.Fatalf("classified = %+v, want one row", classified)
	}
	if res.Reclassified != 1 {
		t.Errorf("Reclassified = %d, want 1: the pair's live claim went from two edges to one, which is the mutation this count has always meant", res.Reclassified)
	}
	if res.ReclassifiedNoWrite != 0 {
		t.Errorf("ReclassifiedNoWrite = %d, want 0: the verdict affirmed a relation and wrote an edge, so the run was not purely destructive", res.ReclassifiedNoWrite)
	}
	if classified[0].CausesDropped != 1 || !classified[0].Withdrawn {
		t.Errorf("row = %+v, want CausesDropped 1 and Withdrawn true: the edge the run deleted has to be on the row the operator reads", classified[0])
	}
	// That the REPORT prints it is the other half, and it is in cmd/ghost:
	// TestSupersedePairLinesNamesTheEdgeACausesCycleRemoved.
}

// scriptedRelation answers one relation to every pair, for the fixtures that need
// a specific verdict rather than a recording classifier.
type scriptedRelation struct{ relation Relation }

func (c *scriptedRelation) ClassifyBatch(_ context.Context, pairs []Candidate) ([]Relation, error) {
	out := make([]Relation, len(pairs))
	for i := range pairs {
		out[i] = c.relation
	}
	return out, nil
}

// TestACausesEdgeCannotDecideTheDirectionASupersedesEdgeIsWrittenIn is a
// BLOCKER, and it is the shape a live 'causes' override creates when it is allowed
// to reach the supersedes relation.
//
// The fixture is the one the review reproduced, and it is deliberately the same
// fixture as TestAPassNeverWritesTheReverseOfALiveCausesEdgeWhenItDoesReAsk with
// the verdict changed:
//
//   - `newer` was created 2026-09, `older` 2026-01, so the CHRONOLOGY is
//     unambiguous;
//   - a live 'causes' edge runs `newer → older`, which asserts the opposite: that
//     the September note is the cause and the January note its effect;
//   - both endpoints moved, so skip-if-unchanged releases the pair;
//   - the classifier answers SUPERSEDES.
//
// Read through the causes override, the pair is asked as (Newer=`older`,
// Older=`newer`) — the prompt labelled the JANUARY note NEWER — and the
// SUPERSEDES branch then writes `supersedes older → newer`. That edge DEMOTES the
// September note, which the store's own ranking reads as the note that replaced
// the January one, and it buries the memory that is actually current. This is
// #641's damage produced by the pass rather than found by it, and it is worse than
// a mislabelled prompt because the write is the thing the harm lands on.
//
// So the causes edge's direction may govern the CAUSES relation and nothing else.
// Where the two disagree, the pair is asked in the direction `orient` gives —
// which is what the pre-#823 pass did for every pair without a live supersedes
// edge — so a SUPERSEDES verdict is written in the direction that can demote a
// note and never in the one that buries it, and the prompt's NEWER label is the
// chronologically newer note whenever the question is a supersedes one.
//
// A live SUPERSEDES edge keeps its own override, deliberately and for the reason
// #641's repair needs: that edge IS the demotion, so the direction to judge it in
// is the direction it was written in, and a REVERSED answer is how the model
// declines a backwards claim.
func TestACausesEdgeCannotDecideTheDirectionASupersedesEdgeIsWrittenIn(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	newer := add(t, store, db, "the restore is being rewritten to run on one spindle", []float32{1, 0, 0}, "2026-09-01 00:00:00")
	older := add(t, store, db, "the restore path on one spindle is safe and fast", []float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")
	// The live 'causes' edge, running the way the timestamps do NOT.
	if err := store.CreateLinkJudged(ctx, newer, older, string(RelationCauses), 0.9, "llm", "2020-01-01 00:00:00"); err != nil {
		t.Fatal(err)
	}
	// Both endpoints move, so nothing about the quiet can be the reason the
	// assertions below hold.
	retagBoth(t, store, newer, older)

	cls := &recordingDirections{}
	_, classified, err := Run(ctx, store, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(cls.asked) != 1 {
		t.Fatalf("asked about %d pair(s) %v, want exactly 1", len(cls.asked), cls.asked)
	}
	// The question, and the label on it. A supersedes question is asked with the
	// chronologically newer note as NEWER, so the answer is a direction the pass
	// can act on.
	if cls.asked[0] != [2]string{newer, older} {
		t.Errorf("asked about %v, want [%s %s]: a 'causes' edge's direction may not decide a supersedes question, and the pair is asked by the timestamps instead",
			cls.asked[0], newer, older)
	}
	// And what was written, read from the graph rather than from the pass's own
	// counters: no edge in the direction that buries the September note.
	edges := liveSupersedesEdges(t, store, newer, older)
	if len(edges) != 1 || edges[0] != [2]string{newer, older} {
		t.Errorf("live supersedes edges = %v, want exactly [%s %s]: the January note must not supersede the September one — that edge demotes the memory that is actually current (#641)",
			edges, newer, older)
	}
	if len(classified) != 1 || classified[0].NewerID != newer {
		t.Errorf("classified = %+v, want one row whose NEWER endpoint is the September note", classified)
	}
}

// recordingDirections answers SUPERSEDES to every pair and records each
// orientation it was handed, which is the only way to see what the PROMPT said as
// well as what the graph ended up holding. The two are different claims and the
// blocker is about both.
type recordingDirections struct{ asked [][2]string }

func (c *recordingDirections) ClassifyBatch(_ context.Context, pairs []Candidate) ([]Relation, error) {
	out := make([]Relation, len(pairs))
	for i, p := range pairs {
		c.asked = append(c.asked, [2]string{p.NewerID, p.OlderID})
		out[i] = RelationSupersedes
	}
	return out, nil
}
