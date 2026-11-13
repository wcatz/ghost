package supersede

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// supersedesEverything answers SUPERSEDES to every pair, in whichever
type supersedesEverything struct {
	judged [][2]string // every {newer, older} the pass handed the classifier
}

func (c *supersedesEverything) ClassifyBatch(_ context.Context, pairs []Candidate) ([]Relation, error) {
	out := make([]Relation, len(pairs))
	for i, p := range pairs {
		c.judged = append(c.judged, [2]string{p.NewerID, p.OlderID})
		out[i] = RelationSupersedes
	}
	return out, nil
}

// unorderedProjections collapses orientations onto the pair they both name, so
func unorderedProjections(dirs [][2]string) map[pairKey]int {
	out := make(map[pairKey]int, len(dirs))
	for _, d := range dirs {
		out[newPairKey(d[0], d[1])]++
	}
	return out
}

// backdateLink rewinds one edge's own stamp, because CreateLink writes
func backdateLink(t *testing.T, db *sql.DB, source, target string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`UPDATE memory_links SET created_at = '2020-01-01 00:00:00' WHERE source_id = ? AND target_id = ?`,
		source, target,
	); err != nil {
		t.Fatalf("backdate link %s→%s: %v", source, target, err)
	}
}

// liveSupersedesEdges reads the live 'supersedes' edges among ids as the graph
func liveSupersedesEdges(t *testing.T, store *memory.Store, ids ...string) [][2]string {
	t.Helper()
	pairs, err := store.SupersedesWithin(context.Background(), ids)
	if err != nil {
		t.Fatalf("SupersedesWithin: %v", err)
	}
	return pairs
}

// TestRunJudgesOneDirectionForAPairTheScanAndALiveLinkDisagreeAbout is #778's
func TestRunJudgesOneDirectionForAPairTheScanAndALiveLinkDisagreeAbout(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	// The stale note, and the fix that replaced it. updated_at is what orients
	stale := add(t, store, db, "bug: the relay stalls on every consumer rebalance", []float32{1, 0, 0}, "2026-01-01 00:00:00")
	fix := add(t, store, db, "the relay rebalance stall is fixed: pin the consumer", []float32{0.98, 0.02, 0}, "2026-09-01 00:00:00")
	if err := store.CreateLink(ctx, stale, fix, "supersedes", 0.95, "llm"); err != nil {
		t.Fatal(err)
	}
	backdateLink(t, db, stale, fix)

	cls := &supersedesEverything{}
	res, classified, err := Run(ctx, store, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// (1) One pair, one question. Both sources propose this pair in opposite
	// orientations, and the pass may ask about it once.
	got := unorderedProjections(cls.judged)
	if len(cls.judged) != 1 {
		t.Errorf("the pass judged %d pair(s) %v, want exactly 1: a pair is classified at most once per pass, whatever its two sources proposed", len(cls.judged), cls.judged)
	}
	for pair, n := range got {
		if n > 1 {
			t.Errorf("pair %s/%s was put to the classifier %d times in one pass (%v)", pair[0], pair[1], n, cls.judged)
		}
	}
	if res.Candidates != 1 {
		t.Errorf("Result.Candidates = %d, want 1: the scan and the live edge are one pair, not two", res.Candidates)
	}
	// (2) The refusal is COUNTED and NAMED. A pass that declined a proposal and
	if res.OppositeLive != 1 {
		t.Errorf("Result.OppositeLive = %d, want 1: the scan proposed the reverse of a live supersedes edge and the pass refused that orientation", res.OppositeLive)
	}
	// (3) No cycle. The edge that was already live is the one this pass may
	edges := liveSupersedesEdges(t, store, stale, fix)
	if len(edges) != 1 || edges[0] != [2]string{stale, fix} {
		t.Errorf("live supersedes edges = %v, want exactly [%s %s]: a pass must never write both directions of one pair, because a cycle demotes both endpoints",
			edges, stale, fix)
	}
	if len(classified) != 1 {
		t.Fatalf("classified = %d pairs, want 1", len(classified))
	}
	if classified[0].NewerID != stale || classified[0].OlderID != fix {
		t.Errorf("judged orientation = %s→%s, want the live link's %s→%s: the recorded direction is the one the graph asserts, and a reversed verdict is how the pass withdraws a wrong edge",
			classified[0].NewerID, classified[0].OlderID, stale, fix)
	}
}

// TestRunReportsAWrongLiveEdgeWhoseDirectionTheScanContradicts is the pay-off
func TestRunReportsAWrongLiveEdgeWhoseDirectionTheScanContradicts(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	stale := add(t, store, db, "bug: the relay stalls on every consumer rebalance", []float32{1, 0, 0}, "2026-01-01 00:00:00")
	fix := add(t, store, db, "the relay rebalance stall is fixed: pin the consumer", []float32{0.98, 0.02, 0}, "2026-09-01 00:00:00")
	if err := store.CreateLink(ctx, stale, fix, "supersedes", 0.95, "llm"); err != nil {
		t.Fatal(err)
	}
	backdateLink(t, db, stale, fix)

	// A direction-aware model: this pair's replacement runs fix→stale, so the
	// stale→fix orientation the graph holds is reversed, and nothing else is.
	cls := &mockClassifier{verdict: func(newer, older string) Relation {
		if newer == "bug: the relay stalls on every consumer rebalance" {
			return RelationReversed
		}
		return RelationSupersedes
	}}
	res, _, err := Run(ctx, store, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reversed != 1 {
		t.Errorf("Result.Reversed = %d, want 1: the pass must judge the pair in the direction the graph asserts, or the reversal never reaches the wrong edge", res.Reversed)
	}
	if res.WithdrawSuppressed != 1 {
		t.Errorf("Result.WithdrawSuppressed = %d, want 1: the edge the verdict contradicts is still live, so it has to be named somewhere the operator reads it", res.WithdrawSuppressed)
	}
	if edges := liveSupersedesEdges(t, store, stale, fix); len(edges) != 1 {
		t.Errorf("live supersedes edges = %v, want the one reported edge: #845 withdraws it from `--reassess`, not from the ordinary pass", edges)
	}
}

// TestRunRefusesAPairTheGraphAlreadyClaimsInBothDirections covers the state a
func TestRunRefusesAPairTheGraphAlreadyClaimsInBothDirections(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	newer := add(t, store, db, "kubernetes now on 1.31", []float32{1, 0, 0}, "2026-09-01 00:00:00")
	older := add(t, store, db, "kubernetes cluster runs 1.27", []float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")
	for _, dir := range [][2]string{{newer, older}, {older, newer}} {
		if err := store.CreateLink(ctx, dir[0], dir[1], "supersedes", 0.95, "llm"); err != nil {
			t.Fatal(err)
		}
	}
	backdateLink(t, db, newer, older)

	cls := &supersedesEverything{}
	res, classified, err := Run(ctx, store, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Bidirectional != 1 {
		t.Errorf("Result.Bidirectional = %d, want 1: a pair the graph claims in both directions is refused, not judged in either direction", res.Bidirectional)
	}
	if len(cls.judged) != 0 || len(classified) != 0 {
		t.Errorf("the pass spent %d classify slot(s) and returned %d verdict(s) on a pair whose both edges are live, want none: no verdict can repair a cycle, and a billed call may confirm it", len(cls.judged), len(classified))
	}
	if res.Created != 0 || res.Confirmed != 0 {
		t.Errorf("created=%d confirmed=%d, want 0/0: the pass must not write into a pair the graph contradicts itself about", res.Created, res.Confirmed)
	}
	if res.Candidates != 0 {
		t.Errorf("Result.Candidates = %d, want 0: a refused pair is not a candidate", res.Candidates)
	}
	if res.WouldWriteLinks() {
		t.Error("WouldWriteLinks() is true for a pass that refused its only pair, so a dry run would promise a link it does not write")
	}
	// The pre-existing cycle is left exactly as found — this pass creates links
	if edges := liveSupersedesEdges(t, store, newer, older); len(edges) != 2 {
		t.Errorf("live supersedes edges = %v, want the pre-existing 2 untouched: the creation pass does not withdraw graph history", edges)
	}
}

// TestRunRefusesToOrientAPairWhoseTimestampsTie is the bulk-import fixture
func TestRunRefusesToOrientAPairWhoseTimestampsTie(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	// One bulk-import stamp for every row: created_at AND updated_at, because
	// only a tie on both leaves the pass with no chronology to read.
	const bulkStamp = "2026-09-20 09:26:05"
	a := add(t, store, db, "prod db timeout is 30s", []float32{1, 0, 0}, bulkStamp)
	b := add(t, store, db, "prod db timeout is 5s", []float32{0.99, 0.01, 0}, bulkStamp)
	c := add(t, store, db, "prod db timeout was 5s until the pool was resized", []float32{0.98, 0.02, 0}, bulkStamp)

	cls := &supersedesEverything{}
	res, classified, err := Run(ctx, store, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Three memories, all mutually similar: three unordered pairs, and the
	// count is per PAIR, not per sighting (each pair is seen from both
	// endpoints).
	if res.Unoriented != 3 {
		t.Errorf("Result.Unoriented = %d, want 3: one per unordered pair among the three tied rows", res.Unoriented)
	}
	if len(cls.judged) != 0 || len(classified) != 0 {
		t.Errorf("the pass asked about %d tied pair(s) (%v), want none: an unknowable direction must not be paid for and must not be cached", len(cls.judged), cls.judged)
	}
	if res.Candidates != 0 || res.Confirmed != 0 || res.Created != 0 {
		t.Errorf("candidates=%d confirmed=%d created=%d, want 0/0/0 for a corpus of tied timestamps", res.Candidates, res.Confirmed, res.Created)
	}
	if edges := liveSupersedesEdges(t, store, a, b, c); len(edges) != 0 {
		t.Errorf("live supersedes edges = %v, want none: id order is not a chronology", edges)
	}
	checked, err := store.SupersedeChecked(ctx, "p")
	if err != nil {
		t.Fatalf("SupersedeChecked: %v", err)
	}
	if len(checked) != 0 {
		t.Errorf("NEITHER cache = %v, want empty: a pair refused for want of a direction is not a verdict", checked)
	}
}

// TestRunStillReclassifiesATiedPairWhoseEdgeIsLive: the tie refusal is a rule
func TestRunStillReclassifiesATiedPairWhoseEdgeIsLive(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	const bulkStamp = "2026-09-20 09:26:05"
	newer := add(t, store, db, "kubernetes now on 1.31", []float32{1, 0, 0}, bulkStamp)
	older := add(t, store, db, "kubernetes cluster runs 1.27", []float32{0, 1, 0}, bulkStamp)
	if err := store.CreateLink(ctx, newer, older, "supersedes", 0.95, "llm"); err != nil {
		t.Fatal(err)
	}
	backdateLink(t, db, newer, older)

	// Dissimilar vectors, so the scan cannot propose this pair at all and the
	// only thing that can judge it is the live edge.
	cls := &supersedesEverything{}
	res, _, err := Run(ctx, store, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(cls.judged) != 1 {
		t.Errorf("the pass judged %d pair(s), want 1: a live edge carries its own direction and is revalidated whatever the timestamps say", len(cls.judged))
	}
	if res.Created != 1 {
		t.Errorf("Result.Created = %d, want 1: the confirmed edge is re-affirmed", res.Created)
	}
	if res.Unoriented != 0 {
		t.Errorf("Result.Unoriented = %d, want 0: the scan never proposed this pair, so nothing was left unoriented", res.Unoriented)
	}
}

// TestSelectCandidatesOrdersATiedUpdatedAtByCreatedAt pins what a tie on
func TestSelectCandidatesOrdersATiedUpdatedAtByCreatedAt(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	// Every row shares one updated_at — the bulk-import stamp — while
	// created_at spreads out, so updated_at orders nothing and created_at
	// orders everything.
	const sharedUpdatedAt = "2026-09-20 09:26:05"
	created := []string{
		"2026-01-01 00:00:00", "2026-02-01 00:00:00", "2026-03-01 00:00:00",
		"2026-04-01 00:00:00", "2026-05-01 00:00:00", "2026-06-01 00:00:00",
	}
	byCreated := make(map[string]string, len(created))
	for i, c := range created {
		id := addStamped(t, store, db, fmt.Sprintf("bulk row %d", i),
			[]float32{1, float32(i) * 0.001, 0}, c, sharedUpdatedAt)
		byCreated[id] = c
	}

	sel, err := SelectCandidates(ctx, store, "p", 0.0)
	if err != nil {
		t.Fatalf("SelectCandidates: %v", err)
	}
	if len(sel.Candidates) != 15 {
		t.Fatalf("got %d candidate pair(s), want all 15 unordered pairs among the six rows: %+v", len(sel.Candidates), sel.Candidates)
	}
	if sel.Unoriented != 0 {
		t.Errorf("Selection.Unoriented = %d, want 0: created_at orders these pairs", sel.Unoriented)
	}
	for _, c := range sel.Candidates {
		if byCreated[c.NewerID] <= byCreated[c.OlderID] {
			t.Errorf("candidate %s→%s is ordered against created_at (%s vs %s); a tie on updated_at is broken by the chronology, never by the id",
				c.NewerID, c.OlderID, byCreated[c.NewerID], byCreated[c.OlderID])
		}
	}
}

// TestRunKeepsOnePassToOnePairPerDirection states the invariant over a corpus
func TestRunKeepsOnePassToOnePairPerDirection(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	a1 := add(t, store, db, "api rate limit is 100 rps", []float32{1, 0, 0}, "2026-01-01 00:00:00")
	a2 := add(t, store, db, "api rate limit raised to 500 rps", []float32{0.99, 0.01, 0}, "2026-09-01 00:00:00")
	b1 := add(t, store, db, "kubernetes cluster runs 1.27", []float32{0, 1, 0}, "2026-01-01 00:00:00")
	b2 := add(t, store, db, "kubernetes now on 1.31", []float32{0, 0.99, 0.01}, "2026-09-01 00:00:00")
	c1 := add(t, store, db, "grafana listens on port 80", []float32{0, 0, 1}, "2026-01-01 00:00:00")
	c2 := add(t, store, db, "grafana moved to port 3000", []float32{0.01, 0, 0.99}, "2026-09-01 00:00:00")

	// A live edge on one pair, pointing the OTHER way from its timestamps: the
	// #641 shape, kept here so the corpus mixes an ordinary fresh pair, an
	// ordinary reclassify pair and a contradicted one.
	if err := store.CreateLink(ctx, a1, a2, "supersedes", 0.95, "llm"); err != nil {
		t.Fatal(err)
	}
	backdateLink(t, db, a1, a2)

	cls := &supersedesEverything{}
	res, _, err := Run(ctx, store, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for pair, n := range unorderedProjections(cls.judged) {
		if n > 1 {
			t.Errorf("pair %s/%s judged %d times in one pass", pair[0], pair[1], n)
		}
	}
	if res.OppositeLive != 1 {
		t.Errorf("Result.OppositeLive = %d, want 1 (only the pair carrying a live edge)", res.OppositeLive)
	}
	if res.Created > len(cls.judged) {
		t.Errorf("Result.Created = %d over %d judged pair(s): a pass cannot write more edges than it decided on", res.Created, len(cls.judged))
	}
	for _, p := range [][2]string{{a1, a2}, {b1, b2}, {c1, c2}} {
		edges := liveSupersedesEdges(t, store, p[0], p[1])
		if len(edges) > 1 {
			t.Errorf("pair %s holds %v: a cycle demotes both endpoints", fmt.Sprintf("%.8s/%.8s", p[0], p[1]), edges)
		}
	}
}
