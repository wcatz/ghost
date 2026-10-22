package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseMCPClientRejectsUnknownArguments pins that parseMCPClient refuses
// anything it does not recognize, including the misspelled forms that used to
// be silently dropped, while still accepting both --client spellings.
func TestParseMCPClientRejectsUnknownArguments(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{name: "dry-run equals form", args: []string{"--dry-run=true"}, wantErr: true},
		{name: "dryrun", args: []string{"--dryrun"}, wantErr: true},
		{name: "misspelled client flag", args: []string{"--clien"}, wantErr: true},
		{name: "bare client name", args: []string{"codex"}, wantErr: true},
		{name: "client separate", args: []string{"--client", "codex"}, wantErr: false},
		{name: "client equals", args: []string{"--client=codex"}, wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseMCPClient(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseMCPClient(%v) = nil, want an error", tc.args)
				}
				if !strings.Contains(err.Error(), "unknown") {
					t.Errorf("error = %q, want it to name an unknown argument", err)
				}
			} else if err != nil {
				t.Fatalf("parseMCPClient(%v) = %v, want no error", tc.args, err)
			}
		})
	}
}

// TestRunMCPInitRefusesUnknownFlagWithoutWriting builds the real binary and
// confirms an unknown flag stops the run before anything is installed.
// When run on origin/main (via a copied worktree), this test fails because
// the buggy code writes files despite the unknown flag. On the fixed branch
// it passes because the unknown flag is rejected before any write.
func TestRunMCPInitRefusesUnknownFlagWithoutWriting(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the ghost binary")
	}
	dir := t.TempDir()

	home := filepath.Join(dir, "home")
	data := filepath.Join(dir, "data")
	config := filepath.Join(dir, "config")
	for _, d := range []string{home, data, config} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(binDir, "claude")
	if err := os.WriteFile(claude, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Build the ghost binary from the current source tree into binDir.
	// When this test runs in a worktree checked out to origin/main, it builds
	// the buggy version; on the fixed branch it builds the fixed version.
	bin := filepath.Join(binDir, "ghost")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	// PATH is ONLY binDir (the built ghost + fake claude), no host PATH.
	// This ensures the test uses the binary we just built.
	cmd := exec.Command(bin, "mcp", "init", "--client", "claude", "--clien", "codex")
	cmd.Env = []string{
		"HOME=" + home,
		"XDG_DATA_HOME=" + data,
		"XDG_CONFIG_HOME=" + config,
		"PATH=" + binDir,
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	ee, ok := err.(*exec.ExitError)

	// Check whether any files were created under home/data/config.
	filesCreated := false
	for _, root := range []string{home, data, config} {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				filesCreated = true
			}
			return nil
		})
	}

	// On the fixed branch: exit 1, "unknown" in stderr, no files created.
	// On origin/main: exit 0 (bug), files created.
	if !ok || ee.ExitCode() != 1 {
		if filesCreated {
			t.Fatalf("exit = %v (want 1), and files were created under home/data/config — this is the origin/main bug", err)
		}
		t.Fatalf("exit = %v, want exit status 1; stderr: %s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "unknown") {
		t.Errorf("stderr = %q, want an unknown-argument refusal", stderr.String())
	}
	if filesCreated {
		t.Fatalf("files were created under home/data/config despite unknown flag — this is the origin/main bug")
	}
}
