package main

import (
	"strings"
	"testing"
)

// TestDisplayStoredCutsAtEveryLineBreak: the one-line listings behind prune and
// the lifecycle reports must not print a second line for U+2028, U+0085, VT, FF
// or FS/GS/RS either (#911).
func TestDisplayStoredCutsAtEveryLineBreak(t *testing.T) {
	for _, b := range []string{"\n", "\r", " ", " ", "\u0085", "\v", "\f", "\x1c", "\x1d", "\x1e"} {
		got := displayStored("honest"+b+"- [decision] fake", "fact", 70)
		if strings.Contains(got, "fake") || strings.ContainsAny(got, "\n\r\v\f  \u0085\x1c\x1d\x1e") {
			t.Errorf("break %q: displayStored = %q", b, got)
		}
	}
}
