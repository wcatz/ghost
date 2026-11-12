package mcpserver

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/provider"
)

type searchStore struct {
	provider.MemoryStore
	set *memory.CandidateSet
}

func (s searchStore) Candidates(context.Context, memory.CandidateRequest) (*memory.CandidateSet, error) {
	return s.set, nil
}

func weakRow(id, content string) memory.Candidate {
	return memory.Candidate{Memory: memory.Memory{ID: id, ProjectID: "test-project", Category: "fact", Content: content, CreatedAt: "2026-09-01 12:00:00", Importance: 0.7}, FTSRank: 6, VectorRank: 0, VectorScore: 0.44, Base: 0.5, Decay: 1.0, Score: 0.5}
}

func weakSet(rows ...memory.Candidate) *memory.CandidateSet {
	return &memory.CandidateSet{Rows: rows, Legs: map[string]memory.LegStatus{"fts": {Applicable: true, Attempted: true, Available: true, CoverageComplete: true}, "vector": {Applicable: true, Attempted: true, Available: true}}}
}

func keywordSet() *memory.CandidateSet {
	return &memory.CandidateSet{Legs: map[string]memory.LegStatus{"fts": {Applicable: true, Attempted: true, Available: true, CoverageComplete: true}, "vector": {Applicable: true, Attempted: false}}}
}

func newSearchSession(t *testing.T, set *memory.CandidateSet) (*Server, *mcp.ClientSession) {
	t.Helper()
	srv := New(searchStore{MemoryStore: testStore(t), set: set}, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	return srv, connectedClient(t, srv)
}

func searchText(t *testing.T, session *mcp.ClientSession, query string) string {
	t.Helper()
	res := callTool(t, session, "ghost_memory_search", map[string]any{"project_id": "test-project", "query": query})
	if res.IsError {
		t.Fatalf("search failed: %s", resultText(res))
	}
	return resultText(res)
}

func saveMemory(t *testing.T, session *mcp.ClientSession, content string) {
	t.Helper()
	res := callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "test-project", "content": content, "category": "fact"})
	if res.IsError {
		t.Fatalf("save: %s", resultText(res))
	}
}

func searchToolDescription(t *testing.T) string {
	t.Helper()
	_, session := newCapSession(t)
	tools, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools.Tools {
		if tl.Name == "ghost_memory_search" {
			return tl.Description
		}
	}
	t.Fatal("missing")
	return ""
}

func countItemLines(out string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "- [") {
			n++
		}
	}
	return n
}

func outcomeOf(out string) string {
	i := strings.Index(out, "outcome=")
	if i < 0 {
		return "no verdict at all"
	}
	rest := out[i+len("outcome="):]
	if j := strings.IndexByte(rest, ' '); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// #580
func TestAStrongSearchCarriesTheVerdictAndNoAbstention(t *testing.T) {
	_, session := newCapSession(t)
	saveMemory(t, session, "database configuration pooling timeout and retry backoff")
	out := searchText(t, session, "database configuration pooling")
	if !strings.Contains(out, "[ghost:outcome=answerable") || !strings.Contains(out, "abstain_cosine=off") || strings.Contains(out, "do not rely") || strings.Contains(out, "trustworthy") {
		t.Fatalf("unexpected strong search: %s", out)
	}
}

func TestAWeakSearchIsLabelledWeakAndKeepsItsRows(t *testing.T) {
	_, session := newSearchSession(t, weakSet(weakRow("A1", "the reconciliation ledger closes nightly"), weakRow("A2", "the ledger reconciles against the billing export")))
	out := searchText(t, session, "ledger reconciliation")
	if !strings.Contains(out, "outcome=weak") || !strings.Contains(out, "reason=below_floor") || !strings.Contains(out, "do not rely") || !strings.Contains(out, "the reconciliation ledger closes nightly") {
		t.Fatalf("unexpected weak search: %s", out)
	}
}

func TestAServerBuiltWithoutNewStillGetsTheShippedCap(t *testing.T) {
	bare := &Server{store: testStore(t), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if got := bare.searchResponseCap(); got != searchResponseMaxBytes {
		t.Fatalf("got %d want %d", got, searchResponseMaxBytes)
	}
	if got := (&Server{store: testStore(t), logger: slog.New(slog.NewTextHandler(io.Discard, nil)), searchMaxBytes: 1}).searchResponseCap(); got != 1 {
		t.Fatalf("got %d want 1", got)
	}
	if got := (&Server{store: testStore(t), logger: slog.New(slog.NewTextHandler(io.Discard, nil)), searchMaxBytes: -1}).searchResponseCap(); got != searchResponseMaxBytes {
		t.Fatalf("got %d want %d", got, searchResponseMaxBytes)
	}
}

func TestTheByteCapCoversTheQualifiersToo(t *testing.T) { // #1483
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)
	for i := range 20 {
		callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "test-project", "content": fmt.Sprintf("deployment rollback runbook step %02d %s", i, strings.Repeat(fmt.Sprintf("note%02d ", i), 70)), "category": "fact", "importance": 0.5})
	}
	query := map[string]any{"project_id": "test-project", "query": "deployment rollback runbook", "limit": 100}
	current := len(resultText(callTool(t, session, "ghost_memory_search", query)))
	historical := len(resultText(callTool(t, session, "ghost_memory_search", map[string]any{"project_id": "test-project", "query": "deployment rollback runbook", "limit": 100, "as_of": asOfToolFuture})))
	if historical <= current {
		t.Fatalf("precondition")
	}
	srv.searchMaxBytes = current
	out := resultText(callTool(t, session, "ghost_memory_search", map[string]any{"project_id": "test-project", "query": "deployment rollback runbook", "limit": 100, "as_of": asOfToolFuture}))
	if !strings.Contains(out, "historical read") || len(out) > srv.searchMaxBytes || !strings.Contains(out, "[ghost:outcome=") {
		t.Fatalf("unexpected capped historical: %d > %d", len(out), srv.searchMaxBytes)
	}
}

func TestAnAbsenceClaimIsWithheldWhenTheSearchWasNotComplete(t *testing.T) {
	_, session := newSearchSession(t, weakSet())
	out := searchText(t, session, "ledger reconciliation")
	if strings.Contains(out, "No matching memories found") || strings.Contains(out, "no sufficiently trustworthy memory found") || !strings.Contains(out, "no match within the searched window") || !strings.Contains(out, "reason=no_candidates") {
		t.Fatalf("unexpected: %s", out)
	}
}

func TestASearchWithNoEmbedderNamesTheLegItNeverRan(t *testing.T) {
	_, session := newSearchSession(t, keywordSet())
	out := searchText(t, session, "ledger reconciliation")
	if strings.Contains(out, "No matching memories found") || !strings.Contains(out, "reason=vector_backend_unavailable") || !strings.Contains(out, ",vector:not_run") || !strings.Contains(out, "only the keyword leg ran") {
		t.Fatalf("unexpected: %s", out)
	}
}

func TestARetiredOnlyMatchWithNoEmbedderReportsAllInvalid(t *testing.T) {
	_, session := newCapSession(t)
	saveGlobalValidityRow(t, session, "the ledger reconciliation runbook has been retired", map[string]any{"valid_from": "2020-01-01", "valid_until": "2021-01-01"})
	out := searchText(t, session, "ledger reconciliation runbook")
	if !strings.Contains(out, "reason=all_invalid") || !strings.Contains(out, ",vector:not_run") || !strings.Contains(out, "withheld as out of date") {
		t.Fatalf("unexpected: %s", out)
	}
}

func TestTheSearchResponseStaysInsideItsByteCap(t *testing.T) {
	_, session := newCapSession(t)
	for i := range 30 {
		saveMemory(t, session, fmt.Sprintf("deployment rollback runbook step %02d %s", i, strings.Repeat(fmt.Sprintf("note%02d ", i), 70)))
	}
	res := callTool(t, session, "ghost_memory_search", map[string]any{"project_id": "test-project", "query": "deployment rollback runbook", "limit": 100})
	if res.IsError {
		t.Fatal(resultText(res))
	}
	out := resultText(res)
	if len(out) > searchResponseMaxBytes || !strings.Contains(out, "[ghost:outcome=") || !strings.Contains(out, "admitted=") || cutCount(t, out) < 1 || !strings.Contains(out, "narrow the query to see the rest") || countItemLines(out) < 10 {
		t.Fatalf("unexpected cap behavior: %d bytes", len(out))
	}
}

func TestAnImpossibleByteBudgetIsAToolError(t *testing.T) {
	srv, session := newCapSession(t)
	srv.searchMaxBytes = 20
	saveMemory(t, session, "database configuration pooling")
	res := callTool(t, session, "ghost_memory_search", map[string]any{"project_id": "test-project", "query": "zebra"})
	if !res.IsError {
		t.Fatalf("expected error: %s", resultText(res))
	}
	out := resultText(res)
	if !strings.Contains(out, "response budget exceeded") || strings.Contains(out, "raise the limit") || strings.Contains(out, "No matching memories found") {
		t.Fatalf("unexpected error: %s", out)
	}
}

func TestTheInstructionsOnlyPromiseReachableReasons(t *testing.T) {
	for _, unreachable := range []string{"retrieval_failed", "no_vector_leg", "all_over_budget"} {
		if strings.Contains(mcpInstructions, unreachable) {
			t.Fatalf("instructions promise unreachable %q", unreachable)
		}
	}
	for _, reachable := range []string{"answerable", "weak", "no_candidates", "all_invalid", "all_out_of_scope", "all_out_of_retention", "not_run", "no_floor_arm", "retrieval_partial", "vector_backend_unavailable", "nothing_cleared_the_bar"} {
		if !strings.Contains(mcpInstructions, reachable) {
			t.Fatalf("instructions missing reachable %q", reachable)
		}
	}
	for _, field := range []string{"abstain_cosine", "admitted", "legs", "candidates", "not_applied", "tokens_est"} {
		if !strings.Contains(mcpInstructions, field) {
			t.Fatalf("instructions missing field %q", field)
		}
	}
	for _, falseClaim := range []string{"legs= for which of the two causes", "see legs= for which", "legs= says which cause"} {
		if strings.Contains(mcpInstructions, falseClaim) {
			t.Fatalf("false claim %q", falseClaim)
		}
	}
	if !strings.Contains(mcpInstructions, "no arm had a value to compare") {
		t.Fatal("missing no arm explanation")
	}
	for _, contract := range []string{mcpInstructions, searchToolDescription(t)} {
		at := strings.Index(contract, "tokens_est=...]")
		if at < 0 {
			t.Fatal("missing shape")
		}
		after := contract[at:]
		if len(after) > 200 {
			after = after[:200]
		}
		if !strings.Contains(after, "retrieval_partial") || !strings.Contains(after, "inside the brackets") {
			t.Fatalf("shape incomplete: %s", after)
		}
	}
	for _, field := range []string{"floor_fts_rank", "candidates", "admitted", "legs", "tokens_est"} {
		if !strings.Contains(mcpInstructions, field) {
			t.Fatalf("truncated shape missing %q", field)
		}
	}
	i := strings.Index(mcpInstructions, "not_applied")
	if i < 0 {
		t.Fatal("missing not_applied")
	}
	gloss := mcpInstructions[i:]
	if end := strings.IndexByte(gloss, '\n'); end > 0 {
		gloss = gloss[:end]
	}
	if !strings.Contains(gloss, "never ran") || !strings.Contains(gloss, "failed") {
		t.Fatalf("not_applied gloss incomplete: %q", gloss)
	}
}

func TestTheSearchDescriptionNamesTheRealCap(t *testing.T) {
	_, session := newCapSession(t)
	tools, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools.Tools {
		if tl.Name == "ghost_memory_search" {
			if !strings.Contains(tl.Description, strconv.Itoa(searchResponseMaxBytes)) {
				t.Fatalf("description missing cap: %s", tl.Description)
			}
			return
		}
	}
	t.Fatal("missing tool")
}

func TestTheConfiguredCosineReachesTheFloor(t *testing.T) {
	applied, appliedSession := newSearchSession(t, weakSet(weakRow("A1", "reconciliation ledger")))
	applied.SetContextConfig(config.ContextConfig{AbstainCosine: 0.62})
	if !strings.Contains(searchText(t, appliedSession, "ledger reconciliation"), "abstain_cosine=0.620") {
		t.Fatal("configured cosine not applied")
	}
	srv, session := newCapSession(t)
	srv.SetContextConfig(config.ContextConfig{AbstainCosine: 0.62})
	saveMemory(t, session, "database configuration pooling")
	noEmbedder := searchText(t, session, "database configuration pooling")
	if !strings.Contains(noEmbedder, "abstain_cosine=not_applied") || strings.Contains(noEmbedder, "abstain_cosine=off") {
		t.Fatalf("no-embedder cosine wrong: %s", noEmbedder)
	}
	_, plainSession := newCapSession(t)
	saveMemory(t, plainSession, "database configuration pooling")
	if got := searchText(t, plainSession, "database configuration pooling"); !strings.Contains(got, "abstain_cosine=off") {
		t.Fatalf("default cosine wrong: %s", got)
	}
}

func TestTheSearchDescriptionRendersItsPunctuation(t *testing.T) {
	_, session := newCapSession(t)
	tools, _ := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	var desc string
	for _, tl := range tools.Tools {
		if tl.Name == "ghost_memory_search" {
			desc = tl.Description
			break
		}
	}
	if strings.Contains(desc, `\u`) || strings.Contains(mcpInstructions, `\u`) || !strings.Contains(desc, "—") || !strings.Contains(desc, "abstain_cosine") || !strings.Contains(desc, "verdict line") || strings.Contains(desc, "A weak or empty answer says so in words") {
		t.Fatalf("description punctuation/claims wrong: %q", desc)
	}
}

func TestAMaximumLengthMemoryStillComesBack(t *testing.T) {
	_, session := newCapSession(t)
	head, tailMark := "deployment rollback runbook ", " END-OF-MEMORY"
	body := strings.Repeat("padding text about the rollback procedure. ", 400)
	overlong := head + strings.TrimSuffix(body, " ") + tailMark
	stored, clamped := memory.ClampContent(overlong)
	if !clamped || len(stored) != memory.MaxContentLen+len(memory.TruncationMarker()) || !strings.HasSuffix(stored, memory.TruncationMarker()) {
		t.Fatal("fixture wrong")
	}
	saveMemory(t, session, overlong)
	out := searchText(t, session, "deployment rollback runbook")
	if !strings.Contains(out, "outcome=answerable") || !strings.Contains(out, memory.TruncationMarker()) || len(out) <= len(stored) {
		t.Fatalf("max memory not returned properly: %s", outcomeOf(out))
	}
	if searchResponseMaxBytes != 2*memory.MaxContentLen {
		t.Fatalf("searchResponseMaxBytes wrong")
	}
}
