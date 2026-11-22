package mcpinit

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsurePreCompactHookIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	ghostBin := "/opt/ghost/ghost"

	var out strings.Builder
	if err := ensurePreCompactHook(&out, sf, ghostBin, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "added PreCompact hook") {
		t.Errorf("first init did not add the hook:\n%s", out.String())
	}
	if !contractHookWired(sf, "PreCompact", "pre-compact", "claude-code", ghostBin) {
		t.Error("the pre-compact hook does not call the contract invocation")
	}

	out.Reset()
	if err := ensurePreCompactHook(&out, sf, ghostBin, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "already configured") {
		t.Errorf("second init was not idempotent:\n%s", out.String())
	}
	entries, _, err := sf.hookEntries("PreCompact")
	if err != nil || len(entries) != 1 {
		t.Errorf("%d pre-compact entries after two inits (err %v), want 1", len(entries), err)
	}
}

func TestHoldOnlyGhostManagedKnowsThePreCompactHook(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := ensureHook(&out, sf, "/opt/ghost/ghost", false); err != nil {
		t.Fatal(err)
	}
	if err := ensureStopHook(&out, sf, "/opt/ghost/ghost", false); err != nil {
		t.Fatal(err)
	}
	if err := ensurePreCompactHook(&out, sf, "/opt/ghost/ghost", false); err != nil {
		t.Fatal(err)
	}
	perms, _ := json.Marshal(map[string]any{"allow": ghostPermissions})
	sf.setRaw("permissions", perms)
	if _, err := sf.setAutoMemoryEnabled(false); err != nil {
		t.Fatal(err)
	}
	if !sf.holdsOnlyGhostManaged() {
		t.Error("a settings file holding Ghost's own pre-compact hook is not recognised as Ghost-managed")
	}
}
