//go:build !windows

package mcpinit

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/adversarial"
)

// TestSharedMarkerPathShapesReadAsNoMarker drives the shared non-regular marker
// path set through the one reader every marker surface uses. A marker is always
// a regular file this package wrote, so each planted shape must read as no
// marker and must return at once rather than blocking or following a link.
func TestSharedMarkerPathShapesReadAsNoMarker(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "planted.fifo")
	mkfifoAt(t, fifo)
	real := filepath.Join(dir, "real.json")
	at := time.Now().UTC().Format(time.RFC3339)
	if err := os.WriteFile(real, mustJSON(testMarker{Project: "p1", At: at}), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, shape := range adversarial.MarkerPathShapes() {
		t.Run(shape, func(t *testing.T) {
			path := filepath.Join(dir, shape)
			switch shape {
			case "fifo":
				path = fifo
			case "symlink-to-fifo":
				if err := os.Symlink(fifo, path); err != nil {
					t.Fatal(err)
				}
			case "symlink-to-marker":
				if err := os.Symlink(real, path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			var got *lifecycleFailureMarker
			withMarkerDeadline(t, []string{fifo}, func() { got = readMarkerAtPath(path) })
			if got != nil {
				t.Errorf("readMarkerAtPath(%s) = %+v, want nil", shape, got)
			}
		})
	}
}
