package main

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/eval/cycle/corpus"
)

func TestNormContent(t *testing.T) {
	a := normContent("  Redis   MAXMEMORY\nmust stay at 512mb ")
	b := normContent("redis maxmemory must stay at 512mb")
	if a != b {
		t.Fatalf("norm mismatch: %q vs %q", a, b)
	}
}

func TestJaccard(t *testing.T) {
	if j := jaccard("alpha beta gamma", "GAMMA BETA ALPHA"); j != 1 {
		t.Fatalf("identical tokens want 1, got %.2f", j)
	}
	if j := jaccard("alpha beta", "delta epsilon"); j != 0 {
		t.Fatalf("disjoint want 0, got %.2f", j)
	}
	if j := jaccard("", "x"); j != 0 {
		t.Fatalf("empty want 0, got %.2f", j)
	}
}

func reflectEntries() []corpus.Entry {
	return []corpus.Entry{
		{Key: "mA", Content: "staging api base url is https://staging.acme.dev/api for integration tests", MergeGroup: "g"},
		{Key: "mB", Content: "integration tests use staging api base https://staging.acme.dev/api endpoint", MergeGroup: "g"},
		{Key: "mC", Content: "export staging api base https://staging.acme.dev/api before integration runs", MergeGroup: "g"},
		{Key: "d1", Content: "commits follow conventional commits style with squash merge only"},
	}
}

func rows(pairs ...[2]string) []memRow {
	out := make([]memRow, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, memRow{ID: p[0], Content: p[1]})
	}
	return out
}

func originals() []memRow {
	e := reflectEntries()
	return rows(
		[2]string{"r1", e[0].Content},
		[2]string{"r2", e[1].Content},
		[2]string{"r3", e[2].Content},
		[2]string{"r4", e[3].Content},
	)
}

func TestGradeReflect_Collapsed(t *testing.T) {
	e := reflectEntries()
	// Consolidator kept one merged row close to mA only.
	after := rows([2]string{"m1", e[0].Content})
	rep := gradeReflect(reflectEntries(), originals(), after)
	if rep.Before != 4 || rep.After != 1 {
		t.Fatalf("counts wrong: %+v", rep)
	}
	g := rep.Groups[0]
	if g.Status != "collapsed" || !strings.Contains(g.Survivors, "mA") {
		t.Fatalf("group wrong: %+v", g)
	}
	if rep.DroppedImportant != 0 || len(rep.DroppedDistractors) != 1 || rep.DroppedDistractors[0] != "d1" {
		t.Fatalf("dropped tracking wrong: %+v", rep)
	}
}

func TestGradeReflect_Partial(t *testing.T) {
	e := reflectEntries()
	after := rows(
		[2]string{"k1", e[1].Content},
		[2]string{"k2", e[2].Content},
	)
	rep := gradeReflect(reflectEntries(), originals(), after)
	if got := rep.Groups[0].Status; got != "partial" {
		t.Fatalf("want partial, got %s (%+v)", got, rep.Groups[0])
	}
	if !strings.Contains(rep.Groups[0].Survivors, "mB") || !strings.Contains(rep.Groups[0].Survivors, "mC") {
		t.Fatalf("survivors wrong: %+v", rep.Groups[0])
	}
	if rep.DroppedImportant != 0 {
		t.Fatalf("partial means nothing fully lost, got %d", rep.DroppedImportant)
	}
	if len(rep.DroppedDistractors) != 1 {
		t.Fatalf("d1 should be dropped: %+v", rep.DroppedDistractors)
	}
}

func TestGradeReflect_Lost(t *testing.T) {
	rep := gradeReflect(reflectEntries(), originals(), nil)
	g := rep.Groups[0]
	if g.Status != "lost" {
		t.Fatalf("want lost, got %+v", g)
	}
	if rep.DroppedImportant != 3 {
		t.Fatalf("want 3 dropped-important, got %d", rep.DroppedImportant)
	}
}
