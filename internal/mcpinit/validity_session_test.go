package mcpinit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// validitySession builds a store whose project bucket holds one row per validity
// verdict the assembler can reach, points XDG_DATA_HOME at it, and returns the
// project path for the block to read.
//
// The verdicts are named by ROLE rather than by the window that produces them,
// because the row is a fixture and the window is a clock: a past `valid_until`
// would have to stay in the past, which a fixed stamp gives, but a future
// `valid_from` has to stay in the FUTURE, which a fixed stamp cannot. So the two
// moving windows are written relative to now and the two fixed ones are not, and
// each row's id says which is which.
func validitySession(t *testing.T) (projectPath string) {
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
	projectPath = filepath.Join(t.TempDir(), "validityproj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatalf("mkdir projectPath: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(projectPath)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	insertProject(t, db, "p1", canonical, "validityproj")

	// A wide created_at spread so the ranking is decided by the seed, and the
	// expired/future rows are given the HIGHEST importance and the NEWEST
	// created_at on purpose: a row that survives must be the one that earned its
	// place, so a dropped row has to be a row the ranking wanted.
	//
	// memory.StoredStampLayout and not RFC 3339, which is the whole reason this
	// fixture needed writing twice. StampLayouts is the stored layout and a bare
	// date, so an RFC 3339 value is UNREADABLE rather than unexpired, and a row
	// whose stamp nobody can read is kept as unset with a note. The first version
	// of this fixture wrote RFC 3339 and the expired and future rows both
	// rendered — a failure that looks like the block ignoring the window, and was
	// really the store declining to read the row's claim at all.
	now := time.Now().UTC()
	rows := []struct {
		id, content, validFrom, validUntil, verifiedAt string
		importance                                     float64
	}{
		{"v-open001", "a window that opened and has not closed", "2026-01-01 00:00:00", "", "", 0.90},
		{"v-expird01", "a window that closed in the past", "2026-01-01 00:00:00",
			now.Add(-24 * time.Hour).Format(memory.StoredStampLayout), "", 0.99},
		{"v-futur01", "a window that has not opened yet", now.Add(24 * time.Hour).Format(memory.StoredStampLayout),
			"", "", 0.98},
		{"v-unver01", "a live window nobody has re-verified", "2026-01-01 00:00:00", "",
			"2026-02-02 00:00:00", 0.97},
	}
	// Every stamp above has to be one the store can read, or this test passes for
	// the wrong reason: an unreadable stamp KEEPS the row, so a fixture of
	// unreadable stamps satisfies every "the window is honoured" assertion while
	// honouring nothing. Asserted rather than assumed, because the failure is
	// silent and looks like a pass.
	for _, r := range rows {
		for _, stamp := range []string{r.validFrom, r.validUntil, r.verifiedAt} {
			if stamp == "" {
				continue
			}
			if _, ok := memory.ParseStamp(stamp); !ok {
				t.Fatalf("fixture stamp %q is not one memory.ParseStamp reads, so the row would be kept "+
					"as unset and this test would pass without testing anything", stamp)
			}
		}
	}
	for i, r := range rows {
		stamp := time.Date(2026, 1, 2+i, 3, 4, 5, 0, time.UTC).Format("2006-01-02 15:04:05")
		if _, err := db.Exec(`
			INSERT INTO memories (id, project_id, category, content, source, importance, created_at,
			                      valid_from, valid_until, verified_at)
			VALUES (?, 'p1', 'fact', ?, 'manual', ?, ?, ?, ?, ?)`,
			r.id, r.content, r.importance, stamp, nullIfEmpty(r.validFrom), nullIfEmpty(r.validUntil), nullIfEmpty(r.verifiedAt),
		); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}
	_ = db.Close()
	t.Setenv("XDG_DATA_HOME", xdgHome)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	return projectPath
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// TestTheSessionStartHonoursTheValidityWindow is the test for a behaviour change
// this PR made without stating it, and the reviewer is what surfaced it.
//
// The private loaders filtered only `resolved_at IS NULL` plus the scope clause,
// so a memory whose `valid_until` had passed — or whose `valid_from` had not
// arrived — was injected into every session as a plain present-tense line.
// `assemble.Run` runs stage 2 unconditionally and there is no knob to decline it,
// so the block now drops both. That is the right outcome: a memory the store
// itself says has retired cannot be offered as current, and the alternative is a
// line asserting as fact something the row has withdrawn. But it changes what
// every session is told, it arrived by inheritance rather than by decision, and
// the golden could not have caught it — sessionstart_golden_test.go's fixture
// seeds no validity column at all, so a store with no dated rows renders
// identically before and after.
//
// So the claim is pinned directly, on all three verdicts rather than the two
// that changed, because the third is the one that must NOT move: `unverified` is
// a flag and not a predicate. A live window nobody has re-checked is still true
// as far as the store knows, and a block that hid those would be making a claim
// the data does not support.
func TestTheSessionStartHonoursTheValidityWindow(t *testing.T) {
	got := renderSessionStart(t, validitySession(t))

	for _, absent := range []string{
		"a window that closed in the past",
		"a window that has not opened yet",
	} {
		if strings.Contains(got, absent) {
			t.Errorf("the block offers %q; a memory whose validity window has closed or has not opened is "+
				"not current, and the block states its rows as present-tense facts. Got:\n%s", absent, got)
		}
	}
	for _, present := range []string{
		"a window that opened and has not closed",
		"a live window nobody has re-verified",
	} {
		if !strings.Contains(got, present) {
			t.Errorf("the block dropped %q; its window is open. `unverified` is a flag and not a "+
				"predicate, so a live row nobody re-checked is still true as far as the store knows. Got:\n%s", present, got)
		}
	}
}
