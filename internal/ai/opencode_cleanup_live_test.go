package ai

import (
	"context"
	"os/exec"
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
	for _, s := range sessions {
		fields := []struct {
			name string
			ms   int64
		}{{"created", s.Created}, {"updated", s.Updated}}
		for _, f := range fields {
			if f.ms == 0 {
				continue // an absent timestamp is refused by selection, not an error
			}
			at := time.UnixMilli(f.ms)
			if !plausibleSessionTime(at, now) {
				t.Errorf("session %s %s timestamp %d reads as %v, outside [%v, %v] — opencode is not reporting Unix milliseconds",
					s.ID, f.name, f.ms, at, sessionTimestampFloor, now.Add(sessionFutureSkew))
			}
		}
	}
	t.Logf("checked %d session(s) against the plausible window", len(sessions))
}
