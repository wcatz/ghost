package maintenance

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func writeRetentionFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func retentionBackups(t *testing.T, dbPath string, stamps ...int64) []string {
	t.Helper()
	paths := make([]string, 0, len(stamps))
	for _, stamp := range stamps {
		path := dbPath + ".pre-migrate-" + strconv.FormatInt(stamp, 10)
		writeRetentionFile(t, path, "backup-"+strconv.FormatInt(stamp, 10))
		when := time.Unix(stamp, 0)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	return paths
}

func TestPrunePreMigrateBackupsKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ghost.db")
	writeRetentionFile(t, dbPath, "live database")
	backups := retentionBackups(t, dbPath, 10, 20, 30, 40)

	removed, err := PrunePreMigrateBackups(dbPath, 2)
	if err != nil {
		t.Fatalf("PrunePreMigrateBackups: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	for _, path := range backups[:2] {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("old backup %s survived: %v", path, err)
		}
	}
	for _, path := range backups[2:] {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("new backup %s was removed: %v", path, err)
		}
	}
	if data, err := os.ReadFile(dbPath); err != nil || string(data) != "live database" {
		t.Fatalf("live database changed: data=%q err=%v", data, err)
	}
}

func TestPrunePreMigrateBackupsSkipsHeldAndLiveFiles(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ghost.db")
	writeRetentionFile(t, dbPath, "live database")
	backups := retentionBackups(t, dbPath, 10, 20, 30)
	held := backups[0]
	old := backups[1]
	newest := backups[2]

	// A hard link wearing the backup name must not let retention remove the
	// live database inode through the backup path.
	linked := dbPath + ".pre-migrate-40"
	if err := os.Link(dbPath, linked); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	f, err := os.Open(held)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })

	oldProbe := openFileProbe
	openFileProbe = func(path string) (bool, error) {
		if path == held {
			return true, nil
		}
		return false, nil
	}
	t.Cleanup(func() { openFileProbe = oldProbe })

	if _, err := PrunePreMigrateBackups(dbPath, 1); err != nil {
		t.Fatalf("PrunePreMigrateBackups: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("unheld old backup %s survived: %v", old, err)
	}
	if _, err := os.Stat(held); err != nil {
		t.Errorf("held backup %s was removed: %v", held, err)
	}
	if _, err := os.Stat(newest); err != nil {
		t.Errorf("newest backup %s was removed: %v", newest, err)
	}
	if _, err := os.Stat(linked); err != nil {
		t.Errorf("hard-linked live database was removed: %v", err)
	}
}

func TestRotateLogsBoundsKnownLogs(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"lifecycle.log", "obsidian-sync.log", "reflect.log", "resolve.log", "supersede.log"} {
		writeRetentionFile(t, filepath.Join(dir, name), "0123456789")
	}

	rotated, err := RotateLogs(dir, 4)
	if err != nil {
		t.Fatalf("RotateLogs: %v", err)
	}
	if rotated != 5 {
		t.Fatalf("rotated = %d, want 5", rotated)
	}
	for _, name := range []string{"lifecycle.log", "obsidian-sync.log", "reflect.log", "resolve.log", "supersede.log"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "6789" {
			t.Errorf("%s = %q, want newest tail %q", name, data, "6789")
		}
	}
}

func TestRotateLogsLeavesHeldLogUntouched(t *testing.T) {
	dir := t.TempDir()
	heldPath := filepath.Join(dir, "lifecycle.log")
	writeRetentionFile(t, heldPath, "0123456789")
	writeRetentionFile(t, filepath.Join(dir, "obsidian-sync.log"), "0123456789")

	oldProbe := openFileProbe
	openFileProbe = func(path string) (bool, error) {
		return path == heldPath, nil
	}
	t.Cleanup(func() { openFileProbe = oldProbe })

	if _, err := RotateLogs(dir, 4); err != nil {
		t.Fatalf("RotateLogs: %v", err)
	}
	if data, err := os.ReadFile(heldPath); err != nil || string(data) != "0123456789" {
		t.Fatalf("held log changed: data=%q err=%v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "obsidian-sync.log")); err != nil || string(data) != "6789" {
		t.Fatalf("unheld log was not rotated: data=%q err=%v", data, err)
	}
}

func TestReapStaleProcessFilesLeavesHeldLock(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "reflect-held.pid")
	lockPath := pidPath + ".lock"
	writeRetentionFile(t, pidPath, "999999999")

	lock, err := openProcessLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	locked, err := tryLockExclusive(lock)
	if err != nil || !locked {
		t.Fatalf("could not hold process lock: locked=%v err=%v", locked, err)
	}
	t.Cleanup(func() {
		_ = unlockProcessLock(lock)
		_ = lock.Close()
	})

	if _, err := ReapStaleProcessFiles(dir); err != nil {
		t.Fatalf("ReapStaleProcessFiles: %v", err)
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Fatalf("held claim's PID file was removed: %v", err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("held claim's lock file was removed: %v", err)
	}
}

func TestReapStaleProcessFilesLeavesHeldPIDFile(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "reflect-held.pid")
	writeRetentionFile(t, pidPath, "999999999")
	oldProbe := openFileProbe
	openFileProbe = func(path string) (bool, error) {
		return path == pidPath, nil
	}
	t.Cleanup(func() { openFileProbe = oldProbe })

	if _, err := ReapStaleProcessFiles(dir); err != nil {
		t.Fatalf("ReapStaleProcessFiles: %v", err)
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Fatalf("held PID file was removed: %v", err)
	}
}

func TestReapStaleProcessFilesRemovesOnlyDeadGhostClaims(t *testing.T) {
	dir := t.TempDir()
	dead := filepath.Join(dir, "reflect-dead.pid")
	live := filepath.Join(dir, "resolve-live.pid")
	orphanLock := filepath.Join(dir, "supersede-orphan.pid.lock")
	unrelated := filepath.Join(dir, "other.pid")
	writeRetentionFile(t, dead, "999999999")
	writeRetentionFile(t, live, strconv.Itoa(os.Getpid()))
	writeRetentionFile(t, orphanLock, "")
	writeRetentionFile(t, unrelated, strconv.Itoa(os.Getpid()))

	removed, err := ReapStaleProcessFiles(dir)
	if err != nil {
		t.Fatalf("ReapStaleProcessFiles: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want dead pid and orphan lock (2)", removed)
	}
	for _, path := range []string{dead, orphanLock} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("stale file %s survived: %v", path, err)
		}
	}
	for _, path := range []string{live, unrelated} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("live/unrelated file %s was removed: %v", path, err)
		}
	}
}
