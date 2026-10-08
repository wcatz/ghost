package mcpinit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// conflictStore seeds a project pair and a global pair, each joined by a
// `contradicts` edge, plus one unlinked row. withdraw invalidates both edges.
func conflictStore(t *testing.T, withdraw bool) string {
	t.Helper()
	xdgHome := t.TempDir()
	ghostDir := filepath.Join(xdgHome, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	db, err := memory.OpenDB(filepath.Join(ghostDir, "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`); err != nil {
		t.Fatalf("insert _global: %v", err)
	}
	projectPath := filepath.Join(t.TempDir(), "conflictproj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(projectPath)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	insertProject(t, db, "p1", canonical, "conflictproj")
	st := memory.NewStore(db, nil)
	t.Cleanup(func() { _ = st.Close() })

	const stamp = "2026-01-02 03:04:05"
	for _, r := range []struct{ id, project, content string }{
		{"cfpa01", "p1", "the project database is postgres"},
		{"cfpb01", "p1", "the project database is mysql"},
		{"cfpc01", "p1", "an unrelated project row"},
		{"cfga01", "_global", "always use tabs"},
		{"cfgb01", "_global", "never use tabs"},
	} {
		if _, err := db.Exec(`
			INSERT INTO memories (id, project_id, category, content, source, importance, created_at, updated_at)
			VALUES (?, ?, 'preference', ?, 'manual', 0.6, ?, ?)`, r.id, r.project, r.content, stamp, stamp); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}
	for _, e := range [][2]string{{"cfpa01", "cfpb01"}, {"cfga01", "cfgb01"}} {
		if err := st.CreateLink(t.Context(), e[0], e[1], "contradicts", 1, "manual"); err != nil {
			t.Fatalf("link: %v", err)
		}
		if withdraw {
			if n, err := st.InvalidateLink(t.Context(), e[0], e[1], "contradicts"); err != nil || n != 1 {
				t.Fatalf("withdraw: n=%d err=%v", n, err)
			}
		}
	}
	t.Setenv("XDG_DATA_HOME", xdgHome)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	return projectPath
}

func blockLine(t *testing.T, block, id string) string {
	t.Helper()
	var found []string
	for _, l := range strings.Split(block, "\n") {
		if strings.HasPrefix(l, "- [") && strings.Contains(l, "`"+id+"` (") {
			found = append(found, l)
		}
	}
	// The hook prints the block more than once (plain and JSON-escaped forms), so
	// the lines must agree rather than be unique.
	if len(found) == 0 {
		t.Fatalf("no line for %s in:\n%s", id, block)
	}
	for _, l := range found[1:] {
		if l != found[0] {
			t.Fatalf("two different lines for %s: %q vs %q", id, found[0], l)
		}
	}
	return found[0]
}

// hasBlockLine reports whether id renders anywhere in the block. The withheld
// side of a separated pair renders no line at all, which blockLine cannot report.
func hasBlockLine(block, id string) bool {
	for _, l := range strings.Split(block, "\n") {
		if strings.HasPrefix(l, "- [") && strings.Contains(l, "`"+id+"` (") {
			return true
		}
	}
	return false
}

// wantSessionStartSeparatedPair asserts the pair is separated in the session-start
// block: exactly one side renders, and its line names the other as withheld.
func wantSessionStartSeparatedPair(t *testing.T, block, a, b string) {
	t.Helper()
	la, lb := hasBlockLine(block, a), hasBlockLine(block, b)
	switch {
	case la && !lb:
		if l := blockLine(t, block, a); !strings.Contains(l, "conflicts_with=`"+b+"`") {
			t.Errorf("%s does not name the withheld %s: %q", a, b, l)
		}
	case lb && !la:
		if l := blockLine(t, block, b); !strings.Contains(l, "conflicts_with=`"+a+"`") {
			t.Errorf("%s does not name the withheld %s: %q", b, a, l)
		}
	default:
		t.Errorf("a separated pair rendered a=%v b=%v, want exactly one side rendered", la, lb)
	}
}

func TestSessionStartSeparatesAContradictingPair(t *testing.T) {
	got := renderSessionStart(t, conflictStore(t, false))
	for _, p := range [][2]string{{"cfpa01", "cfpb01"}, {"cfga01", "cfgb01"}} {
		wantSessionStartSeparatedPair(t, got, p[0], p[1])
	}
	if l := blockLine(t, got, "cfpc01"); strings.Contains(l, "conflicts_with") {
		t.Errorf("an unlinked row is marked: %q", l)
	}
}

func TestSessionStartDoesNotMarkAWithdrawnEdge(t *testing.T) {
	got := renderSessionStart(t, conflictStore(t, true))
	if !strings.Contains(got, "`cfpa01`") || !strings.Contains(got, "`cfgb01`") {
		t.Fatalf("precondition: the rows must still render:\n%s", got)
	}
	if strings.Contains(got, "conflicts_with") {
		t.Errorf("a withdrawn edge is marked:\n%s", got)
	}
}
