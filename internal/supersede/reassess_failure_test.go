package supersede

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// failingStore wraps a store and fails the Nth InvalidateLink, so the
// partial-repair path can be exercised: each invalidation is its own
// transaction, which is exactly why a failure half way through leaves real
// changes behind.
type failingStore struct {
	*memory.Store
	failAt int
	calls  int
}

func (f *failingStore) InvalidateLink(ctx context.Context, sourceID, targetID, relation string) (int64, error) {
	f.calls++
	if f.calls == f.failAt {
		return 0, errors.New("disk full")
	}
	return f.Store.InvalidateLink(ctx, sourceID, targetID, relation)
}

// pairReplyProvider answers with a verdict looked up by the NEWER note's text.
// Batch numbering is positional, so a fixture that means "this verdict for that
// pair" has to match on content the way the prompt carries it.
type pairReplyProvider struct {
	byNewer map[string]string
}

func (p *pairReplyProvider) Classify(_ context.Context, _, userContent string) (string, error) {
	matches := batchPairPattern.FindAllStringSubmatch(userContent, -1)
	if len(matches) == 0 {
		return "", errors.New("pair reply provider: no pairs in the rendered content")
	}
	lines := make([]string, 0, len(matches))
	for _, m := range matches {
		reply, ok := p.byNewer[m[2]]
		if !ok {
			// No scripted answer for this pair: NEITHER is the safe default and
			// the test's own counters say which pairs were meant to be which.
			reply = "NEITHER"
		}
		lines = append(lines, strconv.Itoa(len(lines)+1)+": "+reply)
	}
	return strings.Join(lines, "\n"), nil
}

// TestReassessRetriesAFailedClassifyCall: a batch whose first call fails and
// whose retry answers is judged, and the pass applies every withdrawal that
// verdict produced. The failure #699's rehearsal hit was one call in a project
// of many, and the rerun a minute later withdrew 76 of 87 — so the retry is what
// stands between a blip and a rerun an operator has to remember to make. The
// count is reported, because a pass that needed one must not look like a pass
// that did not.
func TestReassessRetriesAFailedClassifyCall(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	newer, older := seedEdge(t, store, db,
		"A restore that spanned two spindles took 41 minutes and the row count matched afterwards.",
		"The restore path on one spindle is safe and takes under a minute.")

	fp := &flakyProvider{resp: "NEITHER", fails: 1}
	cls := NewRelationClassifier(fp)
	cls.SetRetryDelay(0)
	var buf strings.Builder
	cls.SetLogger(slog.New(slog.NewTextHandler(&buf, nil)))

	res, withdrawn, err := Reassess(ctx, store, cls, "p", true, slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil {
		t.Fatalf("a first-call failure the retry answers must not fail the pass: %v", err)
	}
	if fp.calls != 2 {
		t.Errorf("provider calls = %d, want 2 (the first attempt and its one retry)", fp.calls)
	}
	if res.Neither != 1 || res.Withdrawn != 1 {
		t.Errorf("neither=%d withdrawn=%d, want 1 and 1: the retry's verdict is applied like any other", res.Neither, res.Withdrawn)
	}
	if cls.Retries() != 1 {
		t.Errorf("Retries() = %d, want 1", cls.Retries())
	}
	if len(withdrawn) != 1 || !withdrawn[0].Written {
		t.Errorf("withdrawn = %+v, want the edge the retry's verdict withdrew", withdrawn)
	}
	if pairs, _ := store.SupersedesWithin(ctx, []string{newer, older}); len(pairs) != 0 {
		t.Errorf("the edge survived: %d pair(s) remain", len(pairs))
	}
	if !strings.Contains(buf.String(), "retries=1") {
		t.Errorf("the summary must count the retry:\n%s", buf.String())
	}
}

// TestReassessReportsTheRepairItAlreadyMade pins the failure contract: a failed
// invalidation returns the edges that DID land alongside the error, and the
// summary is still logged. Each invalidation is its own transaction, so the
// edges before the failure are gone and a later pass will not see them again —
// LinksByRelationSource returns live edges only. A repair whose justification is
// auditability cannot report nothing because its Nth write failed.
func TestReassessReportsTheRepairItAlreadyMade(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	ids := make([][2]string, 0, 3)
	for i := 0; i < 3; i++ {
		newer, older := seedEdge(t, store, db,
			"A restore that spanned two spindles took 41 minutes in week "+string(rune('a'+i))+".",
			"The restore path on one spindle is safe and takes under a minute.")
		ids = append(ids, [2]string{newer, older})
	}
	// The call order is one supersedes write and one causes sweep per row, so
	// failAt 3 is the SECOND supersedes write — the first row's pair of calls
	// both succeed. (The causes sweep test below is the failAt 2 case.)
	var buf strings.Builder
	fs := &failingStore{Store: store, failAt: 3}
	cls := NewRelationClassifier(&fakeProvider{resp: "NEITHER"})
	res, withdrawn, err := Reassess(ctx, fs, cls, "p", true, slog.New(slog.NewTextHandler(&buf, nil)))
	if err == nil {
		t.Fatal("a failed invalidation must be fatal, or a repair that did not happen is reported as one")
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Errorf("error %q does not carry the store's failure", err)
	}
	if res.Withdrawn != 1 {
		t.Errorf("Withdrawn = %d, want 1: the first edge really was invalidated", res.Withdrawn)
	}
	if len(withdrawn) != 1 || !withdrawn[0].Written {
		t.Errorf("withdrawn = %+v, want the one edge that landed, marked written", withdrawn)
	}
	if withdrawn[0].NewerID != ids[0][0] {
		t.Errorf("withdrawn names %s, want the FIRST edge %s — the count is the edges that landed, not the ones judged",
			withdrawn[0].NewerID, ids[0][0])
	}
	// The edges decided but not attempted are not in the list: they are still
	// live, so the next pass judges them again.
	live, lerr := store.LinksByRelationSource(ctx, "p", string(RelationSupersedes), "llm")
	if lerr != nil {
		t.Fatalf("LinksByRelationSource: %v", lerr)
	}
	if len(live) != 2 {
		t.Errorf("%d live edge(s) left, want 2: the failure must not have consumed the ones it never reached", len(live))
	}
	if !strings.Contains(buf.String(), "supersede reassess") {
		t.Errorf("the summary was not logged on the failure path:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "failed=true") {
		t.Errorf("the summary must say the pass failed:\n%s", buf.String())
	}
}

// TestReassessKeepsTheRowWhoseCausesSweepFailed: the other half of the failure
// contract. The supersedes withdrawal for this edge landed, and the 'causes'
// sweep that follows it failed — so the row must still be reported, or the count
// and the list disagree and the edge is gone from the graph and from the report
// at the same time.
func TestReassessKeepsTheRowWhoseCausesSweepFailed(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	newer, older := seedEdge(t, store, db,
		"A restore that spanned two spindles took 41 minutes and the row count matched afterwards.",
		"NEVER run the restore with source and target on the same spindle.")
	if err := store.CreateLink(ctx, older, newer, string(RelationCauses), 0.9, "llm"); err != nil {
		t.Fatal(err)
	}
	fs := &failingStore{Store: store, failAt: 2} // the first call withdraws, the second is the sweep
	cls := NewRelationClassifier(&fakeProvider{resp: "SUPERSEDES | replaced: the restore is unsafe on one spindle"})
	res, withdrawn, err := Reassess(ctx, fs, cls, "p", true, discardLogger())
	if err == nil || !strings.Contains(err.Error(), "causes link") {
		t.Fatalf("err = %v, want the sweep's own failure named", err)
	}
	if res.Withdrawn != 1 {
		t.Errorf("Withdrawn = %d, want 1: the supersedes withdrawal landed before the sweep ran", res.Withdrawn)
	}
	if len(withdrawn) != 1 || !withdrawn[0].Written {
		t.Errorf("withdrawn = %+v, want the edge that landed, still listed and marked written", withdrawn)
	}
	if pairs, _ := store.SupersedesWithin(ctx, []string{newer, older}); len(pairs) != 0 {
		t.Errorf("the supersedes edge is still live: %d pair(s)", len(pairs))
	}
	// The sweep's outcome is UNKNOWN after a failed write, and the row has to say
	// that rather than report a count: a definite 0 would tell an operator who
	// read "would sweep 1" in the dry run that nothing else was deleted, which
	// is the one reading this pass cannot afford.
	if len(withdrawn) != 1 {
		t.Fatalf("withdrawn = %+v, want one row", withdrawn)
	}
	if !withdrawn[0].SweepFailed {
		t.Errorf("row = %+v, want SweepFailed set: the sweep errored, so the count is unknown", withdrawn[0])
	}
	if withdrawn[0].CausesSwept != 0 {
		t.Errorf("row reports CausesSwept = %d, want 0 alongside SweepFailed — a failed sweep has no observed count", withdrawn[0].CausesSwept)
	}
	if res.CausesSweepFailed != 1 {
		t.Errorf("CausesSweepFailed = %d, want 1", res.CausesSweepFailed)
	}
	if res.CausesWithdrawn != 0 {
		t.Errorf("CausesWithdrawn = %d, want 0: a failed sweep is counted as failed, not as swept", res.CausesWithdrawn)
	}
	// And the edge really is still there, which is why the answer is "unknown"
	// and not "0, nothing to move".
	causes, err := store.GetLinks(ctx, older)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	live := false
	for _, l := range causes {
		if l.Relation == string(RelationCauses) {
			live = true
		}
	}
	if !live {
		t.Error("the 'causes' edge is gone, so the fixture no longer exercises a failed sweep")
	}
}

// TestReassessSweepsTheOtherRelationOnASelfContradictingVerdict: Run's contract
// for NEITHER, the veto and REVERSED is a both-relations sweep — a 'causes' link
// pointing INTO a note the pass has just decided is still current asserts the
// opposite of that decision. The repair pass applies the same verdicts, so it
// sweeps the same way. (A CAUSES verdict is left alone: there the existing
// 'causes' edge may be saying something true, and a repair pass is not where
// that is re-decided — see the CAUSES test below.)
//
// Each subcase states which half decided it and asserts the counter that proves
// it: the NEITHER and REVERSED pairs carry a plain older note, so the classifier
// really is asked and its verdict is really parsed. An imperative older note
// would let the veto settle all three, and the scripted reply would never be
// read — which is exactly how this test passed for the wrong reason once.
func TestReassessSweepsTheOtherRelationOnASelfContradictingVerdict(t *testing.T) {
	for _, tc := range []struct {
		name string
		// older is the older note's text. Only the veto case states a rule.
		older  string
		reply  string
		byVeto bool
	}{
		{
			name:  "neither",
			older: "The restore path on one spindle is safe and takes under a minute.",
			reply: "NEITHER",
		},
		{
			name:  "reversed",
			older: "The restore path on one spindle is safe and takes under a minute.",
			reply: "REVERSED",
		},
		{
			name:   "vetoed",
			older:  "NEVER run the restore with source and target on the same spindle.",
			reply:  "SUPERSEDES | replaced: the restore is unsafe on one spindle",
			byVeto: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, db := seed(t)
			ctx := context.Background()
			newer, older := seedEdge(t, store, db,
				"A restore that spanned two spindles took 41 minutes and the row count matched afterwards.",
				tc.older)
			// The contradicting edge a pre-#686 rubric (or a hand edit) could
			// have left behind.
			if err := store.CreateLink(ctx, older, newer, string(RelationCauses), 0.9, "llm"); err != nil {
				t.Fatal(err)
			}
			// The dry run comes FIRST, as an operator runs it: it has to predict
			// the sweep, or the summary says 0 above rows marked [+1 causes
			// edge] — and it has to see the edge at all, which it cannot once the
			// apply below has withdrawn it.
			dryRes, dryWithdrawn, err := Reassess(ctx, store, NewRelationClassifier(&fakeProvider{resp: tc.reply}), "p", false, discardLogger())
			if err != nil {
				t.Fatalf("Reassess (dry): %v", err)
			}
			if dryRes.CausesWithdrawn != 1 {
				t.Errorf("dry run predicted %d causes edge(s), want 1: the sweep is a second graph row the operator is about to delete",
					dryRes.CausesWithdrawn)
			}
			if len(dryWithdrawn) != 1 || dryWithdrawn[0].CausesSwept != 1 || dryWithdrawn[0].Written {
				t.Errorf("dry run row = %+v, want one un-written row predicting the sweep", dryWithdrawn)
			}
			if pairs, _ := store.SupersedesWithin(ctx, []string{newer, older}); len(pairs) != 1 {
				t.Fatalf("the dry run withdrew the edge: %d pair(s) remain", len(pairs))
			}

			cls := NewRelationClassifier(&fakeProvider{resp: tc.reply})
			res, withdrawn, err := Reassess(ctx, store, cls, "p", true, discardLogger())
			if err != nil {
				t.Fatalf("Reassess: %v", err)
			}
			// Which half decided it, by the counter that only that half moves.
			switch {
			case tc.byVeto:
				if res.Vetoed != 1 || cls.Calls() != 0 {
					t.Errorf("vetoed=%d calls=%d, want 1 and 0: this subcase exists to be settled by the veto", res.Vetoed, cls.Calls())
				}
			case tc.reply == "NEITHER":
				if res.Neither != 1 || cls.Calls() != 1 {
					t.Errorf("neither=%d calls=%d, want 1 and 1: the classifier verdict must be the one that settles this pair", res.Neither, cls.Calls())
				}
			default:
				if res.Reversed != 1 || cls.Calls() != 1 {
					t.Errorf("reversed=%d calls=%d, want 1 and 1", res.Reversed, cls.Calls())
				}
			}
			if res.CausesWithdrawn != 1 {
				t.Errorf("CausesWithdrawn = %d, want 1: the sweep removes a second graph row and the report has to say so", res.CausesWithdrawn)
			}
			if len(withdrawn) != 1 || withdrawn[0].CausesSwept != 1 {
				t.Errorf("withdrawn = %+v, want one row that reports the causes edge it dropped", withdrawn)
			}
			if pairs, _ := store.SupersedesWithin(ctx, []string{newer, older}); len(pairs) != 0 {
				t.Errorf("the supersedes edge survived: %d pair(s) remain", len(pairs))
			}
			causes, err := store.GetLinks(ctx, older)
			if err != nil {
				t.Fatalf("GetLinks: %v", err)
			}
			for _, l := range causes {
				if l.Relation == string(RelationCauses) {
					t.Errorf("a 'causes' link pointing INTO the still-current note survived: %+v", l)
				}
			}
			// Only the supersedes withdrawal records history: InvalidateLink
			// writes the unsupersede row for that relation alone, so the sweep
			// leaves the audit exactly as Run leaves it.
			assertUnsupersedeHistory(t, store, older)
		})
	}
}

// TestReassessLeavesACausesEdgeUnderACausesVerdict: the sweep is for verdicts
// that deny the relation, not for the one that affirms it. A CAUSES verdict
// means the older note is still true, so the supersedes edge goes — but an
// existing 'causes' edge may be recording exactly that, and deleting it would be
// the repair pass deciding something it was not asked to decide.
func TestReassessLeavesACausesEdgeUnderACausesVerdict(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	newer, older := seedEdge(t, store, db,
		"A restore that spanned two spindles took 41 minutes and the row count matched afterwards.",
		"The restore path on one spindle is safe and takes under a minute.")
	if err := store.CreateLink(ctx, older, newer, string(RelationCauses), 0.9, "llm"); err != nil {
		t.Fatal(err)
	}
	cls := NewRelationClassifier(&fakeProvider{resp: "CAUSES"})
	res, _, err := Reassess(ctx, store, cls, "p", true, discardLogger())
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if res.Causes != 1 || res.Withdrawn != 1 {
		t.Errorf("causes=%d withdrawn=%d, want 1 and 1", res.Causes, res.Withdrawn)
	}
	if res.CausesWithdrawn != 0 {
		t.Errorf("CausesWithdrawn = %d, want 0: a CAUSES verdict affirms that relation, so its row sweeps nothing", res.CausesWithdrawn)
	}
	if pairs, _ := store.SupersedesWithin(ctx, []string{newer, older}); len(pairs) != 0 {
		t.Errorf("the supersedes edge survived a CAUSES verdict: %d pair(s) remain", len(pairs))
	}
	causes, err := store.GetLinks(ctx, older)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	found := false
	for _, l := range causes {
		if l.Relation == string(RelationCauses) {
			found = true
		}
	}
	if !found {
		t.Error("a CAUSES verdict must not sweep the 'causes' edge: the verdict affirms that relation")
	}
}

// TestReassessCountersAddUpToLoaded: a report whose per-outcome numbers do not
// sum to what it read is a report an operator cannot check. Every live edge
// lands in exactly one bucket, so the buckets are the loaded count.
func TestReassessCountersAddUpToLoaded(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	// One edge per outcome: SUPERSEDES, NEITHER, CAUSES, REVERSED, and one the
	// veto settles. Only the last pair's OLDER note states an imperative, so the
	// other four reach the classifier; its newer note retires nothing, so the
	// veto fires and the harness is never asked about it.
	for i := 0; i < 5; i++ {
		olderText := "The ingest service runs Redis 6.2, revision " + string(rune('a'+i)) + "."
		if i == 4 {
			olderText = "NEVER merge on Fridays, rule " + string(rune('a'+i)) + "."
		}
		newer := add(t, store, db, "The ingest service now runs Redis 7.2, revision "+string(rune('a'+i))+".",
			[]float32{1, 0, 0, 0}, "2026-06-01 00:00:00")
		older := add(t, store, db, olderText, []float32{0.999, 0.001, 0, 0}, "2026-01-01 00:00:00")
		if err := store.CreateLink(ctx, newer, older, string(RelationSupersedes), 0.95, "llm"); err != nil {
			t.Fatal(err)
		}
	}
	devID, err := store.Create(ctx, "p", memory.Memory{Category: "fact", Content: "The development pool timeout is 5s.", Importance: 0.7, Source: "mcp", Scope: map[string]string{"environment": "development"}})
	if err != nil {
		t.Fatal(err)
	}
	prodID, err := store.Create(ctx, "p", memory.Memory{Category: "fact", Content: "The production pool timeout is 30s.", Importance: 0.7, Source: "mcp", Scope: map[string]string{"environment": "production"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateLink(ctx, prodID, devID, string(RelationSupersedes), 0.95, "llm"); err != nil {
		t.Fatal(err)
	}

	// One reply per pair, matched by the pair's own text. The fourth pair's reply
	// is deliberately unparseable, so the UNKNOWN bucket has a member too.
	replies := map[string]string{
		"The ingest service now runs Redis 7.2, revision a.": "SUPERSEDES | replaced: it runs Redis 6.2, revision a.",
		"The ingest service now runs Redis 7.2, revision b.": "NEITHER",
		"The ingest service now runs Redis 7.2, revision c.": "CAUSES",
		"The ingest service now runs Redis 7.2, revision d.": "I am not sure, maybe both?",
		// revision e's older note is an imperative the newer note does not
		// retire, so the veto settles it and the harness is never asked.
	}
	cls := NewRelationClassifier(&pairReplyProvider{byNewer: replies})
	res, _, err := Reassess(ctx, store, cls, "p", true, discardLogger())
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	total := res.Skipped + res.Vetoed + res.Confirmed + res.Neither + res.Causes + res.Reversed + res.Unclassified
	if total != res.Loaded {
		t.Errorf("per-outcome counters sum to %d but Loaded is %d (skipped=%d vetoed=%d confirmed=%d neither=%d causes=%d reversed=%d unknown=%d)",
			total, res.Loaded, res.Skipped, res.Vetoed, res.Confirmed, res.Neither, res.Causes, res.Reversed, res.Unclassified)
	}
	if res.Loaded != 6 {
		t.Errorf("Loaded = %d, want 6 (five judged-or-vetoed edges plus one scope-conflicting)", res.Loaded)
	}
	if res.Vetoed != 1 {
		t.Errorf("Vetoed = %d, want 1", res.Vetoed)
	}
	if res.Neither != 1 {
		t.Errorf("Neither = %d, want 1: the counter exists so the per-outcome numbers add up", res.Neither)
	}
	if res.Unclassified != 1 {
		t.Errorf("Unclassified = %d, want 1", res.Unclassified)
	}
	if res.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1 (the scope-conflicting edge)", res.Skipped)
	}
}
