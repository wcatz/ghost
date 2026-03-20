package tool

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// initGitRepo creates a minimal git repo in dir for testing.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	cmds := [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
}

func TestGit_BlockedPushForce(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"push --force", []string{"--force"}},
		{"push -f", []string{"-f"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input, _ := json.Marshal(gitInput{
				Subcommand: "push",
				Args:       tt.args,
			})
			r := execGit(context.Background(), t.TempDir(), input)
			if !r.IsError {
				t.Fatal("expected blocked error")
			}
			if !strings.Contains(r.Content, "blocked") {
				t.Errorf("expected 'blocked', got: %s", r.Content)
			}
		})
	}
}

func TestGit_BlockedResetHard(t *testing.T) {
	input, _ := json.Marshal(gitInput{
		Subcommand: "reset",
		Args:       []string{"--hard"},
	})
	r := execGit(context.Background(), t.TempDir(), input)
	if !r.IsError {
		t.Fatal("expected blocked error")
	}
	if !strings.Contains(r.Content, "blocked") {
		t.Errorf("expected 'blocked', got: %s", r.Content)
	}
}

func TestGit_BlockedCleanVariants(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"clean -f", []string{"-f"}},
		{"clean -fd", []string{"-fd"}},
		{"clean -fdx", []string{"-fdx"}},
		{"clean -xf", []string{"-xf"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input, _ := json.Marshal(gitInput{
				Subcommand: "clean",
				Args:       tt.args,
			})
			r := execGit(context.Background(), t.TempDir(), input)
			if !r.IsError {
				t.Fatalf("expected blocked for git clean %v", tt.args)
			}
			if !strings.Contains(r.Content, "blocked") {
				t.Errorf("expected 'blocked', got: %s", r.Content)
			}
		})
	}
}

func TestGit_CleanDryRunAllowed(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	input, _ := json.Marshal(gitInput{
		Subcommand: "clean",
		Args:       []string{"-n"}, // dry run, no 'f'
	})
	r := execGit(context.Background(), dir, input)
	if r.IsError {
		t.Fatalf("clean -n should be allowed: %s", r.Content)
	}
}

func TestGit_BlockedRepoRedirect(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"--git-dir", []string{"--git-dir=/tmp/evil"}},
		{"--work-tree", []string{"--work-tree=/tmp/evil"}},
		{"-c flag", []string{"-c", "safe.directory=/tmp"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input, _ := json.Marshal(gitInput{
				Subcommand: "status",
				Args:       tt.args,
			})
			r := execGit(context.Background(), t.TempDir(), input)
			if !r.IsError {
				t.Fatal("expected blocked error for repo redirect")
			}
			if !strings.Contains(r.Content, "blocked") {
				t.Errorf("expected 'blocked', got: %s", r.Content)
			}
		})
	}
}

func TestGit_BlockedSubcommandC(t *testing.T) {
	input, _ := json.Marshal(gitInput{
		Subcommand: "-C",
		Args:       []string{"/tmp/evil", "status"},
	})
	r := execGit(context.Background(), t.TempDir(), input)
	if !r.IsError {
		t.Fatal("expected blocked for -C subcommand")
	}
}

func TestGit_ReadOnlyCommands(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	readOnly := []string{"status", "diff", "log", "branch", "remote", "rev-parse", "ls-files"}
	for _, sub := range readOnly {
		t.Run(sub, func(t *testing.T) {
			input, _ := json.Marshal(gitInput{Subcommand: sub})
			r := execGit(context.Background(), dir, input)
			// Read-only commands should succeed in a valid git repo.
			if r.IsError && !strings.Contains(r.Content, "does not have any commits") {
				t.Errorf("git %s should work: %s", sub, r.Content)
			}
		})
	}
}

func TestGit_ReadOnlyClassification(t *testing.T) {
	for cmd := range readOnlyGitCmds {
		if !readOnlyGitCmds[cmd] {
			t.Errorf("%q should be read-only", cmd)
		}
	}
	// Verify write commands are NOT in the read-only map.
	writeCmds := []string{"add", "commit", "checkout", "merge", "rebase", "push", "pull", "stash"}
	for _, cmd := range writeCmds {
		if readOnlyGitCmds[cmd] {
			t.Errorf("%q should NOT be read-only", cmd)
		}
	}
}

func TestGit_StatusInRepo(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	input, _ := json.Marshal(gitInput{Subcommand: "status"})
	r := execGit(context.Background(), dir, input)
	if r.IsError {
		t.Fatalf("git status failed: %s", r.Content)
	}
}

func TestGit_InvalidJSON(t *testing.T) {
	r := execGit(context.Background(), "/tmp", json.RawMessage(`{bad`))
	if !r.IsError {
		t.Fatal("expected error for invalid JSON")
	}
}
