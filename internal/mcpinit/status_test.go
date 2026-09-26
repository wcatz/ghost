package mcpinit

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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

// TestStatus_HookMatchWithQuotedPath verifies that a SessionStart hook whose
// command is a quoted binary path (the form `ghost mcp init` writes on
// Windows, e.g. `"C:\Users\ghost\bin\ghost.exe" hook session-start --source
// claude-code`) is recognized as configured. Before this fix, Status checked
// for the literal substring "ghost hook session-start", which never appears
// once the binary path is quoted — so a fully healthy install was reported as
// missing the hook on every platform where ghostBin isn't literally "ghost".
func TestStatus_HookMatchWithQuotedPath(t *testing.T) {
	statusEnv(t)
	home := os.Getenv("HOME")
	settingsDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatalf("mkdir settings dir: %v", err)
	}
	settings := `{
  "hooks": {
    "SessionStart": [{"matcher": "", "hooks": [{"type": "command", "command": "\"/opt/ghost/bin/ghost\" hook session-start --source claude-code"}]}],
    "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "\"/opt/ghost/bin/ghost\" hook stop --source claude-code"}]}]
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
		t.Errorf("expected SessionStart hook to be recognized as configured, got:\n%s", output)
	}
	if strings.Contains(output, "✗ SessionStart hook missing") {
		t.Errorf("a quoted-path hook command must not be reported missing, got:\n%s", output)
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
	// opencode's env config layers must be empty by default: the tests that
	// need them set them explicitly, and a leaked value from the host would
	// otherwise decide which config file the registration check reads.
	t.Setenv("OPENCODE_CONFIG", "")
	t.Setenv("OPENCODE_CONFIG_CONTENT", "")
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
// Using os.UserConfigDir here would diverge on macOS, where it ignores
// XDG_CONFIG_HOME and returns ~/Library/Application Support, making
// reportConfigFile report "no config file" for a file that exists.
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

// TestStatusOpencode_GhostMissing verifies a missing ghost binary is reported
// as a failed check and blocks "All checks passed.".
func TestStatusOpencode_GhostMissing(t *testing.T) {
	statusEnv(t)

	var out bytes.Buffer
	healthy, err := StatusOpencode(&out)
	if err != nil {
		t.Fatalf("StatusOpencode: %v", err)
	}
	if healthy {
		t.Error("StatusOpencode: healthy = true, want false when the ghost binary is missing")
	}

	output := out.String()
	if !strings.Contains(output, "✗ ghost binary not found in PATH") {
		t.Errorf("expected a failed ghost binary check, got:\n%s", output)
	}
	if strings.Contains(output, "All checks passed.") {
		t.Errorf("missing ghost must not report \"All checks passed.\", got:\n%s", output)
	}
}

// TestStatusOpencode_CleanSetupHealthy verifies a clean opencode setup — a
// ghost binary on PATH, the lifecycle plugin installed, and an enabled
// mcp.ghost registration in the opencode config — is fully healthy and
// prints "All checks passed." without a Claude binary. The plugin is what
// bridges stop events, but it is not the only thing status verifies: the
// registration opencode reads from its own config must be there too.
func TestStatusOpencode_CleanSetupHealthy(t *testing.T) {
	statusEnv(t)
	binDir := writeStubGhost(t)
	t.Setenv("PATH", binDir)
	ghostBin := stubPath(binDir, "ghost")
	installOpencodePluginFile(t, ghostBin)
	writeOpencodeMCPConfig(t, "opencode.jsonc", opencodeMCPRegistration(ghostBin))

	var out bytes.Buffer
	healthy, err := StatusOpencode(&out)
	if err != nil {
		t.Fatalf("StatusOpencode: %v", err)
	}
	if !healthy {
		t.Error("StatusOpencode: healthy = false, want true for a clean opencode setup")
	}

	output := out.String()
	for _, want := range []string{
		"✓ ghost binary: " + ghostBin,
		"✓ lifecycle plugin installed: ",
		"✓ ghost MCP server registered in opencode config",
		"- no Ghost database (run ghost first)",
		"All checks passed.",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("expected %q in output, got:\n%s", want, output)
		}
	}
	if strings.Contains(output, "✗") {
		t.Errorf("a clean opencode setup must have no failed checks, got:\n%s", output)
	}
}

// TestStatusOpencode_PluginMissing verifies the lifecycle plugin check fails
// (and blocks "All checks passed.") when the plugin file is absent, lacks the
// versioned marker, or drifted while keeping its header — a corrupted file
// must not read healthy just because its header survived.
func TestStatusOpencode_PluginMissing(t *testing.T) {
	cases := map[string]func(t *testing.T){
		"absent": func(t *testing.T) {},
		"unrelated file": func(t *testing.T) {
			writePluginFile(t, "// some other plugin")
		},
		"drifted but marker retained": func(t *testing.T) {
			writePluginFile(t, opencodeGhostPluginTS[:200]+"\n// truncated/corrupted tail")
		},
	}
	for name, seed := range cases {
		t.Run(name, func(t *testing.T) {
			statusEnv(t)
			binDir := writeStubGhost(t)
			t.Setenv("PATH", binDir)
			// The registration is seeded too, so this table isolates the
			// plugin check: exactly one check may fail per case.
			writeOpencodeMCPConfig(t, "opencode.jsonc", opencodeMCPRegistration(stubPath(binDir, "ghost")))
			seed(t)

			var out bytes.Buffer
			healthy, err := StatusOpencode(&out)
			if err != nil {
				t.Fatalf("StatusOpencode: %v", err)
			}
			if healthy {
				t.Errorf("%s: healthy = true, want false without a valid lifecycle plugin", name)
			}

			output := out.String()
			if !strings.Contains(output, "✗ lifecycle plugin missing or outdated") {
				t.Errorf("%s: expected failed plugin check, got:\n%s", name, output)
			}
			if strings.Contains(output, "All checks passed.") {
				t.Errorf("%s: must not report \"All checks passed.\", got:\n%s", name, output)
			}
		})
	}
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
		}},
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
				if !strings.Contains(output, "Run `ghost mcp init --client opencode` to fix issues.") {
					t.Errorf("%s: expected actionable footer, got:\n%s", name, output)
				}
				if !strings.Contains(output, "mcp.ghost entry shown above is a config edit") {
					t.Errorf("%s: expected the config-edit footer naming the entry init never writes, got:\n%s", name, output)
				}
			})
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

// TestStatusOpencode_OPENCODEConfigEnv pins that the custom config path
// opencode itself honors ($OPENCODE_CONFIG, precedence between the global
// file and project configs) is part of what status reports on, merged over
// the global file the way opencode merges config layers: a higher layer
// overriding `enabled` wins, and a higher layer without the entry must not
// hide the global one.
func TestStatusOpencode_OPENCODEConfigEnv(t *testing.T) {
	t.Run("custom path overriding enabled wins", func(t *testing.T) {
		statusEnv(t)
		binDir := writeStubGhost(t)
		t.Setenv("PATH", binDir)
		// No plugin: the gate is hard, so the verdict must come from the
		// merged config files rather than the plugin's runtime registration.
		writeOpencodeMCPConfig(t, "opencode.jsonc", opencodeMCPRegistration(stubPath(binDir, "ghost")))
		custom := filepath.Join(t.TempDir(), "custom-opencode.jsonc")
		if err := os.WriteFile(custom, []byte(`{"mcp": {"ghost": {"enabled": false}}}`), 0o644); err != nil {
			t.Fatalf("write custom config: %v", err)
		}
		t.Setenv("OPENCODE_CONFIG", custom)

		var out bytes.Buffer
		healthy, err := StatusOpencode(&out)
		if err != nil {
			t.Fatalf("StatusOpencode: %v", err)
		}
		output := out.String()
		if healthy {
			t.Errorf("healthy = true — $OPENCODE_CONFIG disabling ghost must fail the run, got:\n%s", output)
		}
		line := statusLineContaining(output, "ghost MCP server disabled")
		if !strings.HasPrefix(line, "  ✗ ") {
			t.Errorf("expected a failed check line, got %q (full output:\n%s)", line, output)
		}
		if !strings.Contains(line, custom) {
			t.Errorf("expected the failure to name $OPENCODE_CONFIG %q, got %q", custom, line)
		}
	})

	t.Run("custom path enabling a disabled global entry wins", func(t *testing.T) {
		statusEnv(t)
		binDir := writeStubGhost(t)
		t.Setenv("PATH", binDir)
		// The documented override pattern: the lower layer carries the whole
		// entry but disabled, the higher layer flips only `enabled` — the
		// command below must survive the merge.
		writeOpencodeMCPConfig(t, "opencode.jsonc", fmt.Sprintf(
			`{"mcp": {"ghost": {"type": "local", "command": [%q, "mcp"], "enabled": false}}}`,
			stubPath(binDir, "ghost")))
		custom := filepath.Join(t.TempDir(), "enable-ghost.jsonc")
		if err := os.WriteFile(custom, []byte(`{"mcp": {"ghost": {"enabled": true}}}`), 0o644); err != nil {
			t.Fatalf("write custom config: %v", err)
		}
		t.Setenv("OPENCODE_CONFIG", custom)

		var out bytes.Buffer
		if _, err := StatusOpencode(&out); err != nil {
			t.Fatalf("StatusOpencode: %v", err)
		}
		output := out.String()
		if !strings.Contains(output, "✓ ghost MCP server registered in opencode config") {
			t.Errorf("the custom layer must enable the merged entry without losing the command, got:\n%s", output)
		}
	})

	t.Run("custom path without the entry keeps the global one", func(t *testing.T) {
		statusEnv(t)
		binDir := writeStubGhost(t)
		t.Setenv("PATH", binDir)
		writeOpencodeMCPConfig(t, "opencode.jsonc", opencodeMCPRegistration(stubPath(binDir, "ghost")))
		// A policy-style override file carrying no mcp key at all — opencode
		// merges it with the global file, so the entry below still applies.
		custom := filepath.Join(t.TempDir(), "policy-only.jsonc")
		if err := os.WriteFile(custom, []byte(`{"permissions": {"*": "ask"}}`), 0o644); err != nil {
			t.Fatalf("write custom config: %v", err)
		}
		t.Setenv("OPENCODE_CONFIG", custom)

		var out bytes.Buffer
		if _, err := StatusOpencode(&out); err != nil {
			t.Fatalf("StatusOpencode: %v", err)
		}
		output := out.String()
		if !strings.Contains(output, "✓ ghost MCP server registered in opencode config") {
			t.Errorf("the global entry must survive a $OPENCODE_CONFIG file without one, got:\n%s", output)
		}
		if strings.Contains(output, "ghost MCP server missing from") {
			t.Errorf("must not report the entry missing when the global file provides it, got:\n%s", output)
		}
	})

	t.Run("unset custom path file falls back to the global file", func(t *testing.T) {
		statusEnv(t)
		binDir := writeStubGhost(t)
		t.Setenv("PATH", binDir)
		writeOpencodeMCPConfig(t, "opencode.jsonc", opencodeMCPRegistration(stubPath(binDir, "ghost")))
		t.Setenv("OPENCODE_CONFIG", filepath.Join(t.TempDir(), "does-not-exist.json"))

		var out bytes.Buffer
		if _, err := StatusOpencode(&out); err != nil {
			t.Fatalf("StatusOpencode: %v", err)
		}
		output := out.String()
		if !strings.Contains(output, "✓ ghost MCP server registered in opencode config") {
			t.Errorf("a $OPENCODE_CONFIG pointing at no file must not hide the global entry, got:\n%s", output)
		}
	})
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
	t.Setenv("GHOST_EMBEDDING_ENABLED", "true")
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

// TestReportConfigFile_Missing verifies the config file is reported
// informationally when absent, without failing the check (the config file is
// optional — defaults work without one).
func TestReportConfigFile_Missing(t *testing.T) {
	statusEnv(t)

	var out bytes.Buffer
	reportConfigFile(&out)

	if !strings.Contains(out.String(), "no config file") {
		t.Errorf("expected 'no config file' in output, got: %s", out.String())
	}
}

// TestReportConfigFile_Present verifies the config file's path is reported
// when it exists.
func TestReportConfigFile_Present(t *testing.T) {
	statusEnv(t)
	path := writeGhostConfigFile(t, "")

	var out bytes.Buffer
	reportConfigFile(&out)

	if !strings.Contains(out.String(), path) {
		t.Errorf("expected config file path %q in output, got: %s", path, out.String())
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

// TestCheckStoreHealth_MalformedConfigStillRunsChecks pins that a config file
// which does not parse costs the user the embedding/linking health checks. It
// did once: they were gated on config.Load succeeding, so a broken file — the
// exact case `ghost mcp status` is documented to diagnose — dropped the Ollama,
// embedding-coverage and link lines and could still print "All checks passed."
// while vector search and linking were in fact off. reportConfigFile reports
// the parse error; these checks must still run, on the fallback config.
func TestCheckStoreHealth_MalformedConfigStillRunsChecks(t *testing.T) {
	statusEnv(t)
	// statusEnv disables embedding so the other status tests make no network
	// call. Here that would be self-defeating: the fallback is the environment
	// plus the compiled defaults, and with embedding off checkOllama returns
	// early without reporting through the check closure, so the test could not
	// tell "the checks were dropped" from "embedding is off". Turn it back on so
	// the Ollama check is the one that must appear.
	t.Setenv("GHOST_EMBEDDING_ENABLED", "true")
	writeGhostConfigFile(t, "embedding:\n  model: \"nomic-embed-text\n")

	var out bytes.Buffer
	var ran []string
	store := checkStoreHealth(&out, func(ok bool, pass, fail string) {
		ran = append(ran, pass+fail)
	})
	if store != nil {
		store.Close() //nolint:errcheck
	}

	if len(ran) == 0 {
		t.Errorf("no health check ran on a broken config; the embedding/linking checks were dropped:\n%s", out.String())
	}
	// Whichever way the live probe goes, the Ollama check reports through the
	// check closure with a message naming Ollama.
	if !slices.ContainsFunc(ran, func(s string) bool { return strings.Contains(s, "Ollama") }) {
		t.Errorf("expected the Ollama check to run on the fallback config, got checks: %v", ran)
	}
}

// TestCheckEmbeddingStats pins the embedding-stats classification: an empty
// store passes with "(store empty)", a populated store passes only when some
// memories are embedded, and a populated store with zero embeddings fails.
func TestCheckEmbeddingStats(t *testing.T) {
	cases := []struct {
		embedded, total int
		wantPass        bool
		wantText        string
	}{
		{0, 0, true, "embeddings: 0 memories (store empty)"},
		{0, 5, false, "embeddings: 0/5 memories — vector search and linking inactive"},
		{5, 5, true, "embeddings: 5/5 memories"},
		// Partial coverage is the state a model/dimension/task-prefix change
		// leaves behind, so the line has to say the gap is pending work rather
		// than let "3/5" read as three-fifths of the corpus being searchable.
		{3, 5, true, "embeddings: 3/5 memories (2 awaiting re-embed)"},
	}
	for _, c := range cases {
		var passed bool
		var passText, failText string
		check := func(ok bool, pass, fail string) {
			passed = ok
			passText = pass
			failText = fail
		}
		checkEmbeddingStats(check, c.embedded, c.total)
		if passed != c.wantPass {
			t.Errorf("embedded=%d total=%d: want ok=%v, got %v", c.embedded, c.total, c.wantPass, passed)
		}
		text := passText
		if !passed {
			text = failText
		}
		if text != c.wantText {
			t.Errorf("embedded=%d total=%d: want text %q, got %q", c.embedded, c.total, c.wantText, text)
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

// TestStatus_OllamaDownDurationSuppressedWhenReachable is the regression test
// for the stale-marker failure mode: if the embedding worker's MCP server
// process exits while Ollama is down, nothing removes the marker file until
// a new worker instance later observes Ollama reachable again. `ghost mcp
// status` must not print a contradictory "Ollama down since ..." line next
// to a passing, live "Ollama model installed" check just because a stale
// marker happens to still be sitting on disk.
func TestStatus_OllamaDownDurationSuppressedWhenReachable(t *testing.T) {
	statusEnv(t)
	t.Setenv("PATH", writeStubGhost(t))

	model := "nomic-embed-text:v1.5"
	ollama := ollamaStub(model)
	defer ollama.Close()

	writeGhostConfigFile(t, fmt.Sprintf(`embedding:
  ollama_url: %s
  model: %s
  dimensions: 768
  enabled: true
`, ollama.URL, model))
	t.Setenv("GHOST_EMBEDDING_ENABLED", "true")

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
		t.Errorf("a currently-reachable Ollama must not report a stale down-duration, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "✓ Ollama model "+model+" installed") {
		t.Errorf("expected the live Ollama check to still pass, got:\n%s", out.String())
	}
}

// TestStatus_NoOllamaDownDurationWhenAbsent pins the no-marker baseline: a
// clean install with embedding disabled (statusEnv's default) never prints an
// Ollama-down duration line.
func TestStatus_NoOllamaDownDurationWhenAbsent(t *testing.T) {
	statusEnv(t)
	t.Setenv("PATH", writeStubGhost(t))

	var out bytes.Buffer
	if _, err := Status(&out); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if strings.Contains(out.String(), "Ollama down since") {
		t.Errorf("expected no Ollama-down line without a marker file, got:\n%s", out.String())
	}
}
