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

const slowHarnessSleep = 2 * time.Second

const injectedDefault = 400 * time.Millisecond

func withDefaultTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := defaultTimeout
	defaultTimeout = d
	t.Cleanup(func() { defaultTimeout = prev })
}

type slowHarness struct {
	name   string
	answer string
	want   string
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

func TestHarnessCallKeepsTheCallersDeadline(t *testing.T) {
	t.Run("short deadline cuts, and is not called the default", func(t *testing.T) {
		withDefaultTimeout(t, time.Minute)
		for _, h := range slowHarnesses() {
			t.Run(h.name, func(t *testing.T) {
				bin := writeSlowHarness(t, h)

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

func TestDefaultTimeoutIsFiveMinutes(t *testing.T) {
	if defaultTimeout != 5*time.Minute {
		t.Errorf("defaultTimeout = %v, want 5m0s", defaultTimeout)
	}
}

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
