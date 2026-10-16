package mcpserver

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/claudeimport"
	"github.com/wcatz/ghost/internal/memory"
)

// The first-contact import behind ghost_project_context writes memory rows, so
// it is a writer under the rule that every writer of a memory row notifies
// every resource the write changes (notifyMemoryRow). These tests subscribe a
// real MCP client before the call, since a notification is emitted once and
// cannot be retried.

const (
	importNotifyID   = "impnotifyid01"
	importNotifyName = "impnotify"
)

// importNotifyServer builds a server over a store holding one registered,
// path-shaped project, and points HOME at a fresh temp dir so the Claude memory
// directory the import reads is the sandbox's. It returns the server, the store
// and that project's memory directory, which the caller fills (or leaves
// absent).
func importNotifyServer(t *testing.T) (*Server, *memory.Store, string) {
	t.Helper()
	ctx := context.Background()
	projPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projPath, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store := memory.NewStore(db, logger)
	if err := store.EnsureProject(ctx, importNotifyID, projPath, importNotifyName); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	memDir := filepath.Join(home, ".claude", "projects", claudeimport.EncodeProjectPath(projPath), "memory")
	return New(store, logger, "test"), store, memDir
}

func plantClaudeMemory(t *testing.T, memDir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memDir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func callProjectContext(t *testing.T, session *mcp.ClientSession) {
	t.Helper()
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ghost_project_context",
		Arguments: map[string]any{"project_id": importNotifyName},
	}); err != nil {
		t.Fatalf("CallTool ghost_project_context: %v", err)
	}
}

// expectNoNotification asserts nothing arrives. The tool's notification, if
// any, is sent before the call returns, so on an in-memory transport it is on
// the wire by the time this runs; the window is the one the dry-run delete
// test uses.
func expectNoNotification(t *testing.T, updated <-chan string) {
	t.Helper()
	select {
	case got := <-updated:
		t.Fatalf("expected no resources/updated notification, got %q", got)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestProjectContextImportNotifiesContextSubscriber: a ghost_project_context
// call that imports Claude auto-memory rows into an empty project changed what
// the project's context resource serves, so a client subscribed to it — under
// either the id or the name form of the URI — must receive resources/updated,
// exactly as it does after a save.
func TestProjectContextImportNotifiesContextSubscriber(t *testing.T) {
	srv, store, memDir := importNotifyServer(t)
	plantClaudeMemory(t, memDir, "clean-note.md", "A plain project note with nothing sensitive in it.")

	idURI := "ghost://project/" + importNotifyID + "/context"
	nameURI := "ghost://project/" + importNotifyName + "/context"
	session, updated := subscribeTestClient(t, srv, idURI, nameURI)

	callProjectContext(t, session)

	if got, err := store.CountMemories(context.Background(), importNotifyID); err != nil || got != 1 {
		t.Fatalf("after the import: count = %d (err %v), want 1 imported row", got, err)
	}
	want := map[string]bool{idURI: true, nameURI: true}
	deadline := time.After(notificationWait)
	for len(want) > 0 {
		select {
		case got := <-updated:
			if !want[got] {
				t.Fatalf("unexpected notification %q; still waiting for %v", got, want)
			}
			delete(want, got)
		case <-deadline:
			t.Fatalf("no resources/updated notification within %s for %v", notificationWait, want)
		}
	}
}

// TestProjectContextWithoutImportDoesNotNotify: an empty project with no
// Claude memory directory imports nothing, so the call is a pure read and
// pushes no notification.
func TestProjectContextWithoutImportDoesNotNotify(t *testing.T) {
	srv, store, _ := importNotifyServer(t)
	session, updated := subscribeTestClient(t, srv, "ghost://project/"+importNotifyName+"/context")

	callProjectContext(t, session)

	if got, _ := store.CountMemories(context.Background(), importNotifyID); got != 0 {
		t.Fatalf("count = %d, want 0 (nothing to import)", got)
	}
	expectNoNotification(t, updated)
}

// TestProjectContextRefusedImportDoesNotNotify: the import ran, but its only
// file was refused by the token guard, so no row was written and no resource
// changed. This is the case that exercises the imported>0 gate itself, since
// the import is reached and returns without an error.
func TestProjectContextRefusedImportDoesNotNotify(t *testing.T) {
	srv, store, memDir := importNotifyServer(t)
	plantClaudeMemory(t, memDir, "refused-note.md",
		"Imported from a hostile file.\n\nThe service account value is ghp_0123456789abcdefghijklmnopqrstuvwxyzAB for CI")
	session, updated := subscribeTestClient(t, srv, "ghost://project/"+importNotifyName+"/context")

	callProjectContext(t, session)

	if got, _ := store.CountMemories(context.Background(), importNotifyID); got != 0 {
		t.Fatalf("count = %d, want 0 (the only file was refused)", got)
	}
	expectNoNotification(t, updated)
}

// TestProjectContextOnProjectWithMemoriesImportsNothingAndDoesNotNotify: a
// project that already holds a memory is past first contact, so a planted file
// is not imported and the call pushes nothing.
func TestProjectContextOnProjectWithMemoriesImportsNothingAndDoesNotNotify(t *testing.T) {
	srv, store, memDir := importNotifyServer(t)
	if _, _, _, err := store.Upsert(context.Background(), importNotifyID, "fact", "an existing memory", "mcp", 0.5, nil); err != nil {
		t.Fatal(err)
	}
	plantClaudeMemory(t, memDir, "clean-note.md", "A plain project note with nothing sensitive in it.")
	session, updated := subscribeTestClient(t, srv, "ghost://project/"+importNotifyName+"/context")

	callProjectContext(t, session)

	if got, _ := store.CountMemories(context.Background(), importNotifyID); got != 1 {
		t.Fatalf("count = %d, want 1 (no import past first contact)", got)
	}
	expectNoNotification(t, updated)
}

// TestProjectContextAnnotationsDoNotClaimReadOnly pins the hints a client acts
// on. The tool writes memory rows on first contact, so it is not read-only; it
// deletes and overwrites nothing, so it is not destructive, and that has to be
// said explicitly because the spec defaults destructiveHint to true once
// readOnlyHint is false; and it is not idempotent, because the first call can
// import and a second call does not.
func TestProjectContextAnnotationsDoNotClaimReadOnly(t *testing.T) {
	_, session := newCapSession(t)
	tools, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range tools.Tools {
		if tool.Name != "ghost_project_context" {
			continue
		}
		a := tool.Annotations
		if a == nil {
			t.Fatal("ghost_project_context has no annotations")
		}
		if a.ReadOnlyHint {
			t.Error("ghost_project_context is marked read-only; its first-contact import writes memory rows")
		}
		if a.DestructiveHint == nil || *a.DestructiveHint {
			t.Error("ghost_project_context must declare destructiveHint=false explicitly; it deletes and overwrites nothing")
		}
		if a.IdempotentHint {
			t.Error("ghost_project_context is marked idempotent; a first call can import and a second does not")
		}
		if a.OpenWorldHint == nil || *a.OpenWorldHint {
			t.Error("ghost_project_context must declare openWorldHint=false")
		}
		return
	}
	t.Fatal("ghost_project_context is not registered as a tool")
}
