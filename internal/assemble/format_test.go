package assemble

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode"

	"github.com/wcatz/ghost/internal/adversarial"
)

// formatRenderer is one renderer under test: the name a failure reports and
// the function it renders through.
type formatRenderer struct {
	name string
	fn   func(string) string
}

// formatRenderers are the three renderers that print stored text a reader can
// see, and the three a format character can hide a spelling inside.
func formatRenderers() []formatRenderer {
	return []formatRenderer{
		{"Data", Data},
		{"Label", Label},
		{"TagsLabel", func(s string) string { return TagsLabel([]string{s}) }},
	}
}

// TestEveryFormatCharacterIsVisibleInTheRenderers is #1663: the renderers
// escaped the control characters and the bidirectional controls, and the rest
// of Unicode category Cf passed through raw — so a tag of `gh` + U+200B +
// `ost` printed exactly as `ghost` while being a different string.
func TestEveryFormatCharacterIsVisibleInTheRenderers(t *testing.T) {
	for _, r := range adversarial.FormatRunes() {
		in := "gh" + string(r) + "ost"
		for _, tc := range formatRenderers() {
			out := tc.fn(in)
			if strings.ContainsRune(out, r) {
				t.Errorf("%s printed U+%04X raw: %q", tc.name, r, out)
			}
		}
	}
}

// TestTwoValuesDifferingOnlyInAFormatCharacterNeverRenderIdentically states
// the property the escape exists for: two values that differ in bytes must
// never print identically, because a reader cannot see the difference.
//
// "Identically" is measured the way a reader meets it. The characters a reader
// cannot see are removed from both renderings, and the two must STILL differ —
// so the difference survives in something visible rather than in a character
// that only the bytes held. Without the escape the two renderings are the same
// string once the invisible character is taken away, which is the defect.
func TestTwoValuesDifferingOnlyInAFormatCharacterNeverRenderIdentically(t *testing.T) {
	plain := "ghost"
	for _, r := range adversarial.FormatRunes() {
		hidden := "gh" + string(r) + "ost"
		for _, tc := range formatRenderers() {
			a := stripFormatCharacters(tc.fn(plain))
			b := stripFormatCharacters(tc.fn(hidden))
			if a == b {
				t.Errorf("%s rendered %q and %q identically to a reader: %q", tc.name, plain, hidden, a)
			}
		}
	}
}

// stripFormatCharacters removes every character a reader cannot see, so what
// is left is what the rendering shows.
func stripFormatCharacters(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.In(r, unicode.Cf) {
			return -1
		}
		return r
	}, s)
}

// TestTheTagLabelStaysValidJSONForEveryFormatCharacter pins the two halves of
// the tag label at once: the escape must be one a JSON reader accepts, and it
// must decode back to the stored tag. A character outside the Basic
// Multilingual Plane is written as the surrogate pair JSON requires, because
// JSON has no escape for a code point above U+FFFF.
func TestTheTagLabelStaysValidJSONForEveryFormatCharacter(t *testing.T) {
	for _, r := range adversarial.FormatRunes() {
		tag := "gh" + string(r) + "ost"
		out := TagsLabel([]string{tag})
		raw := strings.TrimPrefix(out, " tags:")
		var decoded []string
		if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
			t.Fatalf("TagsLabel(%q) is not valid JSON: %v (%q)", tag, err, out)
		}
		if len(decoded) != 1 || decoded[0] != tag {
			t.Errorf("TagsLabel(%q) decoded to %q, want the stored tag back", tag, decoded)
		}
	}
}

// TestTheIssueCharactersAreEscaped names the three characters the issue
// reports, so the class sweep above and the reported case are held by name.
func TestTheIssueCharactersAreEscaped(t *testing.T) {
	for _, r := range []rune{0x200B, 0x2060, 0xFEFF} {
		in := "gh" + string(r) + "ost"
		for _, tc := range formatRenderers() {
			out := tc.fn(in)
			if strings.ContainsRune(out, r) {
				t.Errorf("%s printed U+%04X raw: %q", tc.name, r, out)
			}
			if strings.Contains(out, "ghost") {
				t.Errorf("%s printed the hidden word as if it were the plain one: %q", tc.name, out)
			}
		}
	}
}
