package memory

import (
	"context"
	"testing"
	"time"
)

// A retention tier is the one claim a save can make that no lifecycle pass may
// take back, and the portable artifact is the documented way to move a store
// between machines. These tests pin the round trip end to end: the export
// carries the tier verbatim, an import restores it under --trust-provenance,
// and the default downgrades it to the durable tier exactly as it already
// downgrades source and pin — because an artifact is a file that arrived from
// somewhere, and on its own authority it must not be able to plant a
// keep-forever row.

// TestARestoredSessionRowStaysPrunable: the artifact carries no expiry —
// expires_at is derived for a session save and stated by nobody else — so a
// restore that put the tier back without deriving one would leave a row the
// prune can never reach, which is the growth the tier exists to bound. The
// import derives one, and a prune with the clock past it takes the row.
func TestARestoredSessionRowStaysPrunable(t *testing.T) {
	store := portableTestStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	rec := PortableMemory{
		ID: "m1", ProjectID: "p1", Category: "fact", Content: "one conversation's fact",
		Source: "mcp", Retention: RetentionSession,
		CreatedAt: "2026-01-01 00:00:00", UpdatedAt: "2026-01-01 00:00:00",
	}
	created, _, _, err := store.ImportMemory(ctx, rec, ImportOptions{Apply: true, TrustProvenance: true})
	if err != nil || !created {
		t.Fatalf("ImportMemory = (%v, %v); want created", created, err)
	}
	var expiry string
	if err := store.db.QueryRowContext(ctx,
		`SELECT expires_at FROM memories WHERE id = 'm1'`).Scan(&expiry); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if expiry == "" {
		t.Fatal("a restored session row has no expiry, so no prune can ever reach it")
	}
	at, err := time.Parse("2006-01-02 15:04:05", expiry)
	if err != nil {
		t.Fatalf("expires_at %q is not a stored timestamp: %v", expiry, err)
	}
	report, err := store.PruneSessionMemories(ctx, PruneOptions{
		Now:   at.Add(DefaultSessionGrace).Add(time.Minute),
		Apply: true,
	})
	if err != nil {
		t.Fatalf("PruneSessionMemories: %v", err)
	}
	if len(report.RemovedIDs) != 1 {
		t.Fatalf("prune removed %v, want the restored session row", report.RemovedIDs)
	}
	var n int
	if err := store.db.QueryRowContext(ctx,
		`SELECT count(*) FROM memories WHERE id = 'm1'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("a restored session row survived the prune that its tier promised")
	}
}
