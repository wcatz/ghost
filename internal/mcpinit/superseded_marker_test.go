package mcpinit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/memory"
)

// TestAnUnpinnedSupersededConventionIsMarkedOnTheSessionStart: once the
// lifecycle links a replaced convention with a live `supersedes` edge, the lexical
// replacement guess is off for the pair and resolve never retires a convention,
// so the edge is the only signal the block has. The old convention's line must
// carry the superseded marker at its field (the same field the lexical guess's
// marker sits at, and the one the storyline grade reads), name the superseder,
// and come after it.
func TestAnUnpinnedSupersededConventionIsMarkedOnTheSessionStart(t *testing.T) {
	path := pinSession(t, 3,
		pinSessionRow{id: "old-conv01", category: "convention", importance: 0.8,
			content: "Release tags follow semver: tag vMAJOR.MINOR.PATCH from main and run make release to publish."},
		pinSessionRow{id: "new-conv01", category: "convention", importance: 0.8,
			content: "Release tags follow calendar versioning now: tag YYYY.MM.N from main and run make release to publish."})
	db, err := memory.OpenDB(filepath.Join(os.Getenv("XDG_DATA_HOME"), "ghost", "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO memory_links (source_id, target_id, relation, strength, source) VALUES ('new-conv01', 'old-conv01', 'supersedes', 1, 'llm')`); err != nil {
		t.Fatalf("link: %v", err)
	}
	_ = db.Close()
	got := renderSessionStart(t, path)
	var oldLine string
	oldAt, newAt := -1, -1
	for i, l := range strings.Split(got, "\n") {
		switch {
		case strings.Contains(l, "follow semver"):
			oldLine, oldAt = l, i
		case strings.Contains(l, "follow calendar versioning"):
			newAt = i
		}
	}
	if oldAt < 0 || newAt < 0 {
		t.Fatalf("both conventions must be on the block. Got:\n%s", got)
	}
	if oldAt < newAt {
		t.Errorf("the superseded convention comes before its replacement. Got:\n%s", got)
	}
	head := "- [convention] `old-conv01` (0.8 "
	if !strings.HasPrefix(oldLine, head+assemble.SupersededMarker) {
		t.Errorf("the superseded convention does not carry %s at its field: %q", assemble.SupersededMarker, oldLine)
	}
	if !strings.Contains(oldLine, "superseded_by=`new-conv01`") {
		t.Errorf("the superseded convention does not name its superseder: %q", oldLine)
	}
}

// TestAKeepForeverSupersededConventionIsMarkedOnTheSessionStart: a
// `retention=persistent` row is never sunk by the edge, but the edge still
// switches the lexical guess off, so its line must still carry the marker.
func TestAKeepForeverSupersededConventionIsMarkedOnTheSessionStart(t *testing.T) {
	path := pinSession(t, 3,
		pinSessionRow{id: "old-conv01", category: "convention", importance: 0.8,
			content: "Release tags follow semver: tag vMAJOR.MINOR.PATCH from main and run make release to publish."},
		pinSessionRow{id: "new-conv01", category: "convention", importance: 0.8,
			content: "Release tags follow calendar versioning now: tag YYYY.MM.N from main and run make release to publish."})
	db, err := memory.OpenDB(filepath.Join(os.Getenv("XDG_DATA_HOME"), "ghost", "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	for _, q := range []string{
		`UPDATE memories SET retention = 'persistent' WHERE id = 'old-conv01'`,
		`INSERT INTO memory_links (source_id, target_id, relation, strength, source) VALUES ('new-conv01', 'old-conv01', 'supersedes', 1, 'llm')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_ = db.Close()
	got := renderSessionStart(t, path)
	for _, l := range strings.Split(got, "\n") {
		if strings.Contains(l, "follow semver") {
			if !strings.HasPrefix(l, "- [convention] `old-conv01` (0.8 "+assemble.SupersededMarker) || !strings.Contains(l, "superseded_by=`new-conv01`") {
				t.Errorf("the keep-forever superseded convention is not marked: %q", l)
			}
			return
		}
	}
	t.Fatalf("the keep-forever convention is not on the block. Got:\n%s", got)
}
