//go:build e2e

package e2e

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/selfupdate"
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

// seedLifecycleFailure writes the marker `ghost lifecycle` would have written for
// a failed reflect phase, keyed by the project's NAME. The reader looks for this
// project's id first and its name second (markerCandidates), and the alert shows
// when the marker's project matches either — so a name-keyed marker is one the
// session start really does find, which the control leg in the test proves.
func seedLifecycleFailure(t *testing.T, s *sandbox, project string) {
	t.Helper()
	body := fmt.Sprintf(`{"project":%q,"phases_failed":["reflect"],"error":"the reflect phase failed","at":%q,"version":1}`,
		project, time.Now().UTC().Format(time.RFC3339))
	path := filepath.Join(s.dataDir(), "lifecycle-last-failure-"+project+".json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed the lifecycle failure marker: %v", err)
	}
}

// devBuildPremise is the one premise all three tests here share: the binary
// under test is a DEVELOPMENT build, so config.CheckDevDataDir refuses a
// directory GHOST_DEV_FORBID_DATA_DIR names. A release build ignores the
// variable entirely (internal/config/devforbid.go returns before resolving
// anything), so against a release these tests would measure nothing — the
// refusal they assert does not exist in that binary.
//
// The premise is decided by asking the binary what it is, not by assuming it:
// `ghost version` prints the string the release ldflags stamp into main.version
// ("dev" for a plain `go build`), and selfupdate.IsRelease is the same parser
// the product's own guard uses, so the test's answer to "is this a release" is
// the release parser's and not a second spelling of "looks like a version".
//
// What happens when the premise is absent depends on where the binary came
// from. A binary handed in through GHOST_E2E_BIN may be a release — that is
// how a release is checked after it is cut — and the honest result is a skip
// that says why. The suite's own build cannot be a release: it is built with
// no release ldflags, so a release stamp there means the harness started
// stamping a version, and the premise is a hard failure rather than a skip.
// That is what keeps these tests from ever passing for the wrong reason.
func devBuildPremise(t *testing.T, s *sandbox) {
	t.Helper()
	r := s.run("version")
	if r.code != 0 {
		t.Fatalf("ghost version: %s", r)
	}
	line, _, _ := strings.Cut(strings.TrimSpace(r.stdout), "\n")
	ver, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(line), "ghost "), " ")
	if ver == "" {
		t.Fatalf("`ghost version` printed %q, which carries no version to judge", r.stdout)
	}
	if !selfupdate.IsRelease(ver) {
		return
	}
	if e2eBinFromEnv {
		t.Skipf("the dev-only data-dir refusal does not exist in a release build: GHOST_E2E_BIN is %s, which reports version %q", ghostBin, ver)
	}
	t.Fatalf("the binary under test reports version %q, a release, but the suite built it itself: the dev-only data-dir refusal these tests assert does not exist in a release build, so the premise is broken rather than absent", ver)
}

// TestDevBuildRefusesAForbiddenDataDir is the end-to-end shape of the rule: a
// DEVELOPMENT build, pointed at a store that is several schema steps behind,
// refuses to open it and leaves the file exactly as it found it.
//
// The store is a fixture rather than a fresh one on purpose. A fresh store is
// already at the current schema, so an open would have nothing to migrate and
// nothing to back up — the refusal would then be indistinguishable from an open
// that had done nothing. A v17 store makes the difference observable in three
// places at once: the stamp, the pre-migration copy, and the bytes.
//
// The premise is asserted rather than assumed, by devBuildPremise: the suite
// builds the binary with `go build` and no release ldflags, so main.version is
// "dev", and a release stamp there fails the premise rather than skipping it.
func TestDevBuildRefusesAForbiddenDataDir(t *testing.T) {
	s := newSandbox(t)
	devBuildPremise(t, s)

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

// TestLifecycleRunCreatesNothingInAForbiddenDataDir is the second end-to-end
// shape, and the one an independent review found by driving real binaries: the
// lifecycle's own flow reaches the data directory four ways that never open a
// store — scratch.Reap's root, the per-run start stamp, the failure marker its
// phase refusals write, and the marker the next session reads. A dev build that
// ran the whole flow had left a `scratch/` directory and a marker file named
// after a project nothing had resolved.
//
// GHOST_SCRATCH_DIR is removed from the child environment on purpose: the
// sandbox sets it so no harness child can write to a real data dir, and with it
// set the scratch root never resolves the data directory at all — which is
// exactly why a reproduction that left it in place saw nothing here.
func TestLifecycleRunCreatesNothingInAForbiddenDataDir(t *testing.T) {
	s := newSandbox(t)
	devBuildPremise(t, s)
	cs := s.mcpSession(t)
	call(t, cs, "ghost_memory_save", map[string]any{
		"project_id": e2eProject,
		"content":    "a memory the refused lifecycle must not migrate",
		"category":   "architecture",
	})
	if err := cs.Close(); err != nil {
		t.Fatalf("close the fixture's MCP session: %v", err)
	}
	// Bind before the downgrade, and the order matters: `ghost project bind`
	// reaches the store through bootstrap(), so binding a v17 fixture would
	// migrate it back to the current schema and leave the refusal nothing to
	// prove.
	s.mustRun("project", "bind", e2eProject, s.work)
	downgradeToV17(s, t)

	before := mustReadFile(t, s.dbPath())
	beforeNames := namesIn(t, s.dataDir())
	withoutEnv(s, "GHOST_SCRATCH_DIR")
	s.env = append(s.env, config.DevForbidDataDirEnv+"="+s.dataDir())

	// The exit code is not the claim: no auto phase is enabled in the sandbox,
	// so this run may well succeed while having done nothing. The claim is the
	// filesystem, so it is the filesystem that is asserted.
	s.run("lifecycle", e2eProject)

	if got := namesIn(t, s.dataDir()); strings.Join(got, ",") != strings.Join(beforeNames, ",") {
		t.Errorf("the refused lifecycle changed the data directory: before %v, after %v", beforeNames, got)
	}
	for _, name := range []string{"scratch", "lifecycle.log"} {
		if _, err := os.Stat(filepath.Join(s.dataDir(), name)); !os.IsNotExist(err) {
			t.Errorf("the refused lifecycle created %s in the forbidden data dir (stat error: %v)", name, err)
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(s.dataDir(), "lifecycle-*")); len(matches) != 0 {
		t.Errorf("the refused lifecycle wrote lifecycle files: %v", matches)
	}
	if got := stampOf(t, s.dbPath()); got != 17 {
		t.Errorf("the refused lifecycle left the store at user_version %d, want 17 (unchanged)", got)
	}
	after := mustReadFile(t, s.dbPath())
	if !bytes.Equal(after, before) {
		t.Error("the refused lifecycle changed the store's bytes: it migrated the file")
	}

	// The control, in the same store with the variable off: the same command
	// creates the scratch root. Without this leg every assertion above would
	// also pass against a flow that writes nothing on this fixture at all.
	withoutEnv(s, config.DevForbidDataDirEnv)
	s.run("lifecycle", e2eProject)
	if _, err := os.Stat(filepath.Join(s.dataDir(), "scratch")); err != nil {
		t.Errorf("with the variable unset the lifecycle created no scratch root, so the fixture proved nothing: %v", err)
	}
}

// TestSessionStartHookTouchesNothingInAForbiddenDataDir is the third shape. The
// SessionStart hook is the busiest reader of the data directory in the tree —
// the digest, the global memories, the session-count bump, the lifecycle-failure
// alert and its age-based self-clean — and it runs on every session of every
// host. It must fail open: an empty block, no store read, and nothing written.
func TestSessionStartHookTouchesNothingInAForbiddenDataDir(t *testing.T) {
	s := newSandbox(t)
	devBuildPremise(t, s)
	cs := s.mcpSession(t)
	call(t, cs, "ghost_memory_save", map[string]any{
		"project_id": e2eProject,
		"content":    "a memory the refused session start must not read",
		"category":   "architecture",
	})
	if err := cs.Close(); err != nil {
		t.Fatalf("close the fixture's MCP session: %v", err)
	}
	// Bind before the downgrade, for the reason the lifecycle test states.
	s.mustRun("project", "bind", e2eProject, s.work)
	downgradeToV17(s, t)

	// A lifecycle-failure marker already on disk, so the alert path — the one
	// that READS the data directory on every session start, and that deletes an
	// aged marker through ClearLifecycleFailure — is the path under test. It is
	// seeded as a file rather than produced by a failing lifecycle because a run
	// with every auto phase disabled has no failure to record, and enabling one
	// would make this test depend on a harness child it is not about.
	seedLifecycleFailure(t, s, e2eProject)
	// The control, before anything is measured: with the variable unset, this
	// same hook on this same store renders the memory and alerts on the marker.
	// Without it, an empty digest and a silent alert would be indistinguishable
	// from a project that never resolved — and the hook's own job is neither.
	control := s.mustRunStdin(sessionStartPayload("claude-code", s.work), "hook", "session-start", "--source", "claude-code")
	mustContain(t, "the control session start", control.stdout, "a memory the refused session start must not read")
	mustContain(t, "the control session start", control.stdout, "Ghost maintenance alert")

	before := mustReadFile(t, s.dbPath())
	beforeNames := namesIn(t, s.dataDir())
	withoutEnv(s, "GHOST_SCRATCH_DIR")
	s.env = append(s.env, config.DevForbidDataDirEnv+"="+s.dataDir())

	r := s.mustRunStdin(sessionStartPayload("claude-code", s.work), "hook", "session-start", "--source", "claude-code")
	mustNotContain(t, "the refused session start", r.stdout, "a memory the refused session start must not read")
	mustNotContain(t, "the refused session start", r.stdout, "Ghost context")

	if got := namesIn(t, s.dataDir()); strings.Join(got, ",") != strings.Join(beforeNames, ",") {
		t.Errorf("the refused session start changed the data directory: before %v, after %v", beforeNames, got)
	}
	if got := stampOf(t, s.dbPath()); got != 17 {
		t.Errorf("the refused session start left the store at user_version %d, want 17 (unchanged)", got)
	}
	if after := mustReadFile(t, s.dbPath()); !bytes.Equal(after, before) {
		t.Error("the refused session start changed the store's bytes")
	}
}
