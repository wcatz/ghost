package supersede

import (
	"context"
	"database/sql"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestSelectCandidatesSkipsAPersistentEndpoint: a 'supersedes' edge is the most
// consequential thing this pass writes, because the ranking demotes its target
// and `ghost resolve`'s piggyback stamps resolved_at on it — so a wrong edge
// removes a live memory from every later session (the pass is KEEP-biased for
// exactly that reason). A `persistent` row is the user's own statement that this
// must not happen to it, and the pair is refused here rather than classified and
// then filtered: a classify call costs money and the only thing the verdict could
// do with a persistent endpoint is write the edge.
func TestSelectCandidatesSkipsAPersistentEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name        string
		keepNewer   bool
		keepOlder   bool
		wantPairFor string
	}{
		{name: "persistent target", keepOlder: true},
		{name: "persistent source", keepNewer: true},
		{name: "both endpoints", keepNewer: true, keepOlder: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, db := seed(t)
			ctx := context.Background()

			// Two near-identical notes, which is what the pass proposes an edge
			// between, plus an unrelated row so the store is not a corpus of one
			// pair.
			newer := add(t, store, db, "postgres upgraded to 16", []float32{1, 0, 0}, "2026-07-10 00:00:00")
			older := add(t, store, db, "postgres runs version 14", []float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")
			_ = add(t, store, db, "grafana listens on port 80", []float32{0, 0, 1}, "2026-06-01 00:00:00")

			markPersistent(t, db, map[string]bool{newer: tc.keepNewer, older: tc.keepOlder})

			sel, err := SelectCandidates(ctx, store, "p", 0.9)
			if err != nil {
				t.Fatalf("SelectCandidates: %v", err)
			}
			if len(sel.Candidates) != 0 {
				t.Fatalf("got %d candidate(s) for a pair with a persistent endpoint: %+v", len(sel.Candidates), sel.Candidates)
			}
		})
	}
}

// TestSelectCandidatesStillProposesAnOrdinaryPair: the fixture above would also
// pass if the exemption were implemented by refusing every pair, so the same
// corpus without a persistent row has to yield its edge.
func TestSelectCandidatesStillProposesAnOrdinaryPair(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	newer := add(t, store, db, "postgres upgraded to 16", []float32{1, 0, 0}, "2026-07-10 00:00:00")
	older := add(t, store, db, "postgres runs version 14", []float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")
	_ = add(t, store, db, "grafana listens on port 80", []float32{0, 0, 1}, "2026-06-01 00:00:00")

	sel, err := SelectCandidates(ctx, store, "p", 0.9)
	if err != nil {
		t.Fatalf("SelectCandidates: %v", err)
	}
	if len(sel.Candidates) != 1 {
		t.Fatalf("got %d candidates, want the one postgres pair", len(sel.Candidates))
	}
	if sel.Candidates[0].NewerID != newer || sel.Candidates[0].OlderID != older {
		t.Errorf("pair = (%s,%s), want (%s,%s)", sel.Candidates[0].NewerID, sel.Candidates[0].OlderID, newer, older)
	}
}

// TestRunWritesNoEdgeBesideAPersistentRow: the same refusal has to hold on the
// path that actually writes. SelectCandidates is the proposal; Run is the write,
// and a run that classified the pair anyway would have a confirmed verdict with
// nothing to apply it to.
func TestRunWritesNoEdgeBesideAPersistentRow(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	newer := add(t, store, db, "postgres upgraded to 16", []float32{1, 0, 0}, "2026-07-10 00:00:00")
	older := add(t, store, db, "postgres runs version 14", []float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")
	markPersistent(t, db, map[string]bool{newer: false, older: true})

	cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationSupersedes }}
	res, _, err := Run(ctx, store, cls, "p", 0.9, true, discardLogger())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Candidates != 0 {
		t.Errorf("the pass considered %d candidate(s) with a persistent endpoint, want 0", res.Candidates)
	}
	if cls.batchCalls != 0 {
		t.Errorf("the classifier was called %d time(s) for a pair that must never be proposed", cls.batchCalls)
	}

	var edges int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM memory_links WHERE relation = 'supersedes' AND invalidated_at IS NULL`,
	).Scan(&edges); err != nil {
		t.Fatalf("count supersedes edges: %v", err)
	}
	if edges != 0 {
		t.Errorf("the pass wrote %d supersedes edge(s) beside a persistent row", edges)
	}
}

func markPersistent(t *testing.T, db *sql.DB, which map[string]bool) {
	t.Helper()
	for id, want := range which {
		if !want {
			continue
		}
		if _, err := db.Exec(`UPDATE memories SET retention = 'persistent' WHERE id = ?`, id); err != nil {
			t.Fatalf("mark %s persistent: %v", id, err)
		}
		// Read it back through the store, so the fixture is checked against the
		// hydration the pass actually sees rather than against the column.
		got, err := memory.NewStore(db, discardLogger()).GetByIDs(context.Background(), []string{id})
		if err != nil {
			t.Fatalf("read %s back: %v", id, err)
		}
		if len(got) != 1 || got[0].Retention != memory.RetentionPersistent {
			t.Fatalf("fixture did not take: %s reads %+v", id, got)
		}
	}
}

// TestRunRefusesToReAffirmAnEdgeBesideAPersistentRow: SelectCandidates is where
// the fresh path refuses a pair with a persistent endpoint, but the reclassify
// path does not go through it — it re-proposes an EXISTING edge whenever an
// endpoint changed since the edge was written, and those pairs then pass the one
// filter that is documented as the point every pair passes through. Without the
// refusal there, an edge written before a memory was declared keep-forever is
// billed a classify call and re-affirmed the first time that memory is edited:
// exactly the outcome the exemption exists to prevent, and the one place a caller
// would see it as a cost rather than as a missing protection.
func TestRunRefusesToReAffirmAnEdgeBesideAPersistentRow(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	// An edge written while both rows were ordinary, then one of them declared
	// keep-forever, then that row edited — the three steps that put the pair on the
	// reclassify path.
	newer := add(t, store, db, "postgres upgraded to 16 in staging", []float32{1, 0, 0}, "2026-07-10 00:00:00")
	older := add(t, store, db, "postgres runs version 14 in staging", []float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")
	if err := store.CreateLink(ctx, newer, older, "supersedes", 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	markPersistent(t, db, map[string]bool{older: true})
	// Both stamps, and the order matters: the reclassify path re-proposes a pair
	// only when an endpoint's updated_at is LATER than the edge's created_at, so
	// the edge is backdated and then the row is edited after it. Editing the row
	// to a moment before the edge would leave skip-if-unchanged holding the pair
	// quiet, and the test would pass without ever reaching the refusal.
	if _, err := db.ExecContext(ctx, `UPDATE memory_links SET created_at = '2026-08-01 00:00:00'`); err != nil {
		t.Fatalf("backdate the edge: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE memories SET updated_at = '2026-09-01 00:00:00' WHERE id = ?`, older); err != nil {
		t.Fatalf("edit the persistent endpoint so the pair is re-proposed: %v", err)
	}

	cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationSupersedes }}
	res, _, err := Run(ctx, store, cls, "p", 0.9, true, discardLogger())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Candidates != 0 {
		t.Errorf("the pass considered %d candidate(s) beside a keep-forever endpoint, want 0", res.Candidates)
	}
	if cls.batchCalls != 0 {
		t.Errorf("the classifier was called %d time(s) to re-affirm an edge it must not touch", cls.batchCalls)
	}

	// And the control: the same corpus, the same edit, no keep-forever row, IS
	// re-proposed. Without this the test passes if the refusal is implemented by
	// refusing every reclassify pair.
	store2, db2 := seed(t)
	newer2 := add(t, store2, db2, "postgres upgraded to 16 in staging", []float32{1, 0, 0}, "2026-07-10 00:00:00")
	older2 := add(t, store2, db2, "postgres runs version 14 in staging", []float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")
	if err := store2.CreateLink(ctx, newer2, older2, "supersedes", 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink (control): %v", err)
	}
	if _, err := db2.ExecContext(ctx, `UPDATE memory_links SET created_at = '2026-08-01 00:00:00'`); err != nil {
		t.Fatalf("backdate the control edge: %v", err)
	}
	if _, err := db2.ExecContext(ctx,
		`UPDATE memories SET updated_at = '2026-09-01 00:00:00' WHERE id = ?`, older2); err != nil {
		t.Fatalf("edit the control endpoint: %v", err)
	}
	cls2 := &mockClassifier{verdict: func(_, _ string) Relation { return RelationSupersedes }}
	res2, _, err := Run(ctx, store2, cls2, "p", 0.9, true, discardLogger())
	if err != nil {
		t.Fatalf("Run (control): %v", err)
	}
	if res2.Candidates == 0 {
		t.Error("an ordinary reclassify pair was refused too: the fixture proves nothing about the exemption")
	}
}
