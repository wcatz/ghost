package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/reflection"
)

// testDeleteStore returns a real in-memory Store with one project ("proj",
// name "test-project") already created, for exercising runProjectDeleteCore
// against real DeleteProject behavior rather than a mock.
func testDeleteStore(t *testing.T) *memory.Store {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := memory.NewStore(db, logger)

	if err := s.EnsureProject(context.Background(), "proj", "/tmp/proj", "test-project"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return s
}

func TestParseObsidianFlags(t *testing.T) {
	t.Run("both forms and all flags", func(t *testing.T) {
		out, project, interval, err := parseObsidianFlags([]string{"--out", "/v", "--project=ghost", "--interval", "5s"})
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if out != "/v" || project != "ghost" || interval != "5s" {
			t.Fatalf("got out=%q project=%q interval=%q", out, project, interval)
		}
	})
	t.Run("empty is fine", func(t *testing.T) {
		if _, _, _, err := parseObsidianFlags(nil); err != nil {
			t.Fatalf("no flags must not error: %v", err)
		}
	})
	t.Run("unknown flag errors", func(t *testing.T) {
		if _, _, _, err := parseObsidianFlags([]string{"--intervl", "5s"}); err == nil {
			t.Fatal("misspelled flag must error, not silently fall back")
		}
	})
	t.Run("missing value errors", func(t *testing.T) {
		if _, _, _, err := parseObsidianFlags([]string{"--interval"}); err == nil {
			t.Fatal("value flag with no argument must error")
		}
	})
	t.Run("bare positional errors", func(t *testing.T) {
		if _, _, _, err := parseObsidianFlags([]string{"oops"}); err == nil {
			t.Fatal("unexpected positional arg must error")
		}
	})
}

// TestParseMCPClient verifies --client parsing rejects a missing or empty
// value (which previously silently fell back to Claude) and accepts both
// "--client NAME" and "--client=NAME" forms.
func TestParseMCPClient(t *testing.T) {
	t.Run("separate argument", func(t *testing.T) {
		client, err := parseMCPClient([]string{"--client", "opencode"})
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if client != "opencode" {
			t.Fatalf("got %q, want opencode", client)
		}
	})
	t.Run("equals form", func(t *testing.T) {
		client, err := parseMCPClient([]string{"--client=opencode"})
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if client != "opencode" {
			t.Fatalf("got %q, want opencode", client)
		}
	})
	t.Run("absent defaults to empty", func(t *testing.T) {
		client, err := parseMCPClient(nil)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if client != "" {
			t.Fatalf("got %q, want empty (caller supplies default)", client)
		}
	})
	t.Run("missing value errors", func(t *testing.T) {
		if _, err := parseMCPClient([]string{"--client"}); err == nil {
			t.Fatal("--client with no value must error, not silently default")
		}
	})
	t.Run("empty equals value errors", func(t *testing.T) {
		if _, err := parseMCPClient([]string{"--client="}); err == nil {
			t.Fatal("--client= with empty value must error, not silently default")
		}
	})
}

// TestRODSNIsReadOnly guards the obsidian commands' read-only guarantee:
// modernc.org/sqlite honors mode=ro only on file: URI DSNs — with a bare
// path the connection opens silently read-write (verified empirically
// against v1.53.0), which is exactly the regression this test would catch.
func TestRODSNIsReadOnly(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	seed, err := memory.OpenDB(dbPath) // creates the schema read-write
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", roDSN(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck

	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('x', '/x', 'x')`); err == nil {
		t.Fatal("write through the read-only DSN must fail")
	}
	// Reads must still work.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM projects`).Scan(&n); err != nil {
		t.Fatalf("read through the read-only DSN must work: %v", err)
	}
}

// TestRODSNEscapesPath guards against a '?' or '#' in the data-dir path being
// parsed as the query separator or a fragment — which would drop mode=ro
// (opening read-write) or open a different file. It also confirms the DSN
// keeps working end-to-end for such paths.
func TestRODSNEscapesPath(t *testing.T) {
	for _, name := range []string{"plain", "with space", "we?rd", "ha#sh"} {
		t.Run(name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), name, "ghost.db")
			if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
				t.Fatal(err)
			}
			// Seed a DB at exactly dbPath via a correctly-escaped writable URI.
			// (memory.OpenDB uses a bare-path DSN that itself misparses a '?'
			// in the path, so it can't seed these cases — a separate concern.)
			seedDSN := (&url.URL{Scheme: "file", Opaque: (&url.URL{Path: dbPath}).EscapedPath()}).String()
			seed, err := sql.Open("sqlite", seedDSN)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := seed.Exec(`CREATE TABLE canary (x)`); err != nil {
				t.Fatalf("seed at %q: %v", dbPath, err)
			}
			if err := seed.Close(); err != nil {
				t.Fatal(err)
			}

			dsn := roDSN(dbPath)
			if !strings.HasPrefix(dsn, "file:") {
				t.Fatalf("DSN must be a file: URI, got %q", dsn)
			}
			// The raw path separators must not leak into the query: the only
			// '?' in the DSN is the query separator introduced by roDSN, and
			// there must be no unescaped '#'.
			if strings.Count(dsn, "?") != 1 || strings.Contains(dsn, "#") {
				t.Fatalf("path special chars not escaped in DSN: %q", dsn)
			}
			if !strings.Contains(dsn, "mode=ro") {
				t.Fatalf("mode=ro missing from DSN: %q", dsn)
			}

			db, err := sql.Open("sqlite", dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close() //nolint:errcheck
			// Reading the canary proves roDSN resolved to the intended file
			// (not a '?'-truncated or '#'-fragmented wrong path).
			var n int
			if err := db.QueryRow(`SELECT COUNT(*) FROM canary`).Scan(&n); err != nil {
				t.Fatalf("read through DSN for path %q must work: %v", name, err)
			}
			if _, err := db.Exec(`INSERT INTO canary VALUES (1)`); err == nil {
				t.Fatalf("write through read-only DSN for path %q must fail", name)
			}
		})
	}
}

func TestConfirmProjectDeleteName_MatchesExactly(t *testing.T) {
	if !confirmProjectDeleteName("my-project\n", "my-project") {
		t.Error("expected trimmed exact match to confirm")
	}
	if !confirmProjectDeleteName("  my-project  ", "my-project") {
		t.Error("expected surrounding whitespace to be trimmed before comparing")
	}
}

func TestConfirmProjectDeleteName_RejectsMismatch(t *testing.T) {
	if confirmProjectDeleteName("my-projec", "my-project") {
		t.Error("expected a partial/typo'd name not to confirm")
	}
	if confirmProjectDeleteName("", "my-project") {
		t.Error("expected an empty input not to confirm")
	}
}

// TestPrintDeleteSummary_FieldsNotTransposed pins the label-to-value mapping
// with six distinct values (one per field) and a whole-string comparison, so
// swapping any two summary.X arguments inside printDeleteSummary — the same
// bug class the memory-layer fixture in store_test.go was hardened against —
// fails this test instead of passing silently.
func TestPrintDeleteSummary_FieldsNotTransposed(t *testing.T) {
	var out bytes.Buffer
	if err := printDeleteSummary(&out, memory.DeleteProjectSummary{
		ProjectID:   "proj",
		ProjectName: "test-project",
		Memories:    1,
		MemoryLinks: 2,
		Tasks:       3,
		Decisions:   4,
		TokenUsage:  5,
		AuditLog:    6,
	}, "Would delete"); err != nil {
		t.Fatalf("printDeleteSummary: %v", err)
	}

	want := `Would delete "test-project" (proj):
  memories:     1
  memory_links: 2
  tasks:        3
  decisions:    4
  token_usage:  5
  audit_log:    6
`
	if out.String() != want {
		t.Errorf("printDeleteSummary output mismatch:\ngot:\n%s\nwant:\n%s", out.String(), want)
	}
}

// TestRunProjectDeleteCore_MismatchAborts exercises the full confirmation
// gate against a real store: apply=true with a wrong re-typed name at the
// prompt must return an error and leave the project (and its memory)
// completely untouched. This is the spec's "wrong re-typed name aborts
// without deleting" requirement.
func TestRunProjectDeleteCore_MismatchAborts(t *testing.T) {
	store := testDeleteStore(t)
	ctx := context.Background()

	if _, err := store.Create(ctx, "proj", memory.Memory{
		Category: "fact", Content: "must survive an aborted delete", Source: "manual", Importance: 0.5, Tags: []string{},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	var out bytes.Buffer
	err := runProjectDeleteCore(ctx, store, &out, strings.NewReader("wrong-name\n"), "test-project", true)
	if err == nil {
		t.Fatal("expected an error for a mismatched confirmation, got nil")
	}
	if !strings.Contains(err.Error(), "did not match") {
		t.Errorf("expected mismatch error, got: %v", err)
	}

	id, _, rErr := store.ResolveProject(ctx, "test-project")
	if rErr != nil || id == "" {
		t.Errorf("expected project to still exist after aborted delete: id=%q err=%v", id, rErr)
	}
	mems, gErr := store.GetAll(ctx, "proj", 100)
	if gErr != nil {
		t.Fatalf("GetAll: %v", gErr)
	}
	if len(mems) != 1 {
		t.Errorf("expected the seeded memory to survive an aborted delete, got %d memories", len(mems))
	}
	if !strings.Contains(out.String(), "Would delete") {
		t.Errorf("expected dry-run summary in output, got %q", out.String())
	}
	if strings.Contains(out.String(), "\nDeleted ") {
		t.Errorf("did not expect the post-apply 'Deleted' report after an aborted confirmation, got %q", out.String())
	}
}

// TestRunProjectDeleteCore_MatchProceeds is the positive counterpart: the
// correct re-typed name must let the delete proceed and actually remove the
// project and its memories.
func TestRunProjectDeleteCore_MatchProceeds(t *testing.T) {
	store := testDeleteStore(t)
	ctx := context.Background()

	if _, err := store.Create(ctx, "proj", memory.Memory{
		Category: "fact", Content: "should be deleted", Source: "manual", Importance: 0.5, Tags: []string{},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	var out bytes.Buffer
	err := runProjectDeleteCore(ctx, store, &out, strings.NewReader("test-project\n"), "test-project", true)
	if err != nil {
		t.Fatalf("expected a correctly re-typed name to proceed, got error: %v", err)
	}

	if !strings.Contains(out.String(), "Deleted ") {
		t.Errorf("expected the post-apply 'Deleted' report, got %q", out.String())
	}
	id, _, rErr := store.ResolveProject(ctx, "test-project")
	if rErr != nil {
		t.Fatalf("ResolveProject: %v", rErr)
	}
	if id != "" {
		t.Error("expected project to be gone after a correctly confirmed delete")
	}
}

// TestRunProjectDeleteCore_DryRunNeverPrompts confirms apply=false stops
// after the preview and never reads from in or deletes anything, regardless
// of what stdin contains.
func TestRunProjectDeleteCore_DryRunNeverPrompts(t *testing.T) {
	store := testDeleteStore(t)
	ctx := context.Background()

	var out bytes.Buffer
	err := runProjectDeleteCore(ctx, store, &out, strings.NewReader(""), "test-project", false)
	if err != nil {
		t.Fatalf("dry-run should not error: %v", err)
	}
	if !strings.Contains(out.String(), "Would delete") || !strings.Contains(out.String(), "Re-run with --apply") {
		t.Errorf("expected dry-run preview and re-run hint, got %q", out.String())
	}
	id, _, rErr := store.ResolveProject(ctx, "test-project")
	if rErr != nil || id == "" {
		t.Errorf("expected project to still exist after dry-run: id=%q err=%v", id, rErr)
	}
}

// stubBinary writes an executable shell script that prints one opencode
// JSON-lines text event carrying payload, regardless of arguments. It stands
// in for a real `opencode` binary in provider-selection tests.
func stubBinary(t *testing.T, payload string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binary requires a POSIX shell")
	}
	path := filepath.Join(t.TempDir(), "fakeopencode")
	script := "#!/bin/sh\nprintf '%s\\n' '" + payload + "'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return path
}

// TestBuildClassifyProviderForSource_EmptySourceErrors: an empty source is the
// old silent-claude path. It must now fail with the actionable detection error
// before any backend lookup happens — even with a claude binary on PATH.
func TestBuildClassifyProviderForSource_EmptySourceErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\nexit 0"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", dir)
	cfg := &config.Config{}
	_, err := buildClassifyProviderForSource(cfg, "")
	if err == nil || !errors.Is(err, ai.ErrUndetectableHarness) {
		t.Fatalf("want actionable undetectable-harness error, got %v", err)
	}
}

// TestBuildClassifyProviderForSource_UsesConfiguredOpencodeStub: with no binary
// on PATH but an explicit opencode binary configured and a resolved source, the
// returned provider must classify through that opencode binary (the harness's
// zero-Anthropic-spend path). buildClassifyProviderForSource returns
// ai.Provider whose Classify is a plain string verdict.
func TestBuildClassifyProviderForSource_UsesConfiguredOpencodeStub(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	stub := stubBinary(t, `{"type":"text","part":{"type":"text","text":"STUB_CLASSIFY_OK"}}`)
	cfg := &config.Config{}
	cfg.CLI.OpenCodeBinary = stub
	p, err := buildClassifyProviderForSource(cfg, "opencode")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	out, err := p.Classify(context.Background(), "sys prompt", "user content")
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if out != "STUB_CLASSIFY_OK" {
		t.Fatalf("got %q, want STUB_CLASSIFY_OK", out)
	}
}

// TestDetectPhaseSource covers the routing guard for manual reflect/resolve/
// supersede: an explicit --source wins, otherwise the calling harness is
// detected, and an undetectable caller is an error rather than a claude
// default. The undetected case swaps the detection seam because the test
// process itself can be a descendant of a harness (running `go test` from an
// opencode session); the flag and env cases use t.Setenv.
func TestDetectPhaseSource(t *testing.T) {
	t.Run("explicit flag wins over detection", func(t *testing.T) {
		t.Setenv("OPENCODE", "1")
		got, err := detectPhaseSource("claude-code")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "claude-code" {
			t.Fatalf("got %q, want %q", got, "claude-code")
		}
	})

	t.Run("detects the calling harness from the environment", func(t *testing.T) {
		t.Setenv("OPENCODE", "1")
		got, err := detectPhaseSource("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "opencode" {
			t.Fatalf("got %q, want %q", got, "opencode")
		}
	})

	t.Run("undetectable caller is an error", func(t *testing.T) {
		old := detectCallingSource
		detectCallingSource = func() string { return "" }
		t.Cleanup(func() { detectCallingSource = old })
		_, err := detectPhaseSource("")
		if err == nil || !errors.Is(err, ai.ErrUndetectableHarness) {
			t.Fatalf("want actionable error, got %v", err)
		}
	})
}

func TestApplyPhaseModel(t *testing.T) {
	t.Setenv("GHOST_OPENCODE_MODEL", "inherited/model")
	applyPhaseModel("", "claude-code")
	if got := os.Getenv("GHOST_OPENCODE_MODEL"); got != "inherited/model" {
		t.Errorf("empty config must leave the inherited value, got %q", got)
	}
	applyPhaseModel("opencode/big-pickle", "opencode")
	if got := os.Getenv("GHOST_OPENCODE_MODEL"); got != "opencode/big-pickle" {
		t.Errorf("config must pin the model, got %q", got)
	}
}

// TestApplyPhaseModelWarnsOnInertPin pins the stderr warning when a configured
// pin cannot apply: claude/codex/goose have no model flag, so the pin would
// otherwise be silently ignored. An opencode harness (or no harness, e.g. the
// offline sqlite tier before the tier/source resolves) must stay silent.
func TestApplyPhaseModelWarnsOnInertPin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		model   string
		harness string
		wantMsg bool
	}{
		{"opencode honors the pin", "opencode/big-pickle", "opencode", false},
		{"claude-code pin is inert", "opencode/big-pickle", "claude-code", true},
		{"codex pin is inert", "opencode/big-pickle", "codex", true},
		{"goose pin is inert", "opencode/big-pickle", "goose", true},
		{"empty pin never warns", "", "claude-code", false},
		{"no harness never warns", "opencode/big-pickle", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Each subtest starts from a known env state and restores on exit:
			// applyPhaseModel os.Setenvs GHOST_OPENCODE_MODEL, and a leak would
			// make later tests in the package non-deterministic.
			t.Setenv("GHOST_OPENCODE_MODEL", "")
			old := os.Stderr
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatalf("pipe: %v", err)
			}
			os.Stderr = w
			t.Cleanup(func() { os.Stderr = old })

			applyPhaseModel(tc.model, tc.harness)

			_ = w.Close()
			out, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("read stderr: %v", err)
			}
			if got := strings.Contains(string(out), "warning: cli.model_* pin"); got != tc.wantMsg {
				t.Errorf("warning present = %v, want %v (stderr: %q)", got, tc.wantMsg, out)
			}
			if tc.model != "" {
				if got := os.Getenv("GHOST_OPENCODE_MODEL"); got != tc.model {
					t.Errorf("pin must still be set even when warned, GHOST_OPENCODE_MODEL = %q", got)
				}
			}
		})
	}
}

// TestRunAllClients verifies the `--client all` orchestration: banners in run
// order, continue past individual failures, per-failure stderr lines, and the
// failed-names return that drives the exit code.
func TestRunAllClients(t *testing.T) {
	t.Run("continues past failures and reports them", func(t *testing.T) {
		targets := []clientTarget{
			{"ok-first", "", func(w io.Writer, dryRun bool) error { return nil }},
			{"boom", "", func(w io.Writer, dryRun bool) error { return fmt.Errorf("kaput") }},
			{"ok-last", "", func(w io.Writer, dryRun bool) error { return nil }},
			{"boom2", "", func(w io.Writer, dryRun bool) error { return fmt.Errorf("kaput too") }},
		}
		var stdout, stderr bytes.Buffer
		failed := runAllClients(&stdout, &stderr, false, targets)
		if len(failed) != 2 || failed[0] != "boom" || failed[1] != "boom2" {
			t.Fatalf("failed = %v, want [boom boom2]", failed)
		}
		for _, want := range []string{"error (boom): kaput", "error (boom2): kaput too"} {
			if !strings.Contains(stderr.String(), want) {
				t.Errorf("stderr missing %q, got:\n%s", want, stderr.String())
			}
		}
		out := stdout.String()
		firstOK, boomOK, lastOK := strings.Index(out, "=== ok-first ==="), strings.Index(out, "=== boom ==="), strings.Index(out, "=== ok-last ===")
		if firstOK < 0 || boomOK < 0 || lastOK < 0 {
			t.Fatalf("stdout missing a banner, got:\n%s", out)
		}
		if firstOK >= boomOK || boomOK >= lastOK {
			t.Errorf("targets must run in order and failures must not stop the loop, got:\n%s", out)
		}
	})
	t.Run("all succeed returns empty", func(t *testing.T) {
		targets := []clientTarget{
			{"a", "", func(w io.Writer, dryRun bool) error { return nil }},
			{"b", "", func(w io.Writer, dryRun bool) error { return nil }},
		}
		var stdout, stderr bytes.Buffer
		if failed := runAllClients(&stdout, &stderr, true, targets); len(failed) != 0 {
			t.Fatalf("failed = %v, want empty", failed)
		}
		if stderr.Len() != 0 {
			t.Errorf("stderr should stay clean when nothing fails, got:\n%s", stderr.String())
		}
	})
}

// TestMCPInitTargets_CoverAllClients guards the target list against accidental
// drift: every supported client installer appears exactly once, in order.
func TestMCPInitTargets_CoverAllClients(t *testing.T) {
	want := []string{"claude", "opencode", "codex", "goose"}
	targets := mcpInitTargets()
	if len(targets) != len(want) {
		t.Fatalf("got %d targets (%v), want %d", len(targets), targetNames(targets), len(want))
	}
	for i, name := range want {
		if targets[i].name != name {
			t.Errorf("targets[%d] = %q, want %q", i, targets[i].name, name)
		}
		if targets[i].run == nil {
			t.Errorf("target %q has no installer", name)
		}
	}
}

func targetNames(targets []clientTarget) []string {
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = t.name
	}
	return names
}

// TestDetectClients verifies that detectClients returns only the names of
// clients whose binaries are findable on PATH.
func TestDetectClients(t *testing.T) {
	// Create a temp dir with stub binaries for two of the four clients.
	dir := t.TempDir()
	writeStub(t, dir, "opencode")
	writeStub(t, dir, "goose")

	// Override PATH to include only our temp dir.
	t.Setenv("PATH", dir)

	got := detectClients()
	want := []string{"opencode", "goose"}
	if len(got) != len(want) {
		t.Fatalf("detectClients() = %v, want %v", got, want)
	}
	for i, name := range want {
		if got[i] != name {
			t.Errorf("detectClients()[%d] = %q, want %q", i, got[i], name)
		}
	}
}

// TestDetectClients_EmptyPath verifies that detectClients returns nil when
// no client binaries are on PATH.
func TestDetectClients_EmptyPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	got := detectClients()
	if len(got) != 0 {
		t.Fatalf("detectClients() = %v, want empty", got)
	}
}

// writeStub creates an executable stub script at binDir/name.
func writeStub(t *testing.T, binDir, name string) string {
	t.Helper()
	path := filepath.Join(binDir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestMCPLogConfig_QuietWhenSpawned guards the #373 fix: a client-spawned
// server (stderr not a terminal) must drop routine INFO logs from stderr so
// MCP clients that surface stderr don't leak them into the UI.
func TestMCPLogConfig_QuietWhenSpawned(t *testing.T) {
	t.Setenv("GHOST_LOG_FILE", "")
	t.Setenv("GHOST_DEBUG", "")
	writer, level, closeLog := mcpLogConfig(false)
	if level != slog.LevelWarn {
		t.Fatalf("level = %v, want Warn", level)
	}
	if writer != io.Writer(os.Stderr) {
		t.Fatal("writer must be stderr")
	}
	if closeLog != nil {
		t.Fatal("no file opened, closer must be nil")
	}
}

func TestMCPLogConfig_InfoWhenTerminal(t *testing.T) {
	t.Setenv("GHOST_LOG_FILE", "")
	t.Setenv("GHOST_DEBUG", "")
	writer, level, closeLog := mcpLogConfig(true)
	if level != slog.LevelInfo {
		t.Fatalf("level = %v, want Info (interactive debugging)", level)
	}
	if writer != io.Writer(os.Stderr) {
		t.Fatal("writer must be stderr")
	}
	if closeLog != nil {
		t.Fatal("no file opened, closer must be nil")
	}
}

// TestMCPLogConfig_FileRedirect: GHOST_LOG_FILE must divert the full stream
// (INFO included) to the file even when spawned by a client, keeping stderr
// clean while preserving logs for debugging.
func TestMCPLogConfig_FileRedirect(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "ghost.log")
	t.Setenv("GHOST_LOG_FILE", logPath)
	t.Setenv("GHOST_DEBUG", "")
	writer, level, closeLog := mcpLogConfig(false)
	if level != slog.LevelInfo {
		t.Fatalf("level = %v, want Info (full stream to file)", level)
	}
	if closeLog == nil {
		t.Fatal("file opened, closer must be non-nil")
	}
	f, ok := writer.(*os.File)
	if !ok {
		t.Fatalf("writer is %T, want *os.File", writer)
	}
	logger := slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: level}))
	logger.Info("hello from test")
	closeLog()
	if _, err := f.Write([]byte("after close")); err == nil {
		t.Fatal("closer must actually close the file")
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "hello from test") {
		t.Fatalf("log file missing the info line, got %q", data)
	}
}

func TestMCPLogConfig_DebugWins(t *testing.T) {
	t.Setenv("GHOST_LOG_FILE", "")
	t.Setenv("GHOST_DEBUG", "1")
	_, level, _ := mcpLogConfig(false)
	if level != slog.LevelDebug {
		t.Fatalf("level = %v, want Debug", level)
	}
}

// TestMCPLogConfig_UnopenableFileFallsBackQuiet: a GHOST_LOG_FILE that cannot
// be opened must not silently restore INFO logging to stderr in client-spawned
// mode — that would leak the very noise the quiet default removes.
func TestMCPLogConfig_UnopenableFileFallsBackQuiet(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "adir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GHOST_LOG_FILE", dir) // existing directory: open for write fails
	t.Setenv("GHOST_DEBUG", "")
	writer, level, closeLog := mcpLogConfig(false)
	if level != slog.LevelWarn {
		t.Fatalf("level = %v, want Warn (quiet fallback)", level)
	}
	if writer != io.Writer(os.Stderr) {
		t.Fatal("writer must fall back to stderr")
	}
	if closeLog != nil {
		t.Fatal("no file opened, closer must be nil")
	}
}

func TestRunProjectMergeCore(t *testing.T) {
	newStore := func(t *testing.T) *memory.Store {
		t.Helper()
		db, err := memory.OpenDB(":memory:")
		if err != nil {
			t.Fatalf("OpenDB: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
		s := memory.NewStore(db, logger)
		if err := s.EnsureProject(context.Background(), "old-proj", "/tmp/old", "old-name"); err != nil {
			t.Fatalf("EnsureProject old: %v", err)
		}
		if err := s.EnsureProject(context.Background(), "new-proj", "/tmp/new", "new-name"); err != nil {
			t.Fatalf("EnsureProject new: %v", err)
		}
		return s
	}

	t.Run("merges by name and preserves memory IDs", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()
		id, err := store.Create(ctx, "old-proj", memory.Memory{
			Category: "fact", Content: "a fact that must survive the merge", Source: "manual", Importance: 0.5, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}

		var out bytes.Buffer
		if err := runProjectMergeCore(ctx, store, &out, "old-name", "new-name"); err != nil {
			t.Fatalf("runProjectMergeCore: %v", err)
		}

		got, err := store.GetAll(ctx, "new-proj", 10)
		if err != nil {
			t.Fatalf("GetAll after merge: %v", err)
		}
		found := false
		for _, m := range got {
			if m.ID == id {
				found = true
			}
		}
		if !found {
			t.Fatalf("memory %s missing from new-proj after merge", id)
		}
		for _, m := range got {
			if m.ID == id && m.ProjectID != "new-proj" {
				t.Errorf("memory project_id = %q, want new-proj", m.ProjectID)
			}
		}
		oldID, _, _ := store.ResolveProject(ctx, "old-name")
		if oldID != "" {
			t.Errorf("old project still resolves after merge: %q", oldID)
		}
	})

	t.Run("same project refused", func(t *testing.T) {
		store := newStore(t)
		var out bytes.Buffer
		err := runProjectMergeCore(context.Background(), store, &out, "new-name", "new-proj")
		if err == nil || !strings.Contains(err.Error(), "itself") {
			t.Fatalf("expected self-merge refusal, got: %v", err)
		}
	})

	t.Run("unknown project errors with known listing", func(t *testing.T) {
		store := newStore(t)
		var out bytes.Buffer
		err := runProjectMergeCore(context.Background(), store, &out, "nope", "new-name")
		if err == nil || !strings.Contains(err.Error(), `project "nope" not found`) {
			t.Fatalf("expected not-found error, got: %v", err)
		}
	})
}

func TestRunProjectMergeCore_RefusesGlobal(t *testing.T) {
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store := memory.NewStore(db, logger)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "proj", "/tmp/proj", "test-project"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if err := store.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject _global: %v", err)
	}

	for _, tc := range []struct{ oldArg, newArg string }{
		{"_global", "test-project"},
		{"global", "test-project"}, // resolves to _global by name
		{"test-project", "_global"},
	} {
		var out bytes.Buffer
		err := runProjectMergeCore(ctx, store, &out, tc.oldArg, tc.newArg)
		if err == nil || !strings.Contains(err.Error(), "_global") {
			t.Errorf("merge %q -> %q: expected _global refusal, got: %v", tc.oldArg, tc.newArg, err)
		}
	}
}

// TestLifecyclePhasesOrder pins the ordering contract that the single-process
// coordinator exists to enforce: reflect (a rewrite) must run before resolve
// (stamps resolved_at) and supersede (links rows).
func TestLifecyclePhasesOrder(t *testing.T) {
	cfg := &config.Config{}
	cfg.Reflection.AutoReflect = true
	cfg.Reflection.AutoResolve = true
	cfg.Reflection.AutoSupersede = true

	phases := lifecyclePhases(cfg, "proj", true)
	got := make([]string, 0, len(phases))
	for _, p := range phases {
		got = append(got, p.name)
	}
	want := []string{"reflect", "resolve", "supersede"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("phase order = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(phases[0].args, []string{"reflect", "--project", "proj", "--apply", "--require-llm", "--skip-unchanged"}) {
		t.Errorf("reflect args = %v", phases[0].args)
	}
	if !reflect.DeepEqual(phases[1].args, []string{"resolve", "--project", "proj", "--apply"}) {
		t.Errorf("resolve args = %v", phases[1].args)
	}
	if !reflect.DeepEqual(phases[2].args, []string{"supersede", "--project", "proj", "--apply"}) {
		t.Errorf("supersede args = %v", phases[2].args)
	}
}

// TestLifecyclePhasesSkipsReflectWithoutLLM: without a real LLM tier an
// unattended consolidation would rewrite every non-manual memory through the
// Jaccard-only sqlite tier, so the reflect phase must be dropped while the
// non-LLM phases still run.
func TestLifecyclePhasesSkipsReflectWithoutLLM(t *testing.T) {
	cfg := &config.Config{}
	cfg.Reflection.AutoReflect = true
	cfg.Reflection.AutoResolve = true
	cfg.Reflection.AutoSupersede = true

	phases := lifecyclePhases(cfg, "proj", false)
	got := make([]string, 0, len(phases))
	for _, p := range phases {
		got = append(got, p.name)
	}
	want := []string{"resolve", "supersede"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("phase order without LLM = %v, want %v", got, want)
	}
}

func TestReflectSkipDecision(t *testing.T) {
	cases := []struct {
		skip, apply bool
		stored, cur string
		want        bool
	}{
		{true, true, "abc", "abc", true},
		{true, true, "abc", "def", false},
		{true, true, "", "abc", false},     // nothing recorded yet
		{true, false, "abc", "abc", false}, // dry-run never skips
		{false, true, "abc", "abc", false}, // manual run always executes
	}
	for _, c := range cases {
		if got := reflectSkipDecision(c.skip, c.apply, c.stored, c.cur); got != c.want {
			t.Errorf("reflectSkipDecision(%v,%v,%q,%q) = %v, want %v", c.skip, c.apply, c.stored, c.cur, got, c.want)
		}
	}
}

func TestReflectMaySkipDoesNotSkipExplicitPromotion(t *testing.T) {
	if reflectMaySkip(true, true, true, "same", "same") {
		t.Fatal("explicit --promote-globals was skipped as unchanged")
	}
	if !reflectMaySkip(true, true, false, "same", "same") {
		t.Fatal("ordinary unchanged apply should still skip")
	}
}

func TestConsolidatableFilters(t *testing.T) {
	now := "2026-09-21 00:00:00"
	mems := []memory.Memory{
		{ID: "keep", CreatedAt: now},
		{ID: "resolved", ResolvedAt: &now},
		{ID: "pinned", Pinned: true},
		{ID: "manual", Source: "manual"},
	}
	got := consolidatable(mems)
	if len(got) != 1 || got[0].ID != "keep" {
		t.Fatalf("consolidatable = %+v, want only keep", got)
	}
}

func TestLifecyclePhasesEmptyWhenAllDisabled(t *testing.T) {
	if phases := lifecyclePhases(&config.Config{}, "proj", true); len(phases) != 0 {
		t.Fatalf("expected no phases when everything is disabled, got %v", phases)
	}
}

// TestLifecyclePhasesTimeoutFromConfig: zero explicitly disables the phase
// bound (the loaded default is a generous 60 minutes), and any configured value
// propagates to every phase.
func TestLifecyclePhasesTimeoutFromConfig(t *testing.T) {
	cfg := &config.Config{}
	cfg.Reflection.AutoReflect = true
	cfg.Reflection.AutoResolve = true
	cfg.Reflection.AutoSupersede = true

	for _, p := range lifecyclePhases(cfg, "proj", true) {
		if p.timeout != 0 {
			t.Errorf("phase %s timeout = %v, want 0 (bound disabled)", p.name, p.timeout)
		}
	}

	cfg.Reflection.LifecycleTimeoutMinutes = 45
	phases := lifecyclePhases(cfg, "proj", true)
	if len(phases) != 3 {
		t.Fatalf("expected 3 phases, got %d", len(phases))
	}
	for _, p := range phases {
		if p.timeout != 45*time.Minute {
			t.Errorf("phase %s timeout = %v, want 45m", p.name, p.timeout)
		}
	}
}

// TestParseLifecycleArgs pins the argv contract that keeps the coordinator from
// running its write phases against the wrong project. The misroute this guards
// is concrete: the hook used to pass the project positionally, so a project
// literally named "--source" realigned the argv and lifecycle ran for whatever
// followed, while the pid file was claimed for the real project. --project
// therefore takes the NEXT argument verbatim — a dash-prefixed name given that
// way is a name, and the phase subcommands accept the same form — while a BARE
// dash-prefixed positional stays an unknown-flag error, because it is
// indistinguishable from one.
func TestParseLifecycleArgs(t *testing.T) {
	ok := []struct {
		args    []string
		project string
		source  string
	}{
		{[]string{"--project", "ghost"}, "ghost", ""},
		{[]string{"--project", "my project", "--source", "opencode"}, "my project", "opencode"},
		{[]string{"ghost"}, "ghost", ""}, // positional still works for manual use
		{[]string{"ghost", "--source", "cli"}, "ghost", "cli"},
		{[]string{"--project", "-dashy"}, "-dashy", ""}, // dash-prefixed names round-trip through the phases now
		{[]string{"--project", "--odd"}, "--odd", ""},
		{[]string{"--project", "--source"}, "--source", ""}, // value is verbatim, not re-aligned
	}
	for _, tc := range ok {
		project, source, err := parseLifecycleArgs(tc.args)
		if err != nil {
			t.Errorf("parseLifecycleArgs(%v) error: %v", tc.args, err)
			continue
		}
		if project != tc.project || source != tc.source {
			t.Errorf("parseLifecycleArgs(%v) = (%q, %q), want (%q, %q)",
				tc.args, project, source, tc.project, tc.source)
		}
	}

	bad := [][]string{
		{},                                   // no project
		{"--project"},                        // missing value
		{"--source", "x"},                    // no project
		{"--bogus"},                          // unknown flag
		{"a", "b"},                           // extra positional
		{"--project", "a", "b"},              // extra positional after flag
		{"--project", "a", "--project", "b"}, // duplicate flag must not last-win
		{"--project", "a", "--source", "s", "--source", "t"}, // duplicate --source
		{"-dashy"}, // bare dash-prefixed positional: indistinguishable from a flag; use --project -dashy
	}
	for _, args := range bad {
		if _, _, err := parseLifecycleArgs(args); err == nil {
			t.Errorf("parseLifecycleArgs(%v) = nil error, want an error", args)
		}
	}
}

// TestParseReflectArgs pins `ghost reflect` argv parsing: the historical
// positional form (with any accepted flag combination) parses unchanged, and
// --project takes the NEXT argument verbatim — including dash-leading names
// such as -x or --odd — so they read as project names, not flags.
// --project=VALUE matches the parser's other equals-form flags, a valueless
// --project is a clear error, and unknown flags stay silently ignored
// (reflect's historical behavior; resolve/supersede error on them).
func TestParseReflectArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want reflectArgs
	}{
		{"positional", []string{"myproj"}, reflectArgs{project: "myproj", tier: "auto"}},
		{"positional with apply", []string{"myproj", "--apply"}, reflectArgs{project: "myproj", tier: "auto", apply: true}},
		{"promotion is opt-in", []string{"myproj", "--apply", "--promote-globals"},
			reflectArgs{project: "myproj", tier: "auto", apply: true, promoteGlobals: true}},
		{"promotion with lifecycle shape", []string{"--project", "myproj", "--apply", "--require-llm", "--skip-unchanged", "--promote-globals", "--source", "claude"},
			reflectArgs{project: "myproj", tier: "auto", apply: true, requireLLM: true, skipUnchanged: true, promoteGlobals: true, source: "claude"}},
		{"positional with lifecycle flags", []string{"myproj", "--apply", "--require-llm", "--skip-unchanged", "--source", "claude"},
			reflectArgs{project: "myproj", tier: "auto", apply: true, requireLLM: true, skipUnchanged: true, source: "claude"}},
		{"tier separate value", []string{"myproj", "--tier", "cli"}, reflectArgs{project: "myproj", tier: "cli"}},
		{"tier equals value", []string{"myproj", "--tier=cli"}, reflectArgs{project: "myproj", tier: "cli"}},
		{"restore allow-drops source-equals", []string{"myproj", "--restore", "--allow-drops", "--source=codex"},
			reflectArgs{project: "myproj", tier: "auto", restore: true, allowDrops: true, source: "codex"}},
		{"last positional wins", []string{"a", "b"}, reflectArgs{project: "b", tier: "auto"}},
		{"unknown flag ignored", []string{"myproj", "--wat"}, reflectArgs{project: "myproj", tier: "auto"}},
		{"project flag dash value", []string{"--project", "-x", "--apply"}, reflectArgs{project: "-x", tier: "auto", apply: true}},
		{"project flag double-dash value", []string{"--project", "--odd"}, reflectArgs{project: "--odd", tier: "auto"}},
		{"project flag lifecycle shape", []string{"--project", "-myproj", "--apply", "--require-llm", "--skip-unchanged", "--source", "claude"},
			reflectArgs{project: "-myproj", tier: "auto", apply: true, requireLLM: true, skipUnchanged: true, source: "claude"}},
		{"project equals form", []string{"--project=-eq"}, reflectArgs{project: "-eq", tier: "auto"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseReflectArgs(tc.args)
			if err != nil {
				t.Fatalf("parseReflectArgs(%v): %v", tc.args, err)
			}
			if got != tc.want {
				t.Errorf("parseReflectArgs(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"project missing value", []string{"--apply", "--project"}},
		{"project empty equals value", []string{"--project="}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseReflectArgs(tc.args)
			if err == nil {
				t.Fatalf("parseReflectArgs(%v) must reject a valueless --project", tc.args)
			}
			if !strings.Contains(err.Error(), "--project requires a value") {
				t.Errorf("error %q must name the missing --project value", err)
			}
		})
	}
}

// TestParseResolveArgs pins `ghost resolve` argv parsing: exactly one project,
// positionally (unchanged back-compat) or via --project, which takes the NEXT
// argument verbatim so dash-leading names parse as names; a second project in
// any mixture stays the "expected exactly one project" error; valueless
// --project and unknown flags stay clear errors.
func TestParseResolveArgs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		project string
		source  string
		apply   bool
	}{
		{"positional", []string{"myproj"}, "myproj", "", false},
		{"positional with apply", []string{"myproj", "--apply"}, "myproj", "", true},
		{"source separate value", []string{"myproj", "--source", "opencode"}, "myproj", "opencode", false},
		{"source equals value", []string{"myproj", "--source=codex"}, "myproj", "codex", false},
		{"project flag dash value", []string{"--project", "-x", "--apply"}, "-x", "", true},
		{"project flag double-dash value", []string{"--project", "--odd"}, "--odd", "", false},
		{"project flag lifecycle shape", []string{"--project", "-myproj", "--apply", "--source", "claude"}, "-myproj", "claude", true},
		{"project equals form", []string{"--project=-eq"}, "-eq", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project, source, apply, err := parseResolveArgs(tc.args)
			if err != nil {
				t.Fatalf("parseResolveArgs(%v): %v", tc.args, err)
			}
			if project != tc.project || source != tc.source || apply != tc.apply {
				t.Errorf("parseResolveArgs(%v) = (%q, %q, %v), want (%q, %q, %v)",
					tc.args, project, source, apply, tc.project, tc.source, tc.apply)
			}
		})
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"two positionals", []string{"a", "b"}, "expected exactly one project"},
		{"positional then project flag", []string{"a", "--project", "b"}, "expected exactly one project"},
		{"project flag then positional", []string{"--project", "a", "b"}, "expected exactly one project"},
		{"duplicate project flag", []string{"--project", "a", "--project", "b"}, "expected exactly one project"},
		{"project missing value", []string{"--apply", "--project"}, "--project requires a value"},
		{"project empty equals value", []string{"--project="}, "--project requires a value"},
		{"unknown flag", []string{"myproj", "--bogus"}, `unknown flag "--bogus"`},
		{"source missing value", []string{"myproj", "--source"}, `unknown flag "--source"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := parseResolveArgs(tc.args)
			if err == nil {
				t.Fatalf("parseResolveArgs(%v) must fail", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q must contain %q", err, tc.want)
			}
		})
	}
}

// TestParseSupersedeArgs pins `ghost supersede` argv parsing: same shapes as
// resolve except the project is last-wins across positionals (historical
// behavior) and --threshold exists in both value forms, defaulting to 0.80
// when the value does not parse.
func TestParseSupersedeArgs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		project   string
		source    string
		apply     bool
		threshold float32
	}{
		{"positional", []string{"myproj"}, "myproj", "", false, 0.80},
		{"positional with apply", []string{"myproj", "--apply"}, "myproj", "", true, 0.80},
		{"threshold separate value", []string{"myproj", "--threshold", "0.5"}, "myproj", "", false, 0.5},
		{"threshold equals value", []string{"myproj", "--threshold=0.25"}, "myproj", "", false, 0.25},
		{"threshold bad value keeps default", []string{"myproj", "--threshold", "abc"}, "myproj", "", false, 0.80},
		{"source separate value", []string{"myproj", "--source", "opencode"}, "myproj", "opencode", false, 0.80},
		{"source equals value", []string{"myproj", "--source=codex"}, "myproj", "codex", false, 0.80},
		{"last positional wins", []string{"a", "b"}, "b", "", false, 0.80},
		{"project flag dash value", []string{"--project", "-x", "--apply"}, "-x", "", true, 0.80},
		{"project flag double-dash value", []string{"--project", "--odd"}, "--odd", "", false, 0.80},
		{"project flag lifecycle shape", []string{"--project", "-myproj", "--apply", "--source", "claude"}, "-myproj", "claude", true, 0.80},
		{"project equals form", []string{"--project=-eq"}, "-eq", "", false, 0.80},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project, source, apply, threshold, err := parseSupersedeArgs(tc.args)
			if err != nil {
				t.Fatalf("parseSupersedeArgs(%v): %v", tc.args, err)
			}
			if project != tc.project || source != tc.source || apply != tc.apply || threshold != tc.threshold {
				t.Errorf("parseSupersedeArgs(%v) = (%q, %q, %v, %v), want (%q, %q, %v, %v)",
					tc.args, project, source, apply, threshold, tc.project, tc.source, tc.apply, tc.threshold)
			}
		})
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"project missing value", []string{"--apply", "--project"}, "--project requires a value"},
		{"project empty equals value", []string{"--project="}, "--project requires a value"},
		{"unknown flag", []string{"myproj", "--bogus"}, `unknown flag "--bogus"`},
		{"threshold missing value", []string{"myproj", "--threshold"}, `unknown flag "--threshold"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, _, err := parseSupersedeArgs(tc.args)
			if err == nil {
				t.Fatalf("parseSupersedeArgs(%v) must fail", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q must contain %q", err, tc.want)
			}
		})
	}
}

// TestDashProjectLifecycleRoundTrip proves a dash-prefixed project name is
// auto-consolidatable end to end: the coordinator's --project entry parse
// accepts it (no fail-fast guard), every phase argv lifecyclePhases emits
// parses back to exactly that name — not a flag — with its flags intact, and
// the name resolves in the store the way each phase's resolveProjectOrExit
// resolves it.
func TestDashProjectLifecycleRoundTrip(t *testing.T) {
	proj, _, err := parseLifecycleArgs([]string{"--project", "-dashy"})
	if err != nil {
		t.Fatalf("parseLifecycleArgs --project -dashy: %v", err)
	}
	cfg := &config.Config{}
	cfg.Reflection.AutoReflect = true
	cfg.Reflection.AutoResolve = true
	cfg.Reflection.AutoSupersede = true
	phases := lifecyclePhases(cfg, proj, true)
	if len(phases) != 3 {
		t.Fatalf("expected 3 phases, got %d", len(phases))
	}

	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store := memory.NewStore(db, logger)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "dash-id", "/tmp/-dashy", "-dashy"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	for _, ph := range phases {
		args := ph.args[1:] // drop the subcommand word
		var got string
		var apply bool
		switch ph.name {
		case "reflect":
			p, perr := parseReflectArgs(args)
			if perr != nil {
				t.Fatalf("parseReflectArgs(%v): %v", ph.args, perr)
			}
			got, apply = p.project, p.apply
			if !p.requireLLM || !p.skipUnchanged {
				t.Errorf("reflect phase flags lost: %+v", p)
			}
		case "resolve":
			project, _, a, perr := parseResolveArgs(args)
			if perr != nil {
				t.Fatalf("parseResolveArgs(%v): %v", ph.args, perr)
			}
			got, apply = project, a
		case "supersede":
			project, _, a, _, perr := parseSupersedeArgs(args)
			if perr != nil {
				t.Fatalf("parseSupersedeArgs(%v): %v", ph.args, perr)
			}
			got, apply = project, a
		default:
			t.Fatalf("unexpected phase %q", ph.name)
		}
		if got != "-dashy" {
			t.Errorf("%s phase parsed project %q, want -dashy (argv %v)", ph.name, got, ph.args)
		}
		if !apply {
			t.Errorf("%s phase lost --apply: %v", ph.name, ph.args)
		}
		id, _, rerr := store.ResolveProject(ctx, got)
		if rerr != nil || id != "dash-id" {
			t.Errorf("%s project %q did not resolve: id=%q err=%v", ph.name, got, id, rerr)
		}
	}
}

// TestConsolidationContext pins the bound that replaced a hardcoded 3 minutes.
// The important case is zero/negative: WithTimeout(parent, 0) would cancel the
// call immediately, so "unset" must mean unbounded instead.
func TestConsolidationContext(t *testing.T) {
	if _, ok := mustCtx(t, 0).Deadline(); ok {
		t.Fatal("zero minutes must not set a deadline")
	}

	ctx5, cancel5 := consolidationContext(context.Background(), 5)
	defer cancel5()
	dl, ok := ctx5.Deadline()
	if !ok {
		t.Fatal("5 minutes must set a deadline")
	}
	if d := time.Until(dl); d < 4*time.Minute || d > 6*time.Minute {
		t.Errorf("deadline %v is not ~5 minutes away", d)
	}

	if _, ok := mustCtx(t, -1).Deadline(); ok {
		t.Error("negative minutes must not set a deadline")
	}
}

// mustCtx returns the context for the given minutes without leaking its cancel.
func mustCtx(t *testing.T, minutes int) context.Context {
	t.Helper()
	ctx, cancel := consolidationContext(context.Background(), minutes)
	t.Cleanup(cancel)
	return ctx
}

// TestClampReflectMemories pins the shared content cap on consolidation
// output: reflection proposals obey the same memory.MaxContentLen as MCP
// saves, content at the cap is untouched, and a cut is explicit — the
// stored text ends with the marker naming the limit instead of stopping
// mid-sentence with no signal.
func TestClampReflectMemories(t *testing.T) {
	const markerLiteral = " …[truncated at 8000 bytes]"

	mems := []reflection.ReflectMemory{
		{Category: "fact", Content: "short and complete"},
		{Category: "gotcha", Content: strings.Repeat("b", memory.MaxContentLen)},
		{Category: "gotcha", Content: strings.Repeat("a", memory.MaxContentLen+1)},
	}

	if cut := clampReflectMemories(mems); cut != 1 {
		t.Errorf("cut = %d, want 1 (only the over-cap content is cut)", cut)
	}
	if mems[0].Content != "short and complete" {
		t.Errorf("sub-cap content rewritten: %q", mems[0].Content)
	}
	if mems[1].Content != strings.Repeat("b", memory.MaxContentLen) {
		t.Errorf("exactly-at-cap content rewritten: len=%d, want %d", len(mems[1].Content), memory.MaxContentLen)
	}
	want := strings.Repeat("a", memory.MaxContentLen) + markerLiteral
	if mems[2].Content != want {
		t.Errorf("over-cap content = len %d ending %q, want len %d ending %q",
			len(mems[2].Content), mems[2].Content[len(mems[2].Content)-len(markerLiteral):], len(want), markerLiteral)
	}
}
