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

func callExpectingErrorText(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return err.Error()
	}
	if !res.IsError {
		t.Fatalf("%s: expected refusal, got %s", name, toolText(t, res))
	}
	return toolText(t, res)
}

func TestRetentionRoundTripsAndDefaults(t *testing.T) {
	store := testStore(t)
	srv := New(store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test")
	session := connectedClient(t, srv)
	ctx := context.Background()
	for _, tc := range []struct{ tier, content string }{
		{memory.RetentionSession, "the staging deploy finished at 09:12 and the console is still up"},
		{memory.RetentionProject, "the migration runner needs its own lock table, decided at the review"},
		{memory.RetentionPersistent, "run gofmt over cmd and internal before every commit, forever"},
	} {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ghost_memory_save", Arguments: map[string]any{"project_id": "test-project", "content": tc.content, "retention": tc.tier}})
		if err != nil || res.IsError {
			t.Fatalf("%s: err=%v res=%v", tc.tier, err, res)
		}
		if !strings.Contains(toolText(t, res), tc.tier) {
			t.Fatalf("result missing tier: %q", toolText(t, res))
		}
	}
	mems, _ := store.GetByCategory(ctx, "abc123", "fact", 50)
	by := map[string]string{}
	for _, m := range mems {
		by[m.Content] = m.Retention
	}
	for _, tc := range []struct{ tier, content string }{
		{memory.RetentionSession, "the staging deploy finished at 09:12 and the console is still up"},
		{memory.RetentionProject, "the migration runner needs its own lock table, decided at the review"},
		{memory.RetentionPersistent, "run gofmt over cmd and internal before every commit, forever"},
	} {
		if by[tc.content] != tc.tier {
			t.Fatalf("%q retention=%q want %q", tc.content, by[tc.content], tc.tier)
		}
	}
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ghost_memory_save", Arguments: map[string]any{"project_id": "test-project", "content": "a save that names no tier"}}); err != nil {
		t.Fatal(err)
	}
	all, _ := store.GetAll(ctx, "abc123", 50)
	found := false
	for _, m := range all {
		if m.Content == "a save that names no tier" {
			found = true
			if m.Retention != memory.RetentionProject {
				t.Fatalf("default retention=%q", m.Retention)
			}
		}
	}
	if !found {
		t.Fatal("default-tier row missing")
	}
}

func TestRetentionRefusals(t *testing.T) {
	store := testStore(t)
	srv := New(store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test")
	session := connectedClient(t, srv)
	ctx := context.Background()
	for _, bad := range []string{"forever", "keep-forever", "eventually"} {
		text := callExpectingErrorText(t, session, "ghost_memory_save", map[string]any{"project_id": "test-project", "content": "x " + bad, "retention": bad})
		if !strings.Contains(text, "session") || !strings.Contains(text, "persistent") {
			t.Fatalf("save refusal %q: %q", bad, text)
		}
		text = callExpectingErrorText(t, session, "ghost_memories_list", map[string]any{"project_id": "test-project", "retention": bad})
		if !strings.Contains(text, "session") || !strings.Contains(text, "persistent") {
			t.Fatalf("list refusal %q: %q", bad, text)
		}
		text = callExpectingErrorText(t, session, "ghost_memory_search", map[string]any{"project_id": "test-project", "query": "anything", "retention": bad})
		if !strings.Contains(text, "session") || !strings.Contains(text, "persistent") {
			t.Fatalf("search refusal %q: %q", bad, text)
		}
	}
	if mems, _ := store.GetAll(ctx, "abc123", 50); len(mems) != 0 {
		t.Fatalf("refused saves wrote %d rows", len(mems))
	}
	text := callExpectingErrorText(t, session, "ghost_save_global", map[string]any{"content": "a global memory whose tier is a typo", "retention": "eventually"})
	if !strings.Contains(text, "persistent") || !strings.Contains(text, "eventually") {
		t.Fatalf("global refusal: %q", text)
	}
}

func TestRetentionListAndSearchFilters(t *testing.T) {
	store := testStore(t)
	srv := New(store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test")
	session := connectedClient(t, srv)
	ctx := context.Background()
	seed := map[string]string{
		"the lab tunnel has mtu 1400 while we debug fragmentation":   memory.RetentionSession,
		"production ingress requires mtu 1500 for jumbo frames":      memory.RetentionPersistent,
		"the deploy runbook says to drain the queue before rotating": memory.RetentionProject,
	}
	for content, tier := range seed {
		if _, _, _, err := store.UpsertWithOptions(ctx, "abc123", "fact", content, "mcp", 0.6, nil, memory.UpsertOptions{Retention: tier}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tier := range []string{memory.RetentionSession, memory.RetentionPersistent} {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ghost_memories_list", Arguments: map[string]any{"project_id": "test-project", "retention": tier}})
		if err != nil {
			t.Fatal(err)
		}
		text := toolText(t, res)
		count := 0
		for content, want := range seed {
			if strings.Contains(text, content) {
				count++
				if want != tier {
					t.Fatalf("list(%s) included wrong tier row: %q", tier, text)
				}
			}
		}
		if count != 1 {
			t.Fatalf("list(%s) count=%d: %q", tier, count, text)
		}
	}
	all, _ := session.CallTool(ctx, &mcp.CallToolParams{Name: "ghost_memories_list", Arguments: map[string]any{"project_id": "test-project"}})
	for content := range seed {
		if !strings.Contains(toolText(t, all), content) {
			t.Fatalf("unfiltered list dropped %q", content)
		}
	}

	// #573's argument, for the tier: filter must be applied before window closes.
	for _, tc := range []struct{ tier, want, unwanted string }{
		{memory.RetentionSession, "mtu 1400", "mtu 1500"},
		{memory.RetentionPersistent, "mtu 1500", "mtu 1400"},
	} {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ghost_memory_search", Arguments: map[string]any{"project_id": "test-project", "query": "tunnel mtu", "retention": tc.tier}})
		if err != nil {
			t.Fatal(err)
		}
		text := toolText(t, res)
		if !strings.Contains(text, tc.want) || strings.Contains(text, tc.unwanted) {
			t.Fatalf("search(%s): %q", tc.tier, text)
		}
	}
	both, _ := session.CallTool(ctx, &mcp.CallToolParams{Name: "ghost_memory_search", Arguments: map[string]any{"project_id": "test-project", "query": "tunnel mtu"}})
	for _, want := range []string{"mtu 1400", "mtu 1500"} {
		if !strings.Contains(toolText(t, both), want) {
			t.Fatalf("unfiltered search dropped %q", want)
		}
	}
}

func TestGhostSaveGlobal_RoundTripsTheRetentionTier(t *testing.T) {
	store := testStore(t)
	srv := New(store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test")
	session := connectedClient(t, srv)
	ctx := context.Background()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ghost_save_global", Arguments: map[string]any{"content": "yesterday's standup notes are in the shared drive, not in this repo", "retention": memory.RetentionSession}})
	if err != nil || res.IsError || !strings.Contains(toolText(t, res), memory.RetentionSession) {
		t.Fatalf("save_global: err=%v res=%v", err, res)
	}
	mems, _ := store.GetAll(ctx, "_global", 50)
	found := false
	for _, m := range mems {
		if strings.HasPrefix(m.Content, "yesterday's standup notes") {
			found = true
			if m.Retention != memory.RetentionSession {
				t.Fatalf("global retention=%q", m.Retention)
			}
		}
	}
	if !found {
		t.Fatal("global memory missing")
	}
}
