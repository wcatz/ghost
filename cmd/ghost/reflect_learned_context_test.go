package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// learnedContextHarness writes a fake `claude` that answers one reflect round
// with the given learned_context and operations, and appends a line to calls
// on every answer. Only shell builtins run in it, so the empty PATH that
// isolatedLifecycleEnv pins is the whole allowlist.
func learnedContextHarness(t *testing.T, dir, calls, learned string, ops []string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"learned_context": learned, "ops": ops})
	if err != nil {
		t.Fatalf("build harness response: %v", err)
	}
	if strings.ContainsAny(string(payload), "'") {
		t.Fatalf("the harness payload would need shell quoting to survive: %s", payload)
	}
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--help\" ]; then\n" +
		"  printf '%s\\n' '--safe-mode' '--restricted' '--strict-mcp-config' '--disable-slash-commands' '--tools' '--disallowedTools' '--setting-sources'\n" +
		"  exit 0\n" +
		"fi\n" +
		"echo x >> " + calls + "\n" +
		"printf '%s' '" + string(payload) + "'\n"
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec // an executable this test runs
		t.Fatalf("write fake harness: %v", err)
	}
	return path
}

const (
	lcProject  = "lcproj"
	lcForeign  = "zebraproj"
	lcManifest = "the ingest pipeline writes run manifests under /var/lib/ghost"
	lcLedger   = "the exporter reads the ledger from the shared cache"
	lcPrevious = "the ingest pipeline writes run manifests and the exporter reads the ledger"
	lcLearned  = "the exporter shares its ledger cache with zebraproj since commit 4f3a9c2e1"
)

// learnedContextProject seeds a project holding two memories and the given
// stored learned_context, beside a foreign project whose name the
// contamination guard therefore knows. It returns the data directory and a
// function that runs one `ghost reflect --apply` round against a fake claude
// harness and returns its stderr. Every round's harness answers the same
// thing: a rewrite naming the foreign project, and lcLearned, which carries
// the same claim plus a commit SHA nothing in the corpus holds.
func learnedContextProject(t *testing.T, stored string) (string, func(n int) string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binary requires a POSIX shell")
	}
	dataHome := isolatedLifecycleEnv(t)
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(dataHome, "ghost"), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	t.Setenv("GHOST_SCRATCH_DIR", t.TempDir())

	s := openReflectStore(t, dataHome, lcProject)
	if err := s.EnsureProject(ctx, lcForeign, "/nonexistent/"+lcForeign, lcForeign); err != nil {
		t.Fatalf("EnsureProject(%s): %v", lcForeign, err)
	}
	for _, content := range []string{lcManifest, lcLedger} {
		if _, _, _, err := s.Upsert(ctx, lcProject, "fact", content, "mcp", 0.6, nil); err != nil {
			t.Fatalf("seed %q: %v", content, err)
		}
	}
	if err := s.UpdateLearnedContext(ctx, lcProject, stored, "seed summary"); err != nil {
		t.Fatalf("seed learned context: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	round := func(n int) string {
		t.Helper()
		// Older than the round trip, so the replace does not treat a row the
		// previous round wrote as a concurrent save and leave it alone.
		if err := execBackdateAges(dataHome, lcProject); err != nil {
			t.Fatalf("round %d: backdate: %v", n, err)
		}
		st := openReflectStore(t, dataHome, lcProject)
		keepID := idByContent(t, st, lcProject, "run manifests")
		rewriteID := idByContent(t, st, lcProject, "reads the ledger")
		if err := st.Close(); err != nil {
			t.Fatalf("round %d: close store: %v", n, err)
		}
		dir := t.TempDir()
		calls := filepath.Join(dir, "calls")
		t.Setenv("GHOST_CLI_CLAUDE_BINARY", learnedContextHarness(t, dir, calls, lcLearned, []string{
			"keep " + keepID,
			"rewrite " + rewriteID + " -> " + lcLedger + " owned by " + lcForeign,
		}))
		stderr := captureStderr(t, func() {
			_ = captureStdout(t, func() {
				runReflectArgs(t, "reflect", lcProject, "--tier", "cli", "--apply")
			})
		})
		if c := countLines(t, calls); c != 1 {
			t.Fatalf("round %d: the harness answered %d times, want 1\n%s", n, c, stderr)
		}
		return stderr
	}
	return dataHome, round
}

// assertNoForeignMemory fails when any live memory of the project names the
// foreign project.
func assertNoForeignMemory(t *testing.T, dataHome string, n int) {
	t.Helper()
	st := openReflectStore(t, dataHome, lcProject)
	defer st.Close() //nolint:errcheck
	all, err := st.GetAll(context.Background(), lcProject, -1)
	if err != nil {
		t.Fatalf("round %d: GetAll: %v", n, err)
	}
	for _, m := range all {
		if strings.Contains(m.Content, lcForeign) {
			t.Errorf("round %d: a memory naming %s was written: %q", n, lcForeign, m.Content)
		}
	}
}

// TestRunReflectLearnedContextCannotLaunderAForeignProject drives two real
// `ghost reflect --apply` rounds against a fake claude harness that answers
// both with the same thing: a rewrite naming a project this run never saw, and
// a learned_context carrying the same claim plus a commit SHA nothing in the
// corpus holds.
//
// Round 1 is where the guard used to look away: the rewrite was dropped, but
// the learned_context was stored verbatim. Round 2 is where that mattered:
// the stored text was part of the contamination guard's corpus, so the same
// rewrite was accepted and written. Both halves are pinned here. The stored
// learned_context must stay the clean one seeded before round 1, and no live
// memory may name the foreign project after either round.
func TestRunReflectLearnedContextCannotLaunderAForeignProject(t *testing.T) {
	dataHome, round := learnedContextProject(t, lcPrevious)
	check := func(n int, stderr string) {
		t.Helper()
		st := openReflectStore(t, dataHome, lcProject)
		got, err := st.GetLearnedContext(context.Background(), lcProject)
		_ = st.Close()
		if err != nil {
			t.Fatalf("round %d: GetLearnedContext: %v", n, err)
		}
		if got != lcPrevious {
			t.Errorf("round %d: learned_context = %q, want the previous %q kept: a "+
				"harness paragraph naming a project absent from the input, with a "+
				"commit SHA nothing holds, was stored", n, got, lcPrevious)
		}
		assertNoForeignMemory(t, dataHome, n)
		// The refusal is reported by the guard's own log line, and the
		// paragraph it refused is not echoed into it.
		if !strings.Contains(stderr, "reflection dropped the learned context") {
			t.Errorf("round %d: the refused learned context was not reported:\n%s", n, stderr)
		}
		if strings.Contains(stderr, "shares its ledger cache") {
			t.Errorf("round %d: the refused learned context was echoed verbatim:\n%s", n, stderr)
		}
	}
	check(1, round(1))
	check(2, round(2))
}

// TestRunReflectAStoredLearnedContextDoesNotLicenseAForeignProject is the
// corpus half on its own, for a store that already holds the paragraph an
// unguarded build would have written. The stored text names the foreign
// project, and the memory naming it must still be dropped: the stored
// learned_context is the harness's own earlier output, not evidence.
func TestRunReflectAStoredLearnedContextDoesNotLicenseAForeignProject(t *testing.T) {
	dataHome, round := learnedContextProject(t, lcLearned)
	stderr := round(1)
	assertNoForeignMemory(t, dataHome, 1)
	if !strings.Contains(stderr, "reflection dropped a memory naming a project absent from the input") {
		t.Errorf("the memory naming %s was not dropped by the contamination guard:\n%s", lcForeign, stderr)
	}
}
