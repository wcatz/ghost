package mcpserver

// #646 part 3: the retrieval-audit block inside ghost_health.
//
// The arithmetic is tested in internal/audit, and `ghost context --audit` is the
// report an operator asks for by name. What only this layer can establish is that
// the figures are REACHABLE where an agent already is: an agent debugging "search
// returns the wrong things" reads ghost_health and never runs a terminal command.
// A block nobody looks at is a block nobody reads.
//
// So this asserts four things. That each source gets its own line and the lines do
// not pool. That a source this build knows about but that has recorded nothing is
// NAMED, because before #850's passive injections write their records
// session_start and project_context have no rows and silence would read as
// health. That the block says on its face that "ignored" is not a score — the one
// word in it that an agent is most likely to misread as a judgement about a
// memory's quality. And that the whole thing is additive: no new tool, and every
// line ghost_health already printed keeps its name and its place.

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wcatz/ghost/internal/audit"
	"github.com/wcatz/ghost/internal/memory"
)

const (
	retrievalHealthMemory  = "D20E133860CC4AFE38B485AD5371BA59"
	retrievalHealthContent = "the scratch directory is reaped before each lifecycle run begins"
)

// seedRetrievalHealth builds a store with one project, one memory, two recorded
// searches — one that kept the memory and one that kept nothing — and one judged
// verdict per kept pair.
//
// The verdict is filed by running the real lifecycle audit, because the alternative
// is writing retrieval_audit rows by hand, and a fixture that hand-writes the very
// table under test proves only that the renderer prints what the fixture said. The
// hasher key is fixed so the fingerprint is the same on every run.
func seedRetrievalHealth(t *testing.T, store *memory.Store) {
	t.Helper()
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "abc123", "/tmp/test", "test-project"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, err := store.CreateWithID(ctx, "abc123", retrievalHealthMemory, memory.Memory{
		Content:  retrievalHealthContent,
		Category: "gotcha",
		Source:   "manual",
	}); err != nil {
		t.Fatalf("CreateWithID: %v", err)
	}
	for _, rec := range []memory.RetrievalRecord{
		{
			ProjectID: "abc123", SessionID: "s1", Source: "search", Outcome: "answerable",
			Verdicts: []memory.RowVerdict{{ID: retrievalHealthMemory, Kept: true, Stage: "fit", Reason: "fit_response"}},
		},
		{ProjectID: "abc123", SessionID: "s1", Source: "search", Outcome: "empty"},
	} {
		if err := store.RecordRetrieval(ctx, rec); err != nil {
			t.Fatalf("RecordRetrieval(%s): %v", rec.Outcome, err)
		}
	}
	judgeRetrievalHealth(t, store, false)
}

// judgeRetrievalHealth runs the lifecycle audit over the seeded memory, optionally
// under a degraded scan.
func judgeRetrievalHealth(t *testing.T, store *memory.Store, degraded bool) {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	hasher, err := audit.NewHasher(key)
	if err != nil {
		t.Fatalf("NewHasher: %v", err)
	}
	s := audit.NewWithHasher(hasher)
	s.AddProse(retrievalHealthContent)
	if degraded {
		s.MarkDegraded("transcript truncated")
	}
	if _, err := audit.Run(context.Background(), store, "abc123", s); err != nil {
		t.Fatalf("audit.Run: %v", err)
	}
}

// retrievalHealthServer is a server over a store the caller has already seeded.
func retrievalHealthServer(t *testing.T, store *memory.Store) (*Server, string) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	return srv, ghostHealthText(t, srv)
}

// TestHealthReportsRetrievalFiguresPerSource: the block exists, it is per source,
// and the search line carries real figures.
//
// Two memories with DIFFERENT content, one restated by the agent's prose and one
// never mentioned, so the source has a real precision rather than the degenerate
// 100% or 0% a single-memory fixture produces. The contents have to differ:
// Compare works off each memory's own wording, so two memories that say the same
// thing are one finding printed twice and the fixture would assert its own setup
// rather than the block.
func TestHealthReportsRetrievalFiguresPerSource(t *testing.T) {
	store, _ := testStoreWithPath(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "abc123", "/tmp/test", "test-project"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	const unusedID = "3F1C0A2D5B7E4918A6C0D4E2F1B3A5C7D"
	const unusedContent = "the ledger reindexes itself after a snapshot restore"
	for _, m := range []struct {
		id, content string
	}{
		{retrievalHealthMemory, retrievalHealthContent},
		{unusedID, unusedContent},
	} {
		if _, err := store.CreateWithID(ctx, "abc123", m.id, memory.Memory{
			Content:  m.content,
			Category: "gotcha",
			Source:   "manual",
		}); err != nil {
			t.Fatalf("CreateWithID(%s): %v", m.id, err)
		}
	}
	// One call, two memories kept: one is restated by the agent and one is dropped
	// on the floor, which is exactly the shape a precision has to be honest about.
	if err := store.RecordRetrieval(ctx, memory.RetrievalRecord{
		ProjectID: "abc123", SessionID: "s1", Source: "search", Outcome: "answerable",
		Verdicts: []memory.RowVerdict{
			{ID: retrievalHealthMemory, Kept: true, Stage: "fit", Reason: "fit_response"},
			{ID: unusedID, Kept: true, Stage: "fit", Reason: "fit_response"},
		},
	}); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	hasher, err := audit.NewHasher(key)
	if err != nil {
		t.Fatalf("NewHasher: %v", err)
	}
	s := audit.NewWithHasher(hasher)
	s.AddProse(retrievalHealthContent)
	if _, err := audit.Run(ctx, store, "abc123", s); err != nil {
		t.Fatalf("audit.Run: %v", err)
	}

	_, text := retrievalHealthServer(t, store)

	if !strings.Contains(text, "Retrieval") {
		t.Fatalf("ghost_health does not report retrieval figures at all:\n%s", text)
	}
	for _, want := range []string{
		"search:",
		"2 kept",
		"50% used",
		"1 ignored",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("ghost_health does not report %q:\n%s", want, text)
		}
	}
	// The sources with no rows are named rather than absent: before #850 writes
	// their records they are empty on every store in existence, and a block that
	// simply omits them is indistinguishable from a block that has never heard of
	// them.
	for _, absent := range audit.KnownSources {
		if absent == "search" {
			continue
		}
		if !strings.Contains(text, absent) {
			t.Errorf("ghost_health omits the source %q, so an empty source reads as a source this build cannot see:\n%s", absent, text)
		}
		if !strings.Contains(text, "no rows") {
			t.Errorf("ghost_health does not say that %q has no rows yet:\n%s", absent, text)
		}
	}
}

// TestHealthSaysIgnoredIsNotAScore: the single line in this block most likely to be
// misread. "80% ignored" reads as a verdict on memory quality to any agent that
// sees the number without the sentence, and the sentence is the only thing standing
// between an agent and a confident wrong claim about the store.
func TestHealthSaysIgnoredIsNotAScore(t *testing.T) {
	store, _ := testStoreWithPath(t)
	seedRetrievalHealth(t, store)

	_, text := retrievalHealthServer(t, store)

	if !strings.Contains(text, "not a relevance") && !strings.Contains(text, "not a relevance or usefulness score") {
		t.Errorf("the retrieval block does not say that \"ignored\" is not a score:\n%s", text)
	}
}

// TestHealthOnAStoreWithNoProjectsSaysSo: the block has an empty state of its own,
// and it is not "no rows". A store with no project has nothing to have made a
// retrieval, so the header would hang over nothing — which reads as a section that
// ran and found nothing to say, rather than as the absence of a subject.
func TestHealthOnAStoreWithNoProjectsSaysSo(t *testing.T) {
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	// An empty store: migrated, seeded with whatever a store carries, and holding
	// no project. The other health fixtures all start from a project, so this one
	// opens its own.
	srv := New(memory.NewStore(db, logger), logger, "test")

	text := ghostHealthText(t, srv)

	if !strings.Contains(text, "no project is registered") {
		t.Errorf("ghost_health does not say the store has no project to report on:\n%s", text)
	}
	if strings.Contains(text, "Retrieval audit** —") {
		t.Errorf("ghost_health printed a retrieval header with no source under it:\n%s", text)
	}
}

// TestHealthCountsDegradedVerdicts: a degraded verdict is still a verdict and stays
// in the denominator, so a partial read shows up as a clean figure unless it is
// counted. Hidden is the wrong answer here — an agent that reads a precise
// percentage has no way to know the transcripts behind it were truncated.
func TestHealthCountsDegradedVerdicts(t *testing.T) {
	store, _ := testStoreWithPath(t)
	seedRetrievalHealth(t, store)
	judgeRetrievalHealth(t, store, true)

	_, text := retrievalHealthServer(t, store)

	for _, want := range []string{"degraded", "transcript truncated"} {
		if !strings.Contains(text, want) {
			t.Errorf("ghost_health does not report the degraded verdict's %q:\n%s", want, text)
		}
	}
}

// TestHealthRetrievalBlockIsAdditive: ghost_health is a tool other agents parse.
// Every line it already printed keeps its name and its place, the block goes after
// the history one, and no tool is added — the retrieval figures ride the health
// tool an agent already has rather than costing a twenty-third call.
func TestHealthRetrievalBlockIsAdditive(t *testing.T) {
	store, _ := testStoreWithPath(t)
	seedRetrievalHealth(t, store)

	srv, text := retrievalHealthServer(t, store)

	for _, want := range []string{"## Ghost Health", "**Projects:**", "**Total memories:**", "**History:**"} {
		if !strings.Contains(text, want) {
			t.Errorf("the retrieval block cost ghost_health its %q line:\n%s", want, text)
		}
	}
	if history, retrieval := strings.Index(text, "**History:**"), strings.Index(text, "Retrieval audit"); history < 0 || retrieval < 0 || retrieval < history {
		t.Errorf("the retrieval block does not come after the history block, so an existing reader of this tool has to move its anchor:\n%s", text)
	}

	session := connectedClient(t, srv)
	res, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	// 22 is the count docs/mcp.md, docs/architecture.md, docs/README.md and
	// overview.html all state in prose. Nothing here registers a tool, and this
	// assertion is what proves it.
	if len(res.Tools) != 22 {
		names := make([]string, 0, len(res.Tools))
		for _, tool := range res.Tools {
			names = append(names, tool.Name)
		}
		t.Errorf("the server registers %d tools (%s), want 22 — the count the docs state", len(res.Tools), strings.Join(names, ", "))
	}
	for _, tool := range res.Tools {
		if tool.Name == "ghost_health" {
			return
		}
	}
	t.Error("ghost_health is not registered; the retrieval figures ride this tool")
}

// TestHealthNamesTheVerdictsItsFiguresDoNotAccountFor: the report's two verdicts
// that are not in any figure — one whose call fell outside the window or is gone,
// one that names no call at all — have to be stated here too.
//
// An agent reading this block cannot see the store's tables, so a figure it cannot
// reconcile is a figure it will report upstream. The ⚠ is right here even though the
// report prints these as plain notes: this is a claim about the completeness of the
// numbers, and the numbers are what a caller of this tool acts on. Both lines are
// absent when there is nothing unattributed, which is what keeps a fresh store free
// of the glyph (see TestHealthOnAStoreWithNoHistorySaysSo).
func TestHealthNamesTheVerdictsItsFiguresDoNotAccountFor(t *testing.T) {
	store, _ := testStoreWithPath(t)
	seedRetrievalHealth(t, store)
	judgeRetrievalHealth(t, store, false)

	// One verdict against the recorded call (attributed, counted), and one naming no
	// call at all — the shape the write accepts for a verdict about a session.
	recs, err := store.RetrievalRecordsForProject(context.Background(), "abc123", 0)
	if err != nil || len(recs) == 0 {
		t.Fatalf("RetrievalRecordsForProject: %v (%d records)", err, len(recs))
	}
	if err := store.RecordRetrievalAudits(context.Background(), []memory.RetrievalAuditRow{{
		ProjectID: "abc123", SessionID: "s9", Source: "search", MemoryID: retrievalHealthMemory,
		Outcome: "ignored", RecordRowID: 0,
	}}); err != nil {
		t.Fatalf("RecordRetrievalAudits: %v", err)
	}

	_, text := retrievalHealthServer(t, store)

	if !strings.Contains(text, "name no call at all") {
		t.Errorf("ghost_health does not name the unattributed verdict:\n%s", text)
	}
	// The unattributed one is counted, so it appears in the figures AND in the note.
	if !strings.Contains(text, "(1 of 2 scored)") {
		t.Errorf("ghost_health did not count the unattributed verdict in the denominator:\n%s", text)
	}
}

// TestHealthSaysNothingAboutAttributionWhenThereIsNothingToSay: the same block on a
// store whose verdicts are all attributed. Two extra lines on every store an agent
// checks for health is how the ⚠ glyph stops meaning anything.
func TestHealthSaysNothingAboutAttributionWhenThereIsNothingToSay(t *testing.T) {
	store, _ := testStoreWithPath(t)
	seedRetrievalHealth(t, store)
	judgeRetrievalHealth(t, store, false)

	recs, err := store.RetrievalRecordsForProject(context.Background(), "abc123", 0)
	if err != nil || len(recs) == 0 {
		t.Fatalf("RetrievalRecordsForProject: %v", err)
	}
	if err := store.RecordRetrievalAudits(context.Background(), []memory.RetrievalAuditRow{{
		ProjectID: "abc123", SessionID: "s9", Source: "search", MemoryID: retrievalHealthMemory,
		Outcome: "ignored", RecordRowID: recs[0].RowID,
	}}); err != nil {
		t.Fatalf("RecordRetrievalAudits: %v", err)
	}

	_, text := retrievalHealthServer(t, store)

	for _, unwanted := range []string{"name no call at all", "were not counted"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("ghost_health reports %q on a store with nothing unattributed:\n%s", unwanted, text)
		}
	}
}
