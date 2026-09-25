package fileguard

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestQuarantineGraceStartsAtQuarantineNotSourceMtime(t *testing.T) {
	restore := SetProbeForTest(func(string) (bool, error) { return false, nil })
	defer restore()
	root := t.TempDir()
	path := filepath.Join(root, "candidate")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	tombstone, err := Quarantine(path, currentProbe())
	if err != nil {
		t.Fatal(err)
	}
	removed, err := ReapStaleQuarantine(root)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("removed=%d, want recent tombstone preserved", removed)
	}
	if _, err := os.Stat(tombstone); err != nil {
		t.Fatalf("tombstone missing: %v", err)
	}
}

func TestReapStaleQuarantineOnlyOldUnheld(t *testing.T) {
	root := t.TempDir()
	dir, err := QuarantineDir(filepath.Join(root, "placeholder"))
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "tombstone-old-1")
	recent := filepath.Join(dir, "tombstone-recent-1")
	for _, path := range []string{old, recent} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oldTime := time.Now().Add(-2 * QuarantineGrace)
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	removed, err := ReapStaleQuarantineWithProbe(root, func(string) (bool, error) { return false, nil })
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed=%d want 1", removed)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old tombstone survived: %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("recent tombstone removed: %v", err)
	}
}
