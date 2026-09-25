package memory

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// TestExplainSearchReportsPerLegRanks: when an agent gets bad context it has
// no way to tell whether FTS, the vector leg, RRF, decay or the result window
// is to blame. ExplainSearch has to say which leg matched each candidate and
// how far each signal moved it, using the same pipeline that produced the
// real answer — membership comes from SearchHybrid itself, so an explanation
// can never describe a ranking the search did not produce.
func TestExplainSearchReportsPerLegRanks(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "explain.sqlite"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	s := NewStore(db, nil)
	if err := s.EnsureProject(ctx, testProject, "/tmp/explain", testProject); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	// Seed matching content so the FTS leg has something to surface; with no
	// query vector the vector leg is absent, and the explanation must say so
	// rather than report a zero cosine as if it were a real score.
	for i := 0; i < 4; i++ {
		if _, _, _, err := s.Upsert(ctx, testProject, "fact",
			fmt.Sprintf("helmfile sops deployment secret %d", i), "manual", 0.6, nil); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	ex, err := s.ExplainSearch(ctx, testProject, "helmfile sops deployment", nil, 2)
	if err != nil {
		t.Fatalf("ExplainSearch: %v", err)
	}

	if len(ex.Rows) == 0 {
		t.Fatal("no rows explained; an empty explanation cannot diagnose anything")
	}
	if ex.VectorAvailable {
		t.Error("VectorAvailable = true with a nil query vector")
	}
	for _, r := range ex.Rows {
		if r.VectorRank != -1 {
			t.Errorf("row %s VectorRank = %d, want -1 — no vector leg ran, so any other value invents a match", r.ID, r.VectorRank)
		}
		if r.FTSRank < -1 {
			t.Errorf("row %s FTSRank = %d, want a 0-based rank or -1 for absent", r.ID, r.FTSRank)
		}
		if r.SupersedePenalty < 0 || r.NearDuplicatePenalty < 0 {
			t.Errorf("row %s has a negative penalty (%d/%d)", r.ID, r.SupersedePenalty, r.NearDuplicatePenalty)
		}
	}

	// Membership must agree with the real search: whatever SearchHybrid
	// returns is exactly what the explanation marks included.
	final, err := s.SearchHybrid(ctx, testProject, "helmfile sops deployment", nil, 2)
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	included := 0
	for _, r := range ex.Rows {
		if r.Included {
			included++
		}
	}
	if included != len(final) {
		t.Errorf("explanation marks %d rows included, SearchHybrid returned %d", included, len(final))
	}
}

// TestExplainSearchExcludedRowsCarryAReason: a row that is present but not
// returned is the whole point of the exercise — without a stated reason the
// agent is back to guessing. Demotions in this system are membership-
// preserving, so exclusion can only come from the result window.
func TestExplainSearchExcludedRowsCarryAReason(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "explain-cut.sqlite"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	s := NewStore(db, nil)
	if err := s.EnsureProject(ctx, testProject, "/tmp/explain2", testProject); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	// Five matching memories, window of two: three must be excluded.
	for i := 0; i < 5; i++ {
		if _, _, _, err := s.Upsert(ctx, testProject, "fact",
			"helmfile sops deployment step "+string(rune('a'+i)), "manual", 0.5, nil); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	ex, err := s.ExplainSearch(ctx, testProject, "helmfile sops deployment", nil, 2)
	if err != nil {
		t.Fatalf("ExplainSearch: %v", err)
	}

	// Candidate pool is limit*2, so with 5 matching rows and limit 2 the
	// explanation sees more candidates than it can return.
	included, excluded := 0, 0
	for _, r := range ex.Rows {
		if r.Included {
			included++
			if r.Rank < 1 {
				t.Errorf("included row %s has Rank %d, want a 1-based rank", r.ID, r.Rank)
			}
			continue
		}
		excluded++
		if r.Reason == "" {
			t.Errorf("excluded row %s carries no reason; the agent would have to guess why it is missing", r.ID)
		}
	}
	if included == 0 {
		t.Fatal("nothing marked included — membership is wrong, not just the explanation")
	}
	if excluded == 0 {
		t.Skip("candidate pool fitted inside the window; nothing was excluded to explain")
	}
	for _, r := range ex.Rows {
		if r.Included && r.Reason != "" {
			t.Errorf("included row %s carries an exclusion reason %q", r.ID, r.Reason)
		}
	}
}

// TestExplainFTSOnlyScoreMatchesTheRanking: with no vector matches the search
// ranks on the unweighted keyword base — keywordOnlyParams sets FTSWeight=1 —
// so the fused score is 1/(RRFK+rank+1). Reporting the weighted form instead
// would advertise a number 0.3x the one that actually ranked the results —
// an agent comparing the breakdown to the ordering would find them
// irreconcilable, which is the exact confusion explain exists to remove.
func TestExplainFTSOnlyScoreMatchesTheRanking(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "explain-fts-only.sqlite"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	s := NewStore(db, nil)
	if err := s.EnsureProject(ctx, testProject, "/tmp/ftsonly", testProject); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, _, _, err := s.Upsert(ctx, testProject, "fact",
			fmt.Sprintf("traefik routing rule number %d", i), "manual", 0.5, nil); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	ex, err := s.ExplainSearch(ctx, testProject, "traefik routing", nil, 3)
	if err != nil {
		t.Fatalf("ExplainSearch: %v", err)
	}
	if ex.VectorAvailable {
		t.Fatal("VectorAvailable true for a nil query vector")
	}

	p := DefaultSearchParams()
	for _, r := range ex.Rows {
		if r.FTSRank < 0 {
			continue
		}
		want := 1.0 / float64(p.RRFK+r.FTSRank+1)
		if r.RRFScore != want {
			t.Errorf("row %s FTSRank=%d RRFScore=%v, want %v — the unweighted base decayRank actually used",
				r.ID, r.FTSRank, r.RRFScore, want)
		}
	}
}

// TestExplainNearDuplicatePenaltyIsDeterministic: DemotionPenalties decides
// which member of a near-duplicate pair loses from its position in the ids
// slice (rank[b] > rank[a]). Collecting those ids by ranging over maps lets
// Go randomize the order, so the penalty would land on a different member
// every run — an explanation that disagrees with itself is worse than none.
func TestExplainNearDuplicatePenaltyIsDeterministic(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "explain-det.sqlite"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	s := NewStore(db, nil)
	if err := s.EnsureProject(ctx, testProject, "/tmp/det", testProject); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	// A near-duplicate pair plus an unrelated row, all matching the query,
	// so both pair members land in the returned window.
	pair := "vaultwarden runs behind cloudflare tunnel"
	if _, _, _, err := s.Upsert(ctx, testProject, "fact", pair, "manual", 0.6, nil); err != nil {
		t.Fatalf("seed pair: %v", err)
	}
	if _, _, _, err := s.Upsert(ctx, testProject, "fact", pair, "manual", 0.6, nil); err != nil {
		t.Fatalf("seed duplicate: %v", err)
	}
	if _, _, _, err := s.Upsert(ctx, testProject, "fact", "unrelated row about vaultwarden", "manual", 0.5, nil); err != nil {
		t.Fatalf("seed unrelated: %v", err)
	}

	var baseline map[string]int
	for run := 0; run < 8; run++ {
		ex, err := s.ExplainSearch(ctx, testProject, "vaultwarden cloudflare tunnel", nil, 3)
		if err != nil {
			t.Fatalf("ExplainSearch run %d: %v", run, err)
		}
		got := map[string]int{}
		for _, r := range ex.Rows {
			got[r.ID] = r.NearDuplicatePenalty
		}
		if baseline == nil {
			baseline = got
			continue
		}
		for id, want := range baseline {
			if got[id] != want {
				t.Fatalf("run %d assigned near_duplicate_penalty %d to %s, run 0 assigned %d — the assignment depends on map iteration order",
					run, got[id], id, want)
			}
		}
	}
}
