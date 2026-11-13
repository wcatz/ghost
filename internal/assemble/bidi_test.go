package assemble

import (
	"testing"
)

// TestDataLeavesOrdinaryUnicodeByteIdentical asserts common non-ASCII text is
// untouched by Data.
func TestDataLeavesOrdinaryUnicodeByteIdentical(t *testing.T) {
	s := "café\tnaïve 日本語 Привет"
	if got, want := Data(s), "«"+s+"»"; got != want {
		t.Errorf("Data(%q) = %q, want %q", s, got, want)
	}
}
