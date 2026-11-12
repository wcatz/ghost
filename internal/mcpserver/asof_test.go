package mcpserver

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
)

const (
	asOfToolPast   = "2020-01-01T00:00:00Z"
	asOfToolFuture = "2035-01-01T00:00:00Z"
)

func TestSearchAsOfDisclosesPastReadingAndRefusals(t *testing.T) {
	_, session := newCapSession(t)
	res := callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "test-project", "content": "the retention sweep runs at four in the morning", "category": "fact"})
	if res.IsError {
		t.Fatal(resultText(res))
	}
	out := resultText(callTool(t, session, "ghost_memory_search", map[string]any{"project_id": "test-project", "query": "retention sweep", "as_of": asOfToolFuture}))
	if !strings.Contains(out, "retention sweep runs at four") {
		t.Fatalf("future as_of missing memory: %s", out)
	}
	for _, want := range []string{"as_of " + asOfToolFuture, "historical read", "keyword-only", "no vector leg ran"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q: %s", want, out)
		}
	}
	if strings.Index(out, "as_of "+asOfToolFuture) > strings.Index(out, "- [") {
		t.Fatalf("note after rows: %s", out)
	}
	out = resultText(callTool(t, session, "ghost_memory_search", map[string]any{"project_id": "test-project", "query": "retention sweep", "as_of": asOfToolPast}))
	if strings.Contains(out, "retention sweep runs at four") || !strings.Contains(out, "as_of "+asOfToolPast) {
		t.Fatalf("past as_of wrong: %s", out)
	}
	current := resultText(callTool(t, session, "ghost_memory_search", map[string]any{"project_id": "test-project", "query": "retention sweep"}))
	if !strings.Contains(current, "retention sweep runs at four") || strings.Contains(current, "historical read") {
		t.Fatalf("current search wrong: %s", current)
	}
	for _, bad := range []string{"yesterday", "2020-01-01", "2020-01-01 00:00:00", "2035-01-01"} {
		out = resultText(callTool(t, session, "ghost_memory_search", map[string]any{"project_id": "test-project", "query": "anything", "as_of": bad}))
		if !strings.Contains(out, "RFC 3339") {
			t.Fatalf("bad as_of %q not refused: %s", bad, out)
		}
	}
	out = resultText(callTool(t, session, "ghost_memory_search", map[string]any{"project_id": "test-project", "query": "anything", "as_of": asOfToolFuture, "explain": true}))
	if !strings.Contains(out, "explain cannot describe a historical") {
		t.Fatalf("explain+as_of not refused: %s", out)
	}
	if callTool(t, session, "ghost_memory_search", map[string]any{"project_id": "test-project", "query": "anything", "explain": true}).IsError {
		t.Fatal("explain without as_of refused")
	}
	res = callTool(t, session, "ghost_memory_search", map[string]any{"project_id": "test-project", "query": "retention", "as_of": ""})
	if res.IsError || strings.Contains(resultText(res), "as_of") {
		t.Fatalf("empty as_of not treated as current: %s", resultText(res))
	}
}

func TestProjectContextAsOf(t *testing.T) {
	_, session := newCapSession(t)
	res := callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "test-project", "content": "the retention sweep runs at four in the morning", "category": "fact"})
	if res.IsError {
		t.Fatal(resultText(res))
	}
	out := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "test-project", "as_of": asOfToolFuture}))
	if !strings.Contains(out, "as_of "+asOfToolFuture) || !strings.Contains(out, "retention sweep runs at four") || strings.Contains(out, "Save to Ghost as it happens") || !strings.Contains(out, "are not versioned") || strings.Contains(out, "## Learned Context") || strings.Contains(out, globalSectionHeading) {
		t.Fatalf("historical context wrong: %s", out)
	}
	current := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "test-project"}))
	if strings.Contains(current, "historical read") || strings.Contains(current, "are not versioned") {
		t.Fatalf("current context has historical markers: %s", current)
	}
	out = resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "test-project", "as_of": asOfToolPast}))
	if strings.Contains(out, "retention sweep runs at four") || !strings.Contains(out, "as_of "+asOfToolPast) || strings.Contains(out, "is not registered with Ghost yet") {
		t.Fatalf("past context wrong: %s", out)
	}
}

func TestAsOfParsesToUTC(t *testing.T) {
	got, err := parseAsOf("2026-09-20T11:00:00+02:00")
	if err != nil || got == nil || got.UTC().Format(time.RFC3339) != "2026-09-20T09:00:00Z" {
		t.Fatalf("parseAsOf zone: %v %v", got, err)
	}
	empty, err := parseAsOf("")
	if err != nil || empty != nil {
		t.Fatalf("empty as_of: %v %v", empty, err)
	}
}

func backdatedSession(t *testing.T) (*mcp.ClientSession, *sql.DB) {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := memory.NewStore(db, logger)
	if err := store.EnsureProject(context.Background(), "abc123", "/tmp/test", "test-project"); err != nil {
		t.Fatal(err)
	}
	return connectedClient(t, New(store, logger, "test")), db
}

func insertHistoricalRow(t *testing.T, db *sql.DB, id, content string, from, until *string, at time.Time) {
	t.Helper()
	f := memory.StoredStampLayout
	nullable := func(s *string) any {
		if s == nil {
			return nil
		}
		return *s
	}
	stamp := func(d time.Duration) string { return at.Add(d).Format(f) }
	if _, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, valid_from, valid_until, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, "abc123", "fact", content, "manual", nullable(from), nullable(until), stamp(-72*time.Hour), stamp(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO memory_history (memory_id, project_id, category, content, source, recorded_at, phase) VALUES (?, ?, ?, ?, ?, ?, ?)`, id, "abc123", "fact", content, "manual", stamp(-24*time.Hour), "save"); err != nil {
		t.Fatal(err)
	}
}

func TestSearchAsOfKeepsARowWhoseCurrentWindowIsClosedOrUnopened(t *testing.T) { // #910
	at := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	now := time.Now().UTC()
	session, db := backdatedSession(t)
	f := memory.StoredStampLayout
	closedSince := at.Add(90 * 24 * time.Hour).Format(f)
	opensLater := now.Add(90 * 24 * time.Hour).Format(f)
	insertHistoricalRow(t, db, "m-closed-since", "walrus window closed since T", strPtr(at.Add(-90*24*time.Hour).Format(f)), &closedSince, at)
	insertHistoricalRow(t, db, "m-opens-after-now", "walrus window opens after now", strPtr(opensLater), nil, at)
	historical := resultText(callTool(t, session, "ghost_memory_search", map[string]any{"project_id": "test-project", "query": "walrus", "as_of": at.Format(time.RFC3339)}))
	for _, content := range []string{"walrus window closed since T", "walrus window opens after now"} {
		if !strings.Contains(historical, content) {
			t.Fatalf("missing %q: %s", content, historical)
		}
	}
	if !strings.Contains(historical, memory.AsOfBorrowedWindowNote(at)) || strings.Contains(historical, "Validity judged at") {
		t.Fatalf("historical note wrong: %s", historical)
	}
	current := resultText(callTool(t, session, "ghost_memory_search", map[string]any{"project_id": "test-project", "query": "walrus"}))
	for _, content := range []string{"walrus window closed since T", "walrus window opens after now"} {
		if strings.Contains(current, content) {
			t.Fatalf("current search should withhold %q: %s", content, current)
		}
	}
}
