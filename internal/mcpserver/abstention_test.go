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

// The search surface hands an agent three things the assembler now decides: the
// verdict, the machine line, and a sentence saying whether to rely on the rows.
// The assembler's own tests prove the rules; these prove they reach the tool,
// because the tool's text is what an agent actually reads.

// searchStore wraps a real store so a test can decide what retrieval returns.
// Candidate retrieval is not part of provider.MemoryStore — the assembler
// type-asserts for it — so wrapping is how a caller-supplied set reaches the
// tool, the same way failingCandidateStore injects a failure.
type searchStore struct {
	provider.MemoryStore
	set *memory.CandidateSet
}

func (s searchStore) Candidates(context.Context, memory.CandidateRequest) (*memory.CandidateSet, error) {
	return s.set, nil
}

// weakRow is a row the keyword leg retrieved well outside Arm A's rank window,
// in a candidate set whose vector leg ran and answered — the one state in which
// the assembler can call a result weak. A real store cannot produce it from a
// query with a keyword hit (the best keyword row is always rank 0), which is
// exactly why this is injected.
func weakRow(id, content string) memory.Candidate {
	return memory.Candidate{
		Memory: memory.Memory{
			ID: id, ProjectID: "test-project", Category: "fact", Content: content,
			CreatedAt: "2026-09-01 12:00:00", Importance: 0.7,
		},
		FTSRank: 6, VectorRank: 0, VectorScore: 0.44,
		Base: 0.5, Decay: 1.0, Score: 0.5,
	}
}

// The coverage flags mirror what memory.Candidates reports rather than what a
// test would like: the keyword leg vouches for itself when it was not cut off at
// the window, and the vector leg vouches for nothing until its
// expected/indexed/unembedded counts are reconciled. A helper that set both true
// would let an absence claim pass on a corpus the real leg cannot vouch for.
func weakSet(rows ...memory.Candidate) *memory.CandidateSet {
	return &memory.CandidateSet{
		Rows: rows,
		Legs: map[string]memory.LegStatus{
			"fts":    {Applicable: true, Attempted: true, Available: true, CoverageComplete: true},
			"vector": {Applicable: true, Attempted: true, Available: true},
		},
	}
}

// keywordSet is what a machine with no embedder actually gets: the request asked
// for a hybrid search, so the vector leg is applicable — and it was never
// attempted, which is why the search cannot claim absence.
func keywordSet() *memory.CandidateSet {
	return &memory.CandidateSet{
		Legs: map[string]memory.LegStatus{
			"fts":    {Applicable: true, Attempted: true, Available: true, CoverageComplete: true},
			"vector": {Applicable: true, Attempted: false},
		},
	}
}

func newSearchSession(t *testing.T, set *memory.CandidateSet) (*Server, *mcp.ClientSession) {
	t.Helper()
	srv := New(searchStore{MemoryStore: testStore(t), set: set},
		slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	return srv, connectedClient(t, srv)
}

func searchText(t *testing.T, session *mcp.ClientSession, query string) string {
	t.Helper()
	res := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      query,
	})
	if res.IsError {
		t.Fatalf("search failed: %s", resultText(res))
	}
	return resultText(res)
}

func saveMemory(t *testing.T, session *mcp.ClientSession, content string) {
	t.Helper()
	res := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project",
		"content":    content,
		"category":   "fact",
	})
	if res.IsError {
		t.Fatalf("save: %s", resultText(res))
	}
}

// searchToolDescription is the description string exactly as it reaches an MCP
// client, which is not the same as the Go literal: an interpreted string decodes
// its escapes on the way out and a raw one does not. Tests that assert on the
// description's TEXT have to read it from a live session, not from the source.
func searchToolDescription(t *testing.T) string {
	t.Helper()
	_, session := newCapSession(t)
	tools, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tl := range tools.Tools {
		if tl.Name == "ghost_memory_search" {
			return tl.Description
		}
	}
	t.Fatal("ghost_memory_search is not in the tool list")
	return ""
}

// TestAStrongSearchCarriesTheVerdictAndNoAbstention is #580's regression at the
// tool boundary. An answerable result must not acquire an abstention caveat just
// because the assembler grew one: a caveat on a result that withheld nothing
// teaches an agent to discount the whole surface.
func TestAStrongSearchCarriesTheVerdictAndNoAbstention(t *testing.T) {
	_, session := newCapSession(t)
	saveMemory(t, session, "database configuration pooling timeout and retry backoff")

	out := searchText(t, session, "database configuration pooling")

	if !strings.Contains(out, "[ghost:outcome=answerable") {
		t.Errorf("a search result carries no machine verdict:\n%s", out)
	}
	if !strings.Contains(out, "abstain_cosine=off") {
		t.Errorf("the machine line does not report the vector arm as off, so a reader cannot tell a "+
			"disabled threshold from a cosine of zero:\n%s", out)
	}
	if strings.Contains(out, "do not rely") || strings.Contains(out, "trustworthy") {
		t.Errorf("an answerable result carries abstention copy:\n%s", out)
	}
}

// TestAWeakSearchIsLabelledWeakAndKeepsItsRows: the tool is where a weak verdict
// becomes an instruction, and the instruction is worthless without the rows that
// produced it. A weak result must show them AND say not to rely on them.
func TestAWeakSearchIsLabelledWeakAndKeepsItsRows(t *testing.T) {
	_, session := newSearchSession(t, weakSet(
		weakRow("A1", "the reconciliation ledger closes nightly"),
		weakRow("A2", "the ledger reconciles against the billing export"),
	))

	out := searchText(t, session, "ledger reconciliation")

	if !strings.Contains(out, "outcome=weak") || !strings.Contains(out, "reason=below_floor") {
		t.Fatalf("a weak result does not say so on the verdict line:\n%s", out)
	}
	if !strings.Contains(out, "do not rely") {
		t.Errorf("a weak result carries no sentence telling the caller not to rely on the rows:\n%s", out)
	}
	if !strings.Contains(out, "the reconciliation ledger closes nightly") {
		t.Errorf("a weak result withheld its rows, so the warning has nothing to warn about:\n%s", out)
	}
}

// TestAServerBuiltWithoutNewStillGetsTheShippedCap: the response cap lives in a
// field, and `assemble.Budget.MaxBytes` reads 0 as UNBOUNDED — so a Server whose
// field was never set would answer with whatever the corpus holds, silently. The
// tree builds a Server as a struct literal in tests, and a second constructor
// would too, so the field's zero value has to mean the shipped default exactly
// as the two setter-backed fields above it do.
//
// The assertion is on the accessor rather than on the wire because the tool path
// cannot be built from a struct literal: `New` also builds the embedded
// mcp.Server, and without it there are no tools to call. That is the point — the
// cap is read through one accessor from one place, so the two cannot disagree.
func TestAServerBuiltWithoutNewStillGetsTheShippedCap(t *testing.T) {
	bare := &Server{store: testStore(t), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if got := bare.searchResponseCap(); got != searchResponseMaxBytes {
		t.Errorf("searchResponseCap() = %d on a Server whose field was never set, want the shipped "+
			"default %d: zero reads as UNBOUNDED to the assembler, so the cap would vanish rather "+
			"than fall back", got, searchResponseMaxBytes)
	}
	// A cap below the empty envelope is how a test reaches the error path, so a
	// small POSITIVE value has to survive rather than being read as unset.
	if got := (&Server{store: testStore(t), logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		searchMaxBytes: 1}).searchResponseCap(); got != 1 {
		t.Errorf("searchResponseCap() = %d with the field set to 1: a deliberate cap must survive, "+
			"including the below-envelope one the error-path test uses", got)
	}
	// A negative cap is not a deliberate one — it reads as unbounded to the
	// assembler exactly as zero does — so it falls back with it rather than being
	// the one value that removes the cap.
	if got := (&Server{store: testStore(t), logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		searchMaxBytes: -1}).searchResponseCap(); got != searchResponseMaxBytes {
		t.Errorf("searchResponseCap() = %d with the field set to -1, want the shipped default %d: a value "+
			"the assembler cannot use must not be the one that removes the cap", got, searchResponseMaxBytes)
	}
}

// TestTheByteCapCoversTheQualifiersToo: the cap is on the text the caller
// receives, and an `as_of` answer is the assembler's render PLUS a qualifier
// block the surface prepends — roughly 800 bytes saying the block is a past
// reading. If the post-pass measures only the assembler's share, a 16000-byte
// answer ships at ~16800 and rows have already been dropped to fit a number that
// never described what was sent. The delivered text has to be inside the cap, so
// the cap is measured against a historical read and not only a current one.
func TestTheByteCapCoversTheQualifiersToo(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)

	// Small enough that the qualifier is the difference between fitting and not:
	// the same corpus at the shipped cap is well inside it, and a current read of
	// the same corpus fits with room to spare — which is what leaves the
	// disclosure as the only thing that can push a historical read over a cap set
	// to what the current one needed. A corpus already at the cap would be trimmed
	// whether the qualifier was measured or not, and the precondition below would
	// pass for the wrong reason.
	var current, historical int
	for i := range 20 {
		res := callTool(t, session, "ghost_memory_save", map[string]any{
			"project_id": "test-project",
			"content": fmt.Sprintf("deployment rollback runbook step %02d %s",
				i, strings.Repeat(fmt.Sprintf("note%02d ", i), 70)),
			"category":   "fact",
			"importance": 0.5,
		})
		if res.IsError {
			t.Fatalf("save: %s", resultText(res))
		}
	}
	query := map[string]any{
		"project_id": "test-project",
		"query":      "deployment rollback runbook",
		"limit":      100,
	}
	current = len(resultText(callTool(t, session, "ghost_memory_search", query)))
	withAsOf := map[string]any{
		"project_id": "test-project",
		"query":      "deployment rollback runbook",
		"limit":      100,
		"as_of":      asOfToolFuture,
	}
	historical = len(resultText(callTool(t, session, "ghost_memory_search", withAsOf)))
	if historical <= current {
		t.Fatalf("precondition: the as_of answer is %d bytes against %d for a current read, so this "+
			"corpus does not exercise the qualifier's effect on the envelope", historical, current)
	}
	// Now the cap: set to what the current read needed, which is less than the
	// historical read needs once the qualifier is counted, and assert the
	// DELIVERED text fits it.
	srv.searchMaxBytes = current
	out := resultText(callTool(t, session, "ghost_memory_search", withAsOf))
	if !strings.Contains(out, "historical read") {
		t.Fatalf("precondition: the as_of qualifier is missing, so the cap is not being measured against "+
			"the text that carries it:\n%s", tail(out, 300))
	}
	if len(out) > srv.searchMaxBytes {
		t.Errorf("the historical answer is %d bytes against a cap of %d, so the qualifier block was "+
			"prepended to a render the fit pass had already measured and trimmed:\n%s",
			len(out), srv.searchMaxBytes, tail(out, 300))
	}
	if !strings.Contains(out, "[ghost:outcome=") {
		t.Errorf("a capped answer lost its verdict line:\n%s", tail(out, 300))
	}
}

// TestAnAbsenceClaimIsWithheldWhenTheSearchWasNotComplete: `no_candidates` is
// the only reason that may say nothing matched, and only over coverage every
// applicable leg vouched for. Here both legs ran and answered, but the vector
// leg declines to vouch for its own coverage until its counts are reconciled, so
// "no match within the searched window" is what the search can honestly say.
func TestAnAbsenceClaimIsWithheldWhenTheSearchWasNotComplete(t *testing.T) {
	_, session := newSearchSession(t, weakSet())

	out := searchText(t, session, "ledger reconciliation")

	if strings.Contains(out, "No matching memories found") {
		t.Errorf("a search whose vector leg declines to vouch for its coverage still claims the store "+
			"has nothing:\n%s", out)
	}
	if strings.Contains(out, "no sufficiently trustworthy memory found") {
		t.Errorf("the exclusion wording claims rows were found and withheld, which is false here:\n%s", out)
	}
	if !strings.Contains(out, "no match within the searched window") {
		t.Errorf("the answer does not carry the window note that replaces the absence claim:\n%s", out)
	}
	if !strings.Contains(out, "reason=no_candidates") {
		t.Errorf("the verdict line does not name the reason:\n%s", out)
	}
}

// TestASearchWithNoEmbedderNamesTheLegItNeverRan: the same empty answer on a
// machine with no embedder gets a different reason, and the difference is the
// caller's next move. A leg that never ran is a machine to fix; a window that
// was too small is a request to re-send. Naming the leg is also what keeps the
// empty and non-empty branches telling the same story about the same machine —
// the non-empty one already reports the leg's absence on the line for it.
func TestASearchWithNoEmbedderNamesTheLegItNeverRan(t *testing.T) {
	_, session := newSearchSession(t, keywordSet())

	out := searchText(t, session, "ledger reconciliation")

	if strings.Contains(out, "No matching memories found") {
		t.Errorf("a search whose vector leg never ran still claims the store has nothing:\n%s", out)
	}
	if !strings.Contains(out, "reason=vector_backend_unavailable") {
		t.Errorf("the verdict line does not name the leg that could not run:\n%s", out)
	}
	if !strings.Contains(out, ",vector:not_run") {
		t.Errorf("the verdict line does not report the leg as never executed, so the two facts that "+
			"distinguish a missing embedder from a broken search are split:\n%s", out)
	}
	if !strings.Contains(out, "only the keyword leg ran") {
		t.Errorf("the answer does not say which leg carried the search:\n%s", out)
	}
}

// TestTheSearchResponseStaysInsideItsByteCap: the cap is what keeps a 100-row
// result of 8000-byte memories from spending a harness's whole context window.
// It bounds the whole response, framing included, so it is asserted on the bytes
// the caller receives rather than on a row count — and the fixture is sized to
// exceed the cap comfortably, so an uncapped answer fails it rather than
// passing by accident.
func TestTheSearchResponseStaysInsideItsByteCap(t *testing.T) {
	_, session := newCapSession(t)
	// Every row is its own text: rows differing only in a trailing character are
	// near-duplicates, and the save path folds those, so a fixture built that way
	// silently shrinks to a handful of memories and stops exercising the cap.
	for i := range 30 {
		saveMemory(t, session, fmt.Sprintf("deployment rollback runbook step %02d %s",
			i, strings.Repeat(fmt.Sprintf("note%02d ", i), 70)))
	}

	res := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "deployment rollback runbook",
		"limit":      100,
	})
	if res.IsError {
		t.Fatalf("search failed: %s", resultText(res))
	}
	out := resultText(res)

	if len(out) > searchResponseMaxBytes {
		t.Errorf("the response is %d bytes, want at most %d: a search result that can fill a "+
			"harness's context window is a result nobody can use", len(out), searchResponseMaxBytes)
	}
	if !strings.Contains(out, "[ghost:outcome=") {
		t.Errorf("a trimmed response lost its verdict line:\n%s", tail(out, 300))
	}
	if !strings.Contains(out, "admitted=") {
		t.Errorf("the verdict line does not report how many rows were admitted:\n%s", tail(out, 300))
	}
	// And the fixture has to be big enough that the assertion above is a real
	// test: an uncapped answer over this corpus is several times the cap.
	if got := countItemLines(out); got < 10 {
		t.Errorf("only %d item lines came back, so the cap may not have bound anything: "+
			"the fixture is too small to exercise it", got)
	}
}

// countItemLines counts the rendered memory lines, which is how the fixture's
// size is checked without pinning a row count the pipeline may legitimately
// change.
func countItemLines(out string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "- [") {
			n++
		}
	}
	return n
}

// TestAnImpossibleByteBudgetIsAToolError: a cap below the empty envelope is not
// an empty answer, and a caller that received one would go looking for a memory
// to save. The tool's own text has to say the budget could not be met.
func TestAnImpossibleByteBudgetIsAToolError(t *testing.T) {
	srv, session := newCapSession(t)
	srv.searchMaxBytes = 20
	saveMemory(t, session, "database configuration pooling")

	res := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "database configuration pooling",
	})

	if !res.IsError {
		t.Fatalf("an impossible budget returned a successful result: %s", resultText(res))
	}
	out := resultText(res)
	if !strings.Contains(out, "response budget exceeded") {
		t.Errorf("the error does not carry the documented copy: %s", out)
	}
	if strings.Contains(out, "raise the limit") {
		t.Errorf("the error tells the caller to raise the limit, which admits more rows and fails "+
			"identically: %s", out)
	}
	if strings.Contains(out, "No matching memories found") {
		t.Errorf("a budget that could not be met reported absence: %s", out)
	}
}

// TestTheInstructionsOnlyPromiseReachableReasons: the instruction block tells an
// agent how to read the verdict line, and a reason named there that the tool can
// never put on a line is an instruction to wait for something that will not
// arrive. A search whose legs FAILED is converted into a tool error, so
// `retrieval_failed` belongs to that error and never to a rendered answer;
// `no_floor_arm` needs a request with no keyword leg, and this tool always asks
// for a hybrid search. `vector_backend_unavailable` is reachable and named —
// an empty answer on a machine with no embedder carries it — because a leg that
// never ran is a configuration state the caller fixes, not an incident.
func TestTheInstructionsOnlyPromiseReachableReasons(t *testing.T) {
	// Two reasons the assembler can produce that a ghost_memory_search verdict
	// line can never carry, and telling an agent to expect one is an instruction
	// to wait for something that will not arrive. `retrieval_failed` because a
	// search whose legs failed is converted into a tool error; `no_floor_arm`
	// because it needs a request with no keyword leg, and this tool always asks
	// for a hybrid search. `no_floor_arm` is still a real reason — see
	// docs/configuration.md — but it belongs to a surface this instruction block
	// is not about.
	for _, unreachable := range []string{"retrieval_failed", "no_vector_leg"} {
		if strings.Contains(mcpInstructions, unreachable) {
			t.Errorf("mcpInstructions tells the agent to expect %q on a ghost_memory_search verdict "+
				"line, which that tool cannot produce", unreachable)
		}
	}
	// And the ones it can, which a caller will meet and the block must explain.
	for _, reachable := range []string{
		"answerable", "weak", "no_candidates", "all_invalid", "all_over_budget", "all_out_of_scope",
		"all_out_of_retention", "not_run", "no_floor_arm", "retrieval_partial", "vector_backend_unavailable",
		"nothing_cleared_the_bar",
	} {
		if !strings.Contains(mcpInstructions, reachable) {
			t.Errorf("mcpInstructions does not explain %q, which a ghost_memory_search caller can meet", reachable)
		}
	}
	// The FIELDS on the line, not just the reasons. An agent holding the line has
	// to be able to read all of it: `abstain_cosine` is the only signal that a
	// configured floor did nothing, and `admitted` is the only signal that a
	// byte-cap trim happened now that the window caveat is suppressed for one.
	for _, field := range []string{"abstain_cosine", "admitted", "legs", "candidates", "not_applied", "tokens_est"} {
		if !strings.Contains(mcpInstructions, field) {
			t.Errorf("mcpInstructions never mentions %q, so a caller holding the verdict line has no "+
				"way to read it", field)
		}
	}
	// A field must never be described as identifying a cause it cannot identify.
	// `no_floor_arm` and `not_run` are both reached by a machine with no embedder
	// AND by one query the embedder refused, and neither the reason nor legs= can
	// tell them apart — so pointing a caller at legs= for the cause sends a reader
	// to diagnose an outage that is not there.
	for _, falseClaim := range []string{
		"legs= for which of the two causes",
		"see legs= for which",
		"legs= says which cause",
	} {
		if strings.Contains(mcpInstructions, falseClaim) {
			t.Errorf("mcpInstructions tells an agent to read legs= for a cause it cannot carry (%q): "+
				"a missing embedder and a query that would not embed are the same not_run", falseClaim)
		}
	}
	// And the block must actually say so, or the correction reads as a deletion.
	if !strings.Contains(mcpInstructions, "no arm had a value to compare") {
		t.Errorf("mcpInstructions does not say what no_floor_arm means, which is the fact an agent " +
			"needs to tell an unjudged row from a judged one")
	}
	// The shape it shows has to cover the WHOLE line, and the line's last field is
	// not the last thing in it: a leg that ran and failed appends
	// `retrieval_partial` inside the brackets. A parser written from a shape that
	// ends at `tokens_est=...]` stops short of the closing bracket on exactly the
	// degraded answers this block tells the agent to expect.
	//
	// The check is POSITIONAL, and it has to be: the token's name is already in
	// both contracts as a REASON (`retrieval_partial`), so a containment test
	// passes on a contract that never mentions the trailing token at all. What has
	// to hold is that the caveat follows the quoted shape.
	for _, contract := range []string{mcpInstructions, searchToolDescription(t)} {
		at := strings.Index(contract, "tokens_est=...]")
		if at < 0 {
			t.Errorf("the documented verdict line does not quote a shape ending at tokens_est=...]: %.160s", contract)
			continue
		}
		// The window is BOUNDED, and it has to be: both contracts mention the
		// token's name again further down, as a reason, so an unbounded search
		// finds that mention and passes on a contract with no caveat at all. The
		// caveat belongs in the sentence that quotes the shape.
		after := contract[at:]
		if len(after) > 200 {
			after = after[:200]
		}
		if !strings.Contains(after, "retrieval_partial") {
			t.Errorf("the quoted shape ends at tokens_est=...] with no mention of the optional trailing "+
				"retrieval_partial token, so a parser written from it stops short of the closing bracket: %.160s",
				after)
		}
		if !strings.Contains(after, "inside the brackets") {
			t.Errorf("the documented shape does not say WHERE the optional token goes, which is the half "+
				"that makes it parseable: %.160s", after)
		}
	}
	// And the shape it shows has to be the whole line, not its head: an agent
	// parsing the documented shape must not stop at the first two fields.
	for _, field := range []string{"floor_fts_rank", "candidates", "admitted", "legs", "tokens_est"} {
		if !strings.Contains(mcpInstructions, field) {
			t.Errorf("mcpInstructions shows a truncated verdict line, missing %q", field)
		}
	}
	// `not_applied` has TWO causes, and the code prints it for both: the vector
	// leg never executed (no embedder) and it ran and failed. The reason and legs
	// fields say which; the gloss must not claim only the first, or an operator
	// with a working embedder and a broken search reads a configuration state.
	i := strings.Index(mcpInstructions, "not_applied")
	if i < 0 {
		t.Fatal("mcpInstructions never mentions not_applied")
	}
	gloss := mcpInstructions[i:]
	if end := strings.IndexByte(gloss, '\n'); end > 0 {
		gloss = gloss[:end]
	}
	for _, cause := range []string{"never ran", "failed"} {
		if !strings.Contains(gloss, cause) {
			t.Errorf("the not_applied gloss names only one cause, missing %q: %q", cause, gloss)
		}
	}
}

// TestTheSearchDescriptionNamesTheRealCap: an agent budgeting from the
// description has to be told the number the server actually enforces. Half the
// real cap is not a rounding difference — it is a caller that sizes its own
// output against a number the server never applies.
func TestTheSearchDescriptionNamesTheRealCap(t *testing.T) {
	_, session := newCapSession(t)

	tools, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var desc string
	for _, tl := range tools.Tools {
		if tl.Name == "ghost_memory_search" {
			desc = tl.Description
		}
	}
	if desc == "" {
		t.Fatal("ghost_memory_search has no description")
	}
	// The figure is quoted in bytes, exactly as the constant, so the test cannot
	// pass on a rounded restatement of a different number.
	if !strings.Contains(desc, strconv.Itoa(searchResponseMaxBytes)) {
		t.Errorf("the description does not state the %d-byte cap the server enforces:\n%s",
			searchResponseMaxBytes, desc)
	}
}

// TestTheConfiguredCosineReachesTheFloor: context.abstain_cosine is a floor a
// user sets believing it is in force. A key that binds in config and is then
// dropped on the way to the assembler would be a silent no-op, so the value is
// pinned where it is resolved.
//
// Both states are asserted because both are reachable and they must not look
// alike: with a vector leg that answered, the configured threshold is the one
// that was applied; with no embedder — this machine's ordinary state — it was
// configured and never used, which the line says rather than printing a number
// beside a verdict the cosine never touched.
func TestTheConfiguredCosineReachesTheFloor(t *testing.T) {
	applied, appliedSession := newSearchSession(t, weakSet(weakRow("A1", "reconciliation ledger")))
	applied.SetContextConfig(config.ContextConfig{AbstainCosine: 0.62})
	out := searchText(t, appliedSession, "ledger reconciliation")
	if !strings.Contains(out, "abstain_cosine=0.620") {
		t.Errorf("the configured cosine did not reach the applied floor:\n%s", out)
	}

	srv, session := newCapSession(t)
	srv.SetContextConfig(config.ContextConfig{AbstainCosine: 0.62})
	saveMemory(t, session, "database configuration pooling")
	noEmbedder := searchText(t, session, "database configuration pooling")
	if !strings.Contains(noEmbedder, "abstain_cosine=not_applied") {
		t.Errorf("with no vector leg the configured cosine is neither applied nor absent, and the line "+
			"must say so rather than print a threshold no row was compared against:\n%s", noEmbedder)
	}
	if strings.Contains(noEmbedder, "abstain_cosine=off") {
		t.Errorf("a configured arm renders as off, which tells the user their key did nothing:\n%s", noEmbedder)
	}

	// And the default, with nothing configured, stays distinguishable from both.
	_, plainSession := newCapSession(t)
	saveMemory(t, plainSession, "database configuration pooling")
	if got := searchText(t, plainSession, "database configuration pooling"); !strings.Contains(got, "abstain_cosine=off") {
		t.Errorf("an unconfigured arm renders %q, want abstain_cosine=off", tail(got, 200))
	}
}

// TestTheSearchDescriptionRendersItsPunctuation: the description is a Go
// interpreted string literal, so a doubled escape reaches the client as the six
// characters \u2014 while the single escape beside it decodes to an em dash. An
// agent reading the two halves of one description gets two different renderings
// of the same punctuation, and the literal is indistinguishable from text the
// writer meant to show.
func TestTheSearchDescriptionRendersItsPunctuation(t *testing.T) {
	_, session := newCapSession(t)

	tools, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var desc string
	for _, tl := range tools.Tools {
		if tl.Name == "ghost_memory_search" {
			desc = tl.Description
		}
	}
	if desc == "" {
		t.Fatal("ghost_memory_search has no description")
	}
	if strings.Contains(desc, `\u`) {
		t.Errorf("the description ships a literal escape to the client: %q", desc)
	}
	// The same check on the instruction block, which is the OTHER literal form: it
	// is a raw string, so a \u2014 there has no decoding step and reaches the
	// client as six characters. The description is an interpreted string, so its
	// \u2014 does decode — which is exactly why one check does not cover the
	// other and the em-dash mix inside one paragraph went unnoticed.
	if strings.Contains(mcpInstructions, `\u`) {
		t.Errorf("mcpInstructions ships a literal escape to the client: %q", mcpInstructions)
	}
	if !strings.Contains(desc, "—") {
		t.Errorf("the description carries no em dash, so its dashes were escaped or lost: %q", desc)
	}
	if !strings.Contains(desc, "abstain_cosine") || !strings.Contains(desc, "verdict line") {
		t.Errorf("the description does not mention the verdict line an agent has to read, or the "+
			"threshold field on it: %q", desc)
	}
	// The description is the tool contract an agent reads before its first search,
	// so a claim in it has to hold for every outcome it can return. The clause
	// saying a weak OR EMPTY answer says "the rows were found and are not good
	// enough" is false of empty/no_candidates, where no rows were found at all
	// and the answer is absence-framed, so it has to be scoped to the reasons it
	// actually covers.
	if strings.Contains(desc, "A weak or empty answer says so in words") {
		t.Errorf("the description claims every empty answer says the rows were found, which is false of "+
			"empty/no_candidates: %q", desc)
	}
}

// TestAMaximumLengthMemoryStillComesBack: the cap is on the whole response and
// the store's own content cap is 8000 bytes, so a cap at or near that number
// makes a single long memory impossible to return — the fit pass drops the only
// row and the caller is told to raise a limit no caller can raise. The cap has
// to clear the largest thing a caller can ask for, plus the verdict framing.
func TestAMaximumLengthMemoryStillComesBack(t *testing.T) {
	_, session := newCapSession(t)
	// The largest content a writer can store, built the way the store builds it
	// rather than by arithmetic here: memory.ClampContent is TruncateUTF8 at the
	// cap PLUS the truncation marker, so a stored row reaches MaxContentLen plus
	// ~30 bytes and a fixture that stops at MaxContentLen is short by exactly the
	// marker. An earlier version of this test subtracted the marker's length from
	// a fixture stored verbatim, which put it ~60 bytes under the boundary it
	// claimed to test — the marker is the point of building it this way.
	head := "deployment rollback runbook "
	tailMark := " END-OF-MEMORY"
	body := strings.Repeat("padding text about the rollback procedure. ", 400)
	overlong := head + strings.TrimSuffix(body, " ") + tailMark
	stored, clamped := memory.ClampContent(overlong)
	if !clamped {
		t.Fatalf("precondition: the fixture is %d bytes, which the store would store whole, so it is not "+
			"the largest content a writer can produce", len(overlong))
	}
	if want := memory.MaxContentLen + len(memory.TruncationMarker()); len(stored) != want {
		t.Fatalf("stored content is %d bytes, want %d: the cap is what makes this the largest", len(stored), want)
	}
	if !strings.HasSuffix(stored, memory.TruncationMarker()) {
		t.Fatalf("precondition: the stored content does not end in the store's own truncation marker: %q",
			tail(stored, 60))
	}
	saveMemory(t, session, overlong)

	out := searchText(t, session, "deployment rollback runbook")

	if !strings.Contains(out, "outcome=answerable") {
		t.Errorf("a memory at the store's own content cap came back %s, so the response cap is "+
			"below the largest thing a caller can store:\n%s", outcomeOf(out), tail(out, 200))
	}
	// The marker's presence is the sharp half of the assertion: it is the LAST
	// bytes the store wrote, so finding it in the answer proves the whole clamped
	// row was rendered rather than the head alone fitting inside the cap. The
	// fixture's own tail text cannot do this job — the store cut the row before
	// reaching it, and a text that never reached the row proves nothing.
	if !strings.Contains(out, memory.TruncationMarker()) {
		t.Errorf("the memory's last stored bytes are missing from the answer, so the cap is smaller than "+
			"one maximum-length memory:\n%s", tail(out, 200))
	}
	if len(out) <= len(stored) {
		t.Errorf("the answer is %d bytes for %d bytes of content, which cannot be right", len(out), len(stored))
	}
	// The constant is DEFINED as twice the content cap, and pinning the formula
	// rather than a number is what stops a well-meant "just lower it a bit" from
	// quietly reintroducing the failure above: the margin is what covers the item
	// line's framing, the verdict line and the notes, and nothing in the code
	// measures those, so the relationship is the only thing a test can state.
	if searchResponseMaxBytes != 2*memory.MaxContentLen {
		t.Errorf("searchResponseMaxBytes = %d, want twice memory.MaxContentLen (%d): the cap has to clear "+
			"one maximum-length memory plus the response's framing, and 8300 is close enough to 8000 to "+
			"lose that margin on the next content that needs it",
			searchResponseMaxBytes, memory.MaxContentLen)
	}
}

// outcomeOf pulls the verdict out of a rendered answer, so a failure message can
// say which one it was rather than asking the reader to find it.
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
