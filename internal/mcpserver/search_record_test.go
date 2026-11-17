package mcpserver

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// recordingSearchStore is the test store with the one interface the recording
// helper asks for made explicit: a real *memory.Store behind the candidate-set
// double so a recorded row can be read back.
type recordingSearchStore struct {
	searchStore
	real *memory.Store
}

func (s recordingSearchStore) RecordSearch(ctx context.Context, rec memory.SearchRecord) error {
	return s.real.RecordSearch(ctx, rec)
}

func newRecordingSearchSession(t *testing.T, set *memory.CandidateSet) (*Server, *recordingSearchStore) {
	t.Helper()
	real := testStore(t)
	store := recordingSearchStore{
		searchStore: searchStore{MemoryStore: real, set: set},
		real:        real,
	}
	srv := New(store, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	return srv, &store
}

func recordedSearches(t *testing.T, s *recordingSearchStore) []memory.SearchRecord {
	t.Helper()
	rows, err := s.real.SearchRecords(context.Background(), 100)
	if err != nil {
		t.Fatalf("SearchRecords: %v", err)
	}
	return rows
}

func TestMemorySearchRecordsTheQueryAndItsRankedResults(t *testing.T) {
	srv, store := newRecordingSearchSession(t, limitProbeRows(3))
	session := connectedClient(t, srv)
	res := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "probe",
		"scope":      map[string]any{"environment": "production"},
		"category":   "fact",
		"limit":      3,
	})
	if res.IsError {
		t.Fatalf("search failed: %s", resultText(res))
	}
	rows := recordedSearches(t, store)
	if len(rows) != 1 {
		t.Fatalf("recorded %d searches, want 1", len(rows))
	}
	got := rows[0]
	if got.Query != "probe" || got.ProjectID != "abc123" || got.Source != "search" {
		t.Errorf("record = %+v, want the query, project and search source", got)
	}
	if got.Scope["environment"] != "production" || got.Category != "fact" {
		t.Errorf("filters = %+v, want the scope/category the caller sent", got)
	}
	if strings.Join(got.Results, ",") != "probe00,probe01,probe02" {
		t.Errorf("results = %v, want the three ranked ids in order", got.Results)
	}
}

func TestSearchAllRecordsTheWholeCorpusQuery(t *testing.T) {
	srv, store := newRecordingSearchSession(t, limitProbeRows(1))
	ctx := context.Background()
	id, _, _, err := store.real.Upsert(ctx, "abc123", "gotcha", "a proxy timeout gotcha", "mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	session := connectedClient(t, srv)
	res := callTool(t, session, "ghost_search_all", map[string]any{"query": "proxy timeout", "limit": 5})
	if res.IsError {
		t.Fatalf("search_all failed: %s", resultText(res))
	}
	rows := recordedSearches(t, store)
	if len(rows) != 1 {
		t.Fatalf("recorded %d searches, want 1", len(rows))
	}
	got := rows[0]
	if got.Query != "proxy timeout" || got.Source != "search_all" || got.ProjectID != "_global" {
		t.Errorf("record = %+v, want the whole-corpus query under search_all", got)
	}
	if len(got.Results) != 1 || got.Results[0] != id {
		t.Errorf("results = %v, want [%s]", got.Results, id)
	}
}

func TestMemorySearchRecordsAsOfAndTheRequestedLimit(t *testing.T) {
	srv, store := newRecordingSearchSession(t, limitProbeRows(3))
	session := connectedClient(t, srv)
	res := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "probe",
		"as_of":      "2026-09-01T13:00:00Z",
		"limit":      2,
	})
	if res.IsError {
		t.Fatalf("search failed: %s", resultText(res))
	}
	rows := recordedSearches(t, store)
	if len(rows) != 1 {
		t.Fatalf("recorded %d searches, want 1", len(rows))
	}
	if rows[0].AsOf != "2026-09-01T13:00:00Z" {
		t.Errorf("recorded as_of = %q, want the instant the caller asked about", rows[0].AsOf)
	}
	if rows[0].Limit != 2 {
		t.Errorf("recorded limit = %d, want 2", rows[0].Limit)
	}
}

func TestMemorySearchRecordingIsOptOut(t *testing.T) {
	srv, store := newRecordingSearchSession(t, limitProbeRows(2))
	srv.SetSearchRecording(false)
	session := connectedClient(t, srv)
	if res := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project", "query": "anything",
	}); res.IsError {
		t.Fatalf("search failed: %s", resultText(res))
	}
	if rows := recordedSearches(t, store); len(rows) != 0 {
		t.Errorf("recorded %d searches with recording disabled, want 0", len(rows))
	}
}

func TestMemorySearchDropsATokenShapedQueryWithoutFailingTheSearch(t *testing.T) {
	srv, store := newRecordingSearchSession(t, limitProbeRows(2))
	session := connectedClient(t, srv)
	query := "the key is ghp_" + strings.Repeat("a", 36)
	res := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project", "query": query,
	})
	if res.IsError {
		t.Fatalf("search failed: %s", resultText(res))
	}
	if rows := recordedSearches(t, store); len(rows) != 0 {
		t.Errorf("recorded %d searches with a token-shaped query, want 0", len(rows))
	}
}
