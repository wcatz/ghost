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

// Issue #1228 at the command it names: `ghost reflect --apply`, a merge whose
// decision links alone do not fit in a source_ref.
//
// The store drops such a merge and keeps its sources (#1213). Before this fix
// that drop was reported as "an emission claiming a memory that changed or was
// deleted", which is a different event with a different consequence, and it
// suppressed the skip fingerprint — so the same merge was re-proposed and
// dropped on every later run, forever, while the note blamed a row that had not
// moved. Both halves are observable from the command, and both are asserted
// here: the note names the links and the bound, and the second --skip-unchanged
// run does not reach the harness at all.
//
// The harness is a fake executable rather than a real call: `cli.claude_binary`
// is configurable, the run is billed nothing, and the script can count its own
// invocations — which is what makes "not proposed again" a fact about the second
// run instead of a fact about the text it printed.
func TestRunReflectReportsAnOverflowingMergeAndDoesNotReproposeIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake harness requires a POSIX shell")
	}
	dataHome := isolatedLifecycleEnv(t)
	ctx := context.Background()
	const project = "queue"
	if err := os.MkdirAll(filepath.Join(dataHome, "ghost"), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}

	// Thirteen companions: each decision link is 41 bytes plus a separator, so
	// thirteen of them cannot fit in the 512-byte bound and twelve can. The
	// issue's regression condition, seeded rather than asserted by arithmetic so
	// a change to the id format cannot quietly move it.
	//
	// Nine ordinary facts sit beside them so the answer clears the LLM tier's
	// quality gate (a 22-row corpus needs seven memories back) and so the
	// overflowing merge is one refusal among writes rather than the whole
	// corpus. Kept as distinct texts: the sqlite tier is not involved, but two
	// identical ones would read as one fact to the drop audit.
	s := openReflectStore(t, dataHome, project)
	var companions []string
	for i := 0; i < 13; i++ {
		_, mem, _, err := s.RecordDecision(ctx, project,
			fmt.Sprintf("Queue topic %02d", i), fmt.Sprintf("we chose option %02d for the queue", i),
			"measured, and nothing cheaper did the job", nil, nil)
		if err != nil {
			t.Fatalf("RecordDecision %d: %v", i, err)
		}
		companions = append(companions, mem)
	}
	var facts []string
	for i := 0; i < 9; i++ {
		id, err := s.Create(ctx, project, memory.Memory{
			Category: "fact", Source: "mcp", Importance: 0.6,
			Content: fmt.Sprintf("runbook step %02d restarts the queue consumer in place", i),
		})
		if err != nil {
			t.Fatalf("create fact %d: %v", i, err)
		}
		facts = append(facts, id)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
	// An hour ago, so no seeded row reads as a save that raced this run.
	if err := execBackdate(dataHome, project); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	calls := filepath.Join(t.TempDir(), "calls")
	harness := fakeReflectHarness(t, calls, strings.Join(companions, ","),
		mergeFoldText, strings.Join(facts, ","))
	t.Setenv("GHOST_CLI_CLAUDE_BINARY", harness)
	t.Setenv("GHOST_SCRATCH_DIR", t.TempDir())

	report := captureStderr(t, func() {
		runReflectArgs(t, "reflect", project, "--tier", "cli", "--apply")
	})

	// The drop's own reason: how many decision links, and the bound they do not
	// fit. Asserted against the link count and the exported bound rather than a
	// sentence, so the numbers in the note cannot drift away from what the store
	// refused.
	if !strings.Contains(report, fmt.Sprintf("%d decision link", len(companions))) {
		t.Errorf("the report does not say how many decision links overflowed (%d):\n%s", len(companions), report)
	}
	if !strings.Contains(report, fmt.Sprintf("%d-byte bound", memory.MaxSourceRefLen)) {
		t.Errorf("the report does not name the source_ref bound (%d):\n%s", memory.MaxSourceRefLen, report)
	}
	// And NOT the corpus guard's reason: that is the misreport the issue names,
	// and it is what made a drop nobody can repair look like one a later run
	// would fix.
	if strings.Contains(report, "changed or was deleted") {
		t.Errorf("the overflow drop is still reported as a claim on a row that moved:\n%s", report)
	}

	// Every merged decision is still live and still linked to its own decision:
	// the drop keeps the sources rather than truncating the link list, and that
	// is the property a truncated list would have broken silently.
	db, err := openRawDB(dataHome)
	if err != nil {
		t.Fatalf("open for read: %v", err)
	}
	defer db.Close() //nolint:errcheck
	for _, mem := range companions {
		var ref sql.NullString
		if err := db.QueryRow(`SELECT source_ref FROM memories WHERE id = ? AND resolved_at IS NULL`, mem).Scan(&ref); err != nil {
			t.Fatalf("companion %s is not live any more: %v", mem, err)
		}
		if !strings.Contains(ref.String, decisionLinkPrefix) {
			t.Errorf("companion %s lost its decision link across the drop: source_ref = %q", mem, ref.String)
		}
	}

	if got := readReflectSignature(t, dataHome); got == "" {
		t.Fatal("no skip fingerprint was recorded: --skip-unchanged would re-propose this same merge on every run")
	}

	// The second round has to leave the harness alone. Re-proposing the merge
	// would produce the identical overflow, so a fingerprint that keeps
	// --skip-unchanged skipping is the whole mechanism — and the invocation
	// count is what distinguishes it from a run that consolidated and simply
	// dropped again.
	second := captureStderr(t, func() {
		runReflectArgs(t, "reflect", project, "--tier", "cli", "--apply", "--skip-unchanged")
	})
	if !strings.Contains(second, "unchanged since the last applied consolidation") {
		t.Errorf("the second --skip-unchanged run did not skip: %s", second)
	}
	if n := countLines(t, calls); n != 1 {
		t.Errorf("the harness answered %d rounds, want 1: the same merge was proposed again after an overflow drop that cannot resolve itself", n)
	}
}

// mergeFoldText is the replacement prose the fake harness merges the thirteen
// companions into. It restates their vocabulary rather than naming a new topic:
// the drop guard scores each merged source against its OWN merge's tokens and
// re-adds any source the merge does not account for, so a thin one-line merge
// would bring the thirteen back verbatim and the round would never reach the
// store. It states no path, commit, id or project, because a merge naming one
// would be refused for contamination before the decision links were ever read.
const mergeFoldText = "Each queue topic chose an option for it, and the measured rationale records why nothing cheaper did the job"

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
// The operation list is one merge over the decision companions and a keep for
// each other memory — the shape of an ordinary consolidation round that happens
// to fold too many links. `keep` takes exactly one id per line (parseOpLine
// refuses several), so each is its own operation.
//
// The capability list is the one ai's own fixture uses: a binary that lacks any
// of the five required flags is refused with an error rather than run with a
// silently weaker policy.
func fakeReflectHarness(t *testing.T, calls, mergeIDs, mergeText, keepIDs string) string {
	t.Helper()
	ops := []string{"merge " + mergeIDs + " -> " + mergeText}
	for _, id := range strings.Split(keepIDs, ",") {
		if id != "" {
			ops = append(ops, "keep "+id)
		}
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
