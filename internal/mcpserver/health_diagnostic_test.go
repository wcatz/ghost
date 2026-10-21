package mcpserver

// #1738: the diagnostic reads do not wait behind an in-flight write.
//
// A write holds the store's mutex for its whole transaction, and the store's
// primary handle is pinned to one connection, so a report that reads through
// the primary handle waits for the write to commit. A caller with a deadline
// used to get a cancellation instead of a report: ghost_health answers
// "list projects: context canceled" and nothing else.
//
// What is tested here is the tool boundary. The store-level behaviour — that
// the diagnostic read takes the read handle and no mutex — is
// internal/memory's, and this file is what says the TOOL answers.

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wcatz/ghost/internal/memory"
)

// diagnosticServer is a server over a FILE-backed store with the read handle
// the production server is built with (bootstrap opens a second, read-only
// handle for exactly this reason; an in-memory store cannot have one).
func diagnosticServer(t *testing.T) (*Server, *memory.Store, *sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ghost.db")
	db, err := memory.OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	readDB, err := memory.OpenReadDB(path)
	if err != nil {
		t.Fatalf("OpenReadDB: %v", err)
	}
	t.Cleanup(func() { _ = readDB.Close() })
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store := memory.NewStoreWithRead(db, readDB, logger)
	if err := store.EnsureProject(context.Background(), "abc123", "/tmp/test", "test-project"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, err := store.Create(context.Background(), "abc123", memory.Memory{
		Category: "fact", Content: "the relay listens on port 2222 in production", Importance: 0.5, Source: "mcp",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	return New(store, logger, "test"), store, db, path
}

// holdTheStoreLocked starts a write that holds the store's mutex AND the
// store's one connection, and returns the function that releases it.
//
// The write is a project delete that cannot start: a second handle holds the
// database's write lock, so the delete's BEGIN IMMEDIATE waits for it — and it
// waits holding the store's mutex, which is the state a report used to queue
// behind. This is the repro from the issue: a project delete with apply, and
// a diagnostic call on the same session at the same time.
func holdTheStoreLocked(t *testing.T, store *memory.Store, db *sql.DB, path string) (release func()) {
	t.Helper()
	ctx := context.Background()
	blocker, err := memory.OpenDB(path)
	if err != nil {
		t.Fatalf("open the blocking handle: %v", err)
	}
	// BEGIN IMMEDIATE takes the database's write lock and holds it: the
	// delete's own BEGIN cannot get it.
	tx, err := blocker.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin the blocking transaction: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := store.DeleteProject(ctx, "abc123", true)
		done <- err
	}()
	// The delete holds the store's connection while it waits for the lock, and
	// it holds the store's mutex for the whole wait. The connection is the
	// observable half: it is checked out for the wait and released after it.
	held := time.Time{}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if db.Stats().InUse == 1 {
			if held.IsZero() {
				held = time.Now()
			} else if time.Since(held) > 50*time.Millisecond {
				break
			}
		} else {
			held = time.Time{}
		}
		select {
		case err := <-done:
			t.Fatalf("the delete finished while the blocking transaction held the write lock: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the delete never reached the write lock")
		}
		time.Sleep(2 * time.Millisecond)
	}
	return func() {
		_ = tx.Rollback()
		_ = blocker.Close()
		<-done
	}
}

// TestHealthAnswersWhileAWriteIsInFlight: the report is the one thing an
// operator or an agent reaches for WHILE something is slow, so it is the one
// read that must not wait behind the slowness.
func TestHealthAnswersWhileAWriteIsInFlight(t *testing.T) {
	srv, store, db, path := diagnosticServer(t)
	release := holdTheStoreLocked(t, store, db, path)
	defer release()

	session := connectedClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ghost_health"})
	if err != nil {
		t.Fatalf("ghost_health did not answer while a write was in flight: %v", err)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("ghost_health returned %T, want text", result.Content[0])
	}
	for _, want := range []string{
		"## Ghost Health",
		"**Projects:** 1",
		"**Total memories:** 1",
		"**Memory links:**",
		"**History:**",
		"**Retrieval audit**",
	} {
		if !strings.Contains(text.Text, want) {
			t.Errorf("ghost_health does not report %q while a write is in flight:\n%s", want, text.Text)
		}
	}
	// A section that could not be read is a section that waited for the write
	// and gave up: the report would name a store it never looked at.
	if strings.Contains(text.Text, "could not be read") {
		t.Errorf("ghost_health reports a section it could not read while a write was in flight:\n%s", text.Text)
	}
}

// TestListProjectsAnswersWhileAWriteIsInFlight: the project listing is how an
// agent discovers which project to work in, so it is the read that must not
// queue behind a write either.
func TestListProjectsAnswersWhileAWriteIsInFlight(t *testing.T) {
	srv, store, db, path := diagnosticServer(t)
	release := holdTheStoreLocked(t, store, db, path)
	defer release()

	session := connectedClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ghost_list_projects"})
	if err != nil {
		t.Fatalf("ghost_list_projects did not answer while a write was in flight: %v", err)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("ghost_list_projects returned %T, want text", result.Content[0])
	}
	if !strings.Contains(text.Text, "## Ghost Projects") {
		t.Errorf("ghost_list_projects did not answer with its listing:\n%s", text.Text)
	}
	if !strings.Contains(text.Text, "test-project") {
		t.Errorf("ghost_list_projects did not name the project while a write was in flight:\n%s", text.Text)
	}
	if strings.Contains(text.Text, "could not be read") {
		t.Errorf("ghost_list_projects reports a count it could not read while a write was in flight:\n%s", text.Text)
	}
}

// TestTheDiagnosticToolsWorkWithoutAReadHandle: a store with no read handle —
// the in-memory store, the bench store, a store built without
// NewStoreWithRead — has no second handle to read through, and the tools must
// still answer. The fallback is the store's own reads, which is what those
// stores have always used.
func TestTheDiagnosticToolsWorkWithoutAReadHandle(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	session := connectedClient(t, srv)
	for _, name := range []string{"ghost_health", "ghost_list_projects"} {
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name})
		if err != nil {
			t.Fatalf("CallTool %s without a read handle: %v", name, err)
		}
		text, ok := result.Content[0].(*mcp.TextContent)
		if !ok {
			t.Fatalf("%s returned %T, want text", name, result.Content[0])
		}
		if !strings.Contains(text.Text, "test-project") {
			t.Errorf("%s does not name the project without a read handle:\n%s", name, text.Text)
		}
	}
}
