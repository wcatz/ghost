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

// The three ids here are 32-character hex-shaped, so shortID abbreviates each to
// the eight characters every Ghost report prints — which is the point: the rows
// below are read from a report, so the ids in them have to be the form a report
// prints and the follow-up has to be driveable from what it names.
const (
	reclassNewer = "aaaaaaaabbbbccccddddeeeeffff0000"
	reclassOlder = "11111111222233334444555566667777"
	reclassFresh = "88888888777766665555444433332222"
	// reclassCauses is a pair of its own for the 'causes'-edge rows below, and it
	// has to be: a target the follow-up already lists would be deduplicated away,
	// and a row that cannot change the list cannot prove the list excludes it.
	reclassCausesNewer  = "ccccccccddddeeeeffff0000aaaaaaaabbbb"
	reclassCausesTarget = "22222222333344445555666677778888"
)

// reclassRow is a row the pass produced over a pair that CARRIED a live
// 'supersedes' edge, which is the shape every case below but the last one is.
// ReclassifiedFrom names that edge, and a fixture that leaves it empty would be
// describing a reclassify of nothing: a live edge may be 'causes' as well now
// (#823), and both the report and the follow-up branch on which relation it was.
func reclassRow(relation supersede.Relation, reclassified, withdrawn bool) supersede.Classified {
	return supersede.Classified{
		Candidate: supersede.Candidate{
			NewerID: reclassNewer, OlderID: reclassOlder,
		},
		Relation:         relation,
		Reclassified:     reclassified,
		ReclassifiedFrom: supersede.RelationSupersedes,
		Withdrawn:        withdrawn,
		TargetProjectID:  "proj",
	}
}

// TestSupersedePairLinesNamesTheEdgeAPassWithdrew is #785 as the operator meets
// it. The ordinary pass performs the identical InvalidateLink that --reassess
// and --withdraw perform and leaves the identical orphaned `resolved_at`, so
// its withdrawal is as much a repair as theirs — and it used to appear only as
// the `N reclassified` total in the header. An unattended pass then withdrew a
// correct edge, the target stayed `resolved_at`-stamped and out of every
// session, and nobody was handed the command that finishes the repair.
//
// Every row therefore carries the three things the other two paths carry: which
// edge (both ids), which verdict withdrew it, and whether THIS run moved it.
func TestSupersedePairLinesNamesTheEdgeAPassWithdrew(t *testing.T) {
	rows := []struct {
		name       string
		apply      bool
		classified []supersede.Classified
		want       []string
		notWant    []string
	}{
		{
			name:  "a dry run says it would withdraw, and claims no write",
			apply: false,
			classified: []supersede.Classified{
				reclassRow(supersede.RelationNeither, true, false),
			},
			want: []string{
				"would withdraw",
				shortID(reclassNewer) + " -> " + shortID(reclassOlder),
				"neither",
			},
			notWant: []string{"\nwithdrew", "\nalready gone"},
		},
		{
			name:  "an applied run says it withdrew",
			apply: true,
			classified: []supersede.Classified{
				reclassRow(supersede.RelationNeither, true, true),
			},
			want: []string{"withdrew", shortID(reclassNewer) + " -> " + shortID(reclassOlder), "neither"},
			// The past tense, and only where the write landed. A row that said
			// "withdrew" for an edge a concurrent pass had already taken is a
			// report claiming a graph change this run did not make.
			notWant: []string{"would withdraw", "already gone"},
		},
		{
			name:  "an edge a concurrent pass took first is not claimed",
			apply: true,
			classified: []supersede.Classified{
				reclassRow(supersede.RelationNeither, true, false),
			},
			want:    []string{"already gone", shortID(reclassNewer) + " -> " + shortID(reclassOlder)},
			notWant: []string{"would withdraw", "\nwithdrew"},
		},
		{
			name:  "a causes verdict says the pair was re-linked too",
			apply: true,
			classified: []supersede.Classified{
				reclassRow(supersede.RelationCauses, true, true),
			},
			// The pass wrote a second graph row here, and a report that only
			// said "withdrew" would leave the operator deciding about a change
			// they were never told about.
			want: []string{"withdrew", "causes", "as " + shortID(reclassOlder) + " causes -> " + shortID(reclassNewer)},
		},
		{
			name:  "a fresh pair is still reported as the write it is",
			apply: true,
			classified: []supersede.Classified{
				reclassRow(supersede.RelationSupersedes, false, false),
			},
			want:    []string{shortID(reclassNewer) + "  supersedes  " + shortID(reclassOlder)},
			notWant: []string{"withdrew", "would withdraw", "->"},
		},
		{
			name:  "a fresh causes pair keeps the causes line",
			apply: true,
			classified: []supersede.Classified{
				reclassRow(supersede.RelationCauses, false, false),
			},
			want:    []string{shortID(reclassOlder) + "  causes  " + shortID(reclassNewer)},
			notWant: []string{"withdrew", "would withdraw"},
		},
		{
			// A reversed FRESH pair has no edge to withdraw, and the refusal line
			// is the whole report for it. A reversed RECLASSIFY pair has one, so
			// it gets the withdrawal row instead — the two are different findings
			// and one line cannot carry both.
			name:  "a fresh reversal keeps the refusal line",
			apply: true,
			classified: []supersede.Classified{
				reclassRow(supersede.RelationReversed, false, false),
			},
			want:    []string{"reversed, not written"},
			notWant: []string{"withdrew", "would withdraw"},
		},
		{
			// A denying verdict moves a SECOND row: NEITHER and REVERSED both drop
			// the pair's live 'causes' edge, and a row that named only the
			// supersedes edge would say the run moved one row when it moved two.
			// `--reassess` marks the same thing, and these rows are printed in its
			// shape.
			name:  "a withdrawal that also dropped a causes edge says so",
			apply: true,
			classified: []supersede.Classified{
				withCauses(reclassRow(supersede.RelationNeither, true, true), 1),
			},
			want:    []string{"withdrew", "[+1 causes edge dropped]"},
			notWant: []string{"+0 causes", "would withdraw"},
		},
		{
			// The count is what the write returned, so a dry run has none — and a
			// marker over a pass that deleted nothing is the one line this report
			// must not print.
			name:  "a dry run forecasts no causes deletion",
			apply: false,
			classified: []supersede.Classified{
				withCauses(reclassRow(supersede.RelationNeither, true, false), 0),
			},
			want:    []string{"would withdraw"},
			notWant: []string{"causes edge dropped"},
		},
		{
			// A pair the pass JUDGED and did not write, because the other
			// direction was already live when the write was attempted (#806). The
			// row is still here — a verdict was reached — and it has to say
			// so on the row, because a line reading as a link the pass created
			// is the false claim the other three markers above exist to
			// prevent. The "withdrew" vocabulary is held away from it for the
			// same reason: nothing was withdrawn either.
			name:  "a pair the pass judged and did not write is not claimed",
			apply: true,
			classified: []supersede.Classified{
				opposedRow(supersede.RelationSupersedes),
			},
			want:    []string{"not written", "the pair's reverse direction is already live", shortID(reclassNewer) + "  supersedes  " + shortID(reclassOlder)},
			notWant: []string{"withdrew", "would withdraw", "already gone"},
		},
		{
			// The same refusal carried by a CAUSES row. Only a SUPERSEDES row
			// gets the flag in production — the 'causes' write is not guarded,
			// because nothing demotes on it and refusing it there would cost a
			// call per pass forever — and a reclassified row cannot get it
			// either, because the live edge decides the direction it is asked
			// about. The row is here because the RENDERER's rule is "a row
			// that claims a write this run declined says so", whatever the row
			// holds: without the two guards this fails, printing a re-link
			// and a withdrawal for a run that did neither.
			name:  "a refused write is never dressed as a re-link or a withdrawal",
			apply: true,
			classified: []supersede.Classified{
				opposedRow(supersede.RelationCauses),
			},
			want:    []string{"not written", "causes"},
			notWant: []string{"re-linked", "withdrew", "would withdraw", "already gone"},
		},
		{
			name:  "a reversed reclassification is a withdrawal like any other",
			apply: true,
			classified: []supersede.Classified{
				reclassRow(supersede.RelationReversed, true, true),
			},
			want:    []string{"withdrew", "reversed", shortID(reclassNewer) + " -> " + shortID(reclassOlder)},
			notWant: []string{"reversed, not written"},
		},
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

// opposedRow is reclassRow for the one outcome where the pass reached a
// verdict and wrote nothing: the pair's opposite direction was already
// live when the write was attempted (#806). Reclassified is true for the
// CAUSES shape because that is the row the refusal can reach — a
// SUPERSEDES reclassification is asked in the live edge's own direction,
// so nothing opposes it.
func opposedRow(relation supersede.Relation) supersede.Classified {
	row := reclassRow(relation, relation == supersede.RelationCauses, false)
	row.OpposedLive = true
	return row
}

// withCauses is reclassRow plus the second graph row a denying verdict moves.
func withCauses(row supersede.Classified, dropped int) supersede.Classified {
	row.CausesDropped = dropped
	return row
}

// TestSupersedePairLinesPrintsNothingForAnEmptyPass: the header line above says
// how many pairs were considered, and a block that printed a header of its own
// would be a second count of the same thing.
func TestSupersedePairLinesPrintsNothingForAnEmptyPass(t *testing.T) {
	if got := supersedePairLines(true, nil); got != "" {
		t.Errorf("supersedePairLines(true, nil) = %q, want the empty string", got)
	}
	if got := supersedePairLines(true, []supersede.Classified{
		{ // A pair the classifier could not parse: nothing was decided, so there
			// is no relation to print and no edge to have withdrawn.
			Candidate: supersede.Candidate{NewerID: reclassNewer, OlderID: reclassOlder},
			Relation:  supersede.Relation(""),
		},
	}); got != "" {
		t.Errorf("an unparseable verdict printed %q, want nothing: the pass decided nothing about this pair", got)
	}
}

// TestReclassifiedWithdrawalsNamesEveryOrphanedTarget: the withdrawal is half the
// repair, and the half the graph cannot do on its own is the `resolved_at` the
// edge's piggyback stamped on the target. So the follow-up is built over these
// rows and only these rows — a fresh pair contributes none, because no edge ever
// justified a resolution for it.
//
// Every withdrawal counts, INCLUDING one this run did not write: a concurrent
// pass that took the edge first left the same state behind (no live edge, and a
// resolved_at nothing defends), which is exactly the state the repair clears. The
// reason is the same one withdrawnTargets gives.
func TestReclassifiedWithdrawalsNamesEveryOrphanedTarget(t *testing.T) {
	got := withdrawnTargets(reclassifiedWithdrawals([]supersede.Classified{
		reclassRow(supersede.RelationNeither, true, true),
		reclassRow(supersede.RelationReversed, true, false),
		reclassRow(supersede.RelationSupersedes, true, false),
		reclassRow(supersede.RelationCauses, false, false),
		{
			Candidate:    supersede.Candidate{NewerID: reclassFresh, OlderID: reclassOlder},
			Relation:     supersede.RelationNeither,
			Reclassified: false,
		},
		// A pair that carried a live 'causes' edge and came back NEITHER: a
		// real graph change, and NO repair. The follow-up is a `ghost resolve
		// --reassess`, and resolve's supersedes piggyback — the thing that
		// stamped the resolved_at this repair clears — acts on 'supersedes'
		// edges alone, so a withdrawn 'causes' edge orphaned nothing and naming
		// its target would send the operator after a memory nothing is holding
		// down.
		{
			Candidate: supersede.Candidate{
				NewerID: reclassCausesNewer, OlderID: reclassCausesTarget,
			},
			Relation:         supersede.RelationNeither,
			Reclassified:     true,
			ReclassifiedFrom: supersede.RelationCauses,
			Withdrawn:        true,
			TargetProjectID:  "proj",
		},
	}))
	if len(got) != 1 || len(got[0].Targets) != 1 || got[0].Targets[0] != reclassOlder {
		t.Errorf("withdrawnTargets(reclassifiedWithdrawals(...)) = %+v, want just [%s]: the two supersedes withdrawals, deduplicated, and nothing from a fresh, confirmed or 'causes'-edge pair", got, reclassOlder)
	}
	for _, g := range got {
		for _, target := range g.Targets {
			if target == reclassCausesTarget {
				t.Errorf("the follow-up names %s, the target of a withdrawn 'causes' edge: no 'causes' edge ever stamped a resolved_at, so there is nothing there to clear", reclassCausesTarget)
			}
		}
	}

	// And the two withdrawals really do reach the SAME follow-up the other two
	// repair paths print — a scoped resolve repair over the withdrawn targets,
	// never the project-wide re-judge. This is the sentence the operator was
	// never given. The command goes through the shared renderer, so the id is
	// quoted the way a shell reads it as one argument.
	one := withdrawnTargets(reclassifiedWithdrawals([]supersede.Classified{
		reclassRow(supersede.RelationNeither, true, true),
	}))
	if len(one) != 1 {
		t.Fatalf("withdrawnTargets = %+v, want one project group", one)
	}
	block := supersedeReassessFollowup(one[0].ProjectID, one[0].Targets, "")
	if !strings.Contains(block, "ghost resolve proj --reassess --only '"+reclassOlder+"' --apply") {
		t.Errorf("the follow-up an ordinary pass prints is not the scoped resolve repair:\n%s", block)
	}
}

// TestRunSupersedeReportsAndFollowsUpTheEdgeItWithdrew drives the real command,
// because the formatters above are the report's WORDING and the thing #785
// removed was the report's EXISTENCE: the ordinary pass ran the identical
// InvalidateLink and printed neither the edge nor the repair, so a formatter
// test of it would pass against a command that said nothing at all.
//
// The fixture is a live 'supersedes'/'llm' edge over two notes, with the link
// row backdated so skip-if-unchanged re-judges the pair — which is how a pair
// whose endpoints moved reaches the classifier with no vector involved. The
// fake harness answers NEITHER, so the edge is denied and withdrawn.
func TestRunSupersedeReportsAndFollowsUpTheEdgeItWithdrew(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply bool
		// The tense the row must carry, and the hint the run owes the operator
		// when the answer is still to be given.
		row  string
		hint string
	}{
		{name: "dry run", apply: false, row: "would withdraw", hint: "Re-run with --apply to withdraw these edges."},
		{name: "applied", apply: true, row: "withdrew", hint: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataHome := isolatedLifecycleEnv(t)
			// A harness the pass can resolve, answering the one-word verdict
			// contract inside the JSON-lines envelope `opencode run --format json`
			// emits. The stub ignores its arguments, so the same script answers the
			// availability probe and the classify call.
			binDir := t.TempDir()
			harness := filepath.Join(binDir, "opencode")
			writeExecutable(t, harness, "#!/bin/sh\n"+`printf '{"type":"text","part":{"type":"text","text":"NEITHER"}}\n'`+"\n")
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("GHOST_CLI_OPENCODE_BINARY", harness)

			dbPath := filepath.Join(dataHome, "ghost", "ghost.db")
			if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
				t.Fatalf("mkdir data dir: %v", err)
			}
			newer, older := seedLiveSupersedesEdge(t, dbPath)

			origArgs := os.Args
			args := []string{origArgs[0], "supersede", "projy", "--source", "opencode"}
			if tc.apply {
				args = append(args, "--apply")
			}
			os.Args = args
			t.Cleanup(func() { os.Args = origArgs })

			out := captureStdout(t, runSupersede)

			// The edge, by both ids, in the report's own eight-character form, and
			// the verdict that withdrew it.
			for _, want := range []string{
				tc.row,
				shortID(newer) + " -> " + shortID(older),
				"neither",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("`ghost supersede` did not report %q:\n%s", want, out)
				}
			}
			// And the second half of the repair, which only an --apply run owes.
			// The command is scoped to the withdrawn target, never the
			// project-wide re-judge: the block promises that nothing outside the
			// list is judged, and an unscoped command under that heading would be
			// the worst line in the report.
			if tc.apply {
				if !strings.Contains(out, "Follow-up:") {
					t.Errorf("an --apply run withdrew an edge and printed no follow-up:\n%s", out)
				}
				if !strings.Contains(out, "ghost resolve projy --reassess --only") || !strings.Contains(out, older) {
					t.Errorf("the follow-up is not the scoped resolve repair over the withdrawn target:\n%s", out)
				}
				if strings.Contains(out, "reassess --apply\n") {
					t.Errorf("the follow-up fell back on the unscoped re-judge:\n%s", out)
				}
			} else {
				if strings.Contains(out, "Follow-up:") {
					t.Errorf("a dry run printed a follow-up for a repair it did not perform:\n%s", out)
				}
				if !strings.Contains(out, tc.hint) {
					t.Errorf("a dry run that would withdraw an edge does not tell the operator to apply:\n%s", out)
				}
			}

			// The tense is a claim about the graph, so the graph is checked.
			store := openStore(t, dbPath)
			links, err := store.LinksByRelationSource(context.Background(), "projy", string(supersede.RelationSupersedes), "llm")
			if err != nil {
				t.Fatalf("LinksByRelationSource: %v", err)
			}
			if tc.apply && len(links) != 0 {
				t.Errorf("the report says it withdrew the edge, and %d live edge(s) remain", len(links))
			}
			if !tc.apply && len(links) != 1 {
				t.Errorf("a dry run left %d live edge(s), want 1: it wrote something", len(links))
			}
		})
	}
}

// seedLiveSupersedesEdge writes a project with two notes and a live
// 'supersedes'/'llm' edge between them, and backdates the link row so
// skip-if-unchanged re-judges the pair on the next pass. It returns the two ids.
//
// The backdating is what makes this a reclassification rather than a fresh
// proposal: neither memory carries a vector, so the candidate scan proposes
// nothing, and the reclassify half — which reads link rows and never a vector —
// is the only thing that can put this pair to the classifier.
func seedLiveSupersedesEdge(t *testing.T, dbPath string) (newer, older string) {
	t.Helper()
	ctx := context.Background()
	store := openStore(t, dbPath)
	if err := store.EnsureProject(ctx, "projy", "/tmp/projy", "projy"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	ids := make([]string, 0, 2)
	for _, content := range []string{
		"the deploy pipeline now runs the migration check before it stages a build",
		"the deploy pipeline runs the unit suite before it stages a build",
	} {
		id, err := store.Create(ctx, "projy", memory.Memory{
			Category: "architecture", Content: content, Source: "mcp", Importance: 0.7,
		})
		if err != nil {
			t.Fatalf("create memory: %v", err)
		}
		ids = append(ids, id)
	}
	newer, older = ids[0], ids[1]
	if err := store.CreateLink(ctx, newer, older, string(supersede.RelationSupersedes), 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	// Closed before the raw handle opens, so the backdate cannot queue behind a
	// connection the seeding store still holds.
	if err := store.Close(); err != nil {
		t.Fatalf("close the seeding store: %v", err)
	}
	raw, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer raw.Close() //nolint:errcheck
	if _, err := raw.ExecContext(ctx,
		`UPDATE memory_links SET created_at = '2020-01-01 00:00:00' WHERE source_id = ? AND target_id = ?`,
		newer, older,
	); err != nil {
		t.Fatalf("backdate the link row: %v", err)
	}
	return newer, older
}

// TestSupersedeWithdrawFollowsTheTargetIntoItsOwnProject is the second half of
// #786, and it is a promise the first half would otherwise break. A `ghost
// supersede _global --withdraw` whose target stayed in a project is the whole
// point of reaching such a pair from `_global` — and the follow-up it printed was
// `ghost resolve _global --reassess --only <target> --apply`, which resolves the
// selector (a `_global` ref scope reaches a project memory) and then finds
// nothing: `ResolvedCandidates` filters `project_id = ?`, so a memory in `p` is
// not in the `_global` pool. Every selector comes back a miss and the orphaned
// `resolved_at` is never cleared, under a block that says it can now be cleared.
//
// So the repair is scoped to the project that owns the memory being un-hidden,
// which is the project its `resolved_at` lives in. The common case — a target in
// the project the command was run against — is unchanged, because that is the same
// project.
func TestSupersedeWithdrawFollowsTheTargetIntoItsOwnProject(t *testing.T) {
	dataHome := isolatedLifecycleEnv(t)
	dbPath := filepath.Join(dataHome, "ghost", "ghost.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	source, target := seedPromotedSupersedesEdge(t, dbPath)

	origArgs := os.Args
	os.Args = []string{origArgs[0], "supersede", "_global",
		"--withdraw", source[:8], target[:8], "--apply"}
	t.Cleanup(func() { os.Args = origArgs })

	out := captureStdout(t, runSupersede)

	// The edge is gone, and the report says so.
	store := openStore(t, dbPath)
	links, err := store.LinksByRelationSource(context.Background(), "projy", string(supersede.RelationSupersedes), "llm")
	if err != nil {
		t.Fatalf("LinksByRelationSource: %v", err)
	}
	if len(links) != 0 {
		t.Fatalf("live edge(s) = %d after the withdrawal, want 0: the run did not reach the edge at all", len(links))
	}
	// And the follow-up names the project whose pool holds the target, not the one
	// the command was run against.
	if !strings.Contains(out, "ghost resolve projy --reassess --only") {
		t.Errorf("the follow-up is not scoped to the TARGET's project, so its repair cannot reach the memory it un-hides:\n%s", out)
	}
	if strings.Contains(out, "ghost resolve _global --reassess --only") {
		t.Errorf("the follow-up is scoped to _global, whose repair pool holds no project memory:\n%s", out)
	}
	if !strings.Contains(out, target) {
		t.Errorf("the follow-up names no target id:\n%s", out)
	}
}

// seedPromotedSupersedesEdge writes a project with a live 'supersedes'/'llm' edge
// and then promotes the SOURCE into `_global`, which is the shape
// `ghost_memory_promote` leaves behind. It returns the two ids.
func seedPromotedSupersedesEdge(t *testing.T, dbPath string) (source, target string) {
	t.Helper()
	ctx := context.Background()
	store := openStore(t, dbPath)
	if err := store.EnsureProject(ctx, "projy", "/tmp/projy", "projy"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	ids := make([]string, 0, 2)
	for _, content := range []string{
		"the ingest service now writes both replicas before acknowledging",
		"the ingest service acknowledges after the primary replica only",
	} {
		id, err := store.Create(ctx, "projy", memory.Memory{
			Category: "architecture", Content: content, Source: "mcp", Importance: 0.7,
		})
		if err != nil {
			t.Fatalf("create memory: %v", err)
		}
		ids = append(ids, id)
	}
	source, target = ids[0], ids[1]
	if err := store.CreateLink(ctx, source, target, string(supersede.RelationSupersedes), 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	if err := store.PromoteToGlobal(ctx, "projy", source); err != nil {
		t.Fatalf("PromoteToGlobal: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close the seeding store: %v", err)
	}
	return source, target
}

// seedLiveCausesEdge is seedLiveSupersedesEdge for the other relation, and the
// pair it writes is a live 'causes' edge — older→newer, so the SOURCE is the
// older note. Stamped 2020 for the same reason the supersedes fixture is: the
// pair has to be re-judged for a verdict to exist, and the way to reach the
// classifier with no vector involved is a pair whose edge predates both
// endpoints.
func seedLiveCausesEdge(t *testing.T, dbPath string) (older, newer string) {
	t.Helper()
	ctx := context.Background()
	store := openStore(t, dbPath)
	if err := store.EnsureProject(ctx, "projy", "/tmp/projy", "projy"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	ids := make([]string, 0, 2)
	for _, content := range []string{
		"the deploy pipeline runs the unit suite before it stages a build",
		"the deploy pipeline now runs the migration check before it stages a build",
	} {
		id, err := store.Create(ctx, "projy", memory.Memory{
			Category: "architecture", Content: content, Source: "mcp", Importance: 0.7,
		})
		if err != nil {
			t.Fatalf("create memory: %v", err)
		}
		ids = append(ids, id)
	}
	older, newer = ids[0], ids[1]
	if err := store.CreateLink(ctx, older, newer, string(supersede.RelationCauses), 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close the seeding store: %v", err)
	}
	raw, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer raw.Close() //nolint:errcheck
	if _, err := raw.ExecContext(ctx,
		`UPDATE memory_links SET created_at = '2020-01-01 00:00:00' WHERE source_id = ? AND target_id = ?`,
		older, newer,
	); err != nil {
		t.Fatalf("backdate the link row: %v", err)
	}
	return older, newer
}

// TestRunSupersedeReportsACausesEdgeItReplacedWithASupersession is #823 on the
// report, and it drives the real command because the report is the whole of what
// an operator is told about a graph change.
//
// The pass read a live 'causes' edge, judged the pair SUPERSEDES, and replaced
// one graph row with another. Three things have to be true of what it printed:
//
//   - the reclassified row says the pair is now a SUPERSEDES, in words. The
//     bracketed finding is what distinguishes a real replacement from a wrong
//     edge, and an empty one leaves a line that says the run did something and
//     not what.
//
//   - the row names the edge that replaced it. "1 reclassified" in the summary is
//     a total; a total over a relation change is a number nobody can act on, and
//     the second graph row the run moved is the half the line used to leave out.
//
//   - NO resolve follow-up. The follow-up is a `ghost resolve --reassess`, and
//     resolve's supersedes piggyback — the thing that stamped the resolved_at it
//     clears — acts on 'supersedes'/'llm' edges alone. A withdrawn 'causes' edge
//     orphaned no resolution, so printing the repair would send an operator to
//     clear a memory nothing is holding down, scoped to a target that is not the
//     one whose state changed.
func TestRunSupersedeReportsACausesEdgeItReplacedWithASupersession(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply bool
		row   string
	}{
		{name: "dry run", apply: false, row: "would withdraw"},
		{name: "applied", apply: true, row: "withdrew"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataHome := isolatedLifecycleEnv(t)
			binDir := t.TempDir()
			harness := filepath.Join(binDir, "opencode")
			// A supersession with a `replaced:` claim, because the rubric requires
			// one and a verdict without it reads NEITHER — which would make this
			// test pass for a reason that has nothing to do with the relation.
			writeExecutable(t, harness, "#!/bin/sh\n"+`printf '{"type":"text","part":{"type":"text","text":"SUPERSEDES\\nreplaced: the deploy pipeline runs the unit suite before it stages a build"}}\n'`+"\n")
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("GHOST_CLI_OPENCODE_BINARY", harness)

			dbPath := filepath.Join(dataHome, "ghost", "ghost.db")
			if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
				t.Fatalf("mkdir data dir: %v", err)
			}
			older, newer := seedLiveCausesEdge(t, dbPath)

			origArgs := os.Args
			args := []string{origArgs[0], "supersede", "projy", "--source", "opencode"}
			if tc.apply {
				args = append(args, "--apply")
			}
			os.Args = args
			t.Cleanup(func() { os.Args = origArgs })

			out := captureStdout(t, runSupersede)

			for _, want := range []string{
				tc.row,
				"supersedes: the newer note replaces the older one",
				"re-linked",
				"supersedes -> " + shortID(older),
				shortID(newer),
			} {
				if !strings.Contains(out, want) {
					t.Errorf("`ghost supersede` did not report %q:\n%s", want, out)
				}
			}
			// The summary has to count it too: the row is the detail and this is
			// the line an operator reads when they only read one.
			if !strings.Contains(out, "1 reclassified") {
				t.Errorf("the summary does not count the relation change:\n%s", out)
			}
			// And the dry run's hint must be the one that fits what it would do.
			// The "withdraw these edges" hint is tied to the resolve follow-up,
			// which this pair does not owe, so a dry run here promises to write a
			// link — which is exactly what --apply would do.
			if !tc.apply && !strings.Contains(out, "Re-run with --apply to write these links.") {
				t.Errorf("a dry run that would replace the edge does not tell the operator to apply:\n%s", out)
			}
			if !tc.apply && strings.Contains(out, "Re-run with --apply to withdraw these edges.") {
				t.Errorf("a dry run promises to withdraw an edge, and this pair's withdrawal orphans no resolution to repair:\n%s", out)
			}
			// And the repair that is NOT owed.
			if strings.Contains(out, "Follow-up:") || strings.Contains(out, "ghost resolve projy --reassess") {
				t.Errorf("a withdrawn 'causes' edge printed a resolve repair, and no 'causes' edge ever stamped the resolved_at it clears:\n%s", out)
			}

			// The graph, so the tense above is a claim about something.
			store := openStore(t, dbPath)
			ctx := context.Background()
			causes, err := store.LinksByRelationSource(ctx, "projy", string(supersede.RelationCauses), "llm")
			if err != nil {
				t.Fatalf("LinksByRelationSource(causes): %v", err)
			}
			supers, err := store.LinksByRelationSource(ctx, "projy", string(supersede.RelationSupersedes), "llm")
			if err != nil {
				t.Fatalf("LinksByRelationSource(supersedes): %v", err)
			}
			if tc.apply {
				if len(causes) != 0 {
					t.Errorf("the report says the 'causes' edge went and %d live edge(s) remain", len(causes))
				}
				if len(supers) != 1 {
					t.Errorf("the report says the pair is now a supersession and %d live edge(s) are", len(supers))
				}
			} else {
				if len(causes) != 1 || len(supers) != 0 {
					t.Errorf("a dry run left %d 'causes' and %d 'supersedes' edge(s), want 1 and 0: it wrote something", len(causes), len(supers))
				}
			}
		})
	}
}

// TestSupersedePairLinesNamesTheEdgeACausesCycleRemoved is the reporting half of
// the 'causes'-cycle rule, and it is a formatter test on purpose: what the pass
// writes is held by internal/supersede, and what the operator is TOLD is held
// here.
//
// A 'causes' CYCLE answered CAUSES keeps the direction the pass was asked about
// and drops the edge asserting the other, so the pair's live claim goes from two
// edges to one while the relation is unchanged. The row is a reclassified pair
// whose verdict equals the edge it carried, which is the one combination the
// report's skip test used to treat as "nothing happened" — so the run deleted a
// graph row, the summary counted zero reclassifications, and the row printed as an
// ordinary `causes` line.
func TestSupersedePairLinesNamesTheEdgeACausesCycleRemoved(t *testing.T) {
	row := supersede.Classified{
		Candidate: supersede.Candidate{
			NewerID: reclassCausesNewer, OlderID: reclassCausesTarget,
		},
		Relation:         supersede.RelationCauses,
		Reclassified:     true,
		ReclassifiedFrom: supersede.RelationCauses,
		Withdrawn:        true,
		CausesDropped:    1,
		TargetProjectID:  "proj",
	}
	// The two halves of the row: the marker, which is a claim about the graph and
	// has to be the one this run earns, and the clause naming the second row.
	out := supersedePairLines(true, []supersede.Classified{row})
	if !strings.Contains(out, "withdrew") {
		t.Errorf("a row whose live edge this run removed does not carry the `withdrew` marker:\n%s", out)
	}
	if strings.Contains(out, "already gone") {
		t.Errorf("a row this run withdrew claims the edge was already gone:\n%s", out)
	}
	if !strings.Contains(out, "[+1 causes edge dropped]") {
		t.Errorf("the row says nothing about the edge the run removed:\n%s", out)
	}
	// And the same pair in a dry run promises rather than claims. `Withdrawn` is
	// false there because the apply block never ran, which is the distinction the
	// marker reads — so the fixture has to differ, and the fact that it has to is
	// the reason a dry run cannot print a claim it has not earned.
	promised := row
	promised.Withdrawn = false
	dry := supersedePairLines(false, []supersede.Classified{promised})
	if !strings.Contains(dry, "would withdraw") {
		t.Errorf("a dry run's row is not in the would-withdraw tense:\n%s", dry)
	}
	if strings.Contains(dry, "causes edge dropped") {
		t.Errorf("a dry run forecasts a deletion count it never looked for:\n%s", dry)
	}

	// The control: a re-affirmation that moved NOTHING stays off this block. A
	// predicate that admits every re-classified row would make every quiet
	// `causes` re-confirmation print as a withdrawal, which is the false claim in
	// the other direction.
	quiet := row
	quiet.Withdrawn = false
	quiet.CausesDropped = 0
	if out := supersedePairLines(true, []supersede.Classified{quiet}); strings.Contains(out, "withdrew") {
		t.Errorf("a re-affirmation that moved nothing is dressed as a withdrawal:\n%s", out)
	}
}

// TestSupersedePairLinesSaysKeptWhenTheEdgeItIsAboutSurvived is the shape the
// second review round found, and it is a line that made THREE claims where two
// were false.
//
// A live 'supersedes' edge re-AFFIRMED beside a 'causes' CYCLE: the pass kept the
// edge (and re-stamped it), and the verdict removed the cycle's two rows. The row
// was routed into the withdrawal block because the run did move graph rows, and
// the block's markers then said `already gone` — a claim about the 'supersedes'
// edge, which is still live and which this run never removed — over a `re-linked`
// clause announcing a change of relation that had not happened.
func TestSupersedePairLinesSaysKeptWhenTheEdgeItIsAboutSurvived(t *testing.T) {
	row := supersede.Classified{
		Candidate: supersede.Candidate{
			NewerID: reclassNewer, OlderID: reclassOlder,
		},
		Relation:         supersede.RelationSupersedes,
		Reclassified:     true,
		ReclassifiedFrom: supersede.RelationSupersedes,
		CausesDropped:    2,
		TargetProjectID:  "proj",
	}
	out := supersedePairLines(true, []supersede.Classified{row})
	for _, want := range []string{
		"kept",
		"supersedes: the newer note replaces the older one",
		"re-affirmed",
		"[+2 causes edge dropped]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the row does not say %q:\n%s", want, out)
		}
	}
	// The two markers that would be false, named.
	for _, notWant := range []string{"already gone", "would withdraw", "re-linked"} {
		if strings.Contains(out, notWant) {
			t.Errorf("the row claims %q, and the edge it is about never left the graph:\n%s", notWant, out)
		}
	}

	// The control in the other direction: a row whose edge really WAS taken by a
	// concurrent pass still says `already gone`, because that is a fact about the
	// graph and the marker above is only a fact about THIS run.
	taken := row
	taken.Relation = supersede.RelationNeither
	taken.ReclassifiedFrom = supersede.RelationSupersedes
	taken.CausesDropped = 0
	if out := supersedePairLines(true, []supersede.Classified{taken}); !strings.Contains(out, "already gone") {
		t.Errorf("an edge this run did not remove is not reported as already gone:\n%s", out)
	}
}
