package memory

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"testing"
)

// benchDims is the width of the nomic-embed-text vectors the production store holds.
const benchDims = 768

// skipSlowCorpus skips the 50k case under -short: seeding it is the slow part.
func skipSlowCorpus(b *testing.B, n int) {
	if testing.Short() && n >= 50000 {
		b.Skip("building the 50k corpus is the slow part; run without -short to measure it")
	}
}

// benchCorpus builds a deterministic store of n embedded memories through one
// transaction of raw SQL; the seed is fixed so every run scans the same vectors.
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

	rng := rand.New(rand.NewPCG(1, 2))
	vec := make([]float32, benchDims)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		tb.Fatalf("begin seed: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck
	for i := range n {
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

// BenchmarkSearchVector measures one vector query at 1k, 10k and 50k memories
// (issues #556 and #1036); the 50k case takes the uncached path.
func BenchmarkSearchVector(b *testing.B) {
	for _, n := range []int{1000, 10000, 50000} {
		b.Run(fmt.Sprintf("memories=%d", n), func(b *testing.B) {
			skipSlowCorpus(b, n)
			s, query := benchCorpus(b, n)

			ctx := context.Background()
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
// fusion) at 1k, 10k and 50k memories (#1031). The FTS query matches every row.
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
