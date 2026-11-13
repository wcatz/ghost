package mcpserver

import (
	"strings"
	"testing"
)

func TestScopedSearchFiltersAndReportsScope(t *testing.T) {
	_, session := newCapSession(t)
	saveScoped(t, session, "database pool size is 10 in development", "development")
	saveScoped(t, session, "connection pool ceiling for the reporting database sits at fifty rows", "production")
	saveScoped(t, session, "database failover runs in production", "production")
	saveScoped(t, session, "nightly job populates the warehouse with sample rows", "development")

	got := searchScopedWithLimit(t, session, "database", map[string]any{"environment": "production"}, 2)
	if strings.Contains(got, "development") {
		t.Fatalf("a scope=production answer lists a development row:\n%s", got)
	}
	if n := strings.Count(got, "- ["); n != 2 {
		t.Fatalf("scope-filtered answer lists %d rows, want 2:\n%s", n, got)
	}
	if !strings.Contains(got, "[ghost:outcome=") {
		t.Fatalf("answer missing verdict line:\n%s", got)
	}
}

func TestScopeEmptiedAnswerReportsIncompleteness(t *testing.T) {
	_, session := newCapSession(t)
	saveScoped(t, session, "database pool size is 10 in development", "development")

	got := searchScopedWithLimit(t, session, "database", map[string]any{"environment": "production"}, 2)
	if strings.Contains(got, "- [") {
		t.Fatalf("expected no rows, got:\n%s", got)
	}
	if !strings.Contains(got, "incomplete") || !strings.Contains(got, "[ghost:outcome=empty") {
		t.Fatalf("scope-emptied answer missing expected markers:\n%s", got)
	}
}
