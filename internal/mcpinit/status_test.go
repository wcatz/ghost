package mcpinit

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/embedding"
	"github.com/wcatz/ghost/internal/memory"
)

// installOpencodePluginFile installs the rendered lifecycle plugin into the
// test XDG_CONFIG_HOME, the same way `ghost mcp init --client opencode` does.
// ghostBin must match the stub on PATH so status's byte-compare sees it fresh.
func installOpencodePluginFile(t *testing.T, ghostBin string) {
	t.Helper()
	if _, err := installOpencodePlugin(io.Discard, ghostBin, false); err != nil {
		t.Fatalf("install plugin: %v", err)
	}
}

// writePluginFile seeds the plugin path with arbitrary content for drift tests.
func writePluginFile(t *testing.T, content string) {
	t.Helper()
	path, err := opencodePluginPath()
	if err != nil {
		t.Fatalf("plugin path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir plugin dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write plugin: %v", err)
	}
}

// writeOpencodeMCPConfig writes opencode config content under the test's
// XDG_CONFIG_HOME — the file `ghost mcp status --client opencode` reads for
// the mcp.ghost registration. name is "opencode.jsonc" or "opencode.json".
func writeOpencodeMCPConfig(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "opencode", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir opencode config dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// opencodeMCPRegistration renders the current, enabled mcp.ghost entry for
// ghostBin — the shape the opencode registration check must accept.
func opencodeMCPRegistration(ghostBin string) string {
	return fmt.Sprintf(`{"mcp": {"ghost": {"type": "local", "command": [%q, "mcp"], "enabled": true}}}`, ghostBin)
}

// statusLineContaining returns the first output line containing want, so a
// test can assert how the message was rendered (✗ failure vs - info) rather
// than only that the text appeared somewhere.
func statusLineContaining(output, want string) string {
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, want) {
			return line
		}
	}
	return ""
}

// TestReportStaleIntegrations pins the post-upgrade wiring check: stale
// Claude hooks and a drifted opencode plugin each produce an actionable
// hint; current wiring produces no output at all.
func TestReportStaleIntegrations(t *testing.T) {
	t.Run("silent when everything is current", func(t *testing.T) {
		statusEnv(t)
		binDir := writeStubGhost(t)
		t.Setenv("PATH", binDir)
		installOpencodePluginFile(t, stubPath(binDir, "ghost"))

		var out bytes.Buffer
		ReportStaleIntegrations(&out)
		if out.Len() != 0 {
			t.Errorf("current wiring must print nothing, got:\n%s", out.String())
		}
	})

	t.Run("flags pre-contract claude hook", func(t *testing.T) {
		statusEnv(t)

		path, err := settingsPath()
		if err != nil {
			t.Fatalf("settingsPath: %v", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("mkdir settings dir: %v", err)
		}
		if err := os.WriteFile(path, []byte(`{"hooks":{"Stop":[{"matcher":"","hooks":[{"type":"command","command":"/bin/ghost hook stop"}]}]}}`), 0o600); err != nil {
			t.Fatalf("write settings: %v", err)
		}

		var out bytes.Buffer
		ReportStaleIntegrations(&out)
		if !strings.Contains(out.String(), "Claude Code Stop hook is pre-contract") {
			t.Errorf("expected pre-contract stop hook hint, got:\n%s", out.String())
		}
	})

	t.Run("flags drifted codex wiring when ~/.codex exists", func(t *testing.T) {
		statusEnv(t)
		codexHome := t.TempDir()
		t.Setenv("CODEX_HOME", codexHome)
		if err := os.WriteFile(filepath.Join(codexHome, "hooks.json"), []byte(`{"hooks":{}}`), 0o600); err != nil {
			t.Fatalf("write hooks.json: %v", err)
		}

		var out bytes.Buffer
		ReportStaleIntegrations(&out)
		if !strings.Contains(out.String(), "codex integration missing or miswired") {
			t.Errorf("expected codex hint, got:\n%s", out.String())
		}
	})

	t.Run("flags drifted goose package when ~/.agents/plugins exists", func(t *testing.T) {
		statusEnv(t)
		dir := filepath.Join(os.Getenv("HOME"), ".agents", "plugins", "ghost")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir goose plugin dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(`{"name":"ghost"}`), 0o644); err != nil {
			t.Fatalf("write plugin.json: %v", err)
		}

		var out bytes.Buffer
		ReportStaleIntegrations(&out)
		if !strings.Contains(out.String(), "goose plugin package missing or outdated") {
			t.Errorf("expected goose hint, got:\n%s", out.String())
		}
	})

	t.Run("flags drifted opencode plugin when opencode is in use", func(t *testing.T) {
		statusEnv(t)
		writePluginFile(t, "// ghost-opencode v0 — stale body")

		var out bytes.Buffer
		ReportStaleIntegrations(&out)
		if !strings.Contains(out.String(), "opencode lifecycle plugin is missing or outdated") {
			t.Errorf("expected drifted plugin hint, got:\n%s", out.String())
		}
	})
}

// TestAuditStaleReportJudgesWithTheOldImage is #1215's reproduction: the
// post-upgrade wiring check compares installed wiring against the sources
// embedded in the image that RUNS it, so a check `ghost upgrade` runs in the
// old process judges the installed plugin against what the OLD release
// rendered. Installing the plugin exactly as the running image renders it and
// then re-rendering it as the next release would is the two images inside one
// package: the first run is what the old process prints (nothing), the second
// is what the installed binary would print (the hint). That gap is why the
// upgrade path runs the check in the NEW binary instead of in itself.
func TestAuditStaleReportJudgesWithTheOldImage(t *testing.T) {
	statusEnv(t)
	binDir := writeStubGhost(t)
	t.Setenv("PATH", binDir)
	ghostBin := stubPath(binDir, "ghost")
	// Installed as the OLD release rendered it: byte-identical to this image's
	// own render, which is the state an upgrade from the previous release
	// leaves behind.
	installOpencodePluginFile(t, ghostBin)

	var out bytes.Buffer
	ReportStaleIntegrations(&out)
	if out.Len() != 0 {
		t.Fatalf("the old image must see the plugin it rendered itself as current, got:\n%s", out.String())
	}

	// The next release's own source: what the binary the upgrade installs
	// holds in its embed. Nothing but the embedded body changes.
	orig := opencodeGhostPluginTS
	opencodeGhostPluginTS = "// ghost-opencode lifecycle plugin — next release body\n" + orig
	t.Cleanup(func() { opencodeGhostPluginTS = orig })

	out.Reset()
	ReportStaleIntegrations(&out)
	if !strings.Contains(out.String(), "opencode lifecycle plugin is missing or outdated") {
		t.Errorf("the new image must report the plugin the old release rendered, got:\n%s", out.String())
	}
}

// TestStatus_ReportsOpenDBFailure verifies that a database which exists but
// fails to open (e.g. mid-migration foreign-key corruption) is surfaced as a
// failed check, not silently skipped. Before this fix, Status only inspected
// the database when memory.OpenDB succeeded, so a broken database looked
// identical to "All checks passed."
func TestStatus_ReportsOpenDBFailure(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataDir)
	ghostDir := filepath.Join(dataDir, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir ghost dir: %v", err)
	}
	dbPath := filepath.Join(ghostDir, "ghost.db")
	if err := os.WriteFile(dbPath, []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatalf("write fake db: %v", err)
	}

	// Isolate PATH so Status can't shell out to a host-installed `claude`
	// binary — this test only exercises the database-open-failure check.
	t.Setenv("PATH", t.TempDir())

	var out bytes.Buffer
	healthy, err := Status(&out)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if healthy {
		t.Error("Status: healthy = true, want false for a broken database")
	}

	output := out.String()
	if !strings.Contains(output, "✗ database:") {
		t.Errorf("expected a failed database check, got:\n%s", output)
	}
	if strings.Contains(output, "All checks passed.") {
		t.Errorf("a broken database must not report \"All checks passed.\", got:\n%s", output)
	}
}

// TestStatus_ForeignHookIsNotWired: a command that runs another tool but ends
// in ghost's argument tail is not ghost's hook, so status must report the event
// missing rather than configured — the same executable ownership rule
// `ghost mcp init` applies (#1326). The reader used to ignore the binary
// entirely and match the tail alone, so a foreign command read as a healthy
// install and no repair was ever surfaced.
func TestStatus_ForeignHookIsNotWired(t *testing.T) {
	statusEnv(t)
	home := os.Getenv("HOME")
	settingsDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatalf("mkdir settings dir: %v", err)
	}
	settings := `{
  "hooks": {
    "SessionStart": [{"matcher": "", "hooks": [{"type": "command", "command": "/usr/local/bin/othertool hook session-start --source claude-code"}]}],
    "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "/usr/local/bin/othertool hook stop --source claude-code"}]}]
  }
}`
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatalf("write settings.json: %v", err)
	}

	var out bytes.Buffer
	if _, err := Status(&out); err != nil {
		t.Fatalf("Status: %v", err)
	}

	output := out.String()
	if strings.Contains(output, "✓ SessionStart hook configured") {
		t.Errorf("a foreign command carrying ghost's tail must not read as ghost's hook, got:\n%s", output)
	}
	if !strings.Contains(output, "✗ SessionStart hook missing") {
		t.Errorf("expected SessionStart hook to be reported missing, got:\n%s", output)
	}
}

// TestStatus_WrappedGhostHookIsWired: a hand-edited wrapper that does run ghost
// — an assignment prefix, or a directory change joined to the invocation — is
// ghost's own hook, so status must report it configured, because init leaves
// such a command in place and writes nothing to repair it. A reader that looked
// only at the leading token would call it missing and send the operator to a
// `ghost mcp init` that changes nothing.
func TestStatus_WrappedGhostHookIsWired(t *testing.T) {
	statusEnv(t)
	home := os.Getenv("HOME")
	settingsDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatalf("mkdir settings dir: %v", err)
	}
	settings := `{
  "hooks": {
    "SessionStart": [{"matcher": "", "hooks": [{"type": "command", "command": "env GHOST_QUIET=1 /opt/ghost/bin/ghost hook session-start --source claude-code"}]}],
    "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "cd /somewhere && /opt/ghost/bin/ghost hook stop --source claude-code"}]}]
  }
}`
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatalf("write settings.json: %v", err)
	}

	var out bytes.Buffer
	if _, err := Status(&out); err != nil {
		t.Fatalf("Status: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "✓ SessionStart hook configured") {
		t.Errorf("a wrapper that runs ghost is ghost's own hook, got:\n%s", output)
	}
	if !strings.Contains(output, "✓ Stop hook configured") {
		t.Errorf("a wrapper that runs ghost is ghost's own hook, got:\n%s", output)
	}
}

// TestStatus_ReportsInaccessibleDatabase verifies that a database which cannot
// be stat'd for a reason other than absence (e.g. a permission error) is
// surfaced as a failed check instead of being reported as a fresh install.
func TestStatus_ReportsInaccessibleDatabase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod/geteuid semantics on Windows: Chmod(0o000) only toggles the read-only bit and cannot revoke directory access, and os.Geteuid returns -1, so the EACCES path cannot be produced there")
	}
	if os.Geteuid() == 0 {
		t.Skip("permission checks cannot fail as root")
	}
	statusEnv(t)
	dataHome := os.Getenv("XDG_DATA_HOME")
	ghostDir := filepath.Join(dataHome, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir ghost dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ghostDir, "ghost.db"), nil, 0o600); err != nil {
		t.Fatalf("write db file: %v", err)
	}
	// Strip read+traverse so os.Stat on the database returns EACCES.
	if err := os.Chmod(ghostDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ghostDir, 0o700) }) // let TempDir removal succeed

	var out bytes.Buffer
	healthy, err := Status(&out)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if healthy {
		t.Error("Status: healthy = true, want false for an inaccessible database")
	}

	output := out.String()
	if !strings.Contains(output, "✗ database:") {
		t.Errorf("expected a failed database check, got:\n%s", output)
	}
	if strings.Contains(output, "no Ghost database (run ghost first)") {
		t.Errorf("a permission error is not a fresh install, got:\n%s", output)
	}
	if strings.Contains(output, "All checks passed.") {
		t.Errorf("an inaccessible database must not report \"All checks passed.\", got:\n%s", output)
	}
}

// statusEnv isolates a status run from the host: no binaries on PATH or in the
// common install dirs, a clean XDG_CONFIG_HOME/XDG_DATA_HOME, and embeddings
// disabled so the Ollama check can't reach the network.
func statusEnv(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	setHome(t, t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GHOST_EMBEDDING_ENABLED", "false")
	// A port nothing listens on and the compiled default model and width, so a
	// test that turns embedding back on cannot reach a real Ollama on the host
	// or look for a model the host configured. Tests that need a stub set the
	// URL themselves; the model and dimensions match the config files they
	// write.
	t.Setenv("GHOST_OLLAMA_URL", "http://127.0.0.1:1")
	t.Setenv("GHOST_EMBEDDING_MODEL", "nomic-embed-text:v1.5")
	t.Setenv("GHOST_EMBEDDING_DIMENSIONS", "768")
	// opencode's env config layers must be empty by default: the tests that
	// need them set them explicitly, and a leaked value from the host would
	// otherwise decide which config file the registration check reads.
	t.Setenv("OPENCODE_CONFIG", "")
	t.Setenv("OPENCODE_CONFIG_CONTENT", "")
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	orig := systemBinDirs
	systemBinDirs = nil
	t.Cleanup(func() { systemBinDirs = orig })
}

// writeStubGhost creates an executable `ghost` stub in a temp dir and returns
// that dir, so exec.LookPath finds it without touching the host.
func writeStubGhost(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	writeStub(t, binDir, "ghost")
	return binDir
}

// writeGhostConfigFile writes a ghost config.yaml to the exact path
// config.ConfigFilePath resolves (which honors XDG_CONFIG_HOME via
// userConfigDir), so the file is at the location the code under test reads.
func writeGhostConfigFile(t *testing.T, content string) string {
	t.Helper()
	path, err := config.ConfigFilePath()
	if err != nil {
		t.Fatalf("config file path: %v", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir ghost config dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write ghost config: %v", err)
	}
	return path
}

// TestStatusOpencode_MCPRegistration pins the opencode registration check
// (#631): a present, enabled mcp.ghost entry whose command resolves to an
// executable ghost binary passes — in either config spelling opencode reads,
// JSONC comments and trailing commas included — while a missing, disabled or
// wrong-path registration is always reported, naming the file and the edit
// that repairs it. Health weight follows the lifecycle plugin: with the
// plugin current, it registers ghost at startup and overrides whatever the
// file says, so the line is informational and the run stays green (the
// documented plugin-only install must never fail here); with the plugin
// missing, the file is the only registration surface, so the same line fails
// the run and the footer names the config edit — init never writes it.
func TestStatusOpencode_MCPRegistration(t *testing.T) {
	cases := map[string]struct {
		// seed writes the opencode config the case needs and returns the
		// failure substring the status output must carry, or "" when the
		// registration check must pass.
		seed func(t *testing.T, ghostBin string) string
		// cannotJudge marks the cases where status never reaches a verdict
		// about the entry — a config layer it could not read or parse. Those
		// fail the run under *both* gates: opencode drops a layer it cannot
		// parse, so the plugin's registration does not make the entry known,
		// only unjudged.
		cannotJudge bool
	}{
		"present in opencode.jsonc": {seed: func(t *testing.T, ghostBin string) string {
			writeOpencodeMCPConfig(t, "opencode.jsonc", opencodeMCPRegistration(ghostBin))
			return ""
		}},
		"present in opencode.json": {seed: func(t *testing.T, ghostBin string) string {
			writeOpencodeMCPConfig(t, "opencode.json", opencodeMCPRegistration(ghostBin))
			return ""
		}},
		"jsonc with comments and trailing commas": {seed: func(t *testing.T, ghostBin string) string {
			writeOpencodeMCPConfig(t, "opencode.jsonc", fmt.Sprintf(`{
  // Ghost's own registration; opencode allows comments and trailing commas.
  /* the lifecycle plugin bridges stop events */
  "mcp": {
    "ghost": {
      "type": "local",
      "command": [%q, "mcp"],
      "enabled": true,
    },
  },
}`, ghostBin))
			return ""
		}},
		"jsonc preferred over a stale opencode.json": {seed: func(t *testing.T, ghostBin string) string {
			writeOpencodeMCPConfig(t, "opencode.jsonc", opencodeMCPRegistration(ghostBin))
			writeOpencodeMCPConfig(t, "opencode.json", `{"mcp": {"ghost": {"enabled": false}}}`)
			return ""
		}},
		"no config file at all": {seed: func(t *testing.T, ghostBin string) string {
			return "no opencode config file"
		}},
		"config without an mcp.ghost entry": {seed: func(t *testing.T, ghostBin string) string {
			writeOpencodeMCPConfig(t, "opencode.jsonc", `{"model": "anthropic/claude-sonnet-4-5"}`)
			return "ghost MCP server missing from"
		}},
		"disabled": {seed: func(t *testing.T, ghostBin string) string {
			writeOpencodeMCPConfig(t, "opencode.jsonc", fmt.Sprintf(
				`{"mcp": {"ghost": {"type": "local", "command": [%q, "mcp"], "enabled": false}}}`, ghostBin))
			return "ghost MCP server disabled"
		}},
		"points at a different binary": {seed: func(t *testing.T, ghostBin string) string {
			// A second executable that resolves — but is not the ghost on
			// PATH: resolving at all is not enough, it must be the right one.
			other := writeStub(t, t.TempDir(), "ghost-old")
			writeOpencodeMCPConfig(t, "opencode.jsonc", fmt.Sprintf(
				`{"mcp": {"ghost": {"type": "local", "command": [%q, "mcp"], "enabled": true}}}`, other))
			return "not the ghost binary"
		}},
		"command path does not exist": {seed: func(t *testing.T, ghostBin string) string {
			writeOpencodeMCPConfig(t, "opencode.jsonc", fmt.Sprintf(
				`{"mcp": {"ghost": {"type": "local", "command": [%q, "mcp"], "enabled": true}}}`,
				filepath.Join(t.TempDir(), "ghost")))
			return "is not an executable file"
		}},
		"command does not run ghost mcp": {seed: func(t *testing.T, ghostBin string) string {
			writeOpencodeMCPConfig(t, "opencode.jsonc", fmt.Sprintf(
				`{"mcp": {"ghost": {"type": "local", "command": [%q], "enabled": true}}}`, ghostBin))
			return "ghost MCP server command in"
		}},
		"unparseable config": {seed: func(t *testing.T, ghostBin string) string {
			writeOpencodeMCPConfig(t, "opencode.jsonc", `{"mcp": {"ghost": `)
			return "cannot parse"
		}, cannotJudge: true},
		"unreadable config": {seed: func(t *testing.T, ghostBin string) string {
			// A directory where opencode.jsonc belongs: stat finds it, read
			// fails for every user (EISDIR), so status reaches no verdict.
			path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "opencode", "opencode.jsonc")
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatalf("mkdir config path: %v", err)
			}
			return "cannot read"
		}, cannotJudge: true},
	}

	for name, tc := range cases {
		for _, pluginCurrent := range []bool{true, false} {
			gate := "plugin current"
			if !pluginCurrent {
				gate = "plugin missing"
			}
			t.Run(name+" / "+gate, func(t *testing.T) {
				statusEnv(t)
				binDir := writeStubGhost(t)
				t.Setenv("PATH", binDir)
				ghostBin := stubPath(binDir, "ghost")
				if pluginCurrent {
					installOpencodePluginFile(t, ghostBin)
				}
				wantFail := tc.seed(t, ghostBin)

				var out bytes.Buffer
				healthy, err := StatusOpencode(&out)
				if err != nil {
					t.Fatalf("StatusOpencode: %v", err)
				}
				output := out.String()

				if wantFail == "" {
					if !strings.Contains(output, "✓ ghost MCP server registered in opencode config") {
						t.Errorf("%s: expected a passing registration check, got:\n%s", name, output)
					}
					if pluginCurrent {
						if !healthy {
							t.Errorf("%s: healthy = false, want true with a current registration, got:\n%s", name, output)
						}
						if !strings.Contains(output, "All checks passed.") {
							t.Errorf("%s: expected \"All checks passed.\", got:\n%s", name, output)
						}
						if strings.Contains(output, "✗") {
							t.Errorf("%s: a current registration must be the only registration line, got:\n%s", name, output)
						}
						return
					}
					// Plugin missing: the plugin check alone fails the run,
					// and a passing registration adds no config-edit advice.
					if healthy {
						t.Errorf("%s: healthy = true, want false without a lifecycle plugin, got:\n%s", name, output)
					}
					if !strings.Contains(output, "✗ lifecycle plugin missing or outdated") {
						t.Errorf("%s: expected failed plugin check, got:\n%s", name, output)
					}
					if strings.Contains(output, "config edit") {
						t.Errorf("%s: a passing registration must not produce the config-edit footer, got:\n%s", name, output)
					}
					return
				}

				if !strings.Contains(output, wantFail) {
					t.Errorf("%s: expected failure %q, got:\n%s", name, wantFail, output)
				}
				line := statusLineContaining(output, wantFail)

				if pluginCurrent {
					if tc.cannotJudge {
						// A layer status could not read or parse is not a
						// state the plugin's registration papers over: the
						// layer's contents never reach opencode, so status
						// cannot claim the entry is covered — it could not
						// judge it. The run must stay red either way.
						if healthy {
							t.Errorf("%s: plugin current — an unreadable config layer must fail the run, got:\n%s", name, output)
						}
						if !strings.HasPrefix(line, "  ✗ ") {
							t.Errorf("%s: expected a failed \"  ✗ \" check line for %q, got line %q (full output:\n%s)",
								name, wantFail, line, output)
						}
						if strings.Contains(output, "All checks passed.") {
							t.Errorf("%s: must not report \"All checks passed.\", got:\n%s", name, output)
						}
						if strings.Contains(output, "the file entry is only the fallback") {
							t.Errorf("%s: an unjudged config layer must not be reported as the covered fallback, got:\n%s", name, output)
						}
						if !strings.Contains(output, "could not be read or parsed") {
							t.Errorf("%s: expected the config-repair footer for an unreadable layer, got:\n%s", name, output)
						}
						assertNoInitFooter(t, name, output)
						return
					}
					// The plugin registers ghost at startup and overrides the
					// file, so a broken entry is reported but never failed —
					// this is the documented plugin-only install staying green.
					if !healthy {
						t.Errorf("%s: plugin current — a broken file entry must not fail the run, got:\n%s", name, output)
					}
					if !strings.HasPrefix(line, "  - ") {
						t.Errorf("%s: expected an informational \"  - \" line for %q, got line %q (full output:\n%s)",
							name, wantFail, line, output)
					}
					if strings.Contains(output, "✗") {
						t.Errorf("%s: plugin current — no check may fail, got:\n%s", name, output)
					}
					if !strings.Contains(output, "All checks passed.") {
						t.Errorf("%s: expected \"All checks passed.\", got:\n%s", name, output)
					}
					return
				}

				// Plugin missing: the file is the only registration surface,
				// so the same message fails the run — with a footer that says
				// how to repair it, since init never writes this file.
				if healthy {
					t.Errorf("%s: healthy = true, want false without a valid registration, got:\n%s", name, output)
				}
				if !strings.HasPrefix(line, "  ✗ ") {
					t.Errorf("%s: expected a failed \"  ✗ \" check line for %q, got line %q (full output:\n%s)",
						name, wantFail, line, output)
				}
				if strings.Contains(output, "All checks passed.") {
					t.Errorf("%s: must not report \"All checks passed.\", got:\n%s", name, output)
				}
				wantFooter := "mcp.ghost entry shown above is a config edit"
				if tc.cannotJudge {
					wantFooter = "could not be read or parsed"
				}
				if !strings.Contains(output, wantFooter) {
					t.Errorf("%s: expected the footer %q, got:\n%s", name, wantFooter, output)
				}
				assertNoInitFooter(t, name, output)
			})
		}
	}
}

// assertNoInitFooter pins that a registration failure prints exactly one
// remediation, never two contradictory ones. `ghost mcp init --client
// opencode` writes the lifecycle adapter and never the user's opencode
// config, so telling the user to run it is wrong advice for every failure
// that reports a config repair — the two lines sit adjacent and contradict
// ("run mcp init" above "…apply it to opencode's config file (`ghost mcp
// init` never writes that file)").
func assertNoInitFooter(t *testing.T, name, output string) {
	t.Helper()
	if strings.Contains(output, "Run `ghost mcp init --client opencode` to fix issues.") {
		t.Errorf("%s: a config repair must not also print \"run ghost mcp init\" advice, got:\n%s", name, output)
	}
}

// TestStatusOpencode_UnresolvableConfigDir pins the unjudgeable path that
// involves no config file at all: with no $HOME and no XDG_CONFIG_HOME or
// $OPENCODE_CONFIG_DIR, opencode's config directory cannot be resolved. The
// run is still red — nothing about the registration can be asserted — but the
// footer must not tell the user to repair a config file, because no file was
// ever named. The plugin check above already reports the same root cause.
func TestStatusOpencode_UnresolvableConfigDir(t *testing.T) {
	statusEnv(t)
	binDir := writeStubGhost(t)
	t.Setenv("PATH", binDir)
	// An empty config dir string is a real failure mode (a service manager
	// or a container that exports neither), not a test-only contrivance.
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	setHome(t, "") // both vars os.UserHomeDir reads, so the resolution fails everywhere

	var out bytes.Buffer
	healthy, err := StatusOpencode(&out)
	if err != nil {
		t.Fatalf("StatusOpencode: %v", err)
	}
	output := out.String()
	if healthy {
		t.Errorf("healthy = true with no resolvable config directory, got:\n%s", output)
	}
	if !strings.Contains(output, "home dir") {
		t.Errorf("expected the root cause named on a check line, got:\n%s", output)
	}
	for _, wrong := range []string{
		"could not be read or parsed",
		"is a config edit",
		"no opencode config file",
	} {
		if strings.Contains(output, wrong) {
			t.Errorf("%q must not be reported when no config file was ever named, got:\n%s", wrong, output)
		}
	}
}

// TestStatusOpencode_DocumentedPluginOnlyInstallStaysHealthy pins the review
// gate on #631: `ghost mcp init --client opencode` installs only the
// lifecycle plugin — never an opencode config file — and that plugin
// registers ghost at startup (config hook in V1, ctx.mcp.transform in V2),
// overriding whatever the file says. The documented install therefore has no
// mcp.ghost entry at all, and status must stay green, reporting the absent
// entry as the fallback it is instead of failing the run on it.
func TestStatusOpencode_DocumentedPluginOnlyInstallStaysHealthy(t *testing.T) {
	statusEnv(t)
	binDir := writeStubGhost(t)
	t.Setenv("PATH", binDir)
	installOpencodePluginFile(t, stubPath(binDir, "ghost"))

	var out bytes.Buffer
	healthy, err := StatusOpencode(&out)
	if err != nil {
		t.Fatalf("StatusOpencode: %v", err)
	}
	if !healthy {
		t.Errorf("StatusOpencode: healthy = false — the documented plugin-only install must stay green, got:\n%s", out.String())
	}

	output := out.String()
	if strings.Contains(output, "✗") {
		t.Errorf("a documented plugin-only install must have no failed checks, got:\n%s", output)
	}
	if !strings.Contains(output, "All checks passed.") {
		t.Errorf("expected \"All checks passed.\", got:\n%s", output)
	}
	line := statusLineContaining(output, "no opencode config file")
	if !strings.HasPrefix(line, "  - ") {
		t.Errorf("expected the absent entry as an informational line, got line %q (full output:\n%s)", line, output)
	}
}

// TestStatusOpencode_OPENCODEConfigContentEnv pins inline config: when
// $OPENCODE_CONFIG_CONTENT supplies configuration with no file on disk at
// all, status must judge the mcp.ghost entry it carries — passing when the
// content registers ghost, failing with the content named when it disables
// it — instead of reporting "no opencode config file".
func TestStatusOpencode_OPENCODEConfigContentEnv(t *testing.T) {
	cases := map[string]struct {
		content  func(ghostBin string) string
		wantFail string
	}{
		"content registers ghost": {
			content:  opencodeMCPRegistration,
			wantFail: "",
		},
		"content disables ghost": {
			content: func(ghostBin string) string {
				return fmt.Sprintf(
					`{"mcp": {"ghost": {"type": "local", "command": [%q, "mcp"], "enabled": false}}}`, ghostBin)
			},
			wantFail: "ghost MCP server disabled in $OPENCODE_CONFIG_CONTENT",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			statusEnv(t)
			binDir := writeStubGhost(t)
			t.Setenv("PATH", binDir)
			ghostBin := stubPath(binDir, "ghost")
			t.Setenv("OPENCODE_CONFIG_CONTENT", tc.content(ghostBin))

			var out bytes.Buffer
			healthy, err := StatusOpencode(&out)
			if err != nil {
				t.Fatalf("StatusOpencode: %v", err)
			}
			output := out.String()

			if tc.wantFail == "" {
				if !strings.Contains(output, "✓ ghost MCP server registered in opencode config") {
					t.Errorf("expected the inline config's registration to pass, got:\n%s", output)
				}
				return
			}
			if healthy {
				t.Errorf("healthy = true, want false when the inline config disables ghost, got:\n%s", output)
			}
			line := statusLineContaining(output, tc.wantFail)
			if !strings.HasPrefix(line, "  ✗ ") {
				t.Errorf("expected a failed check line naming $OPENCODE_CONFIG_CONTENT, got %q (full output:\n%s)", line, output)
			}
		})
	}
}

// TestStatusOpencode_EmptyStoreHealthy exercises the full DB path against a
// fresh (empty) database with a stubbed Ollama, verifying the total==0
// embeddings check passes and the whole run stays healthy.
func TestStatusOpencode_EmptyStoreHealthy(t *testing.T) {
	ollama := ollamaStub("nomic-embed-text:v1.5")
	defer ollama.Close()

	statusEnv(t)
	writeGhostConfigFile(t, fmt.Sprintf(`embedding:
  ollama_url: %s
  model: nomic-embed-text:v1.5
  dimensions: 768
  enabled: true
`, ollama.URL))
	// The environment layer outranks the config file, so a host's own
	// GHOST_OLLAMA_URL / GHOST_EMBEDDING_MODEL / GHOST_EMBEDDING_DIMENSIONS
	// would redirect the probe away from the stub and fail the run on a host
	// with an Ollama configured. Pin them to the stub's values.
	t.Setenv("GHOST_EMBEDDING_ENABLED", "true")
	t.Setenv("GHOST_OLLAMA_URL", ollama.URL)
	t.Setenv("GHOST_EMBEDDING_MODEL", "nomic-embed-text:v1.5")
	t.Setenv("GHOST_EMBEDDING_DIMENSIONS", "768")
	binDir := writeStubGhost(t)
	t.Setenv("PATH", binDir)

	ghostDir := filepath.Join(os.Getenv("XDG_DATA_HOME"), "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir ghost dir: %v", err)
	}
	db, err := memory.OpenDB(filepath.Join(ghostDir, "ghost.db"))
	if err != nil {
		t.Fatalf("open fresh db: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close fresh db: %v", err)
	}

	installOpencodePluginFile(t, stubPath(binDir, "ghost"))
	writeOpencodeMCPConfig(t, "opencode.json", opencodeMCPRegistration(stubPath(binDir, "ghost")))

	var out bytes.Buffer
	healthy, err := StatusOpencode(&out)
	if err != nil {
		t.Fatalf("StatusOpencode: %v", err)
	}
	if !healthy {
		t.Error("StatusOpencode: healthy = false, want true for an empty but valid store")
	}

	output := out.String()
	for _, want := range []string{
		"✓ Ollama model nomic-embed-text:v1.5 installed",
		"✓ embeddings: 0 memories (store empty)",
		"All checks passed.",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("expected %q in output, got:\n%s", want, output)
		}
	}
}

// TestReportConfigFile_MalformedReported pins that an unparseable config file
// is surfaced here. `ghost mcp status` is where a user goes when their
// settings "don't take effect", and while the parse error was dropped every
// downstream check silently ran on the compiled defaults.
func TestReportConfigFile_MalformedReported(t *testing.T) {
	statusEnv(t)
	path := writeGhostConfigFile(t, "embedding:\n  model: \"nomic-embed-text\n")

	var out bytes.Buffer
	reportConfigFile(&out)

	got := out.String()
	if !strings.Contains(got, path) {
		t.Errorf("expected the config path %q in output, got: %s", path, got)
	}
	if !strings.Contains(got, "parse "+path) {
		t.Errorf("expected the parse error for %q in output, got: %s", path, got)
	}
}

// TestCheckStoreHealth_StatsReadFailureIsReported pins that a store whose
// embedding or link tables cannot be counted is reported as a `!` line and
// does not fail the run. Both stats reads run only when embedding is enabled,
// and a failure there used to fall through the `err == nil` guard with no
// output at all, so a run that could not determine embedding or link coverage
// printed every check it did make as passing with no sign of the gap. The
// reporting is print-only — like reportHistoryGrowth's own unreadable read —
// because a store that opened cleanly and then cannot be read is not wiring
// `ghost mcp init` repairs, so the run stays healthy and the footer is never
// printed.
func TestCheckStoreHealth_StatsReadFailureIsReported(t *testing.T) {
	ollama := ollamaStub("nomic-embed-text:v1.5")
	defer ollama.Close()

	statusEnv(t)
	writeGhostConfigFile(t, fmt.Sprintf(`embedding:
  ollama_url: %s
  model: nomic-embed-text:v1.5
  dimensions: 768
  enabled: true
`, ollama.URL))
	t.Setenv("GHOST_EMBEDDING_ENABLED", "true")
	t.Setenv("GHOST_OLLAMA_URL", ollama.URL)
	binDir := writeStubGhost(t)
	t.Setenv("PATH", binDir)

	ghostDir := filepath.Join(os.Getenv("XDG_DATA_HOME"), "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir ghost dir: %v", err)
	}
	db, err := memory.OpenDB(filepath.Join(ghostDir, "ghost.db"))
	if err != nil {
		t.Fatalf("open fresh db: %v", err)
	}
	for _, table := range []string{"memory_embeddings", "memory_links", "link_scans"} {
		if _, err := db.Exec("DROP TABLE " + table); err != nil {
			t.Fatalf("drop %s: %v", table, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close fresh db: %v", err)
	}

	installOpencodePluginFile(t, stubPath(binDir, "ghost"))
	writeOpencodeMCPConfig(t, "opencode.json", opencodeMCPRegistration(stubPath(binDir, "ghost")))

	var out bytes.Buffer
	healthy, err := StatusOpencode(&out)
	if err != nil {
		t.Fatalf("StatusOpencode: %v", err)
	}
	if !healthy {
		t.Errorf("StatusOpencode: healthy = false, want true when only the stats reads fail; they are reported, not failed:\n%s", out.String())
	}

	output := out.String()
	// Each line must carry the store's OWN error text, so a bare
	// "! embeddings: unavailable" that dropped the error would fail here: the
	// table name is the driver's message for the store this test broke.
	for _, want := range []string{
		"! embeddings:",
		"no such table: memory_embeddings",
		"! memory links:",
		"no such table: memory_links",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("expected %q in output, got:\n%s", want, output)
		}
	}
	if strings.Contains(output, "fix issues") {
		t.Errorf("a print-only stats failure must not print the init footer, got:\n%s", output)
	}
}

// TestCheckEmbeddingStats pins the embedding-stats classification: an empty
// store passes with "(store empty)", a populated store passes only when some
// memories are embedded, and a populated store with zero embeddings fails. It
// also pins the split of the gap: stale rows (a vector under a retired
// identity) are named separately from rows that never had one, but only when
// there is at least one of them.
func TestCheckEmbeddingStats(t *testing.T) {
	cases := []struct {
		embedded, stale, total int
		wantPass               bool
		wantText               string
	}{
		{0, 0, 0, true, "embeddings: 0 memories (store empty)"},
		{0, 0, 5, false, "embeddings: 0/5 memories — vector search and linking inactive"},
		{5, 0, 5, true, "embeddings: 5/5 memories"},
		// Partial coverage is the state a model/dimension/task-prefix change
		// leaves behind, so the line has to say the gap is pending work rather
		// than let "3/5" read as three-fifths of the corpus being searchable.
		// With nothing stale the plain form stands: "0 stale" beside the
		// ordinary first-embed gap would name a diagnosis that does not apply.
		{3, 0, 5, true, "embeddings: 3/5 memories (2 awaiting re-embed)"},
		// Stale rows are the re-embed worker's queued work; unembedded rows
		// have never had a vector. Telling them apart is the whole point of
		// the split — "wait for the worker" and "find out why it never ran"
		// are different answers to "why is search missing memories?".
		{3, 2, 5, true, "embeddings: 3/5 memories (2 awaiting re-embed: 2 stale, 0 unembedded)"},
		{1, 1, 5, true, "embeddings: 1/5 memories (4 awaiting re-embed: 1 stale, 3 unembedded)"},
		// A fully retired store fails, and the failing line is where the
		// operator most needs to see that the vectors exist but are unusable.
		{0, 5, 5, false, "embeddings: 0/5 memories — vector search and linking inactive: 5 stale, 0 unembedded"},
	}
	for _, c := range cases {
		var passed bool
		var passText, failText string
		check := func(ok bool, pass, fail string) {
			passed = ok
			passText = pass
			failText = fail
		}
		checkEmbeddingStats(check, c.embedded, c.stale, c.total)
		if passed != c.wantPass {
			t.Errorf("embedded=%d stale=%d total=%d: want ok=%v, got %v", c.embedded, c.stale, c.total, c.wantPass, passed)
		}
		text := passText
		if !passed {
			text = failText
		}
		if text != c.wantText {
			t.Errorf("embedded=%d stale=%d total=%d: want text %q, got %q", c.embedded, c.stale, c.total, c.wantText, text)
		}
	}
}

// TestStatus_OllamaDownDurationReported verifies that a down-since marker
// written by the embedding worker (internal/embedding.Worker) is surfaced as
// a duration alongside a currently-unreachable Ollama — the core behavior
// requested by issue #287.
func TestStatus_OllamaDownDurationReported(t *testing.T) {
	statusEnv(t)
	t.Setenv("PATH", writeStubGhost(t))

	srv := ollamaStub()
	url := srv.URL
	srv.Close() // unreachable — Status's own live check must see it down too

	writeGhostConfigFile(t, fmt.Sprintf(`embedding:
  ollama_url: %s
  model: nomic-embed-text:v1.5
  dimensions: 768
  enabled: true
`, url))
	t.Setenv("GHOST_EMBEDDING_ENABLED", "true")
	t.Setenv("GHOST_OLLAMA_URL", url)

	ghostDir := filepath.Join(os.Getenv("XDG_DATA_HOME"), "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir ghost dir: %v", err)
	}
	since := time.Now().Add(-(2*time.Hour + 3*time.Minute))
	marker := since.UTC().Format(time.RFC3339)
	if err := os.WriteFile(filepath.Join(ghostDir, embedding.OllamaDownMarkerFilename), []byte(marker), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	var out bytes.Buffer
	if _, err := Status(&out); err != nil {
		t.Fatalf("Status: %v", err)
	}

	want := fmt.Sprintf("Ollama down since %s (2h 3m)", since.UTC().Format("2006-01-02 15:04 UTC"))
	if !strings.Contains(out.String(), want) {
		t.Errorf("expected output to contain %q, got:\n%s", want, out.String())
	}
}

// TestStatus_OllamaDownDurationSuppressedWhenInconclusive is the regression
// test for a liveness probe that missed its own deadline. `Inconclusive` is
// evidence of nothing — the endpoint may be listening and merely slow — so
// `ghost mcp status` must not turn it into the positive, timed claim
// "Ollama down since …": that line reads the worker's down marker, which a
// currently-up-but-busy Ollama can leave stamped for hours, and the probe that
// is supposed to authorise the claim established no outage at all. The
// endpoint must be reported as slow or undetermined instead.
func TestStatus_OllamaDownDurationSuppressedWhenInconclusive(t *testing.T) {
	statusEnv(t)
	t.Setenv("PATH", writeStubGhost(t))

	// A handler that never answers. The liveness probe's own deadline expires
	// with nothing written, which is the Inconclusive verdict — not a refusal.
	release := make(chan struct{})
	wedged := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	// Defers run LIFO, so the handler is released before Close waits on it.
	defer wedged.Close()
	defer close(release)

	model := "nomic-embed-text:v1.5"
	// Assert the liveness state directly, so the test cannot pass on a
	// different verdict reached for some other reason.
	if got := embedding.NewClient(wedged.URL, model, 768).Probe(context.Background()); got != embedding.Inconclusive {
		t.Fatalf("Probe at an endpoint that never answers = %v, want Inconclusive", got)
	}

	writeGhostConfigFile(t, fmt.Sprintf(`embedding:
  ollama_url: %s
  model: %s
  dimensions: 768
  enabled: true
`, wedged.URL, model))
	t.Setenv("GHOST_EMBEDDING_ENABLED", "true")
	t.Setenv("GHOST_OLLAMA_URL", wedged.URL)

	ghostDir := filepath.Join(os.Getenv("XDG_DATA_HOME"), "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir ghost dir: %v", err)
	}
	stale := time.Now().Add(-5 * time.Hour).UTC().Format(time.RFC3339)
	if err := os.WriteFile(filepath.Join(ghostDir, embedding.OllamaDownMarkerFilename), []byte(stale), 0o600); err != nil {
		t.Fatalf("write stale marker: %v", err)
	}

	var out bytes.Buffer
	if _, err := Status(&out); err != nil {
		t.Fatalf("Status: %v", err)
	}

	if strings.Contains(out.String(), "Ollama down since") {
		t.Errorf("an inconclusive liveness probe must not report a down-duration, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "liveness undetermined") {
		t.Errorf("expected the expired probe to be reported as undetermined, got:\n%s", out.String())
	}
}

// TestFindClientBinary_IgnoresSystemDirs verifies that the auto-detect lookup
// reports a client in the home bin dir but not one that exists only in a
// system bin dir, while findBinary still finds the system-dir one.
func TestFindClientBinary_IgnoresSystemDirs(t *testing.T) {
	setHome(t, t.TempDir())
	t.Setenv("PATH", t.TempDir())
	sysDir := t.TempDir()
	orig := systemBinDirs
	systemBinDirs = []string{sysDir}
	t.Cleanup(func() { systemBinDirs = orig })

	sysStub := writeStub(t, sysDir, "claude")
	if got := FindClientBinary("claude"); got != "" {
		t.Fatalf("FindClientBinary found %q in a system dir, want none", got)
	}
	if got := findBinary("claude"); got != sysStub {
		t.Fatalf("findBinary = %q, want %q", got, sysStub)
	}

	home, _ := os.UserHomeDir()
	localBin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(localBin, 0o755); err != nil {
		t.Fatal(err)
	}
	homeStub := writeStub(t, localBin, "claude")
	if got := FindClientBinary("claude"); got != homeStub {
		t.Fatalf("FindClientBinary = %q, want %q", got, homeStub)
	}
}
