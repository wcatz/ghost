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
	dataHome = isolatedLifecycleEnv(t)
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(dataHome, "ghost"), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	s := openReflectStore(t, dataHome, project)
	ids := make([]string, 0, len(contents))
	for _, content := range contents {
		id, err := s.Create(ctx, project, memory.Memory{
			Category: "fact", Source: "mcp", Importance: 0.6, Content: content,
		})
		if err != nil {
			t.Fatalf("seed %q: %v", content, err)
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
	var out, errOut strings.Builder
	captureOutput(t, &out, &errOut, func() {
		runReflectArgs(t, "reflect", project, "--tier", "cli", "--apply")
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
