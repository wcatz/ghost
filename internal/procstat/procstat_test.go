package procstat

import (
	"os"
	"testing"
)

func TestCheckOwnProcessIsAlive(t *testing.T) {
	if got := Check(os.Getpid(), "", false); got != StateAlive {
		t.Fatalf("Check(own pid) = %v, want StateAlive", got)
	}
}

func TestCheckImplausiblePIDIsDead(t *testing.T) {
	if got := Check(999999999, "", false); got != StateDead {
		t.Fatalf("Check(implausible pid) = %v, want StateDead", got)
	}
}
