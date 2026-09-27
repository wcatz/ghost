package mcpserver

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestGhostMemorySave_PinOptsOutOfConsolidation: nothing an agent writes is
// excluded from `ghost reflect` by its source — seeds are 'builtin', agent
// saves 'mcp' — so protecting a memory used to take a SECOND call
// (ghost_memory_pin) and every window in between left it consolidatable. The
// save has to be able to carry the opt-out itself (#549).
func TestGhostMemorySave_PinOptsOutOfConsolidation(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)
	ctx := context.Background()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "ghost_memory_save",
		Arguments: map[string]any{
			"project_id": "test-project",
			"content":    "the stop hook spawns the lifecycle chain detached, so its stderr is a log file nobody opens",
			"category":   "architecture",
			"importance": 0.9,
			"pin":        true,
		},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_memory_save: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got error result: %+v", result.Content)
	}

	all, err := store.GetAll(ctx, "abc123", 10)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(all))
	}
	if !all[0].Pinned {
		t.Error("save(pin: true) stored a memory ghost reflect may rewrite")
	}

	text := toolText(t, result)
	if !strings.Contains(text, "pinned") {
		t.Errorf("result does not report the pin, so the caller cannot tell it took effect: %q", text)
	}
}

// TestGhostMemorySave_PinOnAFoldSaysWhichRowIsProtected: a near-duplicate save
// folds, so the id the result reports is a NEW row while the row consolidation
// would absorb is the existing one. A caller reading only the id would conclude
// the wrong memory is protected.
func TestGhostMemorySave_PinOnAFoldSaysWhichRowIsProtected(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)
	ctx := context.Background()

	const first = "the harness owns its own authentication, so Ghost adds no API key"
	existing, _, _, err := store.Upsert(ctx, "abc123", "fact", first, "mcp", 0.6, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "ghost_memory_save",
		Arguments: map[string]any{
			"project_id": "test-project",
			"content":    "the CLI harness owns its own authentication, so Ghost adds no API key",
			"category":   "fact",
			"pin":        true,
		},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_memory_save: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got error result: %+v", result.Content)
	}
	text := toolText(t, result)
	foldAt := strings.Index(text, "likely duplicate")
	if foldAt < 0 {
		t.Skipf("wording did not fold, so the fold branch was not reached: %q", text)
	}
	// The existing row is already named once, by the "likely duplicate of X"
	// clause — so asserting only that its id appears somewhere would pass even
	// with the pin clause removed. What has to be true is that the PIN note
	// names it too, because that is the sentence telling the caller which row
	// is actually protected.
	pinAt := strings.Index(text, "pinned")
	if pinAt < 0 {
		t.Fatalf("result does not report the pin at all: %q", text)
	}
	if !strings.Contains(text[pinAt:], existing) {
		t.Errorf("the pin clause does not name the folded-in row %s it also pinned: %q", existing, text)
	}

	all, err := store.GetAll(ctx, "abc123", 10)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	for _, m := range all {
		if !m.Pinned {
			t.Errorf("row %s stayed consolidatable on a pinned fold", m.ID)
		}
	}
}

// TestGhostMemorySave_WithoutPinStaysConsolidatable: the opposite side. Pinning
// is opt-in, and a save that does not ask for it must not leave the row exempt
// — that would make every agent save a pinned memory.
func TestGhostMemorySave_WithoutPinStaysConsolidatable(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)
	ctx := context.Background()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "ghost_memory_save",
		Arguments: map[string]any{
			"project_id": "test-project",
			"content":    "the reflect prompt is written to the harness child's stdin, never to argv",
			"category":   "gotcha",
		},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_memory_save: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got error result: %+v", result.Content)
	}

	all, err := store.GetAll(ctx, "abc123", 10)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(all))
	}
	if all[0].Pinned {
		t.Error("a save with no pin argument pinned the memory")
	}
	if text := toolText(t, result); strings.Contains(text, "pinned") {
		t.Errorf("result reports a pin that was not asked for: %q", text)
	}
}

// TestGhostMemorySave_RejectsNonBooleanPin: a client that sends the string
// "true" must be told what is wrong rather than have the field dropped and the
// memory stored unpinned.
func TestGhostMemorySave_RejectsNonBooleanPin(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)
	ctx := context.Background()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "ghost_memory_save",
		Arguments: map[string]any{
			"project_id": "test-project",
			"content":    "a memory whose pin argument is a string",
			"pin":        "true",
		},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_memory_save: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected a rejection for a non-boolean pin, got: %+v", result.Content)
	}
	all, err := store.GetAll(ctx, "abc123", 10)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("a rejected save stored %d memories", len(all))
	}
}

// toolText returns the concatenated text content of a tool result.
func toolText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	var sb strings.Builder
	for _, c := range result.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}
