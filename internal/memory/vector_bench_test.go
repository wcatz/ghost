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

// skipSlowCorpus skips a benchmark case whose corpus is the slow part under
// -short. The 50k corpus is 153 MiB of embeddings seeded in one transaction,
// which is seconds of setup for a measurement that is not what a short run
// wants to pay for.
func skipSlowCorpus(b *testing.B, n int) {
	if testing.Short() && n >= 50000 {
		b.Skip("building the 50k corpus is the slow part; run without -short to measure it")
	}
}

// benchCorpus builds a deterministic store of n embedded memories. The seed is
// fixed so every run of the benchmark scans byte-identical vectors, which is
// what makes a before/after comparison of the numbers meaningful: the only
// thing that changed between them is the search implementation.
//
// The corpus is seeded through one transaction of raw SQL rather than a Create
// and a StoreEmbedding per row. At 50k the per-row path is minutes of setup for
// a benchmark that measures the search; the rows, the FTS index (the memories
// triggers still fire) and the embeddings are the same either way, and the
// vector_epoch the embedding triggers bump is irrelevant here because the
// searches below take the uncached path against a corpus that big.
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		tb.Fatalf("begin seed: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck
	for i := range n {
		// A small positive bias on the first few coordinates gives the corpus a
		// realistic cluster structure, so the top-k has something to find
		// instead of every cosine landing on the same value.
		for d := range vec {
			vec[d] = float32(rng.NormFloat64())
		}
		vec[0] += 1
		vec[1] += 1

		var id string
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO memories (project_id, category, content, source)
			VALUES (?, 'fact', ?, 'manual') RETURNING id`,
			testProject, fmt.Sprintf("benchmark memory %d", i)).Scan(&id); err != nil {
			tb.Fatalf("seed memory %d: %v", i, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO memory_embeddings (memory_id, embedding, model) VALUES (?, ?, ?)`,
			id, float32sToBytes(vec), "bench-model"); err != nil {
			tb.Fatalf("seed embedding %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatalf("commit seed: %v", err)
	}
	return s, vec
}

// BenchmarkSearchVector measures one vector query at 1k, 10k and 50k memories.
// It is the measurement behind issue #556: the brute-force scan held the store's
// read lock — and the single SQLite connection — for the whole corpus, decoded
// every embedding into a fresh slice, and fully sorted every positive cosine.
// All three costs scale with the corpus rather than with the result window, so
// the numbers here are the ones that decide whether the search belongs in the
// supersede pass, which runs it once per memory. The 50k case is the one measured
// for #1036: it is above the vector cache's bound, so it takes the uncached path
// whose scratch buffer is dropped on return — the search whose per-call cost this
// benchmark exists to move.
//
// The corpus is a temp dir and the vectors are generated, so nothing here reads
// or writes a real Ghost store. The 50k case is skipped under -short: building
// its corpus is the slow part, not the search.
func BenchmarkSearchVector(b *testing.B) {
	for _, n := range []int{1000, 10000, 50000} {
		b.Run(fmt.Sprintf("memories=%d", n), func(b *testing.B) {
			skipSlowCorpus(b, n)
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

// BenchmarkSearchHybrid measures one hybrid search (keyword leg, vector leg and
// fusion) at 1k, 10k and 50k memories. It is the measurement behind #1031: the row
// load of the vector leg was most of a hybrid search, and every search
// re-read the whole corpus from SQLite. The FTS query matches every row of the
// corpus (all contents share its words), which is the worst case for the
// keyword leg and does not change what the vector leg costs. The 50k case is
// above the vector cache's bound, so the vector leg is the uncached path #1036
// is about, and the hybrid numbers are the ones the issue measured.
//
// The corpus is a temp dir and the vectors are generated, so nothing here reads
// or writes a real Ghost store. The 50k case is skipped under -short: building
// its corpus is the slow part, not the search.
func BenchmarkSearchHybrid(b *testing.B) {
	for _, n := range []int{1000, 10000, 50000} {
		b.Run(fmt.Sprintf("memories=%d", n), func(b *testing.B) {
			skipSlowCorpus(b, n)
			s, query := benchCorpus(b, n)

			ctx := context.Background()
			if _, err := s.SearchHybrid(ctx, testProject, "benchmark memory", query, 10); err != nil {
				b.Fatalf("SearchHybrid: %v", err)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				res, err := s.SearchHybrid(ctx, testProject, "benchmark memory", query, 10)
				if err != nil {
					b.Fatalf("SearchHybrid: %v", err)
				}
				if len(res) == 0 {
					b.Fatal("SearchHybrid returned no results")
				}
			}
		})
	}
}
