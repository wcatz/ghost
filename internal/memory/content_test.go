package memory

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// truncationMarkerLiteral pins the exact stored marker bytes. A literal
// (not derived from MaxContentLen) so any change to the marker wording or
// the cap fails here and forces a deliberate update of every consumer that
// matches on it.
const truncationMarkerLiteral = " …[truncated at 8000 bytes]"

func TestTruncationMarker_MatchesPinnedLiteral(t *testing.T) {
	if got := TruncationMarker(); got != truncationMarkerLiteral {
		t.Errorf("TruncationMarker() = %q, want pinned literal %q", got, truncationMarkerLiteral)
	}
}

func TestClampContent_FitsIsByteIdentical(t *testing.T) {
	cases := map[string]string{
		"short":          "never push to main",
		"exactly at cap": strings.Repeat("x", MaxContentLen),
	}
	for name, in := range cases {
		out, cut := ClampContent(in)
		if cut {
			t.Errorf("%s: cut=true, want false (content within the cap must not be touched)", name)
		}
		if out != in {
			t.Errorf("%s: content rewritten within the cap: len %d -> %d", name, len(in), len(out))
		}
	}
}

func TestClampContent_OverCapCutsAndMarks(t *testing.T) {
	in := strings.Repeat("z", 12000)
	out, cut := ClampContent(in)
	if !cut {
		t.Fatal("cut=false for 12000 chars, want true")
	}
	want := strings.Repeat("z", MaxContentLen) + truncationMarkerLiteral
	if out != want {
		t.Errorf("clamped content = len %d ending %q, want len %d ending %q",
			len(out), out[len(out)-len(truncationMarkerLiteral):], len(want), truncationMarkerLiteral)
	}
	if !strings.HasSuffix(out, truncationMarkerLiteral) {
		t.Errorf("clamped content must end with the marker; tail %q", out[len(out)-40:])
	}
}

func TestClampContent_MultibyteCutsOnRuneBoundary(t *testing.T) {
	// 3-byte runes: at the 8000-byte cap byte 8000 falls INSIDE a rune
	// (8000 = 3*2666+2), so the rune-boundary backup loop must actually run.
	// A 2-byte rune (é) would land exactly on a boundary and never exercise
	// it — see review nit on the original version of this test.
	in := strings.Repeat("€", 5000) // 15000 bytes
	out, cut := ClampContent(in)
	if !cut {
		t.Fatal("cut=false for 15000-byte input, want true")
	}
	if !utf8.ValidString(out) {
		t.Error("clamped content is not valid UTF-8 — the cut split a rune")
	}
	if !strings.HasSuffix(out, truncationMarkerLiteral) {
		t.Errorf("clamped content must end with the marker; tail %q", out[len(out)-40:])
	}
	body := strings.TrimSuffix(out, truncationMarkerLiteral)
	if len(body) > MaxContentLen {
		t.Errorf("cut body is %d bytes, exceeds the %d cap", len(body), MaxContentLen)
	}
	if len(body) == MaxContentLen {
		t.Errorf("cut body is exactly %d bytes — with 3-byte runes the backup loop cannot have run", MaxContentLen)
	}
}

func TestClampContent_JustOverCapStillMarks(t *testing.T) {
	in := strings.Repeat("z", MaxContentLen+1)
	out, cut := ClampContent(in)
	if !cut {
		t.Fatal("cut=false for cap+1, want true")
	}
	want := strings.Repeat("z", MaxContentLen) + truncationMarkerLiteral
	if out != want {
		t.Errorf("cap+1 clamped content = len %d ending %q, want len %d ending %q",
			len(out), out[len(out)-len(truncationMarkerLiteral):], len(want), truncationMarkerLiteral)
	}
}
