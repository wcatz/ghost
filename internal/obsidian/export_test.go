package obsidian

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func seedStore(t *testing.T) *memory.Store {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "ghost", "/tmp/ghost", "ghost"); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestExport(t *testing.T) {
	store := seedStore(t)
	ctx := context.Background()
	id1, _ := store.Create(ctx, "ghost", memory.Memory{Category: "gotcha", Content: "First fact about embedding", Importance: 0.8, Source: "mcp"})
	id2, _ := store.Create(ctx, "ghost", memory.Memory{Category: "fact", Content: "Second fact about linking", Importance: 0.7, Source: "mcp"})
	if err := store.CreateLink(ctx, id1, id2, "related", 0.83, "auto"); err != nil {
		t.Fatal(err)
	}
	store.RecordDecision(ctx, "ghost", "Use SQLite", "SQLite it is", "zero infra", []string{"Postgres"}, nil)
	store.CreateTask(ctx, "ghost", "Ship mirror", "Build it", 2)
	// A registered project with no entities must not produce a vault folder.
	if err := store.EnsureProject(ctx, "empty", "/tmp/empty", "empty"); err != nil {
		t.Fatal(err)
	}

	vault := filepath.Join(t.TempDir(), "vault")
	ex := &Exporter{Store: store, Logger: slog.Default()}
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatalf("export: %v", err)
	}

	// RecordDecision also saves a companion memory (decisions.go: "creates a
	// decision and also saves it as a memory"), so 2 explicit Creates + 1
	// companion = 3 memory notes.
	mems, _ := filepath.Glob(filepath.Join(vault, "ghost", "Memories", "*.md"))
	if len(mems) != 3 {
		t.Fatalf("want 3 memory notes (2 created + 1 decision companion), got %d", len(mems))
	}
	// Wikilink present between the two linked memories.
	var all strings.Builder
	for _, f := range mems {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		all.Write(data)
	}
	if !strings.Contains(all.String(), "[[") {
		t.Error("expected a wikilink in Related section")
	}
	decs, _ := filepath.Glob(filepath.Join(vault, "ghost", "Decisions", "*.md"))
	tasks, _ := filepath.Glob(filepath.Join(vault, "ghost", "Tasks", "*.md"))
	if len(decs) != 1 || len(tasks) != 1 {
		t.Fatalf("want 1 decision + 1 task, got %d + %d", len(decs), len(tasks))
	}
	if _, err := os.Stat(filepath.Join(vault, "empty")); !os.IsNotExist(err) {
		t.Errorf("project with no entities should not get a folder: %v", err)
	}

	// Idempotence: second export rewrites nothing (mtimes unchanged).
	before, _ := os.Stat(mems[0])
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(mems[0])
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("unchanged note was rewritten")
	}

	// Deletion prunes.
	if err := store.Delete(ctx, id2); err != nil {
		t.Fatal(err)
	}
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatal(err)
	}
	mems2, _ := filepath.Glob(filepath.Join(vault, "ghost", "Memories", "*.md"))
	if len(mems2) >= len(mems) {
		t.Errorf("deleted memory's note should be pruned: %d -> %d", len(mems), len(mems2))
	}
}

func TestExportProjectFilter(t *testing.T) {
	store := seedStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "widget", "/tmp/widget", "widget"); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatal(err)
	}
	ghostID, _ := store.Create(ctx, "ghost", memory.Memory{Category: "fact", Content: "Ghost project fact", Importance: 0.8, Source: "mcp"})
	widgetID, _ := store.Create(ctx, "widget", memory.Memory{Category: "fact", Content: "Widget project fact", Importance: 0.8, Source: "mcp"})
	if _, err := store.Create(ctx, "_global", memory.Memory{Category: "preference", Content: "Global preference fact", Importance: 0.9, Source: "mcp"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateLink(ctx, ghostID, widgetID, "related", 0.9, "auto"); err != nil {
		t.Fatal(err)
	}

	vault := filepath.Join(t.TempDir(), "vault")
	ex := &Exporter{Store: store, Logger: slog.Default()}
	if err := ex.Export(ctx, vault, "ghost"); err != nil {
		t.Fatalf("export: %v", err)
	}

	// Filtered project exported; _global rides along under "Global".
	ghostMems, _ := filepath.Glob(filepath.Join(vault, "ghost", "Memories", "*.md"))
	if len(ghostMems) != 1 {
		t.Fatalf("want 1 ghost memory note, got %d", len(ghostMems))
	}
	globalMems, _ := filepath.Glob(filepath.Join(vault, "Global", "Memories", "*.md"))
	if len(globalMems) != 1 {
		t.Fatalf("want 1 global memory note under Global/, got %d", len(globalMems))
	}
	// Filtered-out project gets no folder at all.
	if _, err := os.Stat(filepath.Join(vault, "widget")); !os.IsNotExist(err) {
		t.Errorf("filtered-out project should not get a folder: %v", err)
	}
	// Link to a filtered-out memory degrades to a plain short ID, not a wikilink.
	data, err := os.ReadFile(ghostMems[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "[[") {
		t.Error("link to filtered-out project should not be a wikilink")
	}
	if !strings.Contains(string(data), widgetID[:8]) {
		t.Error("link to filtered-out project should degrade to its short ID")
	}

	// _global alone never satisfies a filter.
	if err := ex.Export(ctx, vault, "no-such-project"); err == nil {
		t.Error("expected error for unmatched project filter")
	}
}

func TestExportReclaimsOrphanedTmpFiles(t *testing.T) {
	store := seedStore(t)
	ctx := context.Background()
	if _, err := store.Create(ctx, "ghost", memory.Memory{Category: "fact", Content: "Some fact", Importance: 0.8, Source: "mcp"}); err != nil {
		t.Fatal(err)
	}

	vault := filepath.Join(t.TempDir(), "vault")
	ex := &Exporter{Store: store, Logger: slog.Default()}
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatal(err)
	}

	// Simulate a crashed write: orphaned tmp inside a managed subtree.
	stray := filepath.Join(vault, "ghost", "Memories", "foo.md.ghost-tmp")
	if err := os.WriteFile(stray, []byte("orphan"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A tmp file outside managed subtrees is none of our business.
	outside := filepath.Join(vault, "user-notes", "bar.md.ghost-tmp")
	if err := os.MkdirAll(filepath.Dir(outside), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Errorf("orphaned tmp in managed subtree should be reclaimed: %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("tmp file outside managed subtrees must survive: %v", err)
	}
}

func TestFolderNamesCaseCollision(t *testing.T) {
	ps := []memory.Project{
		{ID: "aaaaaaaa-0000-0000-0000-000000000000", Name: "Foo"},
		{ID: "bbbbbbbb-0000-0000-0000-000000000000", Name: "foo"},
	}
	got := folderNames(ps)
	if got[ps[0].ID] != "Foo" {
		t.Errorf("first project keeps its plain folder, got %q", got[ps[0].ID])
	}
	if got[ps[1].ID] != "foo-bbbbbbbb" {
		t.Errorf("later case-colliding project gets -id8 suffix, got %q", got[ps[1].ID])
	}
	if strings.EqualFold(got[ps[0].ID], got[ps[1].ID]) {
		t.Errorf("folders still collide case-insensitively: %q vs %q", got[ps[0].ID], got[ps[1].ID])
	}
}
