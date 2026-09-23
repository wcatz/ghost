# Plugin Hardening Pass Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One PR clustering `34CD5E1E` (three-way plugin-name coupling test), `A5E6A856` (disclosure text, home-anchored cache-path check, `0.0.0` documentation), and `7FFBDD03` (Windows x64/ARM64 automated verification).

**Architecture:** A package-level `managedPluginNames` slice becomes the single assertable source for `PluginInstalled`, and a new coupling test executes the real `assemble-plugin*.sh` scripts to prove marketplace entries, assembled manifest names, and that slice agree as sets. A cross-platform e2e test assembles the host's plugin tree, round-trips it through a zip, and drives the extracted entry point through the MCP stdio protocol, both hooks, and `mcp init` deferral; a new two-arch Windows CI job runs the package on real Windows hosts. Disclosure text goes into every user-visible plugin description.

**Tech Stack:** Go (stdlib `archive/zip`, `os/exec`), bash assemble scripts, jq, GitHub Actions `windows-latest` + `windows-11-arm`.

**Spec:** `docs/superpowers/specs/2026-09-23-plugin-hardening-design.md` (committed as `670851c` on `feat/plugin-hardening`).

**Notes for every task:** commit with `git commit -s` (GPG signing comes from repo config; if signing fails with "No passphrase given", stop and report — never pass `--no-gpg-sign`). Stage only the named paths. Tests live in `internal/mcpinit`, whose `TestMain` (hook_test.go:41) already isolates `HOME`/`XDG_CONFIG_HOME`/`XDG_DATA_HOME` for the whole package run.

---

### Task 1: Name-coupling test + `managedPluginNames` extraction

**Files:**
- Create: `internal/mcpinit/plugin_names_coupling_test.go`
- Modify: `internal/mcpinit/plugin.go` (allow-list switch at lines 82–95)

- [ ] **Step 1: Write the failing test**

Create `internal/mcpinit/plugin_names_coupling_test.go`:

```go
package mcpinit

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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

// runAssemblePOSIX runs scripts/assemble-plugin.sh with the given PLATFORMS
// and returns the assembled tree directory. Skips (never silently passes)
// when bash is unavailable.
func runAssemblePOSIX(t *testing.T, platforms string) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not on PATH: %v", err)
	}
	out := filepath.Join(t.TempDir(), "posix-plugin")
	cmd := exec.Command("bash", "scripts/assemble-plugin.sh", "0.0.0", out)
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(), "PLATFORMS="+platforms)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("assemble-plugin.sh: %v\n%s", err, b)
	}
	return out
}

// runAssembleWindows runs scripts/assemble-plugin-windows.sh for one arch and
// returns the assembled tree directory.
func runAssembleWindows(t *testing.T, arch string) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not on PATH: %v", err)
	}
	out := filepath.Join(t.TempDir(), "windows-plugin-"+arch)
	cmd := exec.Command("bash", "scripts/assemble-plugin-windows.sh", arch, "0.0.0", out)
	cmd.Dir = repoRoot(t)
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
	// Source 1: marketplace entries.
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
		marketplace[strings.ToLower(p.Name)] = true
	}
	if len(marketplace) == 0 {
		t.Fatal("marketplace.json declares no plugin entries")
	}

	// Source 2: what the assemble scripts actually emit.
	assembled := map[string]bool{
		strings.ToLower(manifestName(t, runAssemblePOSIX(t, "linux-amd64"))): true,
	}
	for _, arch := range []string{"amd64", "arm64"} {
		assembled[strings.ToLower(manifestName(t, runAssembleWindows(t, arch)))] = true
	}

	// Source 3: the allow-list PluginInstalled enforces.
	allowed := map[string]bool{}
	for _, n := range managedPluginNames {
		allowed[strings.ToLower(n)] = true
	}

	assertSameNames(t, "marketplace.json", marketplace, "assembled manifests", assembled)
	assertSameNames(t, "assembled manifests", assembled, "managedPluginNames", allowed)
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/mcpinit/ -run TestPluginNameCoupling -count=1`
Expected: COMPILE FAIL with `undefined: managedPluginNames`

- [ ] **Step 3: Extract the allow-list in `plugin.go`**

Replace the comment block + switch inside `PluginInstalled` (current lines 82–95) — the `for key := range doc.Plugins { ... }` body — with:

```go
	for key := range doc.Plugins {
		// Keys are "<plugin>@<marketplace>". Match the exact Ghost-managed
		// set case-insensitively without claiming unrelated plugins whose
		// names merely start with "ghost-".
		name, _, _ := strings.Cut(key, "@")
		for _, n := range managedPluginNames {
			if strings.EqualFold(name, n) {
				return true
			}
		}
	}
	return false
}
```

And add the slice just above `PluginInstalled` (after `isUnderPluginCache`):

```go
// managedPluginNames is the exact set of Ghost-managed plugin names that
// PluginInstalled defers to: the POSIX plugin "ghost" plus the per-arch
// native-Windows entries "ghost-windows-amd64" and "ghost-windows-arm64",
// each of which bundles one .exe. The set must agree with
// .claude-plugin/marketplace.json and with the manifests the assemble
// scripts emit — TestPluginNameCoupling enforces that equality.
var managedPluginNames = []string{"ghost", "ghost-windows-amd64", "ghost-windows-arm64"}
```

(The old comment above the loop — "Keys are `<plugin>@<marketplace>`. The POSIX plugin is..." — is superseded by the slice doc; delete it so the rationale lives in one place.)

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/mcpinit/ -run 'TestPluginNameCoupling|TestPluginInstalledRegistryNames' -count=1 -v`
Expected: PASS — coupling test builds all three trees (3 cross-compiles, seconds) and both name sets match; the existing registry-name table stays green (behavior unchanged).

- [ ] **Step 5: Commit**

```bash
git add internal/mcpinit/plugin.go internal/mcpinit/plugin_names_coupling_test.go
git commit -s -m "test(plugin): assert marketplace, assemble, and allow-list names agree"
```

---

### Task 2: Assemble-script Python fallback + LF checkout pinning

**Files:**
- Modify: `scripts/assemble-plugin.sh` (final JSON-validation block, lines 67–69)
- Modify: `scripts/assemble-plugin-windows.sh` (final JSON-validation block, lines 69–71)
- Create: `.gitattributes`

- [ ] **Step 1: Apply the identical fallback to both scripts**

In BOTH files, replace:

```bash
# The stamped manifest must still be valid JSON.
python3 -c "import json,sys; json.load(open('$MANIFEST'))" \
  || { echo "error: assembled plugin.json is not valid JSON" >&2; exit 1; }
```

with:

```bash
# The stamped manifest must still be valid JSON. Resolve python3 or python:
# Windows runners (Git Bash) expose only `python`.
PY="$(command -v python3 || command -v python || true)"
[ -n "$PY" ] || { echo "error: python3 or python is required to validate the manifest" >&2; exit 1; }
"$PY" -c "import json,sys; json.load(open('$MANIFEST'))" \
  || { echo "error: assembled plugin.json is not valid JSON" >&2; exit 1; }
```

- [ ] **Step 2: Create `.gitattributes`**

```gitattributes
# Shell scripts must check out with LF: a CRLF checkout makes Git Bash on
# Windows fail with "`\r': command not found" the first time an assemble or
# launcher script runs there.
*.sh text eol=lf
plugin/bin/ghost-launcher text eol=lf
```

- [ ] **Step 3: Verify the fallback logic both ways**

Run: `bash -c 'PY="$(command -v python3 || command -v python || true)"; [ -n "$PY" ] && echo "resolved: $PY"'`
Expected: `resolved: /usr/bin/python3` (or equivalent). The `python`-only branch is proven on the Windows CI runners in Task 6.

- [ ] **Step 4: Run the real scripts end-to-end (exercises the python3 branch + JSON check)**

Run: `PLATFORMS=linux-amd64 scripts/assemble-plugin.sh 0.0.0 /tmp/ghost-posix-check && scripts/assemble-plugin-windows.sh amd64 0.0.0 /tmp/ghost-win-check`
Expected: both print their `assembled ... at ...` line; exit 0.

- [ ] **Step 5: Commit**

```bash
git add scripts/assemble-plugin.sh scripts/assemble-plugin-windows.sh .gitattributes
git commit -s -m "fix(scripts): accept python for manifest JSON checks on Windows"
```

---

### Task 3: Home-anchored `isUnderPluginCache`

**Files:**
- Modify: `internal/mcpinit/plugin.go` (`isUnderPluginCache`, lines 51–57; add `"runtime"` import)
- Modify: `internal/mcpinit/plugin_test.go` (`TestIsUnderPluginCache`, lines 11–26; add `"runtime"` import)

- [ ] **Step 1: Write the failing tests**

Replace `TestIsUnderPluginCache` in `plugin_test.go` entirely with:

```go
// TestIsUnderPluginCache pins the home-anchored match: only paths inside the
// current user's Claude Code plugin cache count, a /.claude/plugins/ segment
// under some OTHER root never does (it used to false-positive and refuse
// `ghost upgrade`), and when the home directory cannot be resolved the
// historical substring match stands.
func TestIsUnderPluginCache(t *testing.T) {
	t.Run("anchored to the resolved home", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		inHome := filepath.ToSlash(filepath.Join(home, ".claude", "plugins", "ghost", "1.0.0", "bin", "ghost"))
		elsewhere := filepath.ToSlash(filepath.Join(home, "other-root", ".claude", "plugins", "ghost", "1.0.0", "bin", "ghost"))
		cases := []struct {
			path string
			want bool
		}{
			{inHome, true},
			{"/usr/local/bin/ghost", false},
			{"/home/u/.claude/settings.json", false},
			// The audit's false positive: same segment, foreign root.
			{elsewhere, false},
		}
		for _, tc := range cases {
			if got := isUnderPluginCache(tc.path); got != tc.want {
				t.Errorf("isUnderPluginCache(%q) = %v, want %v", tc.path, got, tc.want)
			}
		}
	})

	t.Run("windows-shaped paths under the same home", func(t *testing.T) {
		t.Setenv("HOME", `C:\Users\u`)
		t.Setenv("USERPROFILE", `C:\Users\u`)
		if !isUnderPluginCache(`C:\Users\u\.claude\plugins\ghost\1.0.0\bin\ghost.exe`) {
			t.Error("want true for the plugin cache under the resolved home")
		}
		if isUnderPluginCache(`D:\other\.claude\plugins\ghost\bin\ghost.exe`) {
			t.Error("want false for a foreign root even with backslashes")
		}
	})

	t.Run("case-insensitive prefix on Windows", func(t *testing.T) {
		if runtime.GOOS != "windows" {
			t.Skip("the case-folding branch only runs on Windows (covered by the windows CI job)")
		}
		t.Setenv("USERPROFILE", `C:\Users\u`)
		if !isUnderPluginCache(`C:/USERS/U/.CLAUDE/PLUGINS/ghost/1.0.0/bin/ghost.exe`) {
			t.Error("want true for a differently-cased executable path on Windows")
		}
	})

	t.Run("substring fallback when home is unresolvable", func(t *testing.T) {
		t.Setenv("HOME", "")
		t.Setenv("USERPROFILE", "")
		if !isUnderPluginCache("/x/.claude/plugins/ghost/bin/ghost") {
			t.Error("want historical substring match when UserHomeDir fails")
		}
		if isUnderPluginCache("/usr/local/bin/ghost") {
			t.Error("want false for a non-cache path even in the fallback")
		}
	})
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/mcpinit/ -run TestIsUnderPluginCache -count=1 -v`
Expected: FAIL — `anchored to the resolved home/elsewhere` reports `= true, want false` (and `windows-shaped/foreign root` likewise), because the current implementation is a substring match.

- [ ] **Step 3: Tighten the implementation**

Replace `isUnderPluginCache` in `plugin.go` with:

```go
// isUnderPluginCache reports whether p lies inside the current user's Claude
// Code plugin cache. Path separators are normalized to forward slashes so the
// check is testable cross-platform and holds on Windows, whose paths use
// backslashes. The prefix is anchored to the home directory so an unrelated
// path that merely contains /.claude/plugins/ never matches; when the home
// directory cannot be resolved, the historical substring match stands so the
// plugin gate never silently weakens.
//
// The anchor is compared in two spellings — the lexical $HOME and its
// symlink-resolved form — because os.Executable() is symlink-resolved on
// Linux (and Claude Code may hand either form): a $HOME containing a symlink
// component must still recognize the cache under either spelling, or a genuine
// plugin binary would stop being recognized and `ghost upgrade` would overwrite
// it. Both roots stay anchored to this user's home, so a foreign
// /.claude/plugins/ root never matches. The gate is deliberately
// per-current-user: under sudo an exe in another user's cache is not this
// user's plugin cache, so that cross-user true→false is intended.
func isUnderPluginCache(p string) bool {
	s := strings.ReplaceAll(filepath.ToSlash(p), `\`, "/")
	home, err := os.UserHomeDir()
	if err != nil {
		// Home cannot be resolved — historical substring match, unchanged.
		return strings.Contains(s, pluginCacheDir)
	}
	roots := []string{slashJoin(home)}
	if resolved, rerr := filepath.EvalSymlinks(home); rerr == nil && resolved != home {
		roots = append(roots, slashJoin(resolved))
	}
	for _, root := range roots {
		if runtime.GOOS == "windows" {
			// The executable path's casing can differ from %USERPROFILE%.
			if strings.HasPrefix(strings.ToLower(s), strings.ToLower(root)) {
				return true
			}
			continue
		}
		if strings.HasPrefix(s, root) {
			return true
		}
	}
	return false
}
```

Add `"runtime"` to the file's import block and add the `slashJoin` helper below `isUnderPluginCache` (the snippet depends on it). Keep the `pluginCacheDir` const (the fallback still uses it).

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/mcpinit/ -run 'TestIsUnderPluginCache|TestRunningAsPlugin|TestPluginInstalledRegistryNames|TestFinalizePlugin' -count=1`
Expected: PASS — new table green, existing detection/marker tests unchanged.

- [ ] **Step 5: Commit**

```bash
git add internal/mcpinit/plugin.go internal/mcpinit/plugin_test.go
git commit -s -m "fix(plugin): anchor plugin-cache path detection to home"
```

---

### Task 4: Disclosure text + `0.0.0` documentation

**Files:**
- Modify: `.claude-plugin/marketplace.json` (3 entry descriptions)
- Modify: `plugin/.claude-plugin/plugin.json` (description)
- Modify: `plugin-windows/.claude-plugin/plugin.json` (description)
- Modify: `README.md` (plugin paragraph, line ~42)
- Modify: `scripts/assemble-plugin.sh` (header comment, after the Usage block)

- [ ] **Step 1: Append the disclosure to the marketplace entries and both plugin manifests**

Define the suffix once, then apply it to all five description strings (three marketplace entries + two manifests):

```bash
SUFFIX=$(cat <<'EOF'
 First-run setup is automatic and persistent: it disables Claude Code's built-in file memory, imports memories from projects Ghost already knows (others import on first use), and writes MEMORY.md redirects in known projects — these changes remain if you uninstall the plugin. The stop-hook save reminder is on by default; the LLM consolidation passes (`reflect`/`resolve`/`supersede`) are opt-in and off by default.
EOF
)
jq --arg s "$SUFFIX" '(.plugins[].description) |= . + $s' .claude-plugin/marketplace.json > /tmp/mp.json \
  && mv /tmp/mp.json .claude-plugin/marketplace.json
jq --arg s "$SUFFIX" '.description += $s' plugin/.claude-plugin/plugin.json > /tmp/p1.json \
  && mv /tmp/p1.json plugin/.claude-plugin/plugin.json
jq --arg s "$SUFFIX" '.description += $s' plugin-windows/.claude-plugin/plugin.json > /tmp/p2.json \
  && mv /tmp/p2.json plugin-windows/.claude-plugin/plugin.json
```

- [ ] **Step 2: Edit the README paragraph**

In `README.md`, replace:

```markdown
The plugin declares the MCP server and both hooks itself, then finalizes on your first session (disables Claude's built-in file memory, imports existing memories, writes project redirects). Updates flow through `/plugin update`.
```

with:

```markdown
The plugin declares the MCP server and both hooks itself, then finalizes on your first session (disables Claude's built-in file memory, imports existing memories, writes project redirects) — these first-run changes persist if you uninstall the plugin. The stop-hook save reminder is on by default; the LLM consolidation passes (`reflect`/`resolve`/`supersede`) are opt-in and off by default. Updates flow through `/plugin update`. `ghost mcp init` remains the path for opencode, codex, goose, and manual MCP setups.
```

- [ ] **Step 3: Document the `0.0.0` placeholder in the assemble-script header**

In `scripts/assemble-plugin.sh`, insert after the `# PLATFORMS may be overridden...` example block (before `set -euo pipefail`):

```bash
# The checked-in plugin manifests carry version 0.0.0 so a source-checkout
# install (`claude --plugin-dir plugin`) is honest about having no release
# version; this script stamps the real release version into the assembled
# copy (assemble-plugin-windows.sh does the same for its manifests).
```

- [ ] **Step 4: Validate JSON and verify the text landed everywhere**

Run:

```bash
for f in .claude-plugin/marketplace.json plugin/.claude-plugin/plugin.json plugin-windows/.claude-plugin/plugin.json; do jq -e . "$f" >/dev/null || exit 1; done
jq -r '[.plugins[].description] | map(select(contains("opt-in and off by default"))) | length' .claude-plugin/marketplace.json
grep -c "opt-in and off by default" plugin/.claude-plugin/plugin.json plugin-windows/.claude-plugin/plugin.json README.md
```

Expected: JSON all valid; `3` from jq (all three entries); each grep reports `1`.

- [ ] **Step 5: Review the diff and commit**

Run: `git diff --stat`
Expected: only the five files listed above.

```bash
git add .claude-plugin/marketplace.json plugin/.claude-plugin/plugin.json plugin-windows/.claude-plugin/plugin.json README.md scripts/assemble-plugin.sh
git commit -s -m "docs(plugin): disclose finalize side effects and opt-in defaults"
```

---

### Task 5: Cross-platform plugin e2e test

**Files:**
- Create: `internal/mcpinit/plugin_e2e_test.go`

Reuses Task 1's `repoRoot`/`runAssemblePOSIX`/`runAssembleWindows`, the package's `isolatedHome` (stophook_test.go:332), `seedProject` (lifecyclelock_test.go:18), and the transcript line fixtures (stophook_test.go:69–72).

- [ ] **Step 1: Write the e2e test**

Create `internal/mcpinit/plugin_e2e_test.go`:

```go
package mcpinit

import (
	"archive/zip"
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// entryPoint returns the executable hooks.json names for the current OS:
// the POSIX launcher, or ghost.exe on native Windows.
func entryPoint(t *testing.T, tree string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return filepath.Join(tree, "bin", "ghost.exe")
	}
	return filepath.Join(tree, "bin", "ghost-launcher")
}

// writeRootZip zips src with entries rooted at the archive top level — the
// layout release.yml produces with `(cd dir && zip -qr ../x.zip .)`.
func writeRootZip(src, dst string) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	walkErr := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		name := filepath.ToSlash(rel)
		if d.IsDir() {
			_, err := zw.Create(name + "/")
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(info.Mode())
		w, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		srcf, err := os.Open(p)
		if err != nil {
			return err
		}
		defer srcf.Close()
		_, err = io.Copy(w, srcf)
		return err
	})
	if walkErr != nil {
		_ = zw.Close()
		return walkErr
	}
	return zw.Close()
}

// extractRootZip extracts an archive written by writeRootZip. Names come from
// our own archive, so no foreign-path guard is needed.
func extractRootZip(src, dst string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, h := range r.File {
		p := filepath.Join(dst, filepath.FromSlash(h.Name))
		if h.FileInfo().IsDir() || strings.HasSuffix(h.Name, "/") {
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		rc, err := h.Open()
		if err != nil {
			return err
		}
		mode := h.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			_ = rc.Close()
			return err
		}
		if _, err := io.Copy(f, rc); err != nil {
			_ = rc.Close()
			_ = f.Close()
			return err
		}
		if err := rc.Close(); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	return nil
}

// zipRoundTrip zips the assembled tree and extracts it fresh: everything the
// e2e test executes comes from the extracted, installable layout.
func zipRoundTrip(t *testing.T, tree string) string {
	t.Helper()
	zipPath := filepath.Join(t.TempDir(), "ghost-plugin.zip")
	if err := writeRootZip(tree, zipPath); err != nil {
		t.Fatalf("zip assembled tree: %v", err)
	}
	extract := filepath.Join(t.TempDir(), "extracted")
	if err := extractRootZip(zipPath, extract); err != nil {
		t.Fatalf("extract plugin zip: %v", err)
	}
	return extract
}

// insertE2EMemory adds the marker memory the session-start assertion looks
// for, in the project seedProject created.
func insertE2EMemory(t *testing.T, dataHome string) {
	t.Helper()
	db, err := memory.OpenDB(filepath.Join(dataHome, "ghost", "ghost.db"))
	if err != nil {
		t.Fatalf("open seeded db: %v", err)
	}
	defer db.Close() //nolint:errcheck
	if _, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance)
		VALUES ('e2emem01', 'e2e-proj', 'fact', 'E2E_SEED_MARKER context from disk', 'manual', 0.99)`); err != nil {
		t.Fatalf("insert e2e memory: %v", err)
	}
}

// runE2EHook runs the assembled tree's hook entry exactly as hooks.json wires
// it (args identical, plugin env pointing at the tree). home isolates
// ~/.claude; dataDir is shared across runs so the finalize marker persists.
func runE2EHook(t *testing.T, tree, dataDir, home, event, stdin string) (stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(entryPoint(t, tree), "hook", event, "--source", "claude-code")
	cmd.Env = append(os.Environ(),
		"CLAUDE_PLUGIN_ROOT="+tree,
		"CLAUDE_PLUGIN_DATA="+dataDir,
		"HOME="+home,
		"USERPROFILE="+home,
	)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("hook %s failed: %v\nstderr: %s", event, err, errb.String())
	}
	return out.String(), errb.String()
}

// TestPluginE2E drives an installed plugin tree end to end: zip round-trip,
// MCP stdio handshake, both lifecycle hooks, and mcp init deferral. It runs
// on every platform's CI (linux proof) and on the windows-latest /
// windows-11-arm legs (native ghost.exe proof).
func TestPluginE2E(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not on PATH: %v", err)
	}

	// Assemble the host's tree, then execute only from the zip round-trip.
	var tree string
	if runtime.GOOS == "windows" {
		tree = runAssembleWindows(t, runtime.GOARCH)
	} else {
		tree = runAssemblePOSIX(t, runtime.GOOS+"-"+runtime.GOARCH)
	}
	installed := zipRoundTrip(t, tree)
	if _, err := os.Stat(entryPoint(t, installed)); err != nil {
		t.Fatalf("entry point missing after round-trip: %v", err)
	}

	// Hermetic home + data dir, one seeded project whose path is the cwd the
	// session-start payload will report.
	dataHome := isolatedHome(t)
	home := filepath.Dir(dataHome)
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		cwd = t.TempDir()
	}
	seedProject(t, dataHome, "e2e-proj", cwd, "e2e-project")
	insertE2EMemory(t, dataHome)

	t.Run("session-start finalizes and injects context", func(t *testing.T) {
		dataDir := t.TempDir() // fresh CLAUDE_PLUGIN_DATA: marker must be created
		payload := fmt.Sprintf(`{"hook_event_name":"SessionStart","session_id":"e2e","cwd":%q,"source":"startup"}`, cwd)

		out1, err1 := runE2EHook(t, installed, dataDir, home, "session-start", payload)
		if !strings.Contains(err1, "finalizing") {
			t.Errorf("first run should finalize on stderr, got %q", err1)
		}
		if strings.Contains(out1, "finalizing") || strings.Contains(out1, "ghost plugin finalize") {
			t.Errorf("finalize banner leaked to stdout: %q", out1)
		}
		if !strings.Contains(out1, "E2E_SEED_MARKER") {
			t.Errorf("expected seeded context injected on stdout, got %q", out1)
		}
		if _, err := os.Stat(filepath.Join(dataDir, finalizeMarkerName)); err != nil {
			t.Errorf("finalize marker not written: %v", err)
		}

		out2, err2 := runE2EHook(t, installed, dataDir, home, "session-start", payload)
		if strings.Contains(err2, "finalizing") {
			t.Errorf("marker must skip finalize on the second run, got %q", err2)
		}
		if !strings.Contains(out2, "E2E_SEED_MARKER") {
			t.Errorf("second run should still inject context, got %q", out2)
		}
	})

	t.Run("stop nudges an unsaved session", func(t *testing.T) {
		transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
		body := lineUser + "\n" + lineToolBash + "\n" + lineText + "\n"
		if err := os.WriteFile(transcript, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		payload := fmt.Sprintf(`{"hook_event_name":"Stop","session_id":"e2e","transcript_path":%q,"cwd":%q,"stop_hook_active":false}`, transcript, cwd)
		out, errOut := runE2EHook(t, installed, t.TempDir(), home, "stop", payload)
		if !strings.Contains(out, "ghost_memory_save") {
			t.Errorf("expected save nudge on stdout, got %q", out)
		}
		if strings.Contains(errOut, "fail-open") {
			t.Errorf("stop payload should parse cleanly, stderr: %q", errOut)
		}
	})

	t.Run("mcp stdio handshake", func(t *testing.T) {
		cmd := exec.Command(entryPoint(t, installed), "mcp")
		cmd.Env = append(os.Environ(), "HOME="+home, "USERPROFILE="+home)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		var errb strings.Builder
		cmd.Stderr = &errb
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		}()

		send := func(v any) {
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}
			if _, err := stdin.Write(append(b, '\n')); err != nil {
				t.Fatalf("write to mcp stdin: %v", err)
			}
		}
		send(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "initialize",
			"params": map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{},
				"clientInfo":      map[string]any{"name": "ghost-e2e", "version": "0"},
			},
		})

		lines := make(chan string)
		go func() {
			sc := bufio.NewScanner(stdout)
			for sc.Scan() {
				lines <- sc.Text()
			}
			close(lines)
		}()
		timeout := time.After(30 * time.Second)
		gotInit, gotTools := false, false
		for !gotTools {
			select {
			case line, ok := <-lines:
				if !ok {
					t.Fatalf("mcp stdout ended early (init=%v tools=%v); stderr: %s", gotInit, gotTools, errb.String())
				}
				var msg struct {
					ID     *float64        `json:"id"`
					Result json.RawMessage `json:"result"`
				}
				if err := json.Unmarshal([]byte(line), &msg); err != nil || msg.ID == nil {
					continue // notification or non-JSON log line
				}
				switch int(*msg.ID) {
				case 1:
					if len(msg.Result) == 0 {
						t.Fatalf("initialize returned an error response: %s", line)
					}
					gotInit = true
					send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
					send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
				case 2:
					var r struct {
						Tools []struct {
							Name string `json:"name"`
						} `json:"tools"`
					}
					if err := json.Unmarshal(msg.Result, &r); err != nil {
						t.Fatalf("parse tools/list result: %v", err)
					}
					found := false
					for _, tl := range r.Tools {
						if tl.Name == "ghost_memory_save" {
							found = true
						}
					}
					if !found {
						t.Errorf("tools/list lacks ghost_memory_save: %s", msg.Result)
					}
					gotTools = true
				}
			case <-timeout:
				t.Fatalf("timed out waiting for MCP responses (init=%v); stderr: %s", gotInit, errb.String())
			}
		}
		_ = stdin.Close() // server should exit on stdin EOF
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("mcp server exited non-zero: %v; stderr: %s", err, errb.String())
			}
		case <-time.After(15 * time.Second):
			t.Error("mcp server did not exit after stdin close")
		}
	})

	t.Run("mcp init defers to an installed plugin", func(t *testing.T) {
		for _, key := range []string{"ghost@ghost", "ghost-windows-amd64@ghost"} {
			t.Run(key, func(t *testing.T) {
				h := t.TempDir()
				t.Setenv("HOME", h)
				t.Setenv("USERPROFILE", h)
				t.Setenv(pluginRootEnv, "")
				reg := filepath.Join(h, ".claude", "plugins")
				if err := os.MkdirAll(reg, 0o755); err != nil {
					t.Fatal(err)
				}
				doc := `{"version":2,"plugins":{"` + key + `":[]}}`
				if err := os.WriteFile(filepath.Join(reg, "installed_plugins.json"), []byte(doc), 0o644); err != nil {
					t.Fatal(err)
				}
				var buf strings.Builder
				if err := Run(&buf, false); err != nil {
					t.Fatalf("Run: %v", err)
				}
				if !strings.Contains(buf.String(), "plugin manages this integration") {
					t.Errorf("expected the defer message, got %q", buf.String())
				}
				if _, err := os.Stat(filepath.Join(h, ".claude.json")); err == nil {
					t.Error("init must not write ~/.claude.json while deferring")
				}
			})
		}
	})
}
```

- [ ] **Step 2: Run the test**

Run: `go test ./internal/mcpinit/ -run TestPluginE2E -count=1 -v`
Expected: PASS, four subtests (`session-start finalizes and injects context`, `stop nudges an unsaved session`, `mcp stdio handshake`, `mcp init defers to an installed plugin` × 2 keys).

If a subtest fails, the failure message carries the subprocess stderr dump — read it before changing anything; each assertion corresponds to a spec §4 guarantee (banner-on-stderr, marker idempotence, seeded injection, nudge output, protocol handshake, defer-without-writes).

- [ ] **Step 3: Run the whole package to catch interference**

Run: `go test ./internal/mcpinit/ -count=1`
Expected: PASS — existing tests unaffected (the new helpers only add names verified collision-free: `entryPoint`, `writeRootZip`, `extractRootZip`, `zipRoundTrip`, `insertE2EMemory`, `runE2EHook`).

- [ ] **Step 4: Commit**

```bash
git add internal/mcpinit/plugin_e2e_test.go
git commit -s -m "test(plugin): e2e archive, MCP stdio, hooks, and init deferral"
```

---

### Task 6: Windows CI job

**Files:**
- Modify: `.github/workflows/ci.yml` (append after the `build-and-test` job)

- [ ] **Step 1: Add the job**

Append:

```yaml
  # Real-Windows verification for the Claude Code plugin: this package owns
  # the coupling test (runs the assemble scripts) and the plugin e2e suite
  # (zip round-trip, MCP stdio handshake, both hooks, init deferral). Two
  # architectures because a native .exe cannot serve both — matching the
  # two per-arch marketplace entries.
  #
  # -run selects the two plugin suites only: the package's other tests
  # isolate HOME but not USERPROFILE, while production resolves
  # os.UserHomeDir() (USERPROFILE on Windows), so a whole-package run here
  # would go red for reasons unrelated to the plugin and write into the
  # runner's real profile. The full package follows up once test isolation
  # sets both variables. go test still compiles every test file (including
  # GOOS=windows-only ones) before -run filters, so build coverage is kept.
  # No -race: these are OS/ARCH legs — linux's build-and-test owns race.
  # If windows-11-arm cannot queue for this repo, trim the matrix to
  # windows-latest.
  windows-plugin:
    strategy:
      fail-fast: false
      matrix:
        os: [windows-latest, windows-11-arm]
    runs-on: ${{ matrix.os }}
    steps:
      - uses: actions/checkout@v7.0.1

      - uses: actions/setup-go@v7
        with:
          go-version-file: go.mod

      - name: Plugin coupling and e2e tests
        run: go test -count=1 -run 'TestPlugin(NameCoupling|E2E)$' ./internal/mcpinit/
```

- [ ] **Step 2: Lint the workflow**

Run: `command -v actionlint >/dev/null && actionlint .github/workflows/ci.yml || echo "actionlint not installed locally — the ci lint job runs it on this PR"`
Expected: either a clean actionlint pass, or the informational line (the PR's `lint` job runs actionlint over the diff anyway).

Also run `python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/ci.yml'))" 2>/dev/null && echo YAML_OK || echo "pyyaml unavailable — rely on actionlint"`
Expected: `YAML_OK` or the fallback note.

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/ci.yml
git commit -s -m "ci: run plugin e2e on windows-latest and windows-11-arm"
```

---

### Task 7: Validation, push, and PR

**Files:**
- Create (temporary): `/tmp/opencode/plugin-hardening-pr.md`

- [ ] **Step 1: Full local validation**

Run:

```bash
go vet ./... && go test ./internal/mcpinit/ -count=1 && go build -o /dev/null ./cmd/ghost
```

Expected: all PASS, exit 0. (Scoped package test per project convention; `build-and-test` CI runs the full `-race ./...` on push.)

- [ ] **Step 2: Review what will be pushed**

Run: `git status --short && git log --oneline origin/main..HEAD`
Expected: clean tree; seven commits — spec (`docs(spec): plugin hardening pass design`) + the six task commits. Account for every path in `git diff --stat origin/main..HEAD`: only the files this plan names.

- [ ] **Step 3: Search open issues before opening the PR (org policy)**

Run each and record the output, including empty results:

```bash
gh issue list --repo wcatz/ghost --state open --search "plugin"
gh issue list --repo wcatz/ghost --state open --search "windows"
gh issue list --repo wcatz/ghost --state open --search "marketplace"
gh issue list --repo wcatz/ghost --state open --search "cache path OR isUnderPluginCache"
gh issue list --repo wcatz/ghost --state closed --search "plugin"
```

Report every query and its result count in the handoff. If an open issue describes this exact work, stop and link instead of opening a duplicate.

- [ ] **Step 4: Push the branch**

Run: `git push -u origin feat/plugin-hardening`
Expected: new branch on origin, no force.

- [ ] **Step 5: Write the PR body to a file and open the PR**

Write `/tmp/opencode/plugin-hardening-pr.md`:

```markdown
- One test now enforces set equality across the three places the plugin name lives: `.claude-plugin/marketplace.json` entries, the manifests `assemble-plugin.sh`/`assemble-plugin-windows.sh` actually emit (scripts are executed, not re-implemented), and the `managedPluginNames` list `PluginInstalled` defers to. Drift on any side previously had no test and surfaced only as a broken install or a lost `ghost mcp init` deferral.
- `isUnderPluginCache` anchors to the resolved home (`$HOME/.claude/plugins/`), so a path that merely contains `/.claude/plugins/` no longer false-positives into a `ghost upgrade` refusal; when the home cannot be resolved the previous substring match stands, keeping the gate exactly as strict as today.
- Marketplace entries, both plugin manifests, and the README now disclose the persistent first-run finalize side effects (auto-memory off, memory import, MEMORY.md redirects — retained after uninstall) and that the reflect/resolve/supersede LLM passes are opt-in and off by default. The checked-in `0.0.0` manifest version is documented as a source-checkout placeholder stamped at assemble time.
- A new e2e suite runs the assembled tree through a zip round-trip, a real MCP `initialize`/`tools/list` handshake, both hooks (finalize banner confined to stderr, seeded context injected on stdout, marker honored on the second run), and `mcp init` deferral against an installed-plugins registry; CI now runs the package on `windows-latest` and `windows-11-arm`.
- The assemble scripts validate their stamped manifest with `python3` or `python` (Windows runners expose only `python`), and `.gitattributes` pins shell scripts to LF checkouts so Git Bash can execute them on Windows.
```

Run:

```bash
gh pr create --title "test(plugin): coupling, cache-path anchor, disclosure, e2e" --body-file /tmp/opencode/plugin-hardening-pr.md
```

- [ ] **Step 6: Verify the live PR record (org policy)**

Run: `gh pr view --json title,body | head -60`
Expected: title exactly `test(plugin): coupling, cache-path anchor, disclosure, e2e`; body matches the file — real line breaks, no literal `\n` escapes, no unexpected wrappers appended yet (bots may append later; re-read after their pass). Fix via edit + re-read if not.

Then: `rm /tmp/opencode/plugin-hardening-pr.md`

- [ ] **Step 7: Watch CI, including both Windows legs**

Run: `gh pr checks --watch`
Expected: all green, including `windows-plugin (windows-latest)` and `windows-plugin (windows-11-arm)`.

**Contingency (spec §4):** if `windows-11-arm` cannot queue (label ineligible for this repo) or fails purely on runner-tooling grounds the portability fixes don't cover, trim the matrix to `[windows-latest]` with a normal (non-force) commit, re-push, and record Windows ARM64 as the remaining manual verification item in Ghost task `7FFBDD03`'s completion notes.

- [ ] **Step 8: Hand off for review**

Stop here — do not merge. Report to the requester: PR link, CI status, the issue-search results from Step 3, and that bot-then-human review is the next gate (per repo policy, human review is mandatory).
