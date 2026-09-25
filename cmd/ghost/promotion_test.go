package main

import (
	"strings"
	"testing"
)

func TestRecoveryWarningDoesNotClaimZeroRowsWereKept(t *testing.T) {
	zero := recoveryWarning(0, 3)
	if !strings.Contains(zero, "none of the 3") || !strings.Contains(zero, "error:") {
		t.Errorf("zero-recovery warning = %q, want an explicit error rather than a false success count", zero)
	}
	if strings.Contains(zero, "0 of 3") {
		t.Errorf("zero-recovery warning still claims rows were kept: %q", zero)
	}

	partial := recoveryWarning(2, 3)
	if partial != "warning: 2 of 3 global memories could not be promoted and were kept in the project" {
		t.Errorf("partial-recovery warning = %q", partial)
	}
}
