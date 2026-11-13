package mcpserver

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
)

// importedOversize is one field's worth of text an artifact carried and the
// import must have cut before it reached the store: 100 KB, far past the 8000
// content cap and the 300 title cap. The tokens are unique per index so one
// field cannot be mistaken for another in a failure message.
func importedOversize(i int, noun string) string {
	return fmt.Sprintf("%s %02d: %s", noun, i, strings.Repeat(fmt.Sprintf("%s%02d.", noun[:2], i), 12000))
}

// importCapSession wires a fully-registered server to an in-memory client
// session over a store the test also holds, because these cases drive the
// IMPORT path — the write a portable artifact performs — and the server's own
// field is the narrower provider.MemoryStore interface, which has no importer.
func importCapSession(t *testing.T) (*Server, *memory.Store, *mcp.ClientSession) {
	t.Helper()
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	return srv, store, connectedClient(t, srv)
}

// TestAnImportedOversizeTaskListsWithinTheResponseBudget is the end of the
// defect #1226 describes, measured at the surface rather than at the row:
// `ghost_task_list` fits its answer to the shared 16000-byte budget, and the
// budget's own rule keeps the first row whole rather than dropping it — which
// is sound ONLY because a row is bounded by construction. An imported task
// with 100 KB of description and 100 KB of title is not, so the one row came
// back at about 200 KB with no way to make it smaller. The import applies the
// caps `ghost_task_create` applies, so the same answer now fits.
func TestAnImportedOversizeTaskListsWithinTheResponseBudget(t *testing.T) {
	_, store, session := importCapSession(t)
	ctx := context.Background()
	if _, _, err := store.ImportTask(ctx, memory.Task{
		ID: "imp-task-0000000000000000", ProjectID: "abc123",
		Title:       importedOversize(1, "title"),
		Description: importedOversize(2, "description"),
		Notes:       importedOversize(3, "notes"),
		Status:      "pending", Priority: 2,
	}, true); err != nil {
		t.Fatalf("ImportTask: %v", err)
	}

	out := resultText(callTool(t, session, "ghost_task_list", map[string]any{"project_id": "test-project"}))

	if strings.Contains(out, truncationMarkerLiteral) {
		// The marker lives inside the stored text, so seeing it here is
		// correct — it is the proof the cut happened and was explicit.
		t.Logf("answer carries the stored truncation marker at %d bytes", len(out))
	}
	if len(out) > searchResponseMaxBytes {
		t.Errorf("ghost_task_list answer = %d bytes against a %d-byte budget; a single task row rendered about 200 KB "+
			"because the importer stored the artifact's text verbatim", len(out), searchResponseMaxBytes)
	}
	if !strings.Contains(out, truncationMarkerLiteral) {
		t.Errorf("the imported task's stored description must show the explicit truncation marker; answer tail %q",
			tail(out, 120))
	}
	if !strings.Contains(out, "- [pending] P2") {
		t.Errorf("the imported task must still be listed; answer %q", tail(out, 400))
	}
}
