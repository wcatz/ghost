package mcpinit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func stopInputFor(t *testing.T, session, transcript string) string {
	t.Helper()
	return contractInputFor(t, "Stop", "claude-code", "claude-jsonl",
		fmt.Sprintf(`{"session_id":%q,"transcript_path":%q,"cwd":"/repo","stop_hook_active":false}`, session, transcript))
}

// isolatedHomeWithStore is isolatedHome plus the data directory a store would
// have made: the marker is written only where one already exists.
func isolatedHomeWithStore(t *testing.T) string {
	t.Helper()
	dataHome := isolatedHome(t)
	if err := os.MkdirAll(filepath.Join(dataHome, "ghost"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dataHome
}

func savesMarkerPath(t *testing.T, dataHome, session string) string {
	t.Helper()
	return filepath.Join(dataHome, "ghost", savesMarkerFile(session))
}

// TestStopGateIsPerTurn: the reminder fires when no Ghost save landed since the
// session's previous stop, not only when the whole session has none.
func TestStopGateIsPerTurn(t *testing.T) {
	t.Run("a save between two stops suppresses the second reminder", func(t *testing.T) {
		isolatedHomeWithStore(t)
		first := writeTranscript(t, lineUser, lineToolBash, lineText)
		if out := runStopHook(t, stopInputFor(t, "sess", first)); !strings.Contains(out, "ghost_memory_save") {
			t.Fatalf("first stop with no saves must nudge, got %q", out)
		}
		second := writeTranscript(t, lineUser, lineToolBash, lineText, lineUser, lineToolBash, lineGhostSave)
		if out := runStopHook(t, stopInputFor(t, "sess", second)); out != "" {
			t.Fatalf("a save landed since the previous stop; got %q", out)
		}
	})
	t.Run("no save since the previous stop reminds even after an earlier save", func(t *testing.T) {
		isolatedHomeWithStore(t)
		first := writeTranscript(t, lineUser, lineToolBash, lineGhostSave)
		if out := runStopHook(t, stopInputFor(t, "sess", first)); out != "" {
			t.Fatalf("first stop saved; got %q", out)
		}
		// The next turn adds tool calls but no save: the old whole-session gate
		// stayed silent here.
		second := writeTranscript(t, lineUser, lineToolBash, lineGhostSave, lineUser, lineToolBash, lineText)
		if out := runStopHook(t, stopInputFor(t, "sess", second)); !strings.Contains(out, "ghost_memory_save") {
			t.Fatalf("no save since the previous stop must nudge, got %q", out)
		}
	})
	t.Run("sessions do not share a count", func(t *testing.T) {
		isolatedHomeWithStore(t)
		saved := writeTranscript(t, lineToolBash, lineGhostSave)
		_ = runStopHook(t, stopInputFor(t, "a", saved))
		other := writeTranscript(t, lineToolBash, lineGhostSave)
		// Session b's first stop has one save and no earlier count: no nudge.
		if out := runStopHook(t, stopInputFor(t, "b", other)); out != "" {
			t.Fatalf("got %q", out)
		}
		none := writeTranscript(t, lineToolBash, lineText)
		if out := runStopHook(t, stopInputFor(t, "c", none)); out == "" {
			t.Fatal("a fresh session with no saves must nudge")
		}
	})
	t.Run("session end removes the marker", func(t *testing.T) {
		dataHome := isolatedHomeWithStore(t)
		path := writeTranscript(t, lineToolBash, lineText)
		_ = runStopHook(t, stopInputFor(t, "gone", path))
		marker := savesMarkerPath(t, dataHome, "gone")
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("marker not written: %v", err)
		}
		var sb strings.Builder
		RunHostEvent("session-end", "claude-code", strings.NewReader(contractInputFor(t, "SessionEnd", "claude-code", "claude-jsonl",
			fmt.Sprintf(`{"session_id":"gone","transcript_path":%q,"cwd":"/repo"}`, path))), &sb, os.Stderr)
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("marker survived session end: %v", err)
		}
	})
}

func TestSaveCountMarker(t *testing.T) {
	t.Run("a write never creates the data dir", func(t *testing.T) {
		dataHome := isolatedHome(t)
		if err := WriteLastSaveCount("s", 3); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dataHome, "ghost")); !os.IsNotExist(err) {
			t.Fatalf("data dir was created: %v", err)
		}
		// A whole stop over a tool-using transcript, with no store, must not either.
		path := writeTranscript(t, lineToolBash, lineText)
		if out := runStopHook(t, stopInputFor(t, "sess", path)); !strings.Contains(out, "ghost_memory_save") {
			t.Fatalf("the nudge must still fire without a marker, got %q", out)
		}
		if _, err := os.Stat(filepath.Join(dataHome, "ghost")); !os.IsNotExist(err) {
			t.Fatalf("the stop hook created the data dir: %v", err)
		}
	})
	t.Run("round trip", func(t *testing.T) {
		isolatedHomeWithStore(t)
		if got := ReadLastSaveCount("s"); got != 0 {
			t.Fatalf("missing marker = %d", got)
		}
		if err := WriteLastSaveCount("s", 3); err != nil {
			t.Fatal(err)
		}
		if got := ReadLastSaveCount("s"); got != 3 {
			t.Fatalf("got %d, want 3", got)
		}
	})
	t.Run("empty session id is inert", func(t *testing.T) {
		isolatedHomeWithStore(t)
		if err := WriteLastSaveCount("", 3); err != nil || ReadLastSaveCount("") != 0 {
			t.Fatal("an empty session id must neither write nor read")
		}
	})
	t.Run("unparseable and undated markers read as zero", func(t *testing.T) {
		dataHome := isolatedHomeWithStore(t)
		if err := WriteLastSaveCount("s", 1); err != nil {
			t.Fatal(err)
		}
		p := savesMarkerPath(t, dataHome, "s")
		for _, body := range []string{"not json", `{"last_save_count":5}`, `{"last_save_count":5,"updated_at":"yesterday"}`} {
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := ReadLastSaveCount("s"); got != 0 {
				t.Errorf("%q read as %d, want 0", body, got)
			}
		}
	})
	t.Run("an expired marker reads as zero and a write sweeps it", func(t *testing.T) {
		dataHome := isolatedHomeWithStore(t)
		if err := WriteLastSaveCount("old", 1); err != nil {
			t.Fatal(err)
		}
		old := savesMarkerPath(t, dataHome, "old")
		stale := time.Now().Add(-lifecycleSavesMarkerMaxAge - time.Hour).UTC().Format(time.RFC3339)
		b, _ := json.Marshal(lifecycleSavesMarker{SessionID: "old", LastSaveCount: 9, UpdatedAt: stale, Version: 1})
		if err := os.WriteFile(old, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if got := ReadLastSaveCount("old"); got != 0 {
			t.Fatalf("expired marker read as %d", got)
		}
		if err := WriteLastSaveCount("new", 1); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(old); !os.IsNotExist(err) {
			t.Fatalf("expired marker not swept: %v", err)
		}
		if ReadLastSaveCount("new") != 1 {
			t.Fatal("the live marker must survive the sweep")
		}
	})
	t.Run("session ids are sanitised into the file name", func(t *testing.T) {
		for in, want := range map[string]string{
			"abc-1_2.3":              "abc-1_2.3",
			"../../x":                ".._.._x",
			"a/b c":                  "a_b_c",
			"":                       "unknown",
			strings.Repeat("z", 200): strings.Repeat("z", 80),
		} {
			if got := sanitizeSavesSessionID(in); got != want {
				t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
			}
		}
	})
}

// TestStopReminderCarriesReasonOnlyWhereTheChannelIsUndocumented pins the use
// of Capability.StopGuidance: hosts without the documented channel also get the
// older top-level reason; neither shape carries a decision.
func TestStopReminderCarriesReasonOnlyWhereTheChannelIsUndocumented(t *testing.T) {
	for _, host := range []string{"claude-code", "opencode", "codex", "goose"} {
		t.Run(host, func(t *testing.T) {
			isolatedHomeWithStore(t)
			path := writeTranscript(t, lineToolBash, lineText)
			input := contractInputFor(t, "Stop", host, "claude-jsonl",
				fmt.Sprintf(`{"session_id":"s","transcript_path":%q,"cwd":"/repo","stop_hook_active":false}`, path))
			var out strings.Builder
			RunHostEvent("stop", host, strings.NewReader(input), &out, os.Stderr)
			var got map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &got); err != nil {
				t.Fatalf("not JSON: %v\n%s", err, out.String())
			}
			if _, has := got["decision"]; has {
				t.Errorf("no decision key expected, got %s", out.String())
			}
			_, hasReason := got["reason"]
			wantReason := host == "codex" || host == "goose"
			if hasReason != wantReason {
				t.Errorf("reason present = %v, want %v: %s", hasReason, wantReason, out.String())
			}
			if !strings.Contains(out.String(), `"additionalContext"`) {
				t.Errorf("additionalContext missing: %s", out.String())
			}
		})
	}
}
