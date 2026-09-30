package memory

// #646: what one retrieval record costs.
//
// This is a measurement of a write added to a path that already writes — the
// search that assembles a block also has to record what it assembled — so the
// number that matters is not the write in isolation but the write at the table
// size a real store has reached. Two sizes are measured for that reason: an empty
// table pays only the insert, while a table at its cap pays the eviction DELETE
// as well, and a design that is cheap while empty and dear once full is a
// design that gets disabled by whoever notices first.
//
// The verdicts are the shape a real call produces (a handful of kept and
// dropped rows), not an empty array, because an empty array understates the
// serialisation and understates the JSON the purge has to re-parse.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
)

// benchStore is testStore for a benchmark. A file-backed database rather than
// :memory:, because the write being measured takes a BEGIN IMMEDIATE and holds
// SQLite's write lock, and an in-memory store would fold the file's own I/O into
// the number in a way a real store does not.
func benchStore(b *testing.B) *Store {
	b.Helper()
	dir := b.TempDir()
	db, err := OpenDB(dir + "/bench.db")
	if err != nil {
		b.Fatalf("OpenDB: %v", err)
	}
	b.Cleanup(func() { _ = db.Close() })

	s := NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	if err := s.EnsureProject(context.Background(), testProject, dir, "test"); err != nil {
		b.Fatalf("EnsureProject: %v", err)
	}
	return s
}

// benchVerdicts is one call's worth of verdicts: what a search that admitted a
// few rows and dropped a few more actually judges. Not an empty array, because
// an empty array understates the serialisation and understates the JSON a purge
// has to re-parse.
func benchVerdicts() []RowVerdict {
	verdicts := make([]RowVerdict, 0, 8)
	for i := range 4 {
		verdicts = append(verdicts, RowVerdict{
			ID: "KEEP-" + digest(i), Kept: true, Stage: "validity", Reason: "valid",
		}, RowVerdict{
			ID: "DROP-" + digest(i), Kept: false, Stage: "predicates", Reason: "category_mismatch",
		})
	}
	return verdicts
}

func benchRetrievalRecord(b *testing.B, atCap bool) {
	s := benchStore(b)
	ctx := context.Background()

	// A call's worth of verdicts: what a search that admitted a few rows and
	// dropped a few more actually judges.
	rec := RetrievalRecord{
		ProjectID: testProject,
		Source:    "search",
		QueryHash: digest(1),
		Outcome:   "answerable",
		Reason:    "floor_met",
		Verdicts:  benchVerdicts(),
	}

	if atCap {
		// Fill to just under the cap so the measured iterations each cross it and
		// pay the eviction — the steady state once a store has been searched
		// enough times.
		prefill := retrievalRecordRowsCap - 2
		prefillRec := rec
		prefillRec.Verdicts = nil
		for i := range prefill {
			prefillRec.QueryHash = digest(i + 100)
			if err := s.RecordRetrieval(ctx, prefillRec); err != nil {
				b.Fatalf("prefill %d: %v", i, err)
			}
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec.QueryHash = digest(i)
		if err := s.RecordRetrieval(ctx, rec); err != nil {
			b.Fatalf("RecordRetrieval: %v", err)
		}
	}
}

func BenchmarkRecordRetrievalEmptyTable(b *testing.B) { benchRetrievalRecord(b, false) }

// BenchmarkRecordRetrievalAtCap: the same call on a table already at its cap,
// where every append also evicts. The difference between the two numbers is the
// cost of the growth policy, which is the part a cap is a decision about.
func BenchmarkRecordRetrievalAtCap(b *testing.B) { benchRetrievalRecord(b, true) }

// BenchmarkRetrievalRecordsRead: the reader the audit will use, over a table
// full of records, newest first. A limit of 200 is a plausible report window and
// a limit of 10 a plausible dashboard one.
func BenchmarkRetrievalRecordsRead(b *testing.B) {
	s := benchStore(b)
	ctx := context.Background()

	for i := range 2000 {
		rec := RetrievalRecord{
			ProjectID: testProject, Source: "search", QueryHash: digest(i),
			Outcome: "answerable", Reason: "floor_met", Verdicts: benchVerdicts(),
		}
		if err := s.RecordRetrieval(ctx, rec); err != nil {
			b.Fatalf("seed %d: %v", i, err)
		}
	}

	for _, limit := range []int{10, 200} {
		b.Run(fmt.Sprintf("limit=%d", limit), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.RetrievalRecords(ctx, limit); err != nil {
					b.Fatalf("RetrievalRecords: %v", err)
				}
			}
		})
	}
}
