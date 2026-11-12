package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/reflection"
	"github.com/wcatz/ghost/internal/resolve"
	"github.com/wcatz/ghost/internal/supersede"
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

// TestParseObsidianFlagsRefusesAnEmptyInterval: --interval is the cadence half
// of the same rule as --out, and an explicit empty value is the unset-variable
// spelling rather than "no flag given". The parser returned "" for it, and
// runObsidian reads "" as "use obsidian.interval" (30s by default), so
// `ghost obsidian sync --interval "$INTERVAL"` with INTERVAL unset polled at a
// cadence nobody chose and exited 0. The default is the flag left off.
func TestParseObsidianFlagsRefusesAnEmptyInterval(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"attached empty", []string{"--interval="}},
		{"separate empty", []string{"--interval", ""}},
		{"attached empty after a valid flag", []string{"--out", "/v", "--interval="}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, interval, err := parseObsidianFlags(tc.args)
			if err == nil {
				t.Fatalf("parseObsidianFlags(%v) = %q with no error: an empty --interval must be refused, not read as the default cadence", tc.args, interval)
			}
			if err.Error() != "--interval requires a value" {
				t.Errorf("parseObsidianFlags(%v) error = %q, want %q", tc.args, err, "--interval requires a value")
			}
		})
	}

	// The duplicate is asked FIRST, as for --out: a cadence followed by an
	// empty one is "given twice" naming both, not a complaint about an empty
	// value the reader could not have meant.
	if _, _, _, err := parseObsidianFlags([]string{"--interval", "5m", "--interval="}); err == nil {
		t.Fatal("a cadence followed by an empty one must be refused")
	} else if !strings.Contains(err.Error(), "was given twice") {
		t.Errorf("error = %q, want the duplicate refusal naming both values", err)
	}

	// Positive controls: a value still parses in both spellings, and an absent
	// --interval is not an error and leaves interval empty for the default to
	// fill.
	for _, args := range [][]string{{"--interval", "30s"}, {"--interval=30s"}} {
		if _, _, interval, err := parseObsidianFlags(args); err != nil || interval != "30s" {
			t.Errorf("parseObsidianFlags(%v) = %q (err %v), want 30s", args, interval, err)
		}
	}
	if _, _, interval, err := parseObsidianFlags([]string{"--out=/v"}); err != nil || interval != "" {
		t.Errorf("absent --interval = %q (err %v), want empty and no error", interval, err)
	}
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

// TestDetectClients verifies that detectClients returns only the names of
// clients whose binaries are findable on PATH.
func TestDetectClients(t *testing.T) {
	// Create a temp dir with stub binaries for two of the four clients.
	dir := t.TempDir()
	writeStub(t, dir, "opencode")
	writeStub(t, dir, "goose")

	// Override PATH to include only our temp dir, and keep the host's home and
	// system bin dirs out of the lookup.
	isolateClientLookup(t)
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
	isolateClientLookup(t)
	t.Setenv("PATH", t.TempDir())
	got := detectClients()
	if len(got) != 0 {
		t.Fatalf("detectClients() = %v, want empty", got)
	}
}

// isolateClientLookup points HOME at a temp dir, so no client in the host's home
// bin dirs is reachable. It returns the home.
func isolateClientLookup(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
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

// TestReflectMaySkipDoesNotSkipExplicitAllowDrops: --allow-drops is the same
// class of flag as --promote-globals. The fingerprint describes the corpus, not
// what the apply was asked to do with it, so an operator who runs a
// --skip-unchanged apply with --allow-drops right after a default one would
// otherwise inherit the earlier verdict, print "unchanged — skipping" and prune
// nothing: the explicit request to delete is discarded, and since #549 widened
// the guard, that prune is now the only way to drop any category.
func TestReflectMaySkipDoesNotSkipExplicitAllowDrops(t *testing.T) {
	if reflectMaySkip(true, true, false, true, "same", "same") {
		t.Fatal("explicit --allow-drops was skipped as unchanged, so the prune silently no-ops")
	}
	if !reflectMaySkip(true, true, false, false, "same", "same") {
		t.Fatal("ordinary unchanged apply should still skip")
	}
}

// TestReportReductionWarningCountsCandidatesPromotionFailed is the case the
// pre-apply count got wrong. With --promote-globals a candidate that cannot
// become a _global row is written back into the PROJECT, so it is a row the
// project still holds — a survivor. Counting only projectMems understated
// retention by the whole candidate set, so a run where every promotion failed
// reported a far larger reduction than happened, on the one line the unattended
// lifecycle path has to report it with.
func TestReportReductionWarningCountsCandidatesPromotionFailed(t *testing.T) {
	live := make([]memory.Memory, 20)
	project := make([]reflection.ReflectMemory, 3)
	candidates := make([]reflection.ReflectMemory, 9)

	// Every promotion failed, so all nine candidates are back in the project and
	// it ends up holding twelve of twenty: above the half, so nothing is reported.
	var buf bytes.Buffer
	reportReductionWarning(&buf, live, project, candidates, true,
		promotionOutcome{applied: true, kept: len(candidates)})
	if buf.Len() != 0 {
		t.Errorf("12 of 20 kept is under two thirds and must not be reported as a >50%% reduction; got:\n%s", buf.String())
	}

	// And the pre-apply view of the same round, which is what the count looks
	// like before anyone knows how many promotions will fail. 3 of 20 is 85%, so
	// this one does report — which is exactly why the apply path must wait for
	// the real number rather than print this one.
	buf.Reset()
	reportReductionWarning(&buf, live, project, candidates, true, promotionOutcome{})
	if !strings.Contains(buf.String(), "left 3 memories in the project vs 20") {
		t.Errorf("the pre-apply count is the optimistic one and must say so; got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "would be promoted") {
		t.Errorf("before the apply, a candidate is classified global and nothing more; got:\n%s", buf.String())
	}
}

// TestReportReductionWarningNeverClaimsAnUnwrittenPromotion: the note said
// "promoted to _global" on a run where promotion had not happened yet, so it
// asserted a write that had not occurred. Each state has to be stated in its own
// words, and none of them may be the one belonging to a different moment.
//
// The third case is the one a bare count could not express, and it is the
// commonest outcome of `--apply --promote-globals`: the apply ran and EVERY
// candidate promoted, so kept is 0 — the same count as "nothing has been written
// yet". Inferring the state from the count printed "would be promoted" AFTER the
// write that had already promoted all nine, which is the same untruth pointing
// the other way.
func TestReportReductionWarningNeverClaimsAnUnwrittenPromotion(t *testing.T) {
	live := make([]memory.Memory, 20)
	project := make([]reflection.ReflectMemory, 3)

	cases := []struct {
		name       string
		candidates int
		promote    bool
		promotion  promotionOutcome
		want       string
		refuse     string
	}{
		{
			// With promotion on, the retained side is projectMems plus whatever
			// came back, so nine candidates do not make this round look retained.
			name: "before the apply", candidates: 9, promote: true, promotion: promotionOutcome{},
			want: "would be promoted to _global on apply", refuse: "promoted to _global,",
		},
		{
			name: "promotion partly failed", candidates: 9, promote: true,
			promotion: promotionOutcome{applied: true, kept: 4},
			want:      "5 promoted to _global, 4 could not be and are back in the project",
			refuse:    "would be promoted",
		},
		{
			name: "promotion applied, all candidates promoted", candidates: 9, promote: true,
			promotion: promotionOutcome{applied: true, kept: 0},
			want:      "all 9 promoted to _global",
			// The bug this case exists for: the same kept=0 as the pre-apply row
			// above, and the opposite truth.
			refuse: "would be promoted",
		},
		{
			// With promotion off every candidate is folded back in, so it takes
			// fewer of them to cross the line — hence its own count.
			name: "promotion off", candidates: 6, promote: false,
			promotion: promotionOutcome{},
			want:      "kept project-scoped", refuse: "_global",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			reportReductionWarning(&buf, live, project, make([]reflection.ReflectMemory, tc.candidates), tc.promote, tc.promotion)
			got := buf.String()
			if !strings.Contains(got, tc.want) {
				t.Errorf("note missing %q; got:\n%s", tc.want, got)
			}
			if tc.refuse != "" && strings.Contains(got, tc.refuse) {
				t.Errorf("note claims %q, which is not true at this point; got:\n%s", tc.refuse, got)
			}
		})
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
		{"--signals", "s"}, // the stop hook's audit sidecar is gone; the flag is unknown
		{"-dashy"},         // bare dash-prefixed positional: indistinguishable from a flag; use --project -dashy
	}
	for _, args := range bad {
		if _, _, err := parseLifecycleArgs(args); err == nil {
			t.Errorf("parseLifecycleArgs(%v) = nil error, want an error", args)
		}
	}
}

// TestParseReflectArgs pins `ghost reflect` argv parsing: the single-project
// forms (with any accepted flag combination) parse unchanged, and --project
// takes the NEXT argument verbatim — including dash-leading names such as -x or
// --odd — so they read as project names, not flags. --project=VALUE matches the
// parser's other equals-form flags, a valueless --project is a clear error, and
// unknown flags stay silently ignored (reflect's historical behavior;
// resolve/supersede error on them). The empty-value and repeat rows for every
// parser that takes a project are in
// TestScopeFlagParsersRefuseEmptyAndRepeated, one table for all of them.
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
// --project and unknown flags stay clear errors; --reassess selects the
// re-evaluation pass over already-resolved memories; and --only/--only-file
// narrow that pass to named memories (#698).
func TestParseResolveArgs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		project  string
		source   string
		apply    bool
		reassess bool
		only     []string
		onlyFile string
		mark     []string
		markFile string
	}{
		{"positional", []string{"myproj"}, "myproj", "", false, false, nil, "", nil, ""},
		{"positional with apply", []string{"myproj", "--apply"}, "myproj", "", true, false, nil, "", nil, ""},
		{"reassess", []string{"myproj", "--reassess"}, "myproj", "", false, true, nil, "", nil, ""},
		{"reassess with apply", []string{"myproj", "--reassess", "--apply"}, "myproj", "", true, true, nil, "", nil, ""},
		{"reassess before project", []string{"--reassess", "myproj"}, "myproj", "", false, true, nil, "", nil, ""},
		{"source separate value", []string{"myproj", "--source", "opencode"}, "myproj", "opencode", false, false, nil, "", nil, ""},
		{"source equals value", []string{"myproj", "--source=codex"}, "myproj", "codex", false, false, nil, "", nil, ""},
		{"project flag dash value", []string{"--project", "-x", "--apply"}, "-x", "", true, false, nil, "", nil, ""},
		{"project flag double-dash value", []string{"--project", "--odd"}, "--odd", "", false, false, nil, "", nil, ""},
		{"project flag lifecycle shape", []string{"--project", "-myproj", "--apply", "--source", "claude"}, "-myproj", "claude", true, false, nil, "", nil, ""},
		{"project equals form", []string{"--project=-eq"}, "-eq", "", false, false, nil, "", nil, ""},
		// One project named ONCE, in either spelling, with the rest of the
		// command around it. The parser counts occurrences rather than testing
		// the value for emptiness, so this is the case that must not move: a
		// counter that refused a second SIGHT rather than a second VALUE would
		// break every ordinary invocation to fix an empty one.
		{"one project flag with the rest of the command", []string{"--project", "myproj", "--reassess", "--apply", "--source", "claude"}, "myproj", "claude", true, true, nil, "", nil, ""},
		{"one project flag in the equals form", []string{"--project=myproj", "--apply"}, "myproj", "", true, false, nil, "", nil, ""},
		// The repair scope. Comma-separated on one flag, repeated across flags,
		// and either spelling of the value form all reach Scope as one list.
		{"only separate value", []string{"p", "--reassess", "--only", "abcdef01"}, "p", "", false, true, []string{"abcdef01"}, "", nil, ""},
		{"only comma list", []string{"p", "--reassess", "--only", "abcdef01,12345678"}, "p", "", false, true, []string{"abcdef01", "12345678"}, "", nil, ""},
		{"only equals form", []string{"p", "--reassess", "--only=abcdef01"}, "p", "", false, true, []string{"abcdef01"}, "", nil, ""},
		{"only repeated", []string{"p", "--reassess", "--only", "abcdef01", "--only", "12345678"}, "p", "", false, true, []string{"abcdef01", "12345678"}, "", nil, ""},
		{"only with spaces and empties", []string{"p", "--reassess", "--only", " abcdef01 , , 12345678 "}, "p", "", false, true, []string{"abcdef01", "12345678"}, "", nil, ""},
		{"only file", []string{"p", "--reassess", "--only-file", "/tmp/ids.txt"}, "p", "", false, true, nil, "/tmp/ids.txt", nil, ""},
		{"only file equals form", []string{"p", "--reassess", "--only-file=/tmp/ids.txt"}, "p", "", false, true, nil, "/tmp/ids.txt", nil, ""},
		// Both at once is the union, flag selectors first: the supersede repair
		// prints both forms for one set of ids.
		{"only and only file together", []string{"p", "--reassess", "--only", "abcdef01", "--only-file", "/tmp/ids.txt"}, "p", "", false, true, []string{"abcdef01"}, "/tmp/ids.txt", nil, ""},
		{"only with apply", []string{"p", "--reassess", "--only", "abcdef01", "--apply"}, "p", "", true, true, []string{"abcdef01"}, "", nil, ""},
		// The targeted mark (#714). It takes the same two forms as the repair
		// scope — a comma-separated list and a one-per-line file — because it is
		// the same list of memories named for the other direction of the same
		// stamp, and an operator who learned one form should not have to learn
		// another.
		{"mark separate value", []string{"p", "--mark", "abcdef01"}, "p", "", false, false, nil, "", []string{"abcdef01"}, ""},
		{"mark comma list", []string{"p", "--mark", "abcdef01,12345678"}, "p", "", false, false, nil, "", []string{"abcdef01", "12345678"}, ""},
		{"mark equals form", []string{"p", "--mark=abcdef01"}, "p", "", false, false, nil, "", []string{"abcdef01"}, ""},
		{"mark repeated", []string{"p", "--mark", "abcdef01", "--mark", "12345678"}, "p", "", false, false, nil, "", []string{"abcdef01", "12345678"}, ""},
		{"mark with spaces and empties", []string{"p", "--mark", " abcdef01 , , 12345678 "}, "p", "", false, false, nil, "", []string{"abcdef01", "12345678"}, ""},
		{"mark file", []string{"p", "--mark-file", "/tmp/ids.txt"}, "p", "", false, false, nil, "", nil, "/tmp/ids.txt"},
		{"mark file equals form", []string{"p", "--mark-file=/tmp/ids.txt"}, "p", "", false, false, nil, "", nil, "/tmp/ids.txt"},
		{"mark and mark file together", []string{"p", "--mark", "abcdef01", "--mark-file", "/tmp/ids.txt"}, "p", "", false, false, nil, "", []string{"abcdef01"}, "/tmp/ids.txt"},
		{"mark with apply", []string{"p", "--mark", "abcdef01", "--apply"}, "p", "", true, false, nil, "", []string{"abcdef01"}, ""},
		// A --source on a --mark run parses, because refusing it would be a flag
		// the parser rejects on some lines and ignores on others. It is simply
		// not used, which the usage text says — the same treatment --withdraw
		// gives it on supersede.
		{"mark with source is accepted and unused", []string{"p", "--mark", "abcdef01", "--source", "opencode"}, "p", "opencode", false, false, nil, "", []string{"abcdef01"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := parseResolveArgs(tc.args)
			if err != nil {
				t.Fatalf("parseResolveArgs(%v): %v", tc.args, err)
			}
			if parsed.project != tc.project || parsed.source != tc.source || parsed.apply != tc.apply ||
				parsed.reassess != tc.reassess || parsed.onlyFile != tc.onlyFile || parsed.markFile != tc.markFile {
				t.Errorf("parseResolveArgs(%v) = %+v, want project=%q source=%q apply=%v reassess=%v onlyFile=%q markFile=%q",
					tc.args, parsed, tc.project, tc.source, tc.apply, tc.reassess, tc.onlyFile, tc.markFile)
			}
			for _, list := range []struct {
				name      string
				got, want []string
			}{
				{name: "only", got: parsed.only, want: tc.only},
				{name: "mark", got: parsed.mark, want: tc.mark},
			} {
				if len(list.got) != len(list.want) {
					t.Fatalf("%s = %v, want %v", list.name, list.got, list.want)
				}
				for i := range list.want {
					if list.got[i] != list.want[i] {
						t.Errorf("%s = %v, want %v", list.name, list.got, list.want)
					}
				}
			}
			// hasMark is what the dispatch branches on, so it has to agree with
			// the two fields the parser filled in — a run that asked to mark
			// nothing would otherwise fall through to the ordinary pass, which
			// judges the whole project and asks a harness about it.
			if want := len(tc.mark) > 0 || tc.markFile != ""; parsed.hasMark() != want {
				t.Errorf("hasMark() = %v, want %v for %v", parsed.hasMark(), want, tc.args)
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
		// The separate-value spelling reaches runResolve as an empty project,
		// where it prints the usage block rather than doing the work — so it is
		// not a silent whole-store pass, but it is the same unset variable in a
		// different spelling, and one flag must be refused for one reason.
		{"project empty separate value", []string{"--project", ""}, "--project requires a value"},
		// A repeat is COUNTED, not inferred from the value. An EMPTY first value
		// used to read as "no project given", so the second one silently became
		// the scope of a run whose command line named the scope twice. The
		// duplicate is asked first, so a NAMED value followed by any second value
		// is named as the duplicate it is — and two empty values stop at the
		// FIRST one, because with no first value there is no scope in the command
		// line to be a duplicate of. These three used to parse at all.
		{"empty separate project then project flag", []string{"--project", "", "--project", "b"}, "--project requires a value"},
		{"project flag then empty project", []string{"--project", "a", "--project="}, "expected exactly one project"},
		{"project equals then empty project", []string{"--project=a", "--project="}, "expected exactly one project"},
		{"project equals then project flag", []string{"--project=a", "--project", "b"}, "expected exactly one project"},
		{"project flag then project equals", []string{"--project", "a", "--project=b"}, "expected exactly one project"},
		// Already refused before the count, and still refused for the same reason
		// rather than for the duplicate further along the line: the empty first
		// value stops the parse where it is.
		{"empty project then project flag", []string{"--project=", "--project", "b"}, "--project requires a value"},
		{"empty project twice", []string{"--project=", "--project="}, "--project requires a value"},
		// An empty POSITIONAL is the same missing operand in a third spelling,
		// and the one that decides nothing: runResolve prints the usage block
		// for an empty project. It is counted so that `ghost resolve "" b` is
		// refused as a repeat rather than running b.
		{"empty positional then project flag", []string{"", "--project", "b"}, "expected exactly one project"},
		{"unknown flag", []string{"myproj", "--bogus"}, `unknown flag "--bogus"`},
		{"source missing value", []string{"myproj", "--source"}, `unknown flag "--source"`},
		// A repair scope without --reassess has nothing to scope: the ordinary
		// pass judges unresolved rows, so accepting the flag would be a flag
		// that parses, changes no decision and says nothing in the report.
		{"only without reassess", []string{"p", "--only", "abcdef01"}, "use them with --reassess"},
		{"only file without reassess", []string{"p", "--only-file", "/tmp/ids.txt"}, "use them with --reassess"},
		{"only missing value", []string{"p", "--reassess", "--only"}, "--only requires at least one memory id or prefix"},
		{"only empty equals value", []string{"p", "--reassess", "--only="}, "--only requires at least one memory id or prefix"},
		// An empty or separator-only value is the routine shell mistake
		// (`--only "$IDS"` with IDS unset), and it used to parse into no
		// selectors at all — which is an UNSCOPED project-wide repair, the one
		// outcome that must never happen by accident, reported as an unscoped
		// run because Pool == Loaded.
		{"only empty separate value", []string{"p", "--reassess", "--only", ""}, "--only requires at least one memory id or prefix"},
		{"only whitespace separate value", []string{"p", "--reassess", "--only", "   "}, "--only requires at least one memory id or prefix"},
		{"only separators only", []string{"p", "--reassess", "--only", ",,"}, "--only requires at least one memory id or prefix"},
		{"only file empty separate value", []string{"p", "--reassess", "--only-file", ""}, "--only-file requires a path"},
		{"only file whitespace separate value", []string{"p", "--reassess", "--only-file", "  "}, "--only-file requires a path"},
		{"only file missing value", []string{"p", "--reassess", "--only-file"}, "--only-file requires a path"},
		{"only file empty equals value", []string{"p", "--reassess", "--only-file="}, "--only-file requires a path"},
		// --mark is the two directions of one stamp, so asking for both would be
		// a command with two dry-run answers. The reader is told which of the two
		// repairs they asked for twice rather than being left to work it out.
		{"mark with reassess", []string{"p", "--mark", "abcdef01", "--reassess"}, "run them as two commands"},
		{"reassess with mark", []string{"p", "--reassess", "--mark", "abcdef01"}, "run them as two commands"},
		{"mark file with reassess", []string{"p", "--mark-file", "/tmp/ids.txt", "--reassess"}, "run them as two commands"},
		{"mark with only", []string{"p", "--mark", "abcdef01", "--reassess", "--only", "12345678"}, "run them as two commands"},
		// An empty --mark is the same unset-variable mistake as an empty --only,
		// and it must fail rather than reach the ordinary pass: a run that
		// classified the whole project and billed a harness call would be the
		// operator's request to mark two memories.
		{"mark missing value", []string{"p", "--mark"}, "--mark requires at least one memory id or prefix"},
		{"mark empty equals value", []string{"p", "--mark="}, "--mark requires at least one memory id or prefix"},
		{"mark empty separate value", []string{"p", "--mark", ""}, "--mark requires at least one memory id or prefix"},
		{"mark whitespace separate value", []string{"p", "--mark", "   "}, "--mark requires at least one memory id or prefix"},
		{"mark separators only", []string{"p", "--mark", ",,"}, "--mark requires at least one memory id or prefix"},
		{"mark file empty separate value", []string{"p", "--mark-file", ""}, "--mark-file requires a path"},
		{"mark file whitespace separate value", []string{"p", "--mark-file", "  "}, "--mark-file requires a path"},
		{"mark file missing value", []string{"p", "--mark-file"}, "--mark-file requires a path"},
		{"mark file empty equals value", []string{"p", "--mark-file="}, "--mark-file requires a path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseResolveArgs(tc.args); err == nil {
				t.Fatalf("parseResolveArgs(%v) must fail", tc.args)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q must contain %q", err, tc.want)
			}
		})
	}
}

// The one shape the whitespace rule cannot have both ways: an id containing " #".
// The line reads as an id plus a comment, so the id is truncated at the '#'. That
// is a documented cost rather than an oversight — the alternative (a '#' anywhere
// starts a comment) truncates every id holding a '#', including a word-internal
// one, and this file is the only surface that can carry those.
func TestReadOnlySelectorsTruncatesAHashThatFollowsWhitespace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ids.txt")
	if err := os.WriteFile(path, []byte("imported note # not part of the id\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readRefSelectors("--only-file", path)
	if err != nil {
		t.Fatalf("readRefSelectors: %v", err)
	}
	if len(got) != 1 || got[0] != "imported note" {
		t.Errorf("readRefSelectors = %q, want the id up to the comment", got)
	}
}

// TestReadOnlySelectorsRefusesAnEmptyList: a file the operator pointed a repair
// at and that names nothing must fail, never fall through to an unscoped pass
// that judges every resolved memory in the project.
func TestReadRefSelectorsRefusesAnEmptyList(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"empty":            "",
		"comments only":    "# nothing here\n#  or here\n",
		"blank lines only": "\n\n   \n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, "ids.txt")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			if _, err := readRefSelectors("--only-file", path); err == nil {
				t.Fatal("a --only-file that names no ids must be an error, not an unscoped run")
			} else if !strings.Contains(err.Error(), "names no memory ids") {
				t.Errorf("error %q must say the file named nothing", err)
			}
		})
	}
}

// TestReadOnlySelectorsNamesAMissingFile: the path is in the error, because the
// operator has to know which of two files in a report they mistyped.
func TestReadRefSelectorsNamesAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-file.txt")
	_, err := readRefSelectors("--only-file", path)
	if err == nil {
		t.Fatal("a missing --only-file must be an error")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q must name the path %q", err, path)
	}
}

// TestResolveSummaryLineReportsStale: a confirmed note a concurrent writer
// rewrote while it was classified is not stamped, and the count is named so
// "resolved 0" is not read as a pass that found nothing (#1052). The suffix is
// absent when the count is zero, which is what keeps every other report line
// here stable.
func TestResolveSummaryLineReportsStale(t *testing.T) {
	got := resolveSummaryLine("proj", resolve.Result{Loaded: 1, Candidates: 1, Confirmed: 1, Stale: 1}, true, 1, 1)
	want := "proj: 1 loaded, 1 after prefilter, 1 confirmed evidence, 0 KEEP vetoed, 0 KEEP cached, 0 UNKNOWN, resolved 0 (1 classify call(s)), 1 skipped as stale\n"
	if got != want {
		t.Errorf("resolveSummaryLine() = %q, want %q", got, want)
	}
}

// TestReassessSummaryLine: the reassess pass reports what it found per outcome
// and what --apply would clear, in both the dry-run and apply verb forms.
func TestReassessSummaryLine(t *testing.T) {
	res := resolve.ReassessResult{Loaded: 40, Vetoed: 6, Cached: 1, ReKept: 9, StillResolved: 22, Demoted: 2, Unknown: 0, Cleared: 9}
	wantDry := "proj: 40 already resolved, 6 KEEP vetoed, 1 KEEP cached, 22 still RESOLVED, 2 still asserted by a link or correction, 0 UNKNOWN, would clear resolved_at for 9 (2 classify call(s))\n"
	if got := reassessSummaryLine("proj", res, false, 9, 2); got != wantDry {
		t.Errorf("reassessSummaryLine() dry = %q, want %q", got, wantDry)
	}
	res.Cleared = 8 // a concurrent clear can stamp fewer rows than were judged
	wantApply := "proj: 40 already resolved, 6 KEEP vetoed, 1 KEEP cached, 22 still RESOLVED, 2 still asserted by a link or correction, 0 UNKNOWN, cleared resolved_at for 8 (2 classify call(s))\n"
	if got := reassessSummaryLine("proj", res, true, 9, 2); got != wantApply {
		t.Errorf("reassessSummaryLine() apply = %q, want %q", got, wantApply)
	}
}

// TestReassessSummaryLineScopedSaysHowMuchItJudged: a scoped run that reported
// "40 already resolved" would be describing a pool it never looked at, and the
// operator's whole reason for scoping is to know the other rows were left alone
// (#698).
func TestReassessSummaryLineScopedSaysHowMuchItJudged(t *testing.T) {
	res := resolve.ReassessResult{Loaded: 3, Pool: 143, Vetoed: 1, Cached: 0, ReKept: 1, StillResolved: 1, Demoted: 0, Unknown: 0, Cleared: 1}
	want := "proj: 3 of 143 already resolved judged, 1 KEEP vetoed, 0 KEEP cached, 1 still RESOLVED, 0 still asserted by a link or correction, 0 UNKNOWN, cleared resolved_at for 1 (1 classify call(s))\n"
	if got := reassessSummaryLine("proj", res, true, 1, 1); got != want {
		t.Errorf("reassessSummaryLine() scoped = %q, want %q", got, want)
	}
}

// TestReassessMissLines: a selector that named nothing is reported on its own
// line, verbatim, because the alternative is a resolution the operator believed
// was repaired and was not.
func TestReassessMissLines(t *testing.T) {
	if got := reassessMissLines(resolve.ReassessResult{Loaded: 2, Pool: 2}); got != "" {
		t.Errorf("an unscoped run must print no miss lines, got %q", got)
	}
	res := resolve.ReassessResult{
		Loaded: 1, Pool: 2,
		Misses: []resolve.ScopeMiss{
			{Spec: "ffffffff", Reason: "no already-resolved memory in this project has that id or prefix"},
		},
	}
	want := "  ffffffff  not judged: no already-resolved memory in this project has that id or prefix\n"
	if got := reassessMissLines(res); got != want {
		t.Errorf("reassessMissLines() = %q, want %q", got, want)
	}
}

// TestSupersedeReport: the pass's report carries the deterministic veto's
// count, and only when the veto settled something. A pass that declined work it
// did not do and printed the same totals as a pass that found nothing to do
// reads as "nothing was skipped" (#686).
func TestSupersedeReport(t *testing.T) {
	dry := "proj: 4 candidate pairs in 1 classify call(s), 2 cached, 1 supersedes, 0 causes, 0 reclassified, would link\n"
	if got := supersedeReport("proj", supersede.Result{Candidates: 4, Skipped: 2, Confirmed: 1}, "would link", false, 1, 0); got != dry {
		t.Errorf("supersedeReport() with nothing vetoed = %q, want %q", got, dry)
	}
	got := supersedeReport("proj", supersede.Result{Candidates: 4, Skipped: 2, Confirmed: 1, Vetoed: 3}, "would link", false, 1, 0)
	if !strings.HasPrefix(got, dry) {
		t.Errorf("supersedeReport() = %q, want the summary line first, unchanged", got)
	}
	if !strings.Contains(got, "  3 pair(s) vetoed:") {
		t.Errorf("supersedeReport() = %q, want it to report 3 vetoed pairs", got)
	}
	if !strings.Contains(got, "no classify call") || !strings.Contains(got, "not cached") {
		t.Errorf("supersedeReport() = %q, want it to say what the veto costs and does not cost", got)
	}
	supersedeReportCountsEdges(t)
	// The two STALE populations, and they are two lines because one counter for
	// both would need a wording true of each, which does not exist. A pre-classify
	// drop spent NO classify call; a pre-write drop spent one and wrote nothing.
	// Both leave the pair in no count on the page, so both get a line in BOTH modes.
	//
	// The pre-write one exists for #834's reason: it is counted as a verdict, so
	// under --apply the summary (which counts writes) does not carry it, and
	// trading a false count for no count still needs the difference to be on the
	// page somewhere.
	preWrite := supersedeReport("proj", supersede.Result{Candidates: 1, Confirmed: 1, Created: 0, StaleAtWrite: 1}, "linked", true, 1, 0)
	if !strings.Contains(preWrite, "  1 pair(s) not written: an endpoint was replaced by a concurrent pass between the classify and the write") {
		t.Errorf("supersedeReport() apply = %q, want the pre-write stale line: a judged pair that wrote no edge is in no count", preWrite)
	}
	if strings.Contains(preWrite, "no call was spent") {
		t.Errorf("supersedeReport() = %q claims no classify call was spent for the PRE-WRITE drop; that pair was classified", preWrite)
	}
	// The pre-classify one must NOT claim a call was spent, and must print in a dry
	// run too — it fires there, and `Result.Candidates` is recomputed from the
	// surviving set, so the pair is in no count on the page in EITHER mode.
	for _, apply := range []bool{true, false} {
		verb := "would link"
		if apply {
			verb = "linked"
		}
		pre := supersedeReport("proj", supersede.Result{Candidates: 1, StaleSkipped: 1}, verb, apply, 0, 0)
		if !strings.Contains(pre, "  1 pair(s) not proposed: an endpoint was replaced by a concurrent pass before the classify") {
			t.Errorf("supersedeReport() apply=%v = %q, want the pre-classify stale line in BOTH modes: the pair is in no count on this page", apply, pre)
		}
		if strings.Contains(pre, "no edge was written") {
			t.Errorf("supersedeReport() apply=%v = %q describes the pre-classify drop as a failed WRITE; no call was spent and there was nothing to write", apply, pre)
		}
	}
	// And the two never share a line: a reader must be able to tell which of the
	// two populations a run reported without reading the code.
	if both := supersedeReport("proj", supersede.Result{Candidates: 2, StaleSkipped: 1, StaleAtWrite: 1}, "linked", true, 1, 0); strings.Count(both, "pair(s) ") != 2 {
		t.Errorf("supersedeReport() = %q, want one line per stale population, not a single merged count", both)
	}
	// The report is mode-agnostic about the veto: a vetoed pair is never linked,
	// so there is nothing for --apply to write either. The apply verb is the
	// only thing that changes.
	apply := supersedeReport("proj", supersede.Result{Candidates: 1, Vetoed: 1}, "linked", true, 0, 0)
	if !strings.HasPrefix(apply, "proj: 1 candidate pairs in 0 classify call(s), 0 cached, 0 supersedes, 0 causes, 0 reclassified, linked\n") {
		t.Errorf("supersedeReport() apply = %q, want the apply verb and the veto count", apply)
	}
	// A pass that had to re-ask a failed call says so (#699). The count is
	// inside the call tally rather than a second sentence, because the calls and
	// the retries are one fact: the retry IS a call, and the line that reports
	// one without the other describes a pass nobody ran.
	//
	// The three orientation refusals are on the report for the same reason the
	// veto is, and each one says what the pass did INSTEAD, because a bare count
	// cannot distinguish a pair that was judged and dropped from a pair that was
	// never seen — and the two call for opposite follow-ups from the operator.
	orientation := supersedeReport("proj", supersede.Result{
		Candidates: 3, Unoriented: 4, OppositeLive: 2, Bidirectional: 1,
	}, "would link", false, 1, 0)
	for _, want := range []string{
		"  4 pair(s) not proposed:",
		"  2 pair(s) proposed the reverse of a live supersedes link:",
		"  1 pair(s) refused:",
		// The REAL project name, not a `<project>` placeholder: the line is a
		// command, and a command with a placeholder in it is one the operator
		// has to edit before it runs — which is where a repair ends up against
		// the wrong project.
		"ghost supersede proj --reassess --consensus 3 --apply",
	} {
		if !strings.Contains(orientation, want) {
			t.Errorf("supersedeReport() = %q, want it to contain %q", orientation, want)
		}
	}
	if strings.Contains(orientation, "<project>") {
		t.Errorf("supersedeReport() = %q, want no <project> placeholder in a command the operator is meant to paste", orientation)
	}
	// A project name that is not one bare shell word is rendered through
	// --project in single quotes, which is the quoting rule the followup package
	// owns and the one that decides whether the pasted command runs at all.
	awkward := supersedeReport("my proj", supersede.Result{Bidirectional: 1}, "would link", false, 0, 0)
	if !strings.Contains(awkward, "ghost supersede --project 'my proj' --reassess --consensus 3 --apply") {
		t.Errorf("supersedeReport() for a name holding a space = %q, want the --project form a shell reads as one argument", awkward)
	}
	// The write-time refusal (#806) is the only line on this report about a pair
	// that was JUDGED and then not written, and it has to say so in those words:
	// a pass that lost that race wrote nothing for the pair, and a report that
	// reads as a link it did not write is the one false claim this report may
	// not make. Its repair is the next ordinary pass plus the settled form of
	// --reassess, and both are named.
	raced := supersedeReport("proj", supersede.Result{Candidates: 2, Confirmed: 1, ReverseLive: 1}, "linked", true, 1, 0)
	for _, want := range []string{
		"  1 pair(s) not written:",
		"a concurrent pass got there first",
		"this run wrote no edge for them",
		"the pair keeps the edge that is there",
		"ghost supersede proj --reassess --consensus 3 --apply",
	} {
		if !strings.Contains(raced, want) {
			t.Errorf("supersedeReport() = %q, want it to contain %q", raced, want)
		}
	}
	// followup.ReassessCommand already renders the applied form, so a line
	// that appended --apply to it would print a command no other report in the
	// tree emits. The parser tolerates the repeat (it sets apply = true per
	// occurrence), which is exactly why only the rendered text catches it — and
	// why this counts --apply over the whole repair rather than looking for the
	// literal `--apply --apply`: #862's first version appended ` --consensus 3
	// --apply` to the ungated renderer and printed `--reassess --apply
	// --consensus 3 --apply`, which this exact substring guard did not see.
	for _, line := range strings.Split(raced, "\n") {
		if n := strings.Count(line, "--apply"); n > 1 {
			t.Errorf("supersedeReport() = %q, want --apply quoted once per command: the renderer already carries it", line)
		}
	}
	// Each reason states the DECISION, not a judgment the pass may never have
	// made. All three counts are taken before the filters that spend a call, so
	// a line claiming a pair "was judged" describes a pass that did not run —
	// the review-gate finding on the OppositeLive line, which the wording here
	// has to keep fixed: the scan's ORIENTATION was refused, and the pair is
	// re-judged only under skip-if-unchanged.
	for _, want := range []string{
		"no classify call, no link",
		"the reverse orientation was refused and the pair keeps the link's direction",
		"only if an endpoint changed since the link was written",
		"not judged, not written",
	} {
		if !strings.Contains(orientation, want) {
			t.Errorf("supersedeReport() = %q, want it to say %q so the count is not read as a pair the classifier saw", orientation, want)
		}
	}
	// The named repair is the APPLIED form: `Reassess` withdraws only under
	// --apply, so the flagless command an operator copies would predict the
	// withdrawal and leave the cycle demoting both endpoints.
	if strings.Contains(orientation, "--reassess`") || strings.Contains(orientation, "--reassess\n") {
		t.Errorf("supersedeReport() = %q, want the repair quoted with --apply, not as a dry run", orientation)
	}
	// A pass with no refusals prints the summary alone, and the reasons do not
	// accumulate across calls: they are per-pass facts, each conditional on its
	// own count.
	if plain := supersedeReport("proj", supersede.Result{Candidates: 2}, "would link", false, 1, 0); strings.Count(plain, "\n") != 1 {
		t.Errorf("supersedeReport() = %q, want the summary line and no refusal lines when nothing was refused", plain)
	}
	retried := supersedeReport("proj", supersede.Result{Candidates: 4, Confirmed: 1}, "would link", false, 3, 1)
	if !strings.Contains(retried, "in 3 classify call(s), 1 retried after a failed call, 0 cached") {
		t.Errorf("supersedeReport() = %q, want the retried call counted next to the calls it made", retried)
	}
}

// supersedeReportCountsEdges is #834 on the report: the summary's two edge
// counts are WRITES under --apply and WOULD-BE WRITES in a dry run, and the
// refused write is named on its own line rather than counted as an edge.
//
// The defect was an asymmetry between the two relations that only became
// reachable when #823 made the 'causes' write guarded: Result.Created counted
// writes while Result.CausesCreated counted verdicts, and the summary printed
// both. So a pass that reached one CAUSES verdict and had the write refused
// reported "1 causes" — a claim about the graph that the refusal line two lines
// below it takes back. Before that change this test's rows below are what the
// report printed under --apply; they are the report lines the fix moves, and
// they are listed as such in the PR.
//
// Why a synthetic Result and not the command: one process cannot reach this
// state. The apply block sweeps the reverse 'causes' edge whenever the pass's
// own read saw it, so the guarded writer is only refused when another process
// committed that edge between the read and the write — a cross-process race,
// which internal/supersede's two-process applyrace harness stages for real and
// where this same split is asserted on the live counters. What is left for the
// report is the pairing of the two numbers, and that is a formatter's job.
func supersedeReportCountsEdges(t *testing.T) {
	t.Helper()
	for _, tc := range []struct {
		name  string
		res   supersede.Result
		verb  string
		apply bool
		// want is the counts clause of the summary line, which is the whole of
		// what this test is about: the two numbers and the mode they came from.
		want string
		// refuse is the line naming the write-time refusals, and it has to be
		// there for the applied rows: a count of 0 with nothing else on the page
		// is a pass that reported nothing to do.
		refuse bool
	}{
		{
			// The #834 row: one verdict, no write. "1 causes" here is the false
			// claim the issue is filed against.
			name: "a refused causes write is not a created edge",
			res:  supersede.Result{Candidates: 2, CausesCreated: 1, ReverseLive: 1},
			verb: "linked", apply: true,
			want: "0 supersedes, 0 causes", refuse: true,
		},
		{
			// The same refusal on the 'supersedes' side, which Result.Created has
			// always counted as a write but the summary used to print
			// Result.Confirmed over. Same one-pair-lost-the-race run, same move.
			name: "a refused supersedes write is not a created edge",
			res:  supersede.Result{Candidates: 2, Confirmed: 1, ReverseLive: 1},
			verb: "linked", apply: true,
			want: "0 supersedes, 0 causes", refuse: true,
		},
		{
			name: "writes that landed are counted",
			res:  supersede.Result{Candidates: 3, Confirmed: 2, Created: 2, CausesCreated: 1, CausesWritten: 1},
			verb: "linked", apply: true,
			want: "2 supersedes, 1 causes", refuse: false,
		},
		{
			// The dry run's numbers are the verdicts, because a dry run attempts
			// no write and has nothing else to count. This is the promise the
			// "Re-run with --apply to write these links." hint below the report
			// makes, and it is keyed off the same two fields (Result.WouldWriteLinks).
			name: "a dry run counts what it would write",
			res:  supersede.Result{Candidates: 3, Confirmed: 2, CausesCreated: 1},
			verb: "would link", apply: false,
			want: "2 supersedes, 1 causes", refuse: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := supersedeReport("proj", tc.res, tc.verb, tc.apply, 1, 0)
			if !strings.Contains(out, tc.want) {
				t.Errorf("the summary does not count %q:\n%s", tc.want, out)
			}
			// The other count must not appear at all: "0 causes" alongside a
			// stray "1 causes" somewhere else on the page would be the same
			// false claim in a second place, and this report is read by people
			// scanning for the number they expect.
			for _, other := range []string{"1 supersedes", "2 causes"} {
				if !strings.Contains(tc.want, other) && strings.Contains(out, other) {
					t.Errorf("the summary counts %q as well:\n%s", other, out)
				}
			}
			if got := strings.Contains(out, "  1 pair(s) not written:"); got != tc.refuse {
				t.Errorf("the refusal line is present=%v, want %v:\n%s", got, tc.refuse, out)
			}
			// The refusal names what this run did not do, in the words the issue
			// asks for: no edge written, and the pair keeps the edge that is
			// there. A count of 0 alone would read as a pass that found nothing.
			if tc.refuse && !strings.Contains(out, "this run wrote no edge for them") {
				t.Errorf("the refusal does not say this run wrote nothing:\n%s", out)
			}
		})
	}
}

// TestSupersedeReassessReport: the repair pass's report is per outcome, and its
// list is the point of the pass — so each line has to name the edge, the rule
// that withdrew it and WHICH decided it, and the headline count has to be the
// one the operator is about to act on. A dry run reports the list it is about to
// print; --apply reports the rows that actually moved, which is fewer whenever a
// concurrent pass got there first.
func TestSupersedeReassessReport(t *testing.T) {
	edges := []supersede.WithdrawnEdge{
		{NewerID: "abcdef0123456789", OlderID: "9876543210fedcba", Reason: "vetoed: older note states a rule (never) the newer note does not retire", Vetoed: true, CausesSwept: 1},
		{NewerID: "1122334455667788", OlderID: "8877665544332211", Reason: "neither: both notes are still true"},
	}
	dry := supersedeReassessReport("proj", supersede.ReassessResult{
		Loaded: 5, Skipped: 1, Vetoed: 1, Confirmed: 1, Neither: 1, Unclassified: 1,
		Withdrawn: 0, CausesWithdrawn: 1, // what the pass predicts for a dry run
	}, false, edges, 2, 0)
	for _, want := range []string{
		// The per-outcome numbers add up to Loaded, the withdrawal count is the
		// two edges below it rather than the (dry-run-zero) Withdrawn field, and
		// the sweep is PREDICTED here — it is a second graph row the operator is
		// about to delete, so a dry run that hid it would be deciding for them.
		"proj: 5 live supersedes edge(s), 1 not judged, 1 vetoed, 1 still supersedes, 1 neither, 0 causes, 0 reversed, 1 UNKNOWN, would withdraw 2, would sweep 1 causes edge(s) (2 classify call(s))",
		"  would withdraw  abcdef01 -> 98765432  [veto, no harness call]  [+1 causes edge]  vetoed:",
		"  would withdraw  11223344 -> 88776655  [classifier]  neither: both notes are still true",
		"Re-run with --apply to withdraw these edges.",
	} {
		if !strings.Contains(dry, want) {
			t.Errorf("dry report missing %q:\n%s", want, dry)
		}
	}
	if strings.Contains(dry, "  withdrew ") {
		t.Errorf("a dry run reported a withdrawal as done:\n%s", dry)
	}

	// A CYCLE — a pair live in BOTH directions — is the one finding whose
	// absence from this report is indistinguishable from a pass that had nothing
	// to do: the headline count is a count of edges, and both edges of an
	// undecided cycle are still live while every number above reads zero. So the
	// block names both edges, says which one stands, and — when the pass could not
	// decide — hands over the two `ghost supersede --withdraw` commands, because a
	// report that describes a demotion with no way out of it is the failure this
	// block exists to prevent.
	//
	// Every verb in the block is the ROW verb, taken from `apply` and from what
	// the writes actually did. A block that printed its own prose said "was
	// withdrawn" for an edge a dry run never touched, directly under a list of
	// "would withdraw" rows, and "withdrew" for one a concurrent pass took first.
	const cFirst, cSecond = "abcdef0123456789", "9876543210fedcba"
	cycle := func(outcome supersede.CycleOutcome, apply bool, rows []supersede.WithdrawnEdge) string {
		return supersedeReassessReport("proj", supersede.ReassessResult{
			Loaded: 2, Unclassified: 1, Cyclic: []supersede.CyclicPair{{
				First:   supersede.CyclicEdge{SourceID: cFirst, TargetID: cSecond},
				Second:  supersede.CyclicEdge{SourceID: cSecond, TargetID: cFirst},
				Outcome: outcome,
			}},
		}, apply, rows, 1, 0)
	}
	// An UNORIENTED cycle — both notes stamped alike, so the pass had no
	// direction to ask about and did not ask. Both edges stand, and the operator
	// decides, because a re-run cannot change the timestamps.
	unoriented := cycle(supersede.CycleUnoriented, true, nil)
	for _, want := range []string{
		"live in BOTH directions",
		"abcdef01 -> 98765432  [stands: no direction knowable, so no edge of this pair moved]",
		"98765432 -> abcdef01  [stands: no direction knowable, so no edge of this pair moved]",
		"a re-run will not change that",
		"ghost supersede proj --withdraw 'abcdef0123456789' '9876543210fedcba' --apply",
		"ghost supersede proj --withdraw '9876543210fedcba' 'abcdef0123456789' --apply",
	} {
		if !strings.Contains(unoriented, want) {
			t.Errorf("cycle report missing %q:\n%s", want, unoriented)
		}
	}
	// A cycle with NO VERDICT is answered by a RERUN, not by the operator: the
	// pass asked and got nothing, so a transient classify failure — #699's mode —
	// must not be reported as a missing chronology and answered with "delete an
	// edge". No --withdraw command is offered for it.
	noVerdict := cycle(supersede.CycleNoVerdict, true, nil)
	if !strings.Contains(noVerdict, "the next pass re-asks it") {
		t.Errorf("a no-verdict cycle does not say the next pass re-asks it:\n%s", noVerdict)
	}
	if strings.Contains(noVerdict, "--withdraw ") {
		t.Errorf("a no-verdict cycle printed withdraw commands, answering a transient harness failure with a deletion:\n%s", noVerdict)
	}
	// A settled cycle says which edge stands, and it does NOT offer the operator
	// the commands: the pass already withdrew one of the two, so naming an edge to
	// withdraw would be naming the one it just removed.
	settled := cycle(supersede.CycleKeptSecond, true, []supersede.WithdrawnEdge{
		{NewerID: cFirst, OlderID: cSecond, Reason: "the reverse of the direction the verdict confirmed", Written: true},
	})
	if !strings.Contains(settled, "98765432 -> abcdef01  [stands: this is the direction the verdict named]") ||
		!strings.Contains(settled, "abcdef01 -> 98765432  [withdrew: the reverse of the direction the verdict named]") {
		t.Errorf("settled cycle does not say which edge stands and moved:\n%s", settled)
	}
	if strings.Contains(settled, "--withdraw ") {
		t.Errorf("a settled cycle printed a withdraw command, for an edge the pass already withdrew:\n%s", settled)
	}
	// The SAME settled cycle in a DRY RUN says "would withdraw" everywhere, and
	// claims no write: the block must be as mode-aware as the rows above it. Both
	// tenses are pinned, each on the run it belongs to — a block that always
	// predicted would satisfy the dry-run half alone, and one that always
	// observed would satisfy the apply half alone.
	if !strings.Contains(settled, "so that edge stands and its reverse is denied") {
		t.Errorf("an applied cycle block does not say which edge the verdict named:\n%s", settled)
	}
	dryCycle := cycle(supersede.CycleKeptSecond, false, []supersede.WithdrawnEdge{
		{NewerID: cFirst, OlderID: cSecond, Reason: "the reverse of the direction the verdict confirmed"},
	})
	if !strings.Contains(dryCycle, "abcdef01 -> 98765432  [would withdraw: the reverse of the direction the verdict named]") {
		t.Errorf("a dry-run cycle block does not predict the withdrawal:\n%s", dryCycle)
	}
	for _, tense := range []string{"withdrew", "was withdrawn", "were withdrawn"} {
		if strings.Contains(dryCycle, tense) {
			t.Errorf("a dry-run cycle block contains the past tense %q:\n%s", tense, dryCycle)
		}
	}
	// An edge a concurrent pass withdrew first, and one whose write was never
	// reached, are neither of the two states above: calling either "withdrew"
	// claims a deletion this run did not make, and calling either "would
	// withdraw" under --apply tells the operator to re-run a pass that ran.
	taken := cycle(supersede.CycleKeptSecond, true, []supersede.WithdrawnEdge{
		{NewerID: cFirst, OlderID: cSecond, Reason: "the reverse of the direction the verdict confirmed", Written: false},
	})
	if !strings.Contains(taken, "[already gone:") {
		t.Errorf("a cycle block does not mark an edge a concurrent pass took first:\n%s", taken)
	}
	notReached := cycle(supersede.CycleBothWithdrawn, true, nil)
	if !strings.Contains(notReached, "STILL LIVE") {
		t.Errorf("a cycle block claims nothing about an edge with no withdrawal row under --apply:\n%s", notReached)
	}
	// A cycle whose both edges the verdict denied says so on each row, and still
	// prints no command — there is nothing left to withdraw.
	if both := cycle(supersede.CycleBothWithdrawn, true, []supersede.WithdrawnEdge{
		{NewerID: cFirst, OlderID: cSecond, Reason: "neither", Written: true},
		{NewerID: cSecond, OlderID: cFirst, Reason: "neither", Written: true},
	}); !strings.Contains(both, "[withdrew: the two notes are not a replacement of one another]") ||
		strings.Count(both, "[withdrew: the two notes are not a replacement of one another]") != 2 {
		t.Errorf("a both-denied cycle does not mark both rows:\n%s", both)
	}

	// Under --apply the list says so per edge, and the headline counts the rows
	// actually invalidated — one here, because a concurrent pass withdrew the
	// vetoed edge first and the report must not claim this pass wrote it.
	applied := []supersede.WithdrawnEdge{
		{NewerID: edges[1].NewerID, OlderID: edges[1].OlderID, Reason: edges[1].Reason, Written: true, CausesSwept: 1},
		{NewerID: edges[0].NewerID, OlderID: edges[0].OlderID, Reason: edges[0].Reason, Vetoed: true},
	}
	apply := supersedeReassessReport("proj", supersede.ReassessResult{
		Loaded: 2, Vetoed: 1, Neither: 1, Withdrawn: 1, CausesWithdrawn: 1,
	}, true, applied, 1, 0)
	if !strings.Contains(apply, "withdrew 1, swept 1 causes edge(s) (1 classify call(s))") {
		t.Errorf("apply report does not count the withdrawal that landed and the edge swept with it:\n%s", apply)
	}
	if !strings.Contains(apply, "  withdrew     11223344 -> 88776655  [classifier]  [+1 causes edge]") {
		t.Errorf("apply report does not mark the edge it wrote, or the causes edge it swept with it:\n%s", apply)
	}
	if strings.Contains(apply, "Re-run with --apply") {
		t.Errorf("an applied report must not offer to apply again:\n%s", apply)
	}
	// A row a concurrent pass withdrew first is neither of the other two
	// markers: calling it "would withdraw" under --apply claims a deletion that
	// did not happen.
	if !strings.Contains(apply, "  already gone  abcdef01 -> 98765432") {
		t.Errorf("apply report mislabels the edge a concurrent pass withdrew first:\n%s", apply)
	}

	// A sweep that failed says "unknown" — on the row and in the summary — and
	// never a count, because after a failed write the count is not knowable and
	// a 0 would read as "nothing else was deleted".
	failed := []supersede.WithdrawnEdge{
		{NewerID: edges[1].NewerID, OlderID: edges[1].OlderID, Reason: edges[1].Reason, Written: true, SweepFailed: true},
	}
	failReport := supersedeReassessReport("proj", supersede.ReassessResult{
		Loaded: 1, Neither: 1, Withdrawn: 1, CausesSweepFailed: 1,
	}, true, failed, 1, 0)
	if !strings.Contains(failReport, "[causes sweep FAILED — unknown]") {
		t.Errorf("a failed sweep must not be reported as a count:\n%s", failReport)
	}
	if !strings.Contains(failReport, "1 causes sweep(s) FAILED (unknown)") {
		t.Errorf("the summary must count the failed sweeps separately from the swept ones:\n%s", failReport)
	}
	if strings.Contains(failReport, "[+0 causes edge]") {
		t.Errorf("a failed sweep printed a marker that says nothing was moved:\n%s", failReport)
	}

	// A dry-run row whose 'causes' PREDICTION could not be read is a different
	// state from a failed sweep (no sweep ran) and a third from "there is
	// nothing else to delete" — the report has to say which, or an operator about
	// to apply is deciding about a deletion nobody looked for.
	prediction := supersedeReassessReport("proj", supersede.ReassessResult{
		Loaded: 1, Vetoed: 1, CausesPredictionFailed: 1,
	}, false, []supersede.WithdrawnEdge{
		{NewerID: edges[0].NewerID, OlderID: edges[0].OlderID, Reason: edges[0].Reason, Vetoed: true, PredictionUnknown: true},
	}, 0, 0)
	if !strings.Contains(prediction, "would withdraw 1") {
		t.Errorf("a dry run whose prediction read failed reported no withdrawal at all:\n%s", prediction)
	}
	if !strings.Contains(prediction, "1 causes prediction(s) unavailable (read failed, unknown)") {
		t.Errorf("the summary must count the unreadable predictions separately from the failed sweeps:\n%s", prediction)
	}
	if !strings.Contains(prediction, "[causes edge — unknown: the prediction read failed]") {
		t.Errorf("the row must say the count is unknown rather than printing a 0:\n%s", prediction)
	}
	if strings.Contains(prediction, "causes sweep(s) FAILED") || strings.Contains(prediction, "withdrew ") {
		t.Errorf("a dry run printed a marker it did not earn:\n%s", prediction)
	}

	// #699: a classify call that failed leaves its pairs UNJUDGED while the rows
	// a rule settled still withdraw. The report has to name both, or a partial
	// repair reads as a complete one — the withdrawal lines look like the whole
	// story, and the edges that never reached a verdict would be invisible.
	partial := supersedeReassessReport("proj", supersede.ReassessResult{
		Loaded: 3, Vetoed: 1, Withdrawn: 1,
		Unjudged: []supersede.UnjudgedPair{
			{NewerID: "5566778899aabbcc", OlderID: "445566778899aabb"},
		},
	}, true, []supersede.WithdrawnEdge{
		{NewerID: edges[0].NewerID, OlderID: edges[0].OlderID, Reason: edges[0].Reason, Vetoed: true, Written: true},
	}, 2, 1)
	for _, want := range []string{
		"0 UNKNOWN, 1 unjudged (no verdict: the classify call failed or answered with the wrong number of verdicts; their edges stand), withdrew 1",
		"(2 classify call(s), 1 retried after a failed call)",
		"  withdrew     abcdef01 -> 98765432  [veto, no harness call]",
		"  unjudged    55667788 -> 44556677  [no verdict: the classify call failed or answered with the wrong number of verdicts, so the edge stands and the next pass re-asks it]",
	} {
		if !strings.Contains(partial, want) {
			t.Errorf("partial-repair report missing %q:\n%s", want, partial)
		}
	}
}

// TestParseSupersedeArgs pins `ghost supersede` argv parsing: same shapes as
// resolve, --threshold exists in both value forms, defaulting to 0.80 when it
// is not typed and refused when its value is not a number in (0, 1] (see
// supersede_threshold_test.go), and --reassess is resolve's repair flag with the same
// meaning (#686): the edges are already in the graph, so --apply withdraws them
// instead of writing new ones. The project repeats — flag then flag, operand
// then flag, either order — are in TestScopeFlagParsersRefuseEmptyAndRepeated,
// one table for every parser that takes a project.
func TestParseSupersedeArgs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		project   string
		source    string
		apply     bool
		reassess  bool
		threshold float32
	}{
		{"positional", []string{"myproj"}, "myproj", "", false, false, 0.80},
		{"positional with apply", []string{"myproj", "--apply"}, "myproj", "", true, false, 0.80},
		{"reassess dry run", []string{"myproj", "--reassess"}, "myproj", "", false, true, 0.80},
		{"reassess with apply", []string{"myproj", "--reassess", "--apply"}, "myproj", "", true, true, 0.80},
		{"reassess before project", []string{"--reassess", "myproj"}, "myproj", "", false, true, 0.80},
		{"threshold separate value", []string{"myproj", "--threshold", "0.5"}, "myproj", "", false, false, 0.5},
		{"threshold equals value", []string{"myproj", "--threshold=0.25"}, "myproj", "", false, false, 0.25},
		{"source separate value", []string{"myproj", "--source", "opencode"}, "myproj", "opencode", false, false, 0.80},
		{"source equals value", []string{"myproj", "--source=codex"}, "myproj", "codex", false, false, 0.80},
		{"project flag dash value", []string{"--project", "-x", "--apply"}, "-x", "", true, false, 0.80},
		{"project flag double-dash value", []string{"--project", "--odd"}, "--odd", "", false, false, 0.80},
		{"project flag lifecycle shape", []string{"--project", "-myproj", "--apply", "--source", "claude"}, "-myproj", "claude", true, false, 0.80},
		{"project equals form", []string{"--project=-eq"}, "-eq", "", false, false, 0.80},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project, source, apply, reassess, threshold, _, withdraw, _, err := parseSupersedeArgs(tc.args)
			if err != nil {
				t.Fatalf("parseSupersedeArgs(%v): %v", tc.args, err)
			}
			if project != tc.project || source != tc.source || apply != tc.apply ||
				reassess != tc.reassess || threshold != tc.threshold {
				t.Errorf("parseSupersedeArgs(%v) = (%q, %q, %v, %v, %v), want (%q, %q, %v, %v, %v)",
					tc.args, project, source, apply, reassess, threshold,
					tc.project, tc.source, tc.apply, tc.reassess, tc.threshold)
			}
			if len(withdraw) != 0 {
				t.Errorf("parseSupersedeArgs(%v) withdrew %+v, want nothing: no case names an edge", tc.args, withdraw)
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
		{"threshold bad value", []string{"myproj", "--threshold", "abc"}, `--threshold needs a cosine similarity in (0, 1], e.g. 0.85: got "abc"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, _, _, _, _, _, err := parseSupersedeArgs(tc.args)
			if err == nil {
				t.Fatalf("parseSupersedeArgs(%v) must fail", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q must contain %q", err, tc.want)
			}
		})
	}
}

// TestConsolidationContextZeroLiftsTheClientDefault is the other half of
// "zero means unbounded". A context with no deadline is not enough on its own:
// every harness client applies its own per-call default to a context that has
// none, so an explicit 0 used to end each harness call at that default. The
// zero branch must say so to the client, and only the zero branch: a
// configured or defaulted timeout keeps its deadline and no marker, so the
// client's default never comes into it.
func TestConsolidationContextZeroLiftsTheClientDefault(t *testing.T) {
	if !ai.DefaultBoundRemoved(mustCtx(t, 0)) {
		t.Error("zero minutes must lift the harness client's per-call default")
	}
	for _, minutes := range []int{5, -1, 200000000} {
		if ai.DefaultBoundRemoved(mustCtx(t, minutes)) {
			t.Errorf("%d minutes must not lift the harness client's per-call default", minutes)
		}
	}
	if ai.DefaultBoundRemoved(context.Background()) {
		t.Error("a context the consolidation path did not build must keep the default")
	}
}

// mustCtx returns the context the consolidation call site builds for the given
// reflection.consolidation_timeout_minutes, without leaking its cancel.
func mustCtx(t *testing.T, minutes int) context.Context {
	t.Helper()
	rc := config.ReflectionConfig{ConsolidationTimeoutMinutes: minutes}
	ctx, cancel := consolidationContext(context.Background(), rc.ConsolidationTimeout())
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

// TestPrintDeleteSummary_FieldsNotTransposed pins the label-to-value mapping
// with seven distinct values (one per field) and a whole-string comparison, so
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
		// A seventh distinct value, one per field, so a swap inside
		// printDeleteSummary fails this test rather than passing silently --
		// which is the whole reason the fixture is shaped this way. RetrievalRecords
		// is here because DeleteProject removes those rows (#646), and a summary
		// that omitted a table its own command deletes would under-report.
		RetrievalRecords: 7,
		// And a ninth, for the objections #648 slice 2 files. A summary that omitted
		// a table its own command deletes would under-report, and the distinct
		// value is what keeps a swap of the flags line from passing.
		MemoryFlags: 9,
	}, "Would delete"); err != nil {
		t.Fatalf("printDeleteSummary: %v", err)
	}

	want := `Would delete test-project (proj):
  memories:     1
  memory_links: 2
  tasks:        3
  decisions:    4
  token_usage:  5
  audit_log:    6
  retrievals:   7
  flags:        9
`
	if out.String() != want {
		t.Errorf("printDeleteSummary output mismatch:\ngot:\n%s\nwant:\n%s", out.String(), want)
	}
}
