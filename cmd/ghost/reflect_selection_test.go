package main

// `ghost reflect --tier` and `--source` are refused outside their documented
// sets. Before, any other value reached the tier switch's "auto" default and
// ran the SQLite floor with exit 0 — with --apply it rewrote the project's
// memories through the mechanical tier. The exit code, the stderr and the
// untouched store are the contract, so the refusal is driven through the built
// binary; the accepted sets are pinned on the validator directly.

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

const (
	reflectTierAllowed   = "allowed: auto, cli, opencode, sqlite"
	reflectSourceAllowed = "allowed: claude-code, opencode, codex, goose"
)

func TestValidateReflectSelectionAcceptsTheDocumentedSets(t *testing.T) {
	for _, tier := range []string{"auto", "cli", "opencode", "sqlite"} {
		for _, source := range []string{"", "claude-code", "opencode", "codex", "goose"} {
			args := []string{"myproj", "--tier", tier}
			if source != "" {
				args = append(args, "--source="+source)
			}
			p, err := parseReflectArgs(args)
			if err != nil {
				t.Fatalf("parseReflectArgs(%v): %v", args, err)
			}
			if p.tier != tier || p.source != source {
				t.Fatalf("parseReflectArgs(%v) = tier %q source %q", args, p.tier, p.source)
			}
			if err := validateReflectSelection(p); err != nil {
				t.Errorf("validateReflectSelection(%v) refused a documented value: %v", args, err)
			}
		}
	}
	// The removed tier passes through to the switch, which answers it with
	// its own removal message rather than the generic refusal.
	if err := validateReflectSelection(reflectArgs{project: "myproj", tier: "haiku"}); err != nil {
		t.Errorf("haiku must reach the tier switch's removal message, got %v", err)
	}
}

func TestValidateReflectSelectionRefusesAnUnknownValue(t *testing.T) {
	for _, tier := range []string{"cil", "sqlit", "AUTO", "Sqlite", ""} {
		err := validateReflectSelection(reflectArgs{project: "myproj", tier: tier})
		if err == nil {
			t.Errorf("--tier %q was accepted", tier)
			continue
		}
		if !strings.Contains(err.Error(), "unknown --tier") || !strings.Contains(err.Error(), reflectTierAllowed) {
			t.Errorf("--tier %q refusal %q does not name the allowed values", tier, err)
		}
	}
	for _, source := range []string{"bogus", "claude", "Claude-Code"} {
		err := validateReflectSelection(reflectArgs{project: "myproj", tier: "auto", source: source})
		if err == nil {
			t.Errorf("--source %q was accepted", source)
			continue
		}
		if !strings.Contains(err.Error(), "unknown --source") || !strings.Contains(err.Error(), reflectSourceAllowed) {
			t.Errorf("--source %q refusal %q does not name the allowed values", source, err)
		}
	}
}

// reflectBinaryRun runs the built binary's `reflect` with an environment built
// from an allowlist: a scratch home, config and data directory, and a PATH with
// no harness on it.
func reflectBinaryRun(t *testing.T, bin, home string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"reflect"}, args...)...)
	cmd.Env = []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"XDG_DATA_HOME=" + filepath.Join(home, "data"),
		"PATH=" + filepath.Join(home, "empty-path"),
		// A temp dir of the run's own, so nothing the binary writes or
		// sweeps under os.TempDir() reaches the machine's.
		"TMPDIR=" + t.TempDir(),
		"GHOST_OLLAMA_URL=http://127.0.0.1:1",
		"GHOST_CLI_CLAUDE_BINARY=" + filepath.Join(home, "no-such-claude"),
		"GHOST_CLI_OPENCODE_BINARY=" + filepath.Join(home, "no-such-opencode"),
		"GHOST_CLI_CODEX_BINARY=" + filepath.Join(home, "no-such-codex"),
		"GHOST_CLI_GOOSE_BINARY=" + filepath.Join(home, "no-such-goose"),
	}
	if v, ok := os.LookupEnv("GHOST_DEV_FORBID_DATA_DIR"); ok {
		cmd.Env = append(cmd.Env, "GHOST_DEV_FORBID_DATA_DIR="+v)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return 0, stderr.String()
	}
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("run %v: %v", args, err)
	}
	return ee.ExitCode(), stderr.String()
}

// seedReflectProject writes a project with two memories into home's data
// directory and returns the rows as stored.
func seedReflectProject(t *testing.T, home, project string) []memory.Memory {
	t.Helper()
	dir := filepath.Join(home, "data", "ghost")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	db, err := memory.OpenDB(filepath.Join(dir, "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	s := memory.NewStore(db, nil)
	defer s.Close() //nolint:errcheck
	ctx := context.Background()
	if err := s.EnsureProject(ctx, project, "/tmp/"+project, project); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	for _, m := range []memory.Memory{
		{Category: "convention", Content: "tabs are used for indentation in the go sources", Source: "mcp", Importance: 0.6},
		{Category: "fact", Content: "the release build is produced by the goreleaser workflow", Source: "mcp", Importance: 0.6},
	} {
		if _, err := s.CreateFromCorpus(ctx, project, m); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return readReflectProject(t, home, project)
}

func readReflectProject(t *testing.T, home, project string) []memory.Memory {
	t.Helper()
	db, err := memory.OpenDB(filepath.Join(home, "data", "ghost", "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	s := memory.NewStore(db, nil)
	defer s.Close() //nolint:errcheck
	rows, err := s.GetAll(context.Background(), project, -1)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	return rows
}

func TestReflectRefusesAnUnknownTierOrSourceBeforeTheStore(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the ghost binary")
	}
	bin := filepath.Join(t.TempDir(), "ghost")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	const project = "alpha"

	refusals := []struct {
		name    string
		args    []string
		wantMsg string
	}{
		{"tier cil", []string{"--tier", "cil", "--apply"}, reflectTierAllowed},
		{"tier sqlit", []string{"--tier=sqlit", "--apply"}, reflectTierAllowed},
		{"tier AUTO", []string{"--tier", "AUTO", "--apply"}, reflectTierAllowed},
		{"source bogus", []string{"--source", "bogus", "--apply"}, reflectSourceAllowed},
		{"source bogus under sqlite", []string{"--tier", "sqlite", "--source=bogus", "--apply"}, reflectSourceAllowed},
	}

	// With no store at all, a refused value must not create one: the refusal
	// comes before bootstrap opens the data directory.
	for _, tc := range refusals {
		t.Run("no store/"+tc.name, func(t *testing.T) {
			home := t.TempDir()
			code, stderr := reflectBinaryRun(t, bin, home, append([]string{project}, tc.args...)...)
			if code != 1 {
				t.Fatalf("exit = %d, want 1; stderr:\n%s", code, stderr)
			}
			if !strings.Contains(stderr, tc.wantMsg) {
				t.Errorf("stderr does not name the allowed values (%q):\n%s", tc.wantMsg, stderr)
			}
			if _, err := os.Stat(filepath.Join(home, "data", "ghost", "ghost.db")); !os.IsNotExist(err) {
				t.Errorf("a refused run opened the store (stat err = %v)", err)
			}
		})
	}

	// With a store, --apply under a refused value consolidates nothing.
	for _, tc := range refusals {
		t.Run("seeded/"+tc.name, func(t *testing.T) {
			home := t.TempDir()
			before := seedReflectProject(t, home, project)
			code, stderr := reflectBinaryRun(t, bin, home, append([]string{project}, tc.args...)...)
			if code != 1 {
				t.Fatalf("exit = %d, want 1; stderr:\n%s", code, stderr)
			}
			if !strings.Contains(stderr, tc.wantMsg) {
				t.Errorf("stderr does not name the allowed values (%q):\n%s", tc.wantMsg, stderr)
			}
			if after := readReflectProject(t, home, project); !reflect.DeepEqual(before, after) {
				t.Errorf("a refused run changed the project's memories:\nbefore %+v\nafter  %+v", before, after)
			}
		})
	}

	// A KNOWN source whose binary is missing keeps the documented fallback:
	// a warning and the SQLite floor, or a failure under --require-llm.
	t.Run("known source without its binary falls back", func(t *testing.T) {
		home := t.TempDir()
		seedReflectProject(t, home, project)
		code, stderr := reflectBinaryRun(t, bin, home, project, "--source", "codex")
		if code != 0 {
			t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr)
		}
		if !strings.Contains(stderr, `no CLI binary matches source "codex"`) {
			t.Errorf("the fallback warning is missing:\n%s", stderr)
		}
		code, stderr = reflectBinaryRun(t, bin, home, project, "--source", "codex", "--require-llm")
		if code != 1 {
			t.Fatalf("--require-llm exit = %d, want 1; stderr:\n%s", code, stderr)
		}
		if strings.Contains(stderr, "unknown --source") {
			t.Errorf("a known source was refused as unknown:\n%s", stderr)
		}
	})
}
