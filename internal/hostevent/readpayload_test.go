package hostevent

import (
	"errors"
	"io"
	"testing"
)

// spaceReader yields limit bytes of spaces, then EOF: far more than the
// ceiling, so an unbounded read returns all of it instead of hanging.
type spaceReader struct{ left int }

func (s *spaceReader) Read(p []byte) (int, error) {
	if s.left == 0 {
		return 0, io.EOF
	}
	n := min(len(p), s.left)
	for i := range p[:n] {
		p[i] = ' '
	}
	s.left -= n
	return n, nil
}

// TestReadPayloadStopsPastTheCeiling: the ceiling must bound the read, not only
// the parse. An unbounded io.ReadAll holds whatever the writer sends; ReadPayload stops one byte past the ceiling, so Parse
// can still say "too large" without the payload ever being held in full.
func TestReadPayloadStopsPastTheCeiling(t *testing.T) {
	orig := maxPayloadBytes
	maxPayloadBytes = 4096
	t.Cleanup(func() { maxPayloadBytes = orig })

	r := &spaceReader{left: 10 * maxPayloadBytes}
	data, err := ReadPayload(r)
	if err != nil {
		t.Fatalf("ReadPayload: %v", err)
	}
	if got, want := len(data), maxPayloadBytes+1; got != want {
		t.Fatalf("read %d bytes, want exactly the ceiling plus one (%d)", got, want)
	}
	if _, err := Parse(data, "stop", "claude-code"); !errors.Is(err, errPayloadTooLarge) {
		t.Fatalf("Parse of an over-ceiling read = %v, want errPayloadTooLarge", err)
	}
}
