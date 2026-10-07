package mcpserver

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/mcpinit"
	"github.com/wcatz/ghost/internal/memory"
)

// TestSessionStartAndProjectContextSaySameSentenceForAnAllExpiredProject is the
// cross-surface half of issue #897: a project whose every row has aged out gets
// one sentence for that state, and the session-start block and
// ghost_project_context print the same bytes. (The out-of-scope state cannot be
// driven end to end: ghost_project_context takes no scope and the session start
// filters scope in SQL, so no stored row reaches the assembler's scope stage on
// either surface; the assemble package pins that case against the same
// function, TestWithheldNoteIsTheAssemblersOwnEmptyNote.)
func TestSessionStartAndProjectContextSaySameSentenceForAnAllExpiredProject(t *testing.T) {
	xdgHome := t.TempDir()
	ghostDir := filepath.Join(xdgHome, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := memory.OpenDB(filepath.Join(ghostDir, "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`); err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('agree', ?, 'agree')`, dir); err != nil {
		t.Fatal(err)
	}
	closed := time.Now().UTC().Add(-24 * time.Hour).Format(memory.StoredStampLayout)
	for _, id := range []string{"e1", "e2"} {
		if _, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance, valid_until)
			VALUES (?, 'agree', 'fact', 'a retired claim '||?, 'manual', 0.9, ?)`, id, id, closed); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("XDG_DATA_HOME", xdgHome)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	st := memory.NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	_, session := validityServerFor(t, st)
	tool := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "agree"}))

	start := mcpinit.RenderSessionContext(dir)
	_, after, ok := strings.Cut(start, "**Memories:**\n")
	if !ok {
		t.Fatalf("the session start rendered no withheld-memories section:\n%s", start)
	}
	note, _, _ := strings.Cut(after, "\n")
	if !strings.Contains(note, "withheld as out of date") {
		t.Fatalf("the session-start note is not the withheld sentence: %q", note)
	}
	if !strings.Contains(tool, note) {
		t.Errorf("the two surfaces disagree.\nsession start: %q\nproject context:\n%s", note, tool)
	}
}
