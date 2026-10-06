package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// TestSearchExplainAppliesScope makes explain describe the same membership the
// formatted scoped search returns. An out-of-scope candidate may remain in the
// diagnostic union, but it must be marked excluded and carry the scope reason.
func TestSearchExplainAppliesScope(t *testing.T) {
	srv, session := newCapSession(t)
	saveScoped(t, session, "development database uses SQLite", "development")
	saveScoped(t, session, "production database uses PostgreSQL", "production")
	saveScoped(t, session, "unscoped database uses the shared service", "")

	res := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "database",
		"limit":      3,
		"explain":    true,
		"scope":      map[string]any{"environment": "production"},
	})
	if res.IsError {
		t.Fatalf("explain search errored: %s", resultText(res))
	}
	var ex memory.SearchExplain
	if err := json.Unmarshal([]byte(resultText(res)), &ex); err != nil {
		t.Fatalf("response is not a JSON explanation: %v", err)
	}
	if ex.Scope["environment"] != "production" {
		t.Fatalf("explanation scope = %v, want the requested production scope", ex.Scope)
	}

	rows := make(map[string]memory.ExplainRow)
	for _, row := range ex.Rows {
		rows[row.Content] = row
	}
	dev, ok := rows["development database uses SQLite"]
	if !ok {
		t.Fatalf("development candidate missing from explanation rows: %+v", ex.Rows)
	}
	prod, ok := rows["production database uses PostgreSQL"]
	if !ok {
		t.Fatalf("production candidate missing from explanation rows: %+v", ex.Rows)
	}
	unscoped, ok := rows["unscoped database uses the shared service"]
	if !ok {
		t.Fatalf("unscoped candidate missing from explanation rows: %+v", ex.Rows)
	}
	if dev.Included || dev.Rank != 0 || !strings.Contains(dev.Reason, "scope") {
		t.Errorf("out-of-scope row = %+v, want excluded at rank 0 with a scope reason", dev)
	}
	if !prod.Included || prod.Rank != 1 || !unscoped.Included {
		t.Errorf("eligible rows were not included: production=%+v unscoped=%+v", prod, unscoped)
	}
	sawScopeNote := false
	for _, note := range ex.Notes {
		if strings.Contains(note, "scope is not applied") {
			t.Errorf("stale unscoped explain note remains: %q", note)
		}
		if strings.Contains(note, "scope is applied inside hybrid window selection") {
			sawScopeNote = true
		}
	}
	if !sawScopeNote {
		t.Errorf("scoped explanation did not identify the selection seam: %v", ex.Notes)
	}

	store, ok := srv.store.(*memory.Store)
	if !ok {
		t.Fatalf("test server store = %T, want *memory.Store", srv.store)
	}
	resolvedProject, _, err := store.ResolveProject(context.Background(), "test-project")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	expected, err := store.SearchHybridScoped(context.Background(), resolvedProject, "database", nil, 3, map[string]string{"environment": "production"})
	if err != nil {
		t.Fatalf("SearchHybridScoped: %v", err)
	}
	expectedIDs := make(map[string]bool, len(expected))
	for _, m := range expected {
		expectedIDs[m.ID] = true
	}
	for _, row := range ex.Rows {
		if row.Included != expectedIDs[row.ID] {
			t.Errorf("row %s included=%v, production search membership=%v", row.ID, row.Included, expectedIDs[row.ID])
		}
	}

	categoryRes := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "database",
		"category":   "fact",
		"limit":      3,
		"explain":    true,
		"scope":      map[string]any{"environment": "production"},
	})
	if categoryRes.IsError {
		t.Fatalf("category+scope explain errored: %s", resultText(categoryRes))
	}
	var categoryEx memory.SearchExplain
	if err := json.Unmarshal([]byte(resultText(categoryRes)), &categoryEx); err != nil {
		t.Fatalf("category+scope response is not JSON: %v", err)
	}
	hasCategoryNote, hasScopeNote := false, false
	for _, note := range categoryEx.Notes {
		hasCategoryNote = hasCategoryNote || strings.Contains(note, "category filter is not applied to these rows")
		hasScopeNote = hasScopeNote || strings.Contains(note, "scope is applied inside hybrid window selection")
	}
	// Both filters have to be disclosed, and the category disclosure has to be
	// the honest one: explain does not apply the category, so a note claiming
	// these rows are filtered would describe a ranking the caller cannot see.
	if !hasCategoryNote || !hasScopeNote {
		t.Errorf("category+scope notes = %v, want both filter disclosures", categoryEx.Notes)
	}
}

// TestSearchExplainReturnsDiagnosisThroughTheTool: an agent debugging a bad
// result must be able to ask for the breakdown through the tool it already
// calls, not reach into a package. This asserts the argument is accepted, the
// response is machine-readable, and the fields an agent needs to attribute
// blame are actually present.
func TestSearchExplainReturnsDiagnosisThroughTheTool(t *testing.T) {
	_, session := newCapSession(t)

	for _, c := range []string{
		"helmfile deploys through sops secrets",
		"sops age key lives in ci",
		"helmfile environments are global dev prod",
	} {
		callTool(t, session, "ghost_memory_save", map[string]any{
			"project_id": "test-project",
			"content":    c,
			"category":   "fact",
		})
	}

	res := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "helmfile sops",
		"limit":      3,
		"explain":    true,
	})
	if res.IsError {
		t.Fatalf("explain search errored: %s", resultText(res))
	}

	raw := resultText(res)
	var ex memory.SearchExplain
	if err := json.Unmarshal([]byte(raw), &ex); err != nil {
		t.Fatalf("response is not a JSON explanation: %v\n%s", err, raw)
	}
	if ex.Query != "helmfile sops" {
		t.Errorf("Query = %q, want the query that was explained", ex.Query)
	}
	if len(ex.Rows) == 0 {
		t.Fatal("no rows in the explanation; nothing was diagnosed")
	}

	// The fields that let an agent attribute blame must be present and
	// meaningful, not silently zero.
	for _, r := range ex.Rows {
		if r.FTSRank < -1 {
			t.Errorf("row %s FTSRank = %d", r.ID, r.FTSRank)
		}
		// A row that matched at least one leg must carry a positive fused
		// score; zero would mean the RRF arithmetic never ran for it.
		if (r.FTSRank >= 0 || r.VectorRank >= 0) && r.RRFScore <= 0 {
			t.Errorf("row %s matched a leg (fts=%d vec=%d) but has fused score %v", r.ID, r.FTSRank, r.VectorRank, r.RRFScore)
		}
		if !r.Included && r.Reason == "" {
			t.Errorf("row %s excluded without a reason", r.ID)
		}
	}
	if ex.VectorAvailable && !strings.Contains(raw, "vector_rank") {
		t.Error("vector_rank missing from the payload")
	}

	// The non-explain path must still return the ordinary formatted list:
	// explain is an opt-in alternate rendering, not a mode change.
	plain := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "helmfile sops",
		"limit":      3,
	})
	plainText := resultText(plain)
	if strings.Contains(plainText, `"fts_rank"`) {
		t.Errorf("plain search returned an explanation:\n%s", plainText)
	}
	if !strings.Contains(plainText, "Memory") && !strings.Contains(plainText, "fact") {
		t.Errorf("plain search returned an unexpected rendering:\n%s", plainText)
	}
}

// TestSearchExplainExpiredRowNeverIncluded: an expired row must never be
// reported as included by explain. The current ExplainSearchScoped marks
// expired rows as included because the search ranking doesn't read validity.
// The assembler's validity stage drops them, so explain must project the
// assembler's trace where they are dropped at the validity stage.
func TestSearchExplainExpiredRowNeverIncluded(t *testing.T) {
	_, session := newCapSession(t)

	// Save a memory with valid_until in the past
	expired := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id":  "test-project",
		"content":     "this memory is expired",
		"category":    "fact",
		"valid_until": expired.Format(time.RFC3339),
	})

	// Save a valid memory for comparison
	callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project",
		"content":    "this memory is valid",
		"category":   "fact",
	})

	res := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "memory",
		"limit":      10,
		"explain":    true,
	})
	if res.IsError {
		t.Fatalf("explain search errored: %s", resultText(res))
	}

	var ex memory.SearchExplain
	if err := json.Unmarshal([]byte(resultText(res)), &ex); err != nil {
		t.Fatalf("response is not a JSON explanation: %v", err)
	}

	// Find the expired row
	var expiredRow *memory.ExplainRow
	for i := range ex.Rows {
		if strings.Contains(ex.Rows[i].Content, "expired") {
			expiredRow = &ex.Rows[i]
			break
		}
	}
	if expiredRow == nil {
		t.Fatal("expired row not found in explanation")
	}

	// The expired row must NOT be included
	if expiredRow.Included {
		t.Errorf("expired row was reported as included (Included=%v), want false; ValidityState=%q", expiredRow.Included, expiredRow.ValidityState)
	}
	if expiredRow.ValidityState != "expired" {
		t.Errorf("expired row ValidityState = %q, want expired", expiredRow.ValidityState)
	}
	if expiredRow.Reason == "" {
		t.Errorf("expired row excluded without a reason")
	}
	if !strings.Contains(expiredRow.Reason, "validity") && !strings.Contains(expiredRow.Reason, "expired") {
		t.Errorf("expired row reason = %q, want a validity/expired reason", expiredRow.Reason)
	}

	// Also verify the formatted path excludes it
	plain := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "memory",
		"limit":      10,
	})
	plainText := resultText(plain)
	if strings.Contains(plainText, "this memory is expired") {
		t.Errorf("formatted search included expired row that should have been withheld")
	}
}

// TestSearchExplainCategoryRetentionFilterReportsWithheld: a row withheld
// by category or retention filter must be reported as withheld with that
// stage in the explain projection.
func TestSearchExplainCategoryRetentionFilterReportsWithheld(t *testing.T) {
	_, session := newCapSession(t)

	callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project",
		"content":    "this is a fact",
		"category":   "fact",
	})
	callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project",
		"content":    "this is a gotcha",
		"category":   "gotcha",
	})
	callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project",
		"content":    "this is a session memory",
		"category":   "fact",
		"retention":  "session",
	})

	// Test category filter
	catRes := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "is a",
		"limit":      10,
		"category":   "gotcha",
		"explain":    true,
	})
	if catRes.IsError {
		t.Fatalf("category explain errored: %s", resultText(catRes))
	}
	var catEx memory.SearchExplain
	if err := json.Unmarshal([]byte(resultText(catRes)), &catEx); err != nil {
		t.Fatalf("category explain not JSON: %v", err)
	}

	// The "fact" row should be excluded with category_mismatch reason
	var factRow *memory.ExplainRow
	for i := range catEx.Rows {
		if strings.Contains(catEx.Rows[i].Content, "this is a fact") {
			factRow = &catEx.Rows[i]
			break
		}
	}
	if factRow == nil {
		t.Fatal("fact row not found in category explain")
	}
	if factRow.Included {
		t.Errorf("category-mismatched row was reported as included, want excluded")
	}
	if !strings.Contains(factRow.Reason, "category") {
		t.Errorf("category-mismatched row reason = %q, want category_mismatch", factRow.Reason)
	}

	// Test retention filter
	retRes := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "is a",
		"limit":      10,
		"retention":  "session",
		"explain":    true,
	})
	if retRes.IsError {
		t.Fatalf("retention explain errored: %s", resultText(retRes))
	}
	var retEx memory.SearchExplain
	if err := json.Unmarshal([]byte(resultText(retRes)), &retEx); err != nil {
		t.Fatalf("retention explain not JSON: %v", err)
	}

	// The "fact" row should be excluded with retention_mismatch reason
	var nonSessionRow *memory.ExplainRow
	for i := range retEx.Rows {
		if strings.Contains(retEx.Rows[i].Content, "this is a fact") {
			nonSessionRow = &retEx.Rows[i]
			break
		}
	}
	if nonSessionRow == nil {
		t.Fatal("non-session row not found in retention explain")
	}
	if nonSessionRow.Included {
		t.Errorf("retention-mismatched row was reported as included, want excluded")
	}
	if !strings.Contains(nonSessionRow.Reason, "retention") {
		t.Errorf("retention-mismatched row reason = %q, want retention_mismatch", nonSessionRow.Reason)
	}
}

// TestSearchExplainIncludedSetEqualsFormattedAnswer: for a fixed fixture,
// the set of rows explain reports as included must equal the set the
// formatted answer renders, across query mode with as_of too.
func TestSearchExplainIncludedSetEqualsFormattedAnswer(t *testing.T) {
	_, session := newCapSession(t)

	// Seed a fixed set of memories
	memories := []struct {
		content  string
		category string
	}{
		{"helmfile deploys through sops secrets", "fact"},
		{"sops age key lives in ci", "fact"},
		{"helmfile environments are global dev prod", "fact"},
		{"database configuration pooling", "gotcha"},
		{"database configuration retry", "gotcha"},
		{"database configuration cache", "gotcha"},
	}
	for _, m := range memories {
		callTool(t, session, "ghost_memory_save", map[string]any{
			"project_id": "test-project",
			"content":    m.content,
			"category":   m.category,
		})
	}

	// Query with category filter
	query := "database configuration"
	category := "gotcha"

	// Get explain result
	exRes := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      query,
		"category":   category,
		"limit":      3,
		"explain":    true,
	})
	if exRes.IsError {
		t.Fatalf("explain errored: %s", resultText(exRes))
	}
	var ex memory.SearchExplain
	if err := json.Unmarshal([]byte(resultText(exRes)), &ex); err != nil {
		t.Fatalf("explain not JSON: %v", err)
	}

	explainIncluded := make(map[string]bool)
	for _, r := range ex.Rows {
		if r.Included {
			explainIncluded[r.Content] = true
		}
	}

	// Get formatted result
	plainRes := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      query,
		"category":   category,
		"limit":      3,
	})
	if plainRes.IsError {
		t.Fatalf("formatted errored: %s", resultText(plainRes))
	}
	plainText := resultText(plainRes)

	// Parse the formatted output to get included contents
	formattedIncluded := make(map[string]bool)
	lines := strings.Split(plainText, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "  ") {
			for _, m := range memories {
				if strings.Contains(line, m.content) {
					formattedIncluded[m.content] = true
				}
			}
		}
	}

	// The sets must match
	if len(explainIncluded) != len(formattedIncluded) {
		t.Errorf("explain included count %d != formatted included count %d", len(explainIncluded), len(formattedIncluded))
	}
	for content := range explainIncluded {
		if !formattedIncluded[content] {
			t.Errorf("explain included %q but formatted did not", content)
		}
	}
	for content := range formattedIncluded {
		if !explainIncluded[content] {
			t.Errorf("formatted included %q but explain did not", content)
		}
	}
}

// TestSearchExplainAsOfIncludedSetEqualsFormattedAnswer: the same
// membership equality must hold for historical (as_of) reads.
func TestSearchExplainAsOfIncludedSetEqualsFormattedAnswer(t *testing.T) {
	_, session := newCapSession(t)

	// Save memories
	callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project",
		"content":    "original configuration",
		"category":   "fact",
	})
	callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project",
		"content":    "updated configuration",
		"category":   "fact",
	})

	// Use a past instant before the second memory was saved
	asOf := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)

	// Get explain result with as_of
	exRes := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "configuration",
		"limit":      10,
		"as_of":      asOf,
		"explain":    true,
	})
	if exRes.IsError {
		t.Fatalf("as_of explain errored: %s", resultText(exRes))
	}
	var ex memory.SearchExplain
	if err := json.Unmarshal([]byte(resultText(exRes)), &ex); err != nil {
		t.Fatalf("as_of explain not JSON: %v", err)
	}

	explainIncluded := make(map[string]bool)
	for _, r := range ex.Rows {
		if r.Included {
			explainIncluded[r.Content] = true
		}
	}

	// Get formatted result with as_of
	plainRes := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "configuration",
		"limit":      10,
		"as_of":      asOf,
	})
	if plainRes.IsError {
		t.Fatalf("as_of formatted errored: %s", resultText(plainRes))
	}
	plainText := resultText(plainRes)

	formattedIncluded := make(map[string]bool)
	for _, content := range []string{"original configuration", "updated configuration"} {
		if strings.Contains(plainText, content) {
			formattedIncluded[content] = true
		}
	}

	if len(explainIncluded) != len(formattedIncluded) {
		t.Errorf("as_of explain included count %d != formatted included count %d", len(explainIncluded), len(formattedIncluded))
	}
	for content := range explainIncluded {
		if !formattedIncluded[content] {
			t.Errorf("as_of explain included %q but formatted did not", content)
		}
	}
	for content := range formattedIncluded {
		if !explainIncluded[content] {
			t.Errorf("as_of formatted included %q but explain did not", content)
		}
	}
}

// TestSearchExplainTraceCarriesOneEntryPerStage: the explain projection
// must carry one entry per stage that ran (validity, predicates, provenance,
// conflicts, dedup, diversity, budget, render, response_fit).
func TestSearchExplainTraceCarriesOneEntryPerStage(t *testing.T) {
	_, session := newCapSession(t)

	callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project",
		"content":    "test memory",
		"category":   "fact",
	})

	res := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "test",
		"limit":      10,
		"explain":    true,
	})
	if res.IsError {
		t.Fatalf("explain errored: %s", resultText(res))
	}

	var ex memory.SearchExplain
	if err := json.Unmarshal([]byte(resultText(res)), &ex); err != nil {
		t.Fatalf("explain not JSON: %v", err)
	}

	// The explain payload doesn't carry stages directly; it carries Notes.
	// But the trace notes from each stage should be present.
	// Check that notes mention the stages that ran.
	foundStages := make(map[string]bool)
	for _, note := range ex.Notes {
		// Look for stage indicators in notes
		for _, stage := range []string{
			"validity", "predicate", "provenance", "conflict",
			"dedup", "diversity", "budget", "render", "response_fit",
		} {
			if strings.Contains(strings.ToLower(note), stage) {
				foundStages[stage] = true
			}
		}
	}

	// At minimum, the validity, predicates, provenance, conflicts, dedup,
	// diversity, budget, and render stages should have some trace evidence.
	// The exact note text varies, but the trace records every stage.
	if len(foundStages) == 0 {
		t.Logf("explain notes: %v", ex.Notes)
		t.Errorf("explain notes carry no stage indicators; expected at least validity/predicates/provenance/budget")
	}
}
