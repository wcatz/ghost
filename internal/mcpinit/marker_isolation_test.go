package mcpinit

import (
	"os"
	"testing"
)

// TestSuccessInOneProjectDoesNotClearAnother is the isolation defect (issue
// #540). The marker was one shared file, and FinishLifecycleRun cleared it on
// any project's success — so project A's next clean run deleted project B's
// still-active failure. B's phase never ran, its failure was recorded
// faithfully, and a run somewhere else made it invisible: consolidation for B
// stayed broken with nothing left on screen to say so.
func TestSuccessInOneProjectDoesNotClearAnother(t *testing.T) {
	dataHome := isolatedHome(t)
	seedProject(t, dataHome, "p1", "/tmp/proj-a", "Proj A")
	seedProject(t, dataHome, "p2", "/tmp/proj-b", "Proj B")

	if err := WriteLifecycleFailure("p2", []string{"reflect"}, "B reflect is broken"); err != nil {
		t.Fatalf("record B's failure: %v", err)
	}

	// A runs clean.
	if err := FinishLifecycleRun("p1", 3, nil, ""); err != nil {
		t.Fatalf("A success: %v", err)
	}

	m := readMarkerFile(t, markerPath(dataHome, "p2"))
	if m.Error != "B reflect is broken" {
		t.Errorf("project A's success erased project B's failure: error = %q", m.Error)
	}
	if len(m.PhasesFailed) != 1 || m.PhasesFailed[0] != "reflect" {
		t.Errorf("phases = %v, want [reflect]", m.PhasesFailed)
	}
}

// TestSuccessStillClearsItsOwnMarker is the positive control: scoping the
// clear must not have made markers immortal. Success still heals the project
// that succeeded.
func TestSuccessStillClearsItsOwnMarker(t *testing.T) {
	dataHome := isolatedHome(t)
	seedProject(t, dataHome, "p1", "/tmp/proj-a", "Proj A")

	// Record under the NAME so the clear has to resolve it the same way the
	// write did — clearing by raw name against an id-keyed file was the bug
	// that made healing silently do nothing.
	if err := WriteLifecycleFailure("Proj A", []string{"reflect"}, "broken"); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	if err := FinishLifecycleRun("Proj A", 2, nil, ""); err != nil {
		t.Fatalf("success: %v", err)
	}

	path := markerPath(dataHome, "p1")
	if _, err := os.ReadFile(path); !os.IsNotExist(err) {
		t.Errorf("success did not clear its own marker at %s (stat err = %v)", path, err)
	}
}

// TestConcurrentFailuresInDifferentProjectsBothSurvive: with one shared file,
// two projects failing at the same moment overwrote each other, so at most one
// project's problem was ever recorded. Per-project files remove the shared
// target entirely.
func TestConcurrentFailuresInDifferentProjectsBothSurvive(t *testing.T) {
	dataHome := isolatedHome(t)
	seedProject(t, dataHome, "p1", "/tmp/proj-a", "Proj A")
	seedProject(t, dataHome, "p2", "/tmp/proj-b", "Proj B")

	if err := WriteLifecycleFailure("p1", []string{"reflect"}, "A broke"); err != nil {
		t.Fatalf("record A: %v", err)
	}
	if err := WriteLifecycleFailure("p2", []string{"resolve", "supersede"}, "B broke"); err != nil {
		t.Fatalf("record B: %v", err)
	}

	a := readMarkerFile(t, markerPath(dataHome, "p1"))
	if a.Error != "A broke" {
		t.Errorf("A's marker = %q, want %q", a.Error, "A broke")
	}
	b := readMarkerFile(t, markerPath(dataHome, "p2"))
	if b.Error != "B broke" {
		t.Errorf("B's marker = %q, want %q — a shared file would have left only one", b.Error, "B broke")
	}
	if len(b.PhasesFailed) != 2 {
		t.Errorf("B's phases = %v, want [resolve supersede]", b.PhasesFailed)
	}
}

// TestClearIsScopedToAProject: clearing must resolve the project the same way
// the writer did, otherwise "healing" targets a file that was never written.
func TestClearIsScopedToAProject(t *testing.T) {
	dataHome := isolatedHome(t)
	seedProject(t, dataHome, "p1", "/tmp/proj-a", "Proj A")
	seedProject(t, dataHome, "p2", "/tmp/proj-b", "Proj B")

	if err := WriteLifecycleFailure("p1", []string{"reflect"}, "A broke"); err != nil {
		t.Fatalf("record A: %v", err)
	}
	if err := WriteLifecycleFailure("p2", []string{"reflect"}, "B broke"); err != nil {
		t.Fatalf("record B: %v", err)
	}

	if err := ClearLifecycleFailure("Proj A"); err != nil {
		t.Fatalf("clear A by name: %v", err)
	}
	if _, err := os.ReadFile(markerPath(dataHome, "p1")); !os.IsNotExist(err) {
		t.Errorf("A's marker survived a clear addressed by its name — the clear did not resolve the project")
	}
	if m := readMarkerFile(t, markerPath(dataHome, "p2")); m.Error != "B broke" {
		t.Errorf("clearing A also cleared B: error = %q", m.Error)
	}
}

// TestMarkerFileNamesAreSanitizedAndDistinct: the project may be an unresolved
// name containing anything the filesystem accepts, and two names differing only
// in punctuation must not collapse onto one file — that would put two
// projects' failures in the same place, which is the bug we just removed.
func TestMarkerFileNamesAreSanitizedAndDistinct(t *testing.T) {
	cases := []string{"p1", "My Project", "a/b", `c:\d`, "weird:name*"}
	seen := map[string]string{}
	for _, c := range cases {
		got := projectMarkerFile(c)
		if prev, dup := seen[got]; dup {
			t.Errorf("projects %q and %q map to the same file %s", prev, c, got)
		}
		seen[got] = c

		for _, r := range got {
			if r == '/' || r == '\\' || r == '*' || r == ':' {
				t.Errorf("project %q produced an unsafe file name %q", c, got)
				break
			}
		}
	}
	if projectMarkerFile("") == projectMarkerFile("unattributed") {
		t.Errorf("an empty project and the literal word collide: %s", projectMarkerFile(""))
	}
}
