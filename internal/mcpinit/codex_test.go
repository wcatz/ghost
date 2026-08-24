package mcpinit

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupCodexTestEnv isolates a RunCodex/StatusCodex test: HOME points at a
// temp dir (the integration installs to <HOME>/.codex), XDG dirs point at
// temp dirs, PATH contains only a stub ghost binary (no claude, no codex),
// and embeddings are disabled so the Ollama check stays offline.
func setupCodexTestEnv(t *testing.T) (home, xdg string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	xdg = t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "")
	t.Setenv("GHOST_EMBEDDING_ENABLED", "false")

	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeStub(t, binDir, "ghost")
	t.Setenv("PATH", binDir)
	return home, xdg
}

func codexConfigToml(home string) string {
	return filepath.Join(home, ".codex", "config.toml")
}

func codexHooksJSON(home string) string {
	return filepath.Join(home, ".codex", "hooks.json")
}

func TestRunCodex_FreshInstall(t *testing.T) {
	home, xdg := setupCodexTestEnv(t)
	ghostBin := filepath.Join(home, "bin", "ghost")

	var out bytes.Buffer
	if err := RunCodex(&out, false); err != nil {
		t.Fatalf("RunCodex: %v", err)
	}

	// config.toml: textual [mcp_servers.ghost] merge with the absolute binary.
	tomlData, err := os.ReadFile(codexConfigToml(home))
	if err != nil {
		t.Fatalf("expected config.toml installed: %v", err)
	}
	if want := renderCodexMCPServerBlock(ghostBin); !strings.Contains(string(tomlData), want) {
		t.Errorf("config.toml should contain the canonical ghost block:\nwant:\n%s\ngot:\n%s", want, tomlData)
	}
	start, end, found := findCodexTOMLTable(strings.Split(string(tomlData), "\n"), codexMCPServerKey)
	if !found || !codexMCPCurrent(parseCodexMCPServerBlock(strings.Split(string(tomlData), "\n")[start:end]), ghostBin) {
		t.Error("config.toml ghost table should parse to command=<abs ghost> args=[\"mcp\"]")
	}

	// hooks.json: all three lifecycle events wired to the contract with
	// --source codex; only SessionEnd carries an explicit timeout.
	hooksData, err := os.ReadFile(codexHooksJSON(home))
	if err != nil {
		t.Fatalf("expected hooks.json installed: %v", err)
	}
	var doc struct {
		Description string `json:"description"`
		Hooks       map[string][]struct {
			Matcher *string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(hooksData, &doc); err != nil {
		t.Fatalf("hooks.json invalid JSON: %v", err)
	}
	if !strings.Contains(doc.Description, "Ghost") {
		t.Errorf("expected a ghost description, got %q", doc.Description)
	}
	for event, token := range map[string]string{
		"SessionStart": "session-start",
		"Stop":         "stop",
		"SessionEnd":   "session-end",
	} {
		rules := doc.Hooks[event]
		if len(rules) != 1 || len(rules[0].Hooks) != 1 {
			t.Errorf("%s: expected one rule with one action, got %+v", event, rules)
			continue
		}
		action := rules[0].Hooks[0]
		wantCmd := "'" + ghostBin + "' hook " + token + " --source codex"
		if action.Type != "command" || action.Command != wantCmd {
			t.Errorf("%s command = %+v, want type=command cmd=%q", event, action, wantCmd)
		}
		if rules[0].Matcher != nil && *rules[0].Matcher != "" {
			t.Errorf("%s matcher should be omitted, got %q", event, *rules[0].Matcher)
		}
		if wantTimeout := map[string]int{"SessionEnd": 3}[event]; action.Timeout != wantTimeout {
			t.Errorf("%s timeout = %d, want %d", event, action.Timeout, wantTimeout)
		}
	}

	output := out.String()
	for _, want := range []string{
		"+ created config.toml with the ghost MCP server",
		"+ installed hooks.json",
		"/hooks",
		"Restart codex to activate.",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("expected %q in output, got:\n%s", want, output)
		}
	}

	// Step 2 bootstraps ghost's own user config.
	if _, err := os.Stat(filepath.Join(xdg, "ghost", "config.yaml")); err != nil {
		t.Errorf("expected ghost config bootstrap to run: %v", err)
	}
}

func TestRunCodex_Idempotent(t *testing.T) {
	home, _ := setupCodexTestEnv(t)

	var first bytes.Buffer
	if err := RunCodex(&first, false); err != nil {
		t.Fatalf("RunCodex (first): %v", err)
	}
	snapshots := map[string][]byte{
		"config.toml": nil,
		"hooks.json":  nil,
	}
	for name, path := range map[string]string{
		"config.toml": codexConfigToml(home),
		"hooks.json":  codexHooksJSON(home),
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		snapshots[name] = data
	}

	var second bytes.Buffer
	if err := RunCodex(&second, false); err != nil {
		t.Fatalf("RunCodex (second): %v", err)
	}
	out2 := second.String()
	if strings.Contains(out2, "+ ") {
		t.Errorf("second run must not install anything, got:\n%s", out2)
	}
	if !strings.Contains(out2, "✓ ghost MCP server already registered") ||
		!strings.Contains(out2, "✓ hooks already wired") {
		t.Errorf("second run should report both artifacts current, got:\n%s", out2)
	}
	for name, path := range map[string]string{
		"config.toml": codexConfigToml(home),
		"hooks.json":  codexHooksJSON(home),
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("re-read %s: %v", name, err)
		}
		if !bytes.Equal(data, snapshots[name]) {
			t.Errorf("second run must not alter an identical %s", name)
		}
	}
}

// TestRunCodex_TOMLAppendPreservesContent seeds a user config with comments
// and another MCP server, then verifies the merge appends exactly the ghost
// block plus one separator newline — every pre-existing byte intact.
func TestRunCodex_TOMLAppendPreservesContent(t *testing.T) {
	home, _ := setupCodexTestEnv(t)
	ghostBin := filepath.Join(home, "bin", "ghost")

	if err := os.MkdirAll(filepath.Dir(codexConfigToml(home)), 0755); err != nil {
		t.Fatal(err)
	}
	seed := `# my model settings
model = "gpt-5.6"

# a colleague's local server
[mcp_servers.other]
command = "/bin/other"
args = ["serve"]
`
	if err := os.WriteFile(codexConfigToml(home), []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := RunCodex(&out, false); err != nil {
		t.Fatalf("RunCodex: %v", err)
	}

	got, err := os.ReadFile(codexConfigToml(home))
	if err != nil {
		t.Fatal(err)
	}
	want := seed + "\n" + renderCodexMCPServerBlock(ghostBin)
	if string(got) != want {
		t.Errorf("merged config.toml mismatch:\nwant:\n%q\ngot:\n%q", want, string(got))
	}
}

// TestRunCodex_TOMLDriftRepairPreservesNeighbors replaces a stale ghost block
// in place while leaving surrounding tables and comments byte-identical.
func TestRunCodex_TOMLDriftRepairPreservesNeighbors(t *testing.T) {
	home, _ := setupCodexTestEnv(t)
	ghostBin := filepath.Join(home, "bin", "ghost")

	if err := os.MkdirAll(filepath.Dir(codexConfigToml(home)), 0755); err != nil {
		t.Fatal(err)
	}
	seed := `# my model settings
model = "gpt-5.6"

[mcp_servers.other]
command = "/bin/other"

[mcp_servers.ghost]
command = '/old/install/ghost'
args = ["mcp"]

# settings for the CI profile live here
[profiles.ci]
model = "gpt-5-mini"
`
	if err := os.WriteFile(codexConfigToml(home), []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := RunCodex(&out, false); err != nil {
		t.Fatalf("RunCodex: %v", err)
	}
	if !strings.Contains(out.String(), "+ repaired the ghost MCP server entry") {
		t.Errorf("drift repair should be reported, got:\n%s", out.String())
	}

	got, err := os.ReadFile(codexConfigToml(home))
	if err != nil {
		t.Fatal(err)
	}
	want := `# my model settings
model = "gpt-5.6"

[mcp_servers.other]
command = "/bin/other"

` + renderCodexMCPServerBlock(ghostBin) + `
# settings for the CI profile live here
[profiles.ci]
model = "gpt-5-mini"
`
	if string(got) != want {
		t.Errorf("repaired config.toml mismatch:\nwant:\n%q\ngot:\n%q", want, string(got))
	}
}

// TestRunCodex_HooksMergePreservesUserHooks verifies that merging into an
// existing hooks.json keeps the user's description, foreign events, and
// non-ghost rules (including unknown per-action fields) intact.
func TestRunCodex_HooksMergePreservesUserHooks(t *testing.T) {
	home, _ := setupCodexTestEnv(t)

	if err := os.MkdirAll(filepath.Dir(codexHooksJSON(home)), 0755); err != nil {
		t.Fatal(err)
	}
	seed := `{
  "description": "my personal hooks",
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [{"type": "command", "command": "echo hi", "statusMessage": "checking"}]
      }
    ],
    "SessionStart": [
      {"hooks": [{"type": "command", "command": "/usr/bin/env fortune"}]}
    ]
  }
}`
	if err := os.WriteFile(codexHooksJSON(home), []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := RunCodex(&out, false); err != nil {
		t.Fatalf("RunCodex: %v", err)
	}

	data, err := os.ReadFile(codexHooksJSON(home))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Description string                     `json:"description"`
		Hooks       map[string]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("merged hooks.json invalid JSON: %v", err)
	}
	if doc.Description != "my personal hooks" {
		t.Errorf("user description must survive, got %q", doc.Description)
	}

	var sessionStart []struct {
		Hooks []map[string]any `json:"hooks"`
	}
	if err := json.Unmarshal(doc.Hooks["SessionStart"], &sessionStart); err != nil {
		t.Fatal(err)
	}
	if len(sessionStart) != 2 {
		t.Fatalf("user SessionStart rule should be preserved alongside ghost's, got %d rules", len(sessionStart))
	}
	if sessionStart[0].Hooks[0]["command"] != "/usr/bin/env fortune" {
		t.Errorf("user SessionStart rule must come first untouched, got %+v", sessionStart[0])
	}

	var preToolUse []struct {
		Matcher string           `json:"matcher"`
		Hooks   []map[string]any `json:"hooks"`
	}
	if err := json.Unmarshal(doc.Hooks["PreToolUse"], &preToolUse); err != nil {
		t.Fatal(err)
	}
	if len(preToolUse) != 1 || preToolUse[0].Matcher != "Bash" || preToolUse[0].Hooks[0]["statusMessage"] != "checking" {
		t.Errorf("user PreToolUse rule must be preserved verbatim, got %+v", preToolUse)
	}

	// The other lifecycle events gained exactly one ghost rule each.
	var stop []struct {
		Hooks []struct{ Command string } `json:"hooks"`
	}
	if err := json.Unmarshal(doc.Hooks["Stop"], &stop); err != nil {
		t.Fatal(err)
	}
	if len(stop) != 1 || len(stop[0].Hooks) != 1 || !strings.Contains(stop[0].Hooks[0].Command, "--source codex") {
		t.Errorf("Stop should carry exactly the ghost rule, got %+v", stop)
	}
}

func TestRunCodex_DryRunWritesNothing(t *testing.T) {
	home, xdg := setupCodexTestEnv(t)

	var out bytes.Buffer
	if err := RunCodex(&out, true); err != nil {
		t.Fatalf("RunCodex dry run: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "~ would") {
		t.Errorf("dry run should preview changes, got:\n%s", output)
	}
	if !strings.Contains(output, "No changes made (dry run).") {
		t.Errorf("dry run footer missing, got:\n%s", output)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex")); !os.IsNotExist(err) {
		t.Error("dry run must not create ~/.codex/")
	}
	cfgPath := filepath.Join(xdg, "ghost", "config.yaml")
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Error("dry run must not write the ghost config file")
	}
}

// TestRunCodex_CODEXHomeRelocatesArtifacts verifies $CODEX_HOME moves both
// artifacts instead of ~/.codex.
func TestRunCodex_CODEXHomeRelocatesArtifacts(t *testing.T) {
	home, _ := setupCodexTestEnv(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)

	var out bytes.Buffer
	if err := RunCodex(&out, false); err != nil {
		t.Fatalf("RunCodex: %v", err)
	}

	for _, name := range []string{"config.toml", "hooks.json"} {
		if _, err := os.Stat(filepath.Join(codexHome, name)); err != nil {
			t.Errorf("expected %s under $CODEX_HOME: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".codex")); !os.IsNotExist(err) {
		t.Error("~/.codex must not be created when CODEX_HOME is set")
	}
}

func TestStatusCodex_HealthyAfterInstall(t *testing.T) {
	home, _ := setupCodexTestEnv(t)
	ghostBin := filepath.Join(home, "bin", "ghost")

	var install bytes.Buffer
	if err := RunCodex(&install, false); err != nil {
		t.Fatalf("RunCodex: %v", err)
	}

	var out bytes.Buffer
	healthy, err := StatusCodex(&out)
	if err != nil {
		t.Fatalf("StatusCodex: %v", err)
	}
	if !healthy {
		t.Errorf("StatusCodex: healthy = false, want true after install, got:\n%s", out.String())
	}
	output := out.String()
	for _, want := range []string{
		"✓ ghost binary: " + ghostBin,
		"✓ ghost MCP server registered in config.toml",
		"✓ SessionStart hook configured",
		"✓ Stop hook configured",
		"✓ SessionEnd hook configured",
		"/hooks",
		"- no Ghost database (run ghost first)",
		"All checks passed.",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("expected %q in output, got:\n%s", want, output)
		}
	}
	if strings.Contains(output, "✗") {
		t.Errorf("a fresh install must have no failed checks, got:\n%s", output)
	}
}

func TestStatusCodex_UnhealthyWithoutInstall(t *testing.T) {
	cases := map[string]struct {
		seed     func(t *testing.T, home string)
		wantFail string
	}{
		"nothing installed": {seed: func(t *testing.T, home string) {}, wantFail: "config.toml not found"},
		"drifted hook source": {seed: func(t *testing.T, home string) {
			path := codexHooksJSON(home)
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
			doc := map[string]any{
				"hooks": map[string]any{
					"SessionStart": []any{map[string]any{
						"hooks": []any{map[string]any{"type": "command", "command": "'/bin/ghost' hook session-start --source claude-code"}},
					}},
				},
			}
			data, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0644); err != nil {
				t.Fatal(err)
			}
		}, wantFail: "SessionStart hook missing or miswired"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			home, _ := setupCodexTestEnv(t)
			tc.seed(t, home)

			var out bytes.Buffer
			healthy, err := StatusCodex(&out)
			if err != nil {
				t.Fatalf("StatusCodex: %v", err)
			}
			if healthy {
				t.Errorf("%s: healthy = true, want false without valid wiring", name)
			}
			output := out.String()
			if !strings.Contains(output, tc.wantFail) {
				t.Errorf("%s: expected failure %q, got:\n%s", name, tc.wantFail, output)
			}
			if strings.Contains(output, "All checks passed.") {
				t.Errorf("%s: must not report \"All checks passed.\", got:\n%s", name, output)
			}
			if !strings.Contains(output, "Run `ghost mcp init --client codex` to fix issues.") {
				t.Errorf("%s: expected actionable footer, got:\n%s", name, output)
			}
		})
	}
}

func TestStatusCodex_GhostMissing(t *testing.T) {
	_, _ = setupCodexTestEnv(t)
	t.Setenv("PATH", t.TempDir())

	var out bytes.Buffer
	healthy, err := StatusCodex(&out)
	if err != nil {
		t.Fatalf("StatusCodex: %v", err)
	}
	if healthy {
		t.Error("StatusCodex: healthy = true, want false when the ghost binary is missing")
	}
	if !strings.Contains(out.String(), "✗ ghost binary not found in PATH") {
		t.Errorf("expected a failed ghost binary check, got:\n%s", out.String())
	}
}

// TestCodexContractHookWired_FlagForms pins the token validation: both
// --source flag forms count, lookalikes and wrong sources never do.
func TestCodexContractHookWired_FlagForms(t *testing.T) {
	rule := func(cmd string) []json.RawMessage {
		raw, err := json.Marshal(codexHookRule{Hooks: []codexHookAction{{Type: "command", Command: cmd}}})
		if err != nil {
			t.Fatal(err)
		}
		return []json.RawMessage{raw}
	}

	cases := []struct {
		cmd  string
		want bool
	}{
		{"'/opt/bin/ghost' hook stop --source codex", true},
		{`"/opt/bin/ghost" hook stop --source=codex`, true},
		{"'/opt/bin/ghost' hook stop", false},
		{"'/opt/bin/ghost' hook stop --source claude-code", false},
		{"'/opt/bin/ghost' hook stop --source codex-extra", false},
	}
	for _, tc := range cases {
		if got := codexContractHookWired(rule(tc.cmd), "stop", "codex"); got != tc.want {
			t.Errorf("codexContractHookWired(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}
