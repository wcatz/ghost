package main

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/reflection"
)

// TestReplacedIDsSurviveTheContentClamp: the join between the operation list and
// the emitted memories is keyed on text, and the emissions are clamped BEFORE
// the map is built. Keying on the operation list's own text therefore matched
// nothing for an emission over memory.MaxContentLen — and the emissions that size
// are the wide merges, the ones with the most ids to point at. The pointer is the
// only thing that follows one memory's history into its successor's, so losing it
// silently is the whole feature going quiet on exactly the case it exists for.
func TestReplacedIDsSurviveTheContentClamp(t *testing.T) {
	overCap := strings.Repeat("a long consolidated fact about the pipeline. ", 400) // comfortably over 8000 bytes
	if len(overCap) <= memory.MaxContentLen {
		t.Fatalf("fixture is %d bytes, which does not exceed the %d cap", len(overCap), memory.MaxContentLen)
	}
	result := &reflection.ReflectionResult{
		Merges: []reflection.Merge{{IDs: []string{"m1", "m2"}, Text: overCap}},
		Memories: []reflection.ReflectMemory{
			{Category: "fact", Content: overCap, Importance: 0.5},
		},
	}

	// What the reflect path does, in the order it does it: clamp the emissions,
	// then build the map, then convert.
	emissions := append([]reflection.ReflectMemory(nil), result.Memories...)
	if cut := clampReflectMemories(emissions); cut == 0 {
		t.Fatal("clampReflectMemories did not clamp the over-cap emission; the fixture does not test the case")
	}
	rows := reflectMemoriesToMemory("p1", emissions, replacedIDsFor(result))
	if len(rows) != 1 {
		t.Fatalf("converted %d rows, want 1", len(rows))
	}
	if len(rows[0].ReplacesIDs) != 2 {
		t.Fatalf("ReplacesIDs = %v, want both ids the merge consumed — the clamp broke the join", rows[0].ReplacesIDs)
	}
	if rows[0].ReplacesIDs[0] != "m1" || rows[0].ReplacesIDs[1] != "m2" {
		t.Errorf("ReplacesIDs = %v, want [m1 m2] in order", rows[0].ReplacesIDs)
	}
	if len(rows[0].Content) >= len(overCap) {
		t.Errorf("the emission was not clamped (%d bytes)", len(rows[0].Content))
	}
}

// TestReplacedIDsIgnoresAnObsoleteDrop: a drop that named no successor has none.
// Pointing a delete row at the memory that happened to absorb it would be a
// fabricated claim about where its knowledge went.
func TestReplacedIDsIgnoresAnObsoleteDrop(t *testing.T) {
	result := &reflection.ReflectionResult{
		Memories: []reflection.ReflectMemory{{Category: "fact", Content: "an unrelated surviving fact"}},
	}
	out := replacedIDsFor(result)
	if len(out) != 0 {
		t.Errorf("a result with no merges or replacements produced %v, want nothing", out)
	}
	rows := reflectMemoriesToMemory("p1", result.Memories, out)
	if len(rows[0].ReplacesIDs) != 0 {
		t.Errorf("ReplacesIDs = %v, want none", rows[0].ReplacesIDs)
	}
}

// TestReplacedIDsCombineMergesAndRewritesForOneEmission: both operation kinds
// can produce the same text, and the ids they disposed of all belong on the one
// row that now holds their content.
func TestReplacedIDsCombineMergesAndRewritesForOneEmission(t *testing.T) {
	const text = "one consolidated statement of the rule"
	result := &reflection.ReflectionResult{
		Merges:       []reflection.Merge{{IDs: []string{"m1", "m2"}, Text: text}},
		Replacements: []reflection.Replacement{{ID: "m3", Text: text}},
		Memories:     []reflection.ReflectMemory{{Category: "convention", Content: text}},
	}
	rows := reflectMemoriesToMemory("p1", result.Memories, replacedIDsFor(result))
	if len(rows) != 1 {
		t.Fatalf("converted %d rows, want 1", len(rows))
	}
	got := rows[0].ReplacesIDs
	if len(got) != 3 {
		t.Fatalf("ReplacesIDs = %v, want all three ids", got)
	}
	seen := map[string]bool{}
	for _, id := range got {
		seen[id] = true
	}
	for _, want := range []string{"m1", "m2", "m3"} {
		if !seen[want] {
			t.Errorf("ReplacesIDs = %v, missing %s", got, want)
		}
	}
}

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

// TestReplacedIDsReachAMergedSuccessorOfASupersession: a successor the
// response MERGED inherits the dropped row through the text join, because the
// merge's own ids name it. The successor-keyed join does not change that —
// there is no emission restating a merged row — so the link still reaches the
// merged row.
func TestReplacedIDsReachAMergedSuccessorOfASupersession(t *testing.T) {
	const text = "one consolidated statement of the rule"
	result := &reflection.ReflectionResult{
		Merges:       []reflection.Merge{{IDs: []string{"kept", "other"}, Text: text}},
		Replacements: []reflection.Replacement{{ID: "dropped", Text: text}},
		Drops:        []reflection.Drop{{ID: "dropped", Reason: "superseded by", Successor: "KEPT"}},
		Memories:     []reflection.ReflectMemory{{Category: "convention", Content: text}},
	}
	rows := reflectMemoriesToMemory("p1", result.Memories, replacedIDsFor(result))
	if len(rows) != 1 {
		t.Fatalf("converted %d rows, want 1", len(rows))
	}
	got := rows[0].ReplacesIDs
	seen := map[string]bool{}
	for _, id := range got {
		seen[id] = true
	}
	for _, want := range []string{"kept", "other", "dropped"} {
		if !seen[want] {
			t.Errorf("ReplacesIDs = %v, missing %s", got, want)
		}
	}
}
