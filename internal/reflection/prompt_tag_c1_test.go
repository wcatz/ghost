package reflection

import (
	"fmt"
	"strings"
	"testing"
)

// TestATagCannotEndItsRecordWithAUnicodeLineSeparatorOrAC1Control pins the runes
// the C0 test does not catch: every C1 control, the no-break space, and the two
// Unicode line separators. A raw U+2028 or U+2029 is a line end to Python's
// splitlines, to JavaScript and to any Unicode-aware reader, so it would end the
// record as a newline does; a raw CSI (U+009B) starts a terminal control
// sequence. Each is spelled, never passed through, and the spelling survives a
// second pass unchanged.
func TestATagCannotEndItsRecordWithAUnicodeLineSeparatorOrAC1Control(t *testing.T) {
	runes := []rune{0x2028, 0x2029, 0xA0}
	for r := rune(0x80); r <= 0x9F; r++ {
		runes = append(runes, r)
	}
	for _, r := range runes {
		tag := "a" + string(r) + "b"
		want := "a" + tagEscape + fmt.Sprintf("<U+%04X>", r) + "b"
		got := tagSubstitution(tag)
		if got != want {
			t.Errorf("tagSubstitution(%q) = %q, want %q", tag, got, want)
		}
		if strings.ContainsRune(got, r) {
			t.Errorf("U+%04X survived tagSubstitution raw: %q", r, got)
		}
		if again := tagSubstitution(got); again != got {
			t.Errorf("U+%04X: a second pass changed the spelling: %q -> %q", r, got, again)
		}
	}
}
