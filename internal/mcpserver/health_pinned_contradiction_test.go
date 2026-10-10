package mcpserver

import (
	"context"
	"database/sql"
	"fmt"
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

// The section is bounded: more than maxHealthPinnedContradictions qualifying
// pinned rows print that many entries, the true total in the heading, and the
// remainder as a count.
func TestHealthBoundsThePinnedContradictionSection(t *testing.T) {
	f := newHealthFixture(t)
	ctx := context.Background()
	// The bound is pinned as a literal: the test must fail if the constant moves.
	const bound = 20
	const n = bound + 2
	db, err := sql.Open("sqlite", "file:"+f.path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close() //nolint:errcheck
	second, err := memory.OpenDB(f.path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer second.Close() //nolint:errcheck
	linker := memory.NewStore(second, nil)
	for i := 0; i < n; i++ {
		pin, err := f.srv.store.Create(ctx, "abc123", memory.Memory{Category: "fact", Content: fmt.Sprintf("pinned claim %02d", i), Source: "manual", Importance: 0.7})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		newer, err := f.srv.store.Create(ctx, "abc123", memory.Memory{Category: "fact", Content: fmt.Sprintf("newer claim %02d", i), Source: "manual", Importance: 0.7})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := db.Exec(`UPDATE memories SET pinned = 1, updated_at = '2026-01-01 00:00:00' WHERE id = ?`, pin); err != nil {
			t.Fatalf("pin: %v", err)
		}
		if _, err := db.Exec(`UPDATE memories SET updated_at = '2026-06-01 00:00:00' WHERE id = ?`, newer); err != nil {
			t.Fatalf("stamp: %v", err)
		}
		if err := linker.CreateLink(ctx, newer, pin, "contradicts", 1, "manual"); err != nil {
			t.Fatalf("link: %v", err)
		}
	}

	report := f.health(t)
	if want := fmt.Sprintf("Pinned rows with open contradictions:** %d", n); !strings.Contains(report, want) {
		t.Errorf("the heading does not carry the true total %d:\n%s", n, report)
	}
	if got := strings.Count(report, "contradicted by **"); got != bound {
		t.Errorf("rendered %d entries, want %d", got, bound)
	}
	if want := fmt.Sprintf("... and %d more", n-bound); !strings.Contains(report, want) {
		t.Errorf("the remainder line %q is missing:\n%s", want, report)
	}
}
