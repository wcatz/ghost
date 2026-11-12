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

type keepVerdictClassifier struct{ calls int }

func (k *keepVerdictClassifier) IsResolvedBatch(_ context.Context, contents []string) ([]resolve.Verdict, error) {
	k.calls++
	out := make([]resolve.Verdict, len(contents))
	for i := range out {
		out[i] = resolve.VerdictKeep
	}
	return out, nil
}

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

func assertUnsupersedeHistory(t *testing.T, store *memory.Store, olderID string) {
	t.Helper()
	entries, err := store.MemoryHistory(context.Background(), olderID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Phase == "unsupersede" {
			if e.RelatedID == "" {
				t.Errorf("unsupersede row missing RelatedID: %+v", e)
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

func TestReassessWithdrawsAnEdgeThatComesBackNeither(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	newer, older := seedEdge(t, store, db,
		"A restore that spanned two spindles took 41 minutes and the row count matched afterwards.",
		"The restore path on one spindle is safe and takes under a minute.")
	dry := NewRelationClassifier(&fakeProvider{resp: "NEITHER"})
	res, withdrawn, err := Reassess(ctx, store, dry, "p", false, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if res.Loaded != 1 || res.Withdrawn != 0 || dry.Calls() != 1 || len(withdrawn) != 1 || withdrawn[0].Written {
		t.Fatalf("dry: res=%+v withdrawn=%+v calls=%d", res, withdrawn, dry.Calls())
	}
	if pairs, _ := store.SupersedesWithin(ctx, []string{newer, older}); len(pairs) != 1 {
		t.Fatalf("dry invalidated edge: %d", len(pairs))
	}
	apply := NewRelationClassifier(&fakeProvider{resp: "NEITHER"})
	res, withdrawn, err = Reassess(ctx, store, apply, "p", true, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if res.Withdrawn != 1 || len(withdrawn) != 1 || !withdrawn[0].Written {
		t.Fatalf("apply: res=%+v withdrawn=%+v", res, withdrawn)
	}
	if pairs, _ := store.SupersedesWithin(ctx, []string{newer, older}); len(pairs) != 0 {
		t.Fatalf("edge survived: %d", len(pairs))
	}
	assertUnsupersedeHistory(t, store, older)
	third := NewRelationClassifier(&fakeProvider{resp: "NEITHER"})
	res, withdrawn, err = Reassess(ctx, store, third, "p", true, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if res.Loaded != 0 || res.Withdrawn != 0 || len(withdrawn) != 0 || third.Calls() != 0 {
		t.Fatalf("third: res=%+v withdrawn=%+v calls=%d", res, withdrawn, third.Calls())
	}
}

func TestReassessWithdrawsAnEdgeTheVetoSettles(t *testing.T) { // #686
	store, db := seed(t)
	ctx := context.Background()
	_, older := seedEdge(t, store, db,
		"The restore path was rewritten last month; the timings below are from the new implementation.",
		"NEVER run the restore with source and target on the same spindle.")
	cls := NewRelationClassifier(&fakeProvider{resp: "SUPERSEDES | replaced: the restore is unsafe on one spindle"})
	res, withdrawn, err := Reassess(ctx, store, cls, "p", true, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if cls.Calls() != 0 || res.Vetoed != 1 || res.Withdrawn != 1 || len(withdrawn) != 1 || !strings.Contains(withdrawn[0].Reason, "vetoed") {
		t.Fatalf("calls=%d res=%+v withdrawn=%+v", cls.Calls(), res, withdrawn)
	}
	if pairs, _ := store.SupersedesWithin(ctx, []string{older}); len(pairs) != 0 {
		t.Fatalf("edge survived: %d", len(pairs))
	}
	assertUnsupersedeHistory(t, store, older)
}

func TestReassessKeepsAnEdgeTheCurrentRulesStillSupport(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	newer, older := seedEdge(t, store, db,
		"The ingest service now runs Redis 7.2: the compose pin moved to 7.2 in the upgrade.",
		"The ingest service runs Redis 6.2, pinned in the compose file.")
	cls := NewRelationClassifier(&fakeProvider{resp: "SUPERSEDES | replaced: it runs Redis 6.2"})
	res, withdrawn, err := Reassess(ctx, store, cls, "p", true, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if res.Confirmed != 1 || res.Withdrawn != 0 || len(withdrawn) != 0 {
		t.Fatalf("res=%+v withdrawn=%+v", res, withdrawn)
	}
	if pairs, _ := store.SupersedesWithin(ctx, []string{newer, older}); len(pairs) != 1 {
		t.Fatalf("pairs=%d", len(pairs))
	}
}

func TestReassessWithdrawsEdgesTheVerdictRefuses(t *testing.T) {
	for _, tc := range []struct {
		name, reply, wantRe string
	}{
		{name: "causes", reply: "CAUSES", wantRe: "causes"},
		{name: "reversed", reply: "REVERSED", wantRe: "reversed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, db := seed(t)
			ctx := context.Background()
			newer, older := seedEdge(t, store, db,
				"A restore that spanned two spindles took 41 minutes and the row count matched afterwards.",
				"The restore path on one spindle is safe and takes under a minute.")
			res, withdrawn, err := Reassess(ctx, store, NewRelationClassifier(&fakeProvider{resp: tc.reply}), "p", true, discardLogger())
			if err != nil {
				t.Fatal(err)
			}
			if res.Withdrawn != 1 || len(withdrawn) != 1 || !strings.Contains(withdrawn[0].Reason, tc.wantRe) || withdrawn[0].NewerID != newer || withdrawn[0].OlderID != older {
				t.Fatalf("res=%+v withdrawn=%+v", res, withdrawn)
			}
			if pairs, _ := store.SupersedesWithin(ctx, []string{newer, older}); len(pairs) != 0 {
				t.Fatalf("edge survived: %d", len(pairs))
			}
			assertUnsupersedeHistory(t, store, older)
		})
	}
}

func TestReassessLeavesAnUnparseableVerdictAlone(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	newer, older := seedEdge(t, store, db,
		"A restore that spanned two spindles took 41 minutes and the row count matched afterwards.",
		"The restore path on one spindle is safe and takes under a minute.")
	res, withdrawn, err := Reassess(ctx, store, NewRelationClassifier(&fakeProvider{resp: "I am not sure, maybe both?"}), "p", true, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if res.Unclassified != 1 || res.Withdrawn != 0 || len(withdrawn) != 0 {
		t.Fatalf("res=%+v withdrawn=%+v", res, withdrawn)
	}
	if pairs, _ := store.SupersedesWithin(ctx, []string{newer, older}); len(pairs) != 1 {
		t.Fatalf("pairs=%d", len(pairs))
	}
}

func TestReassessLeavesAScopeConflictingEdgeAlone(t *testing.T) {
	store, _ := seed(t)
	ctx := context.Background()
	olderID, err := store.Create(ctx, "p", memory.Memory{Category: "fact", Content: "The development profile's pool timeout is 5 seconds.", Importance: 0.7, Source: "mcp", Scope: map[string]string{"environment": "development"}})
	if err != nil {
		t.Fatal(err)
	}
	newerID, err := store.Create(ctx, "p", memory.Memory{Category: "fact", Content: "The production profile's pool timeout is 30 seconds.", Importance: 0.7, Source: "mcp", Scope: map[string]string{"environment": "production"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateLink(ctx, newerID, olderID, string(RelationSupersedes), 0.95, "llm"); err != nil {
		t.Fatal(err)
	}
	cls := NewRelationClassifier(&fakeProvider{resp: "NEITHER"})
	res, withdrawn, err := Reassess(ctx, store, cls, "p", true, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if cls.Calls() != 0 || res.Skipped != 1 || res.Withdrawn != 0 || len(withdrawn) != 0 {
		t.Fatalf("calls=%d res=%+v withdrawn=%+v", cls.Calls(), res, withdrawn)
	}
	links, err := store.LinksByRelationSource(ctx, "p", string(RelationSupersedes), "llm")
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 {
		t.Fatalf("links=%d", len(links))
	}
}

func TestReassessResolvesAChainIntoResolveReassess(t *testing.T) { // #686
	store, db := seed(t)
	ctx := context.Background()
	newer, older := seedEdge(t, store, db,
		"A restore that spanned two spindles took 41 minutes and the row count matched afterwards.",
		"The restore path on one spindle is safe and takes under a minute.")
	if n, err := store.SetResolved(ctx, []string{older}); err != nil || n != 1 {
		t.Fatalf("SetResolved=%d err=%v", n, err)
	}
	keep := &keepVerdictClassifier{}
	before, reKept, err := resolve.Reassess(ctx, store, keep, "p", true, resolve.Scope{}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if before.Demoted != 1 || before.Cleared != 0 || len(reKept) != 0 || keep.calls != 0 || !isResolved(t, store, older) {
		t.Fatalf("before: demoted=%d cleared=%d reKept=%d calls=%d resolved=%v", before.Demoted, before.Cleared, len(reKept), keep.calls, isResolved(t, store, older))
	}
	res, _, err := Reassess(ctx, store, NewRelationClassifier(&fakeProvider{resp: "NEITHER"}), "p", true, discardLogger())
	if err != nil || res.Withdrawn != 1 {
		t.Fatalf("reassess: res=%+v err=%v", res, err)
	}
	keep2 := &keepVerdictClassifier{}
	after, reKept, err := resolve.Reassess(ctx, store, keep2, "p", true, resolve.Scope{}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if after.Demoted != 0 || after.Cleared != 1 || len(reKept) != 1 || keep2.calls != 1 || reKept[0].ID != older {
		t.Fatalf("after: demoted=%d cleared=%d reKept=%+v calls=%d", after.Demoted, after.Cleared, reKept, keep2.calls)
	}
	if isResolved(t, store, older) || isResolved(t, store, newer) {
		t.Fatalf("resolved older=%v newer=%v", isResolved(t, store, older), isResolved(t, store, newer))
	}
	assertUnsupersedeHistory(t, store, older)
}

func TestReassessHoldsTheNoteWhileAnySupersedesEdgeSurvives(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	older := add(t, store, db, "The restore path on one spindle is safe and takes under a minute.", []float32{0, 1, 0}, "2026-01-01 00:00:00")
	newerA := add(t, store, db, "A restore that spanned two spindles took 41 minutes and the row count matched afterwards.", []float32{1, 0, 0}, "2026-06-01 00:00:00")
	newerB := add(t, store, db, "A second restore crossed two spindles, the row count matched again, and the one-spindle timing is no longer representative.", []float32{1, 0, 0.1}, "2026-07-01 00:00:00")
	for _, newer := range []string{newerA, newerB} {
		if err := store.CreateLink(ctx, newer, older, string(RelationSupersedes), 0.95, "llm"); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := store.SetResolved(ctx, []string{older}); err != nil || n != 1 || !isResolved(t, store, older) {
		t.Fatalf("SetResolved=%d err=%v resolved=%v", n, err, isResolved(t, store, older))
	}
	if n, err := store.InvalidateLink(ctx, newerA, older, string(RelationSupersedes)); err != nil || n != 1 {
		t.Fatalf("InvalidateLink=%d err=%v", n, err)
	}
	if links := liveSupersedesInto(t, store, older); len(links) != 1 {
		t.Fatalf("links=%d", len(links))
	}
	keep := &keepVerdictClassifier{}
	held, reKept, err := resolve.Reassess(ctx, store, keep, "p", true, resolve.Scope{}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if held.Demoted != 1 || held.Cleared != 0 || len(reKept) != 0 || keep.calls != 0 || !isResolved(t, store, older) {
		t.Fatalf("held: demoted=%d cleared=%d reKept=%d calls=%d resolved=%v", held.Demoted, held.Cleared, len(reKept), keep.calls, isResolved(t, store, older))
	}
	if n, err := store.InvalidateLink(ctx, newerB, older, string(RelationSupersedes)); err != nil || n != 1 {
		t.Fatalf("InvalidateLink=%d err=%v", n, err)
	}
	if links := liveSupersedesInto(t, store, older); len(links) != 0 {
		t.Fatalf("links=%d", len(links))
	}
	keep2 := &keepVerdictClassifier{}
	released, reKept2, err := resolve.Reassess(ctx, store, keep2, "p", true, resolve.Scope{}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if released.Demoted != 0 || released.Cleared != 1 || len(reKept2) != 1 || keep2.calls != 1 || reKept2[0].ID != older || isResolved(t, store, older) {
		t.Fatalf("released: demoted=%d cleared=%d reKept=%+v calls=%d resolved=%v", released.Demoted, released.Cleared, reKept2, keep2.calls, isResolved(t, store, older))
	}
	assertUnsupersedeHistory(t, store, older)
}

func liveSupersedesInto(t *testing.T, store *memory.Store, id string) []memory.Link {
	t.Helper()
	links, err := store.LinksByRelationSource(context.Background(), "p", "supersedes", "llm")
	if err != nil {
		t.Fatal(err)
	}
	var out []memory.Link
	for _, l := range links {
		if l.TargetID == id {
			out = append(out, l)
		}
	}
	return out
}

func isResolved(t *testing.T, store *memory.Store, id string) bool {
	t.Helper()
	mems, err := store.GetByIDs(context.Background(), []string{id})
	if err != nil || len(mems) != 1 {
		t.Fatalf("GetByIDs=%d err=%v", len(mems), err)
	}
	return mems[0].ResolvedAt != nil
}
