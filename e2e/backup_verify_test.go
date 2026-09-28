//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestCLIBackupVerifyRestore walks the whole path a backup exists for: take one
// of a live store, check it says what it claims, find out that a damaged copy is
// refused rather than trusted, and restore a good one and read the memories back
// through the MCP server. Each step is a separate command a user runs, and the
// interesting failure is the one where any of them is trusted when it should not
// be — so the corruption is introduced deliberately, between the two verifies.
func TestCLIBackupVerifyRestore(t *testing.T) {
	s := newSandbox(t)
	cs := s.mcpSession(t)
	const memories = 3
	for i := 0; i < memories; i++ {
		call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    fmt.Sprintf("a memory that has to survive a restore, number %d", i),
		})
	}

	dest := filepath.Join(s.t.TempDir(), "snapshot.db")
	backup := s.mustRun("backup", "--out", dest)
	// The report names the manifest beside the snapshot. It is not an extra
	// file the user has to know about: the one command that took the backup
	// says where it put the thing that makes the backup checkable.
	mustContain(t, "backup report", backup.stdout, manifestPathFor(dest))
	mustContain(t, "backup report", backup.stdout, "memories:")

	// A copy that has not been touched verifies clean, and names every check
	// rather than a bare "ok" — a reader has to be able to tell a check that
	// passed from a check that could not run.
	verify := s.mustRun("backup", "verify", dest)
	for _, want := range []string{"sha256", "integrity check", "schema version", "row counts", "ok"} {
		mustContain(t, "verify report", verify.stdout, want)
	}
	// The headline says which it is, in the first line, so a reader who stops
	// after one line has not been left to infer it from a column further down.
	mustContain(t, "verify report", verify.stdout, "verified "+dest)
	// And the counts it reports are the ones the file actually holds, read back
	// independently of the command.
	// Scoped to the project: the store also holds Ghost's own builtin global
	// seed, and counting that would make the check about the fixture rather than
	// about whether the copy is the one the manifest describes.
	if got, want := countInFileWhere(t, dest, "SELECT COUNT(*) FROM memories WHERE project_id = ?", e2eProject), memories; got != want {
		t.Fatalf("the backup holds %d project memories, want %d", got, want)
	}

	// One flipped bit in the database header's file change counter (byte 24, see
	// https://sqlite.org/fileformat.html). The file still opens, still passes
	// SQLite's integrity_check and still holds every row, so nothing but the
	// manifest's hash can see it: a verify that passed this would be a verify a
	// restore could not rely on.
	raw := mustReadFile(t, dest)
	if len(raw) <= 24 {
		t.Fatalf("the snapshot is %d bytes, too short to be a SQLite database", len(raw))
	}
	raw[24] ^= 0x01
	if err := os.WriteFile(dest, raw, 0o600); err != nil {
		t.Fatalf("write flipped snapshot: %v", err)
	}
	damaged := s.mustFail("backup", "verify", dest)
	mustMatch(t, "verify of a flipped byte", damaged.stdout+damaged.stderr, "(?i)sha256")
	mustMatch(t, "verify of a flipped byte", damaged.stdout+damaged.stderr, "(?i)fail")
	// Refusing, not merely reporting: the two are different answers and the
	// headline is what tells them apart at a glance.
	mustContain(t, "verify of a flipped byte", damaged.stdout, "refusing "+dest)

	// A path that is not there is reported as such rather than as a healthy
	// backup, because the difference decides whether the reader goes looking for
	// the file.
	missing := s.mustFail("backup", "verify", filepath.Join(s.t.TempDir(), "no-such.db"))
	mustContain(t, "verify of a missing file", missing.stderr, "no-such.db")

	// A copy cut short is what a full disk or an interrupted transfer leaves
	// behind, and it is the case the size check exists for: the file cannot be
	// opened for a schema at all, so nothing but the manifest's own record of
	// its size can say what happened. Restaged from a good backup so the
	// manifest is one a reader would really have.
	// Copied rather than moved: the restore below needs the good snapshot, and a
	// test that damaged the only copy it had would be testing nothing after this.
	short := filepath.Join(s.t.TempDir(), "short.db")
	if err := os.WriteFile(short, mustReadFile(t, dest)[:4096], 0o600); err != nil {
		t.Fatalf("write the truncated copy: %v", err)
	}
	if err := os.WriteFile(manifestPathFor(short), mustReadFile(t, manifestPathFor(dest)), 0o600); err != nil {
		t.Fatalf("copy the manifest beside it: %v", err)
	}
	truncated := s.mustFail("backup", "verify", short)
	mustMatch(t, "verify of a truncated copy", truncated.stdout, "(?i)sha256")
	// The size is named on both sides, because a digest to diff by hand is a
	// worse answer to "is this the whole file" than two numbers.
	mustMatch(t, "verify of a truncated copy", truncated.stdout, `(?i)bytes`)
	mustMatch(t, "verify of a truncated copy", truncated.stdout, `4096`)
	// And nothing claims a row count for a file nobody could read.
	mustNotContain(t, "verify of a truncated copy", truncated.stdout, "no rows")

	// Restore. The snapshot goes back in as ghost.db, which is the documented
	// procedure, and the MCP server that was holding the old file is closed
	// first: restoring under a live writer is the case VACUUM INTO exists to
	// avoid, and this test would not be honest about the copy if it left one.
	if err := cs.Close(); err != nil {
		t.Fatalf("close the mcp session: %v", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(s.dbPath() + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove %s: %v", s.dbPath()+suffix, err)
		}
	}
	if err := os.Rename(dest, s.dbPath()); err != nil {
		t.Fatalf("move the snapshot into the data directory: %v", err)
	}

	// The restored store answers through the server again, and holds what it
	// held before the backup. This is the point of the whole path: a copy that
	// verifies is still worthless if restoring it loses a memory.
	restored := s.mcpSession(t)
	rows := stringsInFile(t, s.dbPath(),
		"SELECT content FROM memories WHERE project_id = ?", e2eProject)
	if len(rows) != memories {
		t.Fatalf("the restored store holds %d project memories, want %d", len(rows), memories)
	}
	found := call(t, restored, "ghost_memory_search", map[string]any{
		"project_id": e2eProject,
		"query":      "restore",
	})
	mustContain(t, "search over the restored store", found, "number 0")
}

// manifestPathFor mirrors memory.ManifestPath. The e2e suite drives the built
// binary, so it cannot import internal/memory — and it should not: where the
// manifest lands is part of the CLI's contract with the user, so a test that
// asks the binary to find it by a name this file spells out is the test that
// notices when the two disagree. The one thing it cannot check is that they
// agree, which is what TestManifestPathSitsBesideTheSnapshot is for.
func manifestPathFor(snapshot string) string { return snapshot + ".manifest.json" }
