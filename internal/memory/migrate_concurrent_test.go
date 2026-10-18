package memory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// laggingStore builds a store one schema step behind this build, holding one
// project and one memory, and returns its path. The stamp alone is lowered:
// every migration step is idempotent (each checks for what it adds), so a
// current store stamped schemaVersion-1 enters migrate() — and therefore takes
// the pre-migration backup — exactly as the previous release's store does,
// without this fixture having to know what the newest step adds.
func laggingStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ghost.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO projects (id, path, name) VALUES ('lag-p', '/lag-p', 'lag-p')`,
		`INSERT INTO memories (id, project_id, category, content, source) VALUES ('lag-m', 'lag-p', 'fact', 'a row that must survive the migration', 'mcp')`,
		fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion-1),
	} {
		if _, err := db.Exec(stmt); err != nil {
			_ = db.Close()
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if n := len(preMigrateBackupsOf(t, path)); n != 0 {
		t.Fatalf("fixture already has %d pre-migration backups", n)
	}
	return path
}

// preMigrateBackupsOf lists the "<db>.pre-migrate-<unix>" copies beside path.
func preMigrateBackupsOf(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	prefix := filepath.Base(path) + ".pre-migrate-"
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			out = append(out, e.Name())
		}
	}
	return out
}

// assertMigratedOnce is the outcome a round of concurrent openers must leave:
// the store is current, its rows are intact, and exactly ONE pre-migration copy
// exists. The count is what tells "one opener migrated and the other saw the
// result" from "both migrated": the steps are idempotent, so a second migration
// leaves an identical store, and the only trace it leaves is a second backup —
// or, inside the same wall-clock second, a refused one.
func assertMigratedOnce(t *testing.T, round int, path string) {
	t.Helper()
	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("round %d: store not openable afterwards: %v", round, err)
	}
	defer db.Close() //nolint:errcheck
	var v, n int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("round %d: user_version: %v", round, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM memories WHERE id = 'lag-m'`).Scan(&n); err != nil {
		t.Fatalf("round %d: count: %v", round, err)
	}
	if v != schemaVersion || n != 1 {
		t.Errorf("round %d: user_version=%d (want %d), surviving rows=%d (want 1)", round, v, schemaVersion, n)
	}
	if got := preMigrateBackupsOf(t, path); len(got) != 1 {
		t.Errorf("round %d: %d pre-migration backups, want exactly 1 (one opener migrates, the others skip both): %v",
			round, len(got), got)
	}
}

// TestConcurrentOpensOfALaggingStoreMigrateOnce opens a store one version back
// from several goroutines at once. Each OpenDB is its own *sql.DB and so its own
// SQLite connection and its own lock-file handle, which is how separate
// processes meet the file. Every opener must succeed, and exactly one of them
// may back up and migrate: the decision is made under the migration lock, from
// a user_version read once the lock is held.
func TestConcurrentOpensOfALaggingStoreMigrateOnce(t *testing.T) {
	const rounds = 10
	const openers = 3
	for r := 0; r < rounds; r++ {
		path := laggingStore(t)
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, openers)
		dbs := make([]*sql.DB, openers)
		for i := 0; i < openers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				dbs[i], errs[i] = OpenDB(path)
			}(i)
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Errorf("round %d: opener %d failed: %v", r, i, err)
				continue
			}
			_ = dbs[i].Close()
		}
		assertMigratedOnce(t, r, path)
	}
}

// TestMultiProcessConcurrentOpenOfALaggingStore is the same race between real
// processes: two editors each spawning `ghost mcp` after an upgrade, or an MCP
// server and a lifecycle child. Each child is the built multiproc helper's open
// role (never a self-exec of this test binary); both report ready, the parent
// releases them together, and each calls memory.OpenDB on the lagging store.
func TestMultiProcessConcurrentOpenOfALaggingStore(t *testing.T) {
	if testing.Short() {
		t.Skip("multi-process test: builds and runs a helper program")
	}
	helper := helperBinary(t)
	home := t.TempDir()
	env := append(os.Environ(), "HOME="+home, "USERPROFILE="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, "config"), "XDG_DATA_HOME="+filepath.Join(home, "data"),
		"XDG_STATE_HOME="+filepath.Join(home, "state"))

	const rounds = 5
	labels := []string{"open-a", "open-b"}
	for r := 0; r < rounds; r++ {
		path := laggingStore(t)
		barrier := t.TempDir()
		type result struct {
			out []byte
			err error
		}
		results := make([]result, len(labels))
		var wg sync.WaitGroup
		for i, label := range labels {
			cmd := exec.Command(helper, "-role", "open", "-db", path, "-label", label, "-barrier", barrier)
			cmd.Env = env
			wg.Add(1)
			go func(i int, cmd *exec.Cmd) {
				defer wg.Done()
				out, err := cmd.Output()
				results[i] = result{out, err}
			}(i, cmd)
		}
		deadline := time.Now().Add(30 * time.Second)
		for _, label := range labels {
			for {
				if _, err := os.Stat(filepath.Join(barrier, "ready-"+label)); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("round %d: %s never reported ready", r, label)
				}
				time.Sleep(2 * time.Millisecond)
			}
		}
		if err := os.WriteFile(filepath.Join(barrier, "go"), nil, 0o600); err != nil {
			t.Fatalf("signal go: %v", err)
		}
		wg.Wait()

		for i, res := range results {
			var rep struct {
				KV   map[string]string `json:"kv"`
				Errs []string          `json:"errs"`
			}
			if err := json.Unmarshal(res.out, &rep); err != nil {
				t.Errorf("round %d: %s: unreadable report (%v): %v\n%s", r, labels[i], res.err, err, res.out)
				continue
			}
			if res.err != nil || len(rep.Errs) > 0 {
				t.Errorf("round %d: %s failed to open the lagging store: %v %v", r, labels[i], res.err, rep.Errs)
				continue
			}
			if got := rep.KV["user_version"]; got != strconv.Itoa(schemaVersion) {
				t.Errorf("round %d: %s saw user_version %q after its open, want %d", r, labels[i], got, schemaVersion)
			}
		}
		assertMigratedOnce(t, r, path)
	}
}

// TestCurrentStoreOpenTakesNoMigrationLock pins the other half of the lock's
// scope: only a lagging store is serialised. An open of a current store — every
// open but the first after an upgrade — neither waits on the lock nor leaves its
// file behind, while the migrating open does leave it (it is never removed).
func TestCurrentStoreOpenTakesNoMigrationLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost.db")
	for i := 0; i < 2; i++ {
		db, err := OpenDB(path)
		if err != nil {
			t.Fatalf("OpenDB #%d: %v", i, err)
		}
		_ = db.Close()
	}
	if _, err := os.Lstat(migrationLockPath(path)); !os.IsNotExist(err) {
		t.Fatalf("a fresh and a current open created %s (err=%v)", migrationLockPath(path), err)
	}

	lagging := laggingStore(t)
	db, err := OpenDB(lagging)
	if err != nil {
		t.Fatalf("OpenDB lagging: %v", err)
	}
	_ = db.Close()
	if info, err := os.Lstat(migrationLockPath(lagging)); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("the migrating open left no lock file at %s (err=%v)", migrationLockPath(lagging), err)
	}
}
