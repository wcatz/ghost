package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
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

func slogDiscard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestBuildClassifyProvider_NoKeyNoBackendErrors: with no API key and no
// resolvable claude/opencode binary, building the classifier must fail loudly.
func TestBuildClassifyProvider_NoKeyNoBackendErrors(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty dir defeats bare-name LookPath lookups
	cfg := &config.Config{}
	_, err := buildClassifyProvider(cfg, slogDiscard())
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("want backend error, got %v", err)
	}
}

// TestBuildClassifyProvider_NoKeyFallsToOpencodeStub: with no API key and no
// claude binary but an explicit opencode binary configured, the returned
// provider must classify through that opencode binary (the harness's
// zero-Anthropic-spend path).
func TestBuildClassifyProvider_NoKeyFallsToOpencodeStub(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	stub := stubBinary(t, `{"type":"text","part":{"type":"text","text":"STUB_CLASSIFY_OK"}}`)
	cfg := &config.Config{}
	cfg.CLI.OpenCodeBinary = stub
	p, err := buildClassifyProvider(cfg, slogDiscard())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	out, err := p.Classify(context.Background(), "sys prompt", "user content")
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if out.Text != "STUB_CLASSIFY_OK" || out.FromFallback {
		t.Fatalf("got %+v", out)
	}
}

// TestBuildClassifyProvider_KeySetBuildsRegardless: with an API key set the
// provider builds even when no CLI backend exists (API primary), preserving
// pre-change behavior.
func TestBuildClassifyProvider_KeySetBuildsRegardless(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cfg := &config.Config{}
	cfg.API.Key = "sk-test"
	p, err := buildClassifyProvider(cfg, slogDiscard())
	if err != nil || p == nil {
		t.Fatalf("build: p=%v err=%v", p, err)
	}
}

// TestRunAllClients verifies the `--client all` orchestration: banners in run
// order, continue past individual failures, per-failure stderr lines, and the
// failed-names return that drives the exit code.
func TestRunAllClients(t *testing.T) {
	t.Run("continues past failures and reports them", func(t *testing.T) {
		targets := []clientTarget{
			{"ok-first", func(w io.Writer, dryRun bool) error { return nil }},
			{"boom", func(w io.Writer, dryRun bool) error { return fmt.Errorf("kaput") }},
			{"ok-last", func(w io.Writer, dryRun bool) error { return nil }},
			{"boom2", func(w io.Writer, dryRun bool) error { return fmt.Errorf("kaput too") }},
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
			{"a", func(w io.Writer, dryRun bool) error { return nil }},
			{"b", func(w io.Writer, dryRun bool) error { return nil }},
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
