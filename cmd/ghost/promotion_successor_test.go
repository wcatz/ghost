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
	rows := reflectMemoriesToMemory("p1", emissions, replacedIDsByText(result))
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
	out := replacedIDsByText(result)
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
	rows := reflectMemoriesToMemory("p1", result.Memories, replacedIDsByText(result))
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
