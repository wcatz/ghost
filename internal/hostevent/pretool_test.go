package hostevent

import "testing"

func TestNormalizeEventPreToolUse(t *testing.T) {
	cases := []string{"PreToolUse", "pre-tool-use", "pretooluse"}
	for _, c := range cases {
		if got := NormalizeEvent(c); got != EventPreToolUse {
			t.Fatalf("NormalizeEvent(%q)=%q want %q", c, got, EventPreToolUse)
		}
	}
}
