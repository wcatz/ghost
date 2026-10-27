package mcpinit

import (
	"strings"
	"testing"
)

// TestStopReminderPerSessionGate pins that the save reminder gates on
// "no Ghost save since this session's previous stop" rather than on the
// session never having saved at all. Two stops sharing one session_id under
// isolatedHome: first with count 1 gives no nudge, second with same count 1
// and tools run must nudge, third with count 2 must not.
func TestStopReminderPerSessionGate(t *testing.T) {
	isolatedHome(t)
	sess := "gate-sess-xyz"

	// First stop: tools plus one Ghost save -> no nudge, and the gate records 1.
	path1 := writeTranscript(t, lineToolBash, lineGhostSave)
	in1 := contractInputFor(t, "Stop", "claude-code", "claude-jsonl",
		`{"session_id":"`+sess+`","transcript_path":"`+path1+`","cwd":"/repo","stop_hook_active":false}`)
	out1 := runStopHookWithInput(t, in1)
	if strings.Contains(out1, "additionalContext") {
		t.Fatalf("first stop with a save must not nudge, got %q", out1)
	}

	// Second stop: same session, same save count (one tool after the earlier
	// save) -> must nudge because no new save since the previous stop.
	path2 := writeTranscript(t, lineToolBash, lineGhostSave, lineToolBash)
	in2 := contractInputFor(t, "Stop", "claude-code", "claude-jsonl",
		`{"session_id":"`+sess+`","transcript_path":"`+path2+`","cwd":"/repo","stop_hook_active":false}`)
	out2 := runStopHookWithInput(t, in2)
	if !strings.Contains(out2, "additionalContext") {
		t.Fatalf("second stop with no new save must nudge, got %q", out2)
	}

	// Third stop: same session, now a second Ghost save -> must not nudge.
	path3 := writeTranscript(t, lineToolBash, lineGhostSave, lineToolBash, lineGhostSave)
	in3 := contractInputFor(t, "Stop", "claude-code", "claude-jsonl",
		`{"session_id":"`+sess+`","transcript_path":"`+path3+`","cwd":"/repo","stop_hook_active":false}`)
	out3 := runStopHookWithInput(t, in3)
	if strings.Contains(out3, "additionalContext") {
		t.Fatalf("third stop with a new save must not nudge, got %q", out3)
	}
}

func runStopHookWithInput(t *testing.T, stdin string) string {
	t.Helper()
	var out strings.Builder
	RunHostEvent("stop", "claude-code", strings.NewReader(stdin), &out, discardWriter{})
	return out.String()
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
