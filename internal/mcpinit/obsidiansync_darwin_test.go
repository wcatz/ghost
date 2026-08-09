//go:build darwin

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
