package ai

import "testing"

// TestLiveTestsAreOptIn pins the switch that stops `go test ./...` from making
// real, billable harness calls.
//
// Before this, the four live classifier tests were gated on cli.Available(),
// which is fed by ai.DetectSource() — so running the suite from inside a
// harness session (CLAUDECODE=1 and friends) detected the harness, found a
// binary, and ran: 48s in internal/resolve and 50s in internal/supersede, on
// every plain `go test ./...`. The gate was answering "is a harness around?"
// when the only question worth asking is "did someone ask for this?".
//
// Strictly "1", deliberately. A truthy parse would make GHOST_LIVE_TESTS=true
// in a shell profile silently re-enable billable calls, which is the same
// class of accident this exists to prevent — the whole point is that nothing
// short of an explicit decision turns them on.
func TestLiveTestsAreOptIn(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{
		{"1", true}, // the documented switch
		{"", false}, // unset — the default `go test ./...` case
		{"0", false},
		{"false", false},
		{"true", false}, // truthy is NOT accepted on purpose
		{"yes", false},
		{" 1 ", false}, // no trimming: the value is a token, not prose
		{"10", false},
		{"on", false},
	}
	for _, c := range cases {
		t.Setenv("GHOST_LIVE_TESTS", c.value)
		if got := LiveTestsEnabled(); got != c.want {
			t.Errorf("GHOST_LIVE_TESTS=%q → LiveTestsEnabled() = %v, want %v", c.value, got, c.want)
		}
	}
}

// TestLiveTestsDisabledWhenUnset covers the case that actually matters in CI
// and on a developer's laptop: the variable simply not being there. t.Setenv
// with an empty string is not the same as it being absent, so this clears it
// the way a fresh shell would.
func TestLiveTestsDisabledWhenUnset(t *testing.T) {
	t.Setenv("GHOST_LIVE_TESTS", "")
	if LiveTestsEnabled() {
		t.Error("LiveTestsEnabled() = true with GHOST_LIVE_TESTS empty — plain `go test ./...` would make billable calls")
	}
}
