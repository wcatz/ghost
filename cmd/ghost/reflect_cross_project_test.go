package main

import (
	"context"
	"strings"
	"testing"
)

// TestCrossProjectCandidatesSurviveTheDropAndStayDiscoverable pins that the
// credential drop happens BEFORE the cross-project fold, and that getting that
// order wrong is silent.
//
// applyReflection folds the caller's cross-project candidates into projectMems
// and nils globalMems when promotion is off. Doing that before the drop means
// the returned keptGlobal is the empty slice the drop builds for a nil input —
// never the caller's set — and the two things the caller needs it for die
// without a compile error or a failing test:
//
//   - "(re-run with --promote-globals to inject them into every project)" becomes
//     unreachable, which is the only discoverability the flag has on the default
//     route;
//   - the ", N cross-project candidates kept project-scoped" component vanishes
//     from the Applied line.
//
// Filtering first also drops a credential among the cross-project candidates
// before they can be promoted, which is the behaviour the drop exists for.
func TestCrossProjectCandidatesSurviveTheDropAndStayDiscoverable(t *testing.T) {
	f := &fakeReflectionApplier{}

	keptProject, keptGlobal, _, _, _, applied, err := applyReflection(
		context.Background(), f, "p1",
		projectMemories("the relay listens on 2222"),
		globals("tabs, not spaces, in every repository we touch"),
		"since", false, nil,
	)
	if err != nil {
		t.Fatalf("applyReflection: %v", err)
	}
	if !applied {
		t.Fatal("applied = false, want true")
	}
	if len(keptGlobal) == 0 {
		t.Error("keptGlobal is empty, so the --promote-globals hint and the cross-project count are both unreachable")
	}
	if len(keptProject) != 1 || !strings.Contains(keptProject[0].Content, "2222") {
		t.Errorf("the returned project set is not the one project proposal: %+v", keptProject)
	}
	// The fold happened for the WRITE, so the candidate lands in the project
	// rather than nowhere. The returned set is deliberately pre-fold, because the
	// caller reassembles the summary from keptProject plus keptCrossProject.
	if len(f.projectMems) != 2 {
		t.Errorf("the written project set holds %d memories, want the project proposal plus the folded candidate: %+v", len(f.projectMems), f.projectMems)
	}
	if f.calls != 1 {
		t.Fatalf("ApplyReflection called %d times, want 1", f.calls)
	}
	if len(f.globalMems) != 0 {
		t.Errorf("with promotion off %d candidates were written to _global, want 0", len(f.globalMems))
	}
}

// TestCredentialAmongCrossProjectCandidatesIsNotPromoted is the other half of
// the order: filtering first is what stops a credential candidate reaching
// _global, where it would be replayed into every project on every save.
func TestCredentialAmongCrossProjectCandidatesIsNotPromoted(t *testing.T) {
	credential := "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd"
	f := &fakeReflectionApplier{}

	keptProject, keptGlobal, _, _, _, _, err := applyReflection(
		context.Background(), f, "p1",
		projectMemories("the relay listens on 2222"),
		globals("the deploy token is "+credential),
		"since", true, nil,
	)
	if err != nil {
		t.Fatalf("applyReflection: %v", err)
	}
	if len(keptGlobal) != 0 {
		t.Errorf("keptGlobal holds %d candidates, want 0: %+v", len(keptGlobal), keptGlobal)
	}
	// What the store actually received is the assertion: a global row written
	// here is replayed into every project on every save.
	if len(f.globalMems) != 0 {
		t.Errorf("%d candidate(s) were written to _global, want 0 — a global row is replayed into every project: %+v", len(f.globalMems), f.globalMems)
	}
	if len(keptProject) != 1 || !strings.Contains(keptProject[0].Content, "2222") {
		t.Errorf("the surviving project set is not the one clean memory: %+v", keptProject)
	}
}
