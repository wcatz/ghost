package mcpinit

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

// largeSessionStore seeds a store with n project memories and a fixed 120-row
// global set, for the session-start latency measurement. Rows carry explicit
// created_at values so the decay ranking is a function of the fixture rather
// than of when the test runs, and the category mix is wide so the two-pass
// behavioral floor has a behavioral pool, a non-behavioral one, and more rows
// than either cap admits.
func largeSessionStore(tb testing.TB, xdgHome string, n int) string {
	tb.Helper()
	ghostDir := filepath.Join(xdgHome, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		tb.Fatalf("mkdir: %v", err)
	}
	db, err := memory.OpenDB(filepath.Join(ghostDir, "ghost.db"))
	if err != nil {
		tb.Fatalf("OpenDB: %v", err)
	}
	defer db.Close() //nolint:errcheck

	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`); err != nil {
		tb.Fatalf("insert _global: %v", err)
	}
	// The directory's basename MUST equal the project name. Store.ResolveProject
	// resolves by exact path, then path prefix, then BASENAME — and the basename
	// step is what makes this fixture portable, because `t.TempDir()` lives under
	// the user's home directory on Windows (where the path carries an 8.3 short
	// component) and not under /tmp as it does on Linux. A directory named after
	// the store size instead of the project therefore resolved on Linux and
	// matched nothing on Windows. Each call gets its own t.TempDir(), so one
	// name serves every size.
	projectPath := filepath.Join(tb.TempDir(), "perfproj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		tb.Fatalf("mkdir project: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(projectPath)
	if err != nil {
		tb.Fatalf("EvalSymlinks: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES (?, ?, ?)`, "pperf", canonical, "perfproj"); err != nil {
		tb.Fatalf("insert project: %v", err)
	}
	// The CANONICAL path is what the caller goes on to use, so the exact-path step
	// of ResolveProject is the one that matches and the fixture does not depend on
	// the path-prefix or basename steps agreeing about a platform's temp path.

	// The measurement's constant: 120 globals is 15x the 8-item global cap, so
	// the global over-fetch, its near-duplicate pass and its cap all engage.
	const globals = 120
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	cats := []string{"preference", "gotcha", "convention", "fact", "architecture", "pattern", "decision", "dependency"}
	tx, err := db.Begin()
	if err != nil {
		tb.Fatalf("begin: %v", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO memories
		(id, project_id, category, content, source, importance, pinned, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'manual', ?, ?, ?, ?)`)
	if err != nil {
		tb.Fatalf("prepare: %v", err)
	}
	stamp := func(i int) string { return base.Add(time.Duration(i) * time.Hour).Format("2006-01-02 15:04:05") }
	for i := 0; i < n; i++ {
		if _, err := stmt.Exec(
			fmt.Sprintf("lmem%06d", i), "pperf", cats[i%len(cats)],
			fmt.Sprintf("performance fixture memory %06d with enough text to make the row realistic in length", i),
			0.9-float64(i%70)*0.01, i%17 == 0, stamp(i), stamp(i),
		); err != nil {
			tb.Fatalf("insert %d: %v", i, err)
		}
	}
	for i := 0; i < globals; i++ {
		if _, err := stmt.Exec(
			fmt.Sprintf("gmem%06d", i), "_global", "preference",
			fmt.Sprintf("global performance fixture memory %06d with enough text to be realistic", i),
			0.9-float64(i%40)*0.01, i%11 == 0, stamp(i), stamp(i),
		); err != nil {
			tb.Fatalf("insert global %d: %v", i, err)
		}
	}
	if err := stmt.Close(); err != nil {
		tb.Fatalf("close stmt: %v", err)
	}
	if err := tx.Commit(); err != nil {
		tb.Fatalf("commit: %v", err)
	}
	return canonical
}

// measureSessionStart runs the session-start loader against a store and returns
// the wall time of the load.
//
// The measured span is loadSessionContext alone, not the whole handler: the
// handler's other costs (the lifecycle marker read, the session-count bump, the
// Obsidian worker) are unrelated to retrieval and would only add noise to a
// number whose entire point is what the assembler changed.
func measureSessionStart(tb testing.TB, projectPath string) time.Duration {
	tb.Helper()
	start := time.Now()
	loadSessionContext(projectPath, config.LoadForHook())
	return time.Since(start)
}

// sessionStartOn builds a store of size n in its own data dir and returns the
// mean steady-state session-start load over the given number of runs.
func sessionStartOn(tb testing.TB, n, runs int) (time.Duration, int, int) {
	tb.Helper()
	xdgHome := tb.TempDir()
	tb.Setenv("XDG_DATA_HOME", xdgHome)
	tb.Setenv("XDG_CONFIG_HOME", tb.TempDir())
	projectPath := largeSessionStore(tb, xdgHome, n)

	// Warm the page cache and the schema, so the figure is the steady-state cost
	// rather than a cold first read of a file that has just been written.
	for i := 0; i < 2; i++ {
		measureSessionStart(tb, projectPath)
	}

	var total time.Duration
	var rows, globals int
	for i := 0; i < runs; i++ {
		total += measureSessionStart(tb, projectPath)
		projectID, _, mems, _, _, _, _, _, _ := loadSessionContext(projectPath, config.LoadForHook())
		if projectID == "" {
			tb.Fatalf("fixture project did not resolve at n=%d", n)
		}
		rows = len(mems)
		g, _, _ := loadGlobals(config.LoadForHook())
		globals = len(g)
	}
	return total / time.Duration(runs), rows, globals
}

// This file and sessionstart_golden_test.go are BASELINES, and they measure the
// loader this migration is replacing rather than the path that replaces it — which
// is why they are recorded on the retriever PR, where the loader is still the code
// that runs, and not on the hook switch, where it is gone. The measurement of the
// NEW path lives in sessionstart_passive_latency_test.go and needs the assembler
// half to exist.
//
// TestSessionStartLatencyAt1000Memories is the number the PR body reports: the
// steady-state session-start load against a 1000-memory store.
//
// It also asserts the caps, which are a contract of the SURFACE rather than an
// outcome of this migration: whatever selects the rows, a session start shows at
// most 15 project and 8 global rows.
func TestSessionStartLatencyAt1000Memories(t *testing.T) {
	if testing.Short() {
		t.Skip("latency measurement: skipped in -short")
	}
	avg, rows, globals := sessionStartOn(t, 1000, 20)
	t.Logf("session-start load, 1000-memory store: mean %s over 20 runs (project rows %d, global rows %d)",
		avg, rows, globals)
	if rows == 0 || globals == 0 {
		t.Fatalf("fixture produced no rows: project %d, global %d", rows, globals)
	}
	if rows > sessionMemoriesCap {
		t.Errorf("project rows: got %d, want at most %d", rows, sessionMemoriesCap)
	}
	if globals > globalsCap {
		t.Errorf("global rows: got %d, want at most %d", globals, globalsCap)
	}
}

// TestSessionStartLoadDoesNotScaleWithStoreSize characterises the LOADER — the
// code that ships today and that this PR does not touch. It is the baseline half
// of the measurement; the half that can fail for an implementation of passive
// retrieval is TestPassiveAssemblerLoadDoesNotScaleWithStoreSize below, which
// runs the same stores through assemble.Run.
//
// It is deliberately modest about what it proves.
//
// The loaders' LIMIT chose which rows are read at all, so moving selection
// behind the assembler risks turning every session start into a whole-store scan
// — which returns the same 15 rows and is invisible from the outside.
//
// What this test is NOT is a proof that the path is bounded. At 100 memories the
// load is ~2.4-4.3ms and at 1000 ~2.6-5.7ms on a shared laptop, with the two
// distributions overlapping almost entirely: most of a session-start load is
// opening the database, resolving the project and reading tasks and decisions, so
// a 1000-row scan of a table this size adds a millisecond or two to a multi-
// millisecond fixed cost. A whole-store read here would plausibly read 2-4x, not
// 10x, so the 5x ceiling below is a backstop against a CATASTROPHIC regression
// rather than a detector. The real guarantee is not a timing at all: both seams
// REFUSE a passive policy that states no over-fetch, so an unbounded fetch cannot
// be expressed. This test confirms the bounded window is real in practice; the
// refusal is what makes it structural.
//
// The bound is a RATIO rather than a duration for the same reason: a wall-clock
// ceiling on a shared laptop is a flake, while a growth ratio is a property of the
// access path. Across ten runs on origin/main and on this branch the ratio
// ranged 1.10x-3.13x, all well inside the 5x ceiling and all in a band where the
// median moved less than the run-to-run spread.
func TestSessionStartLoadDoesNotScaleWithStoreSize(t *testing.T) {
	if testing.Short() {
		t.Skip("latency measurement: skipped in -short")
	}
	small, smallRows, _ := sessionStartOn(t, 100, 20)
	large, largeRows, _ := sessionStartOn(t, 1000, 20)
	t.Logf("session-start load: 100 memories %s (%d rows), 1000 memories %s (%d rows), ratio %.2fx for 10x the rows",
		small, smallRows, large, largeRows, float64(large)/float64(small))

	if small <= 0 {
		t.Fatalf("implausible measurement at 100 memories: %s", small)
	}
	if ratio := float64(large) / float64(small); ratio > 5 {
		t.Errorf("session-start load grew %.2fx for a 10x larger store: the path is reading the whole store rather than a bounded window "+
			"(100 memories %s, 1000 memories %s)", ratio, small, large)
	}
}
