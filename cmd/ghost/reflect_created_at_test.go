package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestRunReflectApplyKeepsAgeOfUnchangedMemory is issue #623 at the layer the
// issue was filed against: the real `ghost reflect --apply` path.
//
// cmd/ghost converts each reflection.ReflectMemory to a memory.Memory carrying
// no Source and no Provenance, and hands it to ReplaceNonManual. A memory the
// consolidator left out comes back through that same path via the #549 drop
// guard (RetainGuardedDrops), byte-identically, so it takes the exact-content
// reuse UPDATE — which used to stamp created_at = datetime('now') and
// source = 'reflection' onto it. An omitted stale memory was therefore written
// back looking brand new on every applied reflect: its decay restarted and its
// mcp provenance was gone.
//
// This drives the sqlite consolidation tier against a real database through
// runReflect itself, so the conversion in cmd/ghost/lifecycle.go is part of
// what is under test. runReflect is called directly — it must never re-exec
// os.Args[0] from a test.
func TestRunReflectApplyKeepsAgeOfUnchangedMemory(t *testing.T) {
	dataHome := isolatedLifecycleEnv(t)

	// Two distinct memories, so neither is a near-duplicate of the other (the
	// sqlite tier merges on Jaccard >= 0.5 / full containment) and both come
	// back out of consolidation byte-identical. Both are non-manual, so
	// reflection owns them and the reuse path is the one taken.
	const archContent = "the block producer writes its KES epochs under /var/lib/cardano/kes"
	const gotchaContent = "the cardano-node metrics port is 12798 and must stay off the public interface"

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
		if err := s.EnsureProject(context.Background(), "projy", "/tmp/projy", "projy"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		return s
	}

	s := seed()
	ctx := context.Background()
	archID, err := s.Create(ctx, "projy", memory.Memory{
		Category: "architecture", Content: archContent, Source: "mcp", Importance: 0.6,
	})
	if err != nil {
		t.Fatalf("create architecture memory: %v", err)
	}
	if _, err := s.Create(ctx, "projy", memory.Memory{
		Category: "gotcha", Content: gotchaContent, Source: "mcp", Importance: 0.5,
	}); err != nil {
		t.Fatalf("create gotcha memory: %v", err)
	}
	// Backdate both rows. ReplaceNonManual treats anything created at or after
	// the consolidation round trip's start as a concurrent save and leaves it
	// untouched, and created_at has one-second granularity — so a row created
	// in the same second as the run is preserved as "concurrent" and never
	// reaches the reuse path this test is about.
	if err := execBackdate(dataHome, "projy"); err != nil {
		t.Fatalf("backdate seeded memories: %v", err)
	}
	backdatedRows, err := s.GetByIDs(ctx, []string{archID})
	if err != nil || len(backdatedRows) == 0 {
		t.Fatalf("GetByIDs before reflect: %v", err)
	}
	backdated := backdatedRows[0].CreatedAt
	if err := s.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	origArgs := os.Args
	os.Args = []string{origArgs[0], "reflect", "projy", "--tier", "sqlite", "--apply"}
	defer func() { os.Args = origArgs }()

	runReflect()

	after := seed()
	rows, err := after.GetByIDs(ctx, []string{archID})
	if err != nil || len(rows) == 0 {
		t.Fatalf("GetByIDs after reflect --apply: %v", err)
	}
	got := rows[0]
	if got.Content != archContent {
		t.Fatalf("content = %q, want the re-emitted memory unchanged", got.Content)
	}
	if got.CreatedAt != backdated {
		t.Errorf("created_at = %q, want the pre-reflect %q — `ghost reflect --apply` re-stamped an "+
			"unchanged memory as new, so its decay restarts on every apply", got.CreatedAt, backdated)
	}
	if got.Source != "mcp" {
		t.Errorf("source = %q, want mcp — a memory reflection did not rewrite keeps its provenance", got.Source)
	}
	if err := after.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}
