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

			cands, err := SelectCandidates(ctx, store, "p", 0.9)
			if err != nil {
				t.Fatalf("SelectCandidates: %v", err)
			}
			if len(cands) != 0 {
				t.Fatalf("got %d candidate(s) for a pair with a persistent endpoint: %+v", len(cands), cands)
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

	cands, err := SelectCandidates(ctx, store, "p", 0.9)
	if err != nil {
		t.Fatalf("SelectCandidates: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want the one postgres pair", len(cands))
	}
	if cands[0].NewerID != newer || cands[0].OlderID != older {
		t.Errorf("pair = (%s,%s), want (%s,%s)", cands[0].NewerID, cands[0].OlderID, newer, older)
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
