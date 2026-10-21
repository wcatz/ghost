//go:build !windows

package mcpinit

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/claudeimport"
	"github.com/wcatz/ghost/internal/memory"
)

// A host configuration path belongs to the host, and it is read with a bare
// os.ReadFile: opening a FIFO for reading blocks until a writer appears, so a
// FIFO at one of these paths stopped `ghost mcp init` and `ghost mcp status`
// with no output at all. The readers refuse a path that is not a regular
// file, and they refuse it by name.

// hostConfigDeadline bounds every run here that reads a host configuration
// path, so a regression that opens a FIFO is a failure rather than a stuck
// test run.
const hostConfigDeadline = 5 * time.Second

// withHostConfigDeadline runs fn and fails the test if it has not returned
// within hostConfigDeadline. fn must not touch the testing package itself:
// on a timeout the test ends while fn is still blocked in the open.
func withHostConfigDeadline(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(hostConfigDeadline):
		t.Fatal("the run did not return: it opened a host configuration path that is not a regular file")
	}
}

// plantFIFO creates a FIFO with no writer on it at path, and skips when the
// filesystem refuses one.
func plantFIFO(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
}

// requireFIFO asserts that the path still holds the FIFO the test planted: a
// refusal leaves the user's file exactly as it was.
func requireFIFO(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	if info.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("%s is no longer a FIFO: %v", path, info.Mode())
	}
}

// TestRunCodex_AFIFOAtTheHooksPathIsRefused: the codex hooks.json is read
// before anything is registered, so a FIFO at that path is refused with the
// file named and nothing written.
func TestRunCodex_AFIFOAtTheHooksPathIsRefused(t *testing.T) {
	home, _ := setupCodexTestEnv(t)
	path := codexHooksJSON(home)
	plantFIFO(t, path)

	var out bytes.Buffer
	var err error
	withHostConfigDeadline(t, func() { err = RunCodex(&out, true) })
	if err == nil {
		t.Fatalf("RunCodex accepted a hooks.json that is not a regular file; output:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the refusal must name the file %q, got: %v", path, err)
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("the refusal must say the path is not a regular file, got: %v", err)
	}
	requireFIFO(t, path)
}

// TestRunCodex_AFIFOAtTheConfigTomlPathIsRefused: the codex config.toml is
// read to merge the MCP server table into it, so a FIFO at that path is
// refused with the file named and nothing written.
func TestRunCodex_AFIFOAtTheConfigTomlPathIsRefused(t *testing.T) {
	home, _ := setupCodexTestEnv(t)
	path := codexConfigToml(home)
	plantFIFO(t, path)

	var out bytes.Buffer
	var err error
	withHostConfigDeadline(t, func() { err = RunCodex(&out, true) })
	if err == nil {
		t.Fatalf("RunCodex accepted a config.toml that is not a regular file; output:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the refusal must name the file %q, got: %v", path, err)
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("the refusal must say the path is not a regular file, got: %v", err)
	}
	requireFIFO(t, path)
}

// TestStatusCodex_AFIFOAtTheHooksPathIsReported: the status reader answers a
// question about the wiring, so a path it cannot read is reported rather than
// read — and the report names the file.
func TestStatusCodex_AFIFOAtTheHooksPathIsReported(t *testing.T) {
	home, _ := setupCodexTestEnv(t)
	path := codexHooksJSON(home)
	plantFIFO(t, path)

	var out bytes.Buffer
	var healthy bool
	var err error
	withHostConfigDeadline(t, func() { healthy, err = StatusCodex(&out) })
	if err != nil {
		t.Fatalf("StatusCodex: %v", err)
	}
	if healthy {
		t.Errorf("status reported a healthy integration over a hooks.json it cannot read; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), path) {
		t.Errorf("the report must name the file %q, got:\n%s", path, out.String())
	}
	if !strings.Contains(out.String(), "not a regular file") {
		t.Errorf("the report must say the path is not a regular file, got:\n%s", out.String())
	}
	requireFIFO(t, path)
}

// TestStatusCodex_AFIFOAtTheConfigTomlPathIsReported: the same for the
// config.toml. A path that cannot be read is not a path that is missing, and
// the report must not send the operator to an init that would write over it.
func TestStatusCodex_AFIFOAtTheConfigTomlPathIsReported(t *testing.T) {
	home, _ := setupCodexTestEnv(t)
	path := codexConfigToml(home)
	plantFIFO(t, path)

	var out bytes.Buffer
	var healthy bool
	var err error
	withHostConfigDeadline(t, func() { healthy, err = StatusCodex(&out) })
	if err != nil {
		t.Fatalf("StatusCodex: %v", err)
	}
	if healthy {
		t.Errorf("status reported a healthy integration over a config.toml it cannot read; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), path) {
		t.Errorf("the report must name the file %q, got:\n%s", path, out.String())
	}
	if !strings.Contains(out.String(), "not a regular file") {
		t.Errorf("the report must say the path is not a regular file, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "config.toml not found") {
		t.Errorf("a config.toml that cannot be read is not a missing one; got:\n%s", out.String())
	}
	requireFIFO(t, path)
}

// TestRun_AFIFOAtTheSettingsPathIsRefused: the Claude settings.json is
// validated before the MCP server is registered, so a FIFO at that path is
// refused with the file named, nothing registered and the file left as it
// was.
func TestRun_AFIFOAtTheSettingsPathIsRefused(t *testing.T) {
	home, binDir := installEnv(t)
	path := filepath.Join(home, ".claude", "settings.json")
	plantFIFO(t, path)

	var out bytes.Buffer
	var err error
	withHostConfigDeadline(t, func() { err = Run(&out, false) })
	if err == nil {
		t.Fatalf("Run accepted a settings.json that is not a regular file; output:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the refusal must name the file %q, got: %v", path, err)
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("the refusal must say the path is not a regular file, got: %v", err)
	}
	// The refusal happens before the registration: the recording stub writes
	// a `calls` log on every invocation, so it must not exist.
	requireMissing(t, binDir, "calls")
	requireFIFO(t, path)
}

// TestStatus_AFIFOAtTheSettingsPathIsReported: the status reader reports the
// settings it cannot read, naming the file.
func TestStatus_AFIFOAtTheSettingsPathIsReported(t *testing.T) {
	home, _ := installEnv(t)
	path := filepath.Join(home, ".claude", "settings.json")
	plantFIFO(t, path)

	var out bytes.Buffer
	var healthy bool
	var err error
	withHostConfigDeadline(t, func() { healthy, err = Status(&out) })
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if healthy {
		t.Errorf("status reported a healthy integration over a settings.json it cannot read; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), path) {
		t.Errorf("the report must name the file %q, got:\n%s", path, out.String())
	}
	if !strings.Contains(out.String(), "not a regular file") {
		t.Errorf("the report must say the path is not a regular file, got:\n%s", out.String())
	}
	requireFIFO(t, path)
}

// TestLoadSettings_ASymlinkToARegularFileStillLoads is the control for the
// refusal above: a symlinked settings.json is an ordinary dotfiles setup, and
// the regular-file check is made on what the link names, so the document is
// read exactly as before.
func TestLoadSettings_ASymlinkToARegularFileStillLoads(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, "real-settings.json")
	if err := os.WriteFile(target, []byte(`{"autoMemoryEnabled":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "settings.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	sf, err := loadSettings(link)
	if err != nil {
		t.Fatalf("loadSettings over a symlink to a regular file = %v, want the document", err)
	}
	if val, ok := sf.getAutoMemoryEnabled(); !ok || val {
		t.Errorf("autoMemoryEnabled = %v (set %v), want false", val, ok)
	}
}

// TestLoadSettings_AFIFOIsRefused pins the reader itself: the settings
// document is never read from a path that is not a regular file.
func TestLoadSettings_AFIFOIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	plantFIFO(t, path)

	var err error
	withHostConfigDeadline(t, func() { _, err = loadSettings(path) })
	if err == nil {
		t.Fatal("loadSettings read a settings.json that is not a regular file")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the refusal must name the file %q, got: %v", path, err)
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("the refusal must say the path is not a regular file, got: %v", err)
	}
}

// TestInstallCodexHooks_AFIFOIsRefused pins the hooks writer's own read.
// RunCodex validates hooks.json before it reaches this step, so the reader is
// driven directly here: it must refuse the path rather than merge into it.
func TestInstallCodexHooks_AFIFOIsRefused(t *testing.T) {
	home, _ := setupCodexTestEnv(t)
	path := codexHooksJSON(home)
	plantFIFO(t, path)

	var out bytes.Buffer
	var err error
	withHostConfigDeadline(t, func() { _, err = installCodexHooks(&out, "/bin/ghost", true) })
	if err == nil {
		t.Fatalf("installCodexHooks merged into a hooks.json that is not a regular file; output:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the refusal must name the file %q, got: %v", path, err)
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("the refusal must say the path is not a regular file, got: %v", err)
	}
	requireFIFO(t, path)
}

// TestInstallCodexMCP_AFIFOIsRefused pins the same for the config.toml merge.
func TestInstallCodexMCP_AFIFOIsRefused(t *testing.T) {
	home, _ := setupCodexTestEnv(t)
	path := codexConfigToml(home)
	plantFIFO(t, path)

	var out bytes.Buffer
	var err error
	withHostConfigDeadline(t, func() { _, err = installCodexMCP(&out, "/bin/ghost", true) })
	if err == nil {
		t.Fatalf("installCodexMCP merged into a config.toml that is not a regular file; output:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the refusal must name the file %q, got: %v", path, err)
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("the refusal must say the path is not a regular file, got: %v", err)
	}
	requireFIFO(t, path)
}

// TestInstallGoosePackage_AFIFOAtThePluginPathIsRefused pins the goose
// plugin writer's own read: a FIFO at any package file path is refused
// rather than overwritten.
func TestInstallGoosePackage_AFIFOAtThePluginPathIsRefused(t *testing.T) {
	isolatedHome(t)
	_ = t.TempDir()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	binDir := t.TempDir()
	writeStub(t, binDir, "ghost")
	t.Setenv("PATH", binDir)

	dir, err := goosePluginDir()
	if err != nil {
		t.Fatalf("goosePluginDir: %v", err)
	}
	// Plant FIFOs at all package file paths so the iteration refuses on the first one.
	for _, f := range goosePackageFiles("/bin/ghost") {
		plantFIFO(t, filepath.Join(dir, f.rel))
	}

	var out bytes.Buffer
	var err2 error
	// Not a dry run: the actual install path reads the files to check if they're current.
	withHostConfigDeadline(t, func() { _, err2 = installGoosePackage(&out, "/bin/ghost", false) })
	if err2 == nil {
		t.Fatalf("installGoosePackage wrote over a goose plugin file that is not a regular file; output:\n%s", out.String())
	}
	// The refusal names the first file it encountered.
	if !strings.Contains(err2.Error(), "not a regular file") {
		t.Errorf("the refusal must say the path is not a regular file, got: %v", err2)
	}
	// All planted FIFOs must remain.
	for _, f := range goosePackageFiles("/bin/ghost") {
		requireFIFO(t, filepath.Join(dir, f.rel))
	}
}

// TestStatusOpencode_AFIFOAtTheConfigFileIsReported: the opencode config
// file (opencode.json/opencode.jsonc) is read by status; a FIFO there is
// reported as unreadable.
func TestStatusOpencode_AFIFOAtTheConfigFileIsReported(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	xdgConfig := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgConfig)
	t.Setenv("GHOST_EMBEDDING_ENABLED", "false")
	t.Setenv("OPENCODE_CONFIG_DIR", filepath.Join(xdgConfig, "opencode"))

	binDir := t.TempDir()
	writeStub(t, binDir, "ghost")
	t.Setenv("PATH", binDir)

	path := filepath.Join(xdgConfig, "opencode", "opencode.json")
	plantFIFO(t, path)

	var out bytes.Buffer
	var healthy bool
	var err error
	withHostConfigDeadline(t, func() { healthy, err = StatusOpencode(&out) })
	if err != nil {
		t.Fatalf("StatusOpencode: %v", err)
	}
	if healthy {
		t.Errorf("status reported a healthy integration over a config file it cannot read; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), path) {
		t.Errorf("the report must name the file %q, got:\n%s", path, out.String())
	}
	if !strings.Contains(out.String(), "not a regular file") {
		t.Errorf("the report must say the path is not a regular file, got:\n%s", out.String())
	}
	requireFIFO(t, path)
}

// TestInstallOpencodePlugin_AFIFOAtThePluginPathIsRefused pins the opencode
// plugin writer's own read: a FIFO at the plugin path is refused rather
// than overwritten.
func TestInstallOpencodePlugin_AFIFOAtThePluginPathIsRefused(t *testing.T) {
	isolatedHome(t)
	_ = t.TempDir()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	binDir := t.TempDir()
	writeStub(t, binDir, "ghost")
	t.Setenv("PATH", binDir)

	path, err := opencodePluginPath()
	if err != nil {
		t.Fatalf("opencodePluginPath: %v", err)
	}
	plantFIFO(t, path)

	// The dry run and the real write must both refuse: the real write is the
	// path that would rename a fresh file over the FIFO.
	for _, dryRun := range []bool{true, false} {
		var out bytes.Buffer
		var err2 error
		withHostConfigDeadline(t, func() { _, err2 = installOpencodePlugin(&out, "/bin/ghost", dryRun) })
		if err2 == nil {
			t.Fatalf("dryRun=%v: installOpencodePlugin wrote over a plugin file that is not a regular file; output:\n%s", dryRun, out.String())
		}
		if !strings.Contains(err2.Error(), path) {
			t.Errorf("dryRun=%v: the refusal must name the file %q, got: %v", dryRun, path, err2)
		}
		if !strings.Contains(err2.Error(), "not a regular file") {
			t.Errorf("dryRun=%v: the refusal must say the path is not a regular file, got: %v", dryRun, err2)
		}
		requireFIFO(t, path)
	}
}

// TestWriteRedirects_AFIFOAtTheRedirectPathIsRefused pins the redirect
// writer's own read: a FIFO at a MEMORY.md redirect path is refused
// rather than overwritten.
func TestWriteRedirects_AFIFOAtTheRedirectPathIsRefused(t *testing.T) {
	isolatedHome(t)
	// Get the home directory that isolatedHome set up.
	home := os.Getenv("HOME")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	binDir := t.TempDir()
	writeStub(t, binDir, "ghost")
	t.Setenv("PATH", binDir)

	// Create a project in the store so writeRedirects has something to do.
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	dbPath := storeAt(t, filepath.Join(dataHome, "ghost"))
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	slog := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := memory.NewStore(db, slog)
	projInfo := projectInfo{ID: "testproj", Path: home, Name: "testproj"}
	if err := store.EnsureProject(context.Background(), projInfo.ID, projInfo.Path, projInfo.Name); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	_ = db.Close()

	// Plant a FIFO at the MEMORY.md redirect path.
	encoded := claudeimport.EncodeProjectPath(home)
	target := filepath.Join(home, ".claude", "projects", encoded, "memory", "MEMORY.md")
	plantFIFO(t, target)

	var out bytes.Buffer
	withHostConfigDeadline(t, func() { writeRedirects(&out, []projectInfo{projInfo}, false) })
	// The output should contain a warning line about the non-regular file.
	if !strings.Contains(out.String(), "not a regular file") {
		t.Errorf("the output must say the path is not a regular file, got:\n%s", out.String())
	}
	// The FIFO must still be there.
	requireFIFO(t, target)
}

// TestPluginInstalled_AFIFOAtTheRegistryPathReturnsFalse pins the plugin
// registry reader: a FIFO at installed_plugins.json returns false
// without hanging.
func TestPluginInstalled_AFIFOAtTheRegistryPathReturnsFalse(t *testing.T) {
	isolatedHome(t)
	home := t.TempDir()
	setHome(t, home)

	path := filepath.Join(home, ".claude", "plugins", "installed_plugins.json")
	plantFIFO(t, path)

	var got bool
	withHostConfigDeadline(t, func() { got = PluginInstalled() })
	if got {
		t.Errorf("PluginInstalled() = true, want false over a FIFO")
	}
	requireFIFO(t, path)
}

// TestStatusOpencode_AFIFOAtThePluginPathIsReportedUnreadable pins status's
// own read of the lifecycle plugin: a FIFO there is reported as unreadable,
// not as a plugin that is missing, which would send the user to an init that
// refuses the same path.
func TestStatusOpencode_AFIFOAtThePluginPathIsReportedUnreadable(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	xdgConfig := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgConfig)
	t.Setenv("GHOST_EMBEDDING_ENABLED", "false")
	t.Setenv("OPENCODE_CONFIG_DIR", filepath.Join(xdgConfig, "opencode"))

	binDir := t.TempDir()
	writeStub(t, binDir, "ghost")
	t.Setenv("PATH", binDir)

	path, err := opencodePluginPath()
	if err != nil {
		t.Fatalf("opencodePluginPath: %v", err)
	}
	plantFIFO(t, path)

	var out bytes.Buffer
	var healthy bool
	withHostConfigDeadline(t, func() { healthy, err = StatusOpencode(&out) })
	if err != nil {
		t.Fatalf("StatusOpencode: %v", err)
	}
	if healthy {
		t.Errorf("status reported a healthy integration over a plugin it cannot read; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "lifecycle plugin unreadable") || !strings.Contains(out.String(), "not a regular file") {
		t.Errorf("status must report the plugin as unreadable and not a regular file, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "missing or outdated") {
		t.Errorf("status must not call an unreadable plugin missing, got:\n%s", out.String())
	}
	requireFIFO(t, path)
}
