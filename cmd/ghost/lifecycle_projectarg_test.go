package main

import (
	"errors"
	"strings"
	"testing"
)

// lifecycleCredential is a value shaped like the Anthropic API key an agent
// produces by pasting a config value into --project, assembled rather than
// written whole so no push-protection rule matches it in the diff.
func lifecycleCredential() string {
	return "sk-ant-api03-" + strings.Repeat("Zz9", 13) + "Q"
}

// TestPhaseFailureArgvWithholdsACredentialShapedProject: the silent-death
// fallback records argv into the durable marker, so the `--project` value must be
// rendered while every other argument survives intact.
func TestPhaseFailureArgvWithholdsACredentialShapedProject(t *testing.T) {
	key := lifecycleCredential()
	got := phaseFailureCause(
		[]string{"supersede", "--project", key, "--apply"},
		errors.New("exit status 1"),
		"",
	)
	if strings.Contains(got, key) {
		t.Errorf("the failure marker argv printed the credential-shaped operand:\n%s", got)
	}
	if !strings.Contains(got, "withheld") {
		t.Errorf("the marker argv did not withhold with a placeholder:\n%s", got)
	}
	if !strings.Contains(got, "--apply") {
		t.Errorf("a neighbouring argument was dropped from the marker argv:\n%s", got)
	}
	// An ordinary operand is still rendered, quoted like every other project
	// argument surface, so the marker names the phase's scope.
	ordinary := phaseFailureCause(
		[]string{"reflect", "--project", "ghost", "--apply"},
		errors.New("exit status 1"),
		"",
	)
	if !strings.Contains(ordinary, `--project "ghost"`) {
		t.Errorf("an ordinary project operand was not rendered:\n%s", ordinary)
	}
}

// TestLifecycleLockRefusalWithholdsACredentialShapedProject: the refusal is
// printed to stderr, which the detached path appends to `lifecycle.log`, so the
// operand goes through the same renderer.
func TestLifecycleLockRefusalWithholdsACredentialShapedProject(t *testing.T) {
	key := lifecycleCredential()
	out := lifecycleLockRefusal(key)
	if strings.Contains(out, key) {
		t.Errorf("the lock refusal printed the credential-shaped operand:\n%s", out)
	}
	if !strings.Contains(out, "already running") {
		t.Errorf("the lock refusal no longer says a run is in progress:\n%s", out)
	}
	if !strings.Contains(out, "withheld") {
		t.Errorf("the lock refusal did not withhold with a placeholder:\n%s", out)
	}
	if got := lifecycleLockRefusal("ghost"); !strings.Contains(got, `"ghost"`) {
		t.Errorf("an ordinary project operand was not named:\n%s", got)
	}
}
