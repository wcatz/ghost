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

// TestImportMemoriesOpensAForbiddenDataDirOnAReleaseBuild is the reason the
// variable can be exported into a developer's shell at all. The same refused
// store, on the artifact a release published, is opened and MIGRATED by the
// ordinary (non-dry-run) import — because that is what a release is for, and a
// guard that stopped it would break the install the developer also uses. The
// open is the read-write one: a dry run is read-only on every build (#1049).
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
	if _, err := importMemories(&out, false); err != nil {
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

// TestRunStopsOnAForbiddenDataDirBeforeReportingDone is the command-level half
// of the guard: a development build pointed at a refused store must stop rather
// than print "Done!" and exit 0. Both places `ghost mcp init` resolves the
// directory — the scratch root at step 2 and the import at step 9 — resolve
// through the same guard and both downgrade a failure to a "(continuing)"
// warning, so the refusal was reported and the run still claimed success: a
// script branching on the exit code could not tell the install's
// data-directory step had been refused.
//
// The recording `claude` stub is the proof the stop is BEFORE the wiring, not
// merely before the "Done!" line: the run must not reach step 4 and register
// the MCP server, so a refused install leaves no half-built client behind.
func TestRunStopsOnAForbiddenDataDirBeforeReportingDone(t *testing.T) {
	withBuildVersion(t, devBuild)
	home, binDir := installEnv(t)
	dataDir := filepath.Join(home, "data", "ghost")
	t.Setenv(config.DevForbidDataDirEnv, dataDir)

	var out bytes.Buffer
	err := Run(&out, false)
	if err == nil {
		t.Fatalf("Run accepted a forbidden data directory, want a refusal; it printed:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), config.DevForbidDataDirEnv) {
		t.Errorf("the refusal does not name %s: %v", config.DevForbidDataDirEnv, err)
	}
	// The data home does not exist yet — run must not have created it — so the
	// refusal names the path with its deepest existing ancestor resolved.
	if want := filepath.Join(realPath(t, home), "data", "ghost"); !strings.Contains(err.Error(), want) {
		t.Errorf("the refusal does not name the data directory %s: %v", want, err)
	}
	if strings.Contains(out.String(), "Done!") {
		t.Errorf("init reported success after refusing the data directory:\n%s", out.String())
	}
	requireMissing(t, binDir, "calls")
	// And the refused directory itself was not created: resolving it for the
	// refusal is a pure query.
	if _, statErr := os.Stat(dataDir); !os.IsNotExist(statErr) {
		t.Errorf("the refused run created the data directory, stat error: %v", statErr)
	}

	// The control: the same sandbox with nothing refused installs and reports
	// success, so the assertions above are about the guard rather than about an
	// init that cannot run in this environment at all.
	t.Setenv(config.DevForbidDataDirEnv, "")
	var ok bytes.Buffer
	if err := Run(&ok, false); err != nil {
		t.Fatalf("Run with nothing refused = %v, want the install; it printed:\n%s", err, ok.String())
	}
	if !strings.Contains(ok.String(), "Done!") {
		t.Errorf("an unrefused install did not report success:\n%s", ok.String())
	}
}
