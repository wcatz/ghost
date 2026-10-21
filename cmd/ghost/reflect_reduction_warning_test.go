package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// The >50% reduction warning on a promotion-off apply, which counts the
// memories the project ends up HOLDING. The count used to be taken before the
// write, from the caller's pre-drop proposal lists, and the write drops
// proposals at its own boundary: a proposal whose content holds a token-shaped
// value is never written, so it is not a memory the project holds. The count
// therefore moved after it was taken, in both directions:
//
//   - a round whose third merge was dropped left 2 of 6 rows, and the warning
//     stayed silent because the pre-drop count was 3;
//   - a round whose every merge was dropped wrote nothing at all, and the
//     warning still fired, because the pre-drop count was 2.
//
// The first is the warning the unattended lifecycle path has no other report
// of; the second is a warning about a compression that did not happen.

// reductionToken is a token-shaped value in the GitHub PAT format, assembled
// rather than written out because GitHub push protection matches that format
// anywhere in a diff and rejects the push (GH013) before review starts.
//
// Letters only, and that is load-bearing rather than cosmetic: a DIGIT in the
// value is an identifier the grounding check refuses (a merge may not
// introduce a number its sources do not carry), and that refusal keeps the
// merge's sources instead of writing the merge — a different path, reached
// before the write boundary these tests are about.
func reductionToken() string { return "ghp_" + strings.Repeat("aZ", 18) }

// The three pairs, and the merge each pair becomes. Every merge restates the
// vocabulary of both its sources, because the drop guard scores a merge's
// sources against the text of THEIR OWN merge and re-adds any source the merge
// does not account for.
var (
	reduceBastionA = "the bastion accepts connections on port 2222"
	reduceBastionB = "the bastion terminates idle sessions on port 2222"
	reduceBastionM = "the bastion accepts connections and terminates idle sessions on port 2222"

	reduceQueueA = "the queue consumer drains the backlog in one pass"
	reduceQueueB = "the queue consumer retries a failed delivery"
	reduceQueueM = "the queue consumer drains the backlog in one pass and retries a failed delivery"

	reduceDeployA = "the deploy pipeline requires a manual approval gate"
	reduceDeployB = "the deploy pipeline refuses an unreviewed release"
	reduceDeployM = "the deploy pipeline requires a manual approval gate and refuses an unreviewed release, and its release token is " + reductionToken()
)

// reflectReductionRound seeds a project with the given rows, runs
// `ghost reflect --apply` against a fake harness answering with the operations
// ops builds from the seeded ids, and returns the two streams and the data
// directory.
func reflectReductionRound(t *testing.T, project string, contents []string, ops func(ids []string) []string) (dataHome, stdout, stderr string) {
	t.Helper()
	rows := make([]memory.Memory, len(contents))
	for i, content := range contents {
		rows[i] = memory.Memory{Category: "fact", Source: "mcp", Importance: 0.6, Content: content}
	}
	return reflectReductionRoundRows(t, project, rows, ops)
}

// reflectReductionRoundRows is the same round with the seeded rows given whole,
// so a row can carry a scope the write refuses to merge across.
func reflectReductionRoundRows(t *testing.T, project string, rows []memory.Memory, ops func(ids []string) []string, extraArgs ...string) (dataHome, stdout, stderr string) {
	t.Helper()
	dataHome = isolatedLifecycleEnv(t)
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(dataHome, "ghost"), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	s := openReflectStore(t, dataHome, project)
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		id, err := s.Create(ctx, project, row)
		if err != nil {
			t.Fatalf("seed %q: %v", row.Content, err)
		}
		ids = append(ids, id)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
	// Older than the round trip, so the replace does not read the seed as a
	// concurrent save and leave it alone.
	if err := execBackdateAges(dataHome, project); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	calls := filepath.Join(t.TempDir(), "calls")
	t.Setenv("GHOST_CLI_CLAUDE_BINARY", opsReflectHarness(t, calls, ops(ids)))
	t.Setenv("GHOST_SCRATCH_DIR", t.TempDir())
	args := append([]string{"reflect", project, "--tier", "cli", "--apply"}, extraArgs...)
	var out, errOut strings.Builder
	captureOutput(t, &out, &errOut, func() {
		runReflectArgs(t, args...)
	})
	if n := countLines(t, calls); n != 1 {
		t.Fatalf("the harness answered %d rounds, want 1", n)
	}
	return dataHome, out.String(), errOut.String()
}

// liveRowCount counts the project's live rows.
func liveRowCount(t *testing.T, dataHome, project string) int {
	t.Helper()
	got := readColumn(t, dataHome,
		`SELECT count(*) FROM memories WHERE project_id = ? AND resolved_at IS NULL`, project)
	n, err := strconv.Atoi(got)
	if err != nil {
		t.Fatalf("the live row count reads %q: %v", got, err)
	}
	return n
}

// TestRunReflectReportsTheReductionTheApplyMade: a merge the write boundary
// drops is not a memory the project holds, so the count has to be taken after
// the write and from the proposals that were written.
//
// Six live rows in three pairs, three merges proposed, and the third
// replacement holds a token-shaped value. Two of the three merges are written,
// so the project ends with 2 of 6 rows — a 67% reduction, over the half the
// warning names. The pre-drop count was 3, and 3 of 6 is exactly half, so the
// warning stayed silent on the one report an unattended apply has of a hard
// compression.
func TestRunReflectReportsTheReductionTheApplyMade(t *testing.T) {
	dataHome, stdout, stderr := reflectReductionRound(t, "reducez",
		[]string{reduceBastionA, reduceBastionB, reduceQueueA, reduceQueueB, reduceDeployA, reduceDeployB},
		func(ids []string) []string {
			return []string{
				"merge " + ids[0] + "," + ids[1] + " -> " + reduceBastionM,
				"merge " + ids[2] + "," + ids[3] + " -> " + reduceQueueM,
				"merge " + ids[4] + "," + ids[5] + " -> " + reduceDeployM,
			}
		})

	if !strings.Contains(stderr, "left 2 memories in the project vs 6") {
		t.Errorf("the project ended with 2 of 6 rows, so the reduction warning is the only report of it; stderr:\n%s", stderr)
	}
	if n := strings.Count(stderr, ">50% reduction"); n != 1 {
		t.Errorf("the reduction warning was printed %d times, want 1:\n%s", n, stderr)
	}
	// The write happened, and it is the write the count describes.
	if !strings.Contains(stdout, "Applied: 2 memories consolidated") {
		t.Errorf("the two merges that survived the drop were not reported as written; stdout:\n%s", stdout)
	}
	if got := liveRowCount(t, dataHome, "reducez"); got != 2 {
		t.Errorf("the project holds %d live rows, want the 2 the warning names", got)
	}
}

// TestRunReflectDoesNotReportAReductionItDidNotMake: a round that wrote
// nothing compressed nothing, so there is no reduction to report.
//
// Six live rows in two groups of three, and both merge replacements hold a
// token-shaped value, so both are dropped at the write boundary and the corpus
// is left exactly as it was. The pre-drop count was 2, and 2 of 6 reads as a
// 67% reduction, so the warning fired on a round that changed nothing — the
// same cry-wolf the missed warning is, pointing the other way.
func TestRunReflectDoesNotReportAReductionItDidNotMake(t *testing.T) {
	dataHome, stdout, stderr := reflectReductionRound(t, "reducez2",
		[]string{
			reduceBastionA, reduceBastionB, "the bastion rotates the operator keys every quarter",
			reduceQueueA, reduceQueueB, "the queue consumer alerts the on-call engineer",
		},
		func(ids []string) []string {
			return []string{
				"merge " + ids[0] + "," + ids[1] + "," + ids[2] + " -> " +
					"the bastion accepts connections on port 2222, terminates idle sessions on port 2222 and rotates the operator keys every quarter, and its operator token is " + reductionToken(),
				"merge " + ids[3] + "," + ids[4] + "," + ids[5] + " -> " +
					"the queue consumer drains the backlog in one pass, retries a failed delivery and alerts the on-call engineer, and its delivery token is " + reductionToken(),
			}
		})

	if strings.Contains(stderr, ">50% reduction") {
		t.Errorf("nothing was written, so there is no reduction to report; stderr:\n%s", stderr)
	}
	if !strings.Contains(stdout, "Applied: nothing") {
		t.Errorf("the round wrote nothing and had to say so; stdout:\n%s", stdout)
	}
	if got := liveRowCount(t, dataHome, "reducez2"); got != 6 {
		t.Errorf("the project holds %d live rows, want the 6 it held before the round", got)
	}
}

// TestRunReflectCountsAScopeRefusedMergeAsTheSourcesItLeavesLive: a merge the
// store refuses writes no row of its own, and it does not take its sources
// either — every one of them stays live under its own scope. Counting that
// proposal as one retained memory therefore understates the project by N-1
// rows, and a round that kept five of six announces a reduction that did not
// happen beside the refusal note saying the rows are still there.
//
// Four bastion rows are merged across disagreeing scopes and refused; the two
// queue rows merge into one. The project holds five rows, which is over half of
// six, so no reduction warning belongs on this round.
func TestRunReflectCountsAScopeRefusedMergeAsTheSourcesItLeavesLive(t *testing.T) {
	dataHome, stdout, stderr := reflectReductionRoundRows(t, "reducez3",
		[]memory.Memory{
			{Category: "fact", Source: "mcp", Importance: 0.6, Content: reduceBastionA,
				Scope: map[string]string{"environment": "production"}},
			{Category: "fact", Source: "mcp", Importance: 0.6, Content: reduceBastionB,
				Scope: map[string]string{"environment": "staging"}},
			{Category: "fact", Source: "mcp", Importance: 0.6, Content: "the bastion logs every handshake"},
			{Category: "fact", Source: "mcp", Importance: 0.6, Content: "the bastion rotates operator keys every quarter",
				Scope: map[string]string{"environment": "production"}},
			{Category: "fact", Source: "mcp", Importance: 0.6, Content: reduceQueueA},
			{Category: "fact", Source: "mcp", Importance: 0.6, Content: reduceQueueB},
		},
		func(ids []string) []string {
			return []string{
				"merge " + ids[0] + "," + ids[1] + "," + ids[2] + "," + ids[3] + " -> " +
					"the bastion accepts connections on port 2222, terminates idle sessions on port 2222, logs every handshake and rotates operator keys every quarter",
				"merge " + ids[4] + "," + ids[5] + " -> " + reduceQueueM,
			}
		})

	if strings.Contains(stderr, ">50% reduction") {
		t.Errorf("the project holds 5 of 6 rows, so there is no reduction to report; stderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "1 merge(s) were refused because their sources' scopes disagree") {
		t.Errorf("the refused merge and its four live sources were not reported; stderr:\n%s", stderr)
	}
	if !strings.Contains(stdout, "Applied: 1 memories consolidated") {
		t.Errorf("the queue merge was not reported as written; stdout:\n%s", stdout)
	}
	if got := liveRowCount(t, dataHome, "reducez3"); got != 5 {
		t.Errorf("the project holds %d live rows, want the 5: four refused sources and the one written merge", got)
	}
}

// TestRunReflectCountsAGlobalOnlyApplyAsTheRowsTheProjectStillHolds: an apply
// whose only proposals are cross-project candidates runs no project replace, so
// the store reports no project retention at all — a zero that means "no replace
// ran" and not "nothing was retained". The project still holds every row it
// held, and a count that read that zero as the retention would announce a
// compression that did not happen.
//
// Six rows, two merges of three rows each into cross-project candidates, applied
// with --promote-globals. Both candidates are written to _global and the project
// keeps its six rows, so there is no reduction to report.
func TestRunReflectCountsAGlobalOnlyApplyAsTheRowsTheProjectStillHolds(t *testing.T) {
	const project = "reduceglobal"
	const bastionMerge = "across all projects the bastion accepts connections on port 2222, " +
		"terminates idle sessions on port 2222 and logs every handshake"
	const queueMerge = "across all projects the queue consumer drains the backlog in one pass, " +
		"retries a failed delivery and alerts the on-call engineer"
	rows := make([]memory.Memory, 0, 6)
	for _, content := range []string{
		reduceBastionA, reduceBastionB, "the bastion logs every handshake",
		reduceQueueA, reduceQueueB, "the queue consumer alerts the on-call engineer",
	} {
		rows = append(rows, memory.Memory{Category: "fact", Source: "mcp", Importance: 0.6, Content: content})
	}
	dataHome, stdout, stderr := reflectReductionRoundRows(t, project, rows,
		func(ids []string) []string {
			return []string{
				"merge " + strings.Join(ids[:3], ",") + " -> " + bastionMerge,
				"merge " + strings.Join(ids[3:], ",") + " -> " + queueMerge,
			}
		},
		"--promote-globals")

	// Both proposals are cross-project candidates, so the round has no project
	// half: no project proposal, and the candidates are promoted rather than
	// folded into the project.
	if !strings.Contains(stdout, "Global-scoped (2)") {
		t.Errorf("the two candidates are not classified as global, so this is not the global-only path; stdout:\n%s", stdout)
	}
	if strings.Contains(stdout, "Project-scoped") {
		t.Errorf("a project-scoped proposal ran, so the project half is not empty; stdout:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Promoted 2/2 global memories") {
		t.Errorf("neither candidate was promoted, so this is not the global-only path; stdout:\n%s", stdout)
	}
	if strings.Contains(stderr, ">50% reduction") {
		t.Errorf("the project kept every row it held, so there is no reduction to report; stderr:\n%s", stderr)
	}
	if got := liveRowCount(t, dataHome, project); got != 6 {
		t.Errorf("the project holds %d live rows, want the 6 it held: a global-only apply consumes nothing", got)
	}
}
