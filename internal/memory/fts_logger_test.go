package memory

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestFTSQueryTermsUsesConfiguredLogger_issue1778(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	var defaultBuf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&defaultBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	s := NewStore(db, logger)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "p", "/tmp/p", "p"); err != nil {
		t.Fatal(err)
	}

	// More than 30 terms triggers the dedup path's truncation warning.
	if _, _, _, err := s.Upsert(ctx, "p", "fact", strings.Repeat("foo ", 40), "manual", 0.5, nil); err != nil {
		t.Fatal(err)
	}

	// More than 10 terms triggers the search path's truncation warning.
	if _, err := s.SearchFTS(ctx, "p", strings.Repeat("bar ", 15), 5); err != nil {
		t.Fatal(err)
	}

	// as_of path also extracts terms through ftsQueryTerms.
	future := time.Now().Add(time.Hour)
	req := CandidateRequest{
		ProjectID: "p",
		Mode:      ProjectScoped,
		Query:     strings.Repeat("baz ", 15),
		Now:       time.Now(),
		AsOf:      &future,
		Condition: CondHybrid,
		Fetch:     Fetch{Limit: 5},
	}
	if _, err := s.Candidates(ctx, req); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(buf.String(), "fts query truncated") {
		t.Fatalf("configured logger did not receive the warning:\n%s", buf.String())
	}
	if strings.Contains(defaultBuf.String(), "fts query truncated") {
		t.Fatalf("default logger received the warning:\n%s", defaultBuf.String())
	}
}
