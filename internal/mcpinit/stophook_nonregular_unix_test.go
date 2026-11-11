//go:build !windows

package mcpinit

import (
	"bytes"
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

// TestTheAuditScanRefusesAFIFOTranscript: the audit scan opens the transcript,
// and it runs at session end too, so a FIFO must be refused
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

// A symlink to a FIFO is stat'ed through, so the audit scan refuses it for the
// same reason it refuses the FIFO itself.
func TestTheAuditScanRefusesASymlinkToAFIFOTranscript(t *testing.T) {
	_, projDir := auditHookRun(t)
	spawned := captureLifecycleChild(t)
	link := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.Symlink(mkfifo(t), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	input := stopInputIn(t, projDir, link, "claude-jsonl")
	var errOut bytes.Buffer

	runWithDeadline(t, func() {
		RunHostEvent("stop", "claude-code", strings.NewReader(input), io.Discard, &errOut)
	})

	if len(*spawned) != 1 {
		t.Fatalf("the hook started %d child(ren), want exactly 1", len(*spawned))
	}
	if !strings.Contains(errOut.String(), "open transcript for the audit") {
		t.Errorf("a symlink to a FIFO was not refused at the open; stderr was %q", errOut.String())
	}
}
