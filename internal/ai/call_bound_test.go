package ai

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A caller that removes the bound on purpose (reflection's
// consolidation_timeout_minutes: 0) hands the client a context with no
// deadline, and so does a caller that never set one. The clients used to treat
// both the same way and cut the call at defaultTimeout, so "no bound" was a
// five-minute bound. These tests pin the three cases apart, for every client:
// an unmarked context with no deadline still gets the default (and the error
// says so), a context marked WithoutDefaultBound runs as long as the harness
// takes, and a caller's own deadline wins either way.

// slowHarnessSleep is how long the fake harness takes to answer. It is well
// past the injected default below, so a call cut at the default fails long
// before the answer arrives.
const slowHarnessSleep = 2 * time.Second

// injectedDefault stands in for the five-minute production default, so a cut
// at the default is observable without waiting five minutes.
const injectedDefault = 400 * time.Millisecond

// withDefaultTimeout swaps the package default for the test and restores it.
// Tests that use it must not run in parallel: the default is package state.
func withDefaultTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := defaultTimeout
	defaultTimeout = d
	t.Cleanup(func() { defaultTimeout = prev })
}

// slowHarness is one client under test: its fake binary answers the client's
// own capability or version check at once, then takes slowHarnessSleep to
// answer the real call.
type slowHarness struct {
	name   string
	answer string // the line the fake prints for the real call
	want   string // what the client returns for that line
	call   func(bin string, ctx context.Context) (string, error)
}

func slowHarnesses() []slowHarness {
	return []slowHarness{
		{
			name: "claude", answer: "OK", want: "OK",
			call: func(bin string, ctx context.Context) (string, error) {
				text, _, err := NewCLIClientWithBinary(bin).Reflect(ctx, "consolidate")
				return text, err
			},
		},
		{
			name: "opencode", answer: `{"type":"text","part":{"type":"text","text":"OK"}}`, want: "OK",
			call: func(bin string, ctx context.Context) (string, error) {
				text, _, err := NewOpenCodeClientWithBinary(bin).Reflect(ctx, "consolidate")
				return text, err
			},
		},
		{
			name: "codex", answer: "OK", want: "OK",
			call: func(bin string, ctx context.Context) (string, error) {
				text, _, err := NewCodexClientWithBinary(bin).Reflect(ctx, "consolidate")
				return text, err
			},
		},
		{
			name: "goose", answer: "OK", want: "OK",
			call: func(bin string, ctx context.Context) (string, error) {
				text, _, err := NewGooseClientWithBinary(bin).Reflect(ctx, "consolidate")
				return text, err
			},
		},
	}
}

// writeSlowHarness writes the fake binary for h. The capability and version
// checks (`--help` for claude, `--version` for opencode, `features list` for
// codex) answer immediately, so only the real call is slow.
func writeSlowHarness(t *testing.T, h slowHarness) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binary requires a POSIX shell")
	}
	t.Setenv("GHOST_SCRATCH_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), h.name)
	var script strings.Builder
	script.WriteString("#!/bin/sh\n")
	script.WriteString("case \"$1\" in\n")
	script.WriteString("  --help)\n")
	for _, flag := range []string{"--safe-mode", "--restricted", "--strict-mcp-config", "--disable-slash-commands", "--tools", "--disallowedTools", "--setting-sources"} {
		fmt.Fprintf(&script, "    printf '%%s\\n' %q\n", flag)
	}
	script.WriteString("    exit 0 ;;\n")
	script.WriteString("  --version) printf '2.0.15\\n'; exit 0 ;;\n")
	script.WriteString("  features) exit 0 ;;\n")
	script.WriteString("esac\n")
	script.WriteString("cat >/dev/null\n")
	fmt.Fprintf(&script, "sleep %d\n", int(slowHarnessSleep/time.Second))
	fmt.Fprintf(&script, "printf '%%s\\n' '%s'\n", h.answer)
	if err := os.WriteFile(path, []byte(script.String()), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", h.name, err)
	}
	return path
}

// TestHarnessCallWithoutDeadlineKeepsTheDefault is every caller that is not
// the consolidation path: resolve, supersede, the ghost_resolve tool, the
// bench classifier and the storyline eval pass a context with no deadline and
// no marker, and they keep the per-call default. The error names that default,
// because the harness's own output (an empty tail after a SIGTERM) does not.
func TestHarnessCallWithoutDeadlineKeepsTheDefault(t *testing.T) {
	withDefaultTimeout(t, injectedDefault)
	for _, h := range slowHarnesses() {
		t.Run(h.name, func(t *testing.T) {
			bin := writeSlowHarness(t, h)
			start := time.Now()
			_, err := h.call(bin, context.Background())
			elapsed := time.Since(start)
			if err == nil {
				t.Fatalf("call returned after %v with no error; want a cut at the %v default", elapsed, injectedDefault)
			}
			if elapsed >= slowHarnessSleep {
				t.Errorf("call took %v, want it cut at the %v default before the %v answer", elapsed, injectedDefault, slowHarnessSleep)
			}
			if !strings.Contains(err.Error(), "default") || !strings.Contains(err.Error(), injectedDefault.String()) {
				t.Errorf("error does not name the %v client default: %v", injectedDefault, err)
			}
		})
	}
}

// TestHarnessCallWithoutDefaultBoundIsNotCut is the explicit 0: a context
// marked WithoutDefaultBound and carrying no deadline runs until the harness
// answers, however far past the default that is.
func TestHarnessCallWithoutDefaultBoundIsNotCut(t *testing.T) {
	withDefaultTimeout(t, injectedDefault)
	for _, h := range slowHarnesses() {
		t.Run(h.name, func(t *testing.T) {
			bin := writeSlowHarness(t, h)
			ctx, cancel := context.WithCancel(WithoutDefaultBound(context.Background()))
			defer cancel()
			start := time.Now()
			text, err := h.call(bin, ctx)
			if err != nil {
				t.Fatalf("unbounded call failed after %v: %v", time.Since(start), err)
			}
			if strings.TrimSpace(text) != h.want {
				t.Errorf("text = %q, want %q", text, h.want)
			}
		})
	}
}

// TestHarnessCallKeepsTheCallersDeadline is a configured timeout: the caller's
// deadline wins over the default in both directions, and over the marker. A
// cut at the caller's own deadline is not blamed on the client default.
func TestHarnessCallKeepsTheCallersDeadline(t *testing.T) {
	t.Run("short deadline cuts, and is not called the default", func(t *testing.T) {
		withDefaultTimeout(t, time.Minute)
		for _, h := range slowHarnesses() {
			t.Run(h.name, func(t *testing.T) {
				bin := writeSlowHarness(t, h)
				// The marker must not lift a deadline the caller did set.
				ctx, cancel := context.WithTimeout(WithoutDefaultBound(context.Background()), injectedDefault)
				defer cancel()
				start := time.Now()
				_, err := h.call(bin, ctx)
				elapsed := time.Since(start)
				if err == nil {
					t.Fatalf("call returned after %v with no error; want a cut at the caller's %v deadline", elapsed, injectedDefault)
				}
				if elapsed >= slowHarnessSleep {
					t.Errorf("call took %v, want it cut at the caller's %v deadline", elapsed, injectedDefault)
				}
				if strings.Contains(err.Error(), "default") {
					t.Errorf("a cut at the caller's own deadline was blamed on the client default: %v", err)
				}
			})
		}
	})
	t.Run("long deadline outlives the default", func(t *testing.T) {
		withDefaultTimeout(t, injectedDefault)
		for _, h := range slowHarnesses() {
			t.Run(h.name, func(t *testing.T) {
				bin := writeSlowHarness(t, h)
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				text, err := h.call(bin, ctx)
				if err != nil {
					t.Fatalf("call under a 30s caller deadline failed: %v", err)
				}
				if strings.TrimSpace(text) != h.want {
					t.Errorf("text = %q, want %q", text, h.want)
				}
			})
		}
	})
}

// TestDefaultTimeoutIsFiveMinutes keeps the injection above honest: the
// production default every unmarked caller gets is still five minutes.
func TestDefaultTimeoutIsFiveMinutes(t *testing.T) {
	if defaultTimeout != 5*time.Minute {
		t.Errorf("defaultTimeout = %v, want 5m0s", defaultTimeout)
	}
}

// TestDefaultBoundMarker pins the marker's reach: it is set only by
// WithoutDefaultBound, and survives the derived contexts a caller builds on it.
func TestDefaultBoundMarker(t *testing.T) {
	if DefaultBoundRemoved(context.Background()) {
		t.Error("a plain context must keep the default bound")
	}
	marked := WithoutDefaultBound(context.Background())
	if !DefaultBoundRemoved(marked) {
		t.Error("WithoutDefaultBound did not mark the context")
	}
	derived, cancel := context.WithCancel(marked)
	defer cancel()
	if !DefaultBoundRemoved(derived) {
		t.Error("the marker must survive a derived context")
	}
}
