package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
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

// TestRunReflectSaysAnOverflowOnlyRoundAppliedNothingAndStillSkips is the round
// the test above cannot stage: the overflowing merge is the only thing the
// harness proposed, so nothing was written and nothing was deleted, and the run
// has to say so — in its OWN words, naming the links and the bound rather than a
// credential or a row that moved — while still recording the skip fingerprint
// that keeps the identical merge from being proposed again.
//
// The fingerprint is the whole point and the note is the other half. Reporting
// the right reason while suppressing the fingerprint would re-propose the merge
// on every run with a report nobody can act on; suppressing the note would make
// a skip the operator cannot explain.
func TestRunReflectSaysAnOverflowOnlyRoundAppliedNothingAndStillSkips(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake harness requires a POSIX shell")
	}
	dataHome := isolatedLifecycleEnv(t)
	ctx := context.Background()
	const project = "vault"
	if err := os.MkdirAll(filepath.Join(dataHome, "ghost"), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}

	// Fourteen decisions, so every link in the fixture names a live decision
	// instead of a made-up id. Their companion memories are then resolved: a
	// resolved row is excluded from the consolidation input (#318) and from the
	// replace's candidate query, which leaves the corpus as exactly the two rows
	// under test.
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
		id, err := s.Create(ctx, project, memory.Memory{
			Category: "fact", Source: "mcp", Importance: 0.6, Content: content,
		})
		if err != nil {
			t.Fatalf("create row %q: %v", content, err)
		}
		rows = append(rows, id)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
	// The links go in with a direct UPDATE because this state is unreachable
	// through a writer: checkReservedSourceRef refuses a decision link anywhere
	// in a caller-supplied source_ref, which is the whole point of the
	// namespace. A row carrying several links only ever exists as what a
	// consolidation left behind, and a test cannot run one consolidation to
	// produce the input of another — the same reason the credential test seeds
	// through CreateFromCorpus.
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
	// An hour ago, so no seeded row reads as a save that raced this run.
	if err := execBackdate(dataHome, project); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	calls := filepath.Join(t.TempDir(), "calls")
	harness := fakeReflectHarness(t, calls, strings.Join(rows, ","), soloMergeText, "")
	t.Setenv("GHOST_CLI_CLAUDE_BINARY", harness)
	t.Setenv("GHOST_SCRATCH_DIR", t.TempDir())

	report := captureStderr(t, func() {
		runReflectArgs(t, "reflect", project, "--tier", "cli", "--apply")
	})

	// The report's own line for a round that applied nothing has to name the
	// links and the bound it did not fit — the reason, with the count and the
	// bound taken from the store's own answer rather than restated.
	if !strings.Contains(report, "every consolidation proposal folded more decision links than one source_ref can hold") {
		t.Errorf("an overflow-only round did not report the overflow as its own reason:\n%s", report)
	}
	if !strings.Contains(report, fmt.Sprintf("(%d link(s), %d-byte bound)", soloMergeLinks, memory.MaxSourceRefLen)) {
		t.Errorf("the report does not carry the link count and the bound the store refused (%d links, %d-byte bound):\n%s", soloMergeLinks, memory.MaxSourceRefLen, report)
	}
	// Neither of the two reasons it is not: a credential this round never held,
	// and a row that never moved.
	if strings.Contains(report, "held a credential value") {
		t.Errorf("an overflow drop is reported as a credential drop:\n%s", report)
	}
	if strings.Contains(report, "changed or was deleted") {
		t.Errorf("an overflow drop is reported as a claim on a row that moved:\n%s", report)
	}

	// Both rows are still live and still carry every link. The drop keeps its
	// sources rather than truncating a link list, and a truncated list is the
	// failure #1213 exists to prevent: it would retire nothing and say nothing.
	live, err := openRawDB(dataHome)
	if err != nil {
		t.Fatalf("open for read: %v", err)
	}
	defer live.Close() //nolint:errcheck
	for _, id := range rows {
		var ref sql.NullString
		if err := live.QueryRow(`SELECT source_ref FROM memories WHERE id = ? AND resolved_at IS NULL`, id).Scan(&ref); err != nil {
			t.Fatalf("row %s is not live any more: %v", id, err)
		}
		if got := strings.Count(ref.String, decisionLinkPrefix); got != soloMergeLinks/2 {
			t.Errorf("row %s carries %d decision link(s), want %d: the drop must keep the whole list", id, got, soloMergeLinks/2)
		}
	}

	if got := readReflectSignature(t, dataHome); got == "" {
		t.Fatal("no skip fingerprint was recorded: --skip-unchanged would re-propose this same merge on every run")
	}
	if strings.Contains(report, "no skip fingerprint is recorded") {
		t.Errorf("the round reported itself as one that wrote nothing, and yet recorded its fingerprint:\n%s", report)
	}

	// And the second round must not reach the harness: the identical merge would
	// overflow again, so paying for it twice is the cost this round avoids.
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

// TestRunReflectDryRunListsAnOverflowingMergeAsRefused is the dry run of the
// corpus above. The accounting section is printed before the write and is meant
// to preview the apply exactly, so a merge the apply drops for folding more
// decision links than one source_ref can hold has to be listed as refused, with
// the link count and the bound, and not under Merges. The numbers are then held
// against what the apply itself reports for the same corpus.
func TestRunReflectDryRunListsAnOverflowingMergeAsRefused(t *testing.T) {
	assertOverflowDryRunMatchesApply(t, false)
}

// TestRunReflectDryRunListsAnOverflowingMergesTextTwinAsKept adds one fact the
// merge does not name whose text is already the merge's text, and the harness
// drops it. The replace keeps that row when it drops the merge, because a row
// carrying an emission's text is one of its claims, so the dry run has to list
// it on the refusal line rather than as a drop the apply does not make.
func TestRunReflectDryRunListsAnOverflowingMergesTextTwinAsKept(t *testing.T) {
	assertOverflowDryRunMatchesApply(t, true)
}

func assertOverflowDryRunMatchesApply(t *testing.T, twin bool) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake harness requires a POSIX shell")
	}
	dataHome := isolatedLifecycleEnv(t)
	ctx := context.Background()
	const project = "queue"
	if err := os.MkdirAll(filepath.Join(dataHome, "ghost"), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
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
	var twinID string
	if twin {
		id, err := s.Create(ctx, project, memory.Memory{
			Category: "fact", Source: "mcp", Importance: 0.6, Content: mergeFoldText,
		})
		if err != nil {
			t.Fatalf("create twin: %v", err)
		}
		twinID = id
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
	if err := execBackdate(dataHome, project); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	calls := filepath.Join(t.TempDir(), "calls")
	harness := fakeReflectHarness(t, calls, strings.Join(companions, ","), mergeFoldText, strings.Join(facts, ","))
	if twin {
		harness = fakeReflectHarnessOps(t, calls, []string{
			"merge " + strings.Join(companions, ",") + " -> " + mergeFoldText,
			"drop " + twinID + " reason: obsolete",
		}, facts)
	}
	t.Setenv("GHOST_CLI_CLAUDE_BINARY", harness)
	t.Setenv("GHOST_SCRATCH_DIR", t.TempDir())
	inputs := len(companions) + len(facts)
	owned := len(companions)
	if twin {
		inputs++
		owned++
	}

	// captureStderr takes both streams: the section is on stdout, the notes on
	// stderr.
	dry := captureStderr(t, func() { runReflectArgs(t, "reflect", project, "--tier", "cli") })
	const header = "Refused because the merge folds more decision links than one source_ref can hold ("
	if got := countReported(t, dry, header); got != owned {
		t.Errorf("%s%d), want %d:\n%s", header, got, owned, dry)
	}
	if !strings.Contains(dry, "Merges (0):") {
		t.Errorf("the dry run still lists the overflowing merge as a merge:\n%s", dry)
	}
	linkLine := regexp.MustCompile(`the merge folds (\d+) decision links, whose joined source_ref would exceed the (\d+)-byte bound; the sources are kept unchanged`)
	var refusal string
	for _, line := range strings.Split(dry, "\n") {
		if strings.HasPrefix(line, "  ") && linkLine.MatchString(line) {
			refusal = line
		}
	}
	if refusal == "" {
		t.Fatalf("the dry run has no link-overflow refusal line:\n%s", dry)
	}
	for _, id := range companions {
		if !strings.Contains(refusal, id) {
			t.Errorf("the refusal line does not name %s: %q", id, refusal)
		}
	}
	if twin {
		if !strings.Contains(refusal, ", and so is "+twinID+", which already carries the merge's text") {
			t.Errorf("the refusal line does not name the text twin %s the apply keeps: %q", twinID, refusal)
		}
		if strings.Count(dry, twinID) != 1 {
			t.Errorf("the twin %s is quoted %d times; the partition owns it once:\n%s", twinID, strings.Count(dry, twinID), dry)
		}
		if !strings.Contains(dry, "Dropped (0,") {
			t.Errorf("the dry run lists a drop of the twin, which the apply keeps:\n%s", dry)
		}
	}
	m := linkLine.FindStringSubmatch(refusal)
	if m[1] != strconv.Itoa(len(companions)) || m[2] != strconv.Itoa(memory.MaxSourceRefLen) {
		t.Errorf("the refusal line says %s links and a %s-byte bound, want %d and %d: %q", m[1], m[2], len(companions), memory.MaxSourceRefLen, refusal)
	}
	// The section still adds up to the input total with the new bucket in it.
	sum := countReported(t, dry, "Kept verbatim: ") + countReported(t, dry, "Passed through (not named): ")
	for _, h := range []string{"Merges (", "Refused by the grounding check (", header,
		"Refused because the sources' scopes disagree (", "Rewrites (", "Dropped (", "Deleted ("} {
		sum += countReported(t, dry, h)
	}
	if total := countReported(t, dry, "Inputs ("); sum != total || total != inputs {
		t.Errorf("the section's counts sum to %d over %d inputs (want %d):\n%s", sum, total, inputs, dry)
	}

	// The apply refuses the same merge for the same number of links.
	apply := captureStderr(t, func() { runReflectArgs(t, "reflect", project, "--tier", "cli", "--apply") })
	note := regexp.MustCompile(`(\d+) emission\(s\) stood for (\d+) decision link\(s\) whose joined source_ref would exceed the (\d+)-byte bound`).FindStringSubmatch(apply)
	if note == nil {
		t.Fatalf("the apply did not report the overflow drop:\n%s", apply)
	}
	if note[1] != "1" || note[2] != m[1] || note[3] != m[2] {
		t.Errorf("the apply reports %s emission(s), %s links, %s-byte bound; the dry run previewed 1, %s, %s", note[1], note[2], note[3], m[1], m[2])
	}
	if !strings.Contains(apply, refusal) {
		t.Errorf("the apply's own accounting does not carry the dry run's refusal line %q:\n%s", refusal, apply)
	}
	db, err := openRawDB(dataHome)
	if err != nil {
		t.Fatalf("open for read: %v", err)
	}
	defer db.Close() //nolint:errcheck
	kept := append([]string(nil), companions...)
	if twin {
		kept = append(kept, twinID)
	}
	for _, mem := range kept {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM memories WHERE id = ? AND resolved_at IS NULL`, mem).Scan(&n); err != nil || n != 1 {
			t.Errorf("%s is not live after the apply (%d, %v), but the dry run said it is kept", mem, n, err)
		}
	}
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
