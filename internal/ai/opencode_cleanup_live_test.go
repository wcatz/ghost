package ai

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/config"
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
	bin := liveOpenCodeBinary(t)
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("opencode not resolvable at %q (cli.opencode_binary or PATH): %v", bin, err)
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
	// prints no log line for a passing test — say so out loud, and tell
	// "nothing to inspect" (a skip) apart from "rows that were there but
	// unreadable" (a failure: that is the shape drift this check exists for).
	if reason, fatal := uncheckedSessionsReason(len(sessions), checked); reason != "" {
		if fatal {
			t.Fatal(reason)
		}
		t.Skip(reason)
	}
	t.Logf("checked %d timestamp(s) across %d session(s) against the plausible window", checked, len(sessions))
}

// liveOpenCodeBinary resolves the binary the live check runs, the same way
// `ghost opencode cleanup-sessions` does: cli.opencode_binary (fed by
// GHOST_CLI_OPENCODE_BINARY), else "opencode" on PATH. Without the override a
// user whose opencode is reachable only through it — precisely the user the
// command exists for — would get a skip where the documented check promised a
// verification. A config that fails to load falls back to PATH with a logged
// note rather than failing a diagnostic.
func liveOpenCodeBinary(t *testing.T) string {
	t.Helper()
	const fallback = "opencode"
	cfg, err := config.Load()
	if err != nil {
		t.Logf("config.Load: %v — falling back to the PATH lookup of %q", err, fallback)
		return fallback
	}
	if cfg.CLI.OpenCodeBinary != "" {
		return cfg.CLI.OpenCodeBinary
	}
	return fallback
}

// TestLiveOpenCodeBinaryHonorsTheConfigOverride pins the resolution: the
// documented verification must find the same binary `ghost opencode
// cleanup-sessions` runs, or it silently skips for exactly the setup that
// needs it. The host is isolated first, so the assertion is a function of
// what this test sets rather than of the machine running it — without that,
// a config file that fails to parse or an ambient GHOST_* value would send
// the helper down its fallback branch and fail an unrelated `go test ./...`.
func TestLiveOpenCodeBinaryHonorsTheConfigOverride(t *testing.T) {
	isolateHostConfig(t)
	t.Setenv("GHOST_CLI_OPENCODE_BINARY", "/opt/bin/opencode")
	if got := liveOpenCodeBinary(t); got != "/opt/bin/opencode" {
		t.Errorf("with GHOST_CLI_OPENCODE_BINARY set, got %q, want the configured binary", got)
	}
}

// isolateHostConfig points HOME and XDG_CONFIG_HOME at a temp dir and clears
// every ambient GHOST_* variable, so the config.Load inside a test sees only
// the compiled defaults plus what the test sets. This is the equivalent of
// internal/config's isolateConfig — a test helper of that package, unreachable
// from here — including its one documented gap: the system-wide
// /etc/ghost/config.yaml layer takes its path from a constant with no
// override, so no test can point it elsewhere.
func isolateHostConfig(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp)
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, "GHOST_") {
			continue
		}
		t.Setenv(name, os.Getenv(name)) // records the original for cleanup
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
	}
}

// uncheckedSessionsReason reports why a live run inspected no timestamps, or
// ("", false) when it inspected at least one. The live test must not pass
// vacuously: `go test` prints no log line for a passing test, so a run that
// checked nothing looks exactly like a verified one.
//
// The two empty cases are different and the caller treats them differently.
// An empty listing means this checkout has nothing to inspect — no claim was
// made, so the run skips. A listing full of timestamp-less rows means the
// sessions were there and the fields were not: that is the JSON shape (or
// unit) change this check exists to catch, so it fails. Skipping there would
// turn the safety net green exactly when it should go red.
func uncheckedSessionsReason(listed, checked int) (reason string, fatal bool) {
	if checked > 0 {
		return "", false
	}
	if listed == 0 {
		return "opencode listed no sessions for this project — the millisecond assumption was NOT checked; re-run from a checkout that has opencode sessions", false
	}
	return fmt.Sprintf("opencode listed %d session(s) but none carried a created/updated timestamp — opencode's session JSON shape or unit changed, so the millisecond assumption is NOT verified", listed), true
}

// TestUncheckedSessionsReason pins the non-vacuity guard itself: an empty
// listing must say "NOT checked" and skip, while rows that were listed but
// carried no timestamp must say the same and be fatal — a drift signal that
// skipped would be indistinguishable from a pass. Unlike the live test above,
// this runs on every plain `go test`: it needs neither the opt-in gate nor a
// CLI.
func TestUncheckedSessionsReason(t *testing.T) {
	cases := []struct {
		name      string
		listed    int
		checked   int
		wantSkip  bool
		wantFatal bool
	}{
		{name: "an empty listing is nothing to inspect", listed: 0, checked: 0, wantSkip: true},
		{name: "listed rows without a timestamp are drift, not a skip", listed: 5, checked: 0, wantFatal: true},
		{name: "one checked timestamp is enough", listed: 1, checked: 1},
		{name: "both timestamps of every row count", listed: 3, checked: 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, fatal := uncheckedSessionsReason(tc.listed, tc.checked)
			if gotSkip := reason != "" && !fatal; gotSkip != tc.wantSkip {
				t.Errorf("uncheckedSessionsReason(%d, %d) = (%q, fatal=%v), want skip=%v", tc.listed, tc.checked, reason, fatal, tc.wantSkip)
			}
			if fatal != tc.wantFatal {
				t.Errorf("uncheckedSessionsReason(%d, %d) fatal = %v, want %v", tc.listed, tc.checked, fatal, tc.wantFatal)
			}
			if reason != "" && !strings.Contains(reason, "NOT verified") && !strings.Contains(reason, "NOT checked") {
				t.Errorf("reason = %q, want it to say the assumption was not verified", reason)
			}
		})
	}
}
