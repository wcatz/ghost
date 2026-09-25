package memory

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateUTF8StopsAtRuneBoundary(t *testing.T) {
	input := strings.Repeat("🙂", 4)
	got := TruncateUTF8(input, 5)
	if !utf8.ValidString(got) {
		t.Fatalf("truncated text is invalid UTF-8: %q", got)
	}
	if len(got) > 5 {
		t.Errorf("truncated text is %d bytes, want <= 5", len(got))
	}
	if got != "🙂" {
		t.Errorf("truncated text = %q, want one whole rune", got)
	}
}
