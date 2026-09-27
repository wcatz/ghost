package main

import (
	"context"
	"strings"
	"testing"
)

// TestPartialDropIsNotCountedAsConsolidated pins that the "Applied: N memories
// consolidated" line counts what was WRITTEN.
//
// The summary is built from the caller's proposal lists, and the credential drop
// happens inside applyReflection. So a round with one good proposal and one
// credential had to report 2 written when 1 was — a report that inflates the
// count of rows in the store and, on a project where every proposal is a
// credential, a report that says the project gained a memory it did not gain.
//
// The counts come from the POST-drop sets that applyReflection returns, not from
// the caller's pre-drop slices.
func TestPartialDropIsNotCountedAsConsolidated(t *testing.T) {
	credential := "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd"
	f := &fakeReflectionApplier{}

	keptProject, keptGlobal, _, _, _, applied, err := applyReflection(
		context.Background(), f, "p1",
		projectMemories("the relay listens on 2222", "the deploy token is "+credential),
		nil, "since", false, nil,
	)
	if err != nil {
		t.Fatalf("applyReflection: %v", err)
	}
	if !applied {
		t.Fatal("applied = false, want true — one proposal survived")
	}
	if f.calls != 1 {
		t.Fatalf("ApplyReflection called %d times, want 1", f.calls)
	}
	if len(keptProject) != 1 {
		t.Errorf("the kept set holds %d proposals, want the one that was written: %+v", len(keptProject), keptProject)
	}
	if len(keptGlobal) != 0 {
		t.Errorf("the kept global set holds %d, want 0", len(keptGlobal))
	}
	for _, m := range keptProject {
		if strings.Contains(m.Content, credential) {
			t.Errorf("the kept set carries the credential: %q", m.Content)
		}
	}
}
