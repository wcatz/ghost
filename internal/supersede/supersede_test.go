package supersede

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// skip-if-unchanged test.
type mockClassifier struct {
	verdict       func(newer, older string) Relation
	calls         []struct{ Newer, Older string }
	batchCalls    int
	lastBatchSize int
}

func (m *mockClassifier) ClassifyBatch(_ context.Context, pairs []Candidate) ([]Relation, error) {
	m.batchCalls++
	m.lastBatchSize = len(pairs)
	out := make([]Relation, len(pairs))
	for i, p := range pairs {
		m.calls = append(m.calls, struct{ Newer, Older string }{p.NewerContent, p.OlderContent})
		out[i] = m.verdict(p.NewerContent, p.OlderContent)
	}
	return out, nil
}

func seed(t *testing.T) (*memory.Store, *sql.DB) {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	t.Cleanup(func() { _ = store.Close() })
	if err := store.EnsureProject(context.Background(), "p", "/tmp/p", "p"); err != nil {
		t.Fatal(err)
	}
	return store, db
}

func add(t *testing.T, store *memory.Store, db *sql.DB, content string, vec []float32, createdAt string) string {
	t.Helper()
	return addStamped(t, store, db, content, vec, createdAt, createdAt)
}

// #641 fixtures whose newer endpoint (by updated_at) was written earlier.
func addStamped(t *testing.T, store *memory.Store, db *sql.DB, content string, vec []float32, createdAt, updatedAt string) string {
	t.Helper()
	ctx := context.Background()
	id, err := store.Create(ctx, "p", memory.Memory{Category: "fact", Content: content, Importance: 0.7, Source: "mcp"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StoreEmbedding(ctx, id, vec, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE memories SET created_at = ?, updated_at = ? WHERE id = ?`, createdAt, updatedAt, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSelectCandidates(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	// Two similar memories (same subject) + one unrelated.
	newer := add(t, store, db, "postgres upgraded to 16", []float32{1, 0, 0}, "2026-07-10 00:00:00")
	older := add(t, store, db, "postgres runs version 14", []float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")
	_ = add(t, store, db, "grafana listens on port 80", []float32{0, 0, 1}, "2026-06-01 00:00:00")

	sel, err := SelectCandidates(ctx, store, "p", 0.9)
	if err != nil {
		t.Fatalf("SelectCandidates: %v", err)
	}
	if len(sel.Candidates) != 1 {
		t.Fatalf("want exactly 1 candidate (the postgres pair), got %d: %+v", len(sel.Candidates), sel.Candidates)
	}
	if sel.Unscored != 0 {
		t.Errorf("Unscored = %d over a corpus where every memory has a vector", sel.Unscored)
	}
	c := sel.Candidates[0]
	if c.NewerID != newer || c.OlderID != older {
		t.Errorf("wrong orientation: newer=%s older=%s (want newer=%s older=%s)", c.NewerID, c.OlderID, newer, older)
	}
	// the older creation (#641).
	if c.NewerCreatedAt != "2026-07-10 00:00:00" || c.OlderCreatedAt != "2026-01-01 00:00:00" {
		t.Errorf("candidate created_at = (%q, %q), want (2026-07-10 00:00:00, 2026-01-01 00:00:00)", c.NewerCreatedAt, c.OlderCreatedAt)
	}
}

// how #716 looked like a fixture problem for as long as it did.
//
// note that was never a candidate anyway.

func TestRunIdempotent(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	newer := add(t, store, db, "go version is 1.26", []float32{1, 0, 0}, "2026-07-01 00:00:00")
	older := add(t, store, db, "go version is 1.24", []float32{0.99, 0, 0}, "2026-01-01 00:00:00")
	cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationSupersedes }}

	r1, _, err := Run(ctx, store, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	// held quiet rather than re-asked (#787), which is the difference between a
	// batch of eight over the whole graph.
	cls2 := &mockClassifier{verdict: func(_, _ string) Relation { return RelationSupersedes }}
	r2, _, err := Run(ctx, store, cls2, "p", 0.9, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Created != 1 {
		t.Errorf("first pass created %d link(s), want 1", r1.Created)
	}
	if r2.Created != 0 || cls2.batchCalls != 0 {
		t.Errorf("second pass created %d link(s) over %d classify call(s), want 0/0: nothing changed since the edge was written, so there is nothing to decide",
			r2.Created, cls2.batchCalls)
	}
	if pairs, _ := store.SupersedesWithin(ctx, []string{newer, older}); len(pairs) != 1 {
		t.Errorf("live supersedes edges = %v, want the one the first pass wrote: a quiet pass is quiet, not a withdrawal", pairs)
	}
}

func TestRunCausesVerdictWritesCausesLinkOlderToNewer(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	newer := add(t, store, db, "decision: reversed the NATS-first decision back to Postgres LISTEN/NOTIFY", []float32{1, 0, 0}, "2026-07-01 00:00:00")
	older := add(t, store, db, "gotcha: NATS delivers at-least-once and can reorder messages under partition rebalance", []float32{0.99, 0, 0}, "2026-01-01 00:00:00")

	cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationCauses }}
	res, classified, err := Run(ctx, store, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.CausesCreated != 1 {
		t.Fatalf("want CausesCreated=1, got %d", res.CausesCreated)
	}
	if len(classified) != 1 || classified[0].Relation != RelationCauses {
		t.Fatalf("want 1 classified pair with RelationCauses, got %+v", classified)
	}

	links, err := store.GetLinks(ctx, older)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	found := false
	for _, l := range links {
		if l.Relation == "causes" {
			found = true
			if l.SourceID != older || l.TargetID != newer {
				t.Errorf("causes link direction wrong: source=%s target=%s (want source=%s target=%s, i.e. older->newer)", l.SourceID, l.TargetID, older, newer)
			}
		}
	}
	if !found {
		t.Error("expected a causes link, found none")
	}

	pairs, _ := store.SupersedesWithin(ctx, []string{newer, older})
	if len(pairs) != 0 {
		t.Errorf("CAUSES verdict must not write a supersedes link, found %d", len(pairs))
	}
}

// TestRunReclassifiesExistingSupersedesToCauses is the #823 flip, and since #845
// really is written beside it. That is the whole of what #845 withholds and it is
// graph history, and a pass that creates links does not delete them (#845). The
// the one the current rules no longer support.
//
// flapping between the two relations.
func TestRunReclassifiesExistingSupersedesToCauses(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	newer := add(t, store, db, "decision: replaced NATS with Postgres LISTEN/NOTIFY", []float32{1, 0, 0}, "2026-07-01 00:00:00")
	older := add(t, store, db, "gotcha: NATS can reorder messages under partition rebalance", []float32{0.99, 0, 0}, "2026-01-01 00:00:00")

	if err := store.CreateLink(ctx, newer, older, "supersedes", 0.95, "llm"); err != nil {
		t.Fatal(err)
	}
	// rather than re-asked (#787) — which is the correct behaviour and would
	backdateLink(t, db, newer, older)

	cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationCauses }}
	res, _, err := Run(ctx, store, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reclassified != 1 {
		t.Errorf("want Reclassified=1, got %d", res.Reclassified)
	}
	if res.WithdrawSuppressed != 1 {
		t.Errorf("want WithdrawSuppressed=1, got %d", res.WithdrawSuppressed)
	}

	pairs, _ := store.SupersedesWithin(ctx, []string{newer, older})
	if len(pairs) != 1 {
		t.Errorf("live supersedes edge(s) = %d, want 1: the ordinary pass reports that withdrawal instead of making it (#845)", len(pairs))
	}

	links, _ := store.GetLinks(ctx, older)
	found := false
	for _, l := range links {
		if l.Relation == "causes" && l.SourceID == older && l.TargetID == newer {
			found = true
		}
	}
	if !found {
		t.Error("expected causes link older->newer after reclassification")
	}
}

func TestRunNeitherInvalidatesExistingCausesLink(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	newer := add(t, store, db, "decision: use gRPC for the service mesh", []float32{1, 0, 0}, "2026-07-01 00:00:00")
	older := add(t, store, db, "note: gRPC requires HTTP/2", []float32{0.99, 0, 0}, "2026-01-01 00:00:00")

	if err := store.CreateLink(ctx, older, newer, "causes", 0.9, "llm"); err != nil {
		t.Fatal(err)
	}
	// REQUIREMENT for this test to reach a verdict at all (#823).
	//
	backdateLink(t, db, older, newer)

	cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationNeither }}
	if _, _, err := Run(ctx, store, cls, "p", 0.9, true, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	links, _ := store.GetLinks(ctx, older)
	for _, l := range links {
		if l.Relation == "causes" {
			t.Errorf("causes link should have been invalidated, still present: %+v", l)
		}
	}
	pairs, _ := store.SupersedesWithin(ctx, []string{newer, older})
	if len(pairs) != 0 {
		t.Error("NEITHER must not create a supersedes link")
	}
}

// no supersedes edge is ever in (#787).
func unchangedLiveEdge(t *testing.T, store *memory.Store, newer, older string) {
	t.Helper()
	ctx := context.Background()

	sel, err := SelectCandidates(ctx, store, "p", 0.9)
	if err != nil {
		t.Fatalf("SelectCandidates: %v", err)
	}
	if len(sel.Candidates) != 1 || sel.Candidates[0].NewerID != newer || sel.Candidates[0].OlderID != older {
		t.Fatalf("the scan must re-propose %s→%s for this fixture to be testing what it claims: got %d candidate(s) %+v",
			newer, older, len(sel.Candidates), sel.Candidates)
	}

	if err := store.CreateLink(ctx, newer, older, "supersedes", 0.95, "llm"); err != nil {
		t.Fatal(err)
	}
	links, err := store.LinksByRelationSource(ctx, "p", "supersedes", "llm")
	if err != nil {
		t.Fatalf("LinksByRelationSource: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("live supersedes edges = %d, want 1", len(links))
	}
	mems, err := store.GetByIDs(ctx, []string{newer, older})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	for _, m := range mems {
		if m.UpdatedAt > links[0].CreatedAt {
			t.Fatalf("fixture is not UNCHANGED: memory %s has updated_at=%s past the edge's created_at=%s",
				m.ID, m.UpdatedAt, links[0].CreatedAt)
		}
	}
}

// TestRunSkipsUnchangedLiveEdgeTheScanAlsoProposes is #787's half that costs
func TestRunSkipsUnchangedLiveEdgeTheScanAlsoProposes(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	newer := add(t, store, db, "kubernetes now on 1.31", []float32{1, 0, 0}, "2026-07-01 00:00:00")
	older := add(t, store, db, "kubernetes cluster runs 1.27", []float32{0.99, 0.01, 0}, "2026-01-01 00:00:00")
	unchangedLiveEdge(t, store, newer, older)

	cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationSupersedes }}
	res, _, err := Run(ctx, store, cls, "p", 0.9, false, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cls.batchCalls != 0 {
		t.Errorf("the pass spent %d classify call(s) %v on an edge whose endpoints have not changed: an unchanged edge costs nothing", cls.batchCalls, cls.calls)
	}
	if res.Candidates != 0 {
		t.Errorf("Result.Candidates = %d, want 0: the pair is held quiet before it is a candidate, so a re-run reports no work rather than work it declined to do", res.Candidates)
	}
	if res.OppositeLive != 0 {
		t.Errorf("Result.OppositeLive = %d, want 0: the scan AGREED with the edge, and an agreed-with orientation is not a refused one", res.OppositeLive)
	}
}

// TestRunCannotWithdrawAnUnchangedLiveEdgeByReAskingIt is #787's other half,
// stable run to run (#779), so a pass that re-asks an edge nobody changed
// target stamped resolved_at with no edge holding it.
func TestRunCannotWithdrawAnUnchangedLiveEdgeByReAskingIt(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	newer := add(t, store, db, "api rate limit raised to 500 rps", []float32{1, 0, 0}, "2026-07-01 00:00:00")
	older := add(t, store, db, "api rate limit is 100 rps", []float32{0.99, 0.01, 0}, "2026-01-01 00:00:00")
	unchangedLiveEdge(t, store, newer, older)

	cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationNeither }}
	res, _, err := Run(ctx, store, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cls.batchCalls != 0 {
		t.Errorf("the pass asked the classifier about an unchanged edge %d time(s) %v", cls.batchCalls, cls.calls)
	}
	if res.Reclassified != 0 || res.ReclassifiedNoWrite != 0 {
		t.Errorf("reclassified=%d noWrite=%d, want 0/0: nothing was reclassified, because nothing was asked", res.Reclassified, res.ReclassifiedNoWrite)
	}
	if pairs, _ := store.SupersedesWithin(ctx, []string{newer, older}); len(pairs) != 1 {
		t.Errorf("live supersedes edges = %v, want the one that was already there: the ordinary pass must not re-roll an edge nobody edited", pairs)
	}
}

// TestRunReConfirmingAnEdgeRefreshesItsFreshnessReference is #784. Re-confirming
// PromoteToGlobal) is re-billed on every pass forever.
//
// similar vectors the pair would be held quiet by #787's branch instead and this
// test would pass whether or not the stamp moved.
func TestRunReConfirmingAnEdgeRefreshesItsFreshnessReference(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	newer := add(t, store, db, "reversed the NATS decision back to Postgres LISTEN/NOTIFY", []float32{1, 0, 0}, "2026-07-01 00:00:00")
	older := add(t, store, db, "unrelated gotcha about DNS caching", []float32{0, 1, 0}, "2026-01-01 00:00:00")

	if err := store.CreateLink(ctx, newer, older, "supersedes", 0.95, "llm"); err != nil {
		t.Fatal(err)
	}
	backdateLink(t, db, newer, older)
	if _, err := db.ExecContext(ctx, `UPDATE memories SET updated_at = '2026-07-15 00:00:00' WHERE id = ?`, older); err != nil {
		t.Fatal(err)
	}

	first := &mockClassifier{verdict: func(_, _ string) Relation { return RelationSupersedes }}
	res, _, err := Run(ctx, store, first, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if first.batchCalls != 1 || res.Confirmed != 1 {
		t.Fatalf("the pass answering an endpoint edit: calls=%d confirmed=%d, want 1/1", first.batchCalls, res.Confirmed)
	}

	// about the rollback #787 already covers.
	for pass := 2; pass <= 3; pass++ {
		cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationSupersedes }}
		res, _, err := Run(ctx, store, cls, "p", 0.9, true, nil)
		if err != nil {
			t.Fatalf("Run (pass %d): %v", pass, err)
		}
		if cls.batchCalls != 0 {
			t.Errorf("pass %d spent %d classify call(s) %v on an edge re-confirmed by pass 1: one endpoint edit buys one call, not one per pass", pass, cls.batchCalls, cls.calls)
		}
		if res.Candidates != 0 {
			t.Errorf("pass %d Result.Candidates = %d, want 0: nothing changed since the re-confirm", pass, res.Candidates)
		}
	}

	if _, err := db.ExecContext(ctx, `UPDATE memories SET updated_at = datetime('now', '+1 minute') WHERE id = ?`, older); err != nil {
		t.Fatal(err)
	}
	fourth := &mockClassifier{verdict: func(_, _ string) Relation { return RelationSupersedes }}
	if _, _, err := Run(ctx, store, fourth, "p", 0.9, true, nil); err != nil {
		t.Fatalf("Run (pass 4): %v", err)
	}
	if fourth.batchCalls != 1 {
		t.Errorf("pass 4 spent %d classify call(s), want 1: a further endpoint edit is what buys the next one", fourth.batchCalls)
	}
}

func TestRunAnEditLandingDuringClassificationStillBuysTheNextCall(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	newer := add(t, store, db, "reversed the NATS decision back to Postgres LISTEN/NOTIFY", []float32{1, 0, 0}, "2026-07-01 00:00:00")
	older := add(t, store, db, "unrelated gotcha about DNS caching", []float32{0, 1, 0}, "2026-07-01 00:00:00")

	if err := store.CreateLink(ctx, newer, older, "supersedes", 0.95, "llm"); err != nil {
		t.Fatal(err)
	}
	backdateLink(t, db, newer, older)

	midFlight := &mockClassifier{verdict: func(_, _ string) Relation {
		if _, err := db.ExecContext(ctx,
			`UPDATE memories SET content = ?, updated_at = '2026-07-02 00:00:00' WHERE id = ?`,
			"unrelated gotcha about DNS caching, and it is gone in 1.19", older,
		); err != nil {
			t.Errorf("edit during classification: %v", err)
		}
		return RelationSupersedes
	}}
	if _, _, err := Run(ctx, store, midFlight, "p", 0.9, true, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if midFlight.batchCalls != 1 {
		t.Fatalf("the pass made %d classify call(s), want 1: the reclassify half is armed", midFlight.batchCalls)
	}

	next := &mockClassifier{verdict: func(_, _ string) Relation { return RelationSupersedes }}
	if _, _, err := Run(ctx, store, next, "p", 0.9, true, nil); err != nil {
		t.Fatalf("Run (after the mid-flight edit): %v", err)
	}
	if next.batchCalls != 1 {
		t.Errorf("the pass made %d classify call(s), want 1: an edit landing while the classifier was running must not be covered by the stamp written afterwards",
			next.batchCalls)
	}
}

// falsehood.

// word one line for both.

// may be recorded.

// pair, and keep going.

type mockShortClassifier struct{}

func (mockShortClassifier) ClassifyBatch(context.Context, []Candidate) ([]Relation, error) {
	return nil, nil
}

// fatal.

// calls while still counting it as a candidate.

// be re-classified.

// still classifies each time.

// freeze link self-healing.

// that --apply would write links for it (#649 review).
func TestResultWouldWriteLinks(t *testing.T) {
	cases := []struct {
		name string
		res  Result
		want bool
	}{
		{"nothing found", Result{}, false},
		{"confirmed pair writes a link", Result{Confirmed: 1}, true},
		{"caused pair writes a link", Result{CausesCreated: 1}, true},
		{"reclassified to causes writes a link", Result{Reclassified: 1, CausesCreated: 1}, true},
		{"only a refused reversal", Result{Reversed: 1, Reclassified: 1, ReclassifiedNoWrite: 1}, false},
		{"only a NEITHER reclassification, which only drops the link", Result{Reclassified: 1, ReclassifiedNoWrite: 1}, false},
		{"a fresh refusal, no live link to drop", Result{Reversed: 1}, false},
		{"one no-write reclassification and one real reclassification", Result{Reclassified: 2, Reversed: 1, ReclassifiedNoWrite: 1}, true},
		{"cache hits and unclassified pairs", Result{Skipped: 3, Unclassified: 2}, false},
		// The WRITE counts never appear here (#834). They are 0 in every dry run,
		{"writes alone, no verdicts", Result{Created: 1, CausesWritten: 1}, false},
	}
	for _, c := range cases {
		if got := c.res.WouldWriteLinks(); got != c.want {
			t.Errorf("%s: WouldWriteLinks() = %v, want %v", c.name, got, c.want)
		}
	}
}

// difference (#649 review). Every verdict that leaves Reclassified without
// leaving a link to write is a NEITHER or a reversal.
//
// has to. Since #845 a bare denial over a 'supersedes' edge moves nothing at all
// denial, where the finding is WithdrawSuppressed.
