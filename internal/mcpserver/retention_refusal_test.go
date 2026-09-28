package mcpserver

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestGhostMemorySearch_RefusesRetentionOverAsOf: the two arguments together are a
// question the change log cannot answer. `memory_history` records the state a
// memory held — its wording, its category, its importance, its pin — and never a
// tier, so the only tier a historical row can carry is the one that row holds
// NOW. Answering with that, silently, produces a confident "nothing found in the
// requested tier" for all three tiers equally, or worse, a filter that looks
// applied and was not. This is the same refusal `explain` gets, for the same
// reason, and it has to happen before the search rather than inside it.
func TestGhostMemorySearch_RefusesRetentionOverAsOf(t *testing.T) {
	store := testStore(t)
	srv := New(store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test")
	session := connectedClient(t, srv)

	// Both shapes of refusal are accepted here, because which one arrives is a
	// property of the SDK's error reporting rather than of the product: what is
	// checked is that the message says why.
	var text string
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "ghost_memory_search",
		Arguments: map[string]any{
			"project_id": "test-project",
			"query":      "tunnel",
			"as_of":      "2026-01-01T00:00:00Z",
			"retention":  "persistent",
		},
	})
	if err != nil {
		text = err.Error()
	} else {
		if !res.IsError {
			t.Fatalf("a tier filter over a historical read was answered: %s", toolText(t, res))
		}
		text = toolText(t, res)
	}
	if !strings.Contains(text, "retention") || !strings.Contains(text, "as_of") {
		t.Errorf("the refusal does not name the two arguments in conflict: %q", text)
	}
	if !strings.Contains(text, "memory_history") {
		t.Errorf("the refusal does not say WHY it cannot answer: %q", text)
	}
	// And it must not have produced an answer shaped like a result.
	if strings.Contains(text, "[ghost:outcome=") {
		t.Errorf("a refused request still rendered a verdict line: %q", text)
	}
}

// TestGhostMemoriesList_KeepsItsOwnScopeWhenNothingIsFiltered: the browse tool
// read two different scopes before this change — `GetAll` (the project alone) with
// no category and `GetByCategory` (the project AND `_global`) with one — and
// unifying them behind the wider reading changed the default answer: a project
// with no memories of its own came back holding the per-install builtin seeds, and
// the "Project is registered but has no memories yet" branch became unreachable.
// The unfiltered browse answers about the project, which is what the tool says it
// does; the `_global` rows a filtered browse has always included are still
// included, because that is what its filter has always meant.
func TestGhostMemoriesList_KeepsItsOwnScopeWhenNothingIsFiltered(t *testing.T) {
	store := testStore(t)
	if err := store.EnsureProject(context.Background(), "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject(_global): %v", err)
	}
	srv := New(store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test")
	session := connectedClient(t, srv)
	ctx := context.Background()

	const shared = "every repository in this workspace uses 2-space YAML indentation"
	if _, _, _, err := store.Upsert(ctx, "_global", "convention", shared, "mcp", 0.7, nil); err != nil {
		t.Fatalf("save a global memory: %v", err)
	}
	if _, _, _, err := store.Upsert(ctx, "abc123", "fact", "the relay listens on port 2222 in staging", "mcp", 0.7, nil); err != nil {
		t.Fatalf("save a project memory: %v", err)
	}

	unfiltered, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_memories_list",
		Arguments: map[string]any{"project_id": "test-project"},
	})
	if err != nil {
		t.Fatalf("ghost_memories_list: %v", err)
	}
	text := toolText(t, unfiltered)
	if !strings.Contains(text, "relay listens on port 2222") {
		t.Errorf("the unfiltered list dropped the project's own memory: %q", text)
	}
	if strings.Contains(text, shared) {
		t.Errorf("the unfiltered list returned a _global memory it did not ask for: %q", text)
	}

	// The project with nothing of its own says so, rather than answering with
	// another project's rows — which is the message this branch exists for.
	if _, _, _, err := store.Upsert(ctx, "abc123", "fact", "another relay listens on port 3333 in staging", "mcp", 0.7, nil); err != nil {
		t.Fatalf("second project memory: %v", err)
	}
	if _, err := store.DeleteProject(ctx, "abc123", true); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if err := store.EnsureProject(ctx, "abc123", "/tmp/test", "test-project"); err != nil {
		t.Fatalf("re-register the project: %v", err)
	}
	empty, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_memories_list",
		Arguments: map[string]any{"project_id": "test-project"},
	})
	if err != nil {
		t.Fatalf("ghost_memories_list: %v", err)
	}
	if text := toolText(t, empty); !strings.Contains(text, "no memories yet") {
		t.Errorf("a project with no memories of its own answered with rows: %q", text)
	}
}
