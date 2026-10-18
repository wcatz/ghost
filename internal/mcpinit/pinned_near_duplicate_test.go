package mcpinit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// TestSessionStartShowsBothRowsOfAPinnedNearDuplicateGlobalPair: two pinned
// `_global` saves of near-identical text, the pair a pinned fold writes (both
// rows pinned, joined by a `duplicate` edge). The global slice removes
// near-duplicate losers, and it used to remove the lower-ranked pin, so the
// row the caller had just pinned was missing from every session start. A pin
// is a slot guarantee on every passive surface, so both appear. The control
// pins only the first save: its unpinned restatement is still withheld.
func TestSessionStartShowsBothRowsOfAPinnedNearDuplicateGlobalPair(t *testing.T) {
	const (
		first  = "Always indent Go code with tabs and run gofmt before every commit in all repositories"
		second = "Always indent Go code with tabs and run gofmt before each commit in all repositories"
	)
	for name, tc := range map[string]struct {
		pinSecond bool
	}{
		"both pinned":       {pinSecond: true},
		"only first pinned": {pinSecond: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			ctx := context.Background()
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "ghost.db")
			db, err := memory.OpenDB(dbPath)
			if err != nil {
				t.Fatalf("OpenDB: %v", err)
			}
			projectPath := filepath.Join(dir, "pinproj")
			if err := os.MkdirAll(projectPath, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			canon, err := filepath.EvalSymlinks(projectPath)
			if err != nil {
				t.Fatalf("EvalSymlinks: %v", err)
			}
			if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('pp', ?, 'pinproj'), ('_global', '_global', 'global')`, canon); err != nil {
				t.Fatalf("insert projects: %v", err)
			}
			st := memory.NewStore(db, nil)
			save := func(content string, pin bool, imp float32) (string, string) {
				t.Helper()
				id, dup, _, err := st.UpsertWithOptions(ctx, memory.GlobalProjectID, "preference", content, "manual", imp, nil, memory.UpsertOptions{Pin: pin})
				if err != nil {
					t.Fatalf("save %q: %v", content, err)
				}
				return id, dup
			}
			a, _ := save(first, true, 0.6)
			b, dupOf := save(second, tc.pinSecond, 0.6)
			if b == a || dupOf != a {
				t.Fatalf("fixture: the second save must be a new row linked as a duplicate of the first (id %s, duplicateOf %q, first %s)", b, dupOf, a)
			}
			save("Prefer table-driven tests for every Go package", false, 0.3)
			save("Write commit messages in the conventional commits style", false, 0.3)
			if err := db.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}

			block := SessionBlockAt(dbPath, projectPath, mustHookConfig(t), time.Now().UTC())
			if !strings.Contains(block, a) {
				t.Errorf("the first pinned global %s is not in the session-start block:\n%s", a, block)
			}
			if tc.pinSecond {
				if !strings.Contains(block, b) {
					t.Errorf("the second pinned global %s, a near-duplicate of a pinned row, is not in the session-start block:\n%s", b, block)
				}
				if strings.Contains(block, "withheld rather than ranked out") {
					t.Errorf("the header counts a withheld row, but no global should have been withheld:\n%s", block)
				}
				return
			}
			if strings.Contains(block, b) {
				t.Errorf("the unpinned near-duplicate %s of a pinned global should still be dropped from the session start, but it is in the block:\n%s", b, block)
			}
		})
	}
}
