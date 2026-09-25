package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// execBackdate rewrites the project's memory timestamps to an hour ago.
func execBackdate(dataHome, project string) error {
	db, err := sql.Open("sqlite", filepath.Join(dataHome, "ghost", "ghost.db"))
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck
	_, err = db.Exec(`UPDATE memories SET created_at = datetime('now', '-1 hour') WHERE project_id = ?`, project)
	return err
}

// TestRunReflectApplyPreservesScopeOfReusedRow drives the real production path
// the review pointed at: `ghost reflect --apply` builds memory.Memory values from
// reflection.ReflectMemory, which carries no machine-readable scope, and hands
// them to ReplaceNonManual. A project holding a scoped fact therefore reached
// the store with Scope nil, and the reuse UPDATE wrote NULL over the live
// scope.
//
// This runs the sqlite consolidation tier against a real database through
// runReflect itself, so the conversion in cmd/ghost/lifecycle.go is part of what
// is under test — a store-only test cannot see a caller that strips the field
// before it arrives.
func TestRunReflectApplyPreservesScopeOfReusedRow(t *testing.T) {
	dataHome := isolatedLifecycleEnv(t)

	// Two distinct facts, so consolidation has something to work with and
	// neither is a near-duplicate of the other (the sqlite tier merges on
	// Jaccard >= 0.5). Both are non-manual, so reflection owns them and the
	// reused-row path is the one taken.
	const scopedContent = "the production deploy pipeline requires a manual approval gate"
	const otherContent = "unit tests run with the race detector on linux only"

	// bootstrap() creates this on demand; the seed store opens the same file
	// first, so the directory has to exist already.
	if err := os.MkdirAll(filepath.Join(dataHome, "ghost"), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}

	seed := func() *memory.Store {
		t.Helper()
		db, err := memory.OpenDB(filepath.Join(dataHome, "ghost", "ghost.db"))
		if err != nil {
			t.Fatalf("OpenDB: %v", err)
		}
		s := memory.NewStore(db, nil)
		if err := s.EnsureProject(context.Background(), "projx", "/tmp/projx", "projx"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		return s
	}

	s := seed()
	ctx := context.Background()
	scopedID, err := s.Create(ctx, "projx", memory.Memory{
		Category: "fact", Content: scopedContent, Source: "reflection", Importance: 0.6,
		Scope: map[string]string{"environment": "production"},
	})
	if err != nil {
		t.Fatalf("create scoped: %v", err)
	}
	if _, err := s.Create(ctx, "projx", memory.Memory{
		Category: "fact", Content: otherContent, Source: "reflection", Importance: 0.5,
	}); err != nil {
		t.Fatalf("create other: %v", err)
	}
	// Backdate both rows. ReplaceNonManual treats anything created at or after
	// the consolidation round trip's start as a concurrent save and leaves it
	// untouched, and created_at has one-second granularity — so a row created
	// in the same second as the run is preserved as "concurrent" and never
	// reaches the reuse path this test is about. An hour ago is unambiguously
	// part of the corpus reflection is allowed to rewrite.
	if err := execBackdate(dataHome, "projx"); err != nil {
		t.Fatalf("backdate seeded memories: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	origArgs := os.Args
	os.Args = []string{origArgs[0], "reflect", "projx", "--tier", "sqlite", "--apply"}
	defer func() { os.Args = origArgs }()

	runReflect()

	after := seed()
	rows, err := after.GetByIDs(ctx, []string{scopedID})
	if err != nil || len(rows) == 0 {
		t.Fatalf("GetByIDs after reflect --apply: %v", err)
	}
	if rows[0].Content != scopedContent {
		t.Fatalf("content = %q, want the re-emitted fact unchanged", rows[0].Content)
	}
	if rows[0].Scope["environment"] != "production" {
		t.Errorf("scope = %v, want environment=production — `ghost reflect --apply` supplies no scope, "+
			"so the reused row must keep the one it had", rows[0].Scope)
	}
	if err := after.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}
