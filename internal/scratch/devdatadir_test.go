package scratch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/config"
)

// TestRootHonoursForbiddenDataDir: a dev build must refuse, rather than create a
// scratch root inside, a data directory GHOST_DEV_FORBID_DATA_DIR forbids, and
// Reap (what the lifecycle calls first) must fail the same way. A release build
// ignores the variable and keeps its scratch root in the real store. The
// override is unset deliberately: with GHOST_SCRATCH_DIR set Root never
// resolves the data directory at all.
func TestRootHonoursForbiddenDataDir(t *testing.T) {
	for _, tt := range []struct {
		name, version string
		refused       bool
	}{
		{"dev build refuses", "dev", true},
		{"release build ignores", "0.39.0", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			prev := config.BuildVersion()
			config.SetBuildVersion(tt.version)
			t.Cleanup(func() { config.SetBuildVersion(prev) })

			dataHome := filepath.Join(t.TempDir(), "data")
			dataDir := filepath.Join(dataHome, "ghost")
			must(t, os.MkdirAll(dataDir, 0o700))
			t.Setenv("XDG_DATA_HOME", dataHome)
			t.Setenv(envDir, "")
			t.Setenv(config.DevForbidDataDirEnv, dataDir)

			got, err := Root()
			if !tt.refused {
				if want := filepath.Join(dataDir, "scratch"); err != nil || got != want {
					t.Fatalf("Root = (%q, %v), want %q", got, err, want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), config.DevForbidDataDirEnv) {
				t.Fatalf("Root error = %v, want a refusal naming %s", err, config.DevForbidDataDirEnv)
			}
			if _, _, err := Reap(time.Hour); err == nil {
				t.Error("Reap = nil, want the refusal Root reports")
			}
			if entries, err := os.ReadDir(dataDir); err != nil || len(entries) != 0 {
				t.Errorf("the forbidden data directory holds %d entries (err %v), want none", len(entries), err)
			}

			// Control: with the variable unset the same call creates the root, so
			// the assertions above are about the guard, not the sandbox.
			t.Setenv(config.DevForbidDataDirEnv, "")
			if got, err := Root(); err != nil || got != filepath.Join(dataDir, "scratch") {
				t.Errorf("Root with the variable unset = (%q, %v), want the scratch root", got, err)
			}
		})
	}
}
