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

const (
	importNotifyID   = "impnotifyid01"
	importNotifyName = "impnotify"
)

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
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "ghost_project_context", Arguments: map[string]any{"project_id": importNotifyName}}); err != nil {
		t.Fatalf("CallTool ghost_project_context: %v", err)
	}
}

func expectNoNotification(t *testing.T, updated <-chan string) {
	t.Helper()
	select {
	case got := <-updated:
		t.Fatalf("expected no resources/updated notification, got %q", got)
	case <-time.After(300 * time.Millisecond):
	}
}

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
			delete(want, got)
		case <-deadline:
			t.Fatalf("missing notifications: %v", want)
		}
	}
}

func TestProjectContextImportNoNotifyCases(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *memory.Store, string)
		want  int
	}{
		{"no files", func(*testing.T, *memory.Store, string) {}, 0},
		{"refused file", func(t *testing.T, _ *memory.Store, memDir string) {
			plantClaudeMemory(t, memDir, "refused-note.md", "Imported from a hostile file.\n\nThe service account value is ghp_0123456789abcdefghijklmnopqrstuvwxyzAB for CI")
		}, 0},
		{"past first contact", func(t *testing.T, store *memory.Store, memDir string) {
			if _, _, _, err := store.Upsert(context.Background(), importNotifyID, "fact", "an existing memory", "mcp", 0.5, nil); err != nil {
				t.Fatal(err)
			}
			plantClaudeMemory(t, memDir, "clean-note.md", "A plain project note with nothing sensitive in it.")
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, store, memDir := importNotifyServer(t)
			tc.setup(t, store, memDir)
			session, updated := subscribeTestClient(t, srv, "ghost://project/"+importNotifyName+"/context")
			callProjectContext(t, session)
			if got, _ := store.CountMemories(context.Background(), importNotifyID); got != tc.want {
				t.Fatalf("count = %d, want %d", got, tc.want)
			}
			expectNoNotification(t, updated)
		})
	}
}

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
		if a == nil || a.ReadOnlyHint {
			t.Fatal("ghost_project_context must not be read-only")
		}
		if a.DestructiveHint == nil || *a.DestructiveHint || a.IdempotentHint {
			t.Fatal("ghost_project_context annotations must declare non-destructive and non-idempotent")
		}
		if a.OpenWorldHint == nil || *a.OpenWorldHint {
			t.Fatal("ghost_project_context must declare openWorldHint=false")
		}
		return
	}
	t.Fatal("ghost_project_context is not registered as a tool")
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
