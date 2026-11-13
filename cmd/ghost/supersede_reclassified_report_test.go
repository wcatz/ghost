package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/supersede"
)

// The ids are 32-character hex-shaped so shortID abbreviates them as every
// report does. reclassCauses* is a second pair for the 'causes' rows, so a
// follow-up that already lists the first pair cannot hide a missing exclusion.
const (
	reclassNewer        = "aaaaaaaabbbbccccddddeeeeffff0000"
	reclassOlder        = "11111111222233334444555566667777"
	reclassFresh        = "88888888777766665555444433332222"
	reclassCausesNewer  = "ccccccccddddeeeeffff0000aaaaaaaabbbb"
	reclassCausesTarget = "22222222333344445555666677778888"
)

// reclassRow is a row over a pair that carried a live 'supersedes' edge (#823:
// the report and follow-up branch on which relation the edge was).
func reclassRow(relation supersede.Relation, reclassified, withdrawn bool) supersede.Classified {
	return supersede.Classified{
		Candidate:        supersede.Candidate{NewerID: reclassNewer, OlderID: reclassOlder},
		Relation:         relation,
		Reclassified:     reclassified,
		ReclassifiedFrom: supersede.RelationSupersedes,
		Withdrawn:        withdrawn,
		TargetProjectID:  "proj",
	}
}

// opposedRow is a row whose write was refused because the pair's opposite
// direction was already live (#806).
func opposedRow(relation supersede.Relation) supersede.Classified {
	row := reclassRow(relation, relation == supersede.RelationCauses, false)
	row.OpposedLive = true
	return row
}

func withCauses(row supersede.Classified, dropped, droppable int) supersede.Classified {
	row.CausesDropped = dropped
	row.CausesDroppable = droppable
	return row
}

// suppressedRow marks a reclassified supersedes row as having its withdrawal
// withheld (#845); a 'causes'-edge row is swept for real and is left alone.
func suppressedRow(row supersede.Classified) supersede.Classified {
	if row.ReclassifiedFrom == supersede.RelationSupersedes {
		row.WithdrawSuppressed = true
		row.Withdrawn = false
	}
	return row
}

// causesRow is reclassRow for a pair whose live edge was 'causes'.
func causesRow(relation supersede.Relation, withdrawn bool) supersede.Classified {
	return supersede.Classified{
		Candidate:        supersede.Candidate{NewerID: reclassCausesNewer, OlderID: reclassCausesTarget},
		Relation:         relation,
		Reclassified:     true,
		ReclassifiedFrom: supersede.RelationCauses,
		Withdrawn:        withdrawn,
		TargetProjectID:  "proj",
	}
}

// TestSupersedePairLinesNamesTheEdgeAPassWouldWithdraw is #785 after #845: the
// ordinary pass names the edge a denying verdict would remove and declines to
// remove it, so the row never claims a deletion the run did not make.
func TestSupersedePairLinesNamesTheEdgeAPassWouldWithdraw(t *testing.T) {
	still := []string{"would withdraw", shortID(reclassNewer) + " -> " + shortID(reclassOlder), "neither", "STILL LIVE"}
	rows := []struct {
		name       string
		apply      bool
		classified []supersede.Classified
		want       []string
		notWant    []string
	}{
		{"a dry run says it would withdraw, and claims no write", false,
			[]supersede.Classified{suppressedRow(reclassRow(supersede.RelationNeither, true, false))},
			still, []string{"\nwithdrew", "\nalready gone"}},
		{"an applied run reports the same withheld withdrawal as a dry run", true,
			[]supersede.Classified{suppressedRow(reclassRow(supersede.RelationNeither, true, false))},
			still, []string{"withdrew", "already gone", "kept"}},
		{"a withheld row names the repair that does remove the edge", true,
			[]supersede.Classified{suppressedRow(reclassRow(supersede.RelationReversed, true, false))},
			[]string{"reversed", "reassess", "--withdraw"}, []string{"withdrew", "already gone"}},
		{"a causes verdict on a withheld supersedes edge names both", true,
			[]supersede.Classified{withCauses(suppressedRow(reclassRow(supersede.RelationCauses, true, false)), 1, 1)},
			[]string{"would withdraw", "STILL LIVE", "[+1 causes edge dropped]",
				"re-linked as " + shortID(reclassOlder) + " causes -> " + shortID(reclassNewer)},
			[]string{"withdrew", "already gone"}},
		{"a causes edge this run really removed says withdrew", true,
			[]supersede.Classified{withCauses(causesRow(supersede.RelationNeither, true), 1, 1)},
			[]string{"withdrew", "[+1 causes edge dropped]"}, []string{"would withdraw", "already gone", "STILL LIVE"}},
		{"an edge a concurrent pass took first is not claimed", true,
			[]supersede.Classified{withCauses(causesRow(supersede.RelationNeither, false), 0, 0)},
			[]string{"already gone", shortID(reclassCausesNewer) + " -> " + shortID(reclassCausesTarget)},
			[]string{"would withdraw", "\nwithdrew", "STILL LIVE"}},
		{"a fresh pair is still reported as the write it is", true,
			[]supersede.Classified{reclassRow(supersede.RelationSupersedes, false, false)},
			[]string{shortID(reclassNewer) + "  supersedes  " + shortID(reclassOlder)}, []string{"withdrew", "would withdraw", "->"}},
		{"a fresh reversal keeps the refusal line", true,
			[]supersede.Classified{reclassRow(supersede.RelationReversed, false, false)},
			[]string{"reversed, not written"}, []string{"withdrew", "would withdraw"}},
		{"a dry run names the deletion it would make, in the future tense", false,
			[]supersede.Classified{withCauses(suppressedRow(reclassRow(supersede.RelationNeither, true, false)), 0, 1)},
			[]string{"would withdraw", "[+1 causes edge would be dropped]", "STILL LIVE"}, []string{"[+1 causes edge dropped]"}},
		{"a pair the pass judged and did not write is not claimed", true,
			[]supersede.Classified{opposedRow(supersede.RelationSupersedes)},
			[]string{"not written", "the pair's reverse direction is already live", shortID(reclassNewer) + "  supersedes  " + shortID(reclassOlder)},
			[]string{"withdrew", "would withdraw", "already gone"}},
		{"a refused write is never dressed as a re-link or a withdrawal", true,
			[]supersede.Classified{opposedRow(supersede.RelationCauses)},
			[]string{"not written", "causes"}, []string{"re-linked", "withdrew", "would withdraw", "already gone"}},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			got := supersedePairLines(tc.apply, tc.classified)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("supersedePairLines() =\n%s\nwant it to contain %q", got, want)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(got, notWant) {
					t.Errorf("supersedePairLines() =\n%s\nwant it NOT to contain %q", got, notWant)
				}
			}
		})
	}
}

// TestAStaleWithheldWithdrawalSaysTheEdgeIsGoneBecauseItIs: a stale row (the
// edge was cascade-deleted by a concurrent pass) falls back to `already gone`
// and the summary carries no withheld line (#845).
func TestAStaleWithheldWithdrawalSaysTheEdgeIsGoneBecauseItIs(t *testing.T) {
	rows := supersedePairLines(true, []supersede.Classified{reclassRow(supersede.RelationNeither, true, false)})
	if !strings.Contains(rows, "already gone") || !strings.Contains(rows, shortID(reclassNewer)+" -> "+shortID(reclassOlder)) {
		t.Errorf("a stale pair's row does not say its edge is gone:\n%s", rows)
	}
	for _, notWant := range []string{"STILL LIVE", "would withdraw", "withdrew"} {
		if strings.Contains(rows, notWant) {
			t.Errorf("a stale pair's row claims %q:\n%s", notWant, rows)
		}
	}
	summary := supersedeReport("projy", supersede.Result{Reclassified: 1, StaleAtWrite: 1}, "linked", true, 1, 0)
	if !strings.Contains(summary, "1 pair(s) not written") {
		t.Errorf("the summary does not report the stale pair:\n%s", summary)
	}
	if strings.Contains(summary, "pair(s) withheld") || strings.Contains(summary, "STILL LIVE") {
		t.Errorf("the summary counts a stale pair as a withheld withdrawal:\n%s", summary)
	}
}

func TestSupersedePairLinesPrintsNothingForAnEmptyPass(t *testing.T) {
	if got := supersedePairLines(true, nil); got != "" {
		t.Errorf("supersedePairLines(true, nil) = %q, want the empty string", got)
	}
	unparsed := supersede.Classified{Candidate: supersede.Candidate{NewerID: reclassNewer, OlderID: reclassOlder}}
	if got := supersedePairLines(true, []supersede.Classified{unparsed}); got != "" {
		t.Errorf("an unparseable verdict printed %q, want nothing", got)
	}
}

// TestReclassifiedWithdrawalsNamesEveryOrphanedTarget: the repair follow-up is
// built over real withdrawals only. A withheld one (#845) leaves the edge live
// and nothing orphaned; a 'causes' edge never stamped a resolved_at.
func TestReclassifiedWithdrawalsNamesEveryOrphanedTarget(t *testing.T) {
	got := withdrawnTargets(reclassifiedWithdrawals([]supersede.Classified{
		suppressedRow(reclassRow(supersede.RelationNeither, true, false)),
		suppressedRow(reclassRow(supersede.RelationReversed, true, false)),
		suppressedRow(reclassRow(supersede.RelationCauses, true, false)),
		reclassRow(supersede.RelationSupersedes, true, false),
		reclassRow(supersede.RelationCauses, false, false),
		{Candidate: supersede.Candidate{NewerID: reclassFresh, OlderID: reclassOlder}, Relation: supersede.RelationNeither},
		causesRow(supersede.RelationNeither, true),
	}))
	if len(got) != 0 {
		t.Errorf("withdrawnTargets(reclassifiedWithdrawals(...)) = %+v, want nothing", got)
	}
	// A row that did orphan a resolved_at still reaches the scoped resolve repair.
	one := withdrawnTargets(reclassifiedWithdrawals([]supersede.Classified{reclassRow(supersede.RelationNeither, true, true)}))
	if len(one) != 1 {
		t.Fatalf("withdrawnTargets = %+v, want one project group", one)
	}
	block := supersedeReassessFollowup(one[0].ProjectID, one[0].Targets, "")
	if !strings.Contains(block, "ghost resolve proj --reassess --only '"+reclassOlder+"' --apply") {
		t.Errorf("the follow-up an ordinary pass prints is not the scoped resolve repair:\n%s", block)
	}
}

// useStubHarness installs an opencode stub that prints the given JSON-lines text.
func useStubHarness(t *testing.T, text string) {
	t.Helper()
	binDir := t.TempDir()
	harness := filepath.Join(binDir, "opencode")
	writeExecutable(t, harness, "#!/bin/sh\n"+`printf '{"type":"text","part":{"type":"text","text":"`+text+`"}}\n'`+"\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GHOST_CLI_OPENCODE_BINARY", harness)
}

// runSupersedeArgs runs `ghost supersede <args>` in process and returns stdout.
func runSupersedeArgs(t *testing.T, args ...string) string {
	t.Helper()
	origArgs := os.Args
	os.Args = append([]string{origArgs[0], "supersede"}, args...)
	t.Cleanup(func() { os.Args = origArgs })
	return captureStdout(t, runSupersede)
}

func seedDataDir(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(isolatedLifecycleEnv(t), "ghost", "ghost.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	return dbPath
}

func liveEdges(t *testing.T, dbPath string, rel supersede.Relation) int {
	t.Helper()
	links, err := openStore(t, dbPath).LinksByRelationSource(context.Background(), "projy", string(rel), "llm")
	if err != nil {
		t.Fatalf("LinksByRelationSource: %v", err)
	}
	return len(links)
}

// seedEdge writes a project with two notes and a live 'llm' edge a -> b of the
// given relation, then runs mutate with the closed store's path and a raw handle.
func seedEdge(t *testing.T, dbPath string, contents [2]string, rel supersede.Relation, mutate func(store *memory.Store, ids [2]string, raw func(q string, args ...any))) [2]string {
	t.Helper()
	ctx := context.Background()
	store := openStore(t, dbPath)
	if err := store.EnsureProject(ctx, "projy", "/tmp/projy", "projy"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	var ids [2]string
	for i, content := range contents {
		id, err := store.Create(ctx, "projy", memory.Memory{Category: "architecture", Content: content, Source: "mcp", Importance: 0.7})
		if err != nil {
			t.Fatalf("create memory: %v", err)
		}
		ids[i] = id
	}
	if err := store.CreateLink(ctx, ids[0], ids[1], string(rel), 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	if mutate != nil {
		mutate(store, ids, func(q string, args ...any) {
			t.Helper()
			if err := store.Close(); err != nil {
				t.Fatalf("close the seeding store: %v", err)
			}
			raw, err := memory.OpenDB(dbPath)
			if err != nil {
				t.Fatalf("OpenDB: %v", err)
			}
			defer raw.Close() //nolint:errcheck
			if _, err := raw.ExecContext(ctx, q, args...); err != nil {
				t.Fatalf("exec %q: %v", q, err)
			}
		})
	}
	return ids
}

const backdateLink = `UPDATE memory_links SET created_at = '2020-01-01 00:00:00' WHERE source_id = ? AND target_id = ?`

// seedLiveSupersedesEdge backdates the link so skip-if-unchanged re-judges the
// pair, which is how a reclassification reaches the classifier with no vector.
func seedLiveSupersedesEdge(t *testing.T, dbPath string) (newer, older string) {
	t.Helper()
	ids := seedEdge(t, dbPath, [2]string{
		"the deploy pipeline now runs the migration check before it stages a build",
		"the deploy pipeline runs the unit suite before it stages a build",
	}, supersede.RelationSupersedes, func(_ *memory.Store, ids [2]string, raw func(string, ...any)) {
		raw(backdateLink, ids[0], ids[1])
	})
	return ids[0], ids[1]
}

// seedLiveCausesEdge is the 'causes' equivalent (older -> newer). The older note
// also gets a real age: a 'causes' pair is asked in the direction orient gives,
// and orient refuses two rows sharing both timestamps.
func seedLiveCausesEdge(t *testing.T, dbPath string) (older, newer string) {
	t.Helper()
	ids := seedEdge(t, dbPath, [2]string{
		"the deploy pipeline runs the unit suite before it stages a build",
		"the deploy pipeline now runs the migration check before it stages a build",
	}, supersede.RelationCauses, func(store *memory.Store, ids [2]string, raw func(string, ...any)) {
		raw(`UPDATE memories SET created_at = '2026-01-01 00:00:00', updated_at = '2026-01-01 00:00:00' WHERE id = ?`, ids[0])
		raw(backdateLink, ids[0], ids[1])
	})
	return ids[0], ids[1]
}

// TestRunSupersedeReportsButDoesNotWithdrawTheEdgeItWouldRemove drives the real
// command: under both modes the denied edge is reported, stays live, and no
// resolve follow-up or "withdraw these edges" hint is printed (#845).
func TestRunSupersedeReportsButDoesNotWithdrawTheEdgeItWouldRemove(t *testing.T) {
	for _, apply := range []bool{false, true} {
		t.Run(map[bool]string{false: "dry run", true: "applied"}[apply], func(t *testing.T) {
			dbPath := seedDataDir(t)
			useStubHarness(t, "NEITHER")
			newer, older := seedLiveSupersedesEdge(t, dbPath)
			args := []string{"projy", "--source", "opencode", "--consensus", "3"}
			if apply {
				args = append(args, "--apply")
			}
			out := runSupersedeArgs(t, args...)
			for _, want := range []string{"would withdraw", shortID(newer) + " -> " + shortID(older), "neither", "STILL LIVE",
				"1 pair(s) withheld", "ghost supersede projy --reassess --consensus 3 --apply"} {
				if !strings.Contains(out, want) {
					t.Errorf("`ghost supersede` did not report %q:\n%s", want, out)
				}
			}
			for _, notWant := range []string{"Follow-up:", "ghost resolve projy --reassess", "Re-run with --apply to withdraw these edges."} {
				if strings.Contains(out, notWant) {
					t.Errorf("a run that left the edge live printed %q:\n%s", notWant, out)
				}
			}
			if n := liveEdges(t, dbPath, supersede.RelationSupersedes); n != 1 {
				t.Errorf("live edge(s) = %d, want 1", n)
			}
		})
	}
}

// TestSupersedeWithdrawFollowsTheTargetIntoItsOwnProject (#786): a `_global
// --withdraw` of a promoted source scopes the resolve repair to the project that
// owns the target, since `_global`'s pool holds no project memory.
func TestSupersedeWithdrawFollowsTheTargetIntoItsOwnProject(t *testing.T) {
	dbPath := seedDataDir(t)
	ids := seedEdge(t, dbPath, [2]string{
		"the ingest service now writes both replicas before acknowledging",
		"the ingest service acknowledges after the primary replica only",
	}, supersede.RelationSupersedes, func(store *memory.Store, ids [2]string, _ func(string, ...any)) {
		if err := store.PromoteToGlobal(context.Background(), "projy", ids[0]); err != nil {
			t.Fatalf("PromoteToGlobal: %v", err)
		}
		if err := store.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	})
	out := runSupersedeArgs(t, "_global", "--withdraw", ids[0][:8], ids[1][:8], "--apply")
	if n := liveEdges(t, dbPath, supersede.RelationSupersedes); n != 0 {
		t.Fatalf("live edge(s) = %d after the withdrawal, want 0", n)
	}
	if !strings.Contains(out, "ghost resolve projy --reassess --only") || strings.Contains(out, "ghost resolve _global --reassess --only") || !strings.Contains(out, ids[1]) {
		t.Errorf("the follow-up is not scoped to the target's project:\n%s", out)
	}
}

// TestRunSupersedeReportsACausesEdgeItReplacedWithASupersession (#823): the row
// says the pair is now a supersession and names the replacing edge, the summary
// counts it, and no resolve repair is owed for a withdrawn 'causes' edge.
func TestRunSupersedeReportsACausesEdgeItReplacedWithASupersession(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply bool
		row   string
	}{{"dry run", false, "would withdraw"}, {"applied", true, "withdrew"}} {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := seedDataDir(t)
			// A `replaced:` claim is required, or the verdict reads NEITHER.
			useStubHarness(t, `SUPERSEDES\\nreplaced: the deploy pipeline runs the unit suite before it stages a build`)
			older, newer := seedLiveCausesEdge(t, dbPath)
			args := []string{"projy", "--source", "opencode"}
			if tc.apply {
				args = append(args, "--apply")
			}
			out := runSupersedeArgs(t, args...)
			for _, want := range []string{tc.row, "supersedes: the newer note replaces the older one", "re-linked",
				"supersedes -> " + shortID(older), shortID(newer), "1 reclassified"} {
				if !strings.Contains(out, want) {
					t.Errorf("`ghost supersede` did not report %q:\n%s", want, out)
				}
			}
			if !tc.apply && !strings.Contains(out, "Re-run with --apply to write these links.") {
				t.Errorf("a dry run does not tell the operator to apply:\n%s", out)
			}
			for _, notWant := range []string{"Follow-up:", "ghost resolve projy --reassess", "Re-run with --apply to withdraw these edges."} {
				if strings.Contains(out, notWant) {
					t.Errorf("printed %q for a withdrawn 'causes' edge:\n%s", notWant, out)
				}
			}
			causes, supers := liveEdges(t, dbPath, supersede.RelationCauses), liveEdges(t, dbPath, supersede.RelationSupersedes)
			wantCauses, wantSupers := 1, 0
			if tc.apply {
				wantCauses, wantSupers = 0, 1
			}
			if causes != wantCauses || supers != wantSupers {
				t.Errorf("live causes/supersedes = %d/%d, want %d/%d", causes, supers, wantCauses, wantSupers)
			}
		})
	}
}

// TestSupersedePairLinesNamesTheEdgeACausesCycleRemoved: a 'causes' cycle
// answered CAUSES drops the opposite edge while the relation is unchanged, and
// the row has to say so, in the future tense for a dry run.
func TestSupersedePairLinesNamesTheEdgeACausesCycleRemoved(t *testing.T) {
	row := causesRow(supersede.RelationCauses, true)
	row.CausesDropped = 1
	out := supersedePairLines(true, []supersede.Classified{row})
	if !strings.Contains(out, "withdrew") || strings.Contains(out, "already gone") || !strings.Contains(out, "[+1 causes edge dropped]") {
		t.Errorf("an applied cycle row misreports:\n%s", out)
	}
	promised := row
	promised.Withdrawn, promised.CausesDropped, promised.CausesDroppable = false, 0, 1
	dry := supersedePairLines(false, []supersede.Classified{promised})
	for _, want := range []string{"would withdraw", "[+1 causes edge would be dropped]", "re-linked"} {
		if !strings.Contains(dry, want) {
			t.Errorf("a dry run's row lacks %q:\n%s", want, dry)
		}
	}
	if strings.Contains(dry, "[+1 causes edge dropped]") {
		t.Errorf("a dry run's row claims a deletion it did not make:\n%s", dry)
	}
	quiet := row
	quiet.Withdrawn, quiet.CausesDropped = false, 0
	if out := supersedePairLines(true, []supersede.Classified{quiet}); strings.Contains(out, "withdrew") {
		t.Errorf("a re-affirmation that moved nothing is dressed as a withdrawal:\n%s", out)
	}
}

// TestSupersedePairLinesSaysKeptWhenTheEdgeItIsAboutSurvived: a re-affirmed
// 'supersedes' edge beside a swept 'causes' cycle is `kept`, never `already gone`.
func TestSupersedePairLinesSaysKeptWhenTheEdgeItIsAboutSurvived(t *testing.T) {
	row := reclassRow(supersede.RelationSupersedes, true, false)
	row.CausesDropped = 2
	out := supersedePairLines(true, []supersede.Classified{row})
	for _, want := range []string{"kept", "supersedes: the newer note replaces the older one", "re-affirmed", "[+2 causes edge dropped]"} {
		if !strings.Contains(out, want) {
			t.Errorf("the row does not say %q:\n%s", want, out)
		}
	}
	for _, notWant := range []string{"already gone", "would withdraw", "re-linked"} {
		if strings.Contains(out, notWant) {
			t.Errorf("the row claims %q:\n%s", notWant, out)
		}
	}
	taken := row
	taken.Relation, taken.CausesDropped = supersede.RelationNeither, 0
	if out := supersedePairLines(true, []supersede.Classified{taken}); !strings.Contains(out, "already gone") {
		t.Errorf("an edge this run did not remove is not reported as already gone:\n%s", out)
	}
}
