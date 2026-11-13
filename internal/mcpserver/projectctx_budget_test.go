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

func mustFit(t *testing.T, name, text string, want ...string) {
	t.Helper()
	if len(text) > searchResponseMaxBytes {
		t.Errorf("%s is %d bytes, budget is %d", name, len(text), searchResponseMaxBytes)
	}
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Errorf("%s missing %q", name, w)
		}
	}
}

func TestProjectContextBudgetSurfacesFit(t *testing.T) {
	store := budgetTestStore(t)
	srv := New(store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test")
	session := connectedClient(t, srv)
	ctx := context.Background()

	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ghost://project/abc123/context"})
	if err != nil {
		t.Fatalf("ReadResource context: %v", err)
	}
	mustFit(t, "project context resource", res.Contents[0].Text, "cut")

	res, err = session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ghost://memories/global"})
	if err != nil {
		t.Fatalf("ReadResource global: %v", err)
	}
	mustFit(t, "global memories resource", res.Contents[0].Text, "cut")

	prompt, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "recall_project", Arguments: map[string]string{"project_id": "abc123"}})
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	text := prompt.Messages[0].Content.(*mcp.TextContent).Text
	mustFit(t, "recall_project", text, "Recall what Ghost knows", "cut")
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
	session := connectedClient(t, New(store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test"))
	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ghost://project/abc123/context"})
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	mustFit(t, "resource", res.Contents[0].Text, "cut")
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
	session := connectedClient(t, New(store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test"))
	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ghost://project/abc123/context"})
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	mustFit(t, "resource", res.Contents[0].Text, "pinned")
}

func longIDToolStore(t *testing.T) *memory.Store {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store := memory.NewStore(db, logger)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject _global: %v", err)
	}
	for i := 0; i < 20; i++ {
		content := fmt.Sprintf("global memory %d %s", i, strings.Repeat("b", 7900))
		if _, _, _, err := store.Upsert(ctx, "_global", "fact", content, "mcp", 0.5, nil); err != nil {
			t.Fatalf("Upsert global: %v", err)
		}
	}
	return store
}

func projectContextToolText(t *testing.T, session *mcp.ClientSession, projectID string) string {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ghost_project_context",
		Arguments: map[string]any{"project_id": projectID},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	text := ""
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	return text
}

func TestProjectContextToolWithAnUnknownProjectIDStaysWithinBudget(t *testing.T) {
	const notRegistered = `Project "no-such-project" is not registered with Ghost yet — nothing has ever been ` +
		`saved for it. Call ghost_memory_save to create it.`
	cases := []struct {
		name         string
		projectID    string
		wantEchoed   bool
		wantSentence string
	}{
		{
			name:       "an id at the bound is echoed whole",
			projectID:  strings.Repeat("a", 512),
			wantEchoed: true,
		},
		{
			name:      "an id past the bound is withheld",
			projectID: strings.Repeat("a", 513),
		},
		{
			name:      "a very long id is withheld",
			projectID: strings.Repeat("a", 20000),
		},
		{
			name:         "an ordinary name is unchanged",
			projectID:    "no-such-project",
			wantEchoed:   true,
			wantSentence: notRegistered,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := longIDToolStore(t)
			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
			srv := New(store, logger, "test")
			session := connectedClient(t, srv)

			text := projectContextToolText(t, session, tc.projectID)
			if len(text) > searchResponseMaxBytes {
				t.Errorf("answer is %d bytes, budget is %d", len(text), searchResponseMaxBytes)
			}
			if !strings.Contains(text, projectContextCutAdvice) {
				t.Errorf("no cut notice in the answer: %.200s", text)
			}
			if tc.wantEchoed {
				if !strings.Contains(text, tc.projectID) {
					t.Errorf("the %d-byte id was not echoed whole", len(tc.projectID))
				}
				if strings.Contains(text, "withheld") {
					t.Errorf("the %d-byte id was withheld: %.200s", len(tc.projectID), text)
				}
			} else {
				if strings.Contains(text, tc.projectID) {
					t.Errorf("the %d-byte id was echoed", len(tc.projectID))
				}
				if !strings.Contains(text, "withheld") {
					t.Errorf("the %d-byte id was not withheld: %.200s", len(tc.projectID), text)
				}
			}
			if tc.wantSentence != "" && !strings.Contains(text, tc.wantSentence) {
				t.Errorf("the not-registered sentence changed: %.200s", text)
			}
		})
	}
}
