package main

// The lifecycle coordinator prints its own project operand, and three of those
// sites were the last place in the package a token-shaped `--project` value came
// back out unrendered (#1087): the lock refusal and the supersede-consensus clamp
// notice print the operand to stderr, which the detached path sends to
// `lifecycle.log`, and `phaseFailureCause` records the whole argv — including
// `--project <value>` — into the failure marker, which persists for up to 30 days
// and is printed at every later session start.
//
// docs/invariants.md's #839 rule is the shape these take: `memory.ProjectArg`
// names an ordinary value (byte-identically, as `%q`) and withholds a
// credential-shaped one behind a placeholder naming the argument and the
// credential's format and no part of the value. The neighbours in the same
// function already did this; these tests hold the three that did not.

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

// TestSupersedeConsensusClampNoticeWithholdsACredentialShapedProject: the clamp
// may only be announced by this line, and the line reaches `lifecycle.log`, so a
// credential-shaped operand must be withheld while the clamp itself is still
// named.
func TestSupersedeConsensusClampNoticeWithholdsACredentialShapedProject(t *testing.T) {
	key := lifecycleCredential()
	out := supersedeConsensusClampNotice(key, 1)
	if out == "" {
		t.Fatal("clamped notice printed nothing")
	}
	if strings.Contains(out, key) {
		t.Errorf("the clamp notice printed the credential-shaped operand:\n%s", out)
	}
	if !strings.Contains(out, "withheld") {
		t.Errorf("the clamp notice did not withhold with a placeholder:\n%s", out)
	}
	if !strings.Contains(out, "UNGATED") {
		t.Errorf("the clamp notice still has to announce the clamp:\n%s", out)
	}
	// An ordinary value is still named, so the reader can find their typo.
	if got := supersedeConsensusClampNotice("ghost", 1); !strings.Contains(got, `"ghost"`) {
		t.Errorf("an ordinary project operand was not named:\n%s", got)
	}
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
