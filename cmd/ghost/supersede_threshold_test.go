package main

// --threshold is the minimum cosine similarity a candidate pair needs before the
// pass spends a classify call on it. It used to drop a parse error and keep the
// 0.80 default, and to take any number at all: `--threshold 0,9` ran at 0.80
// while the operator thought 0.9 was in force, `-1` or `0` sent every
// neighbouring pair to the classifier (one billed harness call each), and `5`
// proposed nothing. A value that is not a number in (0, 1] is refused with exit
// 1, the way --consensus and prune's --grace refuse theirs.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSupersedeArgsThresholdDefault(t *testing.T) {
	_, _, _, _, threshold, _, _, _, err := parseSupersedeArgs([]string{"myproj"})
	if err != nil {
		t.Fatalf("parseSupersedeArgs: %v", err)
	}
	if threshold != 0.80 {
		t.Errorf("threshold = %v, want the 0.80 default when the flag is not typed", threshold)
	}
}

func TestParseSupersedeArgsThresholdAccepted(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want float32
	}{
		{"separate value", []string{"myproj", "--threshold", "0.9"}, 0.9},
		{"equals value", []string{"myproj", "--threshold=0.5"}, 0.5},
		{"upper bound", []string{"myproj", "--threshold", "1"}, 1},
		{"low but positive", []string{"myproj", "--threshold", "0.1"}, 0.1},
		{"before apply", []string{"myproj", "--threshold", "0.85", "--apply"}, 0.85},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, _, threshold, _, _, _, err := parseSupersedeArgs(tc.args)
			if err != nil {
				t.Fatalf("parseSupersedeArgs(%v): %v", tc.args, err)
			}
			if threshold != tc.want {
				t.Errorf("threshold = %v, want %v", threshold, tc.want)
			}
		})
	}
}

// supersedeThresholdRefusals is shared by the parser test and the built-binary
// test so both read the same table.
var supersedeThresholdRefusals = []struct {
	name string
	args []string
	echo string // the operand as the refusal names it
}{
	{"not a number", []string{"--threshold", "abc"}, `"abc"`},
	{"not a number equals form", []string{"--threshold=abc"}, `"abc"`},
	{"decimal comma", []string{"--threshold=0,9"}, `"0,9"`},
	{"empty equals form", []string{"--threshold="}, `""`},
	{"NaN", []string{"--threshold", "NaN"}, `"NaN"`},
	{"infinity", []string{"--threshold", "Inf"}, `"Inf"`},
	{"flag as value", []string{"--threshold", "--apply"}, `"--apply"`},
	{"zero", []string{"--threshold", "0"}, `"0"`},
	{"below range", []string{"--threshold", "-1"}, `"-1"`},
	{"below range equals form", []string{"--threshold=-0.5"}, `"-0.5"`},
	{"underflows to zero", []string{"--threshold", "1e-50"}, `"1e-50"`},
	{"above range", []string{"--threshold", "5"}, `"5"`},
	{"just above range", []string{"--threshold=1.0001"}, `"1.0001"`},
}

func TestParseSupersedeArgsThresholdRefused(t *testing.T) {
	for _, tc := range supersedeThresholdRefusals {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"myproj"}, tc.args...)
			_, _, _, _, _, _, _, _, err := parseSupersedeArgs(args)
			if err == nil {
				t.Fatalf("parseSupersedeArgs(%v) must fail", args)
			}
			msg := err.Error()
			if !strings.Contains(msg, "--threshold") || !strings.Contains(msg, "(0, 1]") {
				t.Errorf("error %q must name the flag and its range (0, 1]", msg)
			}
			if !strings.Contains(msg, tc.echo) {
				t.Errorf("error %q must name the value %s", msg, tc.echo)
			}
		})
	}
}

// TestSupersedeThresholdRefusalExitsOneWithoutModelCall drives the built binary,
// because the exit status is the contract. Exit 1 alone proves little here (a
// project that does not exist also exits 1), so each refused case runs against a
// fresh, empty home and asserts the refusal names the flag, that no harness on
// PATH ran (every one the pass could reach is a fake that leaves a marker), and
// that no data directory was created: the refusal comes before the store is
// opened, and the store is opened before anything is classified. The valid case
// runs against the always-present "global" project so it gets past project
// resolution and into the pass itself.
func TestSupersedeThresholdRefusalExitsOneWithoutModelCall(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the ghost binary")
	}
	bin := filepath.Join(t.TempDir(), "ghost")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	fakes := t.TempDir()
	marker := filepath.Join(t.TempDir(), "harness-called")
	for _, name := range []string{"claude", "opencode", "codex", "goose"} {
		script := "#!/bin/sh\necho " + name + " >> " + marker + "\nexit 1\n"
		if err := os.WriteFile(filepath.Join(fakes, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run := func(t *testing.T, home, project string, args ...string) (int, string) {
		t.Helper()
		cmd := exec.Command(bin, append([]string{"supersede", project, "--source", "claude-code"}, args...)...)
		cmd.Env = []string{
			"HOME=" + home, "XDG_CONFIG_HOME=" + home, "XDG_DATA_HOME=" + home,
			"PATH=" + fakes,
		}
		if v := os.Getenv("GHOST_DEV_FORBID_DATA_DIR"); v != "" {
			cmd.Env = append(cmd.Env, "GHOST_DEV_FORBID_DATA_DIR="+v)
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("run: %v", err)
		}
		return code, stderr.String()
	}

	for _, tc := range supersedeThresholdRefusals {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			code, stderr := run(t, home, "myproj", tc.args...)
			if code != 1 {
				t.Fatalf("exit = %d, want 1\nstderr:\n%s", code, stderr)
			}
			if !strings.Contains(stderr, "--threshold") || !strings.Contains(stderr, "(0, 1]") {
				t.Errorf("stderr does not refuse the threshold:\n%s", stderr)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatalf("a harness was invoked for a refused --threshold")
			}
			if _, err := os.Stat(filepath.Join(home, "ghost")); err == nil {
				t.Errorf("the refused run opened the store (%s exists); the refusal must come first", filepath.Join(home, "ghost"))
			}
		})
	}

	t.Run("valid value is accepted", func(t *testing.T) {
		code, stderr := run(t, t.TempDir(), "global", "--threshold", "0.9")
		if strings.Contains(stderr, "--threshold") {
			t.Errorf("a valid --threshold was refused:\n%s", stderr)
		}
		if code != 0 {
			t.Errorf("exit = %d, want 0 for a valid threshold on an empty project\nstderr:\n%s", code, stderr)
		}
	})
}
