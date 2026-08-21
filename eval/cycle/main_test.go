package main

import (
	"strings"
	"testing"
)

// TestScratchEnvIsolates pins the isolation contract: no pre-existing XDG
// overrides or ANTHROPIC_API_KEY may leak into child processes, and both XDG
// vars must point into the scratch tree.
func TestScratchEnvIsolates(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/leak/data")
	t.Setenv("XDG_CONFIG_HOME", "/leak/config")
	t.Setenv("ANTHROPIC_API_KEY", "sk-leak")
	env := scratchEnv("/scratch")
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "/leak/") || strings.Contains(joined, "ANTHROPIC_API_KEY=") {
		t.Fatalf("leaked env: %s", joined)
	}
	if !strings.Contains(joined, "XDG_DATA_HOME=/scratch/data") ||
		!strings.Contains(joined, "XDG_CONFIG_HOME=/scratch/config") {
		t.Fatalf("missing scratch overrides: %s", joined)
	}
}
