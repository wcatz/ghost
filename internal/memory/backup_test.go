package memory

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// backupTestStore opens a database in a temp dir and returns a store over it.
// The path is a real file rather than :memory: because a backup is a file
// operation — an in-memory database has no file for VACUUM INTO to read.
func backupTestStore(t *testing.T) *Store {
	t.Helper()
	return backupTestStoreAt(t, filepath.Join(t.TempDir(), "ghost.db"))
}

func backupTestStoreAt(t *testing.T, dbPath string) *Store {
	t.Helper()
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
}

// seedBackupFixture gives the store one project, four memories (one resolved,
// one pinned with provenance and scope, one companion to the decision), a
// link, a task and a decision, so the backup's counts have to come from rows
// that are not all the same kind.
func seedBackupFixture(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	if err := s.EnsureProjectWithRepo(ctx, "p1", "/src/p1", "p1", "git@github.com:wcatz/p1.git"); err != nil {
		t.Fatalf("EnsureProjectWithRepo: %v", err)
	}
	conf := 0.75
	first, err := s.Create(ctx, "p1", Memory{Category: "gotcha", Content: "first", Importance: 0.5, Source: "manual"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Create writes the provenance and scope columns but not the pin, so the
	// pin is set the way a user sets it — through the pin API — which is also
	// what makes this fixture's pinned row a real one.
	second, err := s.Create(ctx, "p1", Memory{
		Category:   "architecture",
		Content:    "second",
		Importance: 0.9,
		Source:     "mcp",
		Tags:       []string{"one", "two"},
		Agent:      "opencode",
		SessionID:  "sess-1",
		SourceRef:  "ref-1",
		Confidence: &conf,
		Scope:      map[string]string{"environment": "production"},
	})
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}
	if err := s.TogglePin(ctx, second, true); err != nil {
		t.Fatalf("TogglePin: %v", err)
	}
	if err := s.CreateLink(ctx, first, second, "related", 0.5, "auto"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	if _, err := s.CreateTask(ctx, "p1", "task", "desc", 2); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	// RecordDecision also writes a companion memory, so the fixture ends with
	// four: the two created above, the resolved one, and this companion. The
	// count is asserted rather than computed so a change to what the backup
	// counts is visible here instead of being silently absorbed.
	if _, _, _, err := s.RecordDecision(ctx, "p1", "title", "decision", "why", nil, nil); err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	// A resolved memory, so the backup carries resolved_at rather than only
	// live rows.
	resolved, err := s.Create(ctx, "p1", Memory{Category: "fact", Content: "resolved", Source: "chat"})
	if err != nil {
		t.Fatalf("Create resolved: %v", err)
	}
	if _, err := s.SetResolved(ctx, []string{resolved}); err != nil {
		t.Fatalf("SetResolved: %v", err)
	}
}

// TestStoreBackupSnapshotIsReadableAndComplete: `ghost backup` must write a
// database that opens and carries every row the live store held. A copy that
// opened but had lost the last writes would be worse than no copy at all,
// because it looks restorable.
func TestStoreBackupSnapshotIsReadableAndComplete(t *testing.T) {
	store := backupTestStore(t)
	seedBackupFixture(t, store)

	dest := filepath.Join(t.TempDir(), "snapshot.db")
	got, err := store.Backup(context.Background(), dest)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if got.Path != dest {
		t.Errorf("Path = %q, want %q", got.Path, dest)
	}
	want := BackupCounts{Projects: 1, Memories: 4, MemoryLinks: 1, Tasks: 1, Decisions: 1}
	if got.Counts != want {
		t.Errorf("Counts = %+v, want %+v", got.Counts, want)
	}
	if got.Bytes <= 0 {
		t.Errorf("Bytes = %d, want the snapshot to report its size", got.Bytes)
	}

	// The snapshot is a database, not a byte copy: it opens on its own and
	// holds the same rows, including the provenance and resolved_at a restore
	// depends on.
	bdb, err := OpenReadDB(dest)
	if err != nil {
		t.Fatalf("OpenReadDB(%s): %v", dest, err)
	}
	defer func() { _ = bdb.Close() }()

	var resolved, pinned, scoped int
	row := bdb.QueryRow(`SELECT
		coalesce(sum(CASE WHEN resolved_at IS NOT NULL THEN 1 ELSE 0 END), 0),
		coalesce(sum(pinned), 0),
		coalesce(sum(CASE WHEN scope IS NOT NULL THEN 1 ELSE 0 END), 0)
		FROM memories`)
	if err := row.Scan(&resolved, &pinned, &scoped); err != nil {
		t.Fatalf("count snapshot memories: %v", err)
	}
	if resolved != 1 || pinned != 1 || scoped != 1 {
		t.Errorf("snapshot memories: resolved=%d pinned=%d scoped=%d, want 1/1/1", resolved, pinned, scoped)
	}

	// The live database is untouched and still holds the same rows: a backup
	// is a read of the store, never a truncation of it.
	if n, err := store.CountMemories(context.Background(), "p1"); err != nil || n != 4 {
		t.Errorf("live memories = %d (err %v), want 4", n, err)
	}
}

// TestStoreBackupIsConsistentUnderWrites: the copy is taken while a second
// connection to the SAME file is committing. VACUUM INTO reads one snapshot, so
// the result must be a database that opens and whose reported counts agree with
// the rows it holds — never a torn mixture, never fewer rows than existed
// beforehand, and never a number read from a store that has moved on since.
func TestStoreBackupIsConsistentUnderWrites(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ghost.db")
	store := backupTestStoreAt(t, dbPath)
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	for i := 0; i < 20; i++ {
		if _, err := store.Create(ctx, "p1", Memory{Category: "fact", Content: "seed", Source: "mcp"}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	// A second handle on the SAME file, which is what a live MCP server holding
	// the store open is: one database, two connections, commits arriving while
	// the backup runs. A separate file would make this test prove nothing, since
	// nothing would be contending for the write lock.
	writer := backupTestStoreAt(t, dbPath)
	var concurrent atomic.Int64
	stop := make(chan struct{})
	done := make(chan struct{})
	// Closed once the writer has committed at least once, so the backup cannot
	// start before the writer is running. Without this the "committed nothing
	// during the backup" assertion below is a statement about the scheduler: the
	// writer goroutine may simply never have been scheduled, which is a
	// different failure from a backup that blocked every writer.
	writing := make(chan struct{})
	var announced bool
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := writer.Create(ctx, "p1", Memory{Category: "fact", Content: "concurrent", Source: "mcp"}); err == nil {
				if !announced {
					announced = true
					close(writing)
				}
				concurrent.Add(1)
			}
		}
	}()
	<-writing

	dest := filepath.Join(t.TempDir(), "snapshot.db")
	res, err := store.Backup(ctx, dest)
	if err != nil {
		close(stop)
		<-done
		t.Fatalf("Backup under concurrent writes: %v", err)
	}

	// The report must describe the file that was written, not the live store.
	// The snapshot is immutable, so the comparison below is exact, and the
	// writer is still running so the live count is free to diverge.
	bdb, openErr := OpenReadDB(dest)
	if openErr != nil {
		close(stop)
		<-done
		t.Fatalf("OpenReadDB after concurrent writes: %v", openErr)
	}
	var snapshotRows int
	countErr := bdb.QueryRow(`SELECT count(*) FROM memories`).Scan(&snapshotRows)
	// SQLite's own verdict on the copy, which is the part of "consistent under
	// writes" a row count cannot show. A torn page can hold the right number of
	// rows and still be unrestorable, and this is the check that says so; the
	// concurrent-writer property is worthless without it, because a snapshot
	// nobody can open is not a copy.
	var integrity string
	integrityErr := bdb.QueryRow(`PRAGMA integrity_check`).Scan(&integrity)
	_ = bdb.Close()
	// Commits after the backup returned are excluded: what matters is that the
	// writer was running *across* the vacuum.
	writesDuringBackup := concurrent.Load()
	_ = writesDuringBackup
	close(stop)
	<-done
	if countErr != nil {
		t.Fatalf("count snapshot memories: %v", countErr)
	}
	if integrityErr != nil {
		t.Fatalf("integrity_check the snapshot taken under concurrent writes: %v", integrityErr)
	}
	if integrity != "ok" {
		t.Errorf("integrity_check = %q on the snapshot taken while a writer was committing, want \"ok\"", integrity)
	}
	if res.Counts.Memories != snapshotRows {
		t.Errorf("report says %d memories, the snapshot holds %d — the counts must describe the file, not the live store",
			res.Counts.Memories, snapshotRows)
	}
	// A snapshot that lost the pre-existing rows would be a torn copy rather
	// than an earlier consistent one.
	if snapshotRows < 20 {
		t.Errorf("snapshot memories = %d, want at least the 20 rows present before the writer started", snapshotRows)
	}
	// The writer must have been running for any of the above to mean anything,
	// and `<-writing` above is what establishes it: a test that never actually
	// contended would pass a torn copy.
	//
	// How many commits landed *during* the vacuum is deliberately not asserted.
	// A consistent snapshot is allowed to block writers for its whole duration —
	// that is the mechanism, not a defect — so a backup that admits no writer at
	// all is a correct outcome, and a test that failed on it would be reporting a
	// scheduler as a bug. The assertion that matters is the one above: the report
	// describes the file.
}

// TestStoreBackupRefusesToOverwrite: a backup that silently replaced an
// existing file would destroy the previous copy — the one a user is relying on
// while the new one runs.
func TestStoreBackupRefusesToOverwrite(t *testing.T) {
	store := backupTestStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "snapshot.db")
	if err := os.WriteFile(dest, []byte("precious"), 0o600); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}
	if _, err := store.Backup(ctx, dest); err == nil {
		t.Fatal("Backup must refuse an existing path")
	} else if !strings.Contains(err.Error(), dest) {
		t.Errorf("error must name the path it refused, got %v", err)
	}
	body, err := os.ReadFile(dest)
	if err != nil || string(body) != "precious" {
		t.Errorf("existing file changed: %q (err %v)", body, err)
	}

	// A symlink at the destination whose target does not exist is refused too.
	// A stat-based check that follows the link sees nothing there and reports
	// the path as free, and the copy is then written through the link to
	// wherever it points — which for a --out path is not necessarily inside the
	// directory the user named.
	target := filepath.Join(t.TempDir(), "elsewhere.db")
	link := filepath.Join(t.TempDir(), "link.db")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := store.Backup(ctx, link); err == nil {
		t.Error("Backup must refuse a dangling symlink at the destination")
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("Backup wrote through the symlink to its target")
	}
}

// TestStoreBackupMissingParentIsAnError: a path whose directory does not exist
// has to fail loudly rather than leave the user believing a snapshot exists,
// and the error has to name the directory — SQLite's own "unable to open
// database file" reads as a problem with the database.
func TestStoreBackupMissingParentIsAnError(t *testing.T) {
	store := backupTestStore(t)
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	dest := filepath.Join(missing, "snapshot.db")
	_, err := store.Backup(context.Background(), dest)
	if err == nil {
		t.Fatal("Backup must fail when the destination directory does not exist")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error = %v, want it to name the missing directory", err)
	}
	// "does not exist", not "directory": the OS's own message for a failed
	// create is "no such file or directory", so a word match on "directory"
	// would pass whether or not the explicit check ran.
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error = %v, want it to say the directory does not exist", err)
	}
}

// TestBackupFileNameIsUTCAndFilenameSafe: the default name lands beside the
// database, carries a UTC timestamp a user can sort on, and adds no character
// that is illegal in a Windows path — the same shape the pre-migrate backup
// uses, so one convention covers both.
func TestBackupFileNameIsUTCAndFilenameSafe(t *testing.T) {
	// 2026-09-26T15:32:07Z, deliberately not a local time: a backup name has
	// to be unambiguous when two machines' copies are compared.
	at := time.Date(2026, 9, 26, 15, 32, 7, 0, time.UTC)
	got := BackupFileName("/data/ghost.db", at)
	if want := "/data/ghost.db.backup-20260926T153207Z"; got != want {
		t.Errorf("BackupFileName = %q, want %q", got, want)
	}
	for _, r := range filepath.Base(got) {
		if strings.ContainsRune(`:\/:*?"<>| `, r) {
			t.Errorf("BackupFileName base %q contains %q, illegal in a Windows path", filepath.Base(got), r)
		}
	}
	// A local time in a zone ahead of UTC must still render as UTC, or two
	// machines would disagree about when a backup was taken.
	east := time.Date(2026, 9, 26, 15, 32, 7, 0, time.FixedZone("east", 5*3600))
	if got := BackupFileName("ghost.db", east); got != "ghost.db.backup-20260926T103207Z" {
		t.Errorf("BackupFileName with a non-UTC zone = %q, want the UTC instant", got)
	}
}
