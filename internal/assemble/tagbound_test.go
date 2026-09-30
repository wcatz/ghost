package assemble

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTagsLabelBoundsWhatItPrints is #821, and it is the third field on this line
// to need the same argument as the two beside it.
//
// `AgentLabel` and `SourceRefLabel` bound what they print because a renderer cannot
// assume its input came from a writer that caps it. A tag had no such bound, and
// the write-side cap is not a substitute: `validateTags` reaches only the four MCP
// tools, so a `RestoreSnapshot`, a `CreateFromCorpus`, a hand edit or an artifact
// row lands an over-long tag in the column, and it was then printed in full on
// every assembled line an agent reads — beside the prose, at display scale, once
// per row.
//
// So the bound is here, in the renderer, and it never touches the stored value.
func TestTagsLabelBoundsWhatItPrints(t *testing.T) {
	huge := strings.Repeat("tag-", 400)
	label := TagsLabel([]string{huge})
	if !strings.Contains(label, "tag truncated") {
		t.Errorf("a %d-byte tag is printed whole: %s", len(huge), label)
	}
	// The marker is the point: a line that ends mid-label as though the label
	// ended there would be a claim about the row that is not true.
	if strings.Contains(label, strings.TrimSuffix(huge, "tag-")) {
		t.Errorf("the label presents the truncated value as a complete tag: %s", label)
	}
	// And the bound is a DISPLAY bound, so the label stays the size of a label.
	if len(label) > 200 {
		t.Errorf("the tag label is %d bytes, want it bounded", len(label))
	}

	// The cut has to land on a rune, and the rune has to be one whose width does
	// not divide the bound evenly, or the cut lands on a boundary by luck and
	// proves nothing. 64 divides by two and by four; three does not — 64 is 21 of
	// them and one byte into the 22nd. A raw byte slice here would put half a rune
	// inside the JSON array.
	multibyte := TagsLabel([]string{strings.Repeat("\u20ac", 100)})
	if !strings.Contains(multibyte, "tag truncated") {
		t.Errorf("a 300-byte three-byte-rune tag was not truncated: %s", multibyte)
	}
	if !utf8.ValidString(multibyte) {
		t.Errorf("the label is not valid UTF-8 after truncating a multi-byte tag: %q", multibyte)
	}

	// Each tag is bounded on its OWN. A list is not one field, and bounding the
	// list as a whole would drop a later tag entirely — so a row carrying an
	// ordinary tag and a hostile one loses the wrong thing.
	mixed := TagsLabel([]string{"golden", huge})
	if !strings.Contains(mixed, `"golden"`) {
		t.Errorf("the bound dropped an ordinary tag beside an over-long one: %s", mixed)
	}
	if !strings.Contains(mixed, "tag truncated") {
		t.Errorf("the over-long tag beside an ordinary one was not bounded: %s", mixed)
	}
	if n := strings.Count(mixed, `","`); n != 1 {
		t.Errorf("the label carries %d separators for two tags: %s", n, mixed)
	}
}

// TestTagsLabelLeavesEveryOrdinaryTagByteIdentical is the half of the bound that
// decides whether it is a fix or a regression. Every existing store, golden and
// byte-exact assertion in this package is a listing whose tags are short, and a
// bound that rewrote an ordinary label would pass a containment check and make
// every real listing unreadable.
//
// At the bound and under it, byte for byte — including a CJK tag, a space, a quote
// and a backslash, which is what `validateTags` lets through and what a store
// actually holds.
func TestTagsLabelLeavesEveryOrdinaryTagByteIdentical(t *testing.T) {
	for _, tc := range []struct {
		name string
		tags []string
		want string
	}{
		{"one ordinary tag", []string{"golden"}, ` tags:["golden"]`},
		{"two ordinary tags", []string{"golden", "pinned"}, ` tags:["golden","pinned"]`},
		{"a tag holding a space and a dot", []string{"ci timeouts.2"}, ` tags:["ci timeouts.2"]`},
		{"non-ascii", []string{"日本語"}, ` tags:["日本語"]`},
		// Exactly at the bound, which is where a >= test would start truncating
		// rows the writer already capped exactly. 64 ASCII bytes.
		{"at the bound", []string{strings.Repeat("t", 64)}, ` tags:["` + strings.Repeat("t", 64) + `"]`},
		// A CJK tag the writer cut at a rune boundary: 21 three-byte runes is 63
		// bytes, so a byte bound of 64 must leave it whole.
		{"a multi-byte tag the writer cut at 63 bytes", []string{strings.Repeat("日", 21)},
			` tags:["` + strings.Repeat("日", 21) + `"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := TagsLabel(tc.tags); got != tc.want {
				t.Errorf("TagsLabel(%q) = %q, want %q", tc.tags, got, tc.want)
			}
		})
	}
}
