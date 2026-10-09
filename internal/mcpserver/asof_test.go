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

// The instants the as_of tests read at. The store under a test is written now, so
// a PAST instant can only see memories that did not exist then and a FUTURE one
// can see everything. That is the whole point of the pair: the past read must
// not fall back to the present rows, and the future read must return the same
// thing a current read does.
const (
	asOfToolPast   = "2020-01-01T00:00:00Z"
	asOfToolFuture = "2035-01-01T00:00:00Z"
)

// TestSearchAsOfDisclosesThatItIsAPastReading: the disclosure is the feature on
// this surface. A block assembled at an instant looks exactly like a block
// assembled now unless the answer says otherwise, and an agent that believes it
// is looking at the present will contradict a memory that was true then and is
// not now — which is the failure this whole change exists to prevent.
func TestSearchAsOfDisclosesThatItIsAPastReading(t *testing.T) {
	_, session := newCapSession(t)

	res := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project",
		"content":    "the retention sweep runs at four in the morning",
		"category":   "fact",
	})
	if res.IsError {
		t.Fatalf("save: %s", resultText(res))
	}

	out := resultText(callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "retention sweep",
		"as_of":      asOfToolFuture,
	}))
	if !strings.Contains(out, "retention sweep runs at four") {
		t.Fatalf("a future as_of did not return the memory:\n%s", out)
	}
	for _, want := range []string{"as_of " + asOfToolFuture, "historical read", "keyword-only", "no vector leg ran"} {
		if !strings.Contains(out, want) {
			t.Errorf("the answer does not say %q, so a reader cannot tell this is a past reading:\n%s", want, out)
		}
	}
	// The qualifier leads rather than trails: a reader who stops at the first
	// line has to have met it.
	if idx := strings.Index(out, "as_of "+asOfToolFuture); idx > strings.Index(out, "- [") {
		t.Errorf("the historical note appears after the rows, want it before them:\n%s", out)
	}
}

// TestSearchAsOfInThePastDoesNotAnswerWithThePresent: the safety property. A past
// instant is answered from the recorded versions, and when nothing was recorded
// then the answer is an absence — never the current text, which is the one answer
// that would make the tool look like it is doing historical retrieval while
// telling the reader about today.
func TestSearchAsOfInThePastDoesNotAnswerWithThePresent(t *testing.T) {
	_, session := newCapSession(t)

	res := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project",
		"content":    "the retention sweep runs at four in the morning",
		"category":   "fact",
	})
	if res.IsError {
		t.Fatalf("save: %s", resultText(res))
	}

	out := resultText(callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "retention sweep",
		"as_of":      asOfToolPast,
	}))
	if strings.Contains(out, "retention sweep runs at four") {
		t.Errorf("a past as_of returned the current text:\n%s", out)
	}
	if !strings.Contains(out, "as_of "+asOfToolPast) {
		t.Errorf("an empty historical answer carries no historical note, so the absence reads as an absence from the store:\n%s", out)
	}
	// The same question without as_of still answers, which is what makes the
	// empty historical answer a fact about the instant rather than a broken tool.
	current := resultText(callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "retention sweep",
	}))
	if !strings.Contains(current, "retention sweep runs at four") {
		t.Errorf("the same question without as_of found nothing:\n%s", current)
	}
	if strings.Contains(current, "historical read") {
		t.Errorf("a current answer carries a historical note:\n%s", current)
	}
}

// TestSearchRefusesAnInstantItCannotRead: an as_of the caller could not spell is
// a mistake, and the tool says so. Answering anyway would mean answering with the
// present, under a request that asked for a past instant — the one outcome that
// cannot be detected from the answer.
func TestSearchRefusesAnInstantItCannotRead(t *testing.T) {
	_, session := newCapSession(t)
	for _, bad := range []string{"yesterday", "2020-01-01", "2020-01-01 00:00:00", "2035-01-01"} {
		out := resultText(callTool(t, session, "ghost_memory_search", map[string]any{
			"project_id": "test-project",
			"query":      "anything",
			"as_of":      bad,
		}))
		if !strings.Contains(out, "RFC 3339") {
			t.Errorf("as_of %q was answered with %q, want a refusal naming the layout it could not read", bad, out)
		}
	}
}

// TestProjectContextAsOfRendersTheHistoricalBlock: the second surface. A project
// listing at an instant has to say which instant, and it has to leave out the
// halves that have no history rather than printing today's of them under a
// heading that declares the block a past reading.
func TestProjectContextAsOfRendersTheHistoricalBlock(t *testing.T) {
	_, session := newCapSession(t)

	res := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project",
		"content":    "the retention sweep runs at four in the morning",
		"category":   "fact",
	})
	if res.IsError {
		t.Fatalf("save: %s", resultText(res))
	}

	out := resultText(callTool(t, session, "ghost_project_context", map[string]any{
		"project_id": "test-project",
		"as_of":      asOfToolFuture,
	}))
	if !strings.Contains(out, "as_of "+asOfToolFuture) {
		t.Errorf("the historical block does not name its instant:\n%s", out)
	}
	if !strings.Contains(out, "retention sweep runs at four") {
		t.Errorf("a future as_of did not return the memory:\n%s", out)
	}
	// The closing instruction is about the present, and this block is not.
	if strings.Contains(out, "Save new discoveries with ghost_memory_save") {
		t.Errorf("the historical block ends with the session instruction, which aims the reader at the present:\n%s", out)
	}
	if !strings.Contains(out, "are not versioned") {
		t.Errorf("the historical block does not say that the unversioned halves are omitted:\n%s", out)
	}
	if strings.Contains(out, "## Learned Context") {
		t.Errorf("the historical block carries a learned context derived from today's memories:\n%s", out)
	}
	// The global half is empty (the fixture saved no _global row), so it renders
	// no heading at all: a disclosure appended to the empty half would turn it
	// into a heading over nothing, which reads as a claim about the project's
	// cross-project rows.
	if strings.Contains(out, globalSectionHeading) {
		t.Errorf("the historical block rendered an empty %q section:\n%s", globalSectionHeading, out)
	}

	// A current listing is unchanged: no historical note, and no omission notice
	// either — both of those are statements about a past reading.
	current := resultText(callTool(t, session, "ghost_project_context", map[string]any{
		"project_id": "test-project",
	}))
	if strings.Contains(current, "historical read") {
		t.Errorf("a current project listing carries a historical note:\n%s", current)
	}
	if strings.Contains(current, "are not versioned") {
		t.Errorf("a current project listing carries the historical omission notice:\n%s", current)
	}
}

// TestProjectContextAsOfInThePastReportsAnAbsentProjectSetRatherThanAnEmptyOne:
// the store has memories, so "nothing to show" is not the honest reading — the
// honest one is that nothing was recorded at that instant. The block says so
// through the same note a search would.
func TestProjectContextAsOfInThePastReportsAnAbsentProjectSetRatherThanAnEmptyOne(t *testing.T) {
	_, session := newCapSession(t)

	res := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project",
		"content":    "the retention sweep runs at four in the morning",
		"category":   "fact",
	})
	if res.IsError {
		t.Fatalf("save: %s", resultText(res))
	}

	out := resultText(callTool(t, session, "ghost_project_context", map[string]any{
		"project_id": "test-project",
		"as_of":      asOfToolPast,
	}))
	if strings.Contains(out, "retention sweep runs at four") {
		t.Errorf("a past as_of listed the current memory:\n%s", out)
	}
	if !strings.Contains(out, "as_of "+asOfToolPast) {
		t.Errorf("the block does not say which instant it read:\n%s", out)
	}
	if strings.Contains(out, "is not registered with Ghost yet") {
		t.Errorf("a past read reported the project as unregistered, want a reading of the instant:\n%s", out)
	}
}

// TestAsOfToolArgumentIsOptional keeps the default path honest: the new argument
// is additive, and a request that omits it must behave exactly as it did before
// this change.
func TestAsOfToolArgumentIsOptional(t *testing.T) {
	_, session := newCapSession(t)
	res := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "retention",
		"as_of":      "",
	})
	if res.IsError {
		t.Fatalf("an empty as_of was refused: %s", resultText(res))
	}
	if strings.Contains(resultText(res), "as_of") {
		t.Errorf("an empty as_of produced a historical note:\n%s", resultText(res))
	}
}

// TestSearchExplainRefusedWithAsOf: explain is a projection of the CURRENT
// ranking, over the search index and the live vectors, and an as_of read ranks
// nothing. Answering it for a historical request would return a present-day
// ranking with no qualifier and no trace to say so, which is the one outcome a
// caller cannot detect from the payload.
func TestSearchExplainRefusedWithAsOf(t *testing.T) {
	_, session := newCapSession(t)
	out := resultText(callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "anything",
		"as_of":      asOfToolFuture,
		"explain":    true,
	}))
	if !strings.Contains(out, "explain cannot describe a historical") {
		t.Errorf("explain with as_of returned %q, want a refusal: the payload is a present-day ranking", out)
	}
	// explain alone is untouched.
	plain := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "anything",
		"explain":    true,
	})
	if plain.IsError {
		t.Errorf("explain without as_of was refused: %s", resultText(plain))
	}
}

// TestAsOfParsesToUTC pins the rendering the surfaces print, so a zone-carrying
// input cannot make two surfaces label the same instant differently.
func TestAsOfParsesToUTC(t *testing.T) {
	got, err := parseAsOf("2026-09-20T11:00:00+02:00")
	if err != nil {
		t.Fatalf("parseAsOf: %v", err)
	}
	if got == nil {
		t.Fatal("parseAsOf returned nil for a valid instant")
	}
	if want := "2026-09-20T09:00:00Z"; got.UTC().Format(time.RFC3339) != want {
		t.Errorf("parseAsOf normalised to %s, want %s", got.UTC().Format(time.RFC3339), want)
	}
	if empty, err := parseAsOf(""); err != nil || empty != nil {
		t.Errorf("parseAsOf(\"\") = %v, %v; want nil, nil: an omitted argument is a current read, not an error", empty, err)
	}
}

// backdatedSession wires a fully-registered server over a store the test can
// write raw SQL against. It is the only way a PAST instant can be asked about a
// row a test just wrote: every save stamps its version with the store's own
// clock, so a historical read at an instant before that sees nothing unless the
// version's recorded_at is moved — which is exactly what a store whose history
// predates the read would hold, and what makes the windows below the live row's
// rather than the version's.
func backdatedSession(t *testing.T) (*mcp.ClientSession, *sql.DB) {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := memory.NewStore(db, logger)
	if err := store.EnsureProject(context.Background(), "abc123", "/tmp/test", "test-project"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return connectedClient(t, New(store, logger, "test")), db
}

// insertHistoricalRow writes one memory with a version recorded before at, and the
// validity window the live row holds. The two are deliberately independent: the
// version is what puts the row in a historical read, and the window is the field
// that read has to borrow because memory_history records none.
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
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source, valid_from, valid_until, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, "abc123", "fact", content, "manual", nullable(from), nullable(until),
		stamp(-72*time.Hour), stamp(-24*time.Hour),
	); err != nil {
		t.Fatalf("insert %s: %v", id, err)
	}
	if _, err := db.Exec(
		`INSERT INTO memory_history (memory_id, project_id, category, content, source, recorded_at, phase)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, "abc123", "fact", content, "manual", stamp(-24*time.Hour), "save",
	); err != nil {
		t.Fatalf("insert %s history: %v", id, err)
	}
}

// TestSearchAsOfKeepsARowWhoseCurrentWindowIsClosedOrUnopened is #910 through the
// tool, over a store whose history predates the instant asked about — so the row
// really was there at T, and the only thing that could exclude it is the window
// the read borrows from the live row.
//
// Both rows are returned at T, with the note that says the window shown is
// today's. The same two rows are dropped by a current search, because today the
// window really is closed and really is not open: the change is scoped to a
// historical read, and it is the read that no longer draws the verdict.
func TestSearchAsOfKeepsARowWhoseCurrentWindowIsClosedOrUnopened(t *testing.T) {
	at := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	now := time.Now().UTC()
	session, db := backdatedSession(t)
	f := memory.StoredStampLayout
	closedSince := at.Add(90 * 24 * time.Hour).Format(f) // after T, before now
	opensLater := now.Add(90 * 24 * time.Hour).Format(f) // after now
	insertHistoricalRow(t, db, "m-closed-since", "walrus window closed since T",
		strPtr(at.Add(-90*24*time.Hour).Format(f)), &closedSince, at)
	insertHistoricalRow(t, db, "m-opens-after-now", "walrus window opens after now",
		strPtr(opensLater), nil, at)

	historical := resultText(callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project", "query": "walrus", "as_of": at.Format(time.RFC3339),
	}))
	for _, content := range []string{"walrus window closed since T", "walrus window opens after now"} {
		if !strings.Contains(historical, content) {
			t.Errorf("an as_of search withheld %q, want it returned: the window is the live row's and is not "+
				"evidence about that instant\n%s", content, historical)
		}
	}
	if !strings.Contains(historical, "Validity judged at "+at.Format(time.RFC3339)) {
		t.Errorf("the answer does not state the as_of validity note, so the window its rows show reads as the "+
			"row's own at that instant:\n%s", historical)
	}
	for _, content := range []string{"walrus window closed since T", "walrus window opens after now"} {
		line := searchLine(historical, content)
		if line == "" {
			continue
		}
		for _, verdict := range []string{"expired", "not yet valid"} {
			if strings.Contains(line, verdict) {
				t.Errorf("%q is labelled %q on a row the stage drew no verdict for: %s", content, verdict, line)
			}
		}
	}

	// The same two rows today: the window really has closed and really is not
	// open, so a current search drops both, as it always did.
	current := resultText(callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project", "query": "walrus",
	}))
	for _, content := range []string{"walrus window closed since T", "walrus window opens after now"} {
		if strings.Contains(current, content) {
			t.Errorf("a current search returned %q, want it withheld as out of window today:\n%s", content, current)
		}
	}
}
