package ai

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Tests for the one spawn funnel, harnessCommand, at the level the confinement
// invariant is written in: every backend gets a working directory and
// TMPDIR/TMP/TEMP inside an invocation-owned directory, whatever the scratch
// root is doing, and no caller can be handed a command that would run
// unconfined.
//
// #751 was the shape of bug these tests exist to catch. The funnel degraded to
// "allowlisted environment, inherited working directory" when scratch.Open
// failed, and the opencode and goose clients compensated with a private
// MkdirTemp tree while the claude and codex clients — which discarded the flag
// — ran the child in Ghost's own working directory with the inherited temp
// variables. The fallback now lives in the funnel so no backend can forget it,
// and these tests pin it for every backend rather than for one harnessKind.

// fallbackTempRootEnv names the test-owned system temp the fallback directory
// is created under. It is an environment variable rather than a returned value
// because t.TempDir() mints a fresh directory on every call, and the assertion
// needs the same one the child was confined under.
const fallbackTempRootEnv = "GHOST_TEST_FALLBACK_TEMP_ROOT"

// brokenScratchRoot points GHOST_SCRATCH_DIR at a path that cannot be created:
// a regular file stands where the root's parent has to be a directory, so
// scratch.Open fails with ENOTDIR. That is the same failure a dev build
// pointed at a GHOST_DEV_FORBID_DATA_DIR data dir produces (#723), which is
// exactly the case where confinement matters most.
func brokenScratchRoot(t *testing.T) {
	t.Helper()
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GHOST_SCRATCH_DIR", filepath.Join(blocker, "scratch"))
}

// childReportLine makes a fake harness report where it actually ran: the
// working directory the kernel gave it and the temp variables it inherited.
// It appends rather than truncating so the probe child and the run child stay
// separately visible — the probe is a child too, and in #751 it was one of the
// two that ran unconfined.
const childReportLine = `{ printf 'CHILD pwd=%s tmpdir=%s tmp=%s temp=%s\n' "$PWD" "$TMPDIR" "$TMP" "$TEMP" >> "$HARNESS_FALLBACK_REPORT"; }` + "\n"

// TestEveryHarnessClientChildIsConfinedWhenScratchFails is the end-to-end half
// of #751, one subtest per backend. It drives each client's own run path with a
// shell fake and reads back what the child SAW, rather than inspecting a
// struct: the bug was never in harnessCommand's contract, it was in two clients
// ignoring that contract, so the assertion has to be about the child.
//
// Each fake also answers the capability probe its client makes first
// (`claude --help`, `codex features list`, `opencode --version`), so the probe
// children are confined by the same assertion as the run children. Only fakes
// are spawned, and HOME is a test directory, so nothing reaches a real harness,
// a real home, or the shared system temp.
func TestEveryHarnessClientChildIsConfinedWhenScratchFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake requires a POSIX shell")
	}

	for _, harness := range []struct {
		name string
		kind harnessKind
		// minimum is the number of children a correct call makes: the run
		// itself, plus the capability probe for the three clients that make
		// one. Asserting it stops a fake that was never probed from quietly
		// satisfying "every child was confined" with a single child.
		minimum int
		script  string
		reflect func(ctx context.Context, bin string) error
	}{
		{
			name:    "claude",
			kind:    harnessClaude,
			minimum: 2,
			script: `if [ "$1" = "--help" ]; then
  printf '%s\n' '--safe-mode' '--restricted' '--strict-mcp-config' '--disable-slash-commands' '--tools' '--disallowedTools' '--setting-sources'
  exit 0
fi
printf '%s' 'KEEP'`,
			reflect: func(ctx context.Context, bin string) error {
				_, _, err := (&CLIClient{binary: bin}).Reflect(ctx, "prompt")
				return err
			},
		},
		{
			name:    "codex",
			kind:    harnessCodex,
			minimum: 2,
			script: `if [ "$1" = "features" ]; then
  printf '%s\n' 'shell_tool stable true' 'unified_exec stable true' 'view_image stable true' 'apps stable true' 'plugins stable true' 'tool_suggest stable true' 'skill_mcp_dependency_install stable true' 'remote_plugin stable true' 'hooks stable true' 'multi_agent stable true'
  exit 0
fi
printf '%s' 'KEEP'`,
			reflect: func(ctx context.Context, bin string) error {
				_, _, err := (&CodexClient{binary: bin}).Reflect(ctx, "prompt")
				return err
			},
		},
		{
			name:    "goose",
			kind:    harnessGoose,
			minimum: 1,
			script:  `printf '%s' 'KEEP'`,
			reflect: func(ctx context.Context, bin string) error {
				_, _, err := (&GooseClient{binary: bin}).Reflect(ctx, "prompt")
				return err
			},
		},
		{
			name:    "opencode",
			kind:    harnessOpencode,
			minimum: 2,
			script: `if [ "$1" = "--version" ]; then
  echo '1.18.32'
  exit 0
fi
printf '%s\n' '{"type":"text","part":{"type":"text","text":"KEEP"}}'`,
			reflect: func(ctx context.Context, bin string) error {
				_, _, err := (&OpenCodeClient{binary: bin}).Reflect(ctx, "prompt")
				return err
			},
		},
	} {
		t.Run(harness.name, func(t *testing.T) {
			root := pinFallbackTempRoot(t)
			brokenScratchRoot(t)
			// A report the fakes append to, outside any invocation directory,
			// since those are removed the moment the call returns.
			report := filepath.Join(t.TempDir(), "children")
			// A home that is a test directory, never the caller's: the point
			// is to prove the child is confined, and a real home would let a
			// passing test depend on the machine it ran on.
			home := t.TempDir()
			for key, value := range map[string]string{
				"HOME":                    home,
				"USERPROFILE":             home,
				"XDG_CONFIG_HOME":         filepath.Join(home, "config"),
				"XDG_DATA_HOME":           filepath.Join(home, "data"),
				"HARNESS_FALLBACK_REPORT": report,
				// The hatch is how a test-only name reaches a child; the
				// alternative, adding it to the production allowlist, would be
				// a hole in the policy these tests verify.
				"GHOST_PASSTHROUGH_ENV": "HARNESS_FALLBACK_REPORT",
			} {
				t.Setenv(key, value)
			}

			bin := filepath.Join(t.TempDir(), harness.name)
			if err := os.WriteFile(bin, []byte("#!/bin/sh\nset -e\n"+childReportLine+harness.script+"\n"), 0o755); err != nil {
				t.Fatalf("write fake %s: %v", harness.name, err)
			}
			if err := harness.reflect(context.Background(), bin); err != nil {
				t.Fatalf("Reflect: %v", err)
			}

			children := readChildReports(t, report)
			if len(children) < harness.minimum {
				t.Fatalf("saw %d child invocation(s), want at least %d — a probe that never ran leaves the probe path untested", len(children), harness.minimum)
			}
			for i, child := range children {
				if child.pwd == "" || child.pwd == workingDir(t) {
					t.Errorf("child %d ran with pwd %q, want a private invocation dir", i, child.pwd)
				}
				if got := filepath.Dir(child.pwd); got != root {
					t.Errorf("child %d ran in %q, want a direct child of the fallback root %q", i, child.pwd, root)
				}
				if want := "ghost-" + string(harness.kind) + "-"; !strings.HasPrefix(filepath.Base(child.pwd), want) {
					t.Errorf("child %d ran in %q, want a name beginning %q", i, child.pwd, want)
				}
				for key, got := range map[string]string{
					"TMPDIR": child.tmpdir,
					"TMP":    child.tmp,
					"TEMP":   child.temp,
				} {
					if got != child.pwd {
						t.Errorf("child %d saw %s=%q, want the invocation dir %q", i, key, got, child.pwd)
					}
				}
				// The release runs on the way out of the call, so by now the
				// directory is gone — which is the half that keeps harness
				// droppings from filling a filesystem.
				if _, err := os.Stat(child.pwd); !os.IsNotExist(err) {
					t.Errorf("invocation dir %s outlived the call (err=%v)", child.pwd, err)
				}
			}
		})
	}
}

// pinFallbackTempRoot redirects the system temp to a test-owned directory and
// returns it, so the fallback directory the funnel creates is assertable and
// the real shared temp is never written to.
func pinFallbackTempRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv(fallbackTempRootEnv, root)
	for _, key := range tempDirKeys {
		t.Setenv(key, root)
	}
	return root
}

// workingDir is the directory a child with no cmd.Dir would run in: the test
// process's own working directory, which is Ghost's source tree.
func workingDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// childReport is one fake child's own account of where it ran.
type childReport struct {
	pwd    string
	tmpdir string
	tmp    string
	temp   string
}

// readChildReports parses the lines the fakes appended. A malformed line is a
// failure rather than a skip: a report the test cannot read is a report that
// cannot support the assertion.
func readChildReports(t *testing.T, path string) []childReport {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read child report: %v", err)
	}
	var out []childReport
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "CHILD "))
		values := map[string]string{}
		for _, field := range fields {
			key, value, found := strings.Cut(field, "=")
			if !found {
				t.Fatalf("malformed child report line %q", line)
			}
			values[key] = value
		}
		if len(values) != 4 {
			t.Fatalf("malformed child report line %q", line)
		}
		out = append(out, childReport{
			pwd:    values["pwd"],
			tmpdir: values["tmpdir"],
			tmp:    values["tmp"],
			temp:   values["temp"],
		})
	}
	return out
}

// TestHarnessCommandConfinesEveryClientWhenScratchFails is the funnel-level
// half of #751: with the scratch root unusable, EVERY backend still gets a
// confined command. The fallback directory is created with os.MkdirTemp, so it
// lands under the system temp — which the test pins to its own directory, both
// so the assertion can name it and so nothing reaches the real one.
func TestHarnessCommandConfinesEveryClientWhenScratchFails(t *testing.T) {
	for _, harness := range []struct {
		name string
		kind harnessKind
	}{
		{name: "claude", kind: harnessClaude},
		{name: "codex", kind: harnessCodex},
		{name: "goose", kind: harnessGoose},
		{name: "opencode", kind: harnessOpencode},
	} {
		t.Run(harness.name, func(t *testing.T) {
			brokenScratchRoot(t)
			root := pinFallbackTempRoot(t)
			env := []string{"PATH=/usr/bin", "TMPDIR=/inherited", "TMP=/inherited", "TEMP=/inherited"}

			cmd, release, err := harnessCommand(context.Background(), "true", nil, env, harness.kind)
			if err != nil {
				t.Fatalf("harnessCommand: %v", err)
			}
			defer release()

			if cmd.Dir == "" {
				t.Fatal("cmd.Dir is empty: the child would run in Ghost's own working directory")
			}
			if got := filepath.Dir(cmd.Dir); got != root {
				t.Errorf("cmd.Dir = %q, want a direct child of the fallback root %q", cmd.Dir, root)
			}
			if want := "ghost-" + string(harness.kind) + "-"; !strings.HasPrefix(filepath.Base(cmd.Dir), want) {
				t.Errorf("cmd.Dir = %q, want a name beginning %q so a leaked directory names its harness", cmd.Dir, want)
			}
			for _, key := range tempDirKeys {
				if got := envValue(cmd.Env, key); got != cmd.Dir {
					t.Errorf("%s = %q, want the fallback dir %q", key, got, cmd.Dir)
				}
				count := 0
				for _, kv := range cmd.Env {
					if k, _, _ := strings.Cut(kv, "="); strings.EqualFold(k, key) {
						count++
					}
				}
				if count != 1 {
					t.Errorf("%s appears %d times, want exactly 1", key, count)
				}
			}
			if _, err := os.Stat(cmd.Dir); err != nil {
				t.Fatalf("fallback dir missing before release: %v", err)
			}
			release()
			if _, err := os.Stat(cmd.Dir); !os.IsNotExist(err) {
				t.Errorf("fallback dir %s survived release (err=%v)", cmd.Dir, err)
			}
			release() // must stay safe to call repeatedly
		})
	}
}

// TestHarnessCommandFailsRatherThanRunUnconfined covers the other end of the
// contract. If the scratch root cannot be created AND the system temp cannot
// host a private directory either — the situation the scratch root exists to
// survive — there is nowhere to confine a child to, so the funnel must return
// an error and no command at all. A non-nil command here is the #751 shape one
// level down: something a caller could run unconfined because it had nowhere
// else to go.
func TestHarnessCommandFailsRatherThanRunUnconfined(t *testing.T) {
	brokenScratchRoot(t)
	// A regular file where the temp root should be: os.MkdirTemp cannot create
	// inside it, so the fallback has nowhere to go either.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, key := range tempDirKeys {
		t.Setenv(key, blocker)
	}
	// The precondition has to be real, not assumed. os.MkdirTemp derives its
	// root from the platform's own notion of the system temp — $TMPDIR on unix,
	// GetTempPath2 on Windows, which VERIFIES each candidate path and falls
	// through to the next one when it is not a directory. Blocking the three
	// variables is therefore not guaranteed to block the temp root there, and a
	// test that assumed it would report a platform quirk as a policy breach.
	// Probe the precondition, and say so plainly when it cannot be created.
	if probe, err := os.MkdirTemp("", "block-probe-"); err == nil {
		_ = os.RemoveAll(probe)
		t.Skipf("this platform still resolved a writable temp root (%s) with every temp key blocked", os.TempDir())
	}

	cmd, release, err := harnessCommand(context.Background(), "true", nil, []string{"PATH=/usr/bin"}, harnessClaude)
	if err == nil {
		t.Fatalf("harnessCommand returned no error with no scratch root and no fallback dir (cmd=%v)", cmd)
	}
	if cmd != nil {
		t.Errorf("cmd = %v, want nil: a caller that ignored the error must not be able to run it", cmd)
	}
	if !strings.Contains(err.Error(), "scratch") {
		t.Errorf("error %q does not name the scratch failure that caused it", err)
	}
	release() // safe to call even though nothing was created
}
