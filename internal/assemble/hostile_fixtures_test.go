package assemble

import (
	"strings"
	"testing"
	"unicode"

	"github.com/wcatz/ghost/internal/adversarial"
)

// hasRawControl reports the first rune in s that a reader acts on rather than
// reads: a control (Cc), a format character (Cf), or one of the two separators
// that end a line without being a control.
func hasRawControl(s string) (rune, bool) {
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == 0x2028 || r == 0x2029 {
			return r, true
		}
	}
	return 0, false
}

// TestSharedFormatControlsAreEscapedAtEveryAssembleBoundary runs the shared
// format-control set through every renderer in this package that prints stored
// text. The three renderers had separate defects in turn (#1649 at Data and
// Label, #1651 at the tag label), so one set is run at all of them rather than
// one test per renderer.
func TestSharedFormatControlsAreEscapedAtEveryAssembleBoundary(t *testing.T) {
	renderers := []struct {
		name string
		fn   func(string) string
	}{
		{"Data", Data},
		{"Label", Label},
		{"TagsLabel", func(s string) string { return TagsLabel([]string{s}) }},
		{"AgentLabel", AgentLabel},
		{"SourceRefLabel", SourceRefLabel},
	}
	for _, p := range adversarial.AllControls() {
		for _, r := range renderers {
			out := r.fn(p.Text)
			if bad, ok := hasRawControl(out); ok {
				t.Errorf("%s(%s) printed U+%04X raw: %q", r.name, p.Name, bad, out)
			}
		}
	}
}

// TestSharedFormatControlsAreEscapedInAMemoryLine checks the composed surface,
// because a renderer can be safe on its own while the line that joins them
// loses the escape.
func TestSharedFormatControlsAreEscapedInAMemoryLine(t *testing.T) {
	for _, p := range adversarial.AllControls() {
		line := Item{
			ID:        "pmem00",
			Category:  "fact",
			Content:   p.Text,
			Agent:     p.Text,
			SourceRef: p.Text,
			Tags:      []string{p.Text},
		}.Line()
		if bad, ok := hasRawControl(line); ok {
			// TAB is the one control Data deliberately leaves alone; the shared
			// set holds none, so anything found here is a renderer gap.
			t.Errorf("memory line (%s) printed U+%04X raw:\n%q", p.Name, bad, line)
		}
		if n := len(strings.Split(line, "\n")); n != 1 {
			t.Errorf("memory line (%s) split into %d physical lines:\n%q", p.Name, n, line)
		}
	}
}

// TestSharedOversizedFieldsAreBoundedByTheAssembleLabels checks the renderers
// that promise a bound against a value no writer would accept, which is exactly
// the value a restored row or an artifact can carry.
func TestSharedOversizedFieldsAreBoundedByTheAssembleLabels(t *testing.T) {
	for _, p := range adversarial.Oversized() {
		if got := TagsLabel([]string{p.Text}); !strings.Contains(got, "tag truncated") {
			t.Errorf("TagsLabel(%s) did not mark the cut: %q", p.Name, got)
		}
		if got := AgentLabel(p.Text); !strings.Contains(got, "agent truncated") {
			t.Errorf("AgentLabel(%s) did not mark the cut: %q", p.Name, got)
		}
		if got := SourceRefLabel(p.Text); !strings.Contains(got, "reference truncated") {
			t.Errorf("SourceRefLabel(%s) did not mark the cut: %q", p.Name, got)
		}
		if got := ProjectLabel(p.Text); !strings.Contains(got, "project id truncated") {
			t.Errorf("ProjectLabel(%s) did not mark the cut: %q", p.Name, got)
		}
	}
}
