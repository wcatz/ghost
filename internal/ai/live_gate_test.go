package ai

import "testing"

func TestLiveTestsAreOptIn(t *testing.T) {
	for _, c := range []struct {
		value string
		want  bool
	}{
		{"1", true},
		{"", false},
		{"0", false},
		{"true", false},
		{" 1 ", false},
	} {
		t.Setenv("GHOST_LIVE_TESTS", c.value)
		if got := LiveTestsEnabled(); got != c.want {
			t.Errorf("GHOST_LIVE_TESTS=%q → %v, want %v", c.value, got, c.want)
		}
	}
}
