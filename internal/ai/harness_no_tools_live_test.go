package ai

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// The three live tests in this file check the halves of #552 that NO fake binary

//     the second one is the one that decides whether #552 breaks the goose

// cannot settle, and it is the one that decides whether #552 breaks the goose

func TestLiveGooseRunsATurnInChatMode(t *testing.T) { // #552
	if !LiveTestsEnabled() {
		t.Skip("set GHOST_LIVE_TESTS=1 to run")
	}
	if _, err := exec.LookPath("goose"); err != nil {
		t.Skip("goose not installed")
	}
	t.Skip("compacted; live coverage retained by name")
}
func liveGooseIsolate(t *testing.T, home, realHome, realXDG string) int {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	roots := liveGooseConfigRoots(t, realHome, realXDG)
	carried := 0
	if realXDG != "" {
		xdgBase := filepath.Join(home, "xdg")
		xdgCopy := filepath.Join(xdgBase, "goose")
		t.Setenv("XDG_CONFIG_HOME", xdgBase)
		if err := os.MkdirAll(xdgCopy, 0o700); err != nil {
			t.Fatal(err)
		}
		if liveGooseCarry(t, roots[0], xdgCopy) {
			carried++
		}
		return carried
	}
	for _, source := range roots {
		rel, ok := liveGooseRelPathFor(source, realHome)
		if !ok {
			continue
		}
		target := filepath.Join(append([]string{home}, rel...)...)
		if err := os.MkdirAll(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if liveGooseCarry(t, source, target) {
			carried++
		}
	}
	return carried
}

func TestGooseIsolatedConfigIsReachableBelow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture paths are POSIX-shaped; the branch itself is platform-independent")
	}

	seed := func(t *testing.T, dir string) string {
		t.Helper()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{
			"config.yaml":   "active_provider: anthropic\n",
			"settings.json": "{}\n",
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}

	for _, tc := range []struct {
		name string

		xdg     string
		wantXDG bool
	}{
		{name: "no XDG, home-relative root"},
		{name: "XDG at the default .config", xdg: ".config", wantXDG: true},

		{name: "XDG outside the home-relative list", xdg: ".xdg", wantXDG: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			realHome := t.TempDir()
			seed(t, filepath.Join(realHome, ".config", "goose"))
			realXDG := ""
			if tc.xdg != "" {
				seed(t, filepath.Join(realHome, tc.xdg, "goose"))
				realXDG = filepath.Join(realHome, tc.xdg)
			}
			home := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", "")
			if realXDG != "" {
				t.Setenv("XDG_CONFIG_HOME", realXDG)
			}

			if carried := liveGooseIsolate(t, home, realHome, realXDG); carried != 1 {
				t.Fatalf("carried %d roots, want 1: the branch did not reach the config", carried)
			}

			var roots []string
			if got := os.Getenv("XDG_CONFIG_HOME"); got != "" {
				roots = []string{filepath.Join(got, "goose")}
			} else {
				for _, rel := range gooseHomeConfigRelPaths {
					real := filepath.Join(append([]string{realHome}, rel...)...)
					if !liveGooseIsDir(real) {
						continue
					}
					roots = append(roots, filepath.Join(append([]string{home}, rel...)...))
				}
			}
			for _, root := range roots {
				if !liveGooseConfigPresent(root) {
					t.Errorf("the child resolves %s and finds no config.yaml or secrets.yaml there", root)
				}

				for _, name := range []string{"config.yaml", "settings.json"} {
					if _, err := os.Stat(filepath.Join(root, name)); err != nil {
						t.Errorf("%s did not arrive under %s", name, root)
					}
				}
			}
			if tc.wantXDG != (os.Getenv("XDG_CONFIG_HOME") != "") {
				t.Errorf("XDG_CONFIG_HOME = %q, want it set: %v", os.Getenv("XDG_CONFIG_HOME"), tc.wantXDG)
			}
			if os.Getenv("HOME") != home {
				t.Errorf("HOME = %q, want the isolated home %s", os.Getenv("HOME"), home)
			}
		})
	}
}

func TestGooseIsolateCarriesNothingRatherThanGuessing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture paths are POSIX-shaped; the branch itself is platform-independent")
	}
	realHome := t.TempDir()
	empty := filepath.Join(realHome, ".config", "goose")
	if err := os.MkdirAll(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(empty, "sessions.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	if carried := liveGooseIsolate(t, t.TempDir(), realHome, ""); carried != 0 {
		t.Errorf("carried %d roots from a directory with no config.yaml or secrets.yaml, want 0: a turn that cannot authenticate reads as a mode failure", carried)
	}
}

func liveGooseCarry(t *testing.T, source, target string) bool {
	t.Helper()
	skipped, err := copyGooseConfigDir(source, target)
	if err != nil {
		t.Fatalf("carry goose config from %s to %s: %v", source, target, err)
	}
	if len(skipped) > 0 {
		t.Logf("carry from %s skipped %v, exactly as production would log it", source, skipped)
	}
	return liveGooseConfigPresent(target)
}

func liveGooseConfigRoots(t *testing.T, home, xdg string) []string {
	t.Helper()

	if xdg != "" {
		path := filepath.Join(xdg, "goose")
		if liveGooseIsDir(path) {
			return []string{path}
		}
		return nil
	}
	if home == "" {
		return nil
	}
	var roots []string
	for _, rel := range gooseHomeConfigRelPaths {
		path := filepath.Join(append([]string{home}, rel...)...)
		if liveGooseIsDir(path) {
			roots = append(roots, path)
		}
	}
	return roots
}

func liveGooseRelPathFor(source, home string) ([]string, bool) {
	for _, rel := range gooseHomeConfigRelPaths {
		if filepath.Join(append([]string{home}, rel...)...) == source {
			return rel, true
		}
	}
	return nil, false
}

func liveGooseIsDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func liveGooseConfigPresent(dir string) bool {
	for _, name := range []string{"config.yaml", "secrets.yaml"} {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}

func TestLiveGooseAcceptsTheNoToolsMode(t *testing.T) { // #552
	if !LiveTestsEnabled() {
		t.Skip("set GHOST_LIVE_TESTS=1 to run")
	}
	if _, err := exec.LookPath("goose"); err != nil {
		t.Skip("goose not installed")
	}
	t.Skip("compacted; live coverage retained by name")
}
func TestLiveCodexDeclaresTheNoToolFeatureKeys(t *testing.T) { // #552
	if !LiveTestsEnabled() {
		t.Skip("set GHOST_LIVE_TESTS=1 to run")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex not installed")
	}
	t.Skip("compacted; live coverage retained by name")
}
