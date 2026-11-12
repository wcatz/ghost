package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
)

// TestAResolvedProjectsGlobalRowsSitUnderTheGlobalHeading is #809.
func TestAResolvedProjectsGlobalRowsSitUnderTheGlobalHeading(t *testing.T) {
	srv, session := newValiditySession(t)
	ctx := context.Background()

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "ghost_memory_save",
		Arguments: map[string]any{
			"project_id": "vproj", "content": "a claim about this project only", "category": "preference",
		},
	}); err != nil {
		t.Fatalf("ghost_memory_save: %v", err)
	}
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_save_global",
		Arguments: map[string]any{"content": "a cross-project preference", "category": "preference"},
	}); err != nil {
		t.Fatalf("ghost_save_global: %v", err)
	}

	assertSplit := func(surface, out string) {
		t.Helper()
		memories, globals, ok := strings.Cut(out, globalSectionHeading)
		if !ok {
			t.Fatalf("%s rendered no %q section at all:\n%s", surface, globalSectionHeading, out)
		}
		if !strings.Contains(memories, memorySectionHeading) {
			t.Errorf("%s rendered no %q section:\n%s", surface, memorySectionHeading, out)
		}
		if !strings.Contains(memories, "a claim about this project only") {
			t.Errorf("%s put no row of the project's own under %q:\n%s", surface, memorySectionHeading, out)
		}
		if strings.Contains(memories, "a cross-project preference") {
			t.Errorf("%s listed a cross-project row under %q, with nothing marking it as global — which is "+
				"the row the SessionStart trust guidance is written about:\n%s", surface, memorySectionHeading, out)
		}
		if !strings.Contains(globals, "a cross-project preference") {
			t.Errorf("%s did not deliver the cross-project row under %q:\n%s", surface, globalSectionHeading, out)
		}
		if n := strings.Count(out, globalSectionHeading); n != 1 {
			t.Errorf("%s carries %d %q headings, want exactly one:\n%s", surface, n, globalSectionHeading, out)
		}
		if n := strings.Count(out, memorySectionHeading); n != 1 {
			t.Errorf("%s carries %d %q headings, want exactly one:\n%s", surface, n, memorySectionHeading, out)
		}
	}

	assertSplit("ghost_project_context", resultText(callTool(t, session, "ghost_project_context",
		map[string]any{"project_id": "vproj"})))

	text, err := srv.buildProjectContext(ctx, "vproj", "", searchResponseMaxBytes)
	if err != nil {
		t.Fatalf("buildProjectContext: %v", err)
	}
	assertSplit("ghost://project/{id}/context", text)

	assertSplit("ghost_project_context as_of", resultText(callTool(t, session, "ghost_project_context",
		map[string]any{"project_id": "vproj", "as_of": "2099-01-01T00:00:00Z"})))
}

// case, and a review of #817 found it after the split shipped: `_global` IS a
func TestTheGlobalProjectContextRendersItsOwnWindow(t *testing.T) {
	srv, session := newValiditySession(t)
	ctx := context.Background()

	const globals = 22
	for i := 0; i < globals; i++ {
		if _, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name: "ghost_save_global",
			Arguments: map[string]any{
				"content":  fmt.Sprintf("a cross-project preference numbered %02d", i),
				"category": "preference",
			},
		}); err != nil {
			t.Fatalf("save_global %d: %v", i, err)
		}
	}

	assertWholeBlock := func(surface, out string) {
		t.Helper()
		for _, census := range []string{"No memories found for this project.", "is not registered with Ghost yet"} {
			if strings.Contains(out, census) {
				t.Errorf("%s answered %q on a store holding %d global memories:\n%s", surface, census, globals, out)
			}
		}
		if strings.Contains(out, memorySectionHeading) {
			t.Errorf("%s rendered a %q section for a project whose window IS the globals, so it can only be "+
				"empty:\n%s", surface, memorySectionHeading, out)
		}
		if n := strings.Count(out, globalSectionHeading); n != 1 {
			t.Errorf("%s carries %d %q headings, want exactly one:\n%s", surface, n, globalSectionHeading, out)
		}
		if n := strings.Count(out, "a cross-project preference numbered"); n != projectContextMemoriesCap {
			t.Errorf("%s rendered %d global rows, want the %d its window cap admits:\n%s",
				surface, n, projectContextMemoriesCap, out)
		}
	}

	text, err := srv.buildProjectContext(ctx, memory.GlobalProjectID, "", searchResponseMaxBytes)
	if err != nil {
		t.Fatalf("buildProjectContext(_global): %v", err)
	}
	assertWholeBlock("ghost://project/_global/context", text)

	tool := resultText(callTool(t, session, "ghost_project_context", map[string]any{
		"project_id": memory.GlobalProjectID,
	}))
	assertWholeBlock("ghost_project_context", tool)

	if got, want := strings.Count(tool, "\n- ["), strings.Count(text, "\n- ["); got != want {
		t.Errorf("the tool carries %d rows and the resource %d for the same request, so the two surfaces "+
			"disagree about it:\n--- tool ---\n%s\n--- resource ---\n%s", got, want, tool, text)
	}
	if n := strings.Count(text, "\n- ["); n != projectContextMemoriesCap {
		t.Errorf("the _global block carries %d rows, want the %d its window cap admits:\n%s",
			n, projectContextMemoriesCap, text)
	}
}

func TestTheProjectContextSplitMovesRowsWithoutChangingTheBlock(t *testing.T) {
	srv, session := newValiditySession(t)
	ctx := context.Background()

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "ghost_memory_save",
		Arguments: map[string]any{
			"project_id": "vproj", "content": "the only row of this project's own", "category": "preference",
			"importance": 0.9,
		},
	}); err != nil {
		t.Fatalf("ghost_memory_save: %v", err)
	}
	const globals = 26
	for i := 0; i < globals; i++ {
		if _, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name: "ghost_save_global",
			Arguments: map[string]any{
				"content":  fmt.Sprintf("a cross-project preference numbered %02d", i),
				"category": "preference",
			},
		}); err != nil {
			t.Fatalf("save_global %d: %v", i, err)
		}
	}

	text, err := srv.buildProjectContext(ctx, "vproj", "", searchResponseMaxBytes)
	if err != nil {
		t.Fatalf("buildProjectContext: %v", err)
	}

	if n := strings.Count(text, globalSectionHeading); n != 1 {
		t.Errorf("the block carries %d %q headings, want exactly one:\n%s", n, globalSectionHeading, text)
	}

	memories, _, _ := strings.Cut(text, globalSectionHeading)
	if n := strings.Count(memories, "the only row of this project's own"); n != 1 {
		t.Errorf("the project's own row appears %d time(s) under %q:\n%s", n, memorySectionHeading, text)
	}
	cut := 0
	for i := 0; i < globals; i++ {
		switch n := strings.Count(text, fmt.Sprintf("numbered %02d»", i)); {
		case n > 1:
			t.Errorf("the global row numbered %02d appears %d time(s) in the block; the split moved rows between "+
				"sections and must not duplicate one:\n%s", i, n, text)
		case n == 0:
			cut++
		}
	}
	if n := strings.Count(text, "\n- ["); n != projectContextMemoriesCap {
		t.Errorf("the block holds %d memory rows, want the %d the mixed window's cap admits: the split moved "+
			"rows between sections, it did not add or drop any\n%s", n, projectContextMemoriesCap, text)
	}
	if cut == 0 {
		t.Errorf("every one of the %d global rows reached the block, so the window did not cut and this test "+
			"proves nothing about the reconciliation:\n%s", globals, text)
	}
}
