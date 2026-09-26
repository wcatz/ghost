package ai

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// liveSessionListLimit bounds how much of the real store one live run reads.
// The unit a timestamp carries is a property of the CLI's output shape, not
// of which row is inspected, so a few hundred rows are enough to detect the
// change this test exists for without waiting on a backlog of thousands.
const liveSessionListLimit = 500

// TestOpenCodeSessionList_TimestampsAreMilliseconds is the opt-in half of the
// guard #588's review asked for: selectCleanupSessions treats every timestamp
// as Unix milliseconds, an assumption the fake-binary tests cannot check
// because they write the fixture in milliseconds themselves. This runs the
// real `opencode session list --format json` and refuses any non-zero
// timestamp that falls outside the plausible window, which is what a unit
// change in the CLI would do — seconds land in 1970, nanoseconds millennia
// out.
//
// It only ever lists: no session is selected and nothing is deleted. Off by
// default behind the same GHOST_LIVE_TESTS=1 gate as the live LLM tests, so
// plain `go test ./...` never spawns `opencode`.
func TestOpenCodeSessionList_TimestampsAreMilliseconds(t *testing.T) {
	if !LiveTestsEnabled() {
		t.Skip("live CLI test reads the real session store; set GHOST_LIVE_TESTS=1 to run")
	}
	bin, err := exec.LookPath("opencode")
	if err != nil {
		t.Skipf("opencode not on PATH: %v", err)
	}

	sessions, err := (openCodeCleanupRunner{binary: bin}).listSessions(context.Background(), liveSessionListLimit)
	if err != nil {
		t.Fatalf("session list: %v", err)
	}
	now := time.Now()
	checked := 0
	for _, s := range sessions {
		fields := []struct {
			name string
			ms   int64
		}{{"created", s.Created}, {"updated", s.Updated}}
		for _, f := range fields {
			if f.ms == 0 {
				continue // an absent timestamp is refused by selection, not an error
			}
			checked++
			at := time.UnixMilli(f.ms)
			if !plausibleSessionTime(at, now) {
				t.Errorf("session %s %s timestamp %d reads as %v, outside [%v, %v] — opencode is not reporting Unix milliseconds",
					s.ID, f.name, f.ms, at, sessionTimestampFloor, now.Add(sessionFutureSkew))
			}
		}
	}
	// A pass that inspected nothing proves nothing, and plain `go test`
	// prints no log line for a passing test — say so out loud instead.
	if reason := uncheckedSessionsReason(len(sessions), checked); reason != "" {
		t.Skip(reason)
	}
	t.Logf("checked %d timestamp(s) across %d session(s) against the plausible window", checked, len(sessions))
}

// uncheckedSessionsReason returns why a live run inspected no timestamps, or
// "" when it inspected at least one. The live test must not pass vacuously: a
// listing that came back empty (or full of zeroed rows) would run no
// assertion at all, and `go test` prints no log line for a passing test, so
// the reader of a plain `ok github.com/wcatz/ghost/internal/ai` could not
// tell a verified millisecond assumption from an unchecked one.
func uncheckedSessionsReason(listed, checked int) string {
	if listed == 0 {
		return "opencode listed no sessions for this project — the millisecond assumption was NOT checked; re-run from a checkout that has opencode sessions"
	}
	if checked == 0 {
		return fmt.Sprintf("opencode listed %d session(s) but none carried a non-zero timestamp — the millisecond assumption was NOT checked", listed)
	}
	return ""
}

// TestUncheckedSessionsReason pins the non-vacuity guard itself: an empty or
// zeroed listing must say "NOT checked" rather than let the live test pass on
// no assertions at all. Unlike the live test above, this runs on every plain
// `go test` — it needs neither the opt-in gate nor a CLI.
func TestUncheckedSessionsReason(t *testing.T) {
	cases := []struct {
		name     string
		listed   int
		checked  int
		wantSkip bool
	}{
		{name: "an empty listing checked nothing", listed: 0, checked: 0, wantSkip: true},
		{name: "rows carrying no timestamp checked nothing", listed: 5, checked: 0, wantSkip: true},
		{name: "one checked timestamp is enough", listed: 1, checked: 1, wantSkip: false},
		{name: "both timestamps of every row count", listed: 3, checked: 6, wantSkip: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason := uncheckedSessionsReason(tc.listed, tc.checked)
			if gotSkip := reason != ""; gotSkip != tc.wantSkip {
				t.Errorf("uncheckedSessionsReason(%d, %d) = %q, want skip=%v", tc.listed, tc.checked, reason, tc.wantSkip)
			}
			if tc.wantSkip && !strings.Contains(reason, "NOT checked") {
				t.Errorf("reason = %q, want it to say the assumption was NOT checked", reason)
			}
		})
	}
}
