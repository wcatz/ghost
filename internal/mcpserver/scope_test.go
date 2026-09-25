package mcpserver

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// saveScoped stores one memory under the given scope (nil for unscoped).
func saveScoped(t *testing.T, session *mcp.ClientSession, content, env string) {
	t.Helper()
	args := map[string]any{
		"project_id": "test-project",
		"content":    content,
		"category":   "fact",
	}
	if env != "" {
		args["scope"] = map[string]any{"environment": env}
	}
	if res := callTool(t, session, "ghost_memory_save", args); res.IsError {
		t.Fatalf("save %q failed: %s", content, resultText(res))
	}
}

// searchScoped runs a search with an optional scope filter and returns the
// rendered listing.
func searchScoped(t *testing.T, session *mcp.ClientSession, query string, scope any) string {
	t.Helper()
	args := map[string]any{
		"project_id": "test-project",
		"query":      query,
		"limit":      10,
	}
	if scope != nil {
		args["scope"] = scope
	}
	res := callTool(t, session, "ghost_memory_search", args)
	if res.IsError {
		t.Fatalf("search failed: %s", resultText(res))
	}
	return resultText(res)
}

// TestScopeFilterSeparatesDevelopmentFromProduction is the scenario that made
// scope worth adding: two claims about the same question in two environments.
//
// Before scope existed these were distinguished only because the sentence
// said "development" or "production", so retrieval had to hope semantic
// similarity ranked the right one. A query scoped to production must now
// exclude the development row by construction — while the unscoped row
// survives, because knowledge that names no environment applies to all of
// them and hiding it would make missing scope a reason to drop the most
// reusable facts in the store.
func TestScopeFilterSeparatesDevelopmentFromProduction(t *testing.T) {
	_, session := newCapSession(t)

	saveScoped(t, session, "The project database for development is SQLite.", "development")
	saveScoped(t, session, "The project database for production is PostgreSQL.", "production")
	saveScoped(t, session, "The project database for Helmfile is global, dev and prod.", "") // unscoped

	// Scoped to production.
	out := searchScoped(t, session, "database", map[string]any{"environment": "production"})
	if !strings.Contains(out, "PostgreSQL") {
		t.Errorf("production-scoped search must return the production memory:\n%s", out)
	}
	if !strings.Contains(out, "The project database for Helmfile") {
		t.Errorf("unscoped memory was excluded — knowledge with no stated environment applies everywhere:\n%s", out)
	}
	if strings.Contains(out, "for development is SQLite") {
		t.Errorf("development memory leaked into a production-scoped search:\n%s", out)
	}

	// Scoped to development: the mirror image, so a filter that excludes
	// everything cannot pass by returning nothing.
	dev := searchScoped(t, session, "database", map[string]any{"environment": "development"})
	if !strings.Contains(dev, "for development is SQLite") {
		t.Errorf("development-scoped search must return the development memory:\n%s", dev)
	}
	if strings.Contains(dev, "for production is PostgreSQL") {
		t.Errorf("production memory leaked into a development-scoped search:\n%s", dev)
	}

	// No scope: everything, because nothing is being asked about environment.
	all := searchScoped(t, session, "database", nil)
	for _, want := range []string{"for development is SQLite", "for production is PostgreSQL", "The project database for Helmfile"} {
		if !strings.Contains(all, want) {
			t.Errorf("unscoped search lost %q:\n%s", want, all)
		}
	}
}

// TestScopeIsRenderedListing checks the scope is visible to the agent rather
// than only usable as a filter — an agent cannot reason about a scope it
// cannot see.
func TestScopeIsRenderedListing(t *testing.T) {
	_, session := newCapSession(t)

	saveScoped(t, session, "The project database runs behind the API gateway.", "production")

	out := searchScoped(t, session, "gateway", nil)
	if !strings.Contains(out, "scope{environment=production}") {
		t.Errorf("scope not rendered in the listing:\n%s", out)
	}
}

// TestSaveAcceptsStringifiedScope covers the client behaviour coerce.go
// documents: some MCP clients send an object as a JSON string when the
// advertised schema is a union. Rejecting that shape would make scope
// unusable from those clients; silently dropping it would make retrieval
// filter on a scope nobody wrote.
func TestSaveAcceptsStringifiedScope(t *testing.T) {
	_, session := newCapSession(t)

	res := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project",
		"content":    "The project database scope arrived as a JSON string.",
		"category":   "fact",
		"scope":      `{"environment": "staging"}`,
	})
	if res.IsError {
		t.Fatalf("stringified scope rejected: %s", resultText(res))
	}

	out := searchScoped(t, session, "database", nil)
	if !strings.Contains(out, "scope{environment=staging}") {
		t.Errorf("stringified scope did not persist:\n%s", out)
	}

	// And it must actually filter, not merely display.
	filtered := searchScoped(t, session, "database", map[string]any{"environment": "production"})
	if strings.Contains(filtered, "scope{environment=staging}") {
		t.Errorf("a memory scoped to staging survived a production-scoped search:\n%s", filtered)
	}
}

// TestScopeRejectsNonStringValues: a scope value that is not a string cannot
// be compared against anything, so accepting it would produce a row that
// silently never matches.
func TestScopeRejectsNonStringValues(t *testing.T) {
	_, session := newCapSession(t)

	res := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project",
		"content":    "A memory with a nonsense scope value.",
		"category":   "fact",
		"scope":      map[string]any{"environment": 42},
	})
	if !res.IsError {
		t.Fatalf("non-string scope value was accepted; it can never match a string filter:\n%s", resultText(res))
	}
}

// TestScopeNarrowingIsReported: when the scope filter drops rows from a full
// window, the caller must be told the result may be short rather than
// presented with a truncated list as everything that matched.
//
// The flag is computed once, before any post-filter runs, and this test
// deliberately runs BOTH filters to catch the case that was reported: with
// category applied first, a scope filter that measures "was the window full"
// against the already-shrunk pool sees a count below the fetch limit and
// concludes nothing was ever withheld.
func TestScopeNarrowingIsReported(t *testing.T) {
	_, session := newCapSession(t)

	// Six rows, so a limit of 2 fetches 2*3 = 6 with a category filter set —
	// a full window. Three are category fact, three gotcha; one of the fact
	// rows is production.
	//
	// Contents differ in vocabulary on purpose: near restatements would fold
	// at save time, the window would never fill, and the assertion would pass
	// for the wrong reason.
	rows := []struct{ content, category, env string }{
		{"The development database is SQLite.", "fact", "development"},
		{"Backups of the analytics database run nightly.", "fact", "development"},
		{"The production database is PostgreSQL.", "fact", "production"},
		{"Retention on the warehouse database is thirty days.", "gotcha", "development"},
		{"A read replica of the reporting database lives elsewhere.", "gotcha", "development"},
		{"Slow queries on the metrics database need an index.", "gotcha", "development"},
	}
	for _, r := range rows {
		args := map[string]any{
			"project_id": "test-project",
			"content":    r.content,
			"category":   r.category,
			"scope":      map[string]any{"environment": r.env},
		}
		if res := callTool(t, session, "ghost_memory_save", args); res.IsError {
			t.Fatalf("save %q failed: %s", r.content, resultText(res))
		}
	}

	res := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "database",
		"category":   "fact",
		"limit":      2,
		"scope":      map[string]any{"environment": "production"},
	})
	if res.IsError {
		t.Fatalf("search failed: %s", resultText(res))
	}
	out := resultText(res)

	// Assert against the row contents that are actually seeded. An earlier
	// revision checked "for development", a substring no surviving row
	// contains — an assertion that can never fail says nothing about the
	// filter, which is worse than no assertion at all.
	for _, devRow := range []string{
		"The development database is SQLite.",
		"Backups of the analytics database run nightly.",
	} {
		if strings.Contains(out, devRow) {
			t.Errorf("development row survived a production-scoped search: %q\n%s", devRow, out)
		}
	}
	// Assert the note exists AND names scope, rather than matching one
	// phrasing: with both filters applied it reads "category and scope
	// filters", and pinning that exact string would make a harmless wording
	// change look like a regression.
	idx := strings.Index(out, "may have missed")
	if idx < 0 {
		t.Errorf("scope narrowing was not reported — a truncated result presented as complete:\n%s", out)
	} else {
		start := idx - 80
		if start < 0 {
			start = 0
		}
		if !strings.Contains(out[start:idx], "scope") {
			t.Errorf("the incompleteness note does not name the scope filter:\n%s", out)
		}
	}
}
