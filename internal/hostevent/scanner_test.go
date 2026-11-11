package hostevent

import (
	"strings"
	"testing"
)

// padLine inserts a large "pad" field into a JSON object line, so the line is
// valid JSON of roughly n bytes that still carries the original content.
func padLine(line string, n int) string {
	return `{"pad":"` + strings.Repeat("x", n) + `",` + line[1:]
}

// TestStreamJSONL_LineCeiling: a line past the memory ceiling aborts the scan
// with an error (the caller fails open) instead of being silently skipped,
// which could hide evidence and misjudge the transcript.
func TestStreamJSONL_LineCeiling(t *testing.T) {
	old := maxTranscriptLine
	maxTranscriptLine = 1 << 10
	t.Cleanup(func() { maxTranscriptLine = old })

	var visited int
	err := streamJSONL(strings.NewReader("{}\n"+padLine("{}", 2<<10)+"\n{}\n"), func([]byte) { visited++ })
	if err == nil {
		t.Fatal("expected an error for a line past the ceiling")
	}
	if visited != 1 {
		t.Errorf("visited %d lines before the over-long one, want 1", visited)
	}
}

// TestStreamJSONL_CeilingChargesTheContentNotTheNewline: the ceiling bounds the
// memory one line's CONTENT may take, and a line whose payload is exactly
// maxTranscriptLine is inside it. The reader charges the trailing "\n" — and
// "\r" before it — against the budget as well, so a line of exactly the ceiling
// was refused by one byte over. Nothing was wrong with that line, and a
// transcript that reaches the ceiling is already the pathological case the
// ceiling exists for, so being one byte short of it is the worst place to be
// strict.
func TestStreamJSONL_CeilingChargesTheContentNotTheNewline(t *testing.T) {
	old := maxTranscriptLine
	maxTranscriptLine = 1 << 10
	t.Cleanup(func() { maxTranscriptLine = old })

	// Both spellings of the terminator, because CRLF is what a Windows host
	// writes and it costs two bytes over one.
	for _, eol := range []string{"\n", "\r\n"} {
		var visited []string
		line := strings.Repeat("x", 1<<10)
		if err := streamJSONL(strings.NewReader(line+eol), func(l []byte) { visited = append(visited, string(l)) }); err != nil {
			t.Errorf("a line of exactly the ceiling with %q ending: %v", eol, err)
		}
		if len(visited) != 1 || visited[0] != line {
			t.Errorf("with %q ending the line was not visited whole: %q", eol, visited)
		}
	}
	// One byte of content over is still over, so the ceiling did not simply
	// stop applying.
	var visited int
	err := streamJSONL(strings.NewReader(strings.Repeat("x", (1<<10)+1)+"\n"), func([]byte) { visited++ })
	if err == nil {
		t.Error("a line one byte over the ceiling was accepted, so the ceiling no longer bounds anything")
	}
	if visited != 0 {
		t.Errorf("visited %d lines past the ceiling, want none", visited)
	}
}

// TestStreamJSONL_LineEndings: CRLF and a final line with no newline behave as
// bufio.ScanLines did.
func TestStreamJSONL_LineEndings(t *testing.T) {
	var lines []string
	if err := streamJSONL(strings.NewReader("a\r\nb\nc"), func(l []byte) { lines = append(lines, string(l)) }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(lines, "|") != "a|b|c" {
		t.Errorf("lines = %q, want a|b|c", lines)
	}
}

// TestStreamJSONL_UnterminatedLineBeforeReadError: a line the read error cut
// off is not visited even when its bytes so far parse as JSON — the reader
// never confirmed it ended there. (It used to drive the deleted save-count
// scanner; it now calls streamJSONL directly, which is what it was about.)
func TestStreamJSONL_UnterminatedLineBeforeReadError(t *testing.T) {
	const whole = `{"type":"assistant","message":{"content":[]}}`
	var visited []string
	err := streamJSONL(&auditErrReader{data: []byte(whole + "\n" + whole)}, func(l []byte) { visited = append(visited, string(l)) })
	if err == nil {
		t.Fatal("expected the read error")
	}
	if len(visited) != 1 {
		t.Errorf("visited %d lines, want 1 (the unterminated line before the error must not be visited): %q", len(visited), visited)
	}
}
