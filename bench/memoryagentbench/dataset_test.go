package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSplitFacts(t *testing.T) {
	context := "Here is a list of facts:\n" +
		"0. Thomas Kyd was born in the city of London.\n" +
		"1. The chairperson of Fatah is Mahmoud Abbas.\n" +
		"16. Chanel was founded by Coco Chanel.\n" +
		"34. Chanel was founded by Andy Warhol.\n"
	facts := splitFacts(context)
	want := []string{
		"Thomas Kyd was born in the city of London.",
		"The chairperson of Fatah is Mahmoud Abbas.",
		"Chanel was founded by Coco Chanel.",
		"Chanel was founded by Andy Warhol.",
	}
	if len(facts) != len(want) {
		t.Fatalf("got %d facts, want %d: %v", len(facts), len(want), facts)
	}
	for i, f := range facts {
		if f != want[i] {
			t.Errorf("fact %d = %q, want %q", i, f, want[i])
		}
	}
}

func TestSplitFactsIgnoresNonNumberedLines(t *testing.T) {
	context := "Here is a list of facts:\n0. A fact.\nsome stray commentary line\n1. Another fact.\n"
	facts := splitFacts(context)
	if len(facts) != 2 {
		t.Fatalf("got %d facts, want 2: %v", len(facts), facts)
	}
}

func TestLoadDemos(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "demos.jsonl")
	content := `{"source":"factconsolidation_sh_6k","context":"0. A.\n1. B.\n","questions":["q1"],"answers":[["B"]],"qa_pair_ids":["id0"]}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	demos, err := loadDemos(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(demos) != 1 {
		t.Fatalf("got %d demos, want 1", len(demos))
	}
	if demos[0].Source != "factconsolidation_sh_6k" {
		t.Errorf("source = %q", demos[0].Source)
	}
	facts := splitFacts(demos[0].Context)
	if len(facts) != 2 || facts[0] != "A." || facts[1] != "B." {
		t.Errorf("facts = %v", facts)
	}
}

func TestLoadDemosMismatchedLengths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "demos.jsonl")
	content := `{"source":"bad","context":"0. A.\n","questions":["q1","q2"],"answers":[["A"]],"qa_pair_ids":["id0"]}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDemos(path); err == nil {
		t.Fatal("expected error for mismatched questions/answers/qa_pair_ids lengths")
	}
}
