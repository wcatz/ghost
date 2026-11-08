package memory

import (
	"context"
	"database/sql"
	"testing"
)

// TestCountConsolidatableMemoriesAgreesWithIsConsolidatable pins that the SQL
// count uses the same consolidatable predicate as the Go-side IsConsolidatable.
func TestCountConsolidatableMemoriesAgreesWithIsConsolidatable(t *testing.T) {
	dbPath := t.TempDir() + "/ghost.db"
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()
	store := NewStore(db, nil)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/tmp/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	cases := []Memory{
		{Category: "fact", Content: "consolidatable project", Source: "mcp", Importance: 0.5, Retention: "project"},
		{Category: "fact", Content: "consolidatable session", Source: "mcp", Importance: 0.5, Retention: "session"},
		{Category: "fact", Content: "pinned", Source: "mcp", Importance: 0.5, Pinned: true},
		{Category: "fact", Content: "manual", Source: "manual", Importance: 0.5},
		{Category: "fact", Content: "builtin", Source: "builtin", Importance: 0.5},
		{Category: "fact", Content: "persistent", Source: "mcp", Importance: 0.5, Retention: "persistent"},
	}
	for _, m := range cases {
		if _, err := store.Create(ctx, "p1", m); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	id, _ := store.Create(ctx, "p1", Memory{Category: "fact", Content: "resolved", Source: "mcp", Importance: 0.5})
	if _, err := db.ExecContext(ctx, `UPDATE memories SET resolved_at = datetime('now') WHERE id = ?`, id); err != nil {
		t.Fatalf("resolve row: %v", err)
	}

	got, err := store.CountConsolidatableMemories(ctx, "p1")
	if err != nil {
		t.Fatalf("CountConsolidatableMemories: %v", err)
	}
	rows, err := store.GetAll(ctx, "p1", 1000)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	want := 0
	for _, m := range rows {
		if IsConsolidatable(m) {
			want++
		}
	}
	if got != want {
		t.Fatalf("CountConsolidatableMemories = %d, want %d (IsConsolidatable count)", got, want)
	}
	_ = sql.Open
}
