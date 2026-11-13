//go:build linux

package mcpinit

import (
	"os"
	"testing"
)

func TestProcessStartTime_OwnPID(t *testing.T) {
	token, ok := processStartTime(os.Getpid())
	if !ok {
		t.Fatal("processStartTime(own pid) ok = false, want true")
	}
	if token == "" {
		t.Error("processStartTime(own pid) returned an empty token with ok=true")
	}
}

func TestProcessStartTime_Stable(t *testing.T) {
	a, okA := processStartTime(os.Getpid())
	b, okB := processStartTime(os.Getpid())
	if !okA || !okB {
		t.Fatal("processStartTime(own pid) failed on a repeat call")
	}
	if a != b {
		t.Errorf("processStartTime(own pid) not stable across calls: %q vs %q", a, b)
	}
}

func TestProcessStartTime_ImplausiblePID(t *testing.T) {
	if _, ok := processStartTime(999999999); ok {
		t.Error("processStartTime(implausible pid) ok = true, want false")
	}
}

func TestIsProcessAlive_TokenMatch(t *testing.T) {
	token, ok := processStartTime(os.Getpid())
	if !ok {
		t.Fatal("processStartTime(own pid) failed; can't set up test")
	}
	if !isProcessAlive(os.Getpid(), token, true) {
		t.Error("isProcessAlive(own pid, own true token) = false, want true")
	}
}

func TestIsProcessAlive_TokenMismatch(t *testing.T) {
	if isProcessAlive(os.Getpid(), "definitely-not-the-real-token", true) {
		t.Error("isProcessAlive(own pid, wrong token) = true, want false (PID-reuse must be detected)")
	}
}

func TestIsProcessAlive_NoToken(t *testing.T) {
	if !isProcessAlive(os.Getpid(), "", false) {
		t.Error("isProcessAlive(own pid, haveToken=false) = false, want true (legacy behavior preserved)")
	}
}

func TestIsProcessAlive_DeadPIDIgnoresToken(t *testing.T) {
	if isProcessAlive(999999999, "anything", true) {
		t.Error("isProcessAlive(implausible pid, ...) = true, want false")
	}
}
