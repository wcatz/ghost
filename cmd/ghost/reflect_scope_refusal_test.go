package main

import (
	"context"
	"encoding/json"
	"os"
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

// TestRunReflectSaysAScopeOnlyRoundAppliedNothingAndStillSkips is the round
// whose only emission is the refused merge, so nothing is written or deleted:
// the report has to say why in its own words, not as a credential or a row
// that moved, and still record the fingerprint.
func TestRunReflectSaysAScopeOnlyRoundAppliedNothingAndStillSkips(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake harness requires a POSIX shell")
	}
	dataHome := isolatedLifecycleEnv(t)
	const project = "scopeonly"
	// A scoped row and an unscoped one: the mix is refused like a disagreement.
	prod, unscoped, _ := seedScopeRefusalCorpus(t, dataHome, project,
		map[string]string{"environment": "production"}, nil, false)

	calls := filepath.Join(t.TempDir(), "calls")
	harness := fakeReflectHarness(t, calls, prod+","+unscoped, scopeGateMergeText, "")
	t.Setenv("GHOST_CLI_CLAUDE_BINARY", harness)
	t.Setenv("GHOST_SCRATCH_DIR", t.TempDir())

	report := captureStderr(t, func() {
		runReflectArgs(t, "reflect", project, "--tier", "cli", "--apply")
	})
	t.Logf("report:\n%s", report)
	if !strings.Contains(report, "every consolidation proposal merged memories whose scopes disagree") {
		t.Errorf("a scope-only round did not report the refusal as its reason:\n%s", report)
	}
	for _, wrong := range []string{"held a credential value", "changed or was deleted", "no skip fingerprint is recorded"} {
		if strings.Contains(report, wrong) {
			t.Errorf("a scope-only round reported %q:\n%s", wrong, report)
		}
	}
	if got := readReflectSignature(t, dataHome); got == "" {
		t.Fatal("no skip fingerprint was recorded for a round whose only refusal recurs on every run")
	}
	second := captureStderr(t, func() {
		runReflectArgs(t, "reflect", project, "--tier", "cli", "--apply", "--skip-unchanged")
	})
	if !strings.Contains(second, "unchanged since the last applied consolidation") {
		t.Errorf("the second --skip-unchanged run did not skip: %s", second)
	}
	if n := countLines(t, calls); n != 1 {
		t.Errorf("the harness answered %d rounds, want 1", n)
	}
}

// TestRunReflectConcurrentEditStillWithholdsTheFingerprint is the control: a
// row that really is edited while the harness is answering is still the corpus
// guard's drop, reported in the same words and withholding the fingerprint —
// on its own, and in a round that also refuses a merge over its scopes.
//
// The edit lands during the harness call: the fake harness signals that it is
// running and waits for the test to release it, and the test edits the row in
// between, so the claim the round makes is on a row that changed.
func TestRunReflectConcurrentEditStillWithholdsTheFingerprint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake harness requires a POSIX shell")
	}
	for _, tc := range []struct {
		name         string
		prod         map[string]string
		staging      map[string]string
		scopeRefusal bool
	}{
		{name: "alone", prod: nil, staging: nil, scopeRefusal: false},
		{name: "beside a scope refusal", prod: map[string]string{"environment": "production"}, staging: map[string]string{"environment": "staging"}, scopeRefusal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataHome := isolatedLifecycleEnv(t)
			const project = "scopeedit"
			prod, staging, other := seedScopeRefusalCorpus(t, dataHome, project, tc.prod, tc.staging, true)

			dir := t.TempDir()
			calls := filepath.Join(dir, "calls")
			started := filepath.Join(dir, "started")
			release := filepath.Join(dir, "release")
			harness := gatedReflectHarness(t, calls, started, release, []string{
				"merge " + prod + "," + staging + " -> " + scopeGateMergeText,
				"keep " + other,
			})
			t.Setenv("GHOST_CLI_CLAUDE_BINARY", harness)
			t.Setenv("GHOST_SCRATCH_DIR", t.TempDir())

			report := captureStderr(t, func() {
				done := make(chan struct{})
				go func() {
					defer close(done)
					runReflectArgs(t, "reflect", project, "--tier", "cli", "--apply")
				}()
				waitForFile(t, started)
				db, err := openRawDB(dataHome)
				if err != nil {
					t.Errorf("open to edit: %v", err)
				} else {
					if _, err := db.Exec(`UPDATE memories SET content = ?, updated_at = datetime('now') WHERE id = ?`,
						scopeUnrelatedRow+" and on a restart", other); err != nil {
						t.Errorf("edit the kept row: %v", err)
					}
					_ = db.Close()
				}
				if err := os.WriteFile(release, nil, 0o600); err != nil {
					t.Errorf("release the harness: %v", err)
				}
				<-done
			})
			t.Logf("report:\n%s", report)

			if !strings.Contains(report, "1 emission(s) claiming a row that changed or was deleted were dropped; not recording the skip fingerprint so the next run re-merges them") {
				t.Errorf("the concurrent edit is not reported in its own words:\n%s", report)
			}
			if got := readReflectSignature(t, dataHome); got != "" {
				t.Errorf("a skip fingerprint was recorded over a round whose claimed row changed: %q", got)
			}
			if got := strings.Contains(report, "refused because their sources' scopes disagree"); got != tc.scopeRefusal {
				t.Errorf("scope-refusal note present = %v, want %v:\n%s", got, tc.scopeRefusal, report)
			}
			// Nothing was written in the mixed round, and its summary counts each
			// reason rather than calling every proposal a claim on a moved row.
			if tc.scopeRefusal && !strings.Contains(report, "every consolidation proposal was refused: 1 merged memories whose scopes disagree, 1 claimed a memory that changed or was deleted") {
				t.Errorf("the mixed round's summary does not count both reasons:\n%s", report)
			}
		})
	}
}

// gatedReflectHarness is fakeReflectHarness with a pause in the turn: it
// touches started, waits (bounded) for release to exist, and only then answers
// with ops. That is the window a concurrent edit lands in.
func gatedReflectHarness(t *testing.T, calls, started, release string, ops []string) string {
	t.Helper()
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
		"while [ ! -f " + release + " ] && [ $i -lt 600 ]; do sleep 0.05; i=$((i+1)); done\n" +
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

// TestRunReflectGuardReAddAfterAScopeRefusalStoresNoCopy drives the drop guard's
// re-add through the command. The merge's text carries the staging row and not
// the production one, so the guard hands the production row back verbatim; the
// store refuses the merge because the two scopes disagree. The re-add used to be
// INSERTed beside the kept row as a fresh row with no scope — a production-only
// fact shown in every environment — and with the refusal no longer holding the
// project open, the fingerprint would then freeze that copy in place. After the
// apply the project must hold exactly the rows it had, each under its own scope.
func TestRunReflectGuardReAddAfterAScopeRefusalStoresNoCopy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake harness requires a POSIX shell")
	}
	dataHome := isolatedLifecycleEnv(t)
	ctx := context.Background()
	const project = "scopereadd"
	const (
		prodRow    = "the bastion firmware rollback runs from a laptop with a serial cable"
		stagingRow = "the relay listens behind the staging load balancer"
		mergeText  = "the relay listens behind the staging load balancer, always"
	)
	if err := os.MkdirAll(filepath.Join(dataHome, "ghost"), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	s := openReflectStore(t, dataHome, project)
	prod, err := s.Create(ctx, project, memory.Memory{Category: "fact", Source: "mcp", Importance: 0.6, Content: prodRow, Scope: map[string]string{"environment": "production"}})
	if err != nil {
		t.Fatalf("create prod row: %v", err)
	}
	staging, err := s.Create(ctx, project, memory.Memory{Category: "fact", Source: "mcp", Importance: 0.6, Content: stagingRow, Scope: map[string]string{"environment": "staging"}})
	if err != nil {
		t.Fatalf("create staging row: %v", err)
	}
	other, err := s.Create(ctx, project, memory.Memory{Category: "fact", Source: "mcp", Importance: 0.5, Content: scopeUnrelatedRow})
	if err != nil {
		t.Fatalf("create unrelated row: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
	if err := execBackdate(dataHome, project); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	calls := filepath.Join(t.TempDir(), "calls")
	harness := fakeReflectHarness(t, calls, prod+","+staging, mergeText, other)
	t.Setenv("GHOST_CLI_CLAUDE_BINARY", harness)
	t.Setenv("GHOST_SCRATCH_DIR", t.TempDir())

	report := captureStderr(t, func() {
		runReflectArgs(t, "reflect", project, "--tier", "cli", "--apply", "--skip-unchanged")
	})
	t.Logf("report:\n%s", report)
	// The precondition: the guard really re-added the production row.
	if !strings.Contains(report, "retained 1 verbatim") {
		t.Fatalf("the drop guard did not re-add the production row, so this round is not the case under test:\n%s", report)
	}
	if !strings.Contains(report, "refused because their sources' scopes disagree") {
		t.Errorf("the merge was not refused over its scopes:\n%s", report)
	}

	db, err := openRawDB(dataHome)
	if err != nil {
		t.Fatalf("open for read: %v", err)
	}
	rows, err := db.Query(`SELECT id, content, COALESCE(scope, '') FROM memories WHERE project_id = ?`, project)
	if err != nil {
		t.Fatalf("read rows: %v", err)
	}
	want := map[string]string{prod: `{"environment":"production"}`, staging: `{"environment":"staging"}`, other: ""}
	n := 0
	for rows.Next() {
		var id, content, scope string
		if err := rows.Scan(&id, &content, &scope); err != nil {
			t.Fatalf("scan: %v", err)
		}
		n++
		if w, ok := want[id]; !ok {
			t.Errorf("a new row %s (scope %q) holds %q: the re-add inserted a copy", id, scope, content)
		} else if scope != w {
			t.Errorf("row %s scope = %q, want %q", id, scope, w)
		}
	}
	_ = rows.Close()
	_ = db.Close()
	if n != len(want) {
		t.Errorf("the project holds %d rows, want %d", n, len(want))
	}

	if got := readReflectSignature(t, dataHome); got == "" {
		t.Fatal("no skip fingerprint was recorded")
	}
	second := captureStderr(t, func() {
		runReflectArgs(t, "reflect", project, "--tier", "cli", "--apply", "--skip-unchanged")
	})
	if !strings.Contains(second, "unchanged since the last applied consolidation") {
		t.Errorf("the second --skip-unchanged run did not skip: %s", second)
	}
	if n := countLines(t, calls); n != 1 {
		t.Errorf("the harness answered %d rounds, want 1", n)
	}
}
