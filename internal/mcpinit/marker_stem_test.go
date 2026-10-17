package mcpinit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeOldStemMarker seeds a marker under the file name an older build gave a
// project: the sanitized stem, which folded every byte outside [A-Za-z0-9._-]
// onto "_". It is spelled out here rather than derived from the product so the
// test keeps describing the files that are already on disk after the stem
// changes.
func writeOldStemMarker(t *testing.T, xdgHome, fileStem string, m testMarker) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(xdgHome, "ghost"), 0o700); err != nil {
		t.Fatalf("mkdir ghost dir: %v", err)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal marker: %v", err)
	}
	path := filepath.Join(xdgHome, "ghost", "lifecycle-last-failure-"+fileStem+".json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	return path
}

// TestPunctuationDifferingProjectsKeepSeparateMarkers: "my project" and
// "my_project" are different projects, but the sanitized stem folded the space
// onto "_" and keyed both on lifecycle-last-failure-my_project.json — so the
// second project's clean run deleted the first one's still-active failure and
// its next session start showed no alert. Neither operand resolves to a stored
// project here, so the raw operands are the keys, exactly as for an
// unresolvable --project.
func TestPunctuationDifferingProjectsKeepSeparateMarkers(t *testing.T) {
	dataHome := isolatedHome(t)
	seedProject(t, dataHome, "p1", "/tmp/proj-a", "Proj A")

	if err := WriteLifecycleFailure("my project", []string{"reflect"}, "A reflect is broken"); err != nil {
		t.Fatalf("record A's failure: %v", err)
	}
	if err := FinishLifecycleRun("my_project", 3, nil, ""); err != nil {
		t.Fatalf("B success: %v", err)
	}

	m := readLifecycleFailure("my project", "", nil)
	if m == nil || m.Error != "A reflect is broken" || m.Project != "my project" {
		t.Fatalf("B's clean run erased A's failure: marker = %+v", m)
	}
	if got := lifecycleFailureAlert("my project", ""); !strings.Contains(got, "A reflect is broken") {
		t.Errorf("A's next session start shows no alert: %q", got)
	}
	if projectMarkerFile("my project") == projectMarkerFile("my_project") {
		t.Errorf("both projects key the same file %s", projectMarkerFile("my project"))
	}
}

// TestOldStemMarkerIsReadAndCleared: a marker written by the previous build
// under the sanitized stem must survive the upgrade — still alerting, and still
// healed by the project's own clean run — rather than being stranded until the
// 30-day self-clean.
func TestOldStemMarkerIsReadAndCleared(t *testing.T) {
	dataHome := isolatedHome(t)
	seedProject(t, dataHome, "p1", "/tmp/proj-a", "Proj A")
	path := writeOldStemMarker(t, dataHome, "p1", testMarker{
		Project: "p1", PhasesFailed: []string{"reflect"}, Error: "old build failure",
		At: time.Now().UTC().Format(time.RFC3339), Version: 1,
	})

	if got := lifecycleFailureAlert("p1", "Proj A"); !strings.Contains(got, "old build failure") {
		t.Fatalf("the old-stem marker no longer alerts: %q", got)
	}
	if err := FinishLifecycleRun("Proj A", 2, nil, ""); err != nil {
		t.Fatalf("success: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the project's clean run left its old-stem marker at %s (stat err = %v)", path, err)
	}
	if got := lifecycleFailureAlert("p1", "Proj A"); got != "" {
		t.Errorf("alert after the clean run = %q, want none", got)
	}
}

// TestOldStemMarkerOfAnotherProjectSurvivesAClear is the collision carried
// across the upgrade: an older build stored "my project"'s failure in
// lifecycle-last-failure-my_project.json, which is ALSO the old stem of
// "my_project". The old-stem fallback must not let "my_project"'s clean run
// delete it, and "my project" must still be alerted from it.
func TestOldStemMarkerOfAnotherProjectSurvivesAClear(t *testing.T) {
	dataHome := isolatedHome(t)
	seedProject(t, dataHome, "p1", "/tmp/proj-a", "Proj A")
	path := writeOldStemMarker(t, dataHome, "my_project", testMarker{
		Project: "my project", PhasesFailed: []string{"reflect"}, Error: "A broke before the upgrade",
		At: time.Now().UTC().Format(time.RFC3339), Version: 1,
	})

	if err := FinishLifecycleRun("my_project", 3, nil, ""); err != nil {
		t.Fatalf("B success: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("B's clean run deleted A's old-stem marker: %v", err)
	}
	if got := lifecycleFailureAlert("my_project", ""); got != "" {
		t.Errorf("B's session start shows A's failure: %q", got)
	}
	if got := lifecycleFailureAlert("my project", ""); !strings.Contains(got, "A broke before the upgrade") {
		t.Errorf("A's session start lost its old-stem failure: %q", got)
	}
}

// TestMarkerStemCarriesNoProjectID: the stem is a digest, so the file name is
// injective in practice for ids differing in any byte (case included, for a
// case-insensitive filesystem) and never spells the id out.
func TestMarkerStemCarriesNoProjectID(t *testing.T) {
	ids := []string{"my project", "my_project", "My_project", "my-project", "my.project", "ghp_abcdefghijklmnopqrstuvwxyzABCDEFGHIJ"}
	seen := map[string]string{}
	for _, id := range ids {
		f := projectMarkerFile(id)
		if prev, dup := seen[strings.ToLower(f)]; dup {
			t.Errorf("%q and %q share %s", prev, id, f)
		}
		seen[strings.ToLower(f)] = id
		if strings.Contains(f, strings.ReplaceAll(id, " ", "_")) {
			t.Errorf("file name %s carries the id %q", f, id)
		}
	}
}

// TestAnotherProjectsOldStemMarkerDoesNotHideOwnLegacyMarker: the old-stem
// file "my_project" reads first may hold "my project"'s failure. It must be
// passed over, not returned for the alert to suppress, so the legacy shared
// marker recording "my_project"'s own failure is still reached.
func TestAnotherProjectsOldStemMarkerDoesNotHideOwnLegacyMarker(t *testing.T) {
	dataHome := isolatedHome(t)
	seedProject(t, dataHome, "p1", "/tmp/proj-a", "Proj A")
	now := time.Now().UTC().Format(time.RFC3339)
	writeOldStemMarker(t, dataHome, "my_project", testMarker{
		Project: "my project", PhasesFailed: []string{"reflect"}, Error: "A broke", At: now, Version: 1,
	})
	writeMarkerFile(t, dataHome, testMarker{
		Project: "", PhasesFailed: []string{"resolve"}, Error: "legacy failure", At: now, Version: 1,
	})

	if got := lifecycleFailureAlert("my_project", ""); !strings.Contains(got, "legacy failure") {
		t.Errorf("another project's old-stem marker hid this session's legacy marker: %q", got)
	}
}
