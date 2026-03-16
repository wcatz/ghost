package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestMemorySearch_ByQuery(t *testing.T) {
	store := testStoreForTool(t)
	saveExec := makeMemorySaveExec(store)
	searchExec := makeMemorySearchExec(store)
	ctx := context.Background()
	projectPath := "/tmp/test-project"

	// Seed some memories.
	memories := []memorySaveInput{
		{Content: "Kubernetes pods run containers", Category: "fact", Importance: 0.5, Tags: []string{"k8s"}},
		{Content: "Go channels enable concurrency", Category: "pattern", Importance: 0.7, Tags: []string{"go"}},
		{Content: "SQLite is an embedded database engine", Category: "architecture", Importance: 0.6, Tags: []string{"sqlite"}},
	}
	for _, m := range memories {
		raw, _ := json.Marshal(m)
		r := saveExec(ctx, projectPath, raw)
		if r.IsError {
			t.Fatalf("seed memory: %s", r.Content)
		}
	}

	tests := []struct {
		name      string
		input     memorySearchInput
		wantIn    string // expected substring in result
		wantEmpty bool
	}{
		{
			name:   "search by keyword",
			input:  memorySearchInput{Query: "Kubernetes"},
			wantIn: "Kubernetes",
		},
		{
			name:   "search concurrency",
			input:  memorySearchInput{Query: "concurrency"},
			wantIn: "concurrency",
		},
		{
			name:      "no results",
			input:     memorySearchInput{Query: "blockchain"},
			wantEmpty: true,
		},
		{
			name:   "with limit",
			input:  memorySearchInput{Query: "engine database", Limit: 1},
			wantIn: "SQLite",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.input)
			result := searchExec(ctx, projectPath, raw)
			if result.IsError {
				t.Fatalf("unexpected error: %s", result.Content)
			}
			if tc.wantEmpty {
				if result.Content != "no memories found" {
					t.Errorf("expected 'no memories found', got: %s", result.Content)
				}
				return
			}
			if !strings.Contains(result.Content, tc.wantIn) {
				t.Errorf("expected %q in result, got: %s", tc.wantIn, result.Content)
			}
		})
	}
}

func TestMemorySearch_ByCategoryOnly(t *testing.T) {
	store := testStoreForTool(t)
	saveExec := makeMemorySaveExec(store)
	searchExec := makeMemorySearchExec(store)
	ctx := context.Background()
	projectPath := "/tmp/test-project"

	// Seed memories in different categories.
	raw1, _ := json.Marshal(memorySaveInput{Content: "Category test architecture memory", Category: "architecture", Importance: 0.5})
	saveExec(ctx, projectPath, raw1)
	raw2, _ := json.Marshal(memorySaveInput{Content: "Category test pattern memory", Category: "pattern", Importance: 0.5})
	saveExec(ctx, projectPath, raw2)

	// Search by category only (no query).
	input, _ := json.Marshal(memorySearchInput{Category: "architecture"})
	result := searchExec(ctx, projectPath, input)
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content)
	}
	if !strings.Contains(result.Content, "architecture") {
		t.Errorf("expected architecture result, got: %s", result.Content)
	}
	if strings.Contains(result.Content, "pattern") {
		t.Errorf("should not contain pattern results: %s", result.Content)
	}
}

func TestMemorySearch_ByCategoryAndQuery(t *testing.T) {
	store := testStoreForTool(t)
	saveExec := makeMemorySaveExec(store)
	searchExec := makeMemorySearchExec(store)
	ctx := context.Background()
	projectPath := "/tmp/test-project"

	// Seed memories.
	raw1, _ := json.Marshal(memorySaveInput{Content: "Architecture uses microservices pattern", Category: "architecture", Importance: 0.6})
	saveExec(ctx, projectPath, raw1)
	raw2, _ := json.Marshal(memorySaveInput{Content: "Pattern for retry logic", Category: "pattern", Importance: 0.7})
	saveExec(ctx, projectPath, raw2)

	// Search with category + query filter.
	input, _ := json.Marshal(memorySearchInput{Query: "microservices", Category: "architecture"})
	result := searchExec(ctx, projectPath, input)
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content)
	}
	if !strings.Contains(result.Content, "microservices") {
		t.Errorf("expected microservices in result, got: %s", result.Content)
	}
}

func TestMemorySearch_InvalidJSON(t *testing.T) {
	store := testStoreForTool(t)
	searchExec := makeMemorySearchExec(store)
	ctx := context.Background()

	result := searchExec(ctx, "/tmp/test-project", json.RawMessage(`not json`))
	if !result.IsError {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(result.Content, "invalid input") {
		t.Errorf("expected 'invalid input', got: %s", result.Content)
	}
}

func TestMemorySearch_DefaultLimit(t *testing.T) {
	store := testStoreForTool(t)
	saveExec := makeMemorySaveExec(store)
	searchExec := makeMemorySearchExec(store)
	ctx := context.Background()
	projectPath := "/tmp/test-project"

	// Seed a memory.
	raw, _ := json.Marshal(memorySaveInput{Content: "Default limit test memory about databases", Category: "fact", Importance: 0.5})
	saveExec(ctx, projectPath, raw)

	// Search with no explicit limit (should default to 10).
	input, _ := json.Marshal(memorySearchInput{Query: "databases"})
	result := searchExec(ctx, projectPath, input)
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content)
	}
	if !strings.Contains(result.Content, "databases") {
		t.Errorf("expected 'databases' in result, got: %s", result.Content)
	}
}

func TestRegistry_Basic(t *testing.T) {
	r := NewRegistry()
	store := testStoreForTool(t)
	RegisterAll(r, store)

	defs := r.Definitions()
	if len(defs) < 2 {
		t.Fatalf("expected at least 2 tools, got %d", len(defs))
	}

	// Check memory_save and memory_search are registered.
	names := make(map[string]bool)
	for _, d := range defs {
		names[d.Name] = true
	}
	if !names["memory_save"] {
		t.Error("memory_save not registered")
	}
	if !names["memory_search"] {
		t.Error("memory_search not registered")
	}
}

func TestRegistry_ExecuteUnknownTool(t *testing.T) {
	r := NewRegistry()
	ctx := context.Background()

	result := r.Execute(ctx, "nonexistent_tool", "/tmp", nil)
	if !result.IsError {
		t.Fatal("expected error for unknown tool")
	}
	if !strings.Contains(result.Content, "unknown tool") {
		t.Errorf("expected 'unknown tool', got: %s", result.Content)
	}
}

func TestRegistry_GetApprovalLevel(t *testing.T) {
	r := NewRegistry()
	store := testStoreForTool(t)
	RegisterAll(r, store)

	tests := []struct {
		name     string
		toolName string
		want     ApprovalLevel
	}{
		{"memory_save is none", "memory_save", ApprovalNone},
		{"memory_search is none", "memory_search", ApprovalNone},
		{"unknown defaults to require", "unknown_tool", ApprovalRequire},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := r.GetApprovalLevel(tc.toolName)
			if got != tc.want {
				t.Errorf("GetApprovalLevel(%q) = %d, want %d", tc.toolName, got, tc.want)
			}
		})
	}
}
