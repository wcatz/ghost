package claudeimport

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// newImportStore opens an in-memory store with one project for the import
// tests below.
func newImportStore(t *testing.T, projectID string) (*memory.Store, *slog.Logger) {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store := memory.NewStore(db, logger)
	if err := store.EnsureProject(context.Background(), projectID, "/tmp/test", projectID); err != nil {
		t.Fatal(err)
	}
	return store, logger
}

// duplicateLinkTo reports whether the project holds a row, other than target,
// that carries an active 'duplicate' edge into target.
func duplicateLinkTo(t *testing.T, store *memory.Store, target string) bool {
	t.Helper()
	links, err := store.GetLinks(context.Background(), target)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	for _, l := range links {
		if l.Relation == "duplicate" && l.TargetID == target && l.SourceID != target {
			return true
		}
	}
	return false
}

func memoryByContent(t *testing.T, store *memory.Store, projectID, content string) memory.Memory {
	t.Helper()
	rows, err := store.GetAll(context.Background(), projectID, 1000)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	for _, m := range rows {
		if m.Content == content {
			return m
		}
	}
	t.Fatalf("no row holds %q", content)
	return memory.Memory{}
}

// parsedContent is the text the import stores for a file: ParseMemoryFile adds
// the front matter's name and description as a header, so a test that compares
// rows to files has to read the file the way the import does.
func parsedContent(t *testing.T, path string) string {
	t.Helper()
	content, _, _, skip, err := ParseMemoryFile(path)
	if err != nil || skip {
		t.Fatalf("ParseMemoryFile(%s): skip=%v err=%v", path, skip, err)
	}
	return content
}

const (
	nearDupStored   = "The system uses a 3-layer architecture with memory, orchestrator, and AI layers."
	nearDupImported = "The system uses a 3-layer architecture with memory, orchestrator, and AI layers today."
)

// TestImportFoldsANearDuplicateIntoTheExistingRow pins the behaviour the import
// has always had through Upsert: a Claude file whose text is not byte-identical
// to a stored row, but is a near-duplicate of it, is stored as its own row
// linked to that row as a 'duplicate', and the stored row is strengthened.
// Making the exact-content skip atomic must not turn the import into a plain
// insert that skips the fold.
func TestImportFoldsANearDuplicateIntoTheExistingRow(t *testing.T) {
	const projectID = "test-project"
	store, logger := newImportStore(t, projectID)
	ctx := context.Background()

	seedDir := t.TempDir()
	writeTempFile(t, seedDir, "seed.md", "---\nname: Architecture\ndescription: notes\ntype: project\n---\n"+nearDupStored)
	stored := parsedContent(t, filepath.Join(seedDir, "seed.md"))
	seededID, _, _, err := store.Upsert(ctx, projectID, mapCategory("project"), stored, "manual", 0.5, nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	before := memoryByContent(t, store, projectID, stored)

	memDir := filepath.Join(t.TempDir(), "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTempFile(t, memDir, "project_architecture.md", "---\nname: Architecture\ndescription: notes\ntype: project\n---\n"+nearDupImported)
	incoming := parsedContent(t, filepath.Join(memDir, "project_architecture.md"))

	imported, _, _, _, err := importFromDir(ctx, store, projectID, memDir, logger)
	if err != nil {
		t.Fatalf("importFromDir: %v", err)
	}
	if imported != 1 {
		t.Errorf("imported = %d, want 1 (the near-duplicate is stored as its own row)", imported)
	}
	memoryByContent(t, store, projectID, incoming)

	if !duplicateLinkTo(t, store, seededID) {
		t.Error("the imported near-duplicate is not linked to the stored row as a 'duplicate'")
	}
	after := memoryByContent(t, store, projectID, stored)
	if after.AccessCount <= before.AccessCount {
		t.Errorf("stored row access_count = %d after the fold, want more than %d (the fold strengthens it)", after.AccessCount, before.AccessCount)
	}
}

// TestImportFoldsNearDuplicateFilesWithinOneImport is the same rule inside a
// single run: the second of two near-similar files folds into the row the
// first one just stored.
func TestImportFoldsNearDuplicateFilesWithinOneImport(t *testing.T) {
	const projectID = "test-project"
	store, logger := newImportStore(t, projectID)
	ctx := context.Background()

	memDir := filepath.Join(t.TempDir(), "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// os.ReadDir sorts by name, so a_ is imported before b_.
	writeTempFile(t, memDir, "a_project.md", "---\nname: A\ndescription: notes\ntype: project\n---\n"+nearDupStored)
	writeTempFile(t, memDir, "b_project.md", "---\nname: B\ndescription: notes\ntype: project\n---\n"+nearDupImported)

	imported, _, _, _, err := importFromDir(ctx, store, projectID, memDir, logger)
	if err != nil {
		t.Fatalf("importFromDir: %v", err)
	}
	if imported != 2 {
		t.Errorf("imported = %d, want 2", imported)
	}
	first := memoryByContent(t, store, projectID, parsedContent(t, filepath.Join(memDir, "a_project.md")))
	if !duplicateLinkTo(t, store, first.ID) {
		t.Error("the second, near-similar file is not linked to the first as a 'duplicate'")
	}
}

// TestImportSkipsAByteIdenticalFile keeps the other half of the rule: a file
// whose text is exactly a row the project already holds writes nothing — no
// row, no link, and no strengthening of the stored row.
func TestImportSkipsAByteIdenticalFile(t *testing.T) {
	const projectID = "test-project"
	store, logger := newImportStore(t, projectID)
	ctx := context.Background()

	memDir := filepath.Join(t.TempDir(), "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTempFile(t, memDir, "project_architecture.md", "---\nname: Architecture\ndescription: notes\ntype: project\n---\n"+nearDupStored)
	stored := parsedContent(t, filepath.Join(memDir, "project_architecture.md"))
	seededID, _, _, err := store.Upsert(ctx, projectID, mapCategory("project"), stored, "manual", 0.5, nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	before := memoryByContent(t, store, projectID, stored)

	imported, _, _, _, err := importFromDir(ctx, store, projectID, memDir, logger)
	if err != nil {
		t.Fatalf("importFromDir: %v", err)
	}
	if imported != 0 {
		t.Errorf("imported = %d, want 0", imported)
	}
	if n, err := store.CountMemories(ctx, projectID); err != nil || n != 1 {
		t.Errorf("project holds %d memories (err %v), want 1", n, err)
	}
	if duplicateLinkTo(t, store, seededID) {
		t.Error("a byte-identical file was linked to the stored row instead of skipped")
	}
	after := memoryByContent(t, store, projectID, stored)
	if after.AccessCount != before.AccessCount {
		t.Errorf("stored row access_count moved %d -> %d on a skipped file", before.AccessCount, after.AccessCount)
	}
}
