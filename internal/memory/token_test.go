package memory

// The bare-token allowlist, pinned EXACTLY.
//
// The set is the whole guarantee: SafeToken writes a value bare when every rune is
// in it and quotes the whole string when any rune is not. A rune wrongly admitted
// reaches a line an agent reads as Ghost's own — a control character drives a
// terminal, U+2029 and U+2028 are line terminators to JavaScript and to several
// newline-splitting readers, a bidi control reorders what a reader sees, and a
// zero-width or non-breaking space is invisible in a rendering while still being
// part of the value.
//
// That makes this the one place the set may be stated rather than demonstrated, and
// the reason the assertion below enumerates the EXACT membership instead of
// checking a table of hostile examples. A table passes whenever the set is widened
// by a character nobody listed — TAB, NBSP, NUL, U+2029 — and each of those was
// admitted by an edit that every other test in memory, assemble, resolve and
// reflection stayed green through. assemble's TestTheSharedTokenRuneSetIsOneSet
// cannot catch that either, and it is not this test's job to: that one asks whether
// the two functions AGREE, and two functions that agree on a wrong set are still
// wrong. This asks whether the set is the documented one.
//
// Exhaustive over every code point rather than sampled, for the same reason: a
// sample is the shape of the bug, since the failures are all in the 1100-odd
// characters nobody thought of.

import (
	"strings"
	"testing"
)

// TestTheBareTokenAllowlistIsExactlyTheDocumentedSet: membership is exactly ASCII
// letters, ASCII digits and the seven separators Ghost's own id and scope shapes
// use. Nothing else, and — checked separately — nothing missing.
//
// The membership side is the important half. The "nothing missing" side is here so
// a set emptied by a bad edit is a failure too: an allowlist that admits nothing
// quotes every real id, which is safe and completely useless, and would pass a
// membership test written only against the exclusions.
func TestTheBareTokenAllowlistIsExactlyTheDocumentedSet(t *testing.T) {
	// The set, written out rather than delegated: this test is the definition, so
	// asking IsSafeTokenRune whether it matches itself would assert nothing.
	const allowed = "._-:/@+"
	inSet := func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return true
		}
		for _, a := range allowed {
			if r == a {
				return true
			}
		}
		return false
	}

	var admitted, rejected []rune
	for r := rune(0); r <= 0x10FFFF; r++ {
		want, got := inSet(r), IsSafeTokenRune(r)
		switch {
		case want && !got:
			admitted = append(admitted, r)
		case !want && got:
			rejected = append(rejected, r)
		}
	}

	if len(rejected) > 0 {
		t.Errorf("%d code point(s) are in the bare set that must not be — the first %d %s. "+
			"Every one of them reaches a line an agent reads as Ghost's own: a control "+
			"character drives a terminal, U+2028/U+2029 are line terminators to JavaScript "+
			"and to several newline-splitting readers, a bidi control reorders what a "+
			"reader sees, and a zero-width or non-breaking space hides inside a rendering "+
			"while still being part of the value",
			len(rejected), min(8, len(rejected)), sampleRunes(rejected))
	}
	if len(admitted) > 0 {
		t.Errorf("%d documented code point(s) are missing from the bare set — the first %d %s. "+
			"An allowlist that admits nothing quotes every real id, which is safe and "+
			"useless: the ids Ghost mints are 32 hex characters and a scope name is a word",
			len(admitted), min(8, len(admitted)), sampleRunes(admitted))
	}
}

// TestTheBareTokenAllowlistStatesTheSevenSeparators: the separators are named
// individually rather than left to the loop above, because each is a character some
// future change would plausibly reach for — `+` for a query parameter, `@` for a
// handle, `:` for a namespaced scope — and dropping one silently widens nothing
// and narrows a real listing. The exhaustive test proves the SET matches; this says
// which members are load-bearing, so a reader narrowing it can see the cost.
func TestTheBareTokenAllowlistStatesTheSevenSeparators(t *testing.T) {
	for _, r := range "._-:/@+" {
		if !IsSafeTokenRune(r) {
			t.Errorf("%q is not in the bare set, so an honest stored value containing it "+
				"renders quoted on every listing and every prompt", r)
		}
		// And each renders BARE, which is the consequence the set exists for: the
		// exhaustive test above proves the membership, this proves it reaches the
		// output. A member the renderer still quoted would satisfy one and fail this,
		// which is the pair worth having — membership and consequence are different
		// claims and only one of them is about the function under test.
		if got := SafeToken("id" + string(r) + "v"); got != "id"+string(r)+"v" {
			t.Errorf("SafeToken(%q) = %q, want it written bare; %q is a documented member",
				"id"+string(r)+"v", got, string(r))
		}
	}
}

// TestTheBareTokenAllowlistHoldsNothingInvisible: the exclusions worth naming are
// the ones that do not LOOK like an exclusion — they are printable, and they are
// what a reader would call whitespace or punctuation.
//
// Named rather than folded into the exhaustive loop, because the loop's failure
// message reports the first few offenders and none of these is where that message
// sends a reader looking. Each is a case where the value is INVISIBLE or MISPLACED
// in a rendering while still being part of the string, which is the hardest class
// of stored value to notice by eye.
func TestTheBareTokenAllowlistHoldsNothingInvisible(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    rune
		why  string
	}{
		{"TAB", '\t', "indents whatever follows it and is invisible in a wrapped line"},
		{"space", ' ', "ends the id's own token on the line"},
		{"no-break space", 0x00A0, "looks like a space and breaks no line"},
		{"line separator", 0x2028, "a LINE TERMINATOR to JavaScript"},
		{"paragraph separator", 0x2029, "a LINE TERMINATOR to JavaScript"},
		{"next line", 0x0085, "a LINE TERMINATOR to several newline-splitting readers"},
		{"zero-width space", 0x200B, "invisible in every renderer"},
		{"zero-width joiner", 0x200D, "invisible in every renderer"},
		{"byte-order mark", 0xFEFF, "invisible in every renderer"},
		{"right-to-left override", 0x202E, "reverses how the rest of the line displays"},
		{"left-to-right isolate", 0x2066, "reorders how the rest of the line displays"},
		{"NUL", 0, "ends the string in most readers that do not expect it"},
		{"DEL", 0x7F, "deletes what follows it in a terminal"},
		{"opening data delimiter", '«', "opens a «...» block of its own"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if IsSafeTokenRune(tc.r) {
				t.Errorf("%q (%U) is in the bare set, but it %s — and every value reaching "+
					"SafeToken is stored text a session or an operator chose",
					string(tc.r), tc.r, tc.why)
			}
		})
	}
}

// sampleRunes renders the first few offenders for a failure message. %U per rune,
// bounded, because a widened set can admit a hundred thousand code points and the
// message has to stay readable.
func sampleRunes(rs []rune) string {
	out := ""
	for i, r := range rs {
		if i == 8 {
			return out + " (and more)"
		}
		if i > 0 {
			out += ", "
		}
		out += string(r) + " " + hexU(r)
	}
	return out
}

// hexU renders a rune as U+XXXX, the same shape Go's %U produces, kept separate so
// sampleRunes reads as one line of logic.
func hexU(r rune) string {
	const digits = "0123456789ABCDEF"
	var out []byte
	for v := int(r); v > 0; v >>= 4 {
		out = append([]byte{digits[v&0xF]}, out...)
	}
	for len(out) < 4 {
		out = append([]byte{'0'}, out...)
	}
	return "U+" + string(out)
}

// TestTheAstralCeilingIsTheRealOne: SafeToken's worst-case expansion is ten
// characters for any rune above U+FFFF — more than the six for a BMP rune and more
// than the one for a bare one. So the per-rune cost is monotone in the escaping
// class and the astral case is the top of it.
//
// A caller that bounds a stored value by runes sizes its output by that ceiling, so
// the claim is about the maximum expansion and is tested as one: a change that made
// the renderer charge twelve for an astral rune would otherwise go unnoticed.
func TestTheAstralCeilingIsTheRealOne(t *testing.T) {
	// Six for a BMP rune, ten for an astral one, measured from the renderer rather
	// than from the source of strconv.
	for _, tc := range []struct {
		name       string
		r          string
		wantEscape int
	}{
		{"a bare rune", "s", 1},
		{"a BMP escaping rune", "\u202e", 6},
		{"an astral escaping rune", "\U000e0001", 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SafeToken(tc.r)
			if tc.wantEscape == 1 {
				// The bare case is the one that costs nothing, and it is here so the
				// comparison has its floor: a renderer that began quoting honest ids
				// would change the ceiling for every real row, not only for a hostile one.
				if got != tc.r {
					t.Errorf("SafeToken(%q) = %q, want it written bare; every id Ghost mints and "+
						"every session id an operator types must stay byte-identical", tc.r, got)
				}
				return
			}
			if got == tc.r {
				t.Fatalf("SafeToken(%q) wrote it bare, so this case measures no escaping", tc.r)
			}
			if len(got) != tc.wantEscape+2 {
				t.Errorf("SafeToken(%q) = %q, %d bytes; expected %d characters escaped plus the "+
					"two quotes", tc.r, got, len(got), tc.wantEscape)
			}
		})
	}
}

// TestSafeTokenWritesARealNameBareAndQuotesEverythingElse: the renderer is the
// definition now that three call sites share it, so it is tested where it lives
// rather than only through the one caller that prompted the move.
//
// The bare cases are the ones every real row takes, and they are pinned to be
// byte-identical to the input: a renderer that started quoting honest ids would
// make every listing ugly and would hide all of this escaping on the common case.
// The quoted cases each name a character class rather than a single instance, for
// the reason the allowlist exists.
func TestSafeTokenWritesARealNameBareAndQuotesEverythingElse(t *testing.T) {
	for _, bare := range []string{
		"a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4", // the id shape Ghost mints
		"ses_2", "0", "a.b-c_d:e/f@g+h",
	} {
		if got := SafeToken(bare); got != bare {
			t.Errorf("SafeToken(%q) = %q, want it written bare and unchanged", bare, got)
		}
	}
	if got := SafeToken(""); got != `""` {
		t.Errorf("SafeToken(\"\") = %q, want %q — an empty field must still print something, or a "+
			"reader cannot tell an empty value from a truncated line", got, `""`)
	}
	for _, tc := range []struct {
		name string
		in   string
	}{
		{"a space", "my session"},
		{"a newline", "a\nb"},
		{"a « that opens a data block", "a«b"},
		{"a non-ASCII name", "メモ"},
		{"a JSON fragment", `{"a":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SafeToken(tc.in)
			if got == tc.in {
				t.Errorf("SafeToken(%q) wrote it bare, so a stored value reached a line outside "+
					"the «...» delimiters unquoted", tc.in)
			}
			if strings.ContainsAny(got, "\n\r") {
				t.Errorf("SafeToken(%q) = %q, which still carries a line break", tc.in, got)
			}
		})
	}
}
