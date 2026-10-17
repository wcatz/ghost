package mcpserver

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wcatz/ghost/internal/memory"
)

// #1461, at the tool boundary. ghost_health's pinned, history and retrieval
// blocks already report a failed read as `could not be read`, and the tool's
// OTHER three reads used to fail in silence: embedding coverage printed
// "**Embeddings:** enabled" with no figure, the memory-links line vanished
// whole, and a project whose count failed got no bullet while the total
// quietly excluded it. Each is a place where an unreadable figure looked like
// a healthy one, so the rule is the one those blocks already follow: a read
// that fails says so, under its own name.

// dropStoreTable removes one table through its own handle, from outside the
// store under test — the second-handle arrangement dropHistoryTable uses. A
// store that opened cleanly and then cannot read a table is the only shape in
// which the failure is unambiguous: the read cannot mistake it for an empty
// table, and the store's own state never changes.
func dropStoreTable(t *testing.T, dbPath, table string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open %s to drop %s: %v", dbPath, table, err)
	}
	defer db.Close() //nolint:errcheck
	if _, err := db.Exec("DROP TABLE " + table); err != nil {
		t.Fatalf("drop %s: %v", table, err)
	}
}

// ghostListProjectsText calls ghost_list_projects and returns its text, or
// fails the test.
func ghostListProjectsText(t *testing.T, srv *Server) string {
	t.Helper()
	session := connectedClient(t, srv)
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "ghost_list_projects"})
	if err != nil {
		t.Fatalf("CallTool ghost_list_projects: %v", err)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("ghost_list_projects returned %T, want text", result.Content[0])
	}
	return text.Text
}

// TestHealthSaysWhenTheEmbeddingStatsReadFails: the else branch of the
// coverage read printed "**Embeddings:** enabled" and nothing else, so a
// store whose vector table was gone reported the one state that reads as
// healthy. "enabled" is still a fact (an embedder is wired); the missing
// coverage figure now carries the failure instead of disappearing.
func TestHealthSaysWhenTheEmbeddingStatsReadFails(t *testing.T) {
	store, dbPath := testStoreWithPath(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	// ghost_health only consults EmbeddingStats when an embedder is wired.
	srv.SetEmbedder(&mockEmbedder{}, make(chan string, 1))
	store.SetEmbeddingIdentity("nomic-embed-text:v1.5:768+prefix")

	ctx := context.Background()
	if _, err := store.Create(ctx, "abc123", memory.Memory{
		Category: "fact", Content: "the relay listens on port 2222 in production",
		Importance: 0.5, Source: "mcp",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	dropStoreTable(t, dbPath, "memory_embeddings")

	text := ghostHealthText(t, srv)

	if !strings.Contains(text, "**Embeddings:** enabled — coverage could not be read") {
		t.Errorf("ghost_health reports a bare 'enabled' after a failed coverage read, which reads as healthy:\n%s", text)
	}
	// The rest of the report survives: one unreadable table is not a reason to
	// hand the agent less than it had a moment ago.
	if !strings.Contains(text, "**Total memories:** 1") {
		t.Errorf("ghost_health lost the fields it already reported:\n%s", text)
	}
}

// TestHealthSaysWhenTheLinkStatsReadFails: the memory-links line was omitted
// outright on a failed read, so its absence meant two things at once — no
// links to report, or a read cut short — and an agent could not tell them
// apart. The other sections already report the failure in place; this one
// now does too.
func TestHealthSaysWhenTheLinkStatsReadFails(t *testing.T) {
	store, dbPath := testStoreWithPath(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	dropStoreTable(t, dbPath, "memory_links")

	text := ghostHealthText(t, srv)

	if !strings.Contains(text, "**Memory links:** could not be read") {
		t.Errorf("ghost_health omits the memory-links line after a failed read, so its absence is ambiguous:\n%s", text)
	}
	if !strings.Contains(text, "**Total memories:**") {
		t.Errorf("ghost_health lost the fields it already reported:\n%s", text)
	}
}

// TestHealthNamesAProjectItCouldNotCount: a project whose CountMemories call
// failed used to be `continue`d past — no bullet, no note — and the total
// excluded it while printing as if it were complete. The project exists and
// the listing says so; what its rows add up to is what could not be read.
func TestHealthNamesAProjectItCouldNotCount(t *testing.T) {
	store, dbPath := testStoreWithPath(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	ctx := context.Background()
	if _, err := store.Create(ctx, "abc123", memory.Memory{
		Category: "fact", Content: "the relay listens on port 2222 in production",
		Importance: 0.5, Source: "mcp",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	dropStoreTable(t, dbPath, "memories")

	text := ghostHealthText(t, srv)

	if !strings.Contains(text, "**Projects:** 1") {
		t.Errorf("the project registration is gone from the report although only its rows became unreadable:\n%s", text)
	}
	if !strings.Contains(text, "count could not be read") {
		t.Errorf("a project whose count failed is skipped in silence, so an unreadable project looks like an empty one:\n%s", text)
	}
	if strings.Contains(text, "- **test-project** (abc123): 0 memories") {
		t.Errorf("the report claims a count for a project whose count failed:\n%s", text)
	}
	if !strings.Contains(text, "**Total memories:** 0 — 1 project could not be counted") {
		t.Errorf("the total presents itself as complete although a project was excluded from it:\n%s", text)
	}
}

// TestProjectListingNamesACountItCouldNotRead: the same shape one step
// further out. ghost_list_projects printed `count, _ := CountMemories(...)`
// and rendered 0 for a project it could not count — and this listing is how
// an agent discovers which projects exist, so a 0 is a claim it would act on.
func TestProjectListingNamesACountItCouldNotRead(t *testing.T) {
	store, dbPath := testStoreWithPath(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	ctx := context.Background()
	if _, err := store.Create(ctx, "abc123", memory.Memory{
		Category: "fact", Content: "the relay listens on port 2222 in production",
		Importance: 0.5, Source: "mcp",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	dropStoreTable(t, dbPath, "memories")

	text := ghostListProjectsText(t, srv)

	if !strings.Contains(text, "count could not be read") {
		t.Errorf("ghost_list_projects shows a count for a project it could not count:\n%s", text)
	}
	if strings.Contains(text, "0 memories") {
		t.Errorf("ghost_list_projects claims a count of zero it never read:\n%s", text)
	}
	// The project itself is still listed, with the identifiers that make it
	// usable — only its rows became unreadable.
	if !strings.Contains(text, "test-project") || !strings.Contains(text, "abc123") {
		t.Errorf("the uncountable project disappeared from the listing:\n%s", text)
	}
}
