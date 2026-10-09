package mcpserver

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// ghost_health lists a pinned row a newer row contradicts, and says nothing
// about one it does not (#975).
func TestHealthListsPinnedRowsWithOpenContradictions(t *testing.T) {
	f := newHealthFixture(t)
	if strings.Contains(f.health(t), "Pinned rows with open contradictions") {
		t.Fatalf("a store with no pinned contradiction reports one")
	}

	ctx := context.Background()
	pin, err := f.srv.store.Create(ctx, "abc123", memory.Memory{Category: "fact", Content: "the database is postgres", Source: "manual", Importance: 0.7})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	newer, err := f.srv.store.Create(ctx, "abc123", memory.Memory{Category: "fact", Content: "the database is mysql", Source: "manual", Importance: 0.7})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+f.path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close() //nolint:errcheck
	if _, err := db.Exec(`UPDATE memories SET pinned = 1, updated_at = '2026-01-01 00:00:00' WHERE id = ?`, pin); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if _, err := db.Exec(`UPDATE memories SET updated_at = '2026-06-01 00:00:00' WHERE id = ?`, newer); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	second, err := memory.OpenDB(f.path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer second.Close() //nolint:errcheck
	if err := memory.NewStore(second, nil).CreateLink(ctx, newer, pin, "contradicts", 1, "manual"); err != nil {
		t.Fatalf("link: %v", err)
	}

	report := f.health(t)
	for _, want := range []string{"Pinned rows with open contradictions:** 1", "unpin or update", "contradicted by **" + newer, "the database is mysql"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
}
