package memory

import (
	"strings"
	"testing"
	"unicode/utf8"
)

const truncationMarkerLiteral = " …[truncated at 8000 bytes]"

func TestTruncationMarker_MatchesPinnedLiteral(t *testing.T) {
	if got := TruncationMarker(); got != truncationMarkerLiteral {
		t.Errorf("TruncationMarker() = %q, want pinned literal %q", got, truncationMarkerLiteral)
	}
}

func TestClampContent(t *testing.T) {
	for name, in := range map[string]string{
		"short":          "never push to main",
		"exactly at cap": strings.Repeat("x", MaxContentLen),
	} {
		out, cut := ClampContent(in)
		if cut || out != in {
			t.Errorf("%s: cut=%v len %d->%d, want unchanged", name, cut, len(in), len(out))
		}
	}

	for _, n := range []int{MaxContentLen + 1, 12000} {
		in := strings.Repeat("z", n)
		out, cut := ClampContent(in)
		want := strings.Repeat("z", MaxContentLen) + truncationMarkerLiteral
		if !cut || out != want {
			t.Errorf("len %d: cut=%v got %q, want %q", n, cut, out[len(out)-len(truncationMarkerLiteral):], truncationMarkerLiteral)
		}
	}

	in := strings.Repeat("€", 5000)
	out, cut := ClampContent(in)
	if !cut || !utf8.ValidString(out) || !strings.HasSuffix(out, truncationMarkerLiteral) {
		t.Fatal("multibyte clamp: want cut, valid UTF-8, and marker suffix")
	}
	body := strings.TrimSuffix(out, truncationMarkerLiteral)
	if len(body) > MaxContentLen || len(body) == MaxContentLen {
		t.Fatalf("multibyte body len %d, want < %d", len(body), MaxContentLen)
	}
}
