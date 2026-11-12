package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// Issue #1492 at the command: `ghost reflect`, a merge whose sources' scopes
// disagree.
//
// The store refuses such a merge and keeps both sources, and that refusal used
// to be counted as a claim on a row that changed while consolidation ran. So the
// apply blamed a writer that did not exist and withheld the skip fingerprint,
// and every later `--skip-unchanged` round paid a harness call to be refused the
// same way. The dry run listed the merge as one that would happen. These tests
// drive the real command with a fake harness that counts its own invocations,
// as #1228's overflow tests do.

const (
	scopeGateProdRow    = "the api deploy uses the blue green rollout with manual approval gates"
	scopeGateStagingRow = "the api deploy uses the blue green rollout without manual approval gates"
	scopeGateMergeText  = "the api deploy uses the blue green rollout, with manual approval gates in one environment and without manual approval gates in the other"
	scopeUnrelatedRow   = "the consumer restarts in place after a config change"
)

// seedScopeRefusalCorpus writes the two disagreeing rows and, when withOther is
// set, an unrelated unscoped row a keep can name. Backdated so no row reads as
// a save that raced the run.
func seedScopeRefusalCorpus(t *testing.T, dataHome, project string, prodScope, stagingScope map[string]string, withOther bool) (prod, staging, other string) {
	t.Helper()
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(dataHome, "ghost"), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	s := openReflectStore(t, dataHome, project)
	var err error
	if prod, err = s.Create(ctx, project, memory.Memory{Category: "fact", Source: "mcp", Importance: 0.6, Content: scopeGateProdRow, Scope: prodScope}); err != nil {
		t.Fatalf("create prod row: %v", err)
	}
	if staging, err = s.Create(ctx, project, memory.Memory{Category: "fact", Source: "mcp", Importance: 0.6, Content: scopeGateStagingRow, Scope: stagingScope}); err != nil {
		t.Fatalf("create staging row: %v", err)
	}
	if withOther {
		if other, err = s.Create(ctx, project, memory.Memory{Category: "fact", Source: "mcp", Importance: 0.5, Content: scopeUnrelatedRow}); err != nil {
			t.Fatalf("create unrelated row: %v", err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
	if err := execBackdate(dataHome, project); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	return prod, staging, other
}

// inputAccounting is the "Inputs (...)" section of one reflect report, from its
// header through the Kept/Passed count line.
func inputAccounting(t *testing.T, report string) string {
	t.Helper()
	start := strings.Index(report, "Inputs (")
	if start < 0 {
		t.Fatalf("the report has no input accounting:\n%s", report)
	}
	rest := report[start:]
	end := strings.Index(rest, "Kept verbatim: ")
	if end < 0 {
		t.Fatalf("the input accounting has no closing count line:\n%s", rest)
	}
	if nl := strings.IndexByte(rest[end:], '\n'); nl >= 0 {
		end += nl
	}
	return rest[:end]
}

// TestRunReflectReportsAScopeRefusedMergeAndDoesNotReproposeIt is the issue's
// reproduction: a dry run, then `--apply --skip-unchanged` twice.
func TestRunReflectReportsAScopeRefusedMergeAndDoesNotReproposeIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake harness requires a POSIX shell")
	}
	dataHome := isolatedLifecycleEnv(t)
	const project = "scopegate"
	prod, staging, other := seedScopeRefusalCorpus(t, dataHome, project,
		map[string]string{"environment": "production"}, map[string]string{"environment": "staging"}, true)

	calls := filepath.Join(t.TempDir(), "calls")
	harness := fakeReflectHarness(t, calls, prod+","+staging, scopeGateMergeText, other)
	t.Setenv("GHOST_CLI_CLAUDE_BINARY", harness)
	t.Setenv("GHOST_SCRATCH_DIR", t.TempDir())

	var dryOut string
	_ = captureStderr(t, func() {
		dryOut = captureStdout(t, func() { runReflectArgs(t, "reflect", project, "--tier", "cli") })
	})
	var applyOut string
	applyErr := captureStderr(t, func() {
		applyOut = captureStdout(t, func() { runReflectArgs(t, "reflect", project, "--tier", "cli", "--apply", "--skip-unchanged") })
	})
	t.Logf("dry run:\n%s\napply stdout:\n%s\napply stderr:\n%s", dryOut, applyOut, applyErr)

	// The dry run and the apply print one accounting, and it does not list the
	// refused merge as a merge.
	dry, applied := inputAccounting(t, dryOut), inputAccounting(t, applyOut)
	if dry != applied {
		t.Errorf("the dry run's accounting is not the apply's:\ndry run:\n%s\napply:\n%s", dry, applied)
	}
	if !strings.Contains(dry, "Merges (0):") {
		t.Errorf("the dry run still lists the scope-refused merge as a merge:\n%s", dry)
	}
	if !strings.Contains(dry, "Refused because the sources' scopes disagree (2):") {
		t.Errorf("the dry run does not list the merge the apply refuses:\n%s", dry)
	}

	// The apply names the refusal on its own terms, and blames no writer.
	if !strings.Contains(applyErr, "refused because their sources' scopes disagree") {
		t.Errorf("the apply does not report the scope refusal:\n%s", applyErr)
	}
	if strings.Contains(applyErr, "changed or was deleted") {
		t.Errorf("the scope refusal is still reported as a concurrent edit:\n%s", applyErr)
	}
	if strings.Contains(applyErr, "kept untouched") {
		t.Errorf("the sources a scope refusal keeps are still reported as survivors:\n%s", applyErr)
	}

	// Both sources are still there, each under its own scope.
	db, err := openRawDB(dataHome)
	if err != nil {
		t.Fatalf("open for read: %v", err)
	}
	for id, want := range map[string]string{prod: "production", staging: "staging"} {
		var raw string
		if err := db.QueryRow(`SELECT COALESCE(scope, '') FROM memories WHERE id = ?`, id).Scan(&raw); err != nil {
			t.Fatalf("source %s is gone after the refusal: %v", id, err)
		}
		var scope map[string]string
		if err := json.Unmarshal([]byte(raw), &scope); err != nil || scope["environment"] != want {
			t.Errorf("source %s scope = %q, want environment=%s", id, raw, want)
		}
	}
	_ = db.Close()

	if got := readReflectSignature(t, dataHome); got == "" {
		t.Fatal("no skip fingerprint was recorded: --skip-unchanged would re-propose the same refused merge on every run")
	}

	second := captureStderr(t, func() {
		_ = captureStdout(t, func() { runReflectArgs(t, "reflect", project, "--tier", "cli", "--apply", "--skip-unchanged") })
	})
	t.Logf("second stderr:\n%s", second)
	// One call for the dry run and one for the first apply; the second apply
	// must skip without reaching the harness.
	if n := countLines(t, calls); n != 2 {
		t.Errorf("the harness answered %d rounds, want 2: the refused merge was proposed again", n)
	}
}

// gateSleep is sleep's absolute path, resolved when the package loads and so
// before isolatedLifecycleEnv points PATH at an empty directory. The harness
// inherits that PATH, where a bare `sleep` is not found; the wait loop then
// fails every turn at once, the harness answers within a few milliseconds
// without seeing release, and on a slow machine the round has applied before
// the test's edit lands.
var gateSleep, _ = exec.LookPath("sleep")

// gatedReflectHarness is fakeReflectHarness with a pause in the turn: it
// touches started, waits (bounded) for release to exist, and only then answers
// with ops. That is the window a concurrent edit lands in. A wait that ends
// without release fails the harness, so a gate that does not hold fails the
// round instead of answering early.
func gatedReflectHarness(t *testing.T, calls, started, release string, ops []string) string {
	t.Helper()
	if gateSleep == "" {
		t.Fatal("no sleep binary on the PATH the tests started with; the harness gate cannot wait")
	}
	payload, err := json.Marshal(map[string]any{"ops": ops})
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
		": > " + started + "\n" +
		"i=0\n" +
		"while [ ! -f " + release + " ] && [ $i -lt 600 ]; do " + gateSleep + " 0.05 || exit 1; i=$((i+1)); done\n" +
		"[ -f " + release + " ] || { echo 'the gate was never released' >&2; exit 1; }\n" +
		"printf '%s' '" + string(payload) + "'\n"
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake harness: %v", err)
	}
	return path
}

// waitForFile waits for the gated harness to report that it is running.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("the harness never started (%s)", path)
}
