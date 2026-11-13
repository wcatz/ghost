package supersede

import (
	"context"
	"database/sql"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// This file is #823: what a live 'causes' edge does to the pass, and what it does

// recordingCauses answers CAUSES to every pair and records every orientation it
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

// retagStamp is the ONE updated_at every retagged endpoint carries here. It is
const retagStamp = "2026-10-01 00:00:00"

// retagBoth moves both endpoints' updated_at through the REAL writer, which is
func retagBoth(t *testing.T, store *memory.Store, db *sql.DB, ids ...string) {
	t.Helper()
	ctx := context.Background()
	for _, id := range ids {
		if err := store.UpdateMemory(ctx, "p", id, nil, nil, nil, []string{"retagged-mid-pass"}); err != nil {
			t.Fatalf("retag %s: %v", id, err)
		}
	}
	for _, id := range ids {
		if _, err := db.ExecContext(ctx, `UPDATE memories SET updated_at = ? WHERE id = ?`, retagStamp, id); err != nil {
			t.Fatalf("pin updated_at %s: %v", id, err)
		}
	}
}

// TestALiveCausesEdgeHoldsItsPairQuiet is the ordinary pass's half, and it is the
func TestALiveCausesEdgeHoldsItsPairQuiet(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	newer := add(t, store, db, "the restore is being rewritten to run on one spindle", []float32{1, 0, 0}, "2026-09-01 00:00:00")
	older := add(t, store, db, "the restore path on one spindle is safe and fast", []float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")
	// A live 'causes' edge running NEWER→OLDER, which is the reverse of the
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
	retagBoth(t, store, db, newer, older)

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
	// Both causes counts, and here they must agree: the pass swept the edge that
	// disagreed and then wrote its own, so this is a verdict that became a write
	// with no refusal in between (#834's counters, on the path where they match).
	if res.CausesCreated != 1 || res.CausesWritten != 1 {
		t.Errorf("CausesCreated/CausesWritten = %d/%d, want 1/1: the verdict's own edge replaced the one it contradicted, so the write landed",
			res.CausesCreated, res.CausesWritten)
	}
}

// TestACausesVerdictReAffirmsALiveCausesEdgeThatAgreesWithTheTimestamps is the
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
	retagBoth(t, store, db, newer, older)

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
	// And the WRITE count moves with it (#834). The two agree here because the
	if res.CausesWritten != 1 {
		t.Errorf("CausesWritten = %d, want 1: the verdict was written, and a write count that did not follow the write is the #834 asymmetry again", res.CausesWritten)
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
	retagBoth(t, store, db, newer, older)

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
	if err := store.MarkSupersedeNeither(ctx, "p", map[[2]string]memory.SupersedeCheck{
		{newer, older}: {NewerHash: contentHash(newerText), OlderHash: contentHash(olderText)},
		{older, newer}: {NewerHash: contentHash(olderText), OlderHash: contentHash(newerText)},
	}); err != nil {
		t.Fatalf("MarkSupersedeNeither: %v", err)
	}
	// Both endpoints have moved since, so nothing but the cache could hold this
	// pair quiet.
	retagBoth(t, store, db, newer, older)

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
	retagBoth(t, store, db, newer, older)

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
type recordingDirections struct{ asked [][2]string }

func (c *recordingDirections) ClassifyBatch(_ context.Context, pairs []Candidate) ([]Relation, error) {
	out := make([]Relation, len(pairs))
	for i, p := range pairs {
		c.asked = append(c.asked, [2]string{p.NewerID, p.OlderID})
		out[i] = RelationSupersedes
	}
	return out, nil
}

// TestATiedCausesOnlyPairTheScanAlsoFoundIsCountedOnce is a report-counting
// defect, and the only shape in this file that is about the total rather than
func TestATiedCausesOnlyPairTheScanAlsoFoundIsCountedOnce(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	// Two near neighbours sharing BOTH timestamps — a bulk import's stamp, so
	tied := add(t, store, db, "the restore is being rewritten to run on one spindle", []float32{1, 0, 0, 0}, "2026-09-20 09:26:05")
	tiedOther := add(t, store, db, "the restore path on one spindle is safe and fast", []float32{0.99, 0, 0.14, 0}, "2026-09-20 09:26:05")
	if err := store.CreateLinkJudged(ctx, tied, tiedOther, string(RelationCauses), 0.9, "llm", "2020-01-01 00:00:00"); err != nil {
		t.Fatal(err)
	}
	// The second tied causes-only pair sits in its own direction, so the scan
	// cannot see it as a near neighbour at all and only its live edge reaches
	// it — which is why the count is still owed for a pair the scan never did.
	hidden := add(t, store, db, "an unrelated note about the ingest queue", []float32{0, 1, 0, 0}, "2026-09-20 09:26:05")
	hiddenOther := add(t, store, db, "another unrelated note about the deploy queue", []float32{0, 0.99, 0, 0.14}, "2026-09-20 09:26:05")
	if err := store.CreateLinkJudged(ctx, hidden, hiddenOther, string(RelationCauses), 0.9, "llm", "2020-01-01 00:00:00"); err != nil {
		t.Fatal(err)
	}

	cls := &recordingCauses{}
	res, _, err := Run(ctx, store, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(cls.judged) != 0 {
		t.Fatalf("the pass asked about %v, want nothing: two rows sharing both timestamps have no direction to ask in", cls.judged)
	}
	if res.Unoriented != 2 {
		t.Errorf("Unoriented = %d, want 2: two tied causes-only pairs, one of them the scan also found as a near neighbour and one of them reachable only through its live edge. A pair the scan refused is not also refused by the reconciliation.",
			res.Unoriented)
	}
}
