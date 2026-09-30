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
)

func reclassRow(relation supersede.Relation, reclassified, withdrawn bool) supersede.Classified {
	return supersede.Classified{
		Candidate: supersede.Candidate{
			NewerID: reclassNewer, OlderID: reclassOlder,
		},
		Relation:        relation,
		Reclassified:    reclassified,
		Withdrawn:       withdrawn,
		TargetProjectID: "proj",
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
			// The same refusal on a CAUSES verdict, whose edge runs the other
			// way, so the marker rides the causes line rather than the
			// supersedes one.
			name:  "a refused causes write is marked too",
			apply: true,
			classified: []supersede.Classified{
				opposedRow(supersede.RelationCauses),
			},
			want:    []string{"causes", "not written"},
			notWant: []string{"withdrew", "would withdraw"},
		},
		{
			// And a reclassified pair refused the same way. It cannot happen for
			// a SUPERSEDES verdict (the live edge IS the direction the pair
			// was asked about, so nothing opposes it), which is exactly why
			// the marker is tested before the withdrawal vocabulary: a
			// future verdict that can be refused must not be printed as a
			// withdrawal this run did not make.
			name:  "a refused reclassified write is not dressed as a withdrawal",
			apply: true,
			classified: []supersede.Classified{
				opposedRow(supersede.RelationCauses),
			},
			want:    []string{"not written"},
			notWant: []string{"withdrew", "would withdraw", "already gone"},
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
	}))
	if len(got) != 1 || len(got[0].Targets) != 1 || got[0].Targets[0] != reclassOlder {
		t.Errorf("withdrawnTargets(reclassifiedWithdrawals(...)) = %+v, want just [%s]: the two withdrawals, deduplicated, and nothing from a fresh or confirmed pair", got, reclassOlder)
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
