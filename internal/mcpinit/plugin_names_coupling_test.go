package mcpinit

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// realEnviron is the environment captured at package init — before TestMain
// redirects HOME and the XDG dirs to a temp dir. The assemble scripts run
// `go build`, which resolves GOPATH/GOMODCACHE/GOCACHE from HOME; inheriting
// the redirected HOME would cold-populate a temp module cache (~350 MB),
// make the test network-dependent, and leave read-only module dirs that
// TestMain's cleanup cannot remove.
var realEnviron = os.Environ()

// repoRoot returns the repository root (two directories above this package).
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".claude-plugin", "marketplace.json")); err != nil {
		t.Fatalf("repo root %s has no .claude-plugin/marketplace.json: %v", root, err)
	}
	return root
}

// requireBash skips the test when bash is unavailable — never a silent pass.
func requireBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not on PATH: %v", err)
	}
}

// runAssemblePOSIX runs scripts/assemble-plugin.sh with the given PLATFORMS
// and returns the assembled tree directory.
func runAssemblePOSIX(t *testing.T, platforms string) string {
	t.Helper()
	requireBash(t)
	out := filepath.Join(t.TempDir(), "posix-plugin")
	cmd := exec.Command("bash", "scripts/assemble-plugin.sh", "0.0.0", out)
	cmd.Dir = repoRoot(t)
	cmd.Env = append(append([]string{}, realEnviron...), "PLATFORMS="+platforms)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("assemble-plugin.sh: %v\n%s", err, b)
	}
	return out
}

// runAssembleWindows runs scripts/assemble-plugin-windows.sh for one arch and
// returns the assembled tree directory.
func runAssembleWindows(t *testing.T, arch string) string {
	t.Helper()
	requireBash(t)
	out := filepath.Join(t.TempDir(), "windows-plugin-"+arch)
	cmd := exec.Command("bash", "scripts/assemble-plugin-windows.sh", arch, "0.0.0", out)
	cmd.Dir = repoRoot(t)
	cmd.Env = append([]string{}, realEnviron...)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("assemble-plugin-windows.sh %s: %v\n%s", arch, err, b)
	}
	return out
}

// manifestName reads .name from an assembled tree's plugin manifest.
func manifestName(t *testing.T, tree string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(tree, ".claude-plugin", "plugin.json"))
	if err != nil {
		t.Fatalf("read assembled manifest %s: %v", tree, err)
	}
	var m struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse assembled manifest %s: %v", tree, err)
	}
	if m.Name == "" {
		t.Fatalf("manifest %s has no name field", tree)
	}
	return m.Name
}

// assertSameNames fails when the two name sets differ in either direction.
// Set equality (not subset): a marketplace entry the allow-list misses means
// `ghost mcp init` double-wires instead of deferring; an allow-list name no
// entry produces is dead deferral.
func assertSameNames(t *testing.T, aName string, a map[string]bool, bName string, b map[string]bool) {
	t.Helper()
	for n := range a {
		if !b[n] {
			t.Errorf("%s has %q but %s does not", aName, n, bName)
		}
	}
	for n := range b {
		if !a[n] {
			t.Errorf("%s has %q but %s does not", bName, n, aName)
		}
	}
}

// TestPluginNameCoupling pins the three-way name agreement: the marketplace
// catalog, the manifests the assemble scripts actually emit (including the
// windows script's ghost-windows-$ARCH rewrite), and the managedPluginNames
// allow-list PluginInstalled enforces. Drift on any side previously had no
// test and would surface only as a broken install or a lost init deferral.
func TestPluginNameCoupling(t *testing.T) {
	// Source 1: marketplace entries, exact case — Claude Code matches these
	// at install time and the assemble scripts write them verbatim, so a
	// case-only rename is real drift and must fail here.
	b, err := os.ReadFile(filepath.Join(repoRoot(t), ".claude-plugin", "marketplace.json"))
	if err != nil {
		t.Fatalf("read marketplace.json: %v", err)
	}
	var mp struct {
		Plugins []struct {
			Name string `json:"name"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(b, &mp); err != nil {
		t.Fatalf("parse marketplace.json: %v", err)
	}
	marketplace := map[string]bool{}
	for _, p := range mp.Plugins {
		marketplace[p.Name] = true
	}
	if len(marketplace) == 0 {
		t.Fatal("marketplace.json declares no plugin entries")
	}

	// Source 2: what the assemble scripts actually emit, exact case.
	assembled := map[string]bool{
		manifestName(t, runAssemblePOSIX(t, "linux-amd64")): true,
	}
	for _, arch := range []string{"amd64", "arm64"} {
		assembled[manifestName(t, runAssembleWindows(t, arch))] = true
	}

	// Source 3: the allow-list PluginInstalled enforces, whose domain is
	// strings.EqualFold — compare it case-insensitively against the folded
	// assembled names.
	allowed := map[string]bool{}
	for _, n := range managedPluginNames {
		allowed[strings.ToLower(n)] = true
	}
	assembledFolded := map[string]bool{}
	for n := range assembled {
		assembledFolded[strings.ToLower(n)] = true
	}

	assertSameNames(t, "marketplace.json", marketplace, "assembled manifests", assembled)
	assertSameNames(t, "assembled manifests", assembledFolded, "managedPluginNames", allowed)
}
