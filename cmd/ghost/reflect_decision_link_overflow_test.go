package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// The overflow-only round: the ONE emission the harness answers with is the
// merge that cannot be written.
//
// The corpus is two rows rather than thirteen because both rows have to carry
// links already — that is what makes one merge over them overflow — and because
// a two-row corpus is under gateMinInput (6), so the LLM tier's quality gate
// never demands a second memory back and the round really is a single emission.
// With every emission refused and nothing deleted, `applied` is false, which is
// the branch the test above cannot reach: it seeds keeps, so there the round
// applied something and both the fingerprint guard and the "Applied: nothing"
// reason line sat unreachable.
const (
	// soloMergeLinks links, split evenly across the two rows. Each link is
	// "decision:" plus a 32-character id plus a separator, so seven of them fit
	// one source_ref (293 bytes) and all fourteen joined do not (587). Both
	// properties are load-bearing: a row whose own reference overflowed could
	// not exist, so the overflow has to arrive only through the merge.
	soloMergeLinks = 14
	// The three texts share a vocabulary on purpose, for the reason in
	// mergeFoldText above: the drop guard scores each merged source against its
	// OWN merge's tokens and re-adds any source the merge does not account for.
	// A re-added source is a verbatim emission claiming a row the overflow drop
	// had already marked kept, so it would be refused as a concurrent claim and
	// this round would measure the corpus guard instead of the overflow.
	soloMergeConsumerRow = "queue topic: the option we chose for the consumer is the measured one we recorded"
	soloMergeRetryRow    = "queue topic: the option we chose for the retry path is the measured one we recorded"
	soloMergeText        = "the queue topic, the option we chose, and the measured one we recorded for the consumer and for the retry path"
)

// decisionLinkPrefix is the reserved link a decision companion carries. The
// store's constant is unexported; this is the documented public format (docs/
// mcp.md), and it is spelled out here rather than imported so the assertion
// still holds if the store ever renames the identifier.
const decisionLinkPrefix = "decision:"

// fakeReflectHarness writes an executable standing in for `claude`, which
// answers two different invocations: the `--help` capability probe
// (ai.claudeCapabilitiesFor reads the installed binary's own flags, and a probe
// that fails is an error, not a degraded run) and one reflect turn. It appends a
// line to calls for every turn it answers, so a test can count rounds rather
// than infer them from output.
//
// The operation list is one merge over the named ids and, when keepIDs is not
// empty, a keep for each of them — an ordinary round that happens to fold too
// many links, and a round whose only emission is that merge. `keep` takes
// exactly one id per line (parseOpLine refuses several), so each is its own
// operation.
//
// The capability list is the one ai's own fixture uses: a binary that lacks any
// of the five required flags is refused with an error rather than run with a
// silently weaker policy.
func fakeReflectHarness(t *testing.T, calls, mergeIDs, mergeText, keepIDs string) string {
	t.Helper()
	var keeps []string
	for _, id := range strings.Split(keepIDs, ",") {
		if id != "" {
			keeps = append(keeps, id)
		}
	}
	return fakeReflectHarnessOps(t, calls, []string{"merge " + mergeIDs + " -> " + mergeText}, keeps)
}

// countLines counts the rounds a fake harness answered.
func countLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // a path this test wrote
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read harness call log: %v", err)
	}
	return strings.Count(strings.TrimSpace(string(data)), "\n") + 1
}

// readReflectSignature reads the --skip-unchanged fingerprint, or "" when none
// is recorded.
func readReflectSignature(t *testing.T, dataHome string) string {
	t.Helper()
	db, err := openRawDB(dataHome)
	if err != nil {
		t.Fatalf("open for signature read: %v", err)
	}
	defer db.Close() //nolint:errcheck
	var sig sql.NullString
	if err := db.QueryRow(`SELECT reflect_input_sig FROM ghost_state`).Scan(&sig); err != nil {
		t.Fatalf("read signature: %v", err)
	}
	return sig.String
}

// fakeReflectHarnessOps is fakeReflectHarness with the operation list spelled
// out, for a round that needs operations beside the one merge and its keeps.
func fakeReflectHarnessOps(t *testing.T, calls string, ops, keepIDs []string) string {
	t.Helper()
	for _, id := range keepIDs {
		ops = append(ops, "keep "+id)
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
		"printf '%s' '" + string(payload) + "'\n"
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake harness: %v", err)
	}
	return path
}

// TestRunReflectCountsAnOverflowBesideAConcurrentEdit is the mix the summary's
// mixed-reasons line has to catch without a scope refusal in it: one merge
// overflows the decision-link bound and another proposal claims a row edited
// while the harness was answering. Nothing is written, and the Applied line
// counts both reasons. Falling through to the concurrent-edit line would tell
// the operator the next run re-merges everything, which is false for the
// overflowed merge: it overflows again on every run until it is split.
func TestRunReflectCountsAnOverflowBesideAConcurrentEdit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake harness requires a POSIX shell")
	}
	dataHome := isolatedLifecycleEnv(t)
	ctx := context.Background()
	const project = "vaultmix"
	if err := os.MkdirAll(filepath.Join(dataHome, "ghost"), 0o700); err != nil {
		t.Fatal(err)
	}
	s := openReflectStore(t, dataHome, project)
	var links, companionIDs []string
	for i := 0; i < soloMergeLinks; i++ {
		decision, companion, _, err := s.RecordDecision(ctx, project,
			fmt.Sprintf("Rotation %02d", i), "rotate the queue credential on a schedule",
			"the consumer drops in-flight work while a token is stale", nil, nil)
		if err != nil {
			t.Fatalf("RecordDecision %d: %v", i, err)
		}
		links = append(links, decisionLinkPrefix+decision)
		companionIDs = append(companionIDs, companion)
	}
	if n, err := s.SetResolved(ctx, companionIDs); err != nil || n != len(companionIDs) {
		t.Fatalf("SetResolved = %d, %v; want %d companions resolved so they leave the corpus", n, err, len(companionIDs))
	}
	var rows []string
	for _, content := range []string{soloMergeConsumerRow, soloMergeRetryRow} {
		id, err := s.Create(ctx, project, memory.Memory{Category: "fact", Source: "mcp", Importance: 0.6, Content: content})
		if err != nil {
			t.Fatalf("create row %q: %v", content, err)
		}
		rows = append(rows, id)
	}
	// The row the harness keeps and the test edits while the harness is
	// gated, so the keep is a claim on a row that changed.
	other, err := s.Create(ctx, project, memory.Memory{Category: "fact", Source: "mcp", Importance: 0.5, Content: scopeUnrelatedRow})
	if err != nil {
		t.Fatalf("create the kept row: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
	// Direct UPDATE for the links, for the reason the overflow-only test gives.
	db, err := openRawDB(dataHome)
	if err != nil {
		t.Fatalf("open to write links: %v", err)
	}
	half := soloMergeLinks / 2
	for i, id := range rows {
		lo, hi := 0, half
		if i > 0 {
			lo, hi = half, soloMergeLinks
		}
		if _, err := db.Exec(`UPDATE memories SET source_ref = ? WHERE id = ?`, strings.Join(links[lo:hi], ","), id); err != nil {
			t.Fatalf("write decision links onto %s: %v", id, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close link writer: %v", err)
	}
	if err := execBackdate(dataHome, project); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	started := filepath.Join(dir, "started")
	release := filepath.Join(dir, "release")
	harness := gatedReflectHarness(t, calls, started, release, []string{
		"merge " + strings.Join(rows, ",") + " -> " + soloMergeText,
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
		select {
		case <-done:
			t.Errorf("the reflect round finished before the edit landed, so the edit was not concurrent")
		default:
		}
		if err := os.WriteFile(release, nil, 0o600); err != nil {
			t.Errorf("release the harness: %v", err)
		}
		<-done
	})
	t.Logf("report:\n%s", report)

	// Both stderr notes are the precondition: the round really did hit both
	// reasons, so the Applied line below is judged on a true mix.
	if !strings.Contains(report, "1 emission(s) stood for") {
		t.Fatalf("the round did not overflow, so it is not the mix under test:\n%s", report)
	}
	if !strings.Contains(report, "1 emission(s) claiming a row that changed or was deleted were dropped") {
		t.Fatalf("the round did not drop a claim on an edited row, so it is not the mix under test:\n%s", report)
	}
	want := "Applied: nothing — every consolidation proposal was refused: 0 merged memories whose scopes disagree, 1 claimed a memory that changed or was deleted while consolidation ran, 1 folded more decision links than one source_ref can hold"
	if !strings.Contains(report, want) {
		t.Errorf("the summary does not count both reasons; want %q in:\n%s", want, report)
	}
	if strings.Contains(report, "every consolidation proposal claimed a memory that changed or was deleted while consolidation ran, so none could be written") {
		t.Errorf("the overflowed merge is folded into the concurrent-edit line:\n%s", report)
	}
	// The concurrent edit still withholds the fingerprint: the edited row
	// is waiting to be consolidated.
	if got := readReflectSignature(t, dataHome); got != "" {
		t.Errorf("a skip fingerprint was recorded over a round whose claimed row changed: %q", got)
	}
}
