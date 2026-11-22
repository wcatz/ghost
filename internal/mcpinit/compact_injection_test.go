package mcpinit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/hostevent"
	"github.com/wcatz/ghost/internal/memory"
)

func compactTestDB(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ghost")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", filepath.Dir(dir))
	dbPath := filepath.Join(dir, "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := testStore(db).EnsureProject(ctx, "p1", "p1", "p1"); err != nil {
		t.Fatal(err)
	}
	if _, err := testStore(db).Create(ctx, "p1", memory.Memory{Category: "fact", Content: "a surviving memory", Source: "manual"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return dbPath
}

func saveTestHandoff(t *testing.T, dbPath, sessionID, content string) {
	t.Helper()
	write, closeWrite, err := openWritableStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer closeWrite()
	if err := write.SaveHandoff(context.Background(), memory.SessionHandoff{
		ProjectID: "p1", SessionID: sessionID, Content: content,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCompactSessionStartPutsTheHandoffAheadOfTheBlock(t *testing.T) {
	dbPath := compactTestDB(t)
	saveTestHandoff(t, dbPath, "s2", "**Handoff from the session before compaction:**\n- Current task: «finish the relay»\n")

	var out strings.Builder
	runCompactSessionStart(sessionStartInput{SessionID: "s2", CWD: "p1", Source: "compact"}, hostevent.SourceClaudeCode, &out)
	got := out.String()
	if !strings.Contains(got, "Handoff from the session before compaction") {
		t.Errorf("compact start has no handoff:\n%s", got)
	}
	if !strings.Contains(got, "a surviving memory") {
		t.Errorf("compact start has no ranked block:\n%s", got)
	}
	if strings.Index(got, "Handoff from the session before compaction") > strings.Index(got, "## Ghost context") {
		t.Errorf("the ranked block came before the handoff:\n%s", got)
	}
	if len(got) > sessionHostLimit {
		t.Errorf("compact block of %d bytes exceeds the %d-byte host limit", len(got), sessionHostLimit)
	}
}

func TestCompactSessionStartBoundsALargeHandoffInsideTheHostLimit(t *testing.T) {
	dbPath := compactTestDB(t)
	var large strings.Builder
	large.WriteString("**Handoff from the session before compaction:**\n")
	for i := 0; len(large.String()) < 3*handoffByteCap; i++ {
		large.WriteString("- Open questions: «a long in-flight question with file names and identifiers number " + strings.Repeat("x", 400) + "»\n")
	}
	saveTestHandoff(t, dbPath, "s2", large.String())

	var out strings.Builder
	runCompactSessionStart(sessionStartInput{SessionID: "s2", CWD: "p1", Source: "compact"}, hostevent.SourceClaudeCode, &out)
	got := out.String()
	if len(got) > sessionHostLimit {
		t.Errorf("a large handoff produced %d bytes, over the %d-byte host limit", len(got), sessionHostLimit)
	}
	if !strings.Contains(got, "a surviving memory") {
		t.Errorf("the large handoff crowded out the ranked block:\n%s", got[:min(len(got), 500)])
	}
	if !strings.Contains(got, "## Ghost context") {
		t.Error("the ranked block's header is missing")
	}
	// The handoff is cut to a whole line, so no « data block is left open.
	blockAt := strings.Index(got, "## Ghost context")
	handoffPart := got[:blockAt]
	if len(handoffPart) > handoffByteCap {
		t.Errorf("the handoff claims %d bytes, over the %d-byte share", len(handoffPart), handoffByteCap)
	}
	if n := strings.Count(handoffPart, "«") - strings.Count(handoffPart, "»"); n != 0 {
		t.Errorf("the bounded handoff has %d unclosed data delimiters", n)
	}
}

func TestCompactSessionStartPrefersItsOwnSessionHandoff(t *testing.T) {
	dbPath := compactTestDB(t)
	saveTestHandoff(t, dbPath, "other", "**Handoff:** «another session's task»\n")
	saveTestHandoff(t, dbPath, "s2", "**Handoff:** «this session's own task»\n")

	var out strings.Builder
	runCompactSessionStart(sessionStartInput{SessionID: "s2", CWD: "p1", Source: "compact"}, hostevent.SourceClaudeCode, &out)
	got := out.String()
	if !strings.Contains(got, "this session's own task") {
		t.Errorf("the compacting session did not get its own handoff:\n%s", got)
	}
	if strings.Contains(got, "another session's task") {
		t.Errorf("the compacting session got another session's handoff:\n%s", got)
	}
}

func TestCompactSessionStartNeverHandsAnotherSessionsHandoff(t *testing.T) {
	dbPath := compactTestDB(t)
	saveTestHandoff(t, dbPath, "a", "**Handoff:** «session A task» «session A open question»\n")

	var out strings.Builder
	runCompactSessionStart(sessionStartInput{SessionID: "b", CWD: "p1", Source: "compact"}, hostevent.SourceClaudeCode, &out)
	got := out.String()
	if strings.Contains(got, "session A task") || strings.Contains(got, "session A open question") {
		t.Errorf("session B was handed session A's task or questions:\n%s", got)
	}
	if !strings.Contains(got, "ghost_project_context") {
		t.Errorf("session B without a handoff lost the short notice:\n%s", got)
	}
}

func TestCompactSessionStartWithoutHandoffKeepsTheShortNotice(t *testing.T) {
	compactTestDB(t)
	var out strings.Builder
	runCompactSessionStart(sessionStartInput{SessionID: "s2", CWD: "p1", Source: "compact"}, hostevent.SourceClaudeCode, &out)
	if !strings.Contains(out.String(), "ghost_project_context") {
		t.Errorf("compact start without a handoff lost the short notice: %q", out.String())
	}
}
