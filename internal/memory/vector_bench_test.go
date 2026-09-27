package memory

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"testing"
)

// benchDims is the width of the nomic-embed-text vectors the production store
// holds (embedding.DefaultModel at 768 dimensions), so the benchmark measures
// the row width a real store scans rather than a toy.
const benchDims = 768

// benchCorpus builds a deterministic store of n embedded memories. The seed is
// fixed so every run of the benchmark scans byte-identical vectors, which is
// what makes a before/after comparison of the numbers meaningful: the only
// thing that changed between them is the search implementation.
func benchCorpus(tb testing.TB, n int) (*Store, []float32) {
	tb.Helper()

	dir := tb.TempDir()
	db, err := OpenDB(dir + "/bench.db")
	if err != nil {
		tb.Fatalf("OpenDB: %v", err)
	}
	tb.Cleanup(func() { _ = db.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewStore(db, logger)

	ctx := context.Background()
	if err := s.EnsureProject(ctx, testProject, dir, "test"); err != nil {
		tb.Fatalf("EnsureProject: %v", err)
	}

	// math/rand/v2 with an explicit source: the same n produces the same
	// vectors on every run and on every machine.
	rng := rand.New(rand.NewPCG(1, 2))
	vec := make([]float32, benchDims)
	for i := range n {
		// A small positive bias on the first few coordinates gives the corpus a
		// realistic cluster structure, so the top-k has something to find
		// instead of every cosine landing on the same value.
		for d := range vec {
			vec[d] = float32(rng.NormFloat64())
		}
		vec[0] += 1
		vec[1] += 1

		id, err := s.Create(ctx, testProject, Memory{
			Category: "fact",
			Content:  fmt.Sprintf("benchmark memory %d", i),
			Source:   "manual",
		})
		if err != nil {
			tb.Fatalf("Create %d: %v", i, err)
		}
		if err := s.StoreEmbedding(ctx, id, vec, "bench-model"); err != nil {
			tb.Fatalf("StoreEmbedding %d: %v", i, err)
		}
	}
	return s, vec
}

// BenchmarkSearchVector measures one vector query at 1k and 10k memories. It is
// the measurement behind issue #556: the brute-force scan held the store's read
// lock — and the single SQLite connection — for the whole corpus, decoded every
// embedding into a fresh slice, and fully sorted every positive cosine. All
// three costs scale with the corpus rather than with the result window, so the
// numbers here are the ones that decide whether the search belongs in the
// supersede pass, which runs it once per memory.
//
// The corpus is a temp dir and the vectors are generated, so nothing here reads
// or writes a real Ghost store.
func BenchmarkSearchVector(b *testing.B) {
	for _, n := range []int{1000, 10000} {
		b.Run(fmt.Sprintf("memories=%d", n), func(b *testing.B) {
			s, query := benchCorpus(b, n)

			ctx := context.Background()
			// Warm the page cache and the store's search state so the first
			// iteration does not pay for setup the steady state never pays.
			if _, err := s.SearchVector(ctx, testProject, query, 10); err != nil {
				b.Fatalf("SearchVector: %v", err)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				res, err := s.SearchVector(ctx, testProject, query, 10)
				if err != nil {
					b.Fatalf("SearchVector: %v", err)
				}
				if len(res) == 0 {
					b.Fatal("SearchVector returned no results")
				}
			}
		})
	}
}
