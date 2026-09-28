//go:build e2e

package e2e

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

// withoutEnv removes one variable from the sandbox child's environment. It
// exists so a test can prove what the variable is doing by turning it off again
// in the same store, rather than by comparing two sandboxes that differ in
// everything.
func withoutEnv(s *sandbox, name string) {
	kept := make([]string, 0, len(s.env))
	for _, kv := range s.env {
		if !strings.HasPrefix(kv, name+"=") {
			kept = append(kept, kv)
		}
	}
	s.env = kept
}

// realPath is a path as the filesystem reports it, which is what a refusal names:
// the guard canonicalizes both sides before comparing, and the sandbox root is a
// temp dir that a symlinked /var (macOS) does not spell the way EvalSymlinks
// returns it.
func realPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	return resolved
}

// stampOf reads user_version from the store FILE and closes the handle before
// returning, rather than through the suite's observer (which holds its handle
// until cleanup). The difference matters here: this test's claim is that the
// refused command wrote nothing, and a reader still holding the file open while
// the command runs is a variable this test is trying to measure.
func stampOf(t *testing.T, dbPath string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open %s read-only: %v", dbPath, err)
	}
	defer db.Close() //nolint:errcheck
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return v
}

// namesIn lists a directory's entries, so "nothing was created" is a fact about
// the filesystem rather than an assumption.
func namesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// TestDevBuildRefusesAForbiddenDataDir is the end-to-end shape of the rule: a
// DEVELOPMENT build, pointed at a store that is one schema step behind, refuses
// to open it and leaves the file exactly as it found it.
//
// The store is a fixture rather than a fresh one on purpose. A fresh store is
// already at the current schema, so an open would have nothing to migrate and
// nothing to back up — the refusal would then be indistinguishable from an open
// that had done nothing. A v17 store makes the difference observable in three
// places at once: the stamp, the pre-migration copy, and the bytes.
//
// The premise is asserted rather than assumed. The suite builds the binary with
// `go build` and no release ldflags, so main.version is "dev"; if the harness
// ever starts stamping a version, the release build ignores the variable and
// this test would pass for the wrong reason.
func TestDevBuildRefusesAForbiddenDataDir(t *testing.T) {
	s := newSandbox(t)
	mustContain(t, "the binary under test", s.mustRun("version").stdout, "ghost dev")

	// A store holding a memory, downgraded to v17 so an ordinary open would
	// migrate it. The content is written through the product's own MCP surface,
	// so the rows are the ones a real release would have written.
	cs := s.mcpSession(t)
	call(t, cs, "ghost_memory_save", map[string]any{
		"project_id": e2eProject,
		"content":    "a memory the refused open must not migrate",
		"category":   "architecture",
	})
	// The downgrade writes the file directly, and a live server behind it makes
	// that SQLITE_BUSY on a busy CI runner — the same reason the v16 fixture
	// closes its session first.
	if err := cs.Close(); err != nil {
		t.Fatalf("close the fixture's MCP session: %v", err)
	}
	downgradeToV17(s, t)
	if got := stampOf(t, s.dbPath()); got != 17 {
		t.Fatalf("the fixture is at user_version %d, want 17", got)
	}
	if n := countInFileWhere(t, s.dbPath(), "SELECT COUNT(*) FROM memories WHERE project_id = ?", e2eProject); n == 0 {
		t.Fatal("the fixture holds no memory, so a refused open would be untestable")
	}

	// Everything the refusal must leave alone, measured now.
	before := mustReadFile(t, s.dbPath())
	beforeInfo, err := os.Stat(s.dbPath())
	if err != nil {
		t.Fatalf("stat the store: %v", err)
	}
	beforeNames := namesIn(t, s.dataDir())

	// The sandbox's own data directory is the "real" store here, which is the
	// situation the issue describes: a malformed export left XDG_DATA_HOME
	// pointing at the store a development build must not migrate.
	s.env = append(s.env, config.DevForbidDataDirEnv+"="+s.dataDir())

	// `ghost backup` is the cheapest command that reaches a read-WRITE open
	// through the ordinary bootstrap, which is the open that migrates: it would
	// take a pre-migration backup and stamp v18 without this rule.
	dest := filepath.Join(t.TempDir(), "snapshot.db")
	r := s.mustFail("backup", "--out", dest)
	if r.code != 1 {
		t.Errorf("the refused command exited %d, want 1: a run that failed is 1 and a usage error is 2", r.code)
	}
	mustContain(t, "the refusal", r.stderr, config.DevForbidDataDirEnv)
	mustContain(t, "the refusal", r.stderr, realPath(t, s.dataDir()))

	// The store is untouched: same bytes, same mtime, same stamp, and nothing
	// written beside it.
	after := mustReadFile(t, s.dbPath())
	if !bytes.Equal(after, before) {
		t.Error("the refused command changed the store's bytes: it migrated the file")
	}
	afterInfo, err := os.Stat(s.dbPath())
	if err != nil {
		t.Fatalf("re-stat the store: %v", err)
	}
	if !afterInfo.ModTime().Equal(beforeInfo.ModTime()) {
		t.Errorf("the refused command moved the store's mtime from %s to %s", beforeInfo.ModTime(), afterInfo.ModTime())
	}
	if got := stampOf(t, s.dbPath()); got != 17 {
		t.Errorf("the refused command left the store at user_version %d, want 17 (unchanged)", got)
	}
	if got := backupsIn(t, s.dataDir()); len(got) != 0 {
		t.Errorf("the refused command wrote pre-migration backups: %v", got)
	}
	if got := namesIn(t, s.dataDir()); strings.Join(got, ",") != strings.Join(beforeNames, ",") {
		t.Errorf("the refused command changed the data directory: before %v, after %v", beforeNames, got)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("the refused command wrote its snapshot to %s, want nothing written (stat error: %v)", dest, err)
	}

	// The control, in the same store with the variable turned off: the command
	// opens the fixture, migrates it, and takes the pre-migration copy. Without
	// this leg every assertion above would also pass against a fixture nothing
	// could have migrated.
	withoutEnv(s, config.DevForbidDataDirEnv)
	s.mustRun("backup", "--out", dest)
	if got, want := stampOf(t, s.dbPath()), memory.SchemaVersion(); got != want {
		t.Errorf("with the variable unset the store is at user_version %d, want %d: the fixture was not migratable", got, want)
	}
	if got := backupsIn(t, s.dataDir()); len(got) == 0 {
		t.Error("with the variable unset the open took no pre-migration backup, so the fixture proved nothing")
	}
}
