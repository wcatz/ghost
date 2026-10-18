package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/reflection"
)

// A merge's same-text twin: an input row the merge does NOT name, whose content
// is byte-identical to the merge's text. The join between the operation list and
// the emitted memories is keyed on text, so the twin's own emission (a keep, or
// the pass-through of a row the response never mentioned) used to inherit the
// merge's sources as its ReplacesIDs. The replace then read that verbatim
// emission as a claim on the merge's sources — which the overflowing merge had
// just kept — and dropped it as a claim on a row that changed while
// consolidation ran. Nothing changed: the round printed the concurrent-edit
// line and withheld the skip fingerprint, so every later --skip-unchanged run
// paid for the same work again.
//
// The fixture is the overflow-only corpus (two rows whose decision links do not
// fit one source_ref once merged) plus the twin.

// seedMergeTwinCorpus seeds the two link-carrying rows and the twin, backdated
// so no seeded row reads as a save that raced the round. It returns the merge's
// two sources and the twin's id.
func seedMergeTwinCorpus(t *testing.T, dataHome, project string) (a, b, twin string) {
	t.Helper()
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(dataHome, "ghost"), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
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
	var ids []string
	for _, content := range []string{soloMergeConsumerRow, soloMergeRetryRow, soloMergeText} {
		id, err := s.Create(ctx, project, memory.Memory{
			Category: "fact", Source: "mcp", Importance: 0.6, Content: content,
		})
		if err != nil {
			t.Fatalf("create row %q: %v", content, err)
		}
		ids = append(ids, id)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
	// Direct UPDATE for the reason the overflow-only test gives: a row carrying
	// several decision links is unreachable through a writer.
	db, err := openRawDB(dataHome)
	if err != nil {
		t.Fatalf("open to write links: %v", err)
	}
	half := soloMergeLinks / 2
	for i, id := range ids[:2] {
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
	return ids[0], ids[1], ids[2]
}

// liveWithContent counts the project's live rows carrying content.
func liveWithContent(t *testing.T, dataHome, project, content string) int {
	t.Helper()
	db, err := openRawDB(dataHome)
	if err != nil {
		t.Fatalf("open for read: %v", err)
	}
	defer db.Close() //nolint:errcheck
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memories WHERE project_id = ? AND content = ? AND resolved_at IS NULL`, project, content).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

// TestRunReflectMergeTwinIsNotAConcurrentEdit: the twin is kept (by an explicit
// keep, or as a row the response never named), nothing moved, and the round
// must say so — no concurrent-edit line, every row still live exactly once, and
// the skip fingerprint recorded so the identical overflow is not proposed again.
func TestRunReflectMergeTwinIsNotAConcurrentEdit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake harness requires a POSIX shell")
	}
	// The order of the operations is part of the case: an explicit keep listed
	// BEFORE the merge puts the twin's emission ahead of the merge's, so no rule
	// keyed on position ("the first emission with this text is the merge's")
	// could tell the two apart.
	merge := func(a, b string) string { return "merge " + a + "," + b + " -> " + soloMergeText }
	for _, tc := range []struct {
		name string
		ops  func(a, b, twin string) []string
	}{
		{name: "kept by the response", ops: func(a, b, twin string) []string { return []string{merge(a, b), "keep " + twin} }},
		{name: "kept before the merge", ops: func(a, b, twin string) []string { return []string{"keep " + twin, merge(a, b)} }},
		{name: "passed through unnamed", ops: func(a, b, _ string) []string { return []string{merge(a, b)} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataHome := isolatedLifecycleEnv(t)
			const project = "vaulttwin"
			a, b, twin := seedMergeTwinCorpus(t, dataHome, project)

			// The gated harness for its free operation order, with the gate
			// open from the start: nothing is edited during this round.
			dir := t.TempDir()
			calls := filepath.Join(dir, "calls")
			release := filepath.Join(dir, "release")
			if err := os.WriteFile(release, nil, 0o600); err != nil {
				t.Fatalf("open the harness gate: %v", err)
			}
			harness := gatedReflectHarness(t, calls, filepath.Join(dir, "started"), release, tc.ops(a, b, twin))
			t.Setenv("GHOST_CLI_CLAUDE_BINARY", harness)
			t.Setenv("GHOST_SCRATCH_DIR", t.TempDir())

			report := captureStderr(t, func() {
				runReflectArgs(t, "reflect", project, "--tier", "cli", "--apply")
			})
			t.Logf("report:\n%s", report)

			if strings.Contains(report, "changed or was deleted") {
				t.Errorf("the merge's same-text twin is reported as a claim on a row that moved:\n%s", report)
			}
			// The twin's own keep is what the round wrote: it landed on its row.
			if !strings.Contains(report, "Applied: 1 memories consolidated (1 fact)") {
				t.Errorf("the round does not report the twin's keep as the one memory it consolidated:\n%s", report)
			}
			if !strings.Contains(report, fmt.Sprintf("%d decision link", soloMergeLinks)) {
				t.Errorf("the overflowing merge is not reported as an overflow:\n%s", report)
			}
			if got := readReflectSignature(t, dataHome); got == "" {
				t.Errorf("no skip fingerprint was recorded over a round in which no row moved")
			}
			// The twin lands on its own row as a keep: still exactly one copy,
			// and the merge's sources are kept by the overflow.
			if n := liveWithContent(t, dataHome, project, soloMergeText); n != 1 {
				t.Errorf("%d live row(s) carry the twin's text, want exactly 1", n)
			}
			for _, content := range []string{soloMergeConsumerRow, soloMergeRetryRow} {
				if n := liveWithContent(t, dataHome, project, content); n != 1 {
					t.Errorf("%d live row(s) carry merge source %q, want 1", n, content)
				}
			}
			var live sql.NullString
			db, err := openRawDB(dataHome)
			if err != nil {
				t.Fatalf("open for read: %v", err)
			}
			defer db.Close() //nolint:errcheck
			if err := db.QueryRow(`SELECT id FROM memories WHERE id = ? AND resolved_at IS NULL`, twin).Scan(&live); err != nil {
				t.Errorf("the twin %s is not live under its own id: %v", twin, err)
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
		})
	}
}

// TestRunReflectMergeTwinRealConcurrentEditIsStillDetected is the control: when
// a row in the same fixture really is edited while the harness is answering —
// a merge source, or the twin itself — the round is still the corpus guard's
// drop, in its own words, and the fingerprint is withheld.
func TestRunReflectMergeTwinRealConcurrentEditIsStillDetected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake harness requires a POSIX shell")
	}
	for _, tc := range []struct {
		name   string
		target func(a, b, twin string) (id, content string)
	}{
		{name: "a merge source edited", target: func(a, _, _ string) (string, string) { return a, soloMergeConsumerRow + " and on a restart" }},
		{name: "the twin edited", target: func(_, _, twin string) (string, string) { return twin, soloMergeText + " and on a restart" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataHome := isolatedLifecycleEnv(t)
			const project = "vaulttwinedit"
			a, b, twin := seedMergeTwinCorpus(t, dataHome, project)

			dir := t.TempDir()
			calls := filepath.Join(dir, "calls")
			started := filepath.Join(dir, "started")
			release := filepath.Join(dir, "release")
			harness := gatedReflectHarness(t, calls, started, release, []string{
				"merge " + a + "," + b + " -> " + soloMergeText,
				"keep " + twin,
			})
			t.Setenv("GHOST_CLI_CLAUDE_BINARY", harness)
			t.Setenv("GHOST_SCRATCH_DIR", t.TempDir())

			editID, editContent := tc.target(a, b, twin)
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
					if _, err := db.Exec(`UPDATE memories SET content = ?, updated_at = datetime('now') WHERE id = ?`, editContent, editID); err != nil {
						t.Errorf("edit row: %v", err)
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

			// The exact line the round printed before the twin fix: the merge's
			// claim and the twin's keep are both void, so every proposal is a
			// claim on a moved row, and the edit is left as it is.
			if !strings.Contains(report, "Applied: nothing — every consolidation proposal claimed a memory that changed or was deleted while consolidation ran, so none could be written") {
				t.Errorf("a real concurrent edit is not reported as one:\n%s", report)
			}
			if got := readReflectSignature(t, dataHome); got != "" {
				t.Errorf("a skip fingerprint was recorded over a round whose claimed row changed: %q", got)
			}
			if n := liveWithContent(t, dataHome, project, editContent); n != 1 {
				t.Errorf("%d live row(s) carry the concurrent edit, want 1: the edit must survive", n)
			}
		})
	}
}

// TestReplacedIDsSkipAnEmissionThatRestatesARow pins the join itself: the twin's
// emission restates its own row and takes none of the merge's ids, wherever it
// sits in the emitted list, while the merge's survivor keeps both.
func TestReplacedIDsSkipAnEmissionThatRestatesARow(t *testing.T) {
	const text = "one consolidated statement of the rule"
	merge := reflection.ReflectMemory{Category: "fact", Content: text}
	twin := reflection.ReflectMemory{Category: "fact", Content: text, RestatesID: "x1"}
	result := &reflection.ReflectionResult{
		Merges: []reflection.Merge{{IDs: []string{"m1", "m2"}, Text: text}},
	}
	for _, order := range [][]reflection.ReflectMemory{{merge, twin}, {twin, merge}} {
		rows := reflectMemoriesToMemory("p1", order, replacedIDsByText(result))
		for i, row := range rows {
			if order[i].RestatesID != "" {
				if len(row.ReplacesIDs) != 0 {
					t.Errorf("the twin's row replaces %v, want nothing: it restates its own row", row.ReplacesIDs)
				}
				continue
			}
			if strings.Join(row.ReplacesIDs, ",") != "m1,m2" {
				t.Errorf("the merge's row replaces %v, want [m1 m2]", row.ReplacesIDs)
			}
		}
	}
}
