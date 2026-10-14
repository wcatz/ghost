package mcpinit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wiringOutput runs the wiring steps 5-7 exactly as Run does — the permission
// step, then the three hook reconcilers against the same settings handle — and
// returns what they printed. dryRun selects the tense under test.
func wiringOutput(t *testing.T, path, ghostBin string, dryRun bool) string {
	t.Helper()
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := ensurePermissions(&out, dryRun); err != nil {
		t.Fatalf("ensurePermissions: %v", err)
	}
	if err := ensureHook(&out, sf, ghostBin, dryRun); err != nil {
		t.Fatalf("ensureHook: %v", err)
	}
	if err := ensureStopHook(&out, sf, ghostBin, dryRun); err != nil {
		t.Fatalf("ensureStopHook: %v", err)
	}
	if err := ensureWorkingMomentHooks(&out, sf, ghostBin, dryRun); err != nil {
		t.Fatalf("ensureWorkingMomentHooks: %v", err)
	}
	return out.String()
}

// TestDryRunWiringStepsPrintWouldNotAppliedMarkers pins #1264: the wiring steps
// print the "+ " marker that means a change was written even on a run that
// writes nothing, so the preview claimed to have added permissions and hooks
// the dry run never touched. Every line a dry run prints for a change must use
// the "~ would …" form the other steps already use.
func TestDryRunWiringStepsPrintWouldNotAppliedMarkers(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	path := filepath.Join(home, ".claude", "settings.json")
	mkdirAll(t, filepath.Dir(path))
	original := []byte(`{"permissions":{"allow":["Bash"]}}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}

	got := wiringOutput(t, path, "/opt/ghost/ghost", true)

	if strings.Contains(got, "  + ") {
		t.Errorf("dry run printed an applied marker for a change it did not write:\n%s", got)
	}
	for _, want := range []string{
		"~ would add mcp__ghost__ghost_memory_save",
		"~ would add SessionStart hook",
		"~ would add Stop hook",
		"~ would add UserPromptSubmit hook",
		"~ would add PostToolUse hook",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dry run output missing %q:\n%s", want, got)
		}
	}

	// The preview must also have written nothing: settings.json is byte-identical.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Errorf("dry run changed settings.json:\n got %s\nwant %s", after, original)
	}
}

// TestApplyWiringStepsKeepAppliedMarkers is the other side: an ordinary run
// still prints the "+ " marker, so the fix cannot be "always print would".
func TestApplyWiringStepsKeepAppliedMarkers(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	path := filepath.Join(home, ".claude", "settings.json")
	mkdirAll(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(`{"permissions":{"allow":["Bash"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	got := wiringOutput(t, path, "/opt/ghost/ghost", false)

	if strings.Contains(got, "~ would ") {
		t.Errorf("an apply run printed a preview marker:\n%s", got)
	}
	for _, want := range []string{
		"  + mcp__ghost__ghost_memory_save",
		"  + added SessionStart hook",
		"  + added Stop hook",
		"  + added UserPromptSubmit hook",
		"  + added PostToolUse hook",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("apply output missing %q:\n%s", want, got)
		}
	}
}

// TestDryRunStaleHookRepairPrintsWouldMigrate pins the repair tense too: a dry
// run that would rewrite a stale ghost hook path reports "~ would migrate …",
// never the applied marker, and leaves the file alone.
func TestDryRunStaleHookRepairPrintsWouldMigrate(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	path := filepath.Join(home, ".claude", "settings.json")
	mkdirAll(t, filepath.Dir(path))

	oldBin, newBin := "/opt/old/ghost", "/opt/new/ghost"
	original := userHookFieldsSettings(
		shellQuote(oldBin)+" hook session-start --source claude-code",
		shellQuote(oldBin)+" hook stop --source claude-code",
	)
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := ensureHook(&out, sf, newBin, true); err != nil {
		t.Fatalf("ensureHook: %v", err)
	}
	if err := ensureStopHook(&out, sf, newBin, true); err != nil {
		t.Fatalf("ensureStopHook: %v", err)
	}

	got := out.String()
	if strings.Contains(got, "  + ") {
		t.Errorf("dry run printed an applied marker for a repair it did not write:\n%s", got)
	}
	if !strings.Contains(got, "~ would migrate SessionStart hook") || !strings.Contains(got, "~ would migrate Stop hook") {
		t.Errorf("dry run did not report the repair in the would form:\n%s", got)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != original {
		t.Errorf("dry run changed settings.json:\n got %s\nwant %s", after, original)
	}
}
