package mcpinit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestACleanRunLeavesAMarkerKeyedOnAnAmbiguousName is the first clearing defect
// (#1268). Two projects answer to "shared", so the write that records a failure
// for that name cannot resolve it either and lands on the digest of the name,
// recording the bare string. The other project's clean run resolves its own id
// and then reaches the same key through its stored name — which is also
// "shared" — and the ownership guard used to read the resolution's ambiguity
// error as "no owner", i.e. "not mine", and delete it. The failing project's
// next session start then showed no alert while its consolidation was still
// paused.
func TestACleanRunLeavesAMarkerKeyedOnAnAmbiguousName(t *testing.T) {
	dataHome := isolatedHome(t)
	seedProject(t, dataHome, "first", "", "shared")
	seedProject(t, dataHome, "second", "", "shared")

	if err := WriteLifecycleFailure("shared", []string{"reflect"}, "the ambiguous failure"); err != nil {
		t.Fatalf("record the failure: %v", err)
	}
	path := filepath.Join(dataHome, "ghost", projectMarkerFile("shared"))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the failure was not written under the ambiguous name: %v", err)
	}

	// The project that did NOT fail runs clean. Its stored name is the shared
	// one, so its clear reaches the marker it cannot attribute to itself.
	if err := FinishLifecycleRun("second", 3, nil, ""); err != nil {
		t.Fatalf("clean run: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("a clean run deleted the failure it cannot attribute to itself: %v", err)
	}
	if got := lifecycleFailureAlert("first", "shared"); !strings.Contains(got, "the ambiguous failure") {
		t.Errorf("the failing project's session start lost its alert: %q", got)
	}
}

// TestACleanRunStillHealsTheOperandKeyedMarkerWithNoStore guards the edge the
// ambiguity fix must not swallow (#1268). A data directory whose store cannot be
// read is the WRITER's own fallback — WriteLifecycleFailure keys the marker on
// the operand because nothing resolved it — so the matching clean run has to
// clear that file. Only an ambiguous reference is a key that cannot be
// attributed; a resolution error of any other kind leaves no other project
// claiming the key, and reading it as ambiguous would strand the marker until
// the 30-day self-clean.
func TestACleanRunStillHealsTheOperandKeyedMarkerWithNoStore(t *testing.T) {
	dataHome := isolatedHome(t)
	if err := os.MkdirAll(filepath.Join(dataHome, "ghost"), 0o700); err != nil {
		t.Fatalf("mkdir ghost dir: %v", err)
	}
	// No ghost.db, so every resolve fails exactly as it did for the write.
	if err := WriteLifecycleFailure("proj-x", []string{"reflect"}, "the storeless failure"); err != nil {
		t.Fatalf("record the failure: %v", err)
	}
	path := filepath.Join(dataHome, "ghost", projectMarkerFile("proj-x"))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the storeless failure was not written under its operand: %v", err)
	}

	if err := FinishLifecycleRun("proj-x", 3, nil, ""); err != nil {
		t.Fatalf("clean run: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a clean run left the operand-keyed marker it wrote storelessly: %v", err)
	}
}

// TestALegacyMarkerKeyedOnTheProjectNameIsCleared is the second clearing defect
// (#1268). The pre-per-project writer recorded the unresolved operand — often
// the project's name — in the one shared lifecycle-last-failure.json, and the
// clear compared that field against the resolved ID alone. A marker recording
// "Proj A" was therefore never healed by "Proj A"'s own success, so the alert
// kept saying consolidation was paused after it had recovered, and the retry
// command it printed could not clear it either.
func TestALegacyMarkerKeyedOnTheProjectNameIsCleared(t *testing.T) {
	seedLegacy := func(t *testing.T, dataHome, recorded string) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dataHome, "ghost"), 0o700); err != nil {
			t.Fatalf("mkdir ghost dir: %v", err)
		}
		b, err := json.Marshal(testMarker{
			Project:      recorded,
			PhasesFailed: []string{"reflect"},
			Error:        "the old shared failure",
			At:           time.Now().UTC().Format(time.RFC3339),
			Version:      1,
		})
		if err != nil {
			t.Fatalf("marshal marker: %v", err)
		}
		path := filepath.Join(dataHome, "ghost", legacyMarkerFile)
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatalf("write legacy marker: %v", err)
		}
		return path
	}

	t.Run("the project's own name is healed", func(t *testing.T) {
		dataHome := isolatedHome(t)
		seedProject(t, dataHome, "p1", "/tmp/proj-a", "Proj A")
		path := seedLegacy(t, dataHome, "Proj A")

		if got := lifecycleFailureAlert("p1", "Proj A"); !strings.Contains(got, "the old shared failure") {
			t.Fatalf("the legacy marker no longer alerts: %q", got)
		}
		if err := FinishLifecycleRun("p1", 3, nil, ""); err != nil {
			t.Fatalf("clean run: %v", err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("a clean run left the legacy marker recording the project's name (stat err = %v)", err)
		}
	})

	t.Run("another project's name is left", func(t *testing.T) {
		dataHome := isolatedHome(t)
		seedProject(t, dataHome, "p1", "/tmp/proj-a", "Proj A")
		path := seedLegacy(t, dataHome, "Proj B")

		if err := FinishLifecycleRun("p1", 3, nil, ""); err != nil {
			t.Fatalf("clean run: %v", err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("a clean run cleared a legacy marker naming another project: %v", err)
		}
	})
}
