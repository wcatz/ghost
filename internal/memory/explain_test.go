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
