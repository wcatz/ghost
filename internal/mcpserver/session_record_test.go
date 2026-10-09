package mcpserver

// The retrieval record names the session the call was made in.
//
// The audit judges only the calls a session made, so a record with an empty session id
// is never judged. Over stdio the transport reports no session, so the id comes from the
// host: Claude Code puts the id of the session in the server's environment, the same id
// it sends in every hook payload and names its session record after, and the stop hook's
// scan carries the payload's. Both halves must agree for a call to be judged at all.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func lastRecordSession(t *testing.T, srv *Server) (session string, n int) {
	t.Helper()
	recs, err := srv.store.(*memory.Store).RetrievalRecords(context.Background(), 100)
	if err != nil {
		t.Fatalf("RetrievalRecords: %v", err)
	}
	if len(recs) == 0 {
		return "", 0
	}
	return recs[0].SessionID, len(recs)
}

// TestSearchRecordsTheHostsSessionIDOverStdio: stdio assigns no transport id, so the
// session the host named in the server's environment is recorded WHEN the server is the
// host session (parent lacks CLAUDE_CODE_SESSION_ID).
func TestSearchRecordsTheHostsSessionIDOverStdio(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "host-session-1")
	root := t.TempDir()
	// Fake proc: ghost(100) <- parent(200) WITHOUT CLAUDE_CODE_SESSION_ID
	writeFakeProc(t, root, 100, 200, []string{"PATH=/usr/bin", "HOME=/home/user"})

	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	useFakeProc(t, root, 100)
	srv := New(store, logger, "test")

	session := connectedClient(t, srv)
	saveMem(t, session, "recorded corpus entry about the nightly export", nil)
	listingIDs(t, session, "nightly export", nil)
	if got, n := lastRecordSession(t, srv); n != 1 || got != "host-session-1" {
		t.Fatalf("search recorded %d row(s) with session %q, want 1 row naming host-session-1", n, got)
	}
}

// TestAHostThatNamesNoSessionLeavesTheRecordUnscoped: nothing is invented. Hosts whose
// server environment carries no session id (and a bridge that has none at all) record
// "", and such a call is left unjudged by the audit.
func TestAHostThatNamesNoSessionLeavesTheRecordUnscoped(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	srv, session := newCapSession(t)
	saveMem(t, session, "recorded corpus entry about the nightly export", nil)
	listingIDs(t, session, "nightly export", nil)
	if got, n := lastRecordSession(t, srv); n != 1 || got != "" {
		t.Fatalf("recorded %d row(s) with session %q, want 1 row with none", n, got)
	}
}

// TestProjectContextRecordsTheHostsSessionID: all four project-context surfaces reach
// the assembler through assembleProjectContext, so the process-level id is set there and
// covers the tool, both resources and the prompt alike.
func TestProjectContextRecordsTheHostsSessionID(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "host-session-2")
	root := t.TempDir()
	writeFakeProc(t, root, 100, 200, []string{"PATH=/usr/bin"})
	useFakeProc(t, root, 100)
	st, _ := projectRecordStore(t)
	srv, session := validityServerFor(t, st)
	saveValidityRow(t, session, projectContextSentinel, nil)
	_ = callTool(t, session, "ghost_project_context", map[string]any{"project_id": "vproj"})
	if got, n := lastRecordSession(t, srv); n != 1 || got != "host-session-2" {
		t.Fatalf("project context recorded %d row(s) with session %q, want 1 row naming host-session-2", n, got)
	}
}

// useFakeProc points the host-session decision New makes at a fake process tree
// for the rest of the test.
func useFakeProc(t *testing.T, root string, pid int) {
	t.Helper()
	old := hostSessionResolver
	hostSessionResolver = func() string { return resolveHostSessionID("linux", root, pid) }
	t.Cleanup(func() { hostSessionResolver = old })
}

// writeFakeProc creates a fake /proc tree for testing IsHostSession.
// root is the temp dir, pid is the current process (ghost), ppid is the parent.
func writeFakeProc(t *testing.T, root string, pid, ppid int, parentEnv []string) {
	t.Helper()
	// Write current process (ghost)
	ghostDir := filepath.Join(root, fmt.Sprintf("%d", pid))
	if err := os.MkdirAll(ghostDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", ghostDir, err)
	}
	stat := fmt.Sprintf("%d (ghost) S %d\n", pid, ppid)
	if err := os.WriteFile(filepath.Join(ghostDir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatalf("write ghost stat: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ghostDir, "comm"), []byte("ghost\n"), 0o644); err != nil {
		t.Fatalf("write ghost comm: %v", err)
	}

	// Write parent process
	parentDir := filepath.Join(root, fmt.Sprintf("%d", ppid))
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", parentDir, err)
	}
	stat = fmt.Sprintf("%d (parent) S 1\n", ppid)
	if err := os.WriteFile(filepath.Join(parentDir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatalf("write parent stat: %v", err)
	}
	if err := os.WriteFile(filepath.Join(parentDir, "comm"), []byte("parent\n"), 0o644); err != nil {
		t.Fatalf("write parent comm: %v", err)
	}
	// Write parent environ
	envData := []byte(strings.Join(parentEnv, "\x00") + "\x00")
	if err := os.WriteFile(filepath.Join(parentDir, "environ"), envData, 0o644); err != nil {
		t.Fatalf("write parent environ: %v", err)
	}

	// Write init (pid 1)
	initDir := filepath.Join(root, "1")
	if err := os.MkdirAll(initDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", initDir, err)
	}
	stat = "1 (init) S 0\n"
	if err := os.WriteFile(filepath.Join(initDir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatalf("write init stat: %v", err)
	}
	if err := os.WriteFile(filepath.Join(initDir, "comm"), []byte("init\n"), 0o644); err != nil {
		t.Fatalf("write init comm: %v", err)
	}
	envData = []byte("PATH=/usr/bin\x00")
	if err := os.WriteFile(filepath.Join(initDir, "environ"), envData, 0o644); err != nil {
		t.Fatalf("write init environ: %v", err)
	}
}

// TestSearchRecordsNoSessionWhenParentHasSessionID: a child process that inherits
// CLAUDE_CODE_SESSION_ID from its parent records no session id. The audit leaves
// such calls unjudged.
func TestSearchRecordsNoSessionWhenParentHasSessionID(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "host-session-1")
	root := t.TempDir()
	// Fake proc: ghost(100) <- parent(200) with CLAUDE_CODE_SESSION_ID
	writeFakeProc(t, root, 100, 200, []string{"PATH=/usr/bin", "CLAUDE_CODE_SESSION_ID=host-session-1"})

	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	useFakeProc(t, root, 100)
	srv := New(store, logger, "test")

	session := connectedClient(t, srv)
	saveMem(t, session, "recorded corpus entry about the nightly export", nil)
	listingIDs(t, session, "nightly export", nil)

	if got, n := lastRecordSession(t, srv); n != 1 || got != "" {
		t.Fatalf("child process recorded %d row(s) with session %q, want 1 row with empty session", n, got)
	}
}

// TestSearchRecordsSessionWhenParentLacksSessionID: the host session (parent
// lacks CLAUDE_CODE_SESSION_ID) records its session id.
func TestSearchRecordsSessionWhenParentLacksSessionID(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "host-session-1")
	root := t.TempDir()
	// Fake proc: ghost(100) <- parent(200) WITHOUT CLAUDE_CODE_SESSION_ID
	writeFakeProc(t, root, 100, 200, []string{"PATH=/usr/bin", "HOME=/home/user"})

	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	useFakeProc(t, root, 100)
	srv := New(store, logger, "test")

	session := connectedClient(t, srv)
	saveMem(t, session, "recorded corpus entry about the nightly export", nil)
	listingIDs(t, session, "nightly export", nil)

	if got, n := lastRecordSession(t, srv); n != 1 || got != "host-session-1" {
		t.Fatalf("host session recorded %d row(s) with session %q, want 1 row naming host-session-1", n, got)
	}
}

// TestProjectContextRecordsNoSessionForAnInheritedChild: the project-context path
// must apply the same host-or-child decision as search; a child whose parent's
// environ carries the variable records "".
func TestProjectContextRecordsNoSessionForAnInheritedChild(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "host-session-3")
	root := t.TempDir()
	writeFakeProc(t, root, 100, 200, []string{"PATH=/usr/bin", "CLAUDE_CODE_SESSION_ID=host-session-3"})
	useFakeProc(t, root, 100)
	st, _ := projectRecordStore(t)
	srv, session := validityServerFor(t, st)
	saveValidityRow(t, session, projectContextSentinel, nil)
	_ = callTool(t, session, "ghost_project_context", map[string]any{"project_id": "vproj"})
	if got, n := lastRecordSession(t, srv); n != 1 || got != "" {
		t.Fatalf("child project context recorded %d row(s) with session %q, want 1 row with none", n, got)
	}
}

// TestResolveHostSessionIDNonLinuxKeepsTheEnvironmentsID: without /proc the
// environment's id is used as before.
func TestResolveHostSessionIDNonLinuxKeepsTheEnvironmentsID(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "host-session-4")
	if got := resolveHostSessionID("darwin", t.TempDir(), 100); got != "host-session-4" {
		t.Fatalf("non-linux resolve = %q, want the environment's id", got)
	}
	if got := resolveHostSessionID("linux", t.TempDir(), 100); got != "" {
		t.Fatalf("linux with unreadable proc = %q, want none (fail safe)", got)
	}
}
