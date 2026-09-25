package main

import (
	"testing"
	"unicode/utf8"
)

// TestTruncateForDisplayDoesNotSplitARune covers the dry-run preview. It used
// to slice content[:120] directly, which can cut a multi-byte character in
// half and print an invalid byte. The preview is what a user reads to decide
// whether to pass --apply, so it has to show what will actually be stored.
func TestTruncateForDisplayDoesNotSplitARune(t *testing.T) {
	// A run of 3-byte runes: 120 bytes is 40 runes exactly, but any offset
	// that is not a multiple of 3 lands mid-rune.
	Chinese := "数" // 3 bytes
	s := ""
	for i := 0; i < 100; i++ {
		s += Chinese
	}
	if len(s) <= 120 {
		t.Fatalf("precondition: fixture is %d bytes, want > 120", len(s))
	}

	got := truncateForDisplay(s, 120)
	if !utf8.ValidString(got) {
		t.Errorf("result is not valid UTF-8 — a rune was split: %q", got[len(got)-8:])
	}
	if got[len(got)-3:] != "..." {
		t.Errorf("result does not end in the ellipsis marker: %q", got)
	}

	// A 4-byte rune straddling the cut: emoji at the boundary.
	emoji := ""
	for i := 0; i < 40; i++ {
		emoji += "🙂" // 4 bytes
	}
	cut := truncateForDisplay(emoji, 121) // 121 is one byte into a rune
	if !utf8.ValidString(cut) {
		t.Errorf("4-byte rune split: %q", cut)
	}

	// Short input passes through untouched.
	if got := truncateForDisplay("short", 120); got != "short" {
		t.Errorf("short input = %q, want it unchanged", got)
	}
	// Exactly at the limit: no ellipsis added, nothing cut.
	exact := "a"
	for i := 0; i < 119; i++ {
		exact += "a"
	}
	if got := truncateForDisplay(exact, 120); got != exact {
		t.Errorf("input at exactly the limit was modified: len %d -> %d", len(exact), len(got))
	}
}

// TestTruncateForDisplayNeverExceedsBudget: the ellipsis may shorten the text
// but the total must stay within a rune boundary at or below n, not n plus a
// partial rune.
func TestTruncateForDisplayNeverExceedsBudget(t *testing.T) {
	s := ""
	for i := 0; i < 200; i++ {
		s += "é" // 2 bytes
	}
	got := truncateForDisplay(s, 50)
	if !utf8.ValidString(got) {
		t.Fatalf("invalid UTF-8: %q", got)
	}
	body := got[:len(got)-3] // strip the ellipsis
	if len(body) > 50 {
		t.Errorf("body is %d bytes, want <= 50", len(body))
	}
	if len(body)%2 != 0 {
		t.Errorf("body is %d bytes, not a whole number of 2-byte runes", len(body))
	}
}
