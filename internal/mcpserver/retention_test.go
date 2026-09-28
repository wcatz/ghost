package mcpserver

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
)

// TestGhostMemorySave_RoundTripsTheRetentionTier: the save tool is the only place
// a caller states a tier, and a tier that does not read back out of the store is
// a tier nothing can filter on, decay by or exempt. The result message has to
// name it too, for the same reason the pin is reported: a fold returns a new id
// while the row that carries the protection is the existing one.
func TestGhostMemorySave_RoundTripsTheRetentionTier(t *testing.T) {
	store := testStore(t)
	srv := New(store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test")
	session := connectedClient(t, srv)
	ctx := context.Background()

	for _, tier := range memory.RetentionValues() {
		t.Run(tier, func(t *testing.T) {
			content := "a " + tier + " tier memory saved through the MCP surface"
			result, err := session.CallTool(ctx, &mcp.CallToolParams{
				Name:      "ghost_memory_save",
				Arguments: map[string]any{"project_id": "test-project", "content": content, "retention": tier},
			})
			if err != nil {
				t.Fatalf("CallTool ghost_memory_save: %v", err)
			}
			if result.IsError {
				t.Fatalf("error result: %+v", result.Content)
			}
			mems, err := store.GetByCategory(ctx, "abc123", "fact", 50)
			if err != nil {
				t.Fatalf("GetByCategory: %v", err)
			}
			var found *memory.Memory
			for i := range mems {
				if mems[i].Content == content {
					found = &mems[i]
				}
			}
			if found == nil {
				t.Fatalf("the saved row is not in the store: %+v", mems)
			}
			if found.Retention != tier {
				t.Errorf("stored retention = %q, want %q", found.Retention, tier)
			}
			if !strings.Contains(toolText(t, result), tier) {
				t.Errorf("the result does not report the tier, so a caller cannot tell it took effect: %q", toolText(t, result))
			}
		})
	}
}

// TestGhostMemorySave_RefusesAnUnknownTier: a caller's typo has to be refused in
// the caller's own words, naming the three values that would have worked, and it
// has to write nothing on the way to that refusal.
func TestGhostMemorySave_RefusesAnUnknownTier(t *testing.T) {
	store := testStore(t)
	srv := New(store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test")
	session := connectedClient(t, srv)
	ctx := context.Background()

	for _, bad := range []string{"forever", "SESSION", "keep", "global"} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name: "ghost_memory_save",
			Arguments: map[string]any{
				"project_id": "test-project",
				"content":    "a save whose tier is a typo: " + bad,
				"retention":  bad,
			},
		})
		if err != nil {
			// A handler error surfaces as a protocol error here rather than as an
			// error result, so both shapes are read for the message.
			text := err.Error()
			if !strings.Contains(text, "session") || !strings.Contains(text, "persistent") {
				t.Errorf("error for %q does not name the vocabulary: %v", bad, err)
			}
			continue
		}
		if !result.IsError {
			t.Fatalf("retention %q was accepted by the save tool", bad)
		}
		text := toolText(t, result)
		if !strings.Contains(text, "session") || !strings.Contains(text, "persistent") || !strings.Contains(text, bad) {
			t.Errorf("refusal for %q does not name the value and the vocabulary: %q", bad, text)
		}
	}

	mems, err := store.GetAll(ctx, "abc123", 50)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(mems) != 0 {
		t.Errorf("a refused tier wrote %d row(s)", len(mems))
	}
}

// TestGhostMemorySave_OmitsTheTierByDefault: the tool's schema marks the
// argument optional, and a caller that omits it must land on the tier the corpus
// has always had rather than on an error or on the shortest life.
func TestGhostMemorySave_OmitsTheTierByDefault(t *testing.T) {
	store := testStore(t)
	srv := New(store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test")
	session := connectedClient(t, srv)
	ctx := context.Background()

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_memory_save",
		Arguments: map[string]any{"project_id": "test-project", "content": "a save that names no tier"},
	}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	mems, err := store.GetAll(ctx, "abc123", 50)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(mems) != 1 {
		t.Fatalf("store holds %d rows, want 1", len(mems))
	}
	if mems[0].Retention != memory.RetentionProject {
		t.Errorf("retention = %q, want %q", mems[0].Retention, memory.RetentionProject)
	}
}

// TestGhostMemoriesList_FiltersByTier: browsing is where an operator asks "what
// did I mark keep-forever", and a filter that silently ignored the argument would
// answer with the whole corpus — the same rows, in the same order, with no error
// anywhere.
func TestGhostMemoriesList_FiltersByTier(t *testing.T) {
	store := testStore(t)
	srv := New(store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test")
	session := connectedClient(t, srv)
	ctx := context.Background()

	// Deliberately unlike each other. Two near-identical saves FOLD, and a fold
	// raises the surviving row's tier (the rule TestGhostMemorySave round-trips
	// pins), so a fixture of paraphrases would leave every row at the highest tier
	// the loop reached and this test would prove nothing.
	seed := map[string]string{
		"the lab tunnel has mtu 1400 while we debug fragmentation":   memory.RetentionSession,
		"production ingress requires mtu 1500 for jumbo frames":      memory.RetentionPersistent,
		"the deploy runbook says to drain the queue before rotating": memory.RetentionProject,
	}
	for content, tier := range seed {
		if _, _, _, err := store.UpsertWithOptions(ctx, "abc123", "fact", content, "mcp", 0.6, nil,
			memory.UpsertOptions{Retention: tier}); err != nil {
			t.Fatalf("UpsertWithOptions(%s): %v", tier, err)
		}
	}

	for tier, wantOnly := range map[string]string{
		memory.RetentionSession:    "the lab tunnel has mtu 1400 while we debug fragmentation",
		memory.RetentionPersistent: "production ingress requires mtu 1500 for jumbo frames",
	} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "ghost_memories_list",
			Arguments: map[string]any{"project_id": "test-project", "retention": tier},
		})
		if err != nil {
			t.Fatalf("ghost_memories_list(%s): %v", tier, err)
		}
		text := toolText(t, result)
		if !strings.Contains(text, wantOnly) {
			t.Errorf("list(retention=%s) does not contain the %s row: %q", tier, tier, text)
		}
		for content := range seed {
			if content == wantOnly {
				continue
			}
			if strings.Contains(text, content) {
				t.Errorf("list(retention=%s) returned a %s row: %q", tier, "different", text)
			}
		}
	}

	// The unfiltered call still returns everything, because a filter that changed
	// the default answer would be a filter nobody could turn off.
	all, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_memories_list",
		Arguments: map[string]any{"project_id": "test-project"},
	})
	if err != nil {
		t.Fatalf("ghost_memories_list: %v", err)
	}
	for content := range seed {
		if !strings.Contains(toolText(t, all), content) {
			t.Errorf("the unfiltered list dropped %q", content)
		}
	}
}

// TestGhostMemoriesList_RefusesAnUnknownTier: the same refusal as the save, from
// the same vocabulary. A browse tool that answered a mistyped filter with the
// whole corpus is worse than one that refuses.
func TestGhostMemoriesList_RefusesAnUnknownTier(t *testing.T) {
	store := testStore(t)
	srv := New(store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test")
	session := connectedClient(t, srv)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ghost_memories_list",
		Arguments: map[string]any{"project_id": "test-project", "retention": "keep-forever"},
	})
	if err != nil {
		if !strings.Contains(err.Error(), "persistent") {
			t.Errorf("error does not name the vocabulary: %v", err)
		}
		return
	}
	if !result.IsError {
		t.Fatalf("an unknown tier was accepted by the list tool: %q", toolText(t, result))
	}
	if text := toolText(t, result); !strings.Contains(text, "session") || !strings.Contains(text, "persistent") {
		t.Errorf("refusal does not name the vocabulary: %q", text)
	}
}

// TestGhostMemorySearch_FiltersByTier: search is windowed, so a tier filter has
// to be applied to the widened candidate set before the window closes — a filter
// applied after it can only remove, never add, and a session row the window cut
// would read as absent while the memory exists (#573's argument, for the tier).
func TestGhostMemorySearch_FiltersByTier(t *testing.T) {
	store := testStore(t)
	srv := New(store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test")
	session := connectedClient(t, srv)
	ctx := context.Background()

	for content, tier := range map[string]string{
		// Distinct wording for the same reason as the list fixture: a fold would
		// raise one row's tier to the other's and the filter would have nothing
		// left to separate.
		"the lab tunnel has mtu 1400 while we debug fragmentation": memory.RetentionSession,
		"production ingress requires mtu 1500 for jumbo frames":    memory.RetentionPersistent,
	} {
		if _, _, _, err := store.UpsertWithOptions(ctx, "abc123", "fact", content, "mcp", 0.6, nil,
			memory.UpsertOptions{Retention: tier}); err != nil {
			t.Fatalf("UpsertWithOptions(%s): %v", tier, err)
		}
	}

	for _, tc := range []struct{ tier, want, unwanted string }{
		{memory.RetentionSession, "mtu 1400", "mtu 1500"},
		{memory.RetentionPersistent, "mtu 1500", "mtu 1400"},
	} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name: "ghost_memory_search",
			Arguments: map[string]any{
				"project_id": "test-project",
				"query":      "tunnel mtu",
				"retention":  tc.tier,
			},
		})
		if err != nil {
			t.Fatalf("ghost_memory_search(%s): %v", tc.tier, err)
		}
		text := toolText(t, result)
		if !strings.Contains(text, tc.want) {
			t.Errorf("search(retention=%s) did not return the %s row: %q", tc.tier, tc.tier, text)
		}
		if strings.Contains(text, tc.unwanted) {
			t.Errorf("search(retention=%s) returned a row of another tier: %q", tc.tier, text)
		}
	}

	// Unfiltered, both come back — the filter is opt-in and changes nothing else.
	both, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_memory_search",
		Arguments: map[string]any{"project_id": "test-project", "query": "tunnel mtu"},
	})
	if err != nil {
		t.Fatalf("ghost_memory_search: %v", err)
	}
	for _, want := range []string{"mtu 1400", "mtu 1500"} {
		if !strings.Contains(toolText(t, both), want) {
			t.Errorf("the unfiltered search dropped %q: %s", want, toolText(t, both))
		}
	}
}

// TestGhostMemorySearch_RefusesAnUnknownTier: refused rather than ignored, for
// the reason the as_of parser is: a filter the caller believes was applied and
// that was not is worse than a filter that fails.
func TestGhostMemorySearch_RefusesAnUnknownTier(t *testing.T) {
	store := testStore(t)
	srv := New(store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test")
	session := connectedClient(t, srv)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "ghost_memory_search",
		Arguments: map[string]any{
			"project_id": "test-project",
			"query":      "anything",
			"retention":  "eventually",
		},
	})
	if err != nil {
		if !strings.Contains(err.Error(), "persistent") {
			t.Errorf("error does not name the vocabulary: %v", err)
		}
		return
	}
	if !result.IsError {
		t.Fatalf("an unknown tier was accepted by the search tool: %q", toolText(t, result))
	}
	if text := toolText(t, result); !strings.Contains(text, "session") || !strings.Contains(text, "persistent") {
		t.Errorf("refusal does not name the vocabulary: %q", text)
	}
}
