package main

import (
	"strings"
	"testing"
)

func TestDecideUpgrade(t *testing.T) {
	tests := []struct {
		name            string
		running, latest string
		want            upgradeOutcome
	}{
		{name: "same release is already current", running: "0.32.0", latest: "0.32.0", want: upgradeCurrent},
		{name: "newer patch proceeds", running: "0.32.0", latest: "0.32.1", want: upgradeProceed},
		{name: "newer minor proceeds", running: "0.32.0", latest: "0.33.0", want: upgradeProceed},
		{name: "newer major proceeds", running: "0.32.0", latest: "1.0.0", want: upgradeProceed},
		// The three refusal cases are the guard itself.
		{name: "older patch is refused", running: "0.32.1", latest: "0.32.0", want: upgradeRefuseDowngrade},
		{name: "older minor is refused", running: "0.33.0", latest: "0.32.0", want: upgradeRefuseDowngrade},
		{name: "older major is refused", running: "1.0.0", latest: "0.32.0", want: upgradeRefuseDowngrade},
		// String comparison calls all three of these a downgrade (or an
		// upgrade); only the last one is one.
		{name: "0.10.0 is newer than 0.9.0", running: "0.9.0", latest: "0.10.0", want: upgradeProceed},
		{name: "10.0.0 is newer than 9.0.0", running: "9.0.0", latest: "10.0.0", want: upgradeProceed},
		{name: "a v-prefixed tag is not a different release", running: "0.32.0", latest: "v0.32.0", want: upgradeCurrent},
		{name: "a final release supersedes its rc", running: "0.33.0-rc.1", latest: "0.33.0", want: upgradeProceed},
		{name: "an rc is older than its final release", running: "0.33.0", latest: "0.33.0-rc.1", want: upgradeRefuseDowngrade},
		// An unorderable version keeps the pre-guard behaviour: go ahead, and
		// let the checksum decide.
		{name: "dev build proceeds against a real release", running: "dev", latest: "0.32.0", want: upgradeProceed},
		{name: "dev build is never blocked", running: "dev", latest: "0.0.1", want: upgradeProceed},
		{name: "empty running version proceeds", running: "", latest: "0.32.0", want: upgradeProceed},
		{name: "unorderable release tag proceeds", running: "0.32.0", latest: "nightly", want: upgradeProceed},
		// Unorderable but identical is still the release already installed.
		{name: "identical unorderable version is current", running: "nightly", latest: "nightly", want: upgradeCurrent},
		{name: "identical unorderable version with v prefix is current", running: "nightly", latest: "vnightly", want: upgradeCurrent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decideUpgrade(tt.running, tt.latest); got != tt.want {
				t.Errorf("decideUpgrade(%q, %q) = %v, want %v", tt.running, tt.latest, got, tt.want)
			}
		})
	}
}

func TestDowngradeMessageNamesBothVersions(t *testing.T) {
	msg := downgradeMessage("0.33.0", "0.32.0")
	for _, want := range []string{"0.33.0", "0.32.0", "downgrade"} {
		if !strings.Contains(msg, want) {
			t.Errorf("downgrade message %q should mention %q", msg, want)
		}
	}
}
