package mcpserver

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// seedTermDense stores short, term-dense rows in the given scope, plus one
// long row that shares the query terms but ranks weakly under bm25. The long
// row is the one a scope search has to be able to reach.
func seedTermDense(t *testing.T, session *mcp.ClientSession, shortScope, longScope string) {
	t.Helper()
	for _, s := range []string{
		"database configuration pooling timeout retry backoff cache",
		"database configuration charset collation vacuum analyze",
		"database configuration isolation repeatable read locking",
		"database configuration index tuning query planner hints",
	} {
		saveScoped(t, session, s, shortScope)
	}
	// Long, so bm25 ranks it below the four above despite sharing the terms.
	// It carries the OTHER scope, which is the whole point: a scope search has
	// to reach past the rows that outrank it to find the one that matches.
	saveScoped(t, session, "database configuration replication lag failover promotion quorum elections "+
		"together with the long tail of operational detail that makes this row rank weakly under bm25 "+
		"even though it is the only one that answers the question", longScope)
}

// TestScopeSearchReachesRowsBeyondTheWindow is issue #573.
//
// The scope filter ran as a post-filter over the already-selected window, so
// with limit 3 it saw only the three highest-ranked development rows. The
// production row was present in the wider FTS candidate fetch but had already
// been cut before scope ran, and the tool answered "No matching memories
// found." Selection applies scope to the fused pool now, so the low-ranked
// production candidate can occupy the window instead.
func TestScopeSearchReachesRowsBeyondTheWindow(t *testing.T) {
	_, session := newCapSession(t)
	seedTermDense(t, session, "development", "production")

	out := searchScopedWithLimit(t, session, "database configuration", map[string]string{"environment": "production"}, 3)

	if strings.Contains(out, "No matching memories found") {
		t.Fatalf("scope search reported absence while a production row exists:\n%s", out)
	}
	if !strings.Contains(out, "quorum") {
		t.Errorf("the production row was not returned — scope ran after the result window was cut:\n%s", out)
	}
	if strings.Contains(out, "pooling timeout retry backoff cache") {
		t.Errorf("an out-of-scope development row survived selection:\n%s", out)
	}
}

// TestScopeAndCategoryShortResultReportsIncompleteness covers the combined
// filter case. Scope can fill the caller's requested limit before category
// runs; if category then removes enough rows, the post-filtered answer is still
// short and must not be presented as exhaustive.
func TestScopeAndCategoryShortResultReportsIncompleteness(t *testing.T) {
	_, session := newCapSession(t)
	for i, content := range []string{
		"production database row security policy alpha",
		"production database backup retention beta",
		"production database replication lag gamma",
	} {
		res := callTool(t, session, "ghost_memory_save", map[string]any{
			"project_id": "test-project",
			"content":    content,
			"category":   "gotcha",
			"scope":      map[string]any{"environment": "production"},
		})
		if res.IsError {
			t.Fatalf("save gotcha %d: %s", i, resultText(res))
		}
	}
	saveScoped(t, session, "production database connection pool sizing", "production")

	res := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "production database",
		"category":   "fact",
		"limit":      2,
		"scope":      map[string]any{"environment": "production"},
	})
	if res.IsError {
		t.Fatalf("search failed: %s", resultText(res))
	}
	out := resultText(res)
	if !strings.Contains(out, "connection pool sizing") {
		t.Fatalf("in-scope fact row missing:\n%s", out)
	}
	if !strings.Contains(out, "may exist") || !strings.Contains(out, "category and scope filters") {
		t.Errorf("short combined-filter result did not report that the windowed search was incomplete:\n%s", out)
	}
}

// TestScopeSearchStillReturnsInScopeRows: the fix must reach further without
// losing the rows a widened window returns anyway.
func TestScopeSearchStillReturnsInScopeRows(t *testing.T) {
	_, session := newCapSession(t)
	seedTermDense(t, session, "production", "production")

	out := searchScopedWithLimit(t, session, "database configuration", map[string]string{"environment": "production"}, 3)

	if strings.Contains(out, "No matching memories found") {
		t.Fatalf("in-scope rows were not found:\n%s", out)
	}
	if !strings.Contains(out, "pooling timeout retry backoff cache") {
		t.Errorf("in-scope row missing:\n%s", out)
	}
}

// TestUnscopedRowsStayEligibleUnderAScopeFilter: a row that says nothing about
// a requested scope key is not in conflict with it — general knowledge applies
// everywhere, and hiding it would make a missing scope a reason to drop the
// most reusable facts in the store. Silence is not disagreement.
func TestUnscopedRowsStayEligibleUnderAScopeFilter(t *testing.T) {
	_, session := newCapSession(t)
	saveScoped(t, session, "database configuration pooling retry backoff for the general case", "")

	out := searchScopedWithLimit(t, session, "database configuration", map[string]string{"environment": "production"}, 3)

	if strings.Contains(out, "No matching memories found") {
		t.Errorf("an unscoped row was dropped by a scope filter — silence about a key is not disagreement:\n%s", out)
	}
}

// TestScopeSearchZeroResultNamesTheFilter: when a scoped search finds nothing,
// saying only "no matching memories" claims the store has no such memory. That
// may only be true of the candidates searched, and the caller cannot tell from
// the text alone — so the caveat belongs on the zero-result answer too, not
// only on the partial one.
func TestScopeSearchZeroResultNamesTheFilter(t *testing.T) {
	_, session := newCapSession(t)
	seedTermDense(t, session, "development", "development")

	out := searchScopedWithLimit(t, session, "database configuration", map[string]string{"environment": "production"}, 3)

	if !strings.Contains(out, "No matching memories found") {
		t.Fatalf("precondition: expected no production rows, got:\n%s", out)
	}
	if !strings.Contains(out, "scope filter") {
		t.Errorf("a zero-result scoped search must name the filter that emptied the window, or the "+
			"caller reads absence as a fact about the store:\n%s", out)
	}
}
