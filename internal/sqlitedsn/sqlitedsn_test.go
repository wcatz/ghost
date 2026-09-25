package sqlitedsn

import (
	"strings"
	"testing"
)

func TestReadOnlyURI(t *testing.T) {
	got := ReadOnlyURI("/tmp/we?rd/#db", 1000)
	if !strings.HasPrefix(got, "file:") {
		t.Fatalf("URI = %q, want file scheme", got)
	}
	if !strings.Contains(got, "mode=ro") || !strings.Contains(got, "busy_timeout(1000)") {
		t.Fatalf("URI = %q, missing read-only or timeout", got)
	}
	if strings.Contains(got, "/tmp/we?rd") {
		t.Fatalf("URI = %q, path was not escaped", got)
	}
}
