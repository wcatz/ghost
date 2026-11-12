package mcpserver

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

// testStore creates an in-memory Store suitable for testing.
func testStore(t *testing.T) *memory.Store {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := memory.NewStore(db, logger)

	ctx := context.Background()
	if err := s.EnsureProject(ctx, "abc123", "/tmp/test", "test-project"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return s
}

func TestGhostTaskCreate_RejectsUnknownProject(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)

	ctx := context.Background()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_task_create",
		Arguments: map[string]any{"project_id": "nonexistent-project", "title": "should not be created"},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_task_create: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected error result for unknown project, got: %+v", result.Content)
	}

	tasks, err := store.ListTasks(ctx, "", "", 10)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("task must not be persisted against an empty project id, found %d", len(tasks))
	}
}

// TestGhostDecisionRecord_OpensAnUnknownProject is #956 from the tool's own
// surface: a decision recorded against a project the store has never seen is not
// refused. It used to be — the handler answered `project "<id>" not found` and
// wrote nothing, so an agent's first decision on a fresh store failed and it then
// recorded the decision under the global scope instead.
//
// Both reads are SCOPED, and the previous version of this test was scoped to no
// project at all: `ListDecisions` binds `project_id = ?`, so its `""` returned
// nothing whatever the tool had written, and "must not be persisted" was an
// assertion that could not fail. The two reads here are the one project the
// decision should be in and the one it should not.
func TestGhostDecisionRecord_OpensAnUnknownProject(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)

	ctx := context.Background()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "ghost_decision_record",
		Arguments: map[string]any{
			"project_id": "nonexistent-project",
			"title":      "should be recorded",
			"decision":   "some decision",
			"rationale":  "some rationale",
		},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_decision_record: %v", err)
	}
	if result.IsError {
		t.Fatalf("a decision for an unknown project was refused: %+v", result.Content)
	}

	projects, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	var opened bool
	for _, p := range projects {
		if p.ID == "nonexistent-project" {
			opened = true
		}
	}
	if !opened {
		t.Errorf("the decision did not open a project for the id it was given; the store holds %d project(s)",
			len(projects))
	}

	decisions, err := store.ListDecisions(ctx, "nonexistent-project", "", 10)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(decisions) != 1 {
		t.Fatalf("the opened project holds %d decision(s), want the one just recorded", len(decisions))
	}
	// And nothing was filed under the project the fixture already holds, which is
	// the shape of the workaround this change removes: an agent refused here used
	// to write the decision into whichever project did resolve.
	if elsewhere, err := store.ListDecisions(ctx, "abc123", "", 10); err != nil {
		t.Fatalf("ListDecisions(abc123): %v", err)
	} else if len(elsewhere) != 0 {
		t.Errorf("the decision landed in %d row(s) under an existing project, want 0", len(elsewhere))
	}
}

func TestNew_CreatesServer(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	srv := New(store, logger, "test")
	if srv == nil {
		t.Fatal("New returned nil")
	}
	if srv.store == nil {
		t.Error("server store is nil")
	}
	if srv.mcp == nil {
		t.Error("server mcp is nil")
	}
}

type mockEmbedder struct{}

func (m *mockEmbedder) EmbedQuery(_ context.Context, _ string) ([]float32, error) {
	return []float32{0.1, 0.2, 0.3}, nil
}

// --- Resource tests ---

// --- Store-backed tool logic tests ---
// These test the core logic paths that MCP tool handlers exercise,
// using real in-memory SQLite to verify end-to-end behavior.

func TestSaveAndSearch_EndToEnd(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	// Save a memory via store (simulating ghost_memory_save logic).
	id, dupOf, _, err := store.Upsert(ctx, "abc123", "pattern", "use context.Background() in tests", "mcp", 0.7, []string{"testing"})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if id == "" {
		t.Error("expected non-empty ID")
	}
	if dupOf != "" {
		t.Error("first save should not report a duplicate")
	}

	// Search via FTS (simulating ghost_memory_search without embedder).
	results, err := store.SearchHybrid(ctx, "abc123", "context Background", nil, 10)
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	if len(results) == 0 {
		t.Error("expected at least one search result")
	}
}

func TestDeleteMemory_EndToEnd(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	id, _, _, err := store.Upsert(ctx, "abc123", "gotcha", "watch for nil pointers", "mcp", 0.5, []string{})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Verify it exists.
	all, _ := store.GetAll(ctx, "abc123", 100)
	found := false
	for _, m := range all {
		if m.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatal("memory not found after upsert")
	}

	// Delete it.
	if err := store.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Verify gone.
	all, _ = store.GetAll(ctx, "abc123", 100)
	for _, m := range all {
		if m.ID == id {
			t.Error("memory should be deleted")
		}
	}
}

func TestSaveGlobal_EndToEnd(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	// Ensure _global project.
	if err := store.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatal(err)
	}

	id, _, _, err := store.Upsert(ctx, "_global", "preference", "always use nerdctl", "mcp", 0.8, []string{})
	if err != nil {
		t.Fatalf("Upsert global: %v", err)
	}
	if id == "" {
		t.Error("expected non-empty ID")
	}

	// Verify retrievable.
	mems, err := store.GetTopMemories(ctx, "_global", 50)
	if err != nil {
		t.Fatalf("GetTopMemories: %v", err)
	}
	if len(mems) == 0 {
		t.Error("expected at least one global memory")
	}
}

func TestTaskLifecycle(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	// Create task.
	id, err := store.CreateTask(ctx, "abc123", "Fix the bug", "Segfault in main.go", 1)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if id == "" {
		t.Error("expected task ID")
	}

	// List tasks.
	tasks, err := store.ListTasks(ctx, "abc123", "", 30)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].Title != "Fix the bug" {
		t.Errorf("title = %q", tasks[0].Title)
	}

	// Complete task.
	if _, err := store.CompleteTask(ctx, id, "Fixed in commit abc"); err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}

	// List done tasks.
	done, err := store.ListTasks(ctx, "abc123", "done", 30)
	if err != nil {
		t.Fatalf("ListTasks done: %v", err)
	}
	if len(done) != 1 {
		t.Errorf("expected 1 done task, got %d", len(done))
	}
}

func TestDecisionRecord_EndToEnd(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	id, memID, _, err := store.RecordDecision(ctx, "abc123", "Use SQLite", "Embedded DB for simplicity", "No CGO dependency", []string{"PostgreSQL", "MySQL"}, []string{"database"})
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	if id == "" {
		t.Error("expected decision ID")
	}
	if memID == "" || memID == id {
		t.Errorf("expected distinct non-empty memory ID, got %q (decision ID: %q)", memID, id)
	}

	decisions, err := store.ListDecisions(ctx, "abc123", "", 10)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(decisions) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(decisions))
	}
	if decisions[0].Title != "Use SQLite" {
		t.Errorf("title = %q", decisions[0].Title)
	}
}

func TestHealthOutput(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	// Seed memories with very distinct content to avoid Upsert merge.
	if _, _, _, err := store.Upsert(ctx, "abc123", "fact", "Ghost uses SQLite with FTS5 for full-text search capabilities", "mcp", 0.5, []string{}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.Upsert(ctx, "abc123", "convention", "Kubernetes manifests use helmfile for declarative deployment management", "mcp", 0.6, []string{}); err != nil {
		t.Fatal(err)
	}

	// Count memories.
	count, err := store.CountMemories(ctx, "abc123")
	if err != nil {
		t.Fatalf("CountMemories: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 memories, got %d", count)
	}

	// List projects.
	projects, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 1 {
		t.Errorf("expected 1 project, got %d", len(projects))
	}
}

func TestFormatMemories_EdgeCases(t *testing.T) {
	// Multiple memories with different features.
	mems := []memory.Memory{
		{Category: "fact", Importance: 0.5, Content: "plain memory"},
		{Category: "decision", Importance: 1.0, Content: "critical decision", Pinned: true, Tags: []string{"arch"}},
		{Category: "gotcha", Importance: 0.3, Content: "minor gotcha"},
	}
	result := formatMemories(mems)

	if !strings.Contains(result, "[fact]") || !strings.Contains(result, "[decision]") || !strings.Contains(result, "[gotcha]") {
		t.Errorf("expected all categories in output: %s", result)
	}
	if !strings.Contains(result, "[pinned]") {
		t.Error("expected [pinned] marker")
	}
	if !strings.Contains(result, `tags:["arch"]`) {
		t.Errorf("expected tags in output: %s", result)
	}
	// Each memory on its own line.
	lines := strings.Split(strings.TrimSpace(result), "\n")
	if len(lines) != 3 {
		t.Errorf("expected 3 lines, got %d", len(lines))
	}
}

// TestFormatMemoriesAtJudgesValidityAtTheRequestedInstant fixes the rule a
// historical listing shares with ghost_memory_search (#899): validity is judged AT
// the requested instant. A row valid at T and closed since is shown as valid at
// T; a row whose window had closed, or not yet opened, at T is withheld; a bound
// exactly at T is inside the window; an open-ended or unreadable-bound row is
// kept with no verdict.
func TestFormatMemoriesAtJudgesValidityAtTheRequestedInstant(t *testing.T) {
	at := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	f := memory.StoredStampLayout
	stamp := func(d time.Duration) *string { return strPtr(at.Add(d).Format(f)) }
	verified := stamp(-time.Hour)

	mk := func(id, content string, from, until, ver *string) memory.Memory {
		return memory.Memory{ID: id, Category: "fact", Importance: 0.7, Content: content, ValidFrom: from, ValidUntil: until, VerifiedAt: ver}
	}
	mems := []memory.Memory{
		mk("mem1", "valid at T, closed since", stamp(-48*time.Hour), stamp(24*time.Hour), verified),
		mk("mem2", "opens after T", stamp(24*time.Hour), stamp(72*time.Hour), verified),
		mk("mem3", "closed before T", stamp(-72*time.Hour), stamp(-time.Hour), verified),
		mk("mem4", "open ended", nil, nil, nil),
		mk("mem5", "ends exactly at T", stamp(-48*time.Hour), stamp(0), verified),
		mk("mem6", "starts exactly at T", stamp(0), stamp(48*time.Hour), verified),
		mk("mem7", "unreadable bound", nil, strPtr("not a date"), nil),
		mk("mem8", "unverified window", stamp(-time.Hour), nil, nil),
	}
	result := formatMemoriesAt(mems, at)

	for _, id := range []string{"`mem2`", "`mem3`"} {
		if memoryMetaGroup(result, id) != "" {
			t.Errorf("%s is outside its window at T but is listed:\n%s", id, result)
		}
	}
	for _, id := range []string{"`mem1`", "`mem5`", "`mem6`"} {
		got := memoryMetaGroup(result, id)
		if got == "" {
			t.Errorf("%s is inside its window at T but is missing:\n%s", id, result)
		}
		if strings.Contains(got, "expired") || strings.Contains(got, "not yet valid") || strings.Contains(got, "unverified") {
			t.Errorf("%s is valid at T but is labelled %q", id, got)
		}
	}
	if got := memoryMetaGroup(result, "`mem1`"); !strings.Contains(got, "until") {
		t.Errorf("the window of a row valid at T is not shown: %q", got)
	}
	for _, id := range []string{"`mem4`", "`mem7`"} {
		got := memoryMetaGroup(result, id)
		if !strings.Contains(result, id) {
			t.Errorf("%s states no readable window but is missing", id)
		}
		for _, bad := range []string{"valid from", "until", "expired", "not yet valid", "unverified"} {
			if strings.Contains(got, bad) {
				t.Errorf("%s carries a validity claim %q: %q", id, bad, got)
			}
		}
	}
	if got := memoryMetaGroup(result, "`mem8`"); !strings.Contains(got, "unverified") {
		t.Errorf("an unverified window lost its marker: %q", got)
	}
}

// TestWithholdInvalidAtMatchesTheListingFormatter: the handler withholds before
// the limit and the formatter withholds again defensively; both go through
// memory.ValidityAt, so they must not disagree on which rows survive.
func TestWithholdInvalidAtMatchesTheListingFormatter(t *testing.T) {
	at := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	f := memory.StoredStampLayout
	open := strPtr(at.Add(24 * time.Hour).Format(f))
	closed := strPtr(at.Add(-24 * time.Hour).Format(f))
	rows := []memory.AsOfRow{
		{Memory: memory.Memory{ID: "keep", Category: "fact", Content: "kept", ValidUntil: open}},
		{Memory: memory.Memory{ID: "drop", Category: "fact", Content: "dropped", ValidUntil: closed}},
		{Memory: memory.Memory{ID: "future", Category: "fact", Content: "later", ValidFrom: open}},
	}
	kept := withholdInvalidAt(rows, at)
	if len(kept) != 1 || kept[0].ID != "keep" {
		t.Fatalf("withholdInvalidAt kept %v, want only keep", kept)
	}
	mems := make([]memory.Memory, 0, len(rows))
	for _, r := range rows {
		mems = append(mems, r.Memory)
	}
	out := formatMemoriesAt(mems, at)
	if !strings.Contains(out, "kept") || strings.Contains(out, "dropped") || strings.Contains(out, "later") {
		t.Errorf("the formatter disagrees with withholdInvalidAt:\n%s", out)
	}
}

// TestAsOfSearchReturnsWhatTheListingWithholdsAtT is #910: the two as_of surfaces
// deliberately no longer reach the same verdict, and this is the test that pins
// the difference. Rows are saved through the tools with windows placed around T,
// then ghost_project_context and ghost_memory_search are asked at T.
//
// The listing withholds a row whose window had closed or had not yet opened at T,
// exactly as it did before (#899), and still says validity was judged at T. Search
// returns it: the bounds it would have judged are the PRESENT's, because
// memory_history records no validity, so they are not evidence about T — the
// reason a row whose window has closed since, or has not opened yet, used to be
// dropped from a past answer on the strength of a bound the row did not hold then.
// It says so with the shared note rather than with a verdict word on the line, and
// the rows both surfaces agree on are still agreed on.
func TestAsOfSearchReturnsWhatTheListingWithholdsAtT(t *testing.T) {
	_, session := newCapSession(t)
	const at = "2035-01-01T00:00:00Z"
	rows := []struct {
		content    string
		from, till string
		inWindow   bool
	}{
		{"zebra valid across T", "2034-06-01T00:00:00Z", "2036-01-01T00:00:00Z", true},
		{"zebra closed before T", "", "2034-06-01T00:00:00Z", false},
		{"zebra opens after T", "2036-01-01T00:00:00Z", "", false},
		{"zebra ends exactly at T", "", at, true},
		{"zebra starts exactly at T", at, "2036-06-01T00:00:00Z", true},
		{"zebra open ended", "", "", true},
	}
	for _, r := range rows {
		args := map[string]any{"project_id": "test-project", "content": r.content, "category": "fact"}
		if r.from != "" {
			args["valid_from"] = r.from
		}
		if r.till != "" {
			args["valid_until"] = r.till
		}
		if res := callTool(t, session, "ghost_memory_save", args); res.IsError {
			t.Fatalf("save %q: %s", r.content, resultText(res))
		}
	}

	listing := resultText(callTool(t, session, "ghost_project_context", map[string]any{
		"project_id": "test-project", "as_of": at,
	}))
	// An explicit limit, because the default cap (5) is smaller than the six
	// validity shapes this fixture saves and the assertion below is about every
	// row being returned, not about the cap.
	search := resultText(callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project", "query": "zebra", "as_of": at, "limit": 10,
	}))
	if !strings.Contains(listing, "Validity judged at "+at) {
		t.Errorf("the listing does not say validity was judged at T:\n%s", listing)
	}
	want := memory.AsOfBorrowedWindowNote(asOfInstant(t, at))
	if !strings.Contains(search, want) {
		t.Errorf("the search does not state that the bounds its rows show are the current row's, want:\n%s\n%s",
			want, search)
	}
	// The listing's sentence, which asserts a verdict this read did not draw.
	if strings.Contains(search, "Validity judged at") {
		t.Errorf("the search answer claims validity was judged at T, a judgement this read did not make:\n%s", search)
	}
	for _, r := range rows {
		inListing := strings.Contains(listing, r.content)
		inSearch := strings.Contains(search, r.content)
		if inListing != r.inWindow {
			t.Errorf("%q: listing=%v, want %v (inside its window at T): the listing's rule is unchanged",
				r.content, inListing, r.inWindow)
		}
		if !inSearch {
			t.Errorf("%q: an as_of search withheld the row, want it returned: the window is the live row's and "+
				"is not evidence about that instant\n%s", r.content, search)
			continue
		}
		if line := searchLine(search, r.content); !r.inWindow && line != "" {
			for _, verdict := range []string{"expired", "not yet valid"} {
				if strings.Contains(line, verdict) {
					t.Errorf("%q is labelled %q on a row the stage drew no verdict for: %s", r.content, verdict, line)
				}
			}
		}
	}
}

// asOfInstant parses the RFC 3339 constant the as_of tests read at, so an
// assertion on a rendered note names the same instant the tool was asked about
// rather than a second spelling of it.
func asOfInstant(t *testing.T, at string) time.Time {
	t.Helper()
	got, err := time.Parse(time.RFC3339, at)
	if err != nil {
		t.Fatalf("parse %q: %v", at, err)
	}
	return got
}

// searchLine returns the rendered row line of a search answer whose content is
// content, or "" when the answer holds none. It is the line the answer shows,
// not the block's disclosure, so a verdict word found here is a verdict on a row
// the reader was handed.
func searchLine(answer, content string) string {
	for _, line := range strings.Split(answer, "\n") {
		if strings.Contains(line, content) && strings.HasPrefix(line, "- [") {
			return line
		}
	}
	return ""
}

// memoryMetaGroup returns the parenthesized metadata group on the result line
// whose id marker appears in marker — the text from the last '(' before the «
// data delimiter to the first ')' after it, which is where formatMemoriesInternal
// puts importance, the scope label and the validity window.
func memoryMetaGroup(result, marker string) string {
	for _, line := range strings.Split(result, "\n") {
		if !strings.Contains(line, marker) {
			continue
		}
		if idx := strings.LastIndex(line, "("); idx >= 0 {
			if end := strings.Index(line[idx:], ")"); end >= 0 {
				return line[idx : idx+end+1]
			}
		}
		return ""
	}
	return ""
}

// TestFormatMemoriesAsOf_EmptyListingCarriesNoDisclosure is the empty half of
// the same property: projectContextSection writes a heading only for a non-empty
// body, so a disclosure appended to no rows would render a heading over nothing
// — an empty `## Global (applies to all projects)` reads as a claim about the
// project's cross-project rows. An empty listing is therefore empty, not a
// disclosure.
func TestFormatMemoriesAsOf_EmptyListingCarriesNoDisclosure(t *testing.T) {
	at := time.Date(2026, 10, 5, 22, 20, 24, 0, time.UTC)
	if got := formatMemoriesAt(nil, at); got != "" {
		t.Errorf("formatMemoriesAt with no rows = %q, want empty: a disclosure here is a heading over no rows", got)
	}
	// And through the section writer, which is where the heading is decided.
	var sb strings.Builder
	projectContextSection(&sb, globalSectionHeading, formatMemoriesAt(nil, at))
	if strings.Contains(sb.String(), globalSectionHeading) {
		t.Errorf("an empty historical half rendered %q, want no heading:\n%s", globalSectionHeading, sb.String())
	}
}

func TestDefaultImportance(t *testing.T) {
	f := func(v float32) *float32 { return &v }

	tests := []struct {
		name     string
		p        *float32
		fallback float32
		want     float32
	}{
		{"nil defaults", nil, 0.7, 0.7},
		{"explicit zero", f(0), 0.7, 0},
		{"normal value", f(0.5), 0.7, 0.5},
		{"clamp high", f(2.0), 0.7, 1.0},
		{"clamp negative", f(-1), 0.7, 0},
		{"max value", f(1.0), 0.7, 1.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := defaultImportance(tt.p, tt.fallback)
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func testServer(t *testing.T) *Server {
	t.Helper()
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return &Server{store: store, logger: logger}
}

func TestApplyMemoryUpdate(t *testing.T) {
	srv := testServer(t)
	ctx := context.Background()

	id, err := srv.store.Create(ctx, "abc123", memory.Memory{
		Category: "fact", Content: "original", Source: "mcp", Importance: 0.7, Tags: []string{},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	t.Run("updates content and reports changed fields", func(t *testing.T) {
		msg, err := srv.applyMemoryUpdate(ctx, nil, updateArgs{
			ProjectID: "test-project", MemoryID: id, Content: "corrected",
		})
		if err != nil {
			t.Fatalf("applyMemoryUpdate: %v", err)
		}
		if !strings.Contains(msg, "content") {
			t.Errorf("message should name changed field, got %q", msg)
		}
		mems, _ := srv.store.GetByIDs(ctx, []string{id})
		if mems[0].Content != "corrected" {
			t.Errorf("content = %q, want corrected", mems[0].Content)
		}
	})

	t.Run("rejects wrong project", func(t *testing.T) {
		if err := srv.store.EnsureProject(ctx, "other", "/tmp/other", "other"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		_, err := srv.applyMemoryUpdate(ctx, nil, updateArgs{
			ProjectID: "other", MemoryID: id, Content: "hijack",
		})
		if err == nil {
			t.Error("expected ownership rejection")
		}
		mems, _ := srv.store.GetByIDs(ctx, []string{id})
		if mems[0].Content == "hijack" {
			t.Error("content must be unchanged after rejected cross-project update")
		}
	})

	t.Run("rejects unknown memory", func(t *testing.T) {
		_, err := srv.applyMemoryUpdate(ctx, nil, updateArgs{
			ProjectID: "test-project", MemoryID: "nope", Content: "x",
		})
		if err == nil {
			t.Error("expected not-found error")
		}
	})

	t.Run("rejects invalid category", func(t *testing.T) {
		_, err := srv.applyMemoryUpdate(ctx, nil, updateArgs{
			ProjectID: "test-project", MemoryID: id, Category: "bogus",
		})
		if err == nil {
			t.Error("expected invalid-category error")
		}
	})

	t.Run("rejects empty update", func(t *testing.T) {
		_, err := srv.applyMemoryUpdate(ctx, nil, updateArgs{
			ProjectID: "test-project", MemoryID: id,
		})
		if err == nil {
			t.Error("expected nothing-to-update error")
		}
	})

	t.Run("clamps importance", func(t *testing.T) {
		imp := float32(4.2)
		_, err := srv.applyMemoryUpdate(ctx, nil, updateArgs{
			ProjectID: "test-project", MemoryID: id, Importance: &imp,
		})
		if err != nil {
			t.Fatalf("applyMemoryUpdate: %v", err)
		}
		mems, _ := srv.store.GetByIDs(ctx, []string{id})
		if mems[0].Importance > 1.0 {
			t.Errorf("importance should clamp to 1.0, got %f", mems[0].Importance)
		}
	})
}

func TestPromoteMemory(t *testing.T) {
	srv := testServer(t)
	ctx := context.Background()

	id, err := srv.store.Create(ctx, "abc123", memory.Memory{
		Category: "preference", Content: "tabs never spaces", Source: "mcp", Importance: 0.8, Tags: []string{},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	t.Run("rejects wrong project", func(t *testing.T) {
		if err := srv.store.EnsureProject(ctx, "other", "/tmp/other2", "other"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		if _, err := srv.promoteMemory(ctx, "other", id); err == nil {
			t.Error("expected ownership rejection")
		}
	})

	t.Run("promotes and then rejects re-promotion", func(t *testing.T) {
		msg, err := srv.promoteMemory(ctx, "test-project", id)
		if err != nil {
			t.Fatalf("promoteMemory: %v", err)
		}
		if !strings.Contains(msg, "global") {
			t.Errorf("message should mention global, got %q", msg)
		}
		mems, _ := srv.store.GetByIDs(ctx, []string{id})
		if mems[0].ProjectID != "_global" {
			t.Errorf("project = %q, want _global", mems[0].ProjectID)
		}
		if _, err := srv.promoteMemory(ctx, "test-project", id); err == nil {
			t.Error("expected already-global rejection")
		}
	})

	t.Run("rejects unknown memory", func(t *testing.T) {
		if _, err := srv.promoteMemory(ctx, "test-project", "nope"); err == nil {
			t.Error("expected not-found error")
		}
	})

	t.Run("rejects missing args", func(t *testing.T) {
		if _, err := srv.promoteMemory(ctx, "", id); err == nil {
			t.Error("expected missing project_id error")
		}
		if _, err := srv.promoteMemory(ctx, "test-project", ""); err == nil {
			t.Error("expected missing memory_id error")
		}
	})
}

// connectedClient spins up a Server and a Client wired together over an
// in-memory transport, and returns the connected ClientSession. Callers must
// close the returned session's connection via t.Cleanup handling in Connect.
// The client's name is deliberately unknown to ai.SourceForClientName, so
// tests that need a known harness identity must use connectedClientNamed:
// ghost_resolve no longer falls back to claude for unknown clients.
func connectedClient(t *testing.T, srv *Server) *mcp.ClientSession {
	t.Helper()
	return connectedClientNamed(t, srv, "test-client")
}

// connectedClientNamed connects a client that self-reports name, which drives
// ghost_resolve's session-scoped backend selection.
func connectedClientNamed(t *testing.T, srv *Server, name string) *mcp.ClientSession {
	t.Helper()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	ctx := context.Background()
	if _, err := srv.mcp.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server Connect: %v", err)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: name, Version: "0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestPrompts_RecordDecision(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)

	ctx := context.Background()
	res, err := session.GetPrompt(ctx, &mcp.GetPromptParams{
		Name:      "record_decision",
		Arguments: map[string]string{"project_id": "test-project", "topic": "auth strategy"},
	})
	if err != nil {
		t.Fatalf("GetPrompt record_decision: %v", err)
	}
	text, ok := res.Messages[0].Content.(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", res.Messages[0].Content)
	}
	if !strings.Contains(text.Text, "auth strategy") || !strings.Contains(text.Text, "ghost_decision_record") {
		t.Errorf("expected topic and tool reference in prompt text, got: %s", text.Text)
	}

	if _, err := session.GetPrompt(ctx, &mcp.GetPromptParams{
		Name:      "record_decision",
		Arguments: map[string]string{"project_id": "test-project"},
	}); err == nil {
		t.Error("expected error for missing topic argument")
	}
}

// The waits a subscription test makes, bounded far above what an in-memory
// transport needs — the delivery is a channel hand-off, microseconds of work —
// and far below the package timeout. A test that fails because the behaviour is
// broken must not be able to look like a test that failed because the machine
// was busy (#805), and a bound is only ever paid by a test that is genuinely
// broken.
const (
	// subscriptionWait bounds the wait for a subscription to REGISTER.
	subscriptionWait = 30 * time.Second
	// notificationWait bounds the wait for a notification a test's own trigger
	// emitted exactly once, which cannot be retried without changing what the
	// test is about.
	notificationWait = 30 * time.Second
	// subscriptionProbeInterval paces the re-probe while a subscription is
	// still registering.
	subscriptionProbeInterval = 5 * time.Millisecond
	// subscriptionQuiet is how long the channel must stay empty before the
	// probes are treated as all delivered. It is a delivery-latency bound on
	// an in-memory transport, not a timeout on the behaviour under test.
	subscriptionQuiet = 100 * time.Millisecond
)

// awaitResourceSubscriptions blocks until the SERVER has registered a
// subscription for every uri, and leaves nothing of its own probing in updated.
//
// It exists because ClientSession.Subscribe is fire-and-forget under go-sdk's
// SEP-2575 protocol: it opens the background "subscriptions/listen" stream and
// returns as soon as that call is on the wire, while the server handles the
// stream on a goroutine of its own and then parks it for the life of the
// subscription. Nothing sent after Subscribe is therefore ordered behind the
// registration, and a tool call sent immediately after it can be handled
// FIRST — the notification that tool emits then reaches zero subscribers and
// is never delivered at all. That is a lost notification, not a late one, so
// no longer wait repairs it: the same test with a 30s deadline still failed at
// roughly the same rate as with 2s (#805), while this probe is a signal that a
// subscription is genuinely live.
func awaitResourceSubscriptions(t *testing.T, srv *Server, ctx context.Context, updated <-chan string, uris ...string) {
	t.Helper()
	// A notification for ANY of the requested URIs is progress: a probe that
	// arrives late still proves the subscription it names is registered, and
	// the re-probe loop can leave more than one of them in flight.
	remaining := make(map[string]bool, len(uris))
	for _, uri := range uris {
		remaining[uri] = true
	}
	deadline := time.After(subscriptionWait)
	for len(remaining) > 0 {
		for uri := range remaining {
			srv.notifyResourceUpdated(ctx, uri)
		}
		select {
		case got := <-updated:
			delete(remaining, got)
		case <-time.After(subscriptionProbeInterval):
		case <-deadline:
			t.Fatalf("subscriptions never registered within %s, still missing: %v", subscriptionWait, remaining)
		}
	}
	// Every URI is registered, but probes sent before its registration landed
	// can still be on the wire, and a test that asserts on what arrives NEXT
	// would read one of those as its own trigger's output.
	drainQuiescent(t, updated)
}

// drainQuiescent empties updated and keeps it empty for quiet. A plain
// non-blocking drain is not enough: a notification already sent is delivered
// asynchronously by the client session, so it can land after the drain has
// seen an empty channel. A test whose next assertion is "nothing arrives" would
// then read the probe as the thing it is looking for.
func drainQuiescent(t *testing.T, updated <-chan string) {
	t.Helper()
	deadline := time.After(subscriptionWait)
	for {
		select {
		case <-updated:
			// A probe still arriving; keep waiting for the quiet it implies.
		case <-time.After(subscriptionQuiet):
			return
		case <-deadline:
			t.Fatalf("probe notifications kept arriving for %s after every subscription registered", subscriptionWait)
		}
	}
}

// awaitNotification asserts the next notification is for want. The wait is on
// the event itself — a channel the client's handler writes — so it ends when the
// notification arrives and only expires if it never does.
func awaitNotification(t *testing.T, updated <-chan string, want string) {
	t.Helper()
	select {
	case got := <-updated:
		if got != want {
			t.Errorf("notified URI = %q, want %q", got, want)
		}
	case <-time.After(notificationWait):
		t.Fatalf("no resources/updated notification for %q within %s", want, notificationWait)
	}
}

func TestResourceSubscription_NotifiesNameSubscriberOnToolSave(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	ctx := context.Background()

	// Create a project whose hash ID differs from its name, exactly as the
	// session-start hook does (path-hashed ID + human-readable name).
	if err := store.EnsureProject(ctx, "6bdc098af7f5", "/srv/git/myproj", "myproj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := srv.mcp.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server Connect: %v", err)
	}

	updated := make(chan string, 4)
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, &mcp.ClientOptions{
		ResourceUpdatedHandler: func(ctx context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			updated <- req.Params.URI
		},
	})
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	// Clients read (and thus subscribe to) resources by project name.
	const nameURI = "ghost://project/myproj/context"
	if err := session.Subscribe(ctx, &mcp.SubscribeParams{URI: nameURI}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// The save below emits its notifications exactly once, so a subscription
	// that had not registered yet when it ran would lose them for good (#805).
	awaitResourceSubscriptions(t, srv, ctx, updated, nameURI)

	// Drive the real tool handler, which resolves name -> hash internally.
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_memory_save",
		Arguments: map[string]any{"project_id": "myproj", "content": "name subscriber should be notified", "category": "fact"},
	}); err != nil {
		t.Fatalf("CallTool ghost_memory_save: %v", err)
	}

	awaitNotification(t, updated, nameURI)
}

func TestResourceSubscription_NotifiesContextOnDecisionRecord(t *testing.T) {
	// A decision writes a companion memory in the same transaction and adds a
	// ## Recent Decisions entry, so the context resource a client pinned at
	// session start is stale the moment the call returns (#1271).
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	ctx := context.Background()

	// A hash ID that differs from the name, as the session-start hook creates.
	if err := store.EnsureProject(ctx, "6bdc098af7f5", "/srv/git/myproj", "myproj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := srv.mcp.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server Connect: %v", err)
	}

	updated := make(chan string, 4)
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, &mcp.ClientOptions{
		ResourceUpdatedHandler: func(ctx context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			updated <- req.Params.URI
		},
	})
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	// Subscribe by name, the form a client read the resource with.
	const contextURI = "ghost://project/myproj/context"
	if err := session.Subscribe(ctx, &mcp.SubscribeParams{URI: contextURI}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	awaitResourceSubscriptions(t, srv, ctx, updated, contextURI)

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "ghost_decision_record",
		Arguments: map[string]any{
			"project_id": "myproj",
			"title":      "Use SQLite over Postgres",
			"decision":   "Embedded SQLite with FTS5",
			"rationale":  "Zero external dependencies",
		},
	}); err != nil {
		t.Fatalf("CallTool ghost_decision_record: %v", err)
	}

	awaitNotification(t, updated, contextURI)
}

func TestResourceSubscription_NotifiesGlobalOnGlobalDecisionRecord(t *testing.T) {
	// A decision recorded under _global joins the cross-project rows the
	// ghost://memories/global resource serves, and ghost_save_global notifies
	// that resource for the same reason (#1271).
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	ctx := context.Background()
	if err := store.EnsureProject(ctx, memory.GlobalProjectID, "", "global"); err != nil {
		t.Fatalf("EnsureProject(_global): %v", err)
	}

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := srv.mcp.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server Connect: %v", err)
	}

	updated := make(chan string, 4)
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, &mcp.ClientOptions{
		ResourceUpdatedHandler: func(ctx context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			updated <- req.Params.URI
		},
	})
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	const globalURI = "ghost://memories/global"
	if err := session.Subscribe(ctx, &mcp.SubscribeParams{URI: globalURI}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	awaitResourceSubscriptions(t, srv, ctx, updated, globalURI)

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "ghost_decision_record",
		Arguments: map[string]any{
			"project_id": memory.GlobalProjectID,
			"title":      "Always use 2-space YAML indentation",
			"decision":   "Indent YAML block sequences by two spaces",
			"rationale":  "Matches the surrounding configuration files",
		},
	}); err != nil {
		t.Fatalf("CallTool ghost_decision_record: %v", err)
	}

	awaitNotification(t, updated, globalURI)
}

// TestResourceSubscription_RejectsUnknownURI exercises handleSubscribe
// directly rather than through session.Subscribe. Under go-sdk 1.7.0's
// SEP-2575 protocol, ClientSession.Subscribe dispatches "subscriptions/listen"
// fire-and-forget (see callSubscriptionsListen in the SDK's transport.go) and
// always returns nil once the call is sent, regardless of whether the
// server's SubscribeHandler later rejects the URI — so the rejection is no
// longer observable through the client round trip. handleSubscribe is
// Ghost's own validation logic; test it directly.
func TestResourceSubscription_RejectsUnknownURI(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	ctx := context.Background()
	req := &mcp.SubscribeRequest{Params: &mcp.SubscribeParams{URI: "http://not-ghost/resource"}}
	if err := srv.handleSubscribe(ctx, req); err == nil {
		t.Error("expected error subscribing to non-ghost:// URI")
	}
}

// resolveAnswer is the canonical resolve RESOLVED reply under the #640 verdict
// contract: a RESOLVED must name what closed the note, or the parser reads it
// as a KEEP. Fakes that mean "this harness says resolved" must use it.
const resolveAnswer = "RESOLVED | closed-by: the tracking issue closed"

func writeFakeClaude(t *testing.T, path, answer string) {
	t.Helper()
	script := `#!/bin/sh
if [ "$1" = "--help" ]; then
  printf '%s\n' '--safe-mode' '--restricted' '--strict-mcp-config' '--disable-slash-commands' '--tools' '--disallowedTools' '--setting-sources'
  exit 0
fi
printf '%s' '` + answer + `'
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude binary: %v", err)
	}
}

// TestGhostResolve_UsesConfiguredBinaryAndModelPin covers the cli.*_binary +
// cli.model_resolve path: a fake opencode binary that only answers RESOLVED
// when invoked with `-m opencode/big-pickle` must be used even though a decoy
// opencode (answering KEEP) sits on PATH, and the configured pin must be passed
// constructor-level. The MCP server is long-lived, so the whole test would fail
// (decoy used, badly) if the pin were threaded via process env instead.
func TestGhostResolve_UsesConfiguredBinaryAndModelPin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binaries require a POSIX shell")
	}
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	// The configured opencode binary lives outside PATH; the decoy on PATH
	// answers KEEP, so a PATH-resolved harness would not confirm anything.
	dir := t.TempDir()
	openBin := filepath.Join(dir, "opencode")
	script := `#!/bin/sh
case " $* " in
  *" -m opencode/big-pickle "*) printf '%s\n' '{"type":"text","part":{"type":"text","text":"RESOLVED | closed-by: the tracking issue closed"}}';;
  *) printf '%s\n' '{"type":"text","part":{"type":"text","text":"KEEP"}}';;
esac
`
	if err := os.WriteFile(openBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write configured opencode binary: %v", err)
	}
	decoyDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(decoyDir, "opencode"), []byte("#!/bin/sh\nprintf '%s\\n' '{\"type\":\"text\",\"part\":{\"type\":\"text\",\"text\":\"KEEP\"}}'\n"), 0o755); err != nil {
		t.Fatalf("write decoy opencode binary: %v", err)
	}
	t.Setenv("PATH", decoyDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	srv.SetResolveCLI(config.CLIConfig{
		OpenCodeBinary: openBin,
		ModelResolve:   "opencode/big-pickle",
	})

	ctx := context.Background()
	const content = "root cause: fixed in v2, no further action needed"
	if _, _, _, err := store.Upsert(ctx, "abc123", "gotcha", content, "manual", 0.5, []string{}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	session := connectedClientNamed(t, srv, "opencode")
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_resolve",
		Arguments: map[string]any{"project": "test-project"},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_resolve: %v", err)
	}
	if result.IsError {
		t.Fatalf("ghost_resolve returned an error result: %+v", result.Content)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	if !strings.Contains(text.Text, "1 confirmed evidence") {
		t.Errorf("expected configured opencode with the model pin to confirm, got %q", text.Text)
	}
}

func TestTruncateUTF8(t *testing.T) {
	tests := []struct {
		in       string
		maxBytes int
		want     string
	}{
		{"hello", 10, "hello"},
		{"hello", 4, "hell"},
		{"héllo", 2, "h"}, // é is 2 bytes starting at index 1 — must not split
		{"日本語", 4, "日"},   // each rune is 3 bytes
		{"日本語", 6, "日本"},
		{"", 5, ""},
	}
	for _, tc := range tests {
		got := truncateUTF8(tc.in, tc.maxBytes)
		if got != tc.want {
			t.Errorf("truncateUTF8(%q, %d) = %q, want %q", tc.in, tc.maxBytes, got, tc.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("truncateUTF8(%q, %d) produced invalid UTF-8: %q", tc.in, tc.maxBytes, got)
		}
	}
}

func TestShortID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want string
	}{
		{"shorter than 8", "abc", "abc"},
		{"exactly 8", "12345678", "12345678"},
		{"longer than 8", "123456789abcdef", "12345678"},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shortID(tc.id); got != tc.want {
				t.Errorf("shortID(%q) = %q, want %q", tc.id, got, tc.want)
			}
		})
	}
}

// TestGhostProjectDelete_ApplyRefusesAFuzzyIdentifier: with apply:true the
// tool may only be pointed at a project by the exact id its own dry run
// printed. The dry-run side resolves fuzzy identifiers — the shared
// ResolveProject accepts an id, a name, a canonical path prefix, a repository
// remote and a basename — and an apply that handed the same identifier
// straight through would delete whichever project those words happened to
// resolve to at delete time. A basename and a path must therefore be refused
// before anything is resolved or deleted.
func TestGhostProjectDelete_ApplyRefusesAFuzzyIdentifier(t *testing.T) {
	ctx := context.Background()

	refusesApply := func(t *testing.T, store *memory.Store, identifier string) {
		t.Helper()
		if _, err := store.Create(ctx, "abc123", memory.Memory{
			Category: "fact", Content: "must survive a refused apply", Source: "manual", Importance: 0.5, Tags: []string{},
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
		srv := New(store, logger, "test")
		session := connectedClient(t, srv)

		got := callToolErr(t, session, "ghost_project_delete",
			map[string]any{"project": identifier, "apply": true})
		if !strings.Contains(got, "dry run") {
			t.Errorf("expected the refusal to point at the dry run, got %q", got)
		}

		// The refusal must have deleted nothing: the project row and
		// everything under it survive.
		id, _, err := store.ResolveProject(ctx, "abc123")
		if err != nil {
			t.Fatalf("ResolveProject after refused apply: %v", err)
		}
		if id != "abc123" {
			t.Errorf("expected the project to survive a refused apply, got id %q", id)
		}
		all, err := store.GetAll(ctx, "abc123", 100)
		if err != nil {
			t.Fatalf("GetAll after refused apply: %v", err)
		}
		if len(all) != 1 {
			t.Errorf("expected the seeded memory to survive a refused apply, got %d memories", len(all))
		}
	}

	t.Run("basename", func(t *testing.T) {
		// "test-project" is the project's name, which ResolveProject matches
		// before it ever consults the filesystem — under the pre-fix code
		// apply:true with this identifier deleted the project.
		refusesApply(t, testStore(t), "test-project")
	})

	t.Run("path", func(t *testing.T) {
		// `testStore` records a path that does not exist, and a path resolves
		// only when it names a real directory. Give the project a path that is
		// actually on disk — the shape an agent standing in a checkout passes —
		// so the path the refusal names is one the old code genuinely resolved.
		checkout := t.TempDir()
		db, err := memory.OpenDB(":memory:")
		if err != nil {
			t.Fatalf("OpenDB: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
		store := memory.NewStore(db, logger)
		if err := store.EnsureProject(ctx, "abc123", checkout, "test-project"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		refusesApply(t, store, checkout)
	})
}

func TestGhostProjectDelete_RejectsGlobal(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)
	ctx := context.Background()
	if err := store.SeedGlobalMemories(ctx); err != nil {
		t.Fatalf("SeedGlobalMemories: %v", err)
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_project_delete",
		Arguments: map[string]any{"project": "_global", "apply": true},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_project_delete: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected an error result deleting _global, got success")
	}
}

// TestProjectContextAsOfSaysWhenEveryRowWasWithheldAtT: a block whose every row
// was out of window at T must not read like a project that held nothing then.
// The count is the number of rows withheld, and a project with no rows at that
// instant keeps its current output with no validity note.
func TestProjectContextAsOfSaysWhenEveryRowWasWithheldAtT(t *testing.T) {
	_, session := newCapSession(t)
	const at = "2035-01-01T00:00:00Z"
	for _, content := range []string{"quokka closed one", "quokka closed two"} {
		args := map[string]any{
			"project_id": "test-project", "content": content, "category": "fact",
			"valid_until": "2034-06-01T00:00:00Z",
		}
		if res := callTool(t, session, "ghost_memory_save", args); res.IsError {
			t.Fatalf("save: %s", resultText(res))
		}
	}
	out := resultText(callTool(t, session, "ghost_project_context", map[string]any{
		"project_id": "test-project", "as_of": at,
	}))
	if strings.Contains(out, "quokka closed") {
		t.Errorf("a row out of window at T is listed:\n%s", out)
	}
	if !strings.Contains(out, "Validity judged at "+at) || !strings.Contains(out, "2 memories were withheld") {
		t.Errorf("an all-withheld block does not say 2 rows were withheld at T:\n%s", out)
	}
	if strings.Count(out, "Validity judged at") != 1 {
		t.Errorf("the validity note is not stated once at block level:\n%s", out)
	}

	// No rows at that instant at all: nothing was withheld, so no note.
	empty := resultText(callTool(t, session, "ghost_project_context", map[string]any{
		"project_id": "test-project", "as_of": asOfToolPast,
	}))
	if strings.Contains(empty, "Validity judged at") || strings.Contains(empty, "withheld") {
		t.Errorf("a project with no rows at T gained a validity note:\n%s", empty)
	}
}

// TestGhostTaskComplete_RepeatKeepsNotesAndStamp pins #1143 through the tool:
// a second ghost_task_complete with no notes leaves the stored notes and the
// completed_at stamp alone.
func TestGhostTaskComplete_RepeatKeepsNotesAndStamp(t *testing.T) {
	db, store, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)
	ctx := context.Background()

	id, err := store.CreateTask(ctx, "abc123", "fix the flake", "", 2)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	const notes = "root cause: the retry loop reused a closed conn"
	if res := callTool(t, session, "ghost_task_complete", map[string]any{"task_id": id, "notes": notes}); res.IsError {
		t.Fatalf("first complete: %s", resultText(res))
	}
	const backdated = "2026-01-01 00:00:00"
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET completed_at = ? WHERE id = ?`, backdated, id); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if res := callTool(t, session, "ghost_task_complete", map[string]any{"task_id": id}); res.IsError {
		t.Fatalf("second complete: %s", resultText(res))
	}
	got, err := store.GetTask(ctx, id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Notes != notes {
		t.Errorf("notes = %q, want %q kept", got.Notes, notes)
	}
	if got.CompletedAt != backdated {
		t.Errorf("completed_at = %q, want %q kept", got.CompletedAt, backdated)
	}
}

func TestMoveMemoryRefusals(t *testing.T) {
	srv := testServer(t)
	ctx := context.Background()
	if err := srv.store.EnsureProject(ctx, "def456", "/tmp/second", "second-project"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	id, err := srv.store.Create(ctx, "abc123", memory.Memory{
		Category: "fact", Content: "belongs to the first project", Source: "mcp", Importance: 0.5, Tags: []string{},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	t.Run("refuses a memory the named project does not own", func(t *testing.T) {
		_, err := srv.moveMemory(ctx, nil, "second-project", id, "test-project", false, false)
		if err == nil || !strings.Contains(err.Error(), "does not belong to project") {
			t.Fatalf("err = %v, want a does-not-belong refusal", err)
		}
		mems, _ := srv.store.GetByIDs(ctx, []string{id})
		if mems[0].ProjectID != "abc123" {
			t.Errorf("project = %q after a refused move, want abc123", mems[0].ProjectID)
		}
	})

	t.Run("refuses an unknown target project", func(t *testing.T) {
		_, err := srv.moveMemory(ctx, nil, "test-project", id, "no-such-project", false, false)
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("err = %v, want a target-not-found refusal", err)
		}
		mems, _ := srv.store.GetByIDs(ctx, []string{id})
		if mems[0].ProjectID != "abc123" {
			t.Errorf("project = %q after a refused move, want abc123", mems[0].ProjectID)
		}
	})
}
