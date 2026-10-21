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

func TestProjectContextResourceUnresolvedNameAfterGlobalRowsHasOneBlankLine(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject _global: %v", err)
	}
	if _, _, _, err := store.Upsert(ctx, "_global", "fact", "a small global fact", "mcp", 0.5, nil); err != nil {
		t.Fatalf("Upsert global: %v", err)
	}
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)

	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ghost://project/nope/context"})
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	text := res.Contents[0].Text
	if !strings.Contains(text, "\n\nProject ") {
		t.Fatalf("missing not-registered sentence after a blank line: %q", text)
	}
	if strings.Contains(text, "\n\n\nProject ") {
		t.Errorf("two blank lines before the not-registered sentence: %q", text)
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

// longIDToolStore is the store the project-context tool's budget cases share: a
// `_global` bucket holding twenty rows of about 7,900 bytes each, so the answer
// on every path is well over the 16,000-byte budget and whole rows are cut.
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

// projectContextToolText calls ghost_project_context and returns its text.
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

// The tool's NOT-REGISTERED answer is the one that echoed the caller's project
// id whole, and the echo is FRAMING: a framing at or over the budget makes the
// fit pass keep every row, so the cap stopped applying at all and a 20,000-byte
// id produced an answer of about 180 KB with no cut notice.
//
// The id is bounded at 512 bytes — the bound the context resource and the
// recall_project prompt already use — so an id past it is withheld and the
// framing stays small enough for the cut to apply. An id at or under the bound
// is still echoed whole, because the sentence names the project the caller
// asked about.
//
// The ordinary name is the case that must not move: its sentence is the one
// origin/main printed, byte for byte.
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

// A REGISTERED long project id is a REGRESSION GUARD, not a reproduction: the
// tool never echoes a resolved id, so this answer was within the budget before
// the not-registered answer was bounded. It is held here because that is the
// property the bound relies on — the echo is on the unknown-name path only.
func TestProjectContextToolWithARegisteredLongProjectIDStaysWithinBudget(t *testing.T) {
	longID := strings.Repeat("a", 20000)
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store := memory.NewStore(db, logger)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, longID, longID, longID); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if err := store.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject _global: %v", err)
	}
	for i := 0; i < 20; i++ {
		content := fmt.Sprintf("project memory %d %s", i, strings.Repeat("a", 7900))
		if _, _, _, err := store.Upsert(ctx, longID, "fact", content, "mcp", 0.5, nil); err != nil {
			t.Fatalf("Upsert project: %v", err)
		}
	}
	for i := 0; i < 20; i++ {
		content := fmt.Sprintf("global memory %d %s", i, strings.Repeat("b", 7900))
		if _, _, _, err := store.Upsert(ctx, "_global", "fact", content, "mcp", 0.5, nil); err != nil {
			t.Fatalf("Upsert global: %v", err)
		}
	}
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)

	text := projectContextToolText(t, session, longID)
	if len(text) > searchResponseMaxBytes {
		t.Errorf("answer is %d bytes, budget is %d", len(text), searchResponseMaxBytes)
	}
	if !strings.Contains(text, projectContextCutAdvice) {
		t.Errorf("no cut notice in the answer: %.200s", text)
	}
}
