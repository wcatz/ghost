package mcpserver

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestCategorySearchReachesRowsBeyondTheWindow is the half of #573 that
// #596's narrower fix left open. Scope moved into window selection; category
// stayed a post-filter over the closed window, so a memory in the requested
// category that ranked below the window was never seen and the tool answered
// "No matching memories found." while the memory existed. Category is now
// applied by the assembler before the window closes, so an eligible row beyond
// it takes the slot.
func TestCategorySearchReachesRowsBeyondTheWindow(t *testing.T) {
	_, session := newCapSession(t)

	// Twenty short, term-dense rows in another category, then the one row that
	// answers the question. The search asks for five, and the tool used to
	// widen its fetch to three times that — fifteen rows, all of them gotchas —
	// so the wanted row had to be reached from beyond the closed window to be
	// found at all.
	for i := range 20 {
		content := "database configuration pooling timeout " + string(rune('a'+i))
		res := callTool(t, session, "ghost_memory_save", map[string]any{
			"project_id": "test-project",
			"content":    content,
			"category":   "gotcha",
		})
		if res.IsError {
			t.Fatalf("save gotcha %d: %s", i, resultText(res))
		}
	}
	res := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project",
		"content":    "database configuration replication lag and failover promotion quorum",
		"category":   "architecture",
	})
	if res.IsError {
		t.Fatalf("save architecture: %s", resultText(res))
	}

	out := searchWithCategory(t, session, "database configuration", "architecture", 5)

	if strings.Contains(out, "No matching memories found") {
		t.Fatalf("category search reported absence while a matching memory exists:\n%s", out)
	}
	if !strings.Contains(out, "quorum") {
		t.Errorf("the in-category row was not returned — category ran after the window closed:\n%s", out)
	}
	if strings.Contains(out, "pooling timeout") {
		t.Errorf("an out-of-category row survived the filter:\n%s", out)
	}
}

// TestCategorySearchStillFillsTheWindow: moving the filter earlier must not
// cost the rows a window that was already wide enough returned.
func TestCategorySearchStillFillsTheWindow(t *testing.T) {
	_, session := newCapSession(t)

	for i, content := range []string{
		"database configuration pooling timeout retry backoff alpha",
		"database configuration charset collation vacuum beta",
	} {
		res := callTool(t, session, "ghost_memory_save", map[string]any{
			"project_id": "test-project",
			"content":    content,
			"category":   "architecture",
		})
		if res.IsError {
			t.Fatalf("save architecture %d: %s", i, resultText(res))
		}
	}
	for i := range 5 {
		res := callTool(t, session, "ghost_memory_save", map[string]any{
			"project_id": "test-project",
			"content":    "database configuration filler row " + string(rune('a'+i)),
			"category":   "gotcha",
		})
		if res.IsError {
			t.Fatalf("save gotcha %d: %s", i, resultText(res))
		}
	}

	out := searchWithCategory(t, session, "database configuration", "architecture", 2)

	for _, want := range []string{"pooling timeout retry backoff alpha", "charset collation vacuum beta"} {
		if !strings.Contains(out, want) {
			t.Errorf("in-category row missing from a full window: %q\n%s", want, out)
		}
	}
}

// TestSearchWithoutFiltersIsUnchanged: the seam is only allowed to change
// membership where a predicate was involved, so an ordinary search keeps the
// item format and the rows it returned before.
func TestSearchWithoutFiltersIsUnchanged(t *testing.T) {
	_, session := newCapSession(t)
	for i, content := range []string{
		"database configuration pooling timeout retry",
		"database configuration charset collation vacuum",
	} {
		res := callTool(t, session, "ghost_memory_save", map[string]any{
			"project_id": "test-project",
			"content":    content,
			"category":   "gotcha",
			"importance": 0.8,
			"tags":       []string{"db"},
		})
		if res.IsError {
			t.Fatalf("save %d: %s", i, resultText(res))
		}
	}

	out := searchScopedWithLimit(t, session, "database configuration", nil, 2)

	if strings.Contains(out, "No matching memories found") {
		t.Fatalf("plain search found nothing:\n%s", out)
	}
	// Item format: "- [category] `id` (importance [pinned] tags:[...] ... ) «content»"
	if !strings.Contains(out, "- [gotcha] `") {
		t.Errorf("item line lost its category and id format:\n%s", out)
	}
	if !strings.Contains(out, "(0.8") || !strings.Contains(out, `tags:["db"]`) {
		t.Errorf("item line lost importance or tags:\n%s", out)
	}
	if !strings.Contains(out, "«database configuration pooling timeout retry»") {
		t.Errorf("item line lost its quoted content:\n%s", out)
	}
	if strings.Contains(out, "[ghost:") {
		t.Errorf("search rendered a machine outcome line before the abstention work lands:\n%s", out)
	}
}

// searchWithCategory runs a category-filtered search and returns the rendered
// listing.
func searchWithCategory(t *testing.T, session *mcp.ClientSession, query, category string, limit int) string {
	t.Helper()
	res := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      query,
		"category":   category,
		"limit":      limit,
	})
	if res.IsError {
		t.Fatalf("search failed: %s", resultText(res))
	}
	return resultText(res)
}
