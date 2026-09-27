package supersede

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/resolve"
)

// keepVerdictClassifier answers every resolve question KEEP, which is the answer
// that makes `ghost resolve --reassess` clear a resolved_at. It stands in for
// the real resolve classifier in the chain test below.
type keepVerdictClassifier struct{ calls int }

func (k *keepVerdictClassifier) IsResolvedBatch(_ context.Context, contents []string) ([]resolve.Verdict, error) {
	k.calls++
	out := make([]resolve.Verdict, len(contents))
	for i := range out {
		out[i] = resolve.VerdictKeep
	}
	return out, nil
}

// seedEdge writes a live 'supersedes'/'llm' edge newer→older over two memories
// and returns their ids, backdating the link so a reclassify would fire and
// calling CreateLink itself so the edge is a real graph row.
func seedEdge(t *testing.T, store *memory.Store, db *sql.DB, newerText, olderText string) (newer, older string) {
	t.Helper()
	ctx := context.Background()
	newer = add(t, store, db, newerText, []float32{1, 0, 0, 0}, "2026-06-01 00:00:00")
	older = add(t, store, db, olderText, []float32{0, 1, 0, 0}, "2026-01-01 00:00:00")
	if err := store.CreateLink(ctx, newer, older, string(RelationSupersedes), 0.95, "llm"); err != nil {
		t.Fatal(err)
	}
	return newer, older
}

// assertUnsupersedeHistory checks the audit row the withdrawal has to leave. A
// corpus whose history shows a supersession and no withdrawal reads as though the
// stale claim is still live, so the row is part of what "withdrawn" means — not
// a side effect of it.
func assertUnsupersedeHistory(t *testing.T, store *memory.Store, olderID string) {
	t.Helper()
	entries, err := store.MemoryHistory(context.Background(), olderID, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	for _, e := range entries {
		if e.Phase == "unsupersede" {
			if e.RelatedID == "" {
				t.Errorf("the unsupersede row names no superseding memory: %+v", e)
			}
			return
		}
	}
	t.Errorf("no unsupersede history row for %s; phases: %v", olderID, phases(entries))
}

func phases(entries []memory.HistoryEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Phase)
	}
	return out
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestReassessWithdrawsAnEdgeThatComesBackNeither: the repair pass re-judges a
// live edge with the current rules and, with --apply, invalidates the ones the
// current rules no longer support. The invalidation is the ordinary store path,
// so it writes the `unsupersede` history row — a corpus whose audit shows a
// supersession and no withdrawal reads as though the stale claim is still live.
func TestReassessWithdrawsAnEdgeThatComesBackNeither(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	newer, older := seedEdge(t, store, db,
		"A restore that spanned two spindles took 41 minutes and the row count matched afterwards.",
		"The restore path on one spindle is safe and takes under a minute.")

	// Dry run first: the report names the edge and writes nothing.
	dry := NewRelationClassifier(&fakeProvider{resp: "NEITHER"})
	res, withdrawn, err := Reassess(ctx, store, dry, "p", false, discardLogger())
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if res.Loaded != 1 || res.Withdrawn != 0 {
		t.Errorf("dry run: loaded=%d withdrawn=%d, want 1 and 0", res.Loaded, res.Withdrawn)
	}
	if dry.Calls() != 1 {
		t.Errorf("dry run made %d call(s), want 1", dry.Calls())
	}
	if len(withdrawn) != 1 || withdrawn[0].NewerID != newer || withdrawn[0].OlderID != older {
		t.Fatalf("dry run withdrawn = %+v, want the seeded edge", withdrawn)
	}
	if withdrawn[0].Written {
		t.Error("a dry run must not report a withdrawal it did not make")
	}
	if pairs, _ := store.SupersedesWithin(ctx, []string{newer, older}); len(pairs) != 1 {
		t.Fatalf("dry run invalidated the edge: %d pair(s) remain", len(pairs))
	}

	apply := NewRelationClassifier(&fakeProvider{resp: "NEITHER"})
	res, withdrawn, err = Reassess(ctx, store, apply, "p", true, discardLogger())
	if err != nil {
		t.Fatalf("Reassess (apply): %v", err)
	}
	if res.Withdrawn != 1 || len(withdrawn) != 1 || !withdrawn[0].Written {
		t.Errorf("apply: withdrawn=%d listed=%d written=%v, want 1, 1, true",
			res.Withdrawn, len(withdrawn), len(withdrawn) == 1 && withdrawn[0].Written)
	}
	if pairs, _ := store.SupersedesWithin(ctx, []string{newer, older}); len(pairs) != 0 {
		t.Errorf("the edge survived an apply: %d pair(s) remain", len(pairs))
	}
	assertUnsupersedeHistory(t, store, older)
	// Convergent: a second run has no live edge to judge, so it neither calls
	// nor reports a withdrawal.
	third := NewRelationClassifier(&fakeProvider{resp: "NEITHER"})
	res, withdrawn, err = Reassess(ctx, store, third, "p", true, discardLogger())
	if err != nil {
		t.Fatalf("Reassess (third): %v", err)
	}
	if res.Loaded != 0 || res.Withdrawn != 0 || len(withdrawn) != 0 || third.Calls() != 0 {
		t.Errorf("third run: loaded=%d withdrawn=%d listed=%d calls=%d, want 0,0,0,0 — a withdrawn edge is gone, not re-withdrawn",
			res.Loaded, res.Withdrawn, len(withdrawn), third.Calls())
	}
}

// TestReassessWithdrawsAnEdgeTheVetoSettles: the deterministic veto is part of
// the current rules, so an edge whose older note states a rule the newer note
// never retires is withdrawn without a harness call. That is the exact edge
// #686's judge found demoting a "must" rule.
func TestReassessWithdrawsAnEdgeTheVetoSettles(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	_, older := seedEdge(t, store, db,
		"The restore path was rewritten last month; the timings below are from the new implementation.",
		"NEVER run the restore with source and target on the same spindle.")

	cls := NewRelationClassifier(&fakeProvider{resp: "SUPERSEDES | replaced: the restore is unsafe on one spindle"})
	res, withdrawn, err := Reassess(ctx, store, cls, "p", true, discardLogger())
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if cls.Calls() != 0 {
		t.Errorf("classify calls = %d, want 0: the veto settles the edge for free", cls.Calls())
	}
	if res.Vetoed != 1 || res.Withdrawn != 1 {
		t.Errorf("vetoed=%d withdrawn=%d, want 1 and 1", res.Vetoed, res.Withdrawn)
	}
	if len(withdrawn) != 1 || !strings.Contains(withdrawn[0].Reason, "vetoed") {
		t.Errorf("withdrawn = %+v, want one entry whose reason says the veto settled it", withdrawn)
	}
	if pairs, _ := store.SupersedesWithin(ctx, []string{older}); len(pairs) != 0 {
		t.Errorf("a vetoed edge must not survive the repair: %d pair(s) remain", len(pairs))
	}
	assertUnsupersedeHistory(t, store, older)
}

// TestReassessKeepsAnEdgeTheCurrentRulesStillSupport: the repair pass is not a
// deletion sweep. A true supersession that comes back SUPERSEDES keeps its edge
// and is not re-written, and the pass reports it, so a dry run is an auditable
// list of what would change.
func TestReassessKeepsAnEdgeTheCurrentRulesStillSupport(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	newer, older := seedEdge(t, store, db,
		"The ingest service now runs Redis 7.2: the compose pin moved to 7.2 in the upgrade.",
		"The ingest service runs Redis 6.2, pinned in the compose file.")

	cls := NewRelationClassifier(&fakeProvider{resp: "SUPERSEDES | replaced: it runs Redis 6.2"})
	res, withdrawn, err := Reassess(ctx, store, cls, "p", true, discardLogger())
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if res.Confirmed != 1 || res.Withdrawn != 0 || len(withdrawn) != 0 {
		t.Errorf("confirmed=%d withdrawn=%d listed=%d, want 1, 0, 0", res.Confirmed, res.Withdrawn, len(withdrawn))
	}
	if pairs, _ := store.SupersedesWithin(ctx, []string{newer, older}); len(pairs) != 1 {
		t.Errorf("a supported edge must survive the repair: %d pair(s) remain", len(pairs))
	}
}

// TestReassessWithdrawsEdgesTheVerdictRefuses: a CAUSES or REVERSED verdict
// denies that the newer note replaced the older one, so the edge asserts what
// the verdict just refused. Both go, each under its own reason.
//
// The pass writes no 'causes' link in their place: it is a repair pass, and the
// ordinary pass is where a causes edge is created — on the next ordinary run
// these pairs come back as fresh candidates and earn one.
func TestReassessWithdrawsEdgesTheVerdictRefuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reply  string
		wantRe string
		count  string
	}{
		{name: "causes", reply: "CAUSES", wantRe: "causes", count: "Causes"},
		{name: "reversed", reply: "REVERSED", wantRe: "reversed", count: "Reversed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, db := seed(t)
			ctx := context.Background()
			newer, older := seedEdge(t, store, db,
				"A restore that spanned two spindles took 41 minutes and the row count matched afterwards.",
				"The restore path on one spindle is safe and takes under a minute.")
			cls := NewRelationClassifier(&fakeProvider{resp: tc.reply})
			res, withdrawn, err := Reassess(ctx, store, cls, "p", true, discardLogger())
			if err != nil {
				t.Fatalf("Reassess: %v", err)
			}
			if res.Withdrawn != 1 || len(withdrawn) != 1 {
				t.Fatalf("withdrawn=%d listed=%d, want 1 and 1", res.Withdrawn, len(withdrawn))
			}
			if !strings.Contains(withdrawn[0].Reason, tc.wantRe) {
				t.Errorf("reason %q does not name the %s verdict", withdrawn[0].Reason, tc.wantRe)
			}
			if withdrawn[0].NewerID != newer || withdrawn[0].OlderID != older {
				t.Errorf("withdrawn = %+v, want the seeded edge %s→%s", withdrawn[0], newer, older)
			}
			if pairs, _ := store.SupersedesWithin(ctx, []string{newer, older}); len(pairs) != 0 {
				t.Errorf("a refused verdict must not leave the edge: %d pair(s) remain", len(pairs))
			}
			assertUnsupersedeHistory(t, store, older)
		})
	}
}

// TestReassessLeavesAnUnparseableVerdictAlone: a garbled reply is a missing
// judgment, not a denial of one. The edge stays and the pass reports the pair as
// unclassified, so a later pass can ask again — the same treatment Run gives an
// unclassifiable candidate.
func TestReassessLeavesAnUnparseableVerdictAlone(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	newer, older := seedEdge(t, store, db,
		"A restore that spanned two spindles took 41 minutes and the row count matched afterwards.",
		"The restore path on one spindle is safe and takes under a minute.")

	cls := NewRelationClassifier(&fakeProvider{resp: "I am not sure, maybe both?"})
	res, withdrawn, err := Reassess(ctx, store, cls, "p", true, discardLogger())
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if res.Unclassified != 1 || res.Withdrawn != 0 || len(withdrawn) != 0 {
		t.Errorf("unclassified=%d withdrawn=%d listed=%d, want 1, 0, 0", res.Unclassified, res.Withdrawn, len(withdrawn))
	}
	if pairs, _ := store.SupersedesWithin(ctx, []string{newer, older}); len(pairs) != 1 {
		t.Errorf("a garbled reply must not withdraw an edge: %d pair(s) remain", len(pairs))
	}
}

// TestReassessLeavesAScopeConflictingEdgeAlone: a scope-conflicting edge asserts
// no replacement, and the ordinary pass leaves it in the graph to be exempted at
// read time. The repair pass holds the same line, because withdrawing it would
// be deleting graph history over a rule that is about which pairs may be linked,
// not about whether this one was a supersession.
func TestReassessLeavesAScopeConflictingEdgeAlone(t *testing.T) {
	store, _ := seed(t)
	ctx := context.Background()
	dev := "The development profile's pool timeout is 5 seconds."
	prod := "The production profile's pool timeout is 30 seconds."
	olderID, err := store.Create(ctx, "p", memory.Memory{Category: "fact", Content: dev, Importance: 0.7, Source: "mcp", Scope: map[string]string{"environment": "development"}})
	if err != nil {
		t.Fatal(err)
	}
	newerID, err := store.Create(ctx, "p", memory.Memory{Category: "fact", Content: prod, Importance: 0.7, Source: "mcp", Scope: map[string]string{"environment": "production"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateLink(ctx, newerID, olderID, string(RelationSupersedes), 0.95, "llm"); err != nil {
		t.Fatal(err)
	}

	cls := NewRelationClassifier(&fakeProvider{resp: "NEITHER"})
	res, withdrawn, err := Reassess(ctx, store, cls, "p", true, discardLogger())
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if cls.Calls() != 0 {
		t.Errorf("classify calls = %d, want 0: a scope-conflicting pair is not judged at all", cls.Calls())
	}
	if res.Skipped != 1 || res.Withdrawn != 0 || len(withdrawn) != 0 {
		t.Errorf("skipped=%d withdrawn=%d listed=%d, want 1, 0, 0", res.Skipped, res.Withdrawn, len(withdrawn))
	}
	links, err := store.LinksByRelationSource(ctx, "p", string(RelationSupersedes), "llm")
	if err != nil {
		t.Fatalf("LinksByRelationSource: %v", err)
	}
	if len(links) != 1 {
		t.Errorf("a scope-conflicting edge must be left in the graph, got %d live link(s)", len(links))
	}
}

// TestReassessResolvesAChainIntoResolveReassess is the chain #686 needs. A
// supersedes edge is not informational: `ghost resolve`'s supersedes piggyback
// stamps resolved_at on the older endpoint for free, and its repair pass
// deliberately HONOURS a live edge as a floor. So a wrong edge buries a memory
// twice over, and withdrawing the edge is only half the repair — the resolution
// it caused has to be clearable afterwards, or the memory stays out of
// injection anyway.
func TestReassessResolvesAChainIntoResolveReassess(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	newer, older := seedEdge(t, store, db,
		"A restore that spanned two spindles took 41 minutes and the row count matched afterwards.",
		"The restore path on one spindle is safe and takes under a minute.")
	// The piggyback's effect: the older endpoint is stamped resolved.
	if n, err := store.SetResolved(ctx, []string{older}); err != nil || n != 1 {
		t.Fatalf("SetResolved = %d, %v", n, err)
	}

	// Control: while the edge is live, resolve's repair pass treats the row as
	// still asserted and refuses to clear it — so nothing else in this chain can
	// be what puts the memory back.
	keep := &keepVerdictClassifier{}
	before, reKept, err := resolve.Reassess(ctx, store, keep, "p", true, discardLogger())
	if err != nil {
		t.Fatalf("resolve.Reassess (before): %v", err)
	}
	if before.Demoted != 1 || before.Cleared != 0 || len(reKept) != 0 {
		t.Fatalf("before the withdrawal: asserted=%d cleared=%d reKept=%d, want 1, 0, 0 — a live edge is resolve's floor",
			before.Demoted, before.Cleared, len(reKept))
	}
	if isResolved(t, store, older) == false {
		t.Fatal("the seeded memory is not resolved, so the chain has nothing to clear")
	}

	// Withdraw the edge: it comes back NEITHER under the current rules.
	cls := NewRelationClassifier(&fakeProvider{resp: "NEITHER"})
	res, _, err := Reassess(ctx, store, cls, "p", true, discardLogger())
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if res.Withdrawn != 1 {
		t.Fatalf("withdrawn = %d, want 1", res.Withdrawn)
	}

	// The half that is easy to miss: with the edge gone, resolve's repair pass
	// no longer has a floor to hold the row back, so the same KEEP verdict now
	// clears it and the memory returns to ranked injection.
	keep2 := &keepVerdictClassifier{}
	after, reKept, err := resolve.Reassess(ctx, store, keep2, "p", true, discardLogger())
	if err != nil {
		t.Fatalf("resolve.Reassess (after): %v", err)
	}
	if after.Demoted != 0 {
		t.Errorf("after the withdrawal: asserted=%d, want 0 — a withdrawn edge asserts nothing", after.Demoted)
	}
	if after.Cleared != 1 || len(reKept) != 1 {
		t.Errorf("after the withdrawal: cleared=%d reKept=%d, want 1 and 1", after.Cleared, len(reKept))
	}
	if keep2.calls != 1 {
		t.Errorf("the repair pass made %d classify call(s), want 1: with the edge gone the row is a question again", keep2.calls)
	}
	if reKept[0].ID != older {
		t.Errorf("reKept = %s, want the withdrawn edge's older endpoint %s", reKept[0].ID, older)
	}
	if isResolved(t, store, older) {
		t.Error("the memory is still resolved: the edge it was buried by is gone, so it must be back in injection")
	}
	if isResolved(t, store, newer) {
		t.Error("the newer endpoint was never stamped; the repair must not have invented one")
	}
	// The control pass must not have asked the model at all: the live edge is
	// resolve's floor, so the row never reaches the classifier, and the second
	// pass below is the one whose answer clears it. A first pass that classified
	// the row would mean the edge was not holding it back, and the after-state
	// below would prove nothing.
	if keep.calls != 0 {
		t.Errorf("the control pass made %d classify call(s), want 0: a live supersedes edge is resolve's floor, not a question", keep.calls)
	}
	assertUnsupersedeHistory(t, store, older)
}

// isResolved reports whether the memory carries a resolved_at, read back through
// the store so the answer is the persisted one.
func isResolved(t *testing.T, store *memory.Store, id string) bool {
	t.Helper()
	mems, err := store.GetByIDs(context.Background(), []string{id})
	if err != nil {
		t.Fatalf("GetByIDs(%s): %v", id, err)
	}
	if len(mems) != 1 {
		t.Fatalf("GetByIDs(%s) = %d row(s)", id, len(mems))
	}
	return mems[0].ResolvedAt != nil
}
