//go:build !windows

package mcpinit

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// mkfifo returns the path of a freshly created FIFO with no writer on it, or
// skips when the platform's filesystem refuses one.
func mkfifo(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript.fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	return path
}

// TestRunStopRefusesAFIFOTranscript: a stop payload whose transcript path is a
// FIFO with no writer must not block the hook — opening a FIFO for reading waits
// for a writer, which is the opposite of allowing the stop. The fail-open line
// names the open, which is where the refusal now happens.
func TestRunStopRefusesAFIFOTranscript(t *testing.T) {
	isolatedHome(t)
	path := mkfifo(t)
	input := stopInput(t, path, false)
	var errOut bytes.Buffer

	runWithDeadline(t, func() {
		RunHostEvent("stop", "claude-code", strings.NewReader(input), io.Discard, &errOut)
	})

	if !strings.Contains(errOut.String(), "open transcript") {
		t.Errorf("the refusal is not attributed to the open; stderr was %q", errOut.String())
	}
}

// TestTheAuditScanRefusesAFIFOTranscript: the audit scan opens the same path
// ahead of the nudge, and it runs at session end too, so a FIFO must be refused
// there as well. The lifecycle still spawns: an unreadable transcript costs the
// turn's audit and nothing else.
func TestTheAuditScanRefusesAFIFOTranscript(t *testing.T) {
	_, projDir := auditHookRun(t)
	spawned := captureLifecycleChild(t)
	path := mkfifo(t)
	input := stopInputIn(t, projDir, path, "claude-jsonl")
	var errOut bytes.Buffer

	runWithDeadline(t, func() {
		RunHostEvent("stop", "claude-code", strings.NewReader(input), io.Discard, &errOut)
	})

	if len(*spawned) != 1 {
		t.Fatalf("the hook started %d child(ren), want exactly 1: an unreadable transcript must not cost the lifecycle",
			len(*spawned))
	}
	if got := sidecarPathFrom(t, (*spawned)[0]); got != "" {
		t.Errorf("the child was handed a sidecar scanned from a path that is not a regular file: %q", got)
	}
	if !strings.Contains(errOut.String(), "open transcript for the audit") {
		t.Errorf("the audit refusal is not attributed to the open; stderr was %q", errOut.String())
	}
}

// A symlink to a FIFO is stat'ed through, so it is refused for the same reason
// the FIFO itself is. This is the shape a host could name without meaning to.
func TestRunStopRefusesASymlinkToAFIFOTranscript(t *testing.T) {
	isolatedHome(t)
	fifo := mkfifo(t)
	link := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.Symlink(fifo, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	input := stopInput(t, link, false)
	var errOut bytes.Buffer

	runWithDeadline(t, func() {
		RunHostEvent("stop", "claude-code", strings.NewReader(input), io.Discard, &errOut)
	})

	if !strings.Contains(errOut.String(), "open transcript") {
		t.Errorf("a symlink to a FIFO was not refused at the open; stderr was %q", errOut.String())
	}
}

// TestStopSaveMarkerFIFOIsTreatedAsNoMarker pins finding #1: a FIFO (or any
// non-regular file) at the marker path must not block or crash the hook.
func TestStopSaveMarkerFIFOIsTreatedAsNoMarker(t *testing.T) {
	isolatedHome(t)
	dataDir := filepath.Join(t.TempDir(), "data", "ghost")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sess := "fifo-sess"
	markerPath := stopSaveMarkerPath(sess, "")
	if markerPath == "" {
		t.Fatal("no marker path")
	}
	path := mkfifo(t)
	in := contractInputFor(t, "Stop", "claude-code", "claude-jsonl",
		fmt.Sprintf(`{"session_id":%q,"transcript_path":%q,"cwd":"/repo","stop_hook_active":false}`, sess, path))
	_ = runStopHook(t, in)
}
