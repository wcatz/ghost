package claudeimport

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// duplicateLinkTo reports whether another row carries an active 'duplicate'
// edge into target.
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

// parsedContent is the text the import stores for a file, header included.
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

// nearDupFile writes a project-type memory file into dir.
func nearDupFile(t *testing.T, dir, file, name, body string) {
	t.Helper()
	writeTempFile(t, dir, file, "---\nname: "+name+"\ndescription: notes\ntype: project\n---\n"+body)
}

func newMemDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "memory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A near-duplicate file is stored as its own row, linked to the stored row as a
// 'duplicate', and the stored row is strengthened.
func TestImportFoldsANearDuplicateIntoTheExistingRow(t *testing.T) {
	const projectID = "test-project"
	store, ctx := storeFor(t, projectID), context.Background()

	seedDir := t.TempDir()
	nearDupFile(t, seedDir, "seed.md", "Architecture", nearDupStored)
	stored := parsedContent(t, filepath.Join(seedDir, "seed.md"))
	seededID, _, _, err := store.Upsert(ctx, projectID, mapCategory("project"), stored, "manual", 0.5, nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	before := memoryByContent(t, store, projectID, stored)

	memDir := newMemDir(t)
	nearDupFile(t, memDir, "project_architecture.md", "Architecture", nearDupImported)
	imported, _, _, _, err := importFromDir(ctx, store, projectID, memDir, slog.Default())
	if err != nil {
		t.Fatalf("importFromDir: %v", err)
	}
	if imported != 1 {
		t.Errorf("imported = %d, want 1", imported)
	}
	memoryByContent(t, store, projectID, parsedContent(t, filepath.Join(memDir, "project_architecture.md")))
	if !duplicateLinkTo(t, store, seededID) {
		t.Error("the imported near-duplicate is not linked to the stored row as a 'duplicate'")
	}
	if after := memoryByContent(t, store, projectID, stored); after.AccessCount <= before.AccessCount {
		t.Errorf("access_count = %d, want more than %d (the fold strengthens the row)", after.AccessCount, before.AccessCount)
	}
}

// The same rule inside one run: the second of two near-similar files folds into
// the row the first just stored (os.ReadDir sorts, so a_ precedes b_).
func TestImportFoldsNearDuplicateFilesWithinOneImport(t *testing.T) {
	const projectID = "test-project"
	store := storeFor(t, projectID)
	memDir := newMemDir(t)
	nearDupFile(t, memDir, "a_project.md", "A", nearDupStored)
	nearDupFile(t, memDir, "b_project.md", "B", nearDupImported)

	imported, _, _, _, err := importFromDir(context.Background(), store, projectID, memDir, slog.Default())
	if err != nil {
		t.Fatalf("importFromDir: %v", err)
	}
	if imported != 2 {
		t.Errorf("imported = %d, want 2", imported)
	}
	first := memoryByContent(t, store, projectID, parsedContent(t, filepath.Join(memDir, "a_project.md")))
	if !duplicateLinkTo(t, store, first.ID) {
		t.Error("the second file is not linked to the first as a 'duplicate'")
	}
}

// A file whose text is exactly a stored row writes nothing: no row, no link,
// no strengthening.
func TestImportSkipsAByteIdenticalFile(t *testing.T) {
	const projectID = "test-project"
	store, ctx := storeFor(t, projectID), context.Background()
	memDir := newMemDir(t)
	nearDupFile(t, memDir, "project_architecture.md", "Architecture", nearDupStored)
	stored := parsedContent(t, filepath.Join(memDir, "project_architecture.md"))
	seededID, _, _, err := store.Upsert(ctx, projectID, mapCategory("project"), stored, "manual", 0.5, nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	before := memoryByContent(t, store, projectID, stored)

	imported, _, _, _, err := importFromDir(ctx, store, projectID, memDir, slog.Default())
	if err != nil {
		t.Fatalf("importFromDir: %v", err)
	}
	if n, err := store.CountMemories(ctx, projectID); imported != 0 || err != nil || n != 1 {
		t.Errorf("imported=%d count=%d (err %v), want 0 and 1", imported, n, err)
	}
	if duplicateLinkTo(t, store, seededID) {
		t.Error("a byte-identical file was linked instead of skipped")
	}
	if after := memoryByContent(t, store, projectID, stored); after.AccessCount != before.AccessCount {
		t.Errorf("access_count moved %d -> %d on a skipped file", before.AccessCount, after.AccessCount)
	}
}
