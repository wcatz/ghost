package mcpinit

import (
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
