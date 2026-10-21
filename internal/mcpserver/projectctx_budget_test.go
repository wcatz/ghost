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

func budgetTestStore(t *testing.T) *memory.Store {
	t.Helper()
	store := testStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject _global: %v", err)
	}
	for i := 0; i < 20; i++ {
		content := fmt.Sprintf("project memory %d %s", i, strings.Repeat("a", 7900))
		if _, _, _, err := store.Upsert(ctx, "abc123", "fact", content, "mcp", 0.5, nil); err != nil {
			t.Fatalf("Upsert project: %v", err)
		}
	}
	for i := 0; i < 20; i++ {
		content := fmt.Sprintf("global memory %d %s", i, strings.Repeat("b", 7900))
		if _, _, _, err := store.Upsert(ctx, "_global", "fact", content, "mcp", 0.5, nil); err != nil {
			t.Fatalf("Upsert global: %v", err)
		}
	}
	return store
}

func TestProjectContextResourceFitsResponseBudget(t *testing.T) {
	store := budgetTestStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)
	ctx := context.Background()

	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ghost://project/abc123/context"})
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	text := res.Contents[0].Text
	if len(text) > searchResponseMaxBytes {
		t.Errorf("project context resource is %d bytes, budget is %d", len(text), searchResponseMaxBytes)
	}
	if !strings.Contains(text, "cut") {
		t.Errorf("expected a cut notice, got none")
	}
}

func TestGlobalMemoriesResourceFitsResponseBudget(t *testing.T) {
	store := budgetTestStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)
	ctx := context.Background()

	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ghost://memories/global"})
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	text := res.Contents[0].Text
	if len(text) > searchResponseMaxBytes {
		t.Errorf("global memories resource is %d bytes, budget is %d", len(text), searchResponseMaxBytes)
	}
	if !strings.Contains(text, "cut") {
		t.Errorf("expected a cut notice, got none")
	}
}

func TestRecallProjectPromptFitsResponseBudget(t *testing.T) {
	store := budgetTestStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)
	ctx := context.Background()

	res, err := session.GetPrompt(ctx, &mcp.GetPromptParams{
		Name:      "recall_project",
		Arguments: map[string]string{"project_id": "abc123"},
	})
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	if len(res.Messages) == 0 {
		t.Fatal("no messages")
	}
	tc, ok := res.Messages[0].Content.(*mcp.TextContent)
	if !ok {
		t.Fatal("not text content")
	}
	text := tc.Text
	if !strings.Contains(text, "Recall what Ghost knows") {
		t.Errorf("preface missing")
	}
	if len(text) > searchResponseMaxBytes {
		t.Errorf("recall_project prompt is %d bytes, budget is %d", len(text), searchResponseMaxBytes)
	}
	if !strings.Contains(text, "cut") {
		t.Errorf("expected a cut notice, got none")
	}
	if !strings.Contains(text, "project memory") && !strings.Contains(text, "global memory") {
		t.Errorf("no whole memory row survived")
	}
}

func TestProjectContextResourceLargeDecisionsFitBudget(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, _, _, err := store.RecordDecision(ctx, "abc123", fmt.Sprintf("title %d", i), strings.Repeat("d", 8000), "", nil, nil); err != nil {
			t.Fatalf("RecordDecision: %v", err)
		}
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)

	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ghost://project/abc123/context"})
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	text := res.Contents[0].Text
	if len(text) > searchResponseMaxBytes {
		t.Errorf("resource is %d bytes, budget is %d", len(text), searchResponseMaxBytes)
	}
	if !strings.Contains(text, "cut") {
		t.Errorf("expected a cut notice, got none")
	}
}

func TestProjectContextResourcePinnedRowFitsBudget(t *testing.T) {
	store := budgetTestStore(t)
	ctx := context.Background()
	mems, err := store.GetTopMemories(ctx, "abc123", 1)
	if err != nil || len(mems) == 0 {
		t.Fatalf("GetTopMemories: %v %d", err, len(mems))
	}
	if err := store.TogglePin(ctx, mems[0].ID, true); err != nil {
		t.Fatalf("TogglePin: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)

	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ghost://project/abc123/context"})
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	text := res.Contents[0].Text
	if len(text) > searchResponseMaxBytes {
		t.Errorf("resource is %d bytes, budget is %d", len(text), searchResponseMaxBytes)
	}
	if !strings.Contains(text, "pinned") {
		t.Errorf("expected a pinned notice, got none")
	}
}

func TestProjectContextResourceUnresolvedNameFitsBudget(t *testing.T) {
	store := budgetTestStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)
	ctx := context.Background()

	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ghost://project/does-not-exist/context"})
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	text := res.Contents[0].Text
	if len(text) > searchResponseMaxBytes {
		t.Errorf("resource is %d bytes, budget is %d", len(text), searchResponseMaxBytes)
	}
	if !strings.Contains(text, "is not registered") {
		t.Errorf("expected not-registered sentence")
	}
}

func TestProjectContextResourceUnresolvedEmptyStoreNoLeadingNewline(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)
	ctx := context.Background()

	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ghost://project/nope/context"})
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	text := res.Contents[0].Text
	if strings.HasPrefix(text, "\n") {
		t.Errorf("leading newline: %q", text[:10])
	}
	if !strings.Contains(text, "No memories found for this project.") {
		t.Errorf("missing census line: %q", text)
	}
	if !strings.Contains(text, "Project") || !strings.Contains(text, "is not registered") {
		t.Errorf("missing not-registered sentence: %q", text)
	}
}

func TestProjectContextResourceLongUnresolvedNameFitsBudget(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)
	ctx := context.Background()

	longName := strings.Repeat("a", 20000)
	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ghost://project/" + longName + "/context"})
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	text := res.Contents[0].Text
	if len(text) > searchResponseMaxBytes {
		t.Errorf("resource is %d bytes, budget is %d", len(text), searchResponseMaxBytes)
	}
	if !strings.Contains(text, "is not registered") {
		t.Errorf("expected not-registered sentence")
	}
	if strings.Contains(text, longName) {
		t.Errorf("long name leaked into response")
	}
}

func TestRecallProjectPromptLongUnresolvedNameFitsBudget(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)
	ctx := context.Background()

	longName := strings.Repeat("a", 20000)
	res, err := session.GetPrompt(ctx, &mcp.GetPromptParams{
		Name:      "recall_project",
		Arguments: map[string]string{"project_id": longName},
	})
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	tc := res.Messages[0].Content.(*mcp.TextContent)
	text := tc.Text
	if !strings.Contains(text, "is not registered") {
		t.Errorf("expected not-registered sentence")
	}
	if !strings.Contains(text, "Recall what Ghost knows") {
		t.Errorf("preface missing")
	}
	if len(text) > searchResponseMaxBytes {
		t.Errorf("prompt is %d bytes, budget is %d", len(text), searchResponseMaxBytes)
	}
	if strings.Contains(text, longName) {
		t.Errorf("long name echoed whole")
	}
}

func TestRecallProjectPromptResolvedLongNameFitsBudget(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	longID := strings.Repeat("d", 10000)
	if err := store.EnsureProject(ctx, longID, "/tmp/long", longID); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	for i := 0; i < 20; i++ {
		content := fmt.Sprintf("project memory %d %s", i, strings.Repeat("a", 7900))
		if _, _, _, err := store.Upsert(ctx, longID, "fact", content, "mcp", 0.5, nil); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}
	if err := store.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject _global: %v", err)
	}
	for i := 0; i < 20; i++ {
		content := fmt.Sprintf("global memory %d %s", i, strings.Repeat("b", 7900))
		if _, _, _, err := store.Upsert(ctx, "_global", "fact", content, "mcp", 0.5, nil); err != nil {
			t.Fatalf("Upsert global: %v", err)
		}
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)

	res, err := session.GetPrompt(ctx, &mcp.GetPromptParams{
		Name:      "recall_project",
		Arguments: map[string]string{"project_id": longID},
	})
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	tc := res.Messages[0].Content.(*mcp.TextContent)
	text := tc.Text
	if !strings.Contains(text, "Recall what Ghost knows") {
		t.Errorf("preface missing")
	}
	if len(text) > searchResponseMaxBytes {
		t.Errorf("prompt is %d bytes, budget is %d", len(text), searchResponseMaxBytes)
	}
	if strings.Contains(text, longID) {
		t.Errorf("long name echoed whole")
	}
}

func TestRecallProjectPromptNearBudget(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject _global: %v", err)
	}
	for i := 0; i < 20; i++ {
		content := fmt.Sprintf("project memory %d %s", i, strings.Repeat("a", 5100))
		if _, _, _, err := store.Upsert(ctx, "abc123", "fact", content, "mcp", 0.5, nil); err != nil {
			t.Fatalf("Upsert project: %v", err)
		}
	}
	for i := 0; i < 20; i++ {
		content := fmt.Sprintf("global memory %d %s", i, strings.Repeat("b", 5100))
		if _, _, _, err := store.Upsert(ctx, "_global", "fact", content, "mcp", 0.5, nil); err != nil {
			t.Fatalf("Upsert global: %v", err)
		}
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)

	res, err := session.GetPrompt(ctx, &mcp.GetPromptParams{
		Name:      "recall_project",
		Arguments: map[string]string{"project_id": "abc123"},
	})
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	tc := res.Messages[0].Content.(*mcp.TextContent)
	text := tc.Text
	t.Logf("near-budget prompt length: %d", len(text))
	if !strings.Contains(text, "Recall what Ghost knows") {
		t.Errorf("preface missing")
	}
	if len(text) < 15800 || len(text) > searchResponseMaxBytes {
		t.Errorf("prompt length %d outside 15800..%d", len(text), searchResponseMaxBytes)
	}
}
