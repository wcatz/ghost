package mcpinit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Obviously fake, shaped by the detector's GitHub personal access token rule —
// the fixture TestSessionStart_MarkerProjectWithholdsACredential uses. Two of
// them, because two token operands must not collapse onto one marker.
const (
	markerTokenA = "ghp_" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	markerTokenB = "ghp_" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// TestATokenShapedOperandNeverReachesTheMarker: a token-shaped --project operand
// that resolves to no project used to be stored raw in the marker's project
// field, kept on disk for up to 30 days. Neither a file name nor any file's
// bytes in the data directory may carry it, and the failure must still alert.
func TestATokenShapedOperandNeverReachesTheMarker(t *testing.T) {
	dataHome := isolatedHome(t)
	seedProject(t, dataHome, "p1", "/tmp/proj-a", "Proj A")
	dataDir := filepath.Join(dataHome, "ghost")

	for _, tok := range []string{markerTokenA, markerTokenB} {
		if err := WriteLifecycleFailure(tok, []string{"reflect"}, "reflect failed"); err != nil {
			t.Fatalf("record the failure: %v", err)
		}
	}

	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatalf("read data dir: %v", err)
	}
	for _, e := range entries {
		for _, tok := range []string{markerTokenA, markerTokenB} {
			if strings.Contains(e.Name(), tok) {
				t.Errorf("a file name carries the token: %s", e.Name())
			}
		}
		if e.IsDir() || !strings.HasPrefix(e.Name(), lifecycleMarkerPrefix) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dataDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, tok := range []string{markerTokenA, markerTokenB} {
			if strings.Contains(string(b), tok) {
				t.Errorf("marker %s stores the token: %s", e.Name(), b)
			}
		}
	}

	// Two tokens keep two markers: the withheld value is the same placeholder
	// for both, so a file keyed on it would let one overwrite the other.
	for i, tok := range []string{markerTokenA, markerTokenB} {
		m := readMarkerAtPath(filepath.Join(dataDir, projectMarkerFile(tok)))
		if m == nil {
			t.Fatalf("no marker for token operand %d", i)
		}
		if !strings.Contains(m.Project, "withheld") {
			t.Errorf("project field = %q, want the withheld placeholder", m.Project)
		}
	}

	alert := lifecycleFailureAlert("", "")
	if !strings.HasPrefix(alert, "**Ghost maintenance alert:**") || !strings.Contains(alert, "reflect failed") {
		t.Errorf("the withheld failure no longer alerts: %q", alert)
	}
	if !strings.Contains(alert, "<project withheld: it holds a ") {
		t.Errorf("the alert does not name the withheld project: %q", alert)
	}
}

// TestATokenShapedOperandsCleanRunClearsItsMarker: success heals its own
// project. The run that failed under a token operand clears under the same
// operand, even though the marker no longer records it.
func TestATokenShapedOperandsCleanRunClearsItsMarker(t *testing.T) {
	dataHome := isolatedHome(t)
	seedProject(t, dataHome, "p1", "/tmp/proj-a", "Proj A")
	path := filepath.Join(dataHome, "ghost", projectMarkerFile(markerTokenA))

	if err := WriteLifecycleFailure(markerTokenA, []string{"reflect"}, "reflect failed"); err != nil {
		t.Fatalf("record the failure: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("marker not written: %v", err)
	}
	if err := FinishLifecycleRun(markerTokenA, 1, nil, ""); err != nil {
		t.Fatalf("clean run: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the clean run left the marker on disk: %v", err)
	}
}

// TestAWithheldMarkerSelfCleans: the 30-day self-clean clears by the marker's
// recorded project, and a withheld project names no file. The marker that was
// read must still be deleted, or it lingers on disk forever.
func TestAWithheldMarkerSelfCleans(t *testing.T) {
	dataHome := isolatedHome(t)
	seedProject(t, dataHome, "p1", "/tmp/proj-a", "Proj A")
	path := filepath.Join(dataHome, "ghost", projectMarkerFile(markerTokenA))

	if err := WriteLifecycleFailure(markerTokenA, []string{"reflect"}, "reflect failed"); err != nil {
		t.Fatalf("record the failure: %v", err)
	}
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

	if got := lifecycleFailureAlert("", ""); got != "" {
		t.Errorf("an expired marker alerted: %q", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the self-clean left the expired withheld marker on disk: %v", err)
	}
}
