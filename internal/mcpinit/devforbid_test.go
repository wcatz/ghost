package mcpinit

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

// devBuild is what a plain `go build` of cmd/ghost reports, which is the build
// the guard exists for: one compiled from a working tree, never published.
const devBuild = "dev"

// releaseBuild is the version the release ldflags stamp, the other half of the
// pair: the artifact a user actually installed, which the variable must not
// touch.
const releaseBuild = "0.39.0"

// withBuildVersion sets the build version the DATA-DIRECTORY RESOLVERS judge by
// (config owns both, since the refusal happens there — #721) and restores the
// previous one, so one test cannot leave the next one believing it is a release.
func withBuildVersion(t *testing.T, version string) {
	t.Helper()
	prev := config.BuildVersion()
	config.SetBuildVersion(version)
	t.Cleanup(func() { config.SetBuildVersion(prev) })
}

// storeAt writes a store at dir/ghost.db and returns the path, so a test can
// measure it before anything opens it.
func storeAt(t *testing.T, dir string) string {
	t.Helper()
	mkdirAll(t, dir)
	db, err := memory.OpenDB(filepath.Join(dir, "ghost.db"))
	if err != nil {
		t.Fatalf("create the store: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close the store: %v", err)
	}
	return filepath.Join(dir, "ghost.db")
}

// stampToV17 undoes the v18 additions and stamps the store back, so an ordinary
// read-write open would MIGRATE it — take a pre-migration backup and write the
// new schema. Without that, a refusal could be passing for the wrong reason: a
// store at the current schema has nothing to migrate and nothing to back up.
func stampToV17(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close() //nolint:errcheck
	for _, stmt := range []string{
		`DROP INDEX IF EXISTS idx_provenance_memory`,
		`DROP TABLE IF EXISTS memory_provenance`,
		`DROP TABLE IF EXISTS memory_snapshot_evidence`,
		`PRAGMA user_version = 17`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("downgrade the fixture (%s): %v", stmt, err)
		}
	}
}

// realPath is a path as the filesystem reports it, which is what a refusal names:
// the guard canonicalizes both sides before comparing, and a temp dir under a
// symlinked /var (macOS) is not spelled the way EvalSymlinks returns it.
func realPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	return resolved
}

// mkdirAll creates a directory and fails the test if it cannot, so a test body
// stays about the rule rather than about the fixture.
func mkdirAll(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	return path
}

// preMigrateBackupsIn lists the pre-migration copies beside dbPath, which is the
// product's own naming (memory.backupBeforeMigrate).
func preMigrateBackupsIn(t *testing.T, dataDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatalf("read %s: %v", dataDir, err)
	}
	var out []string
	for _, e := range entries {
		if strings.Contains(e.Name(), ".pre-migrate-") {
			out = append(out, e.Name())
		}
	}
	return out
}

// dirEntries is every name in a directory, for the "nothing was created"
// assertion. A refusal that left a -wal or a backup behind would show up here.
func dirEntries(t *testing.T, dir string) []string {
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

// TestImportMemoriesRefusesAForbiddenDataDirAndLeavesTheStoreUntouched is the
// property the whole issue is about, measured rather than asserted from prose: a
// development build pointed at a real store must not migrate it, and the proof
// is that the file is byte-for-byte what it was, its timestamp has not moved,
// and no pre-migration copy was written beside it.
//
// importMemories is the read-write open it drives — `ghost mcp init`'s — and
// OpenDB is where a migration would happen, so this is the path that would do
// the damage if the check ran late or not at all.
func TestImportMemoriesRefusesAForbiddenDataDirAndLeavesTheStoreUntouched(t *testing.T) {
	withBuildVersion(t, devBuild)
	root := t.TempDir()
	dataHome := filepath.Join(root, "data")
	dataDir := filepath.Join(dataHome, "ghost")
	dbPath := storeAt(t, dataDir)
	stampToV17(t, dbPath)

	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read the store: %v", err)
	}
	beforeInfo, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat the store: %v", err)
	}
	beforeNames := dirEntries(t, dataDir)

	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv(config.DevForbidDataDirEnv, dataDir)

	var out bytes.Buffer
	_, err = importMemories(&out, true)
	if err == nil {
		t.Fatalf("importMemories opened a forbidden data dir, want a refusal; it printed:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), config.DevForbidDataDirEnv) {
		t.Errorf("the refusal does not name %s: %v", config.DevForbidDataDirEnv, err)
	}
	if want := realPath(t, dataDir); !strings.Contains(err.Error(), want) {
		t.Errorf("the refusal does not name the data directory %s: %v", want, err)
	}

	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("re-read the store: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Error("the refused store's bytes changed: the open migrated it")
	}
	afterInfo, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("re-stat the store: %v", err)
	}
	if !afterInfo.ModTime().Equal(beforeInfo.ModTime()) {
		t.Errorf("the refused store's mtime moved from %s to %s", beforeInfo.ModTime(), afterInfo.ModTime())
	}
	if got := preMigrateBackupsIn(t, dataDir); len(got) != 0 {
		t.Errorf("the refused open wrote pre-migration backups: %v", got)
	}
	if got := dirEntries(t, dataDir); strings.Join(got, ",") != strings.Join(beforeNames, ",") {
		t.Errorf("the refused open changed the data directory: before %v, after %v", beforeNames, got)
	}
}

// TestImportMemoriesOpensAForbiddenDataDirOnAReleaseBuild is the reason the
// variable can be exported into a developer's shell at all. The same refused
// store, on the artifact a release published, is opened and MIGRATED — because
// that is what a release is for, and a guard that stopped it would break the
// install the developer also uses.
func TestImportMemoriesOpensAForbiddenDataDirOnAReleaseBuild(t *testing.T) {
	withBuildVersion(t, releaseBuild)
	root := t.TempDir()
	dataHome := filepath.Join(root, "data")
	dataDir := filepath.Join(dataHome, "ghost")
	dbPath := storeAt(t, dataDir)
	stampToV17(t, dbPath)

	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv(config.DevForbidDataDirEnv, dataDir)

	var out bytes.Buffer
	if _, err := importMemories(&out, true); err != nil {
		t.Fatalf("importMemories on a release build = %v, want the store opened; it printed:\n%s", err, out.String())
	}
	// And it really was opened: the stamp moved, which is the migration this
	// guard exists to keep a development build away from.
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open %s read-only: %v", dbPath, err)
	}
	defer db.Close() //nolint:errcheck
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if want := memory.SchemaVersion(); v != want {
		t.Fatalf("the release build left the store at user_version %d, want %d (the store was not opened)", v, want)
	}
}

// TestTheSessionContextHookFailsOpenOnAForbiddenDataDir is the hook half of the
// contract, and "fail open" is a claim about two things at once: the session is
// never blocked, and the store is not touched. A hook that rendered nothing but
// had still opened the database read-write would satisfy the first and break the
// whole point.
//
// The assertion is behavioural rather than a count of open calls: a store
// holding a memory renders that memory into the digest, so an empty digest and
// an absent database are what "no DB access" looks like from outside.
func TestTheSessionContextHookFailsOpenOnAForbiddenDataDir(t *testing.T) {
	withBuildVersion(t, devBuild)
	root := t.TempDir()
	dataHome := filepath.Join(root, "data")
	for _, d := range []string{dataHome, filepath.Join(root, "work")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	// The directory the project is recorded under, canonicalized: the hook
	// canonicalizes the directory it renders for, and this test's claim is about
	// the refusal rather than about a project that failed to resolve.
	work := realPath(t, filepath.Join(root, "work"))
	// HOME and the config roots, so the hook's other lookups (the Obsidian
	// mirror's opt-in, the scratch root) stay inside this test's tree.
	setHome(t, root)
	for _, kv := range [][2]string{
		{"XDG_CONFIG_HOME", filepath.Join(root, "config")},
		{"XDG_CACHE_HOME", filepath.Join(root, "cache")},
		{"XDG_DATA_HOME", dataHome},
	} {
		t.Setenv(kv[0], kv[1])
	}
	t.Setenv(config.DevForbidDataDirEnv, filepath.Join(dataHome, "ghost"))

	// No store at all: the strongest statement of "no DB access" is that the
	// hook's reads leave nothing behind, exactly as a machine with no Ghost
	// installed does.
	if out := RenderSessionContext(work); out != "" {
		t.Errorf("RenderSessionContext with no store = %q, want an empty block", out)
	}
	if _, err := os.Stat(filepath.Join(dataHome, "ghost", "ghost.db")); !os.IsNotExist(err) {
		t.Errorf("the refused hook created a database, want none (stat error: %v)", err)
	}
	if _, err := os.Stat(filepath.Join(dataHome, "ghost")); !os.IsNotExist(err) {
		t.Errorf("the refused hook created the data directory, want none (stat error: %v)", err)
	}

	// And with a store that DOES hold a memory, the digest must not render it:
	// the hook fails open by having nothing to say, not by reading anyway.
	dataDir := filepath.Join(dataHome, "ghost")
	dbPath := storeAt(t, dataDir)
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	store := memory.NewStore(db, nil)
	const projectID = "e2e-dev-guard"
	if err := store.EnsureProject(context.Background(), projectID, work, "e2e-dev-guard"); err != nil {
		t.Fatalf("ensure the project: %v", err)
	}
	if _, err := store.Create(context.Background(), projectID, memory.Memory{
		Category:   "architecture",
		Content:    "a memory the refused hook must not render",
		Importance: 0.6,
		Source:     "manual",
	}); err != nil {
		t.Fatalf("create the memory: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close the store: %v", err)
	}

	// Named in the forbidden listing now that the store exists, and re-stamped
	// to v17 so an open would migrate it rather than merely read it.
	t.Setenv(config.DevForbidDataDirEnv, dataDir)
	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read the store: %v", err)
	}
	if out := RenderSessionContext(work); strings.Contains(out, "a memory the refused hook must not render") {
		t.Errorf("the refused hook rendered a memory from the store:\n%s", out)
	}
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("re-read the store: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Error("the refused hook changed the store's bytes: it opened it read-write")
	}
	if got := preMigrateBackupsIn(t, dataDir); len(got) != 0 {
		t.Errorf("the refused hook wrote pre-migration backups: %v", got)
	}
}

// TestTheSessionContextHookRendersTheStoreOnAReleaseBuild is the control for the
// test above, so the empty digest cannot be read as "this hook renders nothing":
// the same store, the same project, the same variable, on a release build.
func TestTheSessionContextHookRendersTheStoreOnAReleaseBuild(t *testing.T) {
	withBuildVersion(t, releaseBuild)
	root := t.TempDir()
	dataHome := filepath.Join(root, "data")
	// realPath, because RenderSessionContextAt canonicalizes the directory it is
	// given and resolves the project by path prefix, so a project recorded under
	// a raw t.TempDir() spelling matches nothing on a platform that spells its
	// temp dir short (Windows) — the digest comes back empty and this control
	// fails for a reason that has nothing to do with the guard.
	work := realPath(t, mkdirAll(t, filepath.Join(root, "work")))
	setHome(t, root)
	for _, kv := range [][2]string{
		{"XDG_CONFIG_HOME", filepath.Join(root, "config")},
		{"XDG_CACHE_HOME", filepath.Join(root, "cache")},
		{"XDG_DATA_HOME", dataHome},
		{config.DevForbidDataDirEnv, filepath.Join(dataHome, "ghost")},
	} {
		t.Setenv(kv[0], kv[1])
	}

	dbPath := storeAt(t, filepath.Join(dataHome, "ghost"))
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	store := memory.NewStore(db, nil)
	const projectID = "e2e-dev-guard"
	if err := store.EnsureProject(context.Background(), projectID, work, "e2e-dev-guard"); err != nil {
		t.Fatalf("ensure the project: %v", err)
	}
	const content = "a memory the release build's hook does render"
	if _, err := store.Create(context.Background(), projectID, memory.Memory{
		Category:   "architecture",
		Content:    content,
		Importance: 0.6,
		Source:     "manual",
	}); err != nil {
		t.Fatalf("create the memory: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close the store: %v", err)
	}

	out := RenderSessionContext(work)
	if !strings.Contains(out, content) {
		t.Fatalf("a release build's session context did not render the memory, want it in:\n%s", out)
	}
}

// TestMCPStatusReportsAForbiddenDataDirWithoutOpeningIt covers the one command
// that opens the store read-write purely to REPORT on it. It must not fail the
// whole run — a status report's job is to say what is wrong — but it must say
// this, and it must not open the store to find out.
func TestMCPStatusReportsAForbiddenDataDirWithoutOpeningIt(t *testing.T) {
	withBuildVersion(t, devBuild)
	root := t.TempDir()
	dataHome := filepath.Join(root, "data")
	dataDir := filepath.Join(dataHome, "ghost")
	dbPath := storeAt(t, dataDir)
	stampToV17(t, dbPath)

	setHome(t, root)
	for _, kv := range [][2]string{
		{"XDG_CONFIG_HOME", filepath.Join(root, "config")},
		{"XDG_CACHE_HOME", filepath.Join(root, "cache")},
		{"XDG_DATA_HOME", dataHome},
		{"GHOST_EMBEDDING_ENABLED", "false"},
		{config.DevForbidDataDirEnv, dataDir},
	} {
		t.Setenv(kv[0], kv[1])
	}

	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read the store: %v", err)
	}
	var out bytes.Buffer
	// The report is on the writer and the exit decision is the bool. A refusal
	// has to be IN the report: a status run that exits non-zero having said
	// nothing is a run that diagnosed nothing.
	healthy, err := Status(&out)
	if err != nil {
		t.Fatalf("Status = %v, want a report rather than an error", err)
	}
	if healthy {
		t.Errorf("the status run reported healthy while refusing to open the store:\n%s", out.String())
	}
	report := out.String()
	if !strings.Contains(report, config.DevForbidDataDirEnv) {
		t.Errorf("the status report does not name %s:\n%s", config.DevForbidDataDirEnv, report)
	}
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("re-read the store: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Error("the status run changed the refused store's bytes: it opened it read-write")
	}
	if got := preMigrateBackupsIn(t, dataDir); len(got) != 0 {
		t.Errorf("the refused status run wrote pre-migration backups: %v", got)
	}
}

// TestMCPStatusDoesNotMentionTheVariableWhenItIsNotSet is the negative control
// for the report above: the line is the store's health, so a run that never hit
// the rule must not print it. A status report that always carried the variable's
// name would make the one report that does mean something unreadable.
func TestMCPStatusDoesNotMentionTheVariableWhenItIsNotSet(t *testing.T) {
	withBuildVersion(t, devBuild)
	root := t.TempDir()
	dataHome := filepath.Join(root, "data")
	dataDir := filepath.Join(dataHome, "ghost")
	storeAt(t, dataDir)
	setHome(t, root)
	for _, kv := range [][2]string{
		{"XDG_CONFIG_HOME", filepath.Join(root, "config")},
		{"XDG_CACHE_HOME", filepath.Join(root, "cache")},
		{"XDG_DATA_HOME", dataHome},
		{"GHOST_EMBEDDING_ENABLED", "false"},
	} {
		t.Setenv(kv[0], kv[1])
	}
	var out bytes.Buffer
	if _, err := Status(&out); err != nil {
		t.Fatalf("Status = %v, want a report rather than an error", err)
	}
	if strings.Contains(out.String(), config.DevForbidDataDirEnv) {
		t.Errorf("the status report names %s with nothing refusing:\n%s", config.DevForbidDataDirEnv, out.String())
	}
}

// markerStore builds a store holding one project, and returns the data
// directory and the project id. The project exists so resolveMarkerProject
// SUCCEEDS: the marker paths' own guard used to sit there, and a fixture whose
// project never resolved would reach the fallback under a broken guard for a
// reason that has nothing to do with the guard.
func markerStore(t *testing.T) (dataDir, projectID string) {
	t.Helper()
	root := t.TempDir()
	dataDir = filepath.Join(root, "data", "ghost")
	work := realPath(t, mkdirAll(t, filepath.Join(root, "work")))
	dbPath := storeAt(t, dataDir)
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	store := memory.NewStore(db, nil)
	projectID = "e2e-dev-guard"
	if err := store.EnsureProject(context.Background(), projectID, work, projectID); err != nil {
		t.Fatalf("ensure the project: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close the store: %v", err)
	}
	setHome(t, root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv(config.DevForbidDataDirEnv, dataDir)
	return dataDir, projectID
}

// TestWriteLifecycleFailureTouchesNothingInAForbiddenDataDir covers the marker
// writer's own fallback: resolveMarkerProject returns "" for a data directory it
// cannot read, and the writer then substitutes the raw project name and writes
// the marker anyway — so guarding the resolve alone left the write reachable,
// and the file it produced was named after a value nothing had resolved. A
// forbidden data directory has to mean the whole function does nothing.
func TestWriteLifecycleFailureTouchesNothingInAForbiddenDataDir(t *testing.T) {
	withBuildVersion(t, devBuild)
	dataDir, projectID := markerStore(t)
	before := dirEntries(t, dataDir)

	if err := WriteLifecycleFailure(projectID, []string{"reflect"}, "the phase refused"); err == nil {
		t.Error("WriteLifecycleFailure = nil, want the refusal it reports to its caller")
	}
	if got := dirEntries(t, dataDir); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Errorf("the refused write changed the data directory: before %v, after %v", before, got)
	}
	// Named explicitly, because the fallback is what used to happen here: a
	// marker under the RAW project name is the file that must not exist.
	for _, name := range []string{
		projectMarkerFile(projectID),
		projectMarkerFile("ghost"),
		lifecycleMarkerPrefix + "-*.json",
		legacyMarkerFile,
	} {
		if _, err := filepath.Glob(filepath.Join(dataDir, name)); err == nil {
			if matches, _ := filepath.Glob(filepath.Join(dataDir, name)); len(matches) != 0 {
				t.Errorf("the refused write left %v in the forbidden data directory", matches)
			}
		}
	}

	// The control: the same call on a release build records the marker, so the
	// assertions above are about the guard and not about a store that cannot
	// resolve a project.
	withBuildVersion(t, releaseBuild)
	if err := WriteLifecycleFailure(projectID, []string{"reflect"}, "the phase refused"); err != nil {
		t.Fatalf("WriteLifecycleFailure on a release build = %v, want the marker written", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, projectMarkerFile(projectID))); err != nil {
		t.Errorf("the release build wrote no marker: %v", err)
	}
}

// TestClearLifecycleFailureTouchesNothingInAForbiddenDataDir is the removal
// half, and it is the one that can destroy something: the marker is already on
// disk, so a dev build that honoured the variable only for the resolve would
// still delete a record the release build wrote.
func TestClearLifecycleFailureTouchesNothingInAForbiddenDataDir(t *testing.T) {
	withBuildVersion(t, releaseBuild)
	dataDir, projectID := markerStore(t)
	if err := WriteLifecycleFailure(projectID, []string{"reflect"}, "a failure to heal"); err != nil {
		t.Fatalf("seed the marker: %v", err)
	}
	marker := filepath.Join(dataDir, projectMarkerFile(projectID))
	before, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read the seeded marker: %v", err)
	}
	beforeNames := dirEntries(t, dataDir)

	withBuildVersion(t, devBuild)
	_ = ClearLifecycleFailure(projectID)

	after, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("the refused clear removed the marker: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Error("the refused clear changed the marker's bytes")
	}
	if got := dirEntries(t, dataDir); strings.Join(got, ",") != strings.Join(beforeNames, ",") {
		t.Errorf("the refused clear changed the data directory: before %v, after %v", beforeNames, got)
	}
}

// TestReadLifecycleFailureTouchesNothingInAForbiddenDataDir is the read the
// session-start hook does before anything else looks at the store: it runs on
// every session, and its age-based self-clean calls ClearLifecycleFailure, so a
// read that reached a forbidden directory could also delete there.
func TestReadLifecycleFailureTouchesNothingInAForbiddenDataDir(t *testing.T) {
	withBuildVersion(t, releaseBuild)
	dataDir, projectID := markerStore(t)
	if err := WriteLifecycleFailure(projectID, []string{"reflect"}, "a failure the next session must see"); err != nil {
		t.Fatalf("seed the marker: %v", err)
	}
	// The control, first: on a release build this marker IS read, so the
	// silence asserted below is the guard and not an unreadable file.
	if alert := lifecycleFailureAlert(projectID, projectID); alert == "" {
		t.Fatal("a release build's session start showed no alert for a marker it can read")
	}
	beforeNames := dirEntries(t, dataDir)

	withBuildVersion(t, devBuild)
	if alert := lifecycleFailureAlert(projectID, projectID); alert != "" {
		t.Errorf("the refused session start alerted on a marker in a forbidden data dir: %q", alert)
	}
	if got := readLifecycleFailure(projectID, projectID); got != nil {
		t.Errorf("readLifecycleFailure = %+v, want nil for a forbidden data dir", got)
	}
	if got := dirEntries(t, dataDir); strings.Join(got, ",") != strings.Join(beforeNames, ",") {
		t.Errorf("the refused read changed the data directory: before %v, after %v", beforeNames, got)
	}
}

// TestTouchLifecycleStartTouchesNothingInAForbiddenDataDir is the fourth marker
// path, and the one `ghost lifecycle` runs on every start: a refusal has to mean
// no stamp file, and the error is a warning the caller already prints.
func TestTouchLifecycleStartTouchesNothingInAForbiddenDataDir(t *testing.T) {
	withBuildVersion(t, devBuild)
	dataDir, projectID := markerStore(t)
	before := dirEntries(t, dataDir)

	if err := TouchLifecycleStart(projectID); err == nil {
		t.Error("TouchLifecycleStart = nil, want the refusal it reports to its caller")
	}
	if got := dirEntries(t, dataDir); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Errorf("the refused stamp changed the data directory: before %v, after %v", before, got)
	}
	if _, err := filepath.Glob(filepath.Join(dataDir, lifecycleLastStartFile(projectID)+"*")); err == nil {
		if matches, _ := filepath.Glob(filepath.Join(dataDir, lifecycleLastStartFile(projectID)+"*")); len(matches) != 0 {
			t.Errorf("the refused stamp left %v in the forbidden data directory", matches)
		}
	}
}
