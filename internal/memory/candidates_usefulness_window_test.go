package memory

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	sqlite "modernc.org/sqlite"
)

func countedUsefulnessStore(t *testing.T) (*Store, *usefulnessQueryCounter, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ghost.db")
	seed, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	dsn := "file:" + path + "?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)"
	inner, err := sqlite.NewConnector(dsn)
	if err != nil {
		t.Fatalf("NewConnector: %v", err)
	}
	counter := &usefulnessQueryCounter{inner: inner}
	db := sql.OpenDB(counter)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	s := NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	if err := s.EnsureProject(ctx, testProject, "/tmp/candidates-usefulness-window", "test"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return s, counter, path
}

func plantFiveUsefulnessMemories(t *testing.T, s *Store, path string) []string {
	t.Helper()
	ctx := context.Background()
	ids := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		id, err := s.Create(ctx, testProject, Memory{
			Category:   "fact",
			Content:    fmt.Sprintf("the claim carried by memory %d", i),
			Source:     "manual",
			Importance: 0.7,
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		ids = append(ids, id)
	}
	const planted = "2026-01-01 00:00:00"
	for _, id := range ids {
		if _, err := s.db.Exec(`UPDATE memories SET created_at = ?, updated_at = ? WHERE id = ?`, planted, planted, id); err != nil {
			t.Fatalf("stamp memory %s: %v", id, err)
		}
	}
	db := auditPlant(t, path)
	for i, id := range ids {
		usefulnessVerdict(t, db, testProject, id, VerdictOutcomeContradicted, fmt.Sprintf("ses_%d", i), "2026-09-20 09:00:00")
	}
	return ids
}

func usefulnessReadWindowIDs(t *testing.T, counter *usefulnessQueryCounter) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, args := range usefulnessContentReadArgs(counter) {
		for i, a := range args {
			if i == 0 {
				continue
			}
			if v, ok := a.Value.(string); ok {
				out[v] = true
			}
		}
	}
	return out
}

func TestQueryCandidatesReadsUsefulnessOnlyForTheWindow(t *testing.T) {
	s, counter, path := countedUsefulnessStore(t)
	ids := plantFiveUsefulnessMemories(t, s, path)

	ctx := context.Background()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

	counter.reset()
	set, err := s.Candidates(ctx, CandidateRequest{
		ProjectID: testProject,
		Mode:      ProjectScoped,
		Query:     "claim",
		Condition: CondFTSOnly,
		Params:    DefaultSearchParams(),
		Now:       now,
		Fetch:     Fetch{FTSTopK: 5, VectorTopK: 5, Limit: 1},
	})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(set.Rows) == 0 || len(set.Rows) >= len(ids) {
		t.Fatalf("window size %d, want between 1 and %d", len(set.Rows), len(ids)-1)
	}

	readIDs := usefulnessReadWindowIDs(t, counter)
	for _, c := range set.Rows {
		if !readIDs[c.ID] {
			t.Errorf("window row %s was not read by the usefulness read", c.ID)
		}
	}
	for _, id := range ids {
		inWindow := false
		for _, c := range set.Rows {
			if c.ID == id {
				inWindow = true
				break
			}
		}
		if !inWindow && readIDs[id] {
			t.Errorf("usefulness read touched %s, which is outside the candidate window", id)
		}
	}
	for _, c := range set.Rows {
		if c.Usefulness.Contradicted != 1 {
			t.Errorf("row %s Usefulness = %+v, want Contradicted=1", c.ID, c.Usefulness)
		}
	}
}

func TestPassiveCandidatesReadsUsefulnessOnlyForTheWindow(t *testing.T) {
	s, counter, path := countedUsefulnessStore(t)
	ids := plantFiveUsefulnessMemories(t, s, path)

	ctx := context.Background()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

	counter.reset()
	set, err := s.Candidates(ctx, CandidateRequest{
		ProjectID: testProject,
		Mode:      ProjectScoped,
		Query:     "",
		Condition: CondFTSOnly,
		Params:    DefaultSearchParams(),
		Now:       now,
		Fetch:     Fetch{Limit: 2},
		Passive: []SlicePolicy{{
			Bucket:    testProject,
			Order:     OrderDecay,
			OverFetch: 2,
		}},
	})
	if err != nil {
		t.Fatalf("Candidates passive: %v", err)
	}
	if len(set.Rows) == 0 || len(set.Rows) >= len(ids) {
		t.Fatalf("window size %d, want between 1 and %d", len(set.Rows), len(ids)-1)
	}

	readIDs := usefulnessReadWindowIDs(t, counter)
	for _, c := range set.Rows {
		if !readIDs[c.ID] {
			t.Errorf("window row %s was not read by the usefulness read", c.ID)
		}
	}
	for _, id := range ids {
		inWindow := false
		for _, c := range set.Rows {
			if c.ID == id {
				inWindow = true
				break
			}
		}
		if !inWindow && readIDs[id] {
			t.Errorf("usefulness read touched %s, which is outside the candidate window", id)
		}
	}
	for _, c := range set.Rows {
		if c.Usefulness.Contradicted != 1 {
			t.Errorf("row %s Usefulness = %+v, want Contradicted=1", c.ID, c.Usefulness)
		}
	}
}
