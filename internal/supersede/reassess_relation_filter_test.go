package supersede

import (
	"context"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// #686: sweep is scoped to the 'causes' relation.
func TestReassessSweepIsScopedToTheCausesRelation(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	newerA, olderA := seedEdge(t, store, db,
		"A restore that spanned two spindles took 41 minutes and the row count matched afterwards.",
		"The restore path on one spindle is safe and takes under a minute.")
	if err := store.CreateLink(ctx, olderA, newerA, string(RelationCauses), 0.9, "llm"); err != nil {
		t.Fatal(err)
	}
	newerB, olderB := seedEdge(t, store, db,
		"The nightly compaction finishes in 12 minutes on the primary.",
		"The nightly compaction takes under an hour on the primary.")
	if err := store.CreateLink(ctx, olderB, newerB, "related", 0.9, "auto"); err != nil {
		t.Fatal(err)
	}
	dry, dryRows, err := Reassess(ctx, store, NewRelationClassifier(&fakeProvider{resp: "NEITHER"}), "p", false, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if dry.Loaded != 2 || len(dryRows) != 2 || dry.CausesWithdrawn != 1 {
		t.Fatalf("dry: loaded=%d rows=%d causes=%d", dry.Loaded, len(dryRows), dry.CausesWithdrawn)
	}
	if sweptByRow(dryRows)[olderA] != 1 || sweptByRow(dryRows)[olderB] != 0 {
		t.Fatalf("dry rows %+v", dryRows)
	}
	res, rows, err := Reassess(ctx, store, NewRelationClassifier(&fakeProvider{resp: "NEITHER"}), "p", true, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if res.CausesWithdrawn != 1 || len(rows) != 2 {
		t.Fatalf("apply: causes=%d rows=%d", res.CausesWithdrawn, len(rows))
	}
	for _, r := range rows {
		if !r.Written {
			t.Fatalf("row not written: %+v", r)
		}
	}
	if sweptByRow(rows)[olderA] != 1 || sweptByRow(rows)[olderB] != 0 {
		t.Fatalf("rows %+v", rows)
	}
	if liveRelations(t, store, olderA)[string(RelationCauses)] {
		t.Error("causes edge still live")
	}
	if liveRelations(t, store, olderB)[string(RelationSupersedes)] {
		t.Error("supersedes edge still live")
	}
	if !liveRelations(t, store, olderB)["related"] {
		t.Error("related edge removed")
	}
}

func sweptByRow(rows []WithdrawnEdge) map[string]int {
	out := map[string]int{}
	for _, r := range rows {
		out[r.OlderID] = r.CausesSwept
	}
	return out
}

// liveRelations is used by other test files in this package.
func liveRelations(t *testing.T, store *memory.Store, id string) map[string]bool {
	t.Helper()
	links, err := store.GetLinks(context.Background(), id)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	out := map[string]bool{}
	for _, l := range links {
		out[l.Relation] = true
	}
	return out
}
