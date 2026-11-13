package main

import (
	"testing"

	"github.com/wcatz/ghost/internal/reflection"
)

// TestReplacedIDsReachTheKeptSuccessorOfASupersession: a drop that names a
// successor says that row replaces the dropped one, so the successor claims it.
// The successor is the row the drop NAMED, so the join is keyed on its id and
// not on its text.
//
// Without the claim the dropped row was accounted for by nothing at all: the
// replace deleted it as a row no emission carried, its delete history named no
// successor, and a decision companion's link — the only thing a later supersede
// finds the companion by — died with it.
func TestReplacedIDsReachTheKeptSuccessorOfASupersession(t *testing.T) {
	result := &reflection.ReflectionResult{
		Drops: []reflection.Drop{{ID: "dropped", Reason: "superseded by", Successor: "KEPT"}},
		Memories: []reflection.ReflectMemory{
			{Category: "fact", Content: "the surviving row", RestatesID: "KEPT"},
		},
	}
	rows := reflectMemoriesToMemory("p1", result.Memories, replacedIDsFor(result))
	if len(rows) != 1 {
		t.Fatalf("converted %d rows, want 1", len(rows))
	}
	if len(rows[0].ReplacesIDs) != 1 || rows[0].ReplacesIDs[0] != "dropped" {
		t.Errorf("ReplacesIDs = %v, want the dropped row the successor replaces", rows[0].ReplacesIDs)
	}
}

// TestReplacedIDsLeaveAKeepNoDropNamesAlone: the join is keyed on the
// successor's ID, so a keep no drop names takes nothing from it — even when
// its text is the text a merge produced.
//
// Joining it by text instead would hand the keep the merge's sources, and the
// replace would read the row's own keep as a claim on rows the merge named:
// when the merge was then refused, the keep was dropped as a claim on a row
// that changed while consolidation ran, and the round reported a concurrent
// edit nobody made.
func TestReplacedIDsLeaveAKeepNoDropNamesAlone(t *testing.T) {
	const text = "one consolidated statement of the rule"
	result := &reflection.ReflectionResult{
		Merges: []reflection.Merge{{IDs: []string{"m1", "m2"}, Text: text}},
		Memories: []reflection.ReflectMemory{
			{Category: "fact", Content: text, RestatesID: "kept"},
		},
	}
	rows := reflectMemoriesToMemory("p1", result.Memories, replacedIDsFor(result))
	if len(rows) != 1 {
		t.Fatalf("converted %d rows, want 1", len(rows))
	}
	if len(rows[0].ReplacesIDs) != 0 {
		t.Errorf("ReplacesIDs = %v, want none — a keep no drop names consumed nothing", rows[0].ReplacesIDs)
	}
}
