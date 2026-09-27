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
	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/provider"
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

// failingCandidateStore is a store whose retrieval cannot run, so the tool's
// rendering of a failed search can be asserted without breaking a database.
type failingCandidateStore struct {
	provider.MemoryStore
	err error
}

func (f failingCandidateStore) Candidates(context.Context, memory.CandidateRequest) (*memory.CandidateSet, error) {
	return nil, f.err
}

// TestRetrievalFailureIsNotAnAbsence: when retrieval itself fails, the tool has
// to say the search was incomplete, not that nothing matched. Those are the two
// answers an agent acts on oppositely — one retries, the other concludes there
// is no such memory and moves on.
func TestRetrievalFailureIsNotAnAbsence(t *testing.T) {
	store := testStore(t)
	boom := errors.New("candidates: every applicable retrieval leg failed (fts leg: no such table: memories_fts)")
	srv := New(failingCandidateStore{MemoryStore: store, err: boom},
		slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	session := connectedClient(t, srv)

	res := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "database configuration",
	})

	if !res.IsError {
		t.Fatalf("a failed retrieval returned a successful result: %s", resultText(res))
	}
	out := resultText(res)
	if strings.Contains(out, "No matching memories found") {
		t.Errorf("a failed retrieval reported absence: %s", out)
	}
	if !strings.Contains(out, "incomplete") {
		t.Errorf("error text does not tell the caller the search was incomplete rather than empty: %s", out)
	}
	if !strings.Contains(out, "memories_fts") {
		t.Errorf("error text drops the underlying cause: %s", out)
	}
}

// TestExplainUsesTheFormattedPathsWindow: explain reports the ranking of a
// window, so the window it explains has to be the one the formatted path
// searches. A category filter widens that window, and explain used to be given
// the widened width before this branch; narrowing it there made the diagnosis
// describe a window the tool no longer uses, which is the one thing an
// explanation cannot do.
func TestExplainUsesTheFormattedPathsWindow(t *testing.T) {
	_, session := newCapSession(t)
	for i, content := range []string{
		"database configuration pooling timeout alpha",
		"database configuration charset collation beta",
		"database configuration vacuum analyze gamma",
	} {
		res := callTool(t, session, "ghost_memory_save", map[string]any{
			"project_id": "test-project", "content": content, "category": "gotcha",
		})
		if res.IsError {
			t.Fatalf("save %d: %s", i, resultText(res))
		}
	}

	tests := []struct {
		name              string
		limit, wantWindow int
	}{
		{"category widens the window", 2, 6},
		{"the widening is capped", 100, 100},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := callTool(t, session, "ghost_memory_search", map[string]any{
				"project_id": "test-project",
				"query":      "database configuration",
				"category":   "gotcha",
				"limit":      tc.limit,
				"explain":    true,
			})
			if res.IsError {
				t.Fatalf("explain failed: %s", resultText(res))
			}
			var ex memory.SearchExplain
			if err := json.Unmarshal([]byte(resultText(res)), &ex); err != nil {
				t.Fatalf("explain response is not JSON: %v", err)
			}
			if ex.Limit != tc.wantWindow {
				t.Errorf("explained window = %d, want %d (the window the formatted path searches)",
					ex.Limit, tc.wantWindow)
			}
		})
	}
}

// partialFailureStore is a store where one leg failed and the other completed
// with nothing: the retriever's partial case, which is not an error and must not
// be rendered as one either. What it must never be rendered as is an absence.
type partialFailureStore struct {
	provider.MemoryStore
}

func (partialFailureStore) Candidates(context.Context, memory.CandidateRequest) (*memory.CandidateSet, error) {
	set := &memory.CandidateSet{Legs: map[string]memory.LegStatus{
		"fts":    {Applicable: true, Attempted: true, Available: false, Err: "search memories: no such table: memories_fts"},
		"vector": {Applicable: true, Attempted: true, Available: true},
	}}
	return set, nil
}

// TestPartialRetrievalFailureIsNotAnAbsence: a leg that errored while the other
// completed comes back as a zero-row set with statuses, not an error — and the
// handler used to read only len(Items), so the agent received "No matching
// memories found." for a search that never ran its keyword leg. With a filter
// set it received the filter caveat too, blaming the filter for rows the
// retriever never produced.
func TestPartialRetrievalFailureIsNotAnAbsence(t *testing.T) {
	store := testStore(t)
	srv := New(partialFailureStore{MemoryStore: store},
		slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	session := connectedClient(t, srv)

	res := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "database configuration",
		"category":   "fact",
	})

	if !res.IsError {
		t.Fatalf("a search whose keyword leg failed returned a successful result: %s", resultText(res))
	}
	out := resultText(res)
	if strings.Contains(out, "No matching memories found") {
		t.Errorf("a partial retrieval failure reported absence: %s", out)
	}
	if strings.Contains(out, "category filter") {
		t.Errorf("a partial retrieval failure blamed the category filter for rows the retriever never produced: %s", out)
	}
	if !strings.Contains(out, "memories_fts") {
		t.Errorf("error text does not name the leg that failed: %s", out)
	}
}

// degradedStore returns rows from a leg that worked while another leg failed —
// the partial case with a non-empty answer. The rows are real matches, so the
// answer must be degraded rather than destroyed: an error here loses a working
// retrieval to a broken index.
type degradedStore struct {
	provider.MemoryStore
}

func (degradedStore) Candidates(_ context.Context, _ memory.CandidateRequest) (*memory.CandidateSet, error) {
	set := &memory.CandidateSet{Legs: map[string]memory.LegStatus{
		"fts":    {Applicable: true, Attempted: true, Available: false, Err: "search memories: no such table: memories_fts"},
		"vector": {Applicable: true, Attempted: true, Available: true, Eligible: 1},
	}}
	for _, c := range []struct{ id, content string }{
		{"VEC1", "vector-only match that survived the keyword leg failure"},
	} {
		set.Rows = append(set.Rows, memory.Candidate{
			Memory: memory.Memory{
				ID: c.id, ProjectID: "test-project", Category: "fact",
				Content: c.content, CreatedAt: "2026-09-01 12:00:00", Source: "mcp",
			},
			FTSRank: -1, VectorRank: 0, VectorScore: 0.9, Base: 0.9, Decay: 1, Score: 0.9,
		})
	}
	return set, nil
}

// TestPartialFailureWithRowsStillReturnsThem: a leg that failed while another
// returned matches is a degraded answer, not a failed one. Returning a tool
// error here would discard a working retrieval because the keyword index is
// broken, and would claim "unknown whether anything matches" about a result that
// has rows in it.
func TestPartialFailureWithRowsStillReturnsThem(t *testing.T) {
	store := testStore(t)
	srv := New(degradedStore{MemoryStore: store},
		slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	session := connectedClient(t, srv)

	res := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "vector-only match",
	})

	if res.IsError {
		t.Fatalf("a partial failure with rows returned a tool error: %s", resultText(res))
	}
	out := resultText(res)
	if !strings.Contains(out, "vector-only match that survived") {
		t.Errorf("the surviving leg's rows were discarded: %s", out)
	}
	if !strings.Contains(out, "memories_fts") {
		t.Errorf("the answer does not say which leg failed, so the caller cannot tell it is degraded: %s", out)
	}
	if strings.Contains(out, "No matching memories found") {
		t.Errorf("a result with rows reported absence: %s", out)
	}
}

// invalidStore returns only rows whose validity window has closed. The assembler
// withholds them all, so the answer is empty for a reason the caller must be
// told: the rows were found and then judged out of date, which is not the same as
// not existing. Store.ImportMemory and Restore both write these columns, so this
// is reachable from a real artifact.
type invalidStore struct {
	provider.MemoryStore
}

func (invalidStore) Candidates(_ context.Context, _ memory.CandidateRequest) (*memory.CandidateSet, error) {
	past := "2020-01-01 00:00:00"
	set := &memory.CandidateSet{Legs: map[string]memory.LegStatus{
		"fts": {Applicable: true, Attempted: true, Available: true, CoverageComplete: false},
	}}
	set.Rows = append(set.Rows, memory.Candidate{
		Memory: memory.Memory{
			ID: "OLD1", ProjectID: "test-project", Category: "fact",
			Content: "retention policy that has since expired", CreatedAt: "2026-09-01 12:00:00",
			Source: "mcp", ValidUntil: &past,
		},
		Base: 0.9, Decay: 1, Score: 0.9,
	})
	return set, nil
}

// TestExpiredRowsAreReportedAsWithheldNotAbsent: stage 2 dropping every row must
// not render as the bare absence string. A caller told "no matching memories"
// concludes the memory does not exist; the truth is that it was found and
// withheld as out of date, which is a different instruction (refresh it, ask
// about something else).
func TestExpiredRowsAreReportedAsWithheldNotAbsent(t *testing.T) {
	store := testStore(t)
	srv := New(invalidStore{MemoryStore: store},
		slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	session := connectedClient(t, srv)

	res := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "retention policy",
	})

	if res.IsError {
		t.Fatalf("a withheld-for-validity result returned a tool error: %s", resultText(res))
	}
	out := resultText(res)
	if !strings.Contains(out, "out of date") {
		t.Errorf("every row was withheld as expired, but the answer does not say so: %s", out)
	}
	// The absence sentence is not merely accompanied by the explanation, it is
	// not there: a caller who reads only the first line must not be told these
	// memories do not exist when they were found and withheld.
	if strings.Contains(out, "No matching memories found") {
		t.Errorf("a withheld answer leads with an absence claim: %s", out)
	}
}

// TestEmptyWhyMakesNoUniformClaimAndShowsTheBreakdown: the reason set is closed,
// so a set emptied by two stages carries one reason — and the sentence that
// leads the answer must not claim every row failed the same way while the note
// beneath it says otherwise. The copy and the note are asserted together here,
// because their agreement is the property: neither is trustworthy alone.
func TestEmptyWhyMakesNoUniformClaimAndShowsTheBreakdown(t *testing.T) {
	result := assemble.Result{
		Reason: "all_invalid", // the dominant cause, per the assembler's own choice
		Notes: []string{
			"10 candidate rows were removed and none reached the answer: validity 1, budget 9",
		},
	}

	why := emptyWhy(result)
	if why == "" {
		t.Fatal("emptyWhy returned nothing for a set the assembler emptied")
	}
	for _, uniform := range []string{"Every matching memory", "All matching memories"} {
		if strings.Contains(why, uniform) {
			t.Errorf("the answer claims a uniform cause (%q) while the note says two stages removed rows: %s", uniform, why)
		}
	}
	if !strings.Contains(why, "withheld as out of date") {
		t.Errorf("the answer does not say what happened to the rows: %s", why)
	}
	if got := assemblerNotes(result); !strings.Contains(got, "validity 1, budget 9") {
		t.Errorf("the answer omits the per-stage breakdown that makes the sentence checkable: %q", got)
	}

	// A plain no-match keeps the absence sentence: there is nothing withheld to
	// explain, and the caveat covers the window.
	if why := emptyWhy(assemble.Result{Reason: "no_candidates"}); why != "" {
		t.Errorf("emptyWhy(%q) = %q, want nothing: the absence sentence is already true", "no_candidates", why)
	}
}

// TestCategoryAndScopeReasonsAreLeftToTheFilterCaveat: those two reasons are
// already named by filterCaveat, which also suggests the next step, so emptyWhy
// must not duplicate them. The rendered answers are pinned by the existing scope
// tests; this asserts the division of labour so a later reason is not added twice.
func TestCategoryAndScopeReasonsAreLeftToTheFilterCaveat(t *testing.T) {
	for _, reason := range []string{"all_out_of_category", "all_out_of_scope", "all_dedup_dropped", "all_diversity_capped"} {
		if why := emptyWhy(assemble.Result{Reason: reason}); why != "" {
			t.Errorf("emptyWhy(%q) = %q, want \"\": the filter caveat names the filter, and the dedup and "+
				"diversity stages do not run in this version", reason, why)
		}
	}
}
