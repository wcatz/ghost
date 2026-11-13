package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/provider"
)

func searchWithCategory(t *testing.T, session *mcp.ClientSession, query, category string, limit int) string {
	t.Helper()
	res := callTool(t, session, "ghost_memory_search", map[string]any{"project_id": "test-project", "query": query, "category": category, "limit": limit})
	if res.IsError {
		t.Fatalf("search failed: %s", resultText(res))
	}
	return resultText(res)
}

type failingCandidateStore struct {
	provider.MemoryStore
	err error
}

func (f failingCandidateStore) Candidates(context.Context, memory.CandidateRequest) (*memory.CandidateSet, error) {
	return nil, f.err
}

type partialFailureStore struct{ provider.MemoryStore }

func (partialFailureStore) Candidates(context.Context, memory.CandidateRequest) (*memory.CandidateSet, error) {
	return &memory.CandidateSet{Legs: map[string]memory.LegStatus{
		"fts":    {Applicable: true, Attempted: true, Available: false, Err: "search memories: no such table: memories_fts"},
		"vector": {Applicable: true, Attempted: true, Available: true},
	}}, nil
}

type degradedStore struct{ provider.MemoryStore }

func (degradedStore) Candidates(context.Context, memory.CandidateRequest) (*memory.CandidateSet, error) {
	return &memory.CandidateSet{Legs: map[string]memory.LegStatus{
		"fts":    {Applicable: true, Attempted: true, Available: false, Err: "search memories: no such table: memories_fts"},
		"vector": {Applicable: true, Attempted: true, Available: true, Eligible: 1},
	}, Rows: []memory.Candidate{{Memory: memory.Memory{ID: "VEC1", ProjectID: "test-project", Category: "fact", Content: "vector-only match that survived the keyword leg failure", CreatedAt: "2026-09-01 12:00:00", Source: "mcp"}, FTSRank: -1, VectorRank: 0, VectorScore: 0.9, Base: 0.9, Decay: 1, Score: 0.9}}}, nil
}

type invalidStore struct{ provider.MemoryStore }

func (invalidStore) Candidates(context.Context, memory.CandidateRequest) (*memory.CandidateSet, error) {
	past := "2020-01-01 00:00:00"
	return &memory.CandidateSet{Legs: map[string]memory.LegStatus{"fts": {Applicable: true, Attempted: true, Available: true, CoverageComplete: false}}, Rows: []memory.Candidate{{Memory: memory.Memory{ID: "OLD1", ProjectID: "test-project", Category: "fact", Content: "retention policy that has since expired", CreatedAt: "2026-09-01 12:00:00", Source: "mcp", ValidUntil: &past}, Base: 0.9, Decay: 1, Score: 0.9}}}, nil
}

func TestCategorySearchAndPlainSearch(t *testing.T) { // #573 #596

	_, session := newCapSession(t)
	for i := range 20 {
		res := callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "test-project", "content": "database configuration pooling timeout " + string(rune('a'+i)), "category": "gotcha"})
		if res.IsError {
			t.Fatal(resultText(res))
		}
	}
	res := callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "test-project", "content": "database configuration replication lag and failover promotion quorum", "category": "architecture"})
	if res.IsError {
		t.Fatal(resultText(res))
	}
	out := searchWithCategory(t, session, "database configuration", "architecture", 5)
	if strings.Contains(out, "No matching memories found") || !strings.Contains(out, "quorum") || strings.Contains(out, "pooling timeout") {
		t.Fatalf("category search beyond window failed: %s", out)
	}

	_, session2 := newCapSession(t)
	for _, content := range []string{"database configuration pooling timeout retry backoff alpha", "database configuration charset collation vacuum beta"} {
		callTool(t, session2, "ghost_memory_save", map[string]any{"project_id": "test-project", "content": content, "category": "architecture"})
	}
	for i := range 5 {
		callTool(t, session2, "ghost_memory_save", map[string]any{"project_id": "test-project", "content": "database configuration filler row " + string(rune('a'+i)), "category": "gotcha"})
	}
	out = searchWithCategory(t, session2, "database configuration", "architecture", 2)
	for _, want := range []string{"pooling timeout retry backoff alpha", "charset collation vacuum beta"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %s", want, out)
		}
	}

	_, session3 := newCapSession(t)
	for _, content := range []string{"database configuration pooling timeout retry", "database configuration charset collation vacuum"} {
		callTool(t, session3, "ghost_memory_save", map[string]any{"project_id": "test-project", "content": content, "category": "gotcha", "importance": 0.8, "tags": []string{"db"}})
	}
	out = searchScopedWithLimit(t, session3, "database configuration", nil, 2)
	if strings.Contains(out, "No matching memories found") || !strings.Contains(out, "- [gotcha] `") || !strings.Contains(out, "(0.8") || !strings.Contains(out, `tags:["db"]`) || !strings.Contains(out, "«database configuration pooling timeout retry»") || !strings.Contains(out, "[ghost:outcome=answerable") || strings.Contains(out, "do not rely") {
		t.Fatalf("plain search changed: %s", out)
	}
}

func TestRetrievalFailuresAndWithheldRows(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Run("complete failure", func(t *testing.T) {
		srv := New(failingCandidateStore{MemoryStore: testStore(t), err: errors.New("candidates: every applicable retrieval leg failed (fts leg: no such table: memories_fts)")}, logger, "test")
		out := resultText(callTool(t, connectedClient(t, srv), "ghost_memory_search", map[string]any{"project_id": "test-project", "query": "database configuration"}))
		if !strings.Contains(out, "incomplete") || strings.Contains(out, "No matching memories found") || !strings.Contains(out, "memories_fts") {
			t.Fatalf("unexpected: %s", out)
		}
	})
	t.Run("partial failure empty", func(t *testing.T) {
		srv := New(partialFailureStore{MemoryStore: testStore(t)}, logger, "test")
		out := resultText(callTool(t, connectedClient(t, srv), "ghost_memory_search", map[string]any{"project_id": "test-project", "query": "database configuration", "category": "fact"}))
		if !strings.Contains(out, "memories_fts") || strings.Contains(out, "No matching memories found") || strings.Contains(out, "category filter") {
			t.Fatalf("unexpected: %s", out)
		}
	})
	t.Run("partial failure with rows", func(t *testing.T) {
		srv := New(degradedStore{MemoryStore: testStore(t)}, logger, "test")
		res := callTool(t, connectedClient(t, srv), "ghost_memory_search", map[string]any{"project_id": "test-project", "query": "vector-only match"})
		out := resultText(res)
		if res.IsError || !strings.Contains(out, "vector-only match that survived") || !strings.Contains(out, "memories_fts") || strings.Contains(out, "No matching memories found") {
			t.Fatalf("unexpected: err=%v %s", res.IsError, out)
		}
	})
	t.Run("all invalid withheld", func(t *testing.T) {
		srv := New(invalidStore{MemoryStore: testStore(t)}, logger, "test")
		res := callTool(t, connectedClient(t, srv), "ghost_memory_search", map[string]any{"project_id": "test-project", "query": "retention policy"})
		out := resultText(res)
		if res.IsError || !strings.Contains(out, "out of date") || strings.Contains(out, "No matching memories found") {
			t.Fatalf("unexpected: err=%v %s", res.IsError, out)
		}
	})
}

func TestExplainUsesTheFormattedPathsWindow(t *testing.T) {
	_, session := newCapSession(t)
	for _, content := range []string{"database configuration pooling timeout alpha", "database configuration charset collation beta", "database configuration vacuum analyze gamma"} {
		callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "test-project", "content": content, "category": "gotcha"})
	}
	for _, tc := range []struct {
		limit, want int
	}{
		{2, 6}, {100, 100},
	} {
		res := callTool(t, session, "ghost_memory_search", map[string]any{"project_id": "test-project", "query": "database configuration", "category": "gotcha", "limit": tc.limit, "explain": true})
		var ex memory.SearchExplain
		if err := json.Unmarshal([]byte(resultText(res)), &ex); err != nil || ex.Limit != tc.want {
			t.Fatalf("limit=%d got=%d err=%v", tc.limit, ex.Limit, err)
		}
	}
}
