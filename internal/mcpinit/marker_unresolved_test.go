package mcpinit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeUnresolvedMarker records a failure for an operand no store resolves yet,
// as a run against a project an import has not brought in does, and returns the
// file it landed in: the one keyed on the operand as given. The data directory
// exists but holds no database, so the write cannot resolve the operand; it is
// asserted on disk because a write that silently failed would make every
// "the file is gone" assertion after it pass for nothing.
func writeUnresolvedMarker(t *testing.T, dataHome, operand string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dataHome, "ghost"), 0o700); err != nil {
		t.Fatalf("mkdir ghost dir: %v", err)
	}
	if err := WriteLifecycleFailure(operand, []string{"reflect"}, "reflect failed before the import"); err != nil {
		t.Fatalf("record the failure: %v", err)
	}
	path := filepath.Join(dataHome, "ghost", projectMarkerFile(operand))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the unresolved failure was not written under its operand: %v", err)
	}
	return path
}

// TestACleanRunClearsAMarkerKeyedOnTheUnresolvedName: a failure recorded while
// "Proj A" resolved to nothing is keyed on the name. Once the project exists
// under that name with a different id (an imported project), a clean run by its
// name re-resolves the operand to the id and used to remove only the id's file,
// so the alert kept saying consolidation was paused after it had recovered.
func TestACleanRunClearsAMarkerKeyedOnTheUnresolvedName(t *testing.T) {
	dataHome := isolatedHome(t)
	path := writeUnresolvedMarker(t, dataHome, "Proj A")
	seedProject(t, dataHome, "p1", "/tmp/proj-a", "Proj A")

	if err := FinishLifecycleRun("Proj A", 3, nil, ""); err != nil {
		t.Fatalf("clean run by name: %v", err)
	}
	if got := lifecycleFailureAlert("p1", "Proj A"); got != "" {
		t.Errorf("the project's session start still alerts after a clean run by name: %q", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a clean run by name left the name-keyed marker on disk: %v", err)
	}
}

// TestACleanRunByIDClearsAMarkerKeyedOnTheProjectsName: the same marker, and the
// clean run names the project by its id, as the hook-spawned run does. The
// name-keyed file records this project, so it is cleared through the name the
// store holds for the id.
func TestACleanRunByIDClearsAMarkerKeyedOnTheProjectsName(t *testing.T) {
	dataHome := isolatedHome(t)
	path := writeUnresolvedMarker(t, dataHome, "Proj A")
	seedProject(t, dataHome, "p1", "/tmp/proj-a", "Proj A")

	if err := FinishLifecycleRun("p1", 3, nil, ""); err != nil {
		t.Fatalf("clean run by id: %v", err)
	}
	if got := lifecycleFailureAlert("p1", "Proj A"); got != "" {
		t.Errorf("the project's session start still alerts after a clean run by id: %q", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a clean run by id left the name-keyed marker on disk: %v", err)
	}
}

// TestACleanRunLeavesAnotherProjectsMarkerKeyedOnItsName: the name-keyed file is
// checked against the store, not only against the project it records. Project
// "shared" (id) and project p1 named "shared" both key a failure on the same
// file, and both record the bare string "shared", so the recorded project cannot
// tell them apart. p1's clean run must leave the id-keyed marker of the project
// whose id is that name.
func TestACleanRunLeavesAnotherProjectsMarkerKeyedOnItsName(t *testing.T) {
	dataHome := isolatedHome(t)
	seedProject(t, dataHome, "shared", "/tmp/shared", "Shared")
	seedProject(t, dataHome, "p1", "/tmp/proj-a", "shared")
	if err := WriteLifecycleFailure("shared", []string{"reflect"}, "the other project broke"); err != nil {
		t.Fatalf("record the other project's failure: %v", err)
	}
	path := filepath.Join(dataHome, "ghost", projectMarkerFile("shared"))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the other project's failure was not written: %v", err)
	}

	if err := FinishLifecycleRun("p1", 3, nil, ""); err != nil {
		t.Fatalf("clean run: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("p1's clean run deleted the marker of the project whose id is p1's name: %v", err)
	}
}

// TestAnExpiredMarkerKeyedOnTheUnresolvedNameSelfCleans: the 30-day self-clean
// half. The alert reads the name-keyed file for the project, finds it expired,
// and removes it, rather than leaving it on disk indefinitely because the clear
// re-resolved the name to the id.
func TestAnExpiredMarkerKeyedOnTheUnresolvedNameSelfCleans(t *testing.T) {
	dataHome := isolatedHome(t)
	path := writeUnresolvedMarker(t, dataHome, "Proj A")
	seedProject(t, dataHome, "p1", "/tmp/proj-a", "Proj A")

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	var m testMarker
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse marker: %v", err)
	}
	m.At = time.Now().UTC().Add(-31 * 24 * time.Hour).Format(time.RFC3339)
	if b, err = json.Marshal(m); err != nil {
		t.Fatalf("marshal marker: %v", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("backdate marker: %v", err)
	}

	if got := lifecycleFailureAlert("p1", "Proj A"); got != "" {
		t.Errorf("an expired marker alerted: %q", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the self-clean left the expired name-keyed marker on disk: %v", err)
	}
}

// TestAnExpiredIDMarkerDoesNotTakeAFreshNameMarkerWithIt: the self-clean removes
// what has expired and nothing else. The project's id-keyed marker is past the
// self-clean horizon while a failure keyed on its name is current; the alert
// reads the id file first, and clearing the whole project from there would
// delete the current failure along with the stale one. Only a clean run clears
// every key. The stale file goes, the current one survives and is shown.
func TestAnExpiredIDMarkerDoesNotTakeAFreshNameMarkerWithIt(t *testing.T) {
	dataHome := isolatedHome(t)
	namePath := writeUnresolvedMarker(t, dataHome, "Proj A")
	seedProject(t, dataHome, "p1", "/tmp/proj-a", "Proj A")
	if err := WriteLifecycleFailure("p1", []string{"resolve"}, "the stale id failure"); err != nil {
		t.Fatalf("record the id failure: %v", err)
	}
	idPath := filepath.Join(dataHome, "ghost", projectMarkerFile("p1"))
	b, err := os.ReadFile(idPath)
	if err != nil {
		t.Fatalf("read id marker: %v", err)
	}
	var m testMarker
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse id marker: %v", err)
	}
	m.At = time.Now().UTC().Add(-31 * 24 * time.Hour).Format(time.RFC3339)
	if b, err = json.Marshal(m); err != nil {
		t.Fatalf("marshal id marker: %v", err)
	}
	if err := os.WriteFile(idPath, b, 0o600); err != nil {
		t.Fatalf("backdate id marker: %v", err)
	}

	got := lifecycleFailureAlert("p1", "Proj A")
	if !strings.Contains(got, "reflect failed before the import") {
		t.Errorf("the current name-keyed failure is not shown once the stale one expires: %q", got)
	}
	if _, err := os.Stat(namePath); err != nil {
		t.Errorf("the self-clean deleted a current name-keyed marker: %v", err)
	}
	if _, err := os.Stat(idPath); !os.IsNotExist(err) {
		t.Errorf("the self-clean left the expired id-keyed marker on disk: %v", err)
	}
}
