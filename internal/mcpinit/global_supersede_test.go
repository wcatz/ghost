package mcpinit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// globalSupersedeStore seeds a `_global` bucket wide enough for its 8-item cap to
// bind, with one pair joined by a `supersedes` edge whose older row outranks the
// row that replaced it. Without the demotion both are inside the cap; with it the
// superseded row is pushed to the bottom of the 16-row window and cut by the cap.
//
// The ranking is a function of the seed and not of when the test runs, so
// `updated_at` and `created_at` are written explicitly and the importance ladder
// is monotonic with the id, which is what makes "the 8th by the bucket's own
// order" a statement about the fixture rather than about the clock.
func globalSupersedeStore(t *testing.T) string {
	t.Helper()
	xdgHome := t.TempDir()
	ghostDir := filepath.Join(xdgHome, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir ghostDir: %v", err)
	}
	db, err := memory.OpenDB(filepath.Join(ghostDir, "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`); err != nil {
		t.Fatalf("insert _global: %v", err)
	}
	st := memory.NewStore(db, nil)
	t.Cleanup(func() { _ = st.Close() })

	// Ten rows against a cap of eight, so two are cut. `gsup07` is the row the
	// edge names as superseded and it is the 8th by the bucket's own
	// pinned/importance/recency order — inside the cap on its own, and outside it
	// once the demotion has moved it.
	const stamp = "2026-01-02 03:04:05"
	for i := 0; i < 10; i++ {
		id := "gsup0" + string(rune('0'+i))
		content := "a global row that is not involved in the edge"
		switch id {
		case "gsup07":
			content = "the global row a newer one supersedes"
		case "gsup08":
			content = "the global row that replaces an older one"
		}
		importance := 0.50 + float64(i)/100 // 0.50 .. 0.59, ascending with i
		if _, err := db.Exec(`
			INSERT INTO memories (id, project_id, category, content, source, importance, created_at, updated_at)
			VALUES (?, '_global', 'preference', ?, 'manual', ?, ?, ?)`, id, content, importance, stamp, stamp); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	if err := st.CreateLink(t.Context(), "gsup08", "gsup07", "supersedes", 1, "manual"); err != nil {
		t.Fatalf("link supersedes: %v", err)
	}

	projectPath := filepath.Join(t.TempDir(), "gsupproj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatalf("mkdir projectPath: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(projectPath)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	insertProject(t, db, "p1", canonical, "gsupproj")

	t.Setenv("XDG_DATA_HOME", xdgHome)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	return projectPath
}

// TestTheGlobalBucketNowDemotesSupersededRows states the one branch where the
// block is NOT byte-identical through the move, and it is stated here because the
// golden cannot state it.
//
// `loadGlobalMemories` ran exactly one demotion lookup over the global rows:
// DemotionPenalties, the near-duplicate pass. It never called SupersedePenalties,
// so a `supersedes` edge between two `_global` rows changed nothing. The
// assembler's passive demotion runs the supersede lookup for every bucket
// unconditionally — `SlicePolicy` has no field to decline it, and
// `DemoteOnlyWhenOverCap`, which is set only on the project slice, gates the
// NEAR-DUPLICATE step that follows it. So a global row a newer one superseded is
// now pushed to the bottom of the 16-row window, and a bucket capped at 8 cuts
// it. That is a MEMBERSHIP change in the Global section, not a reorder.
//
// It is why the golden's parity claim is scoped to the branches its fixture
// exercises: that fixture's one `supersedes` edge joins two project rows, so it
// cannot see this. Adding a global edge to the golden would have turned the
// golden from a parity proof into a diff record, which is why this is a separate
// test rather than a fixture change.
//
// The change is kept rather than reverted, and the reason is the bucket's own
// stated policy: the global slice drops its near-duplicate losers because "a
// superseded preference is not worth one of eight slots in a block that competes
// for attention across every project", and an explicit `supersedes` edge is a
// stronger signal than a cosine near-duplicate. Restoring parity would mean a
// second policy field saying globals ignore a relationship the rest of the system
// honours.
func TestTheGlobalBucketNowDemotesSupersededRows(t *testing.T) {
	got := renderSessionStart(t, globalSupersedeStore(t))

	if !strings.Contains(got, "the global row that replaces an older one") {
		t.Fatalf("the replacing global row is absent, so the fixture is not exercising the demotion; got:\n%s", got)
	}
	if strings.Contains(got, "the global row a newer one supersedes") {
		t.Errorf("the block offers a global row an explicit supersedes edge names as replaced. That row "+
			"outranks its replacement on the bucket's own order, so it is inside the 8-item cap on its own "+
			"and outside it once the demotion has moved it — which is the change this test exists to "+
			"state. Got:\n%s", got)
	}
}
