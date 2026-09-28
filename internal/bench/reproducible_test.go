package bench

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// sweepReproGrid is the grid this test sweeps: the one point where the two legs
// are weighted EQUALLY, which is where RRF scores collide often enough for the
// tie-break to decide results (#708). It is one point and not SweepGrid's six
// because this is a corpus-wide test and this package's CI budget is the binding
// constraint on adding one: the budget, and the headroom left in it, are written
// down once in the internal/bench package-map bullet, and a second full sweep
// here costs ~75s of -race. What the other five points
// do not need re-measuring — they were byte-identical across every run precisely
// because they barely tie — and the whole six-point table is compared across two
// processes of the real binary by the e2e suite, which is where a full sweep
// costs 14s rather than 75.
func sweepReproGrid() []memory.SearchParams {
	even := memory.DefaultSearchParams()
	even.VecWeight, even.FTSWeight = 0.5, 0.5
	return []memory.SearchParams{even}
}

// TestSweepReproducesAcrossTwoSeedsInOneProcess: the leg-weight sweep publishes
// four decimals per grid point, so the store it measures has to be a function of
// the corpus and not of the run. It was not, and it took two defects to say so
// (#708). Seed wrote every row through store.Create, whose id came from the
// column's hex(randomblob(16)) default, and the store breaks tied fused scores by
// memory id — deliberately, because that is what makes production results stable.
// Fixing only that left a second one, found by this test failing intermittently
// with the ids already derived: on a query where a keyword rank-1 hit and a vector
// rank-1 hit tie on the fused score EXACTLY (0.5/62 each at equal leg weights),
// the last stage of the ranking re-sorts the window by base × DecayFactor, and
// the factor is per-category and a function of created_at — so a seed that
// straddled a wall-clock second gave one of the pair a younger age and a
// different factor, and the pair swapped places between runs of one binary.
//
// Two stores seeded from ONE corpus, in one process, is the tightest form of that
// claim: everything except the seed is shared — the same binary, the same query
// set, the same fixtures, the same bootstrap seed pair — so any difference between
// the two tables is the seed and nothing else. The two store properties the
// ranking reads are asserted alongside, because those are where the differences
// came from and a reader debugging a moved row should not have to re-derive them.
func TestSweepReproducesAcrossTwoSeedsInOneProcess(t *testing.T) {
	// Parallel: same reasoning as the five corpus-wide tests it joins — this one
	// reads the immutable headline corpus, sets no env var and touches no
	// package-level state, so overlapping it buys wall clock and nothing else.
	t.Parallel()
	ds, vecs := loadTestdataDataset(t)
	ctx := context.Background()

	seed := func() (*memory.Store, *sql.DB, []Query) {
		store, db := newBenchStoreWithDB(t)
		queries, err := Seed(ctx, store, db, ds, vecs)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		return store, db, queries
	}
	storeA, dbA, queriesA := seed()
	storeB, dbB, queriesB := seed()

	// The mechanism, both halves of it. An id drawn at random, or a created_at
	// drawn per row, makes every table below a claim about a store nobody can
	// rebuild. The headline corpus declares no age_days, so the whole corpus must
	// come back on ONE timestamp — which is what makes the decay factor identical
	// across candidates, and therefore inert here.
	if diff := firstDiffering(storeIDs(t, dbA), storeIDs(t, dbB)); diff != "" {
		t.Errorf("the same corpus seeded twice produced different ids, so the sweep measures a different store every run: %s", diff)
	}
	for _, c := range []struct {
		name string
		db   *sql.DB
	}{{"first", dbA}, {"second", dbB}} {
		if got := distinctCreatedAt(t, c.db); len(got) != 1 {
			t.Errorf("the %s store's rows carry %d distinct created_at values (%s), want 1: "+
				"two rows a second apart reorder a pair whose fused scores tie exactly",
				c.name, len(got), strings.Join(got, ", "))
		}
	}

	sweep := func(store *memory.Store, queries []Query) string {
		points, err := Sweep(ctx, store, queries, sweepReproGrid())
		if err != nil {
			t.Fatalf("Sweep: %v", err)
		}
		return FormatSweep(points)
	}
	first, second := sweep(storeA, queriesA), sweep(storeB, queriesB)

	// The affected row has to be IN the table this compares, or the test would
	// keep passing on a grid that stopped covering the point that moved.
	if !strings.Contains(first, "vec=0.50") {
		t.Fatalf("the swept table has no vec=0.50 row, so this test no longer covers the point that was unreproducible:\n%s", first)
	}
	if first != second {
		t.Errorf("two seeds of one corpus produced two different sweep tables\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// storeIDs is every id in the store, in id order, so two stores seeded from one
// corpus are compared as names rather than as an insertion-order accident.
func storeIDs(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query("SELECT id FROM memories ORDER BY id")
	if err != nil {
		t.Fatalf("read ids: %v", err)
	}
	defer rows.Close() //nolint:errcheck
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan id: %v", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read ids: %v", err)
	}
	return out
}

// distinctCreatedAt is every created_at the store holds, deduplicated. The
// headline corpus is the one that declares no ages, so the whole of it has to
// land on one stamp for the decay factor to be the same number for every
// candidate — the property docs/benchmarks.md claims for it and the one a
// second-boundary-straddling seed loop silently broke.
func distinctCreatedAt(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query("SELECT DISTINCT created_at FROM memories ORDER BY created_at")
	if err != nil {
		t.Fatalf("read created_at: %v", err)
	}
	defer rows.Close() //nolint:errcheck
	var out []string
	for rows.Next() {
		var at string
		if err := rows.Scan(&at); err != nil {
			t.Fatalf("scan created_at: %v", err)
		}
		out = append(out, at)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read created_at: %v", err)
	}
	return out
}

// firstDiffering names the first position where two id sets part company, and
// the size of the difference. A 551-row dump is not a failure message anyone
// reads; the question this test answers is whether the sets are equal, and where
// they stopped being equal is the whole of the useful part.
func firstDiffering(a, b []string) string {
	if len(a) != len(b) {
		return fmt.Sprintf("the two stores hold %d and %d rows", len(a), len(b))
	}
	same := 0
	for i := range a {
		if a[i] != b[i] {
			return fmt.Sprintf("%d of %d ids differ; the first is %q against %q", len(a)-same, len(a), a[i], b[i])
		}
		same++
	}
	return ""
}
