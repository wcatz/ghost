package assemble

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTagsLabelBoundsWhatItPrints is #821, and it is the third field on this line
// to need the same argument as the two beside it.
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

	// The cut has to land on a rune, and the rune has to be one whose width does not
	// divide the bound evenly, or the cut lands on a boundary by luck and proves
	// nothing.
	multibyte := TagsLabel([]string{strings.Repeat("\u20ac", 100)})
	if !strings.Contains(multibyte, "tag truncated") {
		t.Errorf("a 300-byte three-byte-rune tag was not truncated: %s", multibyte)
	}
	if !utf8.ValidString(multibyte) {
		t.Errorf("the label is not valid UTF-8 after truncating a multi-byte tag: %q", multibyte)
	}

	// Each tag is bounded on its OWN.
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
