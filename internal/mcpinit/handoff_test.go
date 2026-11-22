package mcpinit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/hostevent"
	"github.com/wcatz/ghost/internal/memory"
)

func TestScanHandoffTranscriptExtractsWorkingState(t *testing.T) {
	const transcript = `{"type":"user","message":{"content":"keep working on the relay"}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/repo/relay.go"}}]}}
{"type":"user","message":{"content":[{"type":"text","text":"what port should we use?"},{"type":"text","text":"who owns the config?"}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"mention file_path /repo/decoy.go in prose"}]}}
`

	got := scanHandoffTranscript(hostevent.FormatClaudeJSONL, strings.NewReader(transcript))
	if !strings.Contains(got.Task, "what port should we use?") || !strings.Contains(got.Task, "who owns the config?") {
		t.Errorf("task = %q, want the last user message", got.Task)
	}
	if len(got.Questions) != 2 {
		t.Errorf("questions = %v, want two open questions", got.Questions)
	}
	if len(got.Files) != 1 || got.Files[0] != "/repo/relay.go" {
		t.Errorf("files = %v, want only the file from the tool call", got.Files)
	}
	if got.Position != 4 {
		t.Errorf("position = %d, want 4 lines", got.Position)
	}
}

func TestScanHandoffTranscriptIsFailOpen(t *testing.T) {
	// A malformed line is skipped, and so is an oversized line in the middle:
	// the scan keeps going, finds the later message, and counts every line.
	transcript := "{\"type\":\"user\",\"message\":{\"content\":\"early task\"}}\nnot json at all\n" +
		"{\"type\":\"user\",\"message\":{\"content\":\"" + strings.Repeat("x", 1<<20) + "\"}}\n" +
		"{\"type\":\"user\",\"message\":{\"content\":\"later task\"}}\n"
	got := scanHandoffTranscript(hostevent.FormatClaudeJSONL, strings.NewReader(transcript))
	if got.Task != "later task" || got.Position != 4 {
		t.Errorf("scan = %+v, want the later task and a position of 4", got)
	}
}

func TestScanHandoffTranscriptIgnoresToolResults(t *testing.T) {
	const transcript = `{"type":"user","message":{"content":"fix the relay timeout"}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"grep -r timeout ."}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"relay.go:12: why is this 5s?\nUNIQUEOUTPUT"}]}}
`
	got := scanHandoffTranscript(hostevent.FormatClaudeJSONL, strings.NewReader(transcript))
	if got.Task != "fix the relay timeout" {
		t.Errorf("task = %q, want the real user message", got.Task)
	}
	all := got.Task + strings.Join(got.Questions, "\n")
	if strings.Contains(all, "UNIQUEOUTPUT") || strings.Contains(all, "why is this") {
		t.Errorf("tool output leaked into task or questions: %q %v", got.Task, got.Questions)
	}
	if got.Position != 3 {
		t.Errorf("position = %d, want 3", got.Position)
	}
}

func TestRunPreCompactStoresAHandoffWithAPointer(t *testing.T) {
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
	if err := testStore(db).EnsureProject(t.Context(), "p1", "p1", "p1"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	transcript := filepath.Join(t.TempDir(), "t.jsonl")
	writeFile(t, transcript, "{\"type\":\"user\",\"message\":{\"content\":\"the relay task\"}}\n"+
		"{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"tool_use\",\"name\":\"Read\",\"input\":{\"file_path\":\"/repo/relay.go\"}}]}}\n")
	var stderr strings.Builder
	runPreCompact(hostevent.Payload{
		HookEventName:  "PreCompact",
		SessionID:      "s1",
		CWD:            "p1",
		TranscriptPath: transcript,
		Contract:       &hostevent.Envelope{Version: hostevent.ContractVersion, Source: string(hostevent.SourceClaudeCode), TranscriptFormat: hostevent.FormatClaudeJSONL},
	}, &stderr)
	if stderr.Len() != 0 {
		t.Errorf("pre-compact wrote to stderr: %s", stderr.String())
	}
	read, err := memory.OpenReadDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = read.Close() }()
	store := sessionStore(read)
	h, ok, err := store.HandoffForSession(context.Background(), "p1", "s1", time.Now())
	if err != nil || !ok {
		t.Fatalf("HandoffForSession = %+v,%v,%v", h, ok, err)
	}
	if h.SessionID != "s1" || h.TranscriptPath != transcript || h.TranscriptPosition != 2 {
		t.Errorf("pointer = %+v, want session s1 at position 2 of the transcript", h)
	}
	for _, want := range []string{"the relay task", "/repo/relay.go", "s1", "position 2"} {
		if !strings.Contains(h.Content, want) {
			t.Errorf("handoff content %q does not carry %q", h.Content, want)
		}
	}
}

func TestRunPreCompactMissingTranscriptIsFailOpen(t *testing.T) {
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
	if err := testStore(db).EnsureProject(t.Context(), "p1", "/repo", "p1"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	var stderr strings.Builder
	runPreCompact(hostevent.Payload{
		HookEventName:  "PreCompact",
		SessionID:      "s1",
		CWD:            "p1",
		TranscriptPath: filepath.Join(t.TempDir(), "gone.jsonl"),
		Contract:       &hostevent.Envelope{Version: hostevent.ContractVersion, Source: string(hostevent.SourceClaudeCode), TranscriptFormat: hostevent.FormatClaudeJSONL},
	}, &stderr)
	if !strings.Contains(stderr.String(), "fail-open") || !strings.Contains(stderr.String(), "transcript") {
		t.Errorf("a missing transcript produced no fail-open diagnostic: %q", stderr.String())
	}
	read, err := memory.OpenReadDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = read.Close() }()
	if _, ok, err := sessionStore(read).HandoffForSession(context.Background(), "p1", "s1", time.Now()); err != nil || ok {
		t.Errorf("a missing transcript stored a handoff: %+v,%v", ok, err)
	}
}

// TestPreCompactNeverStoresATokenFromTheTranscript pins the token guard on the
// text a handoff copies out of the transcript: a user line holding a
// credential-shaped value stores no handoff text containing it, and the compact
// session-start that replays the handoff does not carry it either.
func TestPreCompactNeverStoresATokenFromTheTranscript(t *testing.T) {
	dbPath := compactTestDB(t)
	token := "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd"
	transcript := filepath.Join(t.TempDir(), "t.jsonl")
	writeFile(t, transcript, "{\"type\":\"user\",\"message\":{\"content\":\"fix the relay, my token is "+token+" ok?\"}}\n"+
		"{\"type\":\"user\",\"message\":{\"content\":\"what is the port?\\nuse "+token+" for auth?\"}}\n")
	var stderr strings.Builder
	runPreCompact(hostevent.Payload{
		HookEventName:  "PreCompact",
		SessionID:      "s1",
		CWD:            "p1",
		TranscriptPath: transcript,
		Contract:       &hostevent.Envelope{Version: hostevent.ContractVersion, Source: string(hostevent.SourceClaudeCode), TranscriptFormat: hostevent.FormatClaudeJSONL},
	}, &stderr)

	read, err := memory.OpenReadDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	h, ok, err := sessionStore(read).HandoffForSession(context.Background(), "p1", "s1", time.Now())
	_ = read.Close()
	if err != nil || !ok {
		t.Fatalf("HandoffForSession = %+v,%v,%v", h, ok, err)
	}
	if strings.Contains(h.Content, token) || strings.Contains(h.Content, "ghp_") {
		t.Errorf("stored handoff carries the token:\n%s", h.Content)
	}
	if !strings.Contains(h.Content, "what is the port?") {
		t.Errorf("the clean open question was dropped with the flagged ones:\n%s", h.Content)
	}

	var out strings.Builder
	runCompactSessionStart(sessionStartInput{SessionID: "s1", CWD: "p1", Source: "compact"}, hostevent.SourceClaudeCode, &out)
	if strings.Contains(out.String(), "ghp_") {
		t.Errorf("compact session-start carries the token:\n%s", out.String())
	}
}

// TestHandoffRenderCapFitsTheStoreCap holds the storage cap at or above what
// the compact session-start will ever render, so the render cap is never the
// number the store silently undercuts.
func TestHandoffRenderCapFitsTheStoreCap(t *testing.T) {
	if memory.MaxHandoffContentBytes < handoffByteCap {
		t.Errorf("store cap %d is below the %d-byte render cap", memory.MaxHandoffContentBytes, handoffByteCap)
	}
}
