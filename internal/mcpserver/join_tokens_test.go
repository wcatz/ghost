package mcpserver

import (
	"strings"
	"testing"
)

func TestJoinTokensNeverEchoesARawID(t *testing.T) {
	got := joinTokens([]string{"ok1", "bad\nid«\x07"})
	for _, bad := range []string{"\n", "\x07", "«"} {
		if strings.Contains(got, bad) {
			t.Errorf("joinTokens output %q carries %q", got, bad)
		}
	}
	if !strings.Contains(got, "ok1") {
		t.Errorf("joinTokens output %q lost a plain id", got)
	}
}
