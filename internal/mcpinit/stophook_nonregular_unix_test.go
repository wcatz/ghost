//go:build unix

package mcpinit

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestStopSaveMarkerFIFOIsTreatedAsNoMarker pins finding #1: a FIFO (or any
// non-regular file) at the marker path must not block or crash the hook.
func TestStopSaveMarkerFIFOIsTreatedAsNoMarker(t *testing.T) {
	dataHome := isolatedHome(t)
	dataDir := filepath.Join(dataHome, "ghost")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sess := "fifo-sess"
	markerPath := stopSaveMarkerPath(sess, "")
	if markerPath == "" {
		t.Fatal("no marker path")
	}
	if err := syscall.Mkfifo(markerPath, 0o600); err != nil {
		t.Fatal(err)
	}
	// Must return promptly (no hang) and not panic.
	path := writeTranscript(t, lineToolBash)
	in := contractInputFor(t, "Stop", "claude-code", "claude-jsonl",
		fmt.Sprintf(`{"session_id":%q,"transcript_path":%q,"cwd":"/repo","stop_hook_active":false}`, sess, path))
	_ = runStopHook(t, in)
}
