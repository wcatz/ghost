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

const liveSessionListLimit = 500

// guard #588's review asked for: selectCleanupSessions treats every timestamp
func TestOpenCodeSessionList_TimestampsAreMilliseconds(t *testing.T) {
	if !LiveTestsEnabled() {
		t.Skip("live CLI test; set GHOST_LIVE_TESTS=1 to run")
	}
	bin := liveOpenCodeBinary(t)
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("opencode not resolvable: %v", err)
	}
	sessions, err := (openCodeCleanupRunner{binary: bin}).listSessions(context.Background(), liveSessionListLimit)
	if err != nil {
		t.Fatalf("session list: %v", err)
	}
	now := time.Now()
	checked := 0
	for _, s := range sessions {
		for _, f := range []struct {
			ms int64
		}{{s.Created}, {s.Updated}} {
			if f.ms == 0 {
				continue
			}
			checked++
			at := time.UnixMilli(f.ms)
			if !plausibleSessionTime(at, now) {
				t.Fatalf("timestamp %d out of window", f.ms)
			}
		}
	}
	if reason, fatal := uncheckedSessionsReason(len(sessions), checked); reason != "" {
		if fatal {
			t.Fatal(reason)
		}
		t.Skip(reason)
	}
}

func liveOpenCodeBinary(t *testing.T) string {
	t.Helper()
	cfg, err := config.Load()
	if err != nil || cfg.CLI.OpenCodeBinary == "" {
		return "opencode"
	}
	return cfg.CLI.OpenCodeBinary
}

func TestLiveOpenCodeBinaryHonorsTheConfigOverride(t *testing.T) {
	isolateHostConfig(t)
	t.Setenv("GHOST_CLI_OPENCODE_BINARY", "/opt/bin/opencode")
	if got := liveOpenCodeBinary(t); got != "/opt/bin/opencode" {
		t.Errorf("got %q", got)
	}
}

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
		t.Setenv(name, os.Getenv(name))
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
	}
}

func uncheckedSessionsReason(listed, checked int) (string, bool) {
	if checked > 0 {
		return "", false
	}
	if listed == 0 {
		return "NOT checked", false
	}
	return fmt.Sprintf("NOT verified (%d listed)", listed), true
}

func TestUncheckedSessionsReason(t *testing.T) {
	cases := []struct {
		listed, checked int
		wantSkip        bool
		wantFatal       bool
	}{
		{0, 0, true, false},
		{5, 0, false, true},
		{1, 1, false, false},
		{3, 6, false, false},
	}
	for _, tc := range cases {
		reason, fatal := uncheckedSessionsReason(tc.listed, tc.checked)
		if gotSkip := reason != "" && !fatal; gotSkip != tc.wantSkip || fatal != tc.wantFatal {
			t.Fatalf("(%d,%d)->%q,%v", tc.listed, tc.checked, reason, fatal)
		}
	}
}
