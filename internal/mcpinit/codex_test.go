package mcpinit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	setHome(t, home)
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
	ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")

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
		// Rendered exactly as production renders it (platform shellQuote);
		// the quote styles themselves are pinned by TestShellQuote*.
		wantCmd := shellQuote(ghostBin) + " hook " + token + " --source codex"
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
	ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")

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
	ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")

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

// TestRunCodex_TOMLDriftRepairPreservesSubTables pins the byte-preservation
// promise of the textual merge: a repair rewrites ONLY the ghost table's own
// command/args keys. A hand-tuned [mcp_servers.ghost.env] sub-table, the user's
// other keys and comments inside the ghost table, and every unrelated table
// come through byte-identical.
func TestRunCodex_TOMLDriftRepairPreservesSubTables(t *testing.T) {
	home, _ := setupCodexTestEnv(t)
	ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")

	seed := "# my model settings\n" +
		"model = \"gpt-5.6\"\n" +
		"\n" +
		"[mcp_servers.other]\n" +
		"command = \"/bin/other\"\n" +
		"\n" +
		"# Ghost persistent memory (managed by `ghost mcp init --client codex`)\n" +
		"[mcp_servers.ghost]\n" +
		"# the mcp subcommand only, never a wrapper script\n" +
		"command = '/old/install/ghost'\n" +
		"args = [\"mcp\", \"--stale-flag\"]\n" +
		"startup_timeout_sec = 30\n" +
		"\n" +
		"# hand-tuned: keep the server off my login shell environment\n" +
		"[mcp_servers.ghost.env]\n" +
		"GHOST_PROFILE = \"personal\"\n" +
		"RUST_LOG = \"warn\"\n" +
		"\n" +
		"# settings for the CI profile live here\n" +
		"[profiles.ci]\n" +
		"model = \"gpt-5-mini\"\n"
	envTable := "# hand-tuned: keep the server off my login shell environment\n" +
		"[mcp_servers.ghost.env]\n" +
		"GHOST_PROFILE = \"personal\"\n" +
		"RUST_LOG = \"warn\"\n"

	if err := os.MkdirAll(filepath.Dir(codexConfigToml(home)), 0755); err != nil {
		t.Fatal(err)
	}
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
	// Only the two drifted keys move: command is repointed at the resolved
	// binary, args is reset to the contract invocation.
	want := "# my model settings\n" +
		"model = \"gpt-5.6\"\n" +
		"\n" +
		"[mcp_servers.other]\n" +
		"command = \"/bin/other\"\n" +
		"\n" +
		"# Ghost persistent memory (managed by `ghost mcp init --client codex`)\n" +
		"[mcp_servers.ghost]\n" +
		"# the mcp subcommand only, never a wrapper script\n" +
		"command = " + codexTOMLString(ghostBin) + "\n" +
		"args = [\"mcp\"]\n" +
		"startup_timeout_sec = 30\n" +
		"\n" + envTable +
		"\n" +
		"# settings for the CI profile live here\n" +
		"[profiles.ci]\n" +
		"model = \"gpt-5-mini\"\n"
	if string(got) != want {
		t.Errorf("repaired config.toml mismatch:\nwant:\n%s\ngot:\n%s", want, got)
	}
	if !strings.Contains(string(got), envTable) {
		t.Errorf("hand-tuned [mcp_servers.ghost.env] sub-table was not preserved:\n%s", got)
	}
}

// TestRunCodex_TOMLRepairInsertsMissingKeys covers the other half of the
// contract: a ghost table that defines neither command nor args (only user
// keys, or nothing but an env sub-table) gets the owned keys inserted without
// losing the lines already there.
func TestRunCodex_TOMLRepairInsertsMissingKeys(t *testing.T) {
	cases := map[string]struct{ seed, want string }{
		"table with only the user's own keys": {
			seed: "[mcp_servers.ghost]\n" +
				"startup_timeout_sec = 30\n" +
				"\n" +
				"[mcp_servers.ghost.env]\n" +
				"GHOST_PROFILE = \"personal\"\n",
			// The owned pair is inserted ahead of the user's key, and the
			// managed marker added above the header.
			want: "# Ghost persistent memory (managed by `ghost mcp init --client codex`)\n" +
				"[mcp_servers.ghost]\n" +
				"command = {BIN}\n" +
				"args = [\"mcp\"]\n" +
				"startup_timeout_sec = 30\n" +
				"\n" +
				"[mcp_servers.ghost.env]\n" +
				"GHOST_PROFILE = \"personal\"\n",
		},
		"bare header followed by a sub-table": {
			seed: "[mcp_servers.ghost]\n" +
				"\n" +
				"[mcp_servers.ghost.env]\n" +
				"GHOST_PROFILE = \"personal\"\n",
			want: "# Ghost persistent memory (managed by `ghost mcp init --client codex`)\n" +
				"[mcp_servers.ghost]\n" +
				"command = {BIN}\n" +
				"args = [\"mcp\"]\n" +
				"\n" +
				"[mcp_servers.ghost.env]\n" +
				"GHOST_PROFILE = \"personal\"\n",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			home, _ := setupCodexTestEnv(t)
			ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")
			want := strings.ReplaceAll(tc.want, "{BIN}", codexTOMLString(ghostBin))

			if err := os.MkdirAll(filepath.Dir(codexConfigToml(home)), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(codexConfigToml(home), []byte(tc.seed), 0644); err != nil {
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
			if string(got) != want {
				t.Errorf("repaired config.toml mismatch:\nwant:\n%q\ngot:\n%q", want, got)
			}

			// The inserted entry must read as current, so a second init is a
			// no-op rather than another rewrite.
			if !codexMCPCurrent(parseCodexMCPServerBlock(mustCodexGhostSpan(t, string(got))), ghostBin) {
				t.Error("inserted ghost table should parse to the current command/args")
			}
			var second bytes.Buffer
			if err := RunCodex(&second, false); err != nil {
				t.Fatalf("RunCodex (second): %v", err)
			}
			if !strings.Contains(second.String(), "✓ ghost MCP server already registered") {
				t.Errorf("second init should find the entry current, got:\n%s", second.String())
			}
			again, err := os.ReadFile(codexConfigToml(home))
			if err != nil {
				t.Fatal(err)
			}
			if string(again) != string(got) {
				t.Errorf("second init must not alter the file:\nwas:\n%s\nnow:\n%s", got, again)
			}
		})
	}
}

// TestRunCodex_TOMLRepairLeavesSubTableKeysAlone pins the span boundary: a
// sub-table under the ghost table may hold a key that shares an owned key's
// name, and that line belongs to the user. Reading it as the server's own
// command would both misreport drift and rewrite the user's sub-table.
func TestRunCodex_TOMLRepairLeavesSubTableKeysAlone(t *testing.T) {
	home, _ := setupCodexTestEnv(t)
	ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")

	seed := "# Ghost persistent memory (managed by `ghost mcp init --client codex`)\n" +
		"[mcp_servers.ghost]\n" +
		"command = '/old/install/ghost'\n" +
		"args = [\"mcp\"]\n" +
		"\n" +
		"[mcp_servers.ghost.overrides]\n" +
		"command = \"/usr/local/bin/patch-me\"\n" +
		"args = [\"--mine\"]\n"
	want := "# Ghost persistent memory (managed by `ghost mcp init --client codex`)\n" +
		"[mcp_servers.ghost]\n" +
		"command = " + codexTOMLString(ghostBin) + "\n" +
		"args = [\"mcp\"]\n" +
		"\n" +
		"[mcp_servers.ghost.overrides]\n" +
		"command = \"/usr/local/bin/patch-me\"\n" +
		"args = [\"--mine\"]\n"

	if err := os.MkdirAll(filepath.Dir(codexConfigToml(home)), 0755); err != nil {
		t.Fatal(err)
	}
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
	if string(got) != want {
		t.Errorf("repaired config.toml mismatch:\nwant:\n%q\ngot:\n%q", want, got)
	}
}

// TestRunCodex_TOMLRepairNormalizesQuotedOwnedKey pins the quoted spelling of
// an owned key: TOML reads "command" and command as the same key, so the
// quoted line has to be rewritten in place. Leaving it beside a freshly
// inserted bare line would hand codex a duplicate-key document.
func TestRunCodex_TOMLRepairNormalizesQuotedOwnedKey(t *testing.T) {
	home, _ := setupCodexTestEnv(t)
	ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")

	seed := "# Ghost persistent memory (managed by `ghost mcp init --client codex`)\n" +
		"[mcp_servers.ghost]\n" +
		"\"command\" = '/old/install/ghost'\n" +
		"'args' = [\"mcp\", \"--stale\"]\n"
	want := "# Ghost persistent memory (managed by `ghost mcp init --client codex`)\n" +
		"[mcp_servers.ghost]\n" +
		"command = " + codexTOMLString(ghostBin) + "\n" +
		"args = [\"mcp\"]\n"

	if err := os.MkdirAll(filepath.Dir(codexConfigToml(home)), 0755); err != nil {
		t.Fatal(err)
	}
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
	if string(got) != want {
		t.Errorf("repaired config.toml mismatch:\nwant:\n%q\ngot:\n%q", want, got)
	}
	if strings.Contains(string(got), `"command"`) || strings.Contains(string(got), `'args'`) {
		t.Error("the quoted owned keys were left beside the bare ones")
	}
	// A quoted spelling must also read as current without a rewrite, so init
	// does not churn a hand-written table on every run.
	fresh := "# Ghost persistent memory (managed by `ghost mcp init --client codex`)\n" +
		"[mcp_servers.ghost]\n" +
		"\"command\" = " + codexTOMLString(ghostBin) + "\n" +
		"args = [\"mcp\"]\n"
	if err := os.WriteFile(codexConfigToml(home), []byte(fresh), 0644); err != nil {
		t.Fatal(err)
	}
	var second bytes.Buffer
	if err := RunCodex(&second, false); err != nil {
		t.Fatalf("RunCodex (second): %v", err)
	}
	if !strings.Contains(second.String(), "✓ ghost MCP server already registered") {
		t.Errorf("a quoted command key should read as current, got:\n%s", second.String())
	}
}

// TestRunCodex_TOMLRepairValueEndings covers the two ways a value's bracket
// count lies about where the value ends. A "#" comment can hold an unbalanced
// bracket, and a backslash-escaped quote inside a basic string can look like
// the end of the string. If the scanner believes the value is still running,
// the repair swallows every following line as part of it and deletes the
// user's keys.
func TestRunCodex_TOMLRepairValueEndings(t *testing.T) {
	cases := map[string]struct{ seed, want string }{
		"trailing comment holding an unbalanced bracket": {
			// The "[" in the comment is not TOML structure.
			seed: "[mcp_servers.ghost]\n" +
				"command = \"/old/install/ghost\" # was [v1\n" +
				"startup_timeout_sec = 30\n" +
				"cwd = \"/x\"\n" +
				"args = [\"mcp\", \"--stale\"]\n",
			want: "[mcp_servers.ghost]\n" +
				"command = {BIN}\n" +
				"startup_timeout_sec = 30\n" +
				"cwd = \"/x\"\n" +
				"args = [\"mcp\"]\n",
		},
		"comment inside a multi-line array": {
			// A comment at bracket depth > 0 must not end the scan, or the
			// array looks unfinished and everything below it is deleted.
			seed: "[mcp_servers.ghost]\n" +
				"command = '/old/install/ghost'\n" +
				"args = [\n" +
				"  \"mcp\", # only the stdio server\n" +
				"]\n" +
				"keep = 1\n" +
				"\n" +
				"[mcp_servers.ghost.env]\n" +
				"GHOST_PROFILE = \"personal\"\n" +
				"\n" +
				"[profiles.ci]\n" +
				"model = \"gpt-5-mini\"\n",
			want: "[mcp_servers.ghost]\n" +
				"command = {BIN}\n" +
				"args = [\"mcp\"]\n" +
				"keep = 1\n" +
				"\n" +
				"[mcp_servers.ghost.env]\n" +
				"GHOST_PROFILE = \"personal\"\n" +
				"\n" +
				"[profiles.ci]\n" +
				"model = \"gpt-5-mini\"\n",
		},
		"unquoted nested array element": {
			// [1, 2] is bracketed like a header but is not one: treating it as
			// a table header ends the span early, which drops env_keep's opening
			// bracket and leaves a duplicate command/args below.
			seed: "[mcp_servers.ghost]\n" +
				"env_keep = [\n" +
				"  [1, 2]\n" +
				"]\n" +
				"command = '/old/install/ghost'\n" +
				"args = [\"mcp\"]\n",
			want: "[mcp_servers.ghost]\n" +
				"env_keep = [\n" +
				"  [1, 2]\n" +
				"]\n" +
				"command = {BIN}\n" +
				"args = [\"mcp\"]\n",
		},
		"nested array element with a dotted string": {
			// ["A", "b.c"] starts and ends with a quote but holds two values,
			// so it must not pass for one quoted key part.
			seed: "[mcp_servers.ghost]\n" +
				"env_keep = [\n" +
				"  [\"A\", \"b.c\"]\n" +
				"]\n" +
				"command = '/old/install/ghost'\n" +
				"args = [\"mcp\"]\n",
			want: "[mcp_servers.ghost]\n" +
				"env_keep = [\n" +
				"  [\"A\", \"b.c\"]\n" +
				"]\n" +
				"command = {BIN}\n" +
				"args = [\"mcp\"]\n",
		},
		"nested array element holding a bare value": {
			// [true] is a plausible table header too, so only the fact that a
			// bare true is a value and not a key keeps it in the array above.
			seed: "[mcp_servers.ghost]\n" +
				"env_keep = [\n" +
				"  [true]\n" +
				"]\n" +
				"command = '/old/install/ghost'\n" +
				"args = [\"mcp\"]\n",
			want: "[mcp_servers.ghost]\n" +
				"env_keep = [\n" +
				"  [true]\n" +
				"]\n" +
				"command = {BIN}\n" +
				"args = [\"mcp\"]\n",
		},
		"escaped quote inside a basic string": {
			// args = ["a\"["] holds one element, a"[ , so it ends on that line.
			seed: "[mcp_servers.ghost]\n" +
				"command = \"/old/install/ghost\"\n" +
				"args = [\"a\\\"[\"]\n" +
				"keep = 1\n",
			want: "[mcp_servers.ghost]\n" +
				"command = {BIN}\n" +
				"args = [\"mcp\"]\n" +
				"keep = 1\n",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			home, _ := setupCodexTestEnv(t)
			ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")
			// A table that carries no managed marker gets one added above it.
			want := codexMCPServerComment + "\n" +
				strings.ReplaceAll(tc.want, "{BIN}", codexTOMLString(ghostBin))

			if err := os.MkdirAll(filepath.Dir(codexConfigToml(home)), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(codexConfigToml(home), []byte(tc.seed), 0644); err != nil {
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
			if string(got) != want {
				t.Errorf("repaired config.toml mismatch:\nwant:\n%q\ngot:\n%q", want, got)
			}
		})
	}
}

// TestRunCodex_TOMLHeaderSpellings covers the ways a user can spell the ghost
// table header. Every spelling that names mcp_servers.ghost must be repaired in
// place: treating an unrecognised spelling as "absent" appends a second table,
// which is a duplicate-key document codex cannot parse.
func TestRunCodex_TOMLHeaderSpellings(t *testing.T) {
	for _, header := range []string{
		"[mcp_servers.ghost]",          // the canonical spelling
		"[mcp_servers.ghost] # mine",   // trailing comment
		"[\"mcp_servers\".\"ghost\"]",  // quoted key parts
		"[ mcp_servers . ghost ]",      // whitespace around the dots
		"[mcp_servers.ghost ]  # mine", // both
	} {
		t.Run(header, func(t *testing.T) {
			home, _ := setupCodexTestEnv(t)
			ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")

			seed := header + "\n" +
				"command = '/old/install/ghost'\n" +
				"args = [\"mcp\"]\n" +
				"\n" +
				"[profiles.ci]\n" +
				"model = \"gpt-5-mini\"\n"
			// The user's header line is preserved verbatim; only the two owned
			// keys are rewritten, and the managed marker is added above.
			want := codexMCPServerComment + "\n" +
				header + "\n" +
				"command = " + codexTOMLString(ghostBin) + "\n" +
				"args = [\"mcp\"]\n" +
				"\n" +
				"[profiles.ci]\n" +
				"model = \"gpt-5-mini\"\n"

			if err := os.MkdirAll(filepath.Dir(codexConfigToml(home)), 0755); err != nil {
				t.Fatal(err)
			}
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
			if string(got) != want {
				t.Errorf("repaired config.toml mismatch:\nwant:\n%q\ngot:\n%q", want, got)
			}
			if n := countCodexGhostHeaders(string(got)); n != 1 {
				t.Errorf("the ghost table must have exactly one header, got %d:\n%s", n, got)
			}
			if !strings.Contains(out.String(), "+ repaired the ghost MCP server entry") {
				t.Errorf("expected a repair, got:\n%s", out.String())
			}
		})
	}
}

// TestRunCodex_TOMLLeadingBOM covers a config.toml whose first line starts with
// a UTF-8 byte-order mark, which a Windows editor leaves there. Codex's TOML
// lexer strips one before it reads the document, so such a file is valid codex
// config, but U+FEFF is not whitespace: a scanner that only trims space cannot
// normalise a BOM-prefixed header, so init read the ghost table as absent and
// appended a SECOND one (a duplicate-key document TOML rejects) while status
// read the same way and called the broken install healthy. The merge strips one
// leading BOM before scanning and writes it back, so the repair lands in place
// and the user's file keeps its encoding.
func TestRunCodex_TOMLLeadingBOM(t *testing.T) {
	const bom = "\ufeff"

	t.Run("table header", func(t *testing.T) {
		home, _ := setupCodexTestEnv(t)
		ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")

		seed := bom + "[mcp_servers.ghost]\n" +
			"command = '/old/install/ghost'\n" +
			"args = [\"mcp\"]\n" +
			"\n" +
			"[profiles.ci]\n" +
			"model = \"gpt-5-mini\"\n"
		// The BOM stays at the very start of the file, where the TOML parser
		// expects it; only the two owned keys are rewritten.
		want := bom + codexMCPServerComment + "\n" +
			"[mcp_servers.ghost]\n" +
			"command = " + codexTOMLString(ghostBin) + "\n" +
			"args = [\"mcp\"]\n" +
			"\n" +
			"[profiles.ci]\n" +
			"model = \"gpt-5-mini\"\n"

		if err := os.MkdirAll(filepath.Dir(codexConfigToml(home)), 0755); err != nil {
			t.Fatal(err)
		}
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
		if string(got) != want {
			t.Errorf("repaired config.toml mismatch:\nwant:\n%q\ngot:\n%q", want, got)
		}
		if n := countCodexGhostHeaders(string(got)); n != 1 {
			t.Errorf("the ghost table must have exactly one header, got %d:\n%s", n, got)
		}
		if !strings.HasPrefix(string(got), bom) {
			t.Errorf("the byte-order mark should be preserved on write, got:\n%q", got)
		}
		if !strings.Contains(out.String(), "+ repaired the ghost MCP server entry") {
			t.Errorf("expected a repair, got:\n%s", out.String())
		}

		// A second run over the repaired file is a fixed point.
		var again bytes.Buffer
		if err := RunCodex(&again, false); err != nil {
			t.Fatalf("RunCodex (second run): %v", err)
		}
		gotAgain, err := os.ReadFile(codexConfigToml(home))
		if err != nil {
			t.Fatal(err)
		}
		if string(gotAgain) != string(got) {
			t.Errorf("second run changed the file:\nbefore:\n%q\nafter:\n%q", got, gotAgain)
		}

		var status bytes.Buffer
		if _, err := StatusCodex(&status); err != nil {
			t.Fatalf("StatusCodex: %v", err)
		}
		if !strings.Contains(status.String(), "✓ ghost MCP server registered in config.toml") {
			t.Errorf("status should see the repaired table as registered, got:\n%s", status.String())
		}
	})

	// The BOM must not hide the other two guards: a dotted or inline definition
	// would otherwise be appended beside, and an unparseable header would get a
	// second table written next to it.
	t.Run("dotted key", func(t *testing.T) {
		home, _ := setupCodexTestEnv(t)
		seed := bom + "mcp_servers.ghost = { command = \"/bin/ghost\", args = [\"mcp\"] }\n"

		if err := os.MkdirAll(filepath.Dir(codexConfigToml(home)), 0755); err != nil {
			t.Fatal(err)
		}
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
		if string(got) != seed {
			t.Errorf("a dotted definition must be left unchanged, got:\n%s", got)
		}
		if !strings.Contains(out.String(), "dotted or inline key") {
			t.Errorf("expected a dotted-key warning, got:\n%s", out.String())
		}
	})

	t.Run("ambiguous header", func(t *testing.T) {
		home, _ := setupCodexTestEnv(t)
		seed := bom + "[[mcp_servers.ghost]]\ncommand = \"/bin/ghost\"\n"

		if err := os.MkdirAll(filepath.Dir(codexConfigToml(home)), 0755); err != nil {
			t.Fatal(err)
		}
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
		if string(got) != seed {
			t.Errorf("an unparseable header must be left unchanged, got:\n%s", got)
		}
		if !strings.Contains(out.String(), "left unchanged") {
			t.Errorf("expected a warning that the file was left unchanged, got:\n%s", out.String())
		}
	})
}

// TestRunCodex_TOMLAmbiguousGhostHeaderLeftAlone covers headers that name ghost
// but cannot be parsed with confidence, or that use a structure ghost does not
// manage. init must leave such a file alone: reading it as "absent" would append
// a duplicate table, and rewriting inside a table whose shape we do not
// understand risks eating the user's keys.
func TestRunCodex_TOMLAmbiguousGhostHeaderLeftAlone(t *testing.T) {
	// A malformed value above the header must not hide it: an array-of-tables
	// header can never be an array element, so the value does not swallow it.
	for _, header := range []string{
		"[mcp_servers.ghost.]",    // trailing dot: no such key path
		"[mcp_servers..ghost]",    // empty key part
		"[[mcp_servers.ghost]]",   // array of tables, not a table
		"[[mcp_servers.ghost",     // truncated array-of-tables header
		"[\"mcp_servers.ghost\"]", // one literal key holding a dot, not a path
		"[\"mcp_servers\".ghost",  // unterminated header
	} {
		t.Run(header, func(t *testing.T) {
			home, _ := setupCodexTestEnv(t)

			seed := "# my model settings\n" +
				"model = \"gpt-5.6\"\n" +
				"unterminated = [1, 2\n" +
				"\n" +
				header + "\n" +
				"command = \"/bin/ghost\"\n" +
				"\n" +
				"[profiles.ci]\n" +
				"model = \"gpt-5-mini\"\n"
			if err := os.MkdirAll(filepath.Dir(codexConfigToml(home)), 0755); err != nil {
				t.Fatal(err)
			}
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
			if string(got) != seed {
				t.Errorf("config.toml must be left byte-for-byte unchanged, got:\n%s", got)
			}
			output := out.String()
			if !strings.Contains(output, "left unchanged") {
				t.Errorf("expected a warning that the file was left unchanged, got:\n%s", output)
			}
			if !strings.Contains(output, fmt.Sprintf("%q", header)) {
				t.Errorf("the warning should quote the offending header %q, got:\n%s", header, output)
			}

			var status bytes.Buffer
			if _, err := StatusCodex(&status); err != nil {
				t.Fatalf("StatusCodex: %v", err)
			}
			if strings.Contains(status.String(), "missing from config.toml") {
				t.Errorf("status must not call a present server missing, got:\n%s", status.String())
			}
		})
	}
}

// countCodexGhostHeaders counts table headers in a config.toml body that name
// the ghost MCP server, in any spelling. A repair must leave exactly one: a
// second one is a duplicate-key document codex cannot parse.
func countCodexGhostHeaders(content string) int {
	n := 0
	for _, line := range strings.Split(content, "\n") {
		// Strip a byte-order mark the line may carry: only the file's first
		// line has one, but a repair can prepend a comment above it.
		trimmed := strings.TrimPrefix(strings.TrimSpace(line), "\ufeff")
		if !strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if name, ok := normaliseCodexTableName(trimmed); ok && name == codexMCPServerKey {
			n++
		}
	}
	return n
}

// TestRunCodex_TOMLRepairKeepsNestedArray covers a bracketed line that is not a
// table header: an array element nested inside a multi-line value. Read as a
// header it would end the ghost table's span early, so the repair would insert a
// fresh command/args pair above the user's own keys and leave the originals
// below, which codex rejects as a duplicate-key document.
func TestRunCodex_TOMLRepairKeepsNestedArray(t *testing.T) {
	home, _ := setupCodexTestEnv(t)
	ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")

	// The nested ["b"] element is bracketed and carries no '=', so only
	// continuation tracking tells it apart from a table header.
	seed := "[mcp_servers.ghost]\n" +
		"command = '/old/install/ghost'\n" +
		"env_keep = [\n" +
		"  \"a\",\n" +
		"  [\"b\"]\n" +
		"]\n" +
		"args = [\"mcp\", \"--stale\"]\n"
	want := codexMCPServerComment + "\n" +
		"[mcp_servers.ghost]\n" +
		"command = " + codexTOMLString(ghostBin) + "\n" +
		"env_keep = [\n" +
		"  \"a\",\n" +
		"  [\"b\"]\n" +
		"]\n" +
		"args = [\"mcp\"]\n"

	if err := os.MkdirAll(filepath.Dir(codexConfigToml(home)), 0755); err != nil {
		t.Fatal(err)
	}
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
	if string(got) != want {
		t.Errorf("repaired config.toml mismatch:\nwant:\n%q\ngot:\n%q", want, got)
	}
	if n := countCodexGhostHeaders(string(got)); n != 1 {
		t.Errorf("the ghost table must have exactly one header, got %d:\n%s", n, got)
	}
}

// TestRunCodex_TOMLRepairSurvivesMultilineString is the #608 case. A TOML
// multi-line string — three single quotes, or three double quotes — runs until
// its own closing delimiter, so a line inside one that happens to start with
// '[' is string CONTENT and not a table header. Read as a header it ends the
// ghost table's span early, and the repair then writes ghost's own `args` into a
// span that already has one — a second args line, which is invalid TOML and
// makes codex reject the whole file.
func TestRunCodex_TOMLRepairSurvivesMultilineString(t *testing.T) {
	for _, delim := range []string{"'''", `"""`} {
		t.Run(delim, func(t *testing.T) {
			home, _ := setupCodexTestEnv(t)
			ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")

			seed := "[mcp_servers.ghost]\n" +
				"command = '/old/install/ghost'\n" +
				"description = " + delim + "\n" +
				"[not_a_table]\n" +
				delim + "\n" +
				"args = [\"mcp\", \"--stale\"]\n"
			want := codexMCPServerComment + "\n" +
				"[mcp_servers.ghost]\n" +
				"command = " + codexTOMLString(ghostBin) + "\n" +
				"description = " + delim + "\n" +
				"[not_a_table]\n" +
				delim + "\n" +
				"args = [\"mcp\"]\n"

			if err := os.MkdirAll(filepath.Dir(codexConfigToml(home)), 0755); err != nil {
				t.Fatal(err)
			}
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
			if string(got) != want {
				t.Errorf("repaired config.toml mismatch:\nwant:\n%q\ngot:\n%q", want, got)
			}
			if n := countCodexGhostHeaders(string(got)); n != 1 {
				t.Errorf("the ghost table must have exactly one header, got %d:\n%s", n, got)
			}
		})
	}
}

// TestRunCodex_TOMLRepairSurvivesAnUnterminatedMultilineString is the other
// half of the multi-line case, and the one the first fix got wrong. Honouring a
// multi-line string means a header inside it is content — but an opener that is
// never closed would then hold the value open for every remaining line, so the
// malformation rule could never fire again and every table below the typo would
// be invisible to all three scanners. A [mcp_servers.ghost] table below the typo
// would then read as absent, and init would append a SECOND copy of it.
//
// The ghost table is above the typo as well as below in the sub-cases, because
// the same swallowing widens a span past the next header and drops that
// server's keys — the other half of what the rule prevents.
func TestRunCodex_TOMLRepairSurvivesAnUnterminatedMultilineString(t *testing.T) {
	t.Run("ghost table below the typo", func(t *testing.T) {
		home, _ := setupCodexTestEnv(t)
		ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")

		seed := "# a user's own table\n" +
			"[unrelated]\n" +
			"description = '''\n" +
			"a string the author never closed\n" +
			"\n" +
			"[mcp_servers.ghost]\n" +
			"command = '/old/install/ghost'\n" +
			"args = [\"mcp\", \"--stale\"]\n"
		want := "# a user's own table\n" +
			"[unrelated]\n" +
			"description = '''\n" +
			"a string the author never closed\n" +
			"\n" +
			codexMCPServerComment + "\n" +
			"[mcp_servers.ghost]\n" +
			"command = " + codexTOMLString(ghostBin) + "\n" +
			"args = [\"mcp\"]\n"

		if err := os.MkdirAll(filepath.Dir(codexConfigToml(home)), 0755); err != nil {
			t.Fatal(err)
		}
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
		if n := countCodexGhostHeaders(string(got)); n != 1 {
			t.Errorf("the ghost table must have exactly one header, got %d:\n%s", n, got)
		}
		if string(got) != want {
			t.Errorf("repaired config.toml mismatch:\nwant:\n%q\ngot:\n%q", want, got)
		}
	})

	t.Run("the next server's keys survive the ghost table above the typo", func(t *testing.T) {
		home, _ := setupCodexTestEnv(t)
		ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")

		seed := "[mcp_servers.ghost]\n" +
			"command = '/old/install/ghost'\n" +
			"description = '''\n" +
			"never closed\n" +
			"\n" +
			"[mcp_servers.other]\n" +
			"command = \"/usr/bin/other\"\n" +
			"args = [\"serve\"]\n"

		if err := os.MkdirAll(filepath.Dir(codexConfigToml(home)), 0755); err != nil {
			t.Fatal(err)
		}
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
		if n := countCodexGhostHeaders(string(got)); n != 1 {
			t.Errorf("the ghost table must have exactly one header, got %d:\n%s", n, got)
		}
		if !strings.Contains(string(got), `command = "/usr/bin/other"`) {
			t.Errorf("the neighbouring server's key was swept into the ghost table's span:\n%s", got)
		}
		if !strings.Contains(string(got), `args = ["serve"]`) {
			t.Errorf("the neighbouring server's args were dropped:\n%s", got)
		}
		_ = ghostBin
	})
}

// TestRunCodex_TOMLRepairKeepsCRLF pins that a repair does not leave a CRLF
// config.toml with mixed line endings. The file is split on "\n", so every
// preserved line keeps its "\r" while ghost's own key lines carry none — and a
// document with two endings in it is one a reviewer cannot read and an editor
// will rewrite. The repair is not the place to convert the file, so it keeps
// the ending the file already had.
func TestRunCodex_TOMLRepairKeepsCRLF(t *testing.T) {
	home, _ := setupCodexTestEnv(t)
	ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")

	seed := "[mcp_servers.ghost]\r\n" +
		"command = '/old/install/ghost'\r\n" +
		"args = [\"mcp\", \"--stale\"]\r\n" +
		"\r\n" +
		"[mcp_servers.other]\r\n" +
		"command = \"/usr/bin/other\"\r\n"
	want := codexMCPServerComment + "\r\n" +
		"[mcp_servers.ghost]\r\n" +
		"command = " + codexTOMLString(ghostBin) + "\r\n" +
		"args = [\"mcp\"]\r\n" +
		"\r\n" +
		"[mcp_servers.other]\r\n" +
		"command = \"/usr/bin/other\"\r\n"

	if err := os.MkdirAll(filepath.Dir(codexConfigToml(home)), 0755); err != nil {
		t.Fatal(err)
	}
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
	if string(got) != want {
		t.Errorf("repaired config.toml mismatch:\nwant:\n%q\ngot:\n%q", want, got)
	}
	// Named on its own, because "the whole file matches" already fails for a
	// file that kept CRLF everywhere: what this pins is that no line ENDING
	// changed, and a bare-LF repair of a CRLF file would look identical to a
	// correct one in every other respect.
	if i := strings.IndexByte(string(got), '\n'); i > 0 && got[i-1] != '\r' {
		t.Errorf("the first line lost its CR, so the repair mixed endings:\n%q", got)
	}
	if n := bytes.Count(got, []byte("\n")); n != bytes.Count(got, []byte("\r\n")) {
		t.Errorf("%d newlines but %d CRLFs, so the repair mixed endings:\n%q", n, bytes.Count(got, []byte("\r\n")), got)
	}
}

// TestRunCodex_TOMLRepairNormalisesAMixedEndingFile pins what the repair does to
// a config.toml that already carries both endings: the whole file becomes CRLF,
// because the join can only be one ending. Pinned because the alternative
// reading — a mixed file keeps its mix — is what the code was originally
// described as doing, and it is not what it does.
//
// The fixture is ONE CRLF line against SIX bare-LF ones, so it also pins WHICH
// rule: a majority rule picks LF for this fixture, and the test then fails
// because `want` is all-CRLF. The rule is presence, on the reasoning in
// installCodexMCP — a config carrying any CRLF was written by something on
// Windows, and CRLF is the ending that file wants back.
func TestRunCodex_TOMLRepairNormalisesAMixedEndingFile(t *testing.T) {
	home, _ := setupCodexTestEnv(t)
	ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")

	// One CRLF, six LF.
	seed := "[mcp_servers.ghost]\r\n" +
		"command = '/old/install/ghost'\n" +
		"args = [\"mcp\", \"--stale\"]\n" +
		"\n" +
		"[mcp_servers.other]\n" +
		"command = \"/usr/bin/other\"\n" +
		"args = [\"serve\"]\n"
	want := codexMCPServerComment + "\r\n" +
		"[mcp_servers.ghost]\r\n" +
		"command = " + codexTOMLString(ghostBin) + "\r\n" +
		"args = [\"mcp\"]\r\n" +
		"\r\n" +
		"[mcp_servers.other]\r\n" +
		"command = \"/usr/bin/other\"\r\n" +
		"args = [\"serve\"]\r\n"

	if err := os.MkdirAll(filepath.Dir(codexConfigToml(home)), 0755); err != nil {
		t.Fatal(err)
	}
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
	if string(got) != want {
		t.Errorf("repaired config.toml mismatch:\nwant:\n%q\ngot:\n%q", want, got)
	}
	if n, crlf := bytes.Count(got, []byte("\n")), bytes.Count(got, []byte("\r\n")); n != crlf {
		t.Errorf("%d newlines but %d CRLFs, so the file is still mixed:\n%q", n, crlf, got)
	}
}

// TestRunCodex_TOMLAppendBacksUpThePreGhostFile pins the path that makes the
// .bak worth having. The first init APPENDS a ghost table, so on that run the
// file on disk is the user's config.toml before Ghost ever touched it — and if
// the backup were taken only on a later repair, every .bak would already
// contain a ghost table (a repair can only find one) and the pre-ghost copy
// would never exist at all. The commonest damage, a first merge that went
// wrong, is what would then have nothing to restore.
func TestRunCodex_TOMLAppendBacksUpThePreGhostFile(t *testing.T) {
	home, _ := setupCodexTestEnv(t)
	ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")
	path := codexConfigToml(home)

	seed := "# the user's own file\n[unrelated]\nmodel = \"gpt-5\"\n"
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := RunCodex(&out, false); err != nil {
		t.Fatalf("RunCodex: %v", err)
	}
	// The whole file, exactly: the seed with the managed block appended after a
	// blank line. That is what establishes the append path — the block is in the
	// output and nothing else was — and it is asserted as bytes because a looser
	// check would also pass on the repair path, which is the path this test is
	// not about. The .bak assertion below is the load-bearing half; this one
	// proves which path it ran on.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := seed + "\n" + renderCodexMCPServerBlock(ghostBin); string(got) != want {
		t.Fatalf("appended config.toml mismatch:\nwant:\n%q\ngot:\n%q", want, got)
	}

	bak, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("the first init left no .bak beside the user's config.toml: %v", err)
	}
	if string(bak) != seed {
		t.Errorf(".bak = %q, want the pre-ghost file %q", bak, seed)
	}
	if strings.Contains(string(bak), "mcp_servers") {
		t.Error(".bak already mentions the ghost server, so it is not the pre-ghost file")
	}
}

// TestRunCodex_TOMLDoesNotBackUpAnEmptyConfig: a 0-byte config.toml is the one
// case where a .bak has nothing worth keeping, and because the backup is written
// O_EXCL and never rolled forward, a 0-byte one would stand forever and suppress
// the real pre-ghost copy a later repair of a file the user has since filled in
// would otherwise take. Restoring that would wipe their keys, which is worse than
// never having written it. A file holding only comments is still backed up — see
// the guard in installCodexMCP — because its bytes are the user's either way.
func TestRunCodex_TOMLDoesNotBackUpAnEmptyConfig(t *testing.T) {
	home, _ := setupCodexTestEnv(t)
	path := codexConfigToml(home)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := RunCodex(&out, false); err != nil {
		t.Fatalf("RunCodex: %v", err)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Errorf("an empty config.toml produced a .bak (Stat err = %v), which would block every later backup", err)
	}
	if b, err := os.ReadFile(path); err != nil || !strings.Contains(string(b), "[mcp_servers.ghost]") {
		t.Errorf("the registration did not happen (err %v):\n%q", err, b)
	}
}

// TestRunCodex_TOMLDoesNotBackUpItsOwnOutput pins the other skip, beside the
// empty-file one: a config.toml holding nothing but the block ghost manages is
// ghost's own output, so a repair of it takes no .bak. The shape that reaches
// the repair path is the block an earlier init wrote with a command a later
// upgrade moved — the stale-path repair — and the .bak slot is the user's
// pre-ghost file, which ghost's own bytes must never fill, because the backup
// is never rolled forward and the user's file would then have nowhere to go.
// A ghost table carrying anything ghost did not write — different args, a
// sub-table, another table — keeps its .bak, pinned by the tests around this
// one.
func TestRunCodex_TOMLDoesNotBackUpItsOwnOutput(t *testing.T) {
	home, _ := setupCodexTestEnv(t)
	ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")
	path := codexConfigToml(home)

	seed := codexMCPServerComment + "\n" +
		"[mcp_servers.ghost]\n" +
		"command = '/old/install/ghost'\n" +
		"args = [\"mcp\"]\n"
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := RunCodex(&out, false); err != nil {
		t.Fatalf("RunCodex: %v", err)
	}
	// The repair ran: the stale command is replaced with this run's binary.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), codexTOMLString(ghostBin)) {
		t.Fatalf("the stale-path repair did not run:\n%s", got)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		bak, _ := os.ReadFile(path + ".bak")
		t.Errorf("a repair of ghost's own output left a .bak:\n%q", bak)
	}
}

// TestRunCodex_TOMLRepairBacksUpOnce pins that a repair of a user's
// config.toml keeps their pre-ghost file beside it, the same way a save of
// settings.json does. The repair rewrites lines inside the user's own file, and
// the one copy of it that predates ghost is the only way back if the rewrite is
// wrong about what the file said.
func TestRunCodex_TOMLRepairBacksUpOnce(t *testing.T) {
	home, _ := setupCodexTestEnv(t)
	ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")
	path := codexConfigToml(home)

	seed := "[mcp_servers.ghost]\n" +
		"command = '/old/install/ghost'\n" +
		"args = [\"mcp\", \"--stale\"]\n"
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := RunCodex(&out, false); err != nil {
		t.Fatalf("RunCodex: %v", err)
	}
	bak, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("the repair left no .bak beside the user's config.toml: %v", err)
	}
	if string(bak) != seed {
		t.Errorf(".bak = %q, want the pre-ghost file %q", bak, seed)
	}
	if !strings.Contains(string(bak), "'/old/install/ghost'") {
		t.Error(".bak is not the file as it was before the repair, so it cannot be a way back")
	}
	_ = ghostBin

	// A second run must not roll the backup forward over the first one: the
	// only pristine copy is the pre-ghost one, and ghost's own output is not a
	// way back to it.
	if err := RunCodex(&out, false); err != nil {
		t.Fatalf("second RunCodex: %v", err)
	}
	again, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("read .bak after the second run: %v", err)
	}
	if string(again) != seed {
		t.Errorf(".bak was rolled forward to ghost's own output:\n%q", again)
	}
}

// TestRunCodex_TOMLRepairSurvivesMalformedValue pins what happens when a value
// never closes. A stray unterminated value must not make the repair swallow the
// rest of config.toml, delete a different MCP server's keys, or hide the ghost
// table so init appends a second copy of it.
func TestRunCodex_TOMLRepairSurvivesMalformedValue(t *testing.T) {
	// keep is a line below the ghost table that must survive the repair.
	cases := map[string]struct{ seed, want, keep string }{
		"unterminated value inside the ghost table": {
			// The unterminated extra swallows [mcp_servers.other] when a header
			// is not treated as closing an open value.
			seed: "[mcp_servers.ghost]\n" +
				"command = '/old/ghost'\n" +
				"args = [\"mcp\"]\n" +
				"extra = [1, 2\n" +
				"\n" +
				"[mcp_servers.other]\n" +
				"command = \"/usr/bin/other\"\n" +
				"args = [\"serve\"]\n",
			keep: `command = "/usr/bin/other"`,
			want: codexMCPServerComment + "\n" +
				"[mcp_servers.ghost]\n" +
				"command = {BIN}\n" +
				"args = [\"mcp\"]\n" +
				"extra = [1, 2\n" +
				"\n" +
				"[mcp_servers.other]\n" +
				"command = \"/usr/bin/other\"\n" +
				"args = [\"serve\"]\n",
		},
		"array-of-tables header below a malformed value": {
			keep: `args = ["serve"]`,
			// [[...]] can never be an array element, so it ends the malformed
			// value. Swallowed as value content, the list entry's own args line
			// lands inside the ghost span and is dropped as a duplicate key.
			seed: "[mcp_servers.ghost]\n" +
				"command = '/old/ghost'\n" +
				"args = [\"mcp\"]\n" +
				"extra = [1, 2\n" +
				"\n" +
				"[[mcp_servers.list]]\n" +
				"args = [\"serve\"]\n",
			want: codexMCPServerComment + "\n" +
				"[mcp_servers.ghost]\n" +
				"command = {BIN}\n" +
				"args = [\"mcp\"]\n" +
				"extra = [1, 2\n" +
				"\n" +
				"[[mcp_servers.list]]\n" +
				"args = [\"serve\"]\n",
		},
		"quoted foreign header below a malformed value": {
			keep: `command = "/usr/bin/other"`,
			// ["mcp_servers.other"] is a literal table name, so it ends the
			// malformed value. Read as value content it would leave the other
			// server's command and args inside the ghost span, where the
			// repair drops them as duplicate keys.
			seed: "[mcp_servers.ghost]\n" +
				"command = '/old/ghost'\n" +
				"args = [\"mcp\"]\n" +
				"extra = [1, 2\n" +
				"\n" +
				"[\"mcp_servers.other\"]\n" +
				"command = \"/usr/bin/other\"\n" +
				"args = [\"serve\"]\n",
			want: codexMCPServerComment + "\n" +
				"[mcp_servers.ghost]\n" +
				"command = {BIN}\n" +
				"args = [\"mcp\"]\n" +
				"extra = [1, 2\n" +
				"\n" +
				"[\"mcp_servers.other\"]\n" +
				"command = \"/usr/bin/other\"\n" +
				"args = [\"serve\"]\n",
		},
		"unterminated value above the ghost table": {
			keep: `command = "/usr/bin/other"`,
			// The ghost table is still found and repaired in place, so init
			// does not append a duplicate definition of a table already there.
			seed: "[profiles.dev]\n" +
				"extra = [1, 2\n" +
				"\n" +
				"[mcp_servers.ghost]\n" +
				"command = '/old/ghost'\n" +
				"args = [\"mcp\"]\n" +
				"\n" +
				"[mcp_servers.other]\n" +
				"command = \"/usr/bin/other\"\n" +
				"args = [\"serve\"]\n",
			want: "[profiles.dev]\n" +
				"extra = [1, 2\n" +
				"\n" +
				codexMCPServerComment + "\n" +
				"[mcp_servers.ghost]\n" +
				"command = {BIN}\n" +
				"args = [\"mcp\"]\n" +
				"\n" +
				"[mcp_servers.other]\n" +
				"command = \"/usr/bin/other\"\n" +
				"args = [\"serve\"]\n",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			home, _ := setupCodexTestEnv(t)
			ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")
			// Each want spells out where the managed marker lands, which is
			// directly above the ghost table header.
			want := strings.ReplaceAll(tc.want, "{BIN}", codexTOMLString(ghostBin))

			if err := os.MkdirAll(filepath.Dir(codexConfigToml(home)), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(codexConfigToml(home), []byte(tc.seed), 0644); err != nil {
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
			if string(got) != want {
				t.Errorf("repaired config.toml mismatch:\nwant:\n%q\ngot:\n%q", want, got)
			}
			if n := countCodexGhostHeaders(string(got)); n != 1 {
				t.Errorf("the ghost table must appear exactly once, got %d:\n%s", n, got)
			}
			if tc.keep != "" && !strings.Contains(string(got), tc.keep) {
				t.Errorf("a line below the ghost table was lost (%s):\n%s", tc.keep, got)
			}
		})
	}
}

// TestRunCodex_TOMLValueLineMentioningGhostIsNotAHeader pins that a bracketed
// line which is value content, not a broken table header, does not make the
// initializer refuse a perfectly valid config. The refusal exists to stop a
// duplicate [mcp_servers.ghost] table, not to object to the word appearing in a
// value.
func TestRunCodex_TOMLValueLineMentioningGhostIsNotAHeader(t *testing.T) {
	home, _ := setupCodexTestEnv(t)
	ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")

	seed := codexMCPServerComment + "\n" +
		"[mcp_servers.ghost]\n" +
		"command = " + codexTOMLString(ghostBin) + "\n" +
		"args = [\"mcp\"]\n" +
		"labels = [\n" +
		"  [\"ghost\"],\n" +
		"  [\"mcp_servers.ghost\", \"alias\"],\n" +
		"]\n"
	if err := os.MkdirAll(filepath.Dir(codexConfigToml(home)), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexConfigToml(home), []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := RunCodex(&out, false); err != nil {
		t.Fatalf("RunCodex: %v", err)
	}
	if strings.Contains(out.String(), "left unchanged") {
		t.Errorf("value content naming ghost must not be refused, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "✓ ghost MCP server already registered") {
		t.Errorf("the current registration should be left alone, got:\n%s", out.String())
	}
	got, err := os.ReadFile(codexConfigToml(home))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != seed {
		t.Errorf("config.toml must be untouched, got:\n%s", got)
	}
}

// mustCodexGhostSpan returns the ghost table's own lines in a config.toml body.
func mustCodexGhostSpan(t *testing.T, content string) []string {
	t.Helper()
	lines := strings.Split(content, "\n")
	start, end, ok := findCodexTOMLTable(lines, codexMCPServerKey)
	if !ok {
		t.Fatalf("no %s table in:\n%s", codexMCPServerKey, content)
	}
	return lines[start:end]
}

// TestRunCodex_TOMLDottedGhostEntryLeftAlone covers the form the line-wise
// scanner cannot see: the ghost server defined by a dotted or inline key. The
// textual merge must refuse to touch such a file — appending a second
// [mcp_servers.ghost] table next to the inline one would be a duplicate-key
// error for codex and a silent corruption of the user's config.
func TestRunCodex_TOMLDottedGhostEntryLeftAlone(t *testing.T) {
	cases := map[string]string{
		"inline table at top level": `mcp_servers.ghost = { command = "/bin/ghost", args = ["mcp"] }`,
		"inline table under [mcp_servers]": "[mcp_servers]\n" +
			"ghost = { command = \"/bin/ghost\", args = [\"mcp\"] }",
		"dotted key into the server": `mcp_servers.ghost.command = "/bin/ghost"`,
		"quoted first key part":      `"mcp_servers".ghost = { command = "/bin/ghost", args = ["mcp"] }`,
		"spaced dot separator":       `mcp_servers . ghost = { command = "/bin/ghost", args = ["mcp"] }`,
		"quoted parts and spaces":    ` "mcp_servers" . ghost = { command = "/bin/ghost" }`,
		"inline mcp_servers table":   `mcp_servers = { ghost = { command = "/bin/ghost", args = ["mcp"] } }`,
		"quoted part under mcp_servers": "[mcp_servers]\n" +
			"\"ghost\" = { command = \"/bin/ghost\", args = [\"mcp\"] }",
		"dotted key with a literal part": "[mcp_servers]\n" +
			"ghost.\"a.b\" = 1",
		"dotted key with a literal part at the top level": `"mcp_servers".ghost."a.b" = 1`,
		"multi-line inline mcp_servers table": "mcp_servers = {\n" +
			"  ghost = { command = \"/bin/ghost\", args = [\"mcp\"] },\n" +
			"}",
		"multi-line inline server table": "mcp_servers.ghost = {\n" +
			"  command = \"/bin/ghost\",\n" +
			"  args = [\"mcp\"],\n" +
			"}",
	}
	for name, ghostLine := range cases {
		t.Run(name, func(t *testing.T) {
			home, _ := setupCodexTestEnv(t)

			seed := "# my model settings\n" +
				"model = \"gpt-5.6\"\n" +
				"\n" +
				ghostLine + "\n" +
				"\n" +
				"[profiles.ci]\n" +
				"model = \"gpt-5-mini\"\n"
			if err := os.MkdirAll(filepath.Dir(codexConfigToml(home)), 0755); err != nil {
				t.Fatal(err)
			}
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
			if string(got) != seed {
				t.Errorf("config.toml must be left byte-for-byte unchanged, got:\n%s", got)
			}
			if strings.Contains(string(got), "["+codexMCPServerKey+"]") {
				t.Error("init must not append a duplicate [mcp_servers.ghost] table")
			}
			output := out.String()
			if !strings.Contains(output, "dotted or inline") {
				t.Errorf("expected a warning about the dotted/inline form, got:\n%s", output)
			}
			if !strings.Contains(output, "left unchanged") {
				t.Errorf("warning should say the file was left unchanged, got:\n%s", output)
			}

			// Status must name the real problem instead of claiming the
			// server is missing from a file that does define it.
			var status bytes.Buffer
			if _, err := StatusCodex(&status); err != nil {
				t.Fatalf("StatusCodex: %v", err)
			}
			if !strings.Contains(status.String(), "dotted or inline") {
				t.Errorf("status should report the dotted/inline form, got:\n%s", status.String())
			}
			if strings.Contains(status.String(), "missing from config.toml") {
				t.Errorf("status must not report a present server as missing, got:\n%s", status.String())
			}
		})
	}
}

// TestRunCodex_TOMLDottedGhostEntryDryRunWarns verifies the refusal is also
// reported, without writing, when the user previews the change.
func TestRunCodex_TOMLDottedGhostEntryDryRunWarns(t *testing.T) {
	home, _ := setupCodexTestEnv(t)
	seed := "mcp_servers.ghost = { command = \"/bin/ghost\", args = [\"mcp\"] }\n"
	if err := os.MkdirAll(filepath.Dir(codexConfigToml(home)), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexConfigToml(home), []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := RunCodex(&out, true); err != nil {
		t.Fatalf("RunCodex dry run: %v", err)
	}
	if !strings.Contains(out.String(), "dotted or inline") {
		t.Errorf("dry run should warn about the dotted/inline form, got:\n%s", out.String())
	}
	got, err := os.ReadFile(codexConfigToml(home))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != seed {
		t.Errorf("dry run must not modify config.toml, got:\n%s", got)
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

// TestRunCodex_HooksMergePreservesUserTextAndKeyOrder verifies that merging
// preserves user text with &&, <, > and non-alphabetical key order, and that
// a second run is a byte-for-byte no-op.
func TestRunCodex_HooksMergePreservesUserTextAndKeyOrder(t *testing.T) {
	home, _ := setupCodexTestEnv(t)

	hooksPath := codexHooksJSON(home)
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0755); err != nil {
		t.Fatal(err)
	}
	// Seed with user content containing &&, <, > and keys in non-alphabetical order:
	// Zeta, hooks, Alpha at top level; within hooks: Stop before SessionStart
	seed := "{\"Zeta\":1,\"hooks\":{\"Stop\":[{\"hooks\":[{\"type\":\"command\",\"command\":\"echo a && cat <x >y\"}]}],\"SessionStart\":[{\"hooks\":[{\"type\":\"command\",\"command\":\"mytool --check <input && echo ok\"}]}]},\"Alpha\":2}"
	if err := os.WriteFile(hooksPath, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	// First run
	var out bytes.Buffer
	if err := RunCodex(&out, false); err != nil {
		t.Fatalf("RunCodex first: %v", err)
	}

	data, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	first := string(data)
	t.Logf("first run output:\n%s", first)

	// Verify user content preserved exactly
	if !strings.Contains(first, "echo a && cat <x >y") {
		t.Errorf("user Stop command escaped/lost")
	}
	if !strings.Contains(first, "mytool --check <input && echo ok") {
		t.Errorf("user SessionStart command escaped/lost")
	}
	// Verify top-level key order: Zeta, hooks, Alpha (plus description appended)
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(first), &doc); err != nil {
		t.Fatal(err)
	}
	// Use a simple scanner to find top-level key positions
	findTopLevelKey := func(key string, start int) int {
		target := "\"" + key + "\""
		for i := start; i < len(first); i++ {
			if strings.HasPrefix(first[i:], target) {
				// Check if preceded by { or , (skipping whitespace)
				j := i - 1
				for j >= 0 && (first[j] == ' ' || first[j] == '\n' || first[j] == '\t' || first[j] == '\r') {
					j--
				}
				if j >= 0 && (first[j] == '{' || first[j] == ',') {
					return i
				}
			}
		}
		return -1
	}
	iZeta := findTopLevelKey("Zeta", 0)
	iHooks := findTopLevelKey("hooks", iZeta+1)
	iAlpha := findTopLevelKey("Alpha", iHooks+1)
	t.Logf("iZeta=%d, iHooks=%d, iAlpha=%d", iZeta, iHooks, iAlpha)
	if iZeta < 0 || iHooks < 0 || iAlpha < 0 || iZeta >= iHooks || iHooks >= iAlpha {
		t.Errorf("top-level key order changed: want Zeta < hooks < Alpha (iZeta=%d, iHooks=%d, iAlpha=%d)", iZeta, iHooks, iAlpha)
	}
	// Verify hooks event order: Stop then SessionStart (check by byte position in raw text)
	hooksRaw := doc["hooks"]
	// Find positions of "Stop" and "SessionStart" at the top level of hooks object
	hooksStr := string(hooksRaw)
	stopPos := strings.Index(hooksStr, "Stop")
	sessionStartPos := strings.Index(hooksStr, "SessionStart")
	if stopPos < 0 || sessionStartPos < 0 || stopPos >= sessionStartPos {
		t.Errorf("hooks event order changed: want Stop before SessionStart (stopPos=%d, sessionStartPos=%d)", stopPos, sessionStartPos)
	}
	// Second run - must be byte-for-byte identical
	var secondOut bytes.Buffer
	if err := RunCodex(&secondOut, false); err != nil {
		t.Fatalf("RunCodex second: %v", err)
	}
	data2, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	second := string(data2)
	if first != second {
		t.Errorf("second run changed bytes:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// TestRunCodex_FreshInstallWithSpecialPathIsAFixedPoint verifies that a fresh
// codex hooks.json install and the run after it write the same bytes, even
// when the resolved ghost binary path holds a character the JSON encoder
// escapes. The fresh install renders the document with HTML escaping off, so
// the merge path must render ghost's own rule the same way — otherwise the
// second run rewrites the file the first one wrote, and the install never
// reports the hooks as already wired.
func TestRunCodex_FreshInstallWithSpecialPathIsAFixedPoint(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "")
	t.Setenv("GHOST_EMBEDDING_ENABLED", "false")

	// A binary directory whose name holds an escapable character, so the
	// command ghost writes carries it.
	binDir := filepath.Join(home, "b&in")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeStub(t, binDir, "ghost")
	t.Setenv("PATH", binDir)

	var out bytes.Buffer
	if err := RunCodex(&out, false); err != nil {
		t.Fatalf("RunCodex (first): %v", err)
	}

	first, err := os.ReadFile(codexHooksJSON(home))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), "b&in") {
		t.Errorf("the installed command must carry the path unescaped, got:\n%s", first)
	}

	out.Reset()
	if err := RunCodex(&out, false); err != nil {
		t.Fatalf("RunCodex (second): %v", err)
	}

	second, err := os.ReadFile(codexHooksJSON(home))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("a second install must not rewrite the file:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if !strings.Contains(out.String(), "hooks already wired") {
		t.Errorf("the second install must report the hooks as already wired, got:\n%s", out.String())
	}
}

// codexMergedRule is the parsed view of one rule in a merged hooks.json.
type codexMergedRule struct {
	Matcher string           `json:"matcher"`
	Hooks   []map[string]any `json:"hooks"`
}

// codexMergedEvent parses one event's rule list out of a merged document.
func codexMergedEvent(t *testing.T, doc []byte, event string) []codexMergedRule {
	t.Helper()
	var parsed struct {
		Hooks map[string][]codexMergedRule `json:"hooks"`
	}
	if err := json.Unmarshal(doc, &parsed); err != nil {
		t.Fatalf("merged hooks.json invalid JSON: %v", err)
	}
	return parsed.Hooks[event]
}

// codexRuleCommands returns the commands of one rule's actions in order.
func codexRuleCommands(rule codexMergedRule) []string {
	cmds := make([]string, 0, len(rule.Hooks))
	for _, action := range rule.Hooks {
		cmd, _ := action["command"].(string)
		cmds = append(cmds, cmd)
	}
	return cmds
}

// TestAuditCodexMixedRuleKeepsUserAction is #1217's reproduction: the user
// wired ghost beside their own command inside one SessionStart rule. Ghost
// owns the action it rendered, so a stale path is repaired in place — the
// user's action and the rule's matcher are not ghost's to delete, and a fresh
// run must not print that it touched the file without saying what was lost.
func TestAuditCodexMixedRuleKeepsUserAction(t *testing.T) {
	const userAction = "/usr/local/bin/notify-start"
	seed := []byte(`{
  "hooks": {
    "SessionStart": [
      {"matcher":"startup","hooks":[
        {"type":"command","command":` + jsonString(shellQuote("/old/ghost")+" hook session-start --source codex") + `},
        {"type":"command","command":` + jsonString(userAction) + `}
      ]}
    ]
  }
}`)

	got, err := mergeCodexHooksConfig(seed, "/new/ghost")
	if err != nil {
		t.Fatalf("mergeCodexHooksConfig: %v", err)
	}
	rules := codexMergedEvent(t, []byte(got), "SessionStart")
	if len(rules) != 1 {
		t.Fatalf("the user's rule must survive, got %d rules:\n%s", len(rules), got)
	}
	if rules[0].Matcher != "startup" {
		t.Errorf("the rule's matcher must survive, got %q", rules[0].Matcher)
	}
	cmds := codexRuleCommands(rules[0])
	if len(cmds) != 2 {
		t.Fatalf("the user's action must survive beside ghost's, got %v", cmds)
	}
	if want := shellQuote("/new/ghost") + " hook session-start --source codex"; cmds[0] != want {
		t.Errorf("ghost's action should be repaired to the resolved path, got %q, want %q", cmds[0], want)
	}
	if cmds[1] != userAction {
		t.Errorf("user action %s was deleted", userAction)
	}

	// Ghost still fires once: no second rule of its own was appended.
	for _, event := range []string{"Stop", "SessionEnd"} {
		other := codexMergedEvent(t, []byte(got), event)
		if len(other) != 1 || len(other[0].Hooks) != 1 {
			t.Errorf("%s should carry exactly ghost's rule, got %+v", event, other)
		}
	}
}

// TestAuditCodexMixedRuleCurrentPath is the same rule with ghost's action
// already at the resolved path: nothing needed repairing, so the merge owes
// the user's action exactly what it owed it above.
func TestAuditCodexMixedRuleCurrentPath(t *testing.T) {
	const userAction = "/usr/local/bin/notify-start"
	current := shellQuote("/new/ghost") + " hook session-start --source codex"
	seed := []byte(`{
  "hooks": {
    "SessionStart": [
      {"matcher":"startup","hooks":[
        {"type":"command","command":` + jsonString(current) + `},
        {"type":"command","command":` + jsonString(userAction) + `}
      ]}
    ]
  }
}`)

	got, err := mergeCodexHooksConfig(seed, "/new/ghost")
	if err != nil {
		t.Fatalf("mergeCodexHooksConfig: %v", err)
	}
	rules := codexMergedEvent(t, []byte(got), "SessionStart")
	if len(rules) != 1 {
		t.Fatalf("the user's rule must survive, got %d rules:\n%s", len(rules), got)
	}
	cmds := codexRuleCommands(rules[0])
	if len(cmds) != 2 || cmds[0] != current || cmds[1] != userAction {
		t.Errorf("user action %s was deleted even though the ghost action is already current, got %v", userAction, cmds)
	}
	if rules[0].Matcher != "startup" {
		t.Errorf("the rule's matcher must survive, got %q", rules[0].Matcher)
	}

	again, err := mergeCodexHooksConfig([]byte(got), "/new/ghost")
	if err != nil {
		t.Fatalf("mergeCodexHooksConfig (second pass): %v", err)
	}
	if again != got {
		t.Errorf("merging the merged document again must be a byte-level no-op:\nfirst:\n%s\nsecond:\n%s", got, again)
	}
}

// TestMergeCodexHooksLeavesAGhostCommandGhostDidNotRender pins the other half:
// a rule running some other ghost subcommand is the user's, is preserved
// verbatim, and does not stop the event from being wired.
func TestMergeCodexHooksLeavesAGhostCommandGhostDidNotRender(t *testing.T) {
	const userAction = "'/usr/local/bin/ghost' mcp status"
	seed := []byte(`{
  "hooks": {
    "SessionStart": [
      {"hooks": [{"type": "command", "command": "` + userAction + `"}]},
      {"hooks": [{"type": "command", "command": "/usr/local/bin/say-start"}]}
    ]
  }
}`)

	got, err := mergeCodexHooksConfig(seed, "/new/ghost")
	if err != nil {
		t.Fatalf("mergeCodexHooksConfig: %v", err)
	}
	rules := codexMergedEvent(t, []byte(got), "SessionStart")
	if len(rules) != 3 {
		t.Fatalf("both user rules should survive beside ghost's, got %d:\n%s", len(rules), got)
	}
	if cmds := codexRuleCommands(rules[0]); len(cmds) != 1 || cmds[0] != userAction {
		t.Errorf("a ghost command ghost did not render must be left alone, got %v", cmds)
	}
	if cmds := codexRuleCommands(rules[1]); len(cmds) != 1 || cmds[0] != "/usr/local/bin/say-start" {
		t.Errorf("the user's own rule must come through untouched, got %v", cmds)
	}
	if want := shellQuote("/new/ghost") + " hook session-start --source codex"; codexRuleCommands(rules[2])[0] != want {
		t.Errorf("ghost's own rule should be appended, got %+v", rules[2])
	}
}

// TestMergeCodexHooksFiresGhostOncePerEvent pins the invariant that makes
// rewriting in place safe: ghost owns ONE action per event, so a second copy
// — our own rule beside a rule the user extended — is pruned rather than left
// to fire twice.
func TestMergeCodexHooksFiresGhostOncePerEvent(t *testing.T) {
	const userAction = "/usr/local/bin/notify-start"
	seed := []byte(`{
  "hooks": {
    "SessionStart": [
      {"hooks": [{"type": "command", "command": ` + jsonString(shellQuote("/new/ghost")+" hook session-start --source codex") + `}]},
      {"matcher":"startup","hooks":[
        {"type":"command","command":` + jsonString(shellQuote("/old/ghost")+" hook session-start --source codex") + `},
        {"type":"command","command":` + jsonString(userAction) + `}
      ]}
    ]
  }
}`)

	got, err := mergeCodexHooksConfig(seed, "/new/ghost")
	if err != nil {
		t.Fatalf("mergeCodexHooksConfig: %v", err)
	}
	rules := codexMergedEvent(t, []byte(got), "SessionStart")
	if len(rules) != 2 {
		t.Fatalf("both rules should survive, got %d:\n%s", len(rules), got)
	}
	ghost, seen := 0, 0
	for _, rule := range rules {
		if rule.Matcher == "startup" {
			cmds := codexRuleCommands(rule)
			if len(cmds) != 1 || cmds[0] != userAction {
				t.Errorf("the user's rule must keep its matcher and its own action, got %+v", rule)
			}
			continue
		}
		for _, cmd := range codexRuleCommands(rule) {
			if strings.Contains(cmd, "hook session-start --source codex") {
				ghost++
				continue
			}
			seen++
		}
	}
	if ghost != 1 {
		t.Errorf("ghost must fire exactly once per event, got %d actions", ghost)
	}
	if seen != 0 {
		t.Errorf("no user action outside the user's own rule should be carried, got %d", seen)
	}
}

// TestMergeCodexHooksRecognizesAWindowsBatchShim pins the launcher form the
// other tests never reach: on Windows a ghost command can resolve to a
// "ghost.bat"/"ghost.cmd" shim rather than the native ".exe", and the merge
// must still treat that action as ghost's own. Missing it makes every re-run
// append a second rule, so hooks.json never reaches a fixed point — the
// failure the idempotence test sees only on a Windows runner.
func TestMergeCodexHooksRecognizesAWindowsBatchShim(t *testing.T) {
	const userAction = "C:\\Tools\\notify.exe"
	shim := "\"C:\\Tools\\ghost.bat\" hook session-start --source codex"
	seed := []byte(`{
  "hooks": {
    "SessionStart": [
      {"matcher":"startup","hooks":[
        {"type":"command","command":` + jsonString(shim) + `},
        {"type":"command","command":` + jsonString(userAction) + `}
      ]}
    ]
  }
}`)

	got, err := mergeCodexHooksConfig(seed, "/new/ghost")
	if err != nil {
		t.Fatalf("mergeCodexHooksConfig: %v", err)
	}
	rules := codexMergedEvent(t, []byte(got), "SessionStart")
	if len(rules) != 1 {
		t.Fatalf("a recognised shim must not gain a second rule, got %d:\n%s", len(rules), got)
	}
	cmds := codexRuleCommands(rules[0])
	if len(cmds) != 2 {
		t.Fatalf("the user's action must survive beside the repaired shim, got %v", cmds)
	}
	if want := shellQuote("/new/ghost") + " hook session-start --source codex"; cmds[0] != want {
		t.Errorf("the shim action should be repaired in place, got %q, want %q", cmds[0], want)
	}
	if cmds[1] != userAction {
		t.Errorf("user action %s was deleted", userAction)
	}
}

// TestMergeCodexHooksFixedPointForAnyBinaryName is #1249's regression: the
// merge recognised its own earlier output only when the baked binary's
// basename was "ghost", so any other name — /g, a renamed .exe, a differently
// named .bat/.cmd shim — was not recognised as existing, the install's
// byte-compare never saw existing == desired, and every pass appended another
// identical rule to each event. Recognition now compares against the path
// being rendered as well, so feeding the output back is a byte-level no-op
// for every accepted input and every path.
func TestMergeCodexHooksFixedPointForAnyBinaryName(t *testing.T) {
	bins := []string{"/g", "/opt/my tools/ghost-cli", `C:\Tools\ghostx.exe`, "/usr/local/bin/ghostx.bat"}
	for _, bin := range bins {
		t.Run(bin, func(t *testing.T) {
			rendered := renderCodexHooksConfig(bin)
			mixed := `{"hooks":{"SessionStart":[{"matcher":"startup","hooks":[` +
				`{"type":"command","command":` + jsonString(shellQuote("/usr/local/bin/ghost")+" hook session-start --source codex") + `},` +
				`{"type":"command","command":` + jsonString("/usr/local/bin/notify-start") +
				`}]}]}}`
			inputs := []string{`{}`, `{"hooks": {}}`, rendered, mixed}
			var firstMixed string
			for _, input := range inputs {
				first, err := mergeCodexHooksConfig([]byte(input), bin)
				if err != nil {
					t.Fatalf("mergeCodexHooksConfig(%s): %v", input, err)
				}
				second, err := mergeCodexHooksConfig([]byte(first), bin)
				if err != nil {
					t.Fatalf("mergeCodexHooksConfig (second pass): %v", err)
				}
				if second != first {
					t.Errorf("pass 2 diverged by %+d bytes:\nfirst:\n%s\nsecond:\n%s", len(second)-len(first), first, second)
				}
				third, err := mergeCodexHooksConfig([]byte(second), bin)
				if err != nil {
					t.Fatalf("mergeCodexHooksConfig (third pass): %v", err)
				}
				if third != first {
					t.Errorf("pass 3 diverged by %+d bytes:\nfirst:\n%s\nthird:\n%s", len(third)-len(first), first, third)
				}
				if input == mixed {
					firstMixed = first
				}

				// Ghost fires exactly once per event, at the path this run bakes.
				for _, event := range []struct{ key, token string }{
					{"SessionStart", "session-start"},
					{"Stop", "stop"},
					{"SessionEnd", "session-end"},
				} {
					want := shellQuote(bin) + " hook " + event.token + " --source codex"
					fired := 0
					for _, rule := range codexMergedEvent(t, []byte(first), event.key) {
						for _, cmd := range codexRuleCommands(rule) {
							if cmd == want {
								fired++
							}
						}
					}
					if fired != 1 {
						t.Errorf("%s: ghost must fire exactly once, got %d actions equal to %q:\n%s", event.key, fired, want, first)
					}
				}
			}

			// The mixed rule keeps what is the user's: its matcher, its own
			// action, and the stale ghost-named path repaired in place.
			rules := codexMergedEvent(t, []byte(firstMixed), "SessionStart")
			if len(rules) != 1 {
				t.Fatalf("the user's rule must survive, got %d rules:\n%s", len(rules), firstMixed)
			}
			if rules[0].Matcher != "startup" {
				t.Errorf("the rule's matcher must survive, got %q", rules[0].Matcher)
			}
			cmds := codexRuleCommands(rules[0])
			if len(cmds) != 2 {
				t.Fatalf("the user's action must survive beside ghost's, got %v", cmds)
			}
			if cmds[1] != "/usr/local/bin/notify-start" {
				t.Errorf("user action was rewritten, got %q", cmds[1])
			}
			if want := shellQuote(bin) + " hook session-start --source codex"; cmds[0] != want {
				t.Errorf("the stale ghost-named action should be repaired to the baked path, got %q, want %q", cmds[0], want)
			}
		})
	}
}

// TestMergeCodexHooksRepairsGhostsOwnActionFields pins the whole-action repair.
// Rewriting only ghost's command left a stale budget on the action itself:
// a hand-edited or pre-#1217 `"timeout":1` on the SessionEnd action is not the
// same thing as a current one, and SessionStart/Stop carry no timeout of their
// own, so a budget someone left on them survives every run. Ghost's action is
// therefore written as what ghost renders — command, type and timeout. Every
// other key of that action, and every user action beside it, is not ghost's to
// touch. It also pins that a ghost action is matched by its executable and its
// event alone, so a `--source` copied from another host's file is rewritten to
// this host's.
func TestMergeCodexHooksRepairsGhostsOwnActionFields(t *testing.T) {
	const userAction = "https://example.com/end"
	stalePath := shellQuote("/old/ghost") + " hook session-end --source codex"
	foreignSource := shellQuote("/old/ghost") + " hook stop --source claude"
	currentStart := shellQuote("/new/ghost") + " hook session-start --source codex"
	seed := []byte(`{
  "hooks": {
    "SessionEnd": [
      {"matcher":"end","hooks":[
        {"type":"command","command":` + jsonString(stalePath) + `,"timeout":1,"note":"mine"},
        {"type":"http","command":` + jsonString(userAction) + `,"timeout":9}
      ]}
    ],
    "Stop": [
      {"hooks": [{"type": "command", "command": ` + jsonString(foreignSource) + `}]}
    ],
    "SessionStart": [
      {"hooks": [
        {"type":"command","command":` + jsonString(currentStart) + `,"timeout":42,"async":true}
      ]}
    ]
  }
}`)

	got, err := mergeCodexHooksConfig(seed, "/new/ghost")
	if err != nil {
		t.Fatalf("mergeCodexHooksConfig: %v", err)
	}

	// SessionEnd: the stale path AND its stale budget are repaired, and the
	// action keeps the key the user added to it.
	endRules := codexMergedEvent(t, []byte(got), "SessionEnd")
	if len(endRules) != 1 || endRules[0].Matcher != "end" {
		t.Fatalf("the user's rule must survive with its matcher, got %+v", endRules)
	}
	actions := endRules[0].Hooks
	if len(actions) != 2 {
		t.Fatalf("the user's action must survive beside ghost's, got %+v", actions)
	}
	ghostAction := actions[0]
	if want := shellQuote("/new/ghost") + " hook session-end --source codex"; ghostAction["command"] != want {
		t.Errorf("ghost action command = %v, want %q", ghostAction["command"], want)
	}
	if ghostAction["type"] != "command" {
		t.Errorf("ghost action type = %v, want %q", ghostAction["type"], "command")
	}
	if timeout, _ := ghostAction["timeout"].(float64); timeout != float64(codexSessionEndTimeoutSec) {
		t.Errorf("ghost action timeout = %v, want %d", ghostAction["timeout"], codexSessionEndTimeoutSec)
	}
	if ghostAction["note"] != "mine" {
		t.Errorf("an unknown key on ghost's own action is not ghost's to delete, got %v", ghostAction["note"])
	}
	user := actions[1]
	if user["type"] != "http" || user["command"] != userAction {
		t.Errorf("a user action must survive untouched, got %+v", user)
	}
	if timeout, _ := user["timeout"].(float64); timeout != 9 {
		t.Errorf("a user action's own timeout was rewritten, got %v", user["timeout"])
	}

	// Stop: matched on the executable and the event alone, so another host's
	// --source is rewritten to this host's, and the budget it did not need goes.
	stop := codexMergedEvent(t, []byte(got), "Stop")
	if len(stop) != 1 || len(stop[0].Hooks) != 1 {
		t.Fatalf("Stop should carry exactly one action, got %+v", stop)
	}
	if want := shellQuote("/new/ghost") + " hook stop --source codex"; stop[0].Hooks[0]["command"] != want {
		t.Errorf("ghost action --source = %v, want %q", stop[0].Hooks[0]["command"], want)
	}
	if _, ok := stop[0].Hooks[0]["timeout"]; ok {
		t.Errorf("Stop carries no timeout of its own, so a stale one must go, got %+v", stop[0].Hooks[0])
	}

	// SessionStart: already at the current path, so only the fields are still
	// repaired — the budget some other event needs is not left on this one.
	start := codexMergedEvent(t, []byte(got), "SessionStart")
	if len(start) != 1 || len(start[0].Hooks) != 1 {
		t.Fatalf("SessionStart should carry exactly one action, got %+v", start)
	}
	startAction := start[0].Hooks[0]
	if startAction["command"] != currentStart {
		t.Errorf("a current ghost action command changed, got %v", startAction["command"])
	}
	if _, ok := startAction["timeout"]; ok {
		t.Errorf("SessionStart carries no timeout of its own, so a stale one must go, got %+v", startAction)
	}
	if startAction["async"] != true {
		t.Errorf("an unknown key on ghost's own action is not ghost's to delete, got %v", startAction["async"])
	}

	// A repair settles: merging the merged document again changes no byte.
	again, err := mergeCodexHooksConfig([]byte(got), "/new/ghost")
	if err != nil {
		t.Fatalf("mergeCodexHooksConfig (second pass): %v", err)
	}
	if again != got {
		t.Errorf("merging the merged document again must be a byte-level no-op:\nfirst:\n%s\nsecond:\n%s", got, again)
	}
}

// TestMergeCodexHooksTreatsAWrappedGhostActionAsItsOwn is #1345's core
// reproduction: a hand-edited wrapper that RUNS ghost's hook for the event —
// an assignment prefix, a `cd … &&` prefix, a quoted argument carrying the
// whole invocation — is ghost's OWN action under the same ownership rule the
// Claude installer applies (ghostHookTail), so no second rule is appended
// beside it and the wrapper is left byte-identical. Codex runs every matching
// hook, so a duplicate SessionStart rule fires ghost's context twice per
// session.
func TestMergeCodexHooksTreatsAWrappedGhostActionAsItsOwn(t *testing.T) {
	const ghostBin = "/new/ghost"
	for _, ev := range []struct{ key, token string }{
		{"SessionStart", "session-start"},
		{"Stop", "stop"},
		{"SessionEnd", "session-end"},
	} {
		for _, w := range []struct {
			label string
			wrap  func(cmd string) string
		}{
			{"assignment-prefix", func(cmd string) string { return "GHOST_X=1 " + cmd }},
			{"cd-and", func(cmd string) string { return "cd /somewhere && " + cmd }},
			{"sh-c", func(cmd string) string { return `/bin/sh -c "` + cmd + `"` }},
		} {
			t.Run(ev.key+"/"+w.label, func(t *testing.T) {
				if w.label == "sh-c" && runtime.GOOS == "windows" {
					t.Skip("POSIX-nested shell quoting; sh-c shape exercised on unix runners")
				}
				canonical := shellQuote(ghostBin) + " hook " + ev.token + " --source codex"
				wrapped := w.wrap(canonical)
				seed := []byte(`{
  "hooks": {
    "` + ev.key + `": [
      {"hooks": [{"type": "command", "command": ` + jsonString(wrapped) + `}]}
    ]
  }
}`)

				got, err := mergeCodexHooksConfig(seed, ghostBin)
				if err != nil {
					t.Fatalf("mergeCodexHooksConfig: %v", err)
				}
				rules := codexMergedEvent(t, []byte(got), ev.key)
				if len(rules) != 1 {
					t.Fatalf("a wrapped ghost action must not gain a second rule, got %d rules:\n%s", len(rules), got)
				}
				cmds := codexRuleCommands(rules[0])
				if len(cmds) != 1 || cmds[0] != wrapped {
					t.Errorf("the wrapped action must be left byte-identical, got %v", cmds)
				}

				// Fixed point: merging the merged document changes no byte.
				again, err := mergeCodexHooksConfig([]byte(got), ghostBin)
				if err != nil {
					t.Fatalf("mergeCodexHooksConfig (second pass): %v", err)
				}
				if again != got {
					t.Errorf("pass 2 diverged by %+d bytes:\nfirst:\n%s\nsecond:\n%s", len(again)-len(got), got, again)
				}
			})
		}
	}
}

// TestMergeCodexHooksAddsItsRuleBesideAForeignCommand pins the other half of
// #1345: a wrapper that does NOT run ghost stays the user's and still gets
// ghost's rule beside it. A ghost binary named inside another tool's quoted
// ARGUMENT is not a command that runs, and neither is a ghost name the shell
// hands to echo — reading either as ghost's own is the #1326 false positive
// that left a foreign wrapper in place only by accident.
func TestMergeCodexHooksAddsItsRuleBesideAForeignCommand(t *testing.T) {
	const ghostBin = "/new/ghost"
	for _, ev := range []struct{ key, token string }{
		{"SessionStart", "session-start"},
		{"Stop", "stop"},
		{"SessionEnd", "session-end"},
	} {
		for _, foreign := range []struct{ label, command string }{
			{"foreign-tool", "/usr/local/bin/othertool hook " + ev.token + " --source codex"},
			{"argument-position", "echo /opt/bin/ghost hook " + ev.token + " --source codex"},
			{"quoted-argument", `/usr/local/bin/othertool --note "ghost hook ` + ev.token + ` --source codex"`},
			{"echoed-inside-sh-c", `/bin/sh -c "echo ghost hook ` + ev.token + ` --source codex"`},
		} {
			t.Run(ev.key+"/"+foreign.label, func(t *testing.T) {
				seed := []byte(`{
  "hooks": {
    "` + ev.key + `": [
      {"hooks": [{"type": "command", "command": ` + jsonString(foreign.command) + `}]}
    ]
  }
}`)

				got, err := mergeCodexHooksConfig(seed, ghostBin)
				if err != nil {
					t.Fatalf("mergeCodexHooksConfig: %v", err)
				}
				rules := codexMergedEvent(t, []byte(got), ev.key)
				if len(rules) != 2 {
					t.Fatalf("a foreign command must survive beside ghost's own rule, got %d rules:\n%s", len(rules), got)
				}
				if cmds := codexRuleCommands(rules[0]); len(cmds) != 1 || cmds[0] != foreign.command {
					t.Errorf("the foreign action must be carried across byte for byte, got %v", cmds)
				}
				want := shellQuote(ghostBin) + " hook " + ev.token + " --source codex"
				if cmds := codexRuleCommands(rules[1]); len(cmds) != 1 || cmds[0] != want {
					t.Errorf("ghost's own rule should be appended, got %v", cmds)
				}

				again, err := mergeCodexHooksConfig([]byte(got), ghostBin)
				if err != nil {
					t.Fatalf("mergeCodexHooksConfig (second pass): %v", err)
				}
				if again != got {
					t.Errorf("pass 2 diverged by %+d bytes:\nfirst:\n%s\nsecond:\n%s", len(again)-len(got), got, again)
				}
			})
		}
	}
}

// TestMergeCodexHooksNeverRewritesAWrappedAction pins the repair bound that
// makes recognising a wrapper safe: only a command whose LEADING token is a
// ghost binary is eligible for the in-place rewrite, so a wrapper around a
// ghost path this run would not render is kept byte-identical rather than
// pointed at the baked binary. The user's prefix and their choice of path are
// both theirs to keep.
func TestMergeCodexHooksNeverRewritesAWrappedAction(t *testing.T) {
	const ghostBin = "/new/ghost"
	stale := shellQuote("/usr/local/bin/ghost") + " hook stop --source codex"
	wrapped := "GHOST_X=1 " + stale
	seed := []byte(`{
  "hooks": {
    "Stop": [
      {"hooks": [{"type": "command", "command": ` + jsonString(wrapped) + `}]}
    ]
  }
}`)

	got, err := mergeCodexHooksConfig(seed, ghostBin)
	if err != nil {
		t.Fatalf("mergeCodexHooksConfig: %v", err)
	}
	rules := codexMergedEvent(t, []byte(got), "Stop")
	if len(rules) != 1 {
		t.Fatalf("the wrapped action should stand alone, got %d rules:\n%s", len(rules), got)
	}
	if cmds := codexRuleCommands(rules[0]); len(cmds) != 1 || cmds[0] != wrapped {
		t.Errorf("a wrapped action with a stale path must be left byte-identical, got %v", cmds)
	}

	again, err := mergeCodexHooksConfig([]byte(got), ghostBin)
	if err != nil {
		t.Fatalf("mergeCodexHooksConfig (second pass): %v", err)
	}
	if again != got {
		t.Errorf("pass 2 diverged by %+d bytes:\nfirst:\n%s\nsecond:\n%s", len(again)-len(got), got, again)
	}
}

// TestRunCodex_HooksWrappedGhostRuleIsNeverDuplicated is #1345's installer-level
// reproduction end to end: seed hooks.json with a hand-edited wrapper that runs
// ghost's SessionStart hook and run the installer. The wrapper is recognised as
// ghost's own, so the installer reports the hooks as already wired, appends
// no second rule, and leaves the file byte-identical — a duplicate
// SessionStart rule fires ghost's context twice per session.
func TestRunCodex_HooksWrappedGhostRuleIsNeverDuplicated(t *testing.T) {
	home, _ := setupCodexTestEnv(t)
	ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")

	if err := os.MkdirAll(filepath.Dir(codexHooksJSON(home)), 0755); err != nil {
		t.Fatal(err)
	}
	canonical := shellQuote(ghostBin) + " hook session-start --source codex"
	wrapped := "GHOST_X=1 " + canonical
	rendered := renderCodexHooksConfig(ghostBin)
	seed := strings.Replace(rendered, jsonString(canonical), jsonString(wrapped), 1)
	if seed == rendered {
		t.Fatal("the seeded hooks.json must differ from the canonical document")
	}
	if err := os.WriteFile(codexHooksJSON(home), []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := RunCodex(&out, false); err != nil {
		t.Fatalf("RunCodex: %v", err)
	}
	if !strings.Contains(out.String(), "✓ hooks already wired") {
		t.Errorf("run 1 must recognise the wrapped rule as ghost's own:\n%s", out.String())
	}

	data, err := os.ReadFile(codexHooksJSON(home))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != seed {
		t.Errorf("the wrapped rule must be left byte-identical:\nseed:\n%s\ngot:\n%s", seed, data)
	}
	rules := codexMergedEvent(t, data, "SessionStart")
	if len(rules) != 1 || len(codexRuleCommands(rules[0])) != 1 || codexRuleCommands(rules[0])[0] != wrapped {
		t.Errorf("SessionStart must hold only the wrapped command, got %+v", rules)
	}

	// Run 2 is a fixed point too.
	out.Reset()
	if err := RunCodex(&out, false); err != nil {
		t.Fatalf("RunCodex (second): %v", err)
	}
	if !strings.Contains(out.String(), "✓ hooks already wired") {
		t.Errorf("run 2 must stay a fixed point:\n%s", out.String())
	}
}

// TestStatusCodex_WrappedHooksAreWired pins the status side of #1345: the
// wiring check asks the same ownership rule the merge applies (ghostHookTail),
// so a hand-edited wrapper that runs ghost reads as wired, and a foreign
// command that merely ends in the ghost tail never does. A reader that matched
// the tail alone reported a wrapper as missing and sent the operator to an
// init that rewrote nothing, over a file init reported as already wired.
func TestStatusCodex_WrappedHooksAreWired(t *testing.T) {
	home, _ := setupCodexTestEnv(t)
	ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")

	if err := os.MkdirAll(filepath.Dir(codexHooksJSON(home)), 0755); err != nil {
		t.Fatal(err)
	}
	rendered := renderCodexHooksConfig(ghostBin)
	for _, token := range []string{"session-start", "stop", "session-end"} {
		canonical := shellQuote(ghostBin) + " hook " + token + " --source codex"
		rendered = strings.Replace(rendered, jsonString(canonical), jsonString("GHOST_X=1 "+canonical), 1)
	}
	if err := os.WriteFile(codexHooksJSON(home), []byte(rendered), 0644); err != nil {
		t.Fatal(err)
	}

	var install bytes.Buffer
	if err := RunCodex(&install, false); err != nil {
		t.Fatalf("RunCodex: %v", err)
	}
	if !strings.Contains(install.String(), "✓ hooks already wired") {
		t.Errorf("the wrapped hooks must read as wired on install:\n%s", install.String())
	}

	var out bytes.Buffer
	healthy, err := StatusCodex(&out)
	if err != nil {
		t.Fatalf("StatusCodex: %v", err)
	}
	if !healthy {
		t.Errorf("StatusCodex: healthy = false over wrapped wiring, want true:\n%s", out.String())
	}
	if strings.Contains(out.String(), "✗") {
		t.Errorf("a wrapped ghost rule must not read as missing:\n%s", out.String())
	}
}

// TestMergeCodexHooksPrefersTheWrappedSurvivor pins the #1345 self-heal in
// both orders: a wrapper that carries its own command beside ghost's hook wins
// over a canonical ghost rule, whether it sits before or after it. Unwrapped
// (canonical) actions that came second were pruned already; what must not
// happen is a wrapper being pruned and its chained command lost with it.
func TestMergeCodexHooksPrefersTheWrappedSurvivor(t *testing.T) {
	const ghostBin = "/new/ghost"
	for _, order := range []string{"canonical-first", "wrapper-first"} {
		t.Run(order, func(t *testing.T) {
			for _, ev := range []struct{ key, token string }{
				{"SessionStart", "session-start"},
				{"Stop", "stop"},
				{"SessionEnd", "session-end"},
			} {
				t.Run(ev.key, func(t *testing.T) {
					canonical := shellQuote(ghostBin) + " hook " + ev.token + " --source codex"
					wrapped := "cd /d && " + shellQuote(ghostBin) + " hook " + ev.token + " --source codex && /usr/local/bin/other-tool"
					var first, second string
					if order == "canonical-first" {
						first, second = canonical, wrapped
					} else {
						first, second = wrapped, canonical
					}
					seed := []byte(`{
  "hooks": {
    "` + ev.key + `": [
      {"hooks": [{"type": "command", "command": ` + jsonString(first) + `}]},
      {"hooks": [{"type": "command", "command": ` + jsonString(second) + `}]}
    ]
  }
}`)

					got, err := mergeCodexHooksConfig(seed, ghostBin)
					if err != nil {
						t.Fatalf("mergeCodexHooksConfig: %v", err)
					}
					rules := codexMergedEvent(t, []byte(got), ev.key)
					if len(rules) != 1 {
						t.Fatalf("preferred survivor must stand alone, got %d rules:\n%s", len(rules), got)
					}
					cmds := codexRuleCommands(rules[0])
					if len(cmds) != 1 || cmds[0] != wrapped {
						t.Fatalf("the wrapper must survive byte-identical, got %v", cmds)
					}

					ghostFires := 0
					for _, c := range cmds {
						if strings.Contains(c, "hook "+ev.token+" --source codex") {
							ghostFires++
						}
					}
					if ghostFires != 1 {
						t.Errorf("ghost must fire exactly once, got %d actions", ghostFires)
					}

					again, err := mergeCodexHooksConfig([]byte(got), ghostBin)
					if err != nil {
						t.Fatalf("mergeCodexHooksConfig (second pass): %v", err)
					}
					if again != got {
						t.Errorf("pass 2 diverged by %+d bytes:\nfirst:\n%s\nsecond:\n%s", len(again)-len(got), got, again)
					}
				})
			}
		})
	}
}

// TestMergeCodexHooksKeepsEveryWrappedAction pins the corollary for the new
// rule: when no canonical ghost action is present, every wrapped rule that
// runs ghost is carried across and nothing is pruned — a wrapper is never
// removed, even if another wrapper runs ghost beside it.
func TestMergeCodexHooksKeepsEveryWrappedAction(t *testing.T) {
	const ghostBin = "/new/ghost"
	for _, ev := range []struct{ key, token string }{
		{"Stop", "stop"},
	} {
		w1 := "cd /d && " + shellQuote(ghostBin) + " hook " + ev.token + " --source codex && /usr/local/bin/first-tool"
		w2 := "GHOST_X=1 " + shellQuote(ghostBin) + " hook " + ev.token + " --source codex"
		seed := []byte(`{
  "hooks": {
    "` + ev.key + `": [
      {"hooks": [{"type": "command", "command": ` + jsonString(w1) + `}]},
      {"hooks": [{"type": "command", "command": ` + jsonString(w2) + `}]}
    ]
  }
}`)
		got, err := mergeCodexHooksConfig(seed, ghostBin)
		if err != nil {
			t.Fatal(err)
		}
		rules := codexMergedEvent(t, []byte(got), ev.key)
		if len(rules) != 2 {
			t.Fatalf("both wrapped actions must survive, got %d:\n%s", len(rules), got)
		}
		got1 := codexRuleCommands(rules[0])[0]
		got2 := codexRuleCommands(rules[1])[0]
		if got1 != w1 || got2 != w2 {
			t.Fatalf("both wrappers must stay byte-identical:\ngot: %v %v", got1, got2)
		}
		if again, err := mergeCodexHooksConfig([]byte(got), ghostBin); err != nil || again != got {
			t.Fatalf("not a fixed point: %v", err)
		}
	}
}

// TestMergeCodexHooksReadsOnlyGhostsOwnArguments pins the reviewer finding on
// the wrapped-survivor read: the --source token that decides whether a wrapper
// is wired must belong to ghost's own invocation, so the scan stops at the
// first shell connective. A --source codex on a command chained behind && or ;
// is another tool's and must never suppress the canonical codex rule.
func TestMergeCodexHooksReadsOnlyGhostsOwnArguments(t *testing.T) {
	const ghostBin = "/new/ghost"
	for _, ev := range []struct{ key, token string }{
		{"SessionStart", "session-start"},
		{"Stop", "stop"},
		{"SessionEnd", "session-end"},
	} {
		t.Run(ev.key, func(t *testing.T) {
			canonical := shellQuote(ghostBin) + " hook " + ev.token + " --source codex"

			// A --source codex after the ghost invocation belongs to the
			// chained command, so the wrapper is not ghost's wired survivor
			// and the canonical rule must be appended beside it.
			chained := "cd /d && " + shellQuote(ghostBin) + " hook " + ev.token + " && /usr/local/bin/tool --source codex"
			seed := []byte(`{
  "hooks": {
    "` + ev.key + `": [
      {"hooks": [{"type": "command", "command": ` + jsonString(chained) + `}]}
    ]
  }
}`)
			got, err := mergeCodexHooksConfig(seed, ghostBin)
			if err != nil {
				t.Fatalf("mergeCodexHooksConfig: %v", err)
			}
			rules := codexMergedEvent(t, []byte(got), ev.key)
			if len(rules) != 2 {
				t.Fatalf("a --source of a chained command must not count: ghost's rule should be appended, got %d rules:\n%s", len(rules), got)
			}
			if cmds := codexRuleCommands(rules[0]); len(cmds) != 1 || cmds[0] != chained {
				t.Errorf("the wrapper must survive byte-identical, got %v", cmds)
			}
			if cmds := codexRuleCommands(rules[1]); len(cmds) != 1 || cmds[0] != canonical {
				t.Errorf("ghost's own codex rule should be appended beside the wrapper, got %v", cmds)
			}

			// The day's positive control: a --source codex that IS ghost's own
			// still makes the wrapper the sole survivor.
			own := "cd /d && " + shellQuote(ghostBin) + " hook " + ev.token + " --source codex && /usr/local/bin/other-tool"
			seed = []byte(`{
  "hooks": {
    "` + ev.key + `": [
      {"hooks": [{"type": "command", "command": ` + jsonString(own) + `}]}
    ]
  }
}`)
			got, err = mergeCodexHooksConfig(seed, ghostBin)
			if err != nil {
				t.Fatalf("mergeCodexHooksConfig: %v", err)
			}
			rules = codexMergedEvent(t, []byte(got), ev.key)
			if len(rules) != 1 {
				t.Fatalf("ghost's own --source must keep the wrapper as sole survivor, got %d rules:\n%s", len(rules), got)
			}
			if cmds := codexRuleCommands(rules[0]); len(cmds) != 1 || cmds[0] != own {
				t.Errorf("the wrapper must survive byte-identical, got %v", cmds)
			}
			if again, err := mergeCodexHooksConfig([]byte(got), ghostBin); err != nil || again != got {
				t.Errorf("not a fixed point: %v", err)
			}
		})
	}
}

// TestAuditCodexHooksNull pins #1219: a hooks.json whose "hooks" value is the
// JSON literal null must be refused, not merged into. json.Unmarshal decodes
// null into a nil map with no error, so without the isJSONNull guard the
// assignment of ghost's rendered rules panics with "assignment to entry in nil
// map" — under a dry run too, because the merge runs before the dry-run check,
// and with it every client after codex in a multi-client run. The Claude
// settings path refuses the same value the same way (findHookCommand's
// "hooks is null").
func TestAuditCodexHooksNull(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("mergeCodexHooksConfig panicked on a null hooks value: %v", r)
		}
	}()

	if _, err := mergeCodexHooksConfig([]byte(`{"description":"my hooks","hooks":null}`), "/new/ghost"); err == nil {
		t.Fatal("a null hooks value must be refused, not silently overwritten with ghost's rules")
	}
}

// TestAuditCodexHooksNullEventValue pins #1247: the top-level guard stops at the
// top level, but a managed event's value being the JSON literal null is the same
// structurally-invalid-but-present data one level down. json.Unmarshal decodes
// null into a nil []json.RawMessage with no error, so without a guard the merge
// hands that nil slice to mergeCodexEventRules, which appends ghost's canonical
// rule as if the event were empty — the user's explicit null is silently
// replaced. The guard runs in the shared decode, so the install pre-flight and
// the merge refuse the same shape with the same error, as the top-level guard
// does.
func TestAuditCodexHooksNullEventValue(t *testing.T) {
	_, err := mergeCodexHooksConfig([]byte(`{"description":"my hooks","hooks":{"SessionStart":null}}`), "/new/ghost")
	if err == nil {
		t.Fatal("a null event value must be refused, not silently replaced with ghost's rule")
	}
	if !strings.Contains(err.Error(), "hooks.json") {
		t.Errorf("refusal must name the file, got: %v", err)
	}
	if !strings.Contains(err.Error(), "SessionStart") {
		t.Errorf("refusal must name the event, got: %v", err)
	}
}

// TestMergeCodexHooksLeavesAnUnmanagedEventNull pins the SCOPE of the #1247
// guard: it covers only the events ghost manages. An explicit null on an event
// ghost does not touch is the user's data and is carried through verbatim, as
// any foreign event is, so the guard does not start refusing a file init has
// always been able to merge for every managed event.
func TestMergeCodexHooksLeavesAnUnmanagedEventNull(t *testing.T) {
	got, err := mergeCodexHooksConfig([]byte(`{"hooks":{"PreToolUse":null}}`), "/new/ghost")
	if err != nil {
		t.Fatalf("an unmanaged event's null must not be refused: %v", err)
	}
	var top struct {
		Hooks map[string]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(got), &top); err != nil {
		t.Fatalf("parse merged hooks.json: %v", err)
	}
	if raw, ok := top.Hooks["PreToolUse"]; !ok || !isJSONNull(raw) {
		t.Errorf("an unmanaged event's null must survive the merge verbatim, got: %q", raw)
	}
}

// TestRunCodex_NullHooksIsRefused pins the install path the audit walked: a
// hooks.json carrying an explicit null is refused with an error rather than
// crashing, and the file is left exactly as it was. It runs the dry-run form,
// where the merge happens before the dry-run check and the panic used to take
// the whole command down.
func TestRunCodex_NullHooksIsRefused(t *testing.T) {
	home, _ := setupCodexTestEnv(t)
	path := codexHooksJSON(home)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	const seed = `{"description":"my hooks","hooks":null}`
	if err := os.WriteFile(path, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := RunCodex(&out, true); err == nil {
		t.Fatal("a null hooks value must be refused, not merged into")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != seed {
		t.Errorf("a refused merge must leave the file untouched:\ngot:\n%s", got)
	}
}

// TestRunCodex_NullEventValueRefused pins the install path for #1247: a managed
// event whose value is an explicit null is refused by the pre-flight — the same
// shared decode the merge uses — naming the file and the event, before step 3
// registers the MCP server, with the file left exactly as written. It covers
// `{"hooks":{"SessionStart":null}}`, the shape the top-level guard does not
// reach.
func TestRunCodex_NullEventValueRefused(t *testing.T) {
	home, _ := setupCodexTestEnv(t)
	path := codexHooksJSON(home)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	const seed = `{"description":"my hooks","hooks":{"SessionStart":null}}`
	if err := os.WriteFile(path, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := RunCodex(&out, true)
	if err == nil {
		t.Fatal("a null event value must be refused, not merged into")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("refusal must name the file, got: %v", err)
	}
	if !strings.Contains(err.Error(), "SessionStart") {
		t.Errorf("refusal must name the event, got: %v", err)
	}
	if !strings.Contains(err.Error(), "Fix or delete") {
		t.Errorf("refusal must carry the edit that clears it, got: %v", err)
	}
	if strings.Contains(err.Error(), "to retry") {
		t.Errorf("refusal must not carry the generic re-run hint, got: %v", err)
	}
	if _, statErr := os.Stat(codexConfigToml(home)); !os.IsNotExist(statErr) {
		t.Errorf("a refused hooks.json must leave config.toml unregistered (stat err %v)", statErr)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != seed {
		t.Errorf("a refused file must be left untouched, got:\n%s", got)
	}
}

// TestMergeCodexHooksRefusesANullDocument pins the DOCUMENT level of the null
// rules: a hooks.json whose whole content is the JSON literal null. json.Unmarshal
// decodes it into a nil map with no error, so the "hooks" member reads as
// absent, the pre-flight passes and the merge writes ghost's canonical
// document over the user's file. A null document is not an empty document to
// fill in, so it is refused by name — the same rule the Claude settings path
// applies to a null settings.json.
func TestMergeCodexHooksRefusesANullDocument(t *testing.T) {
	for _, doc := range []string{"null", "null\n"} {
		_, err := mergeCodexHooksConfig([]byte(doc), "/new/ghost")
		if err == nil {
			t.Errorf("a null document (%q) must be refused, not overwritten with ghost's canonical document", doc)
			continue
		}
		if !strings.Contains(err.Error(), "hooks.json") {
			t.Errorf("refusal for %q must name the file, got: %v", doc, err)
		}
	}
}

// TestRunCodex_NullDocumentIsRefused pins the install path: a hooks.json whose
// document is the JSON literal null is refused by the pre-flight — the shared
// decode the merge uses — before step 3 registers the MCP server, with the file
// left exactly as written.
func TestRunCodex_NullDocumentIsRefused(t *testing.T) {
	home, _ := setupCodexTestEnv(t)
	path := codexHooksJSON(home)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	const seed = "null"
	if err := os.WriteFile(path, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := RunCodex(&out, false)
	if err == nil {
		t.Fatalf("a null hooks.json document must be refused; output:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("refusal must name the file, got: %v", err)
	}
	if _, statErr := os.Stat(codexConfigToml(home)); !os.IsNotExist(statErr) {
		t.Errorf("a refused hooks.json must leave config.toml unregistered (stat err %v)", statErr)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != seed {
		t.Errorf("a refused file must be left untouched, got:\n%s", got)
	}
}

// TestRunCodex_BlankHooksJSONWritten pins #1296: a 0-byte or whitespace-only
// hooks.json carries nothing to merge, so it is treated as absent and written
// as the canonical document in one run, the way a 0-byte config.toml is
// appended to. Refusing it instead would leave every re-run refusing the same
// file until the user deleted it.
func TestRunCodex_BlankHooksJSONWritten(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed string
	}{
		{"empty", ""},
		{"whitespace", "  \n\t\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, _ := setupCodexTestEnv(t)
			path := codexHooksJSON(home)
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.seed), 0644); err != nil {
				t.Fatal(err)
			}

			var out bytes.Buffer
			if err := RunCodex(&out, false); err != nil {
				t.Fatalf("RunCodex over a blank hooks.json must succeed, got: %v", err)
			}
			if !strings.Contains(out.String(), "+ installed hooks.json") {
				t.Errorf("a blank hooks.json must be reported as installed, got:\n%s", out.String())
			}

			ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if want := renderCodexHooksConfig(ghostBin); string(data) != want {
				t.Errorf("a blank hooks.json must be written as the canonical document:\ngot:\n%s\nwant:\n%s", data, want)
			}

			// Idempotent: a second run over the canonical document writes
			// nothing and reports it as already wired.
			var second bytes.Buffer
			if err := RunCodex(&second, false); err != nil {
				t.Fatalf("second RunCodex: %v", err)
			}
			if !strings.Contains(second.String(), "✓ hooks already wired") {
				t.Errorf("a second run must be a no-op, got:\n%s", second.String())
			}
		})
	}
}

// TestRunCodex_RefusesMalformedHooksBeforeRegistering pins #1296's other half:
// a hooks.json that is present but structurally invalid is refused BEFORE step
// 3 registers the MCP server in config.toml, with the file named and the edit
// that clears it — never the generic re-run hint, which a re-run cannot
// satisfy while the file stays invalid. The file is left exactly as written
// and no config.toml is created, so a refused install wires nothing.
func TestRunCodex_RefusesMalformedHooksBeforeRegistering(t *testing.T) {
	home, _ := setupCodexTestEnv(t)
	path := codexHooksJSON(home)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	const seed = `{"hooks":`
	if err := os.WriteFile(path, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := RunCodex(&out, false)
	if err == nil {
		t.Fatalf("a malformed hooks.json must be refused; output:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("refusal must name the file, got: %v", err)
	}
	if !strings.Contains(err.Error(), "Fix or delete") {
		t.Errorf("refusal must name the edit that clears it, got: %v", err)
	}
	if strings.Contains(err.Error(), "to retry") {
		t.Errorf("refusal must not carry the generic re-run hint, got: %v", err)
	}
	// Nothing half installed: the refusal lands before step 3, so config.toml
	// must not have been created.
	if _, statErr := os.Stat(codexConfigToml(home)); !os.IsNotExist(statErr) {
		t.Errorf("a refused hooks.json must leave config.toml unregistered (stat err %v)", statErr)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != seed {
		t.Errorf("a refused file must be left untouched, got:\n%s", got)
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
	ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")

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
		"null hooks document": {seed: func(t *testing.T, home string) {
			path := codexHooksJSON(home)
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("null\n"), 0644); err != nil {
				t.Fatal(err)
			}
		}, wantFail: "cannot read hooks: hooks.json is null"},
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

	const ghostBin = "/opt/bin/ghost"
	cases := []struct {
		cmd  string
		want bool
	}{
		{"'/opt/bin/ghost' hook stop --source codex", true},
		{`"/opt/bin/ghost" hook stop --source=codex`, true},
		{"'/opt/bin/ghost' hook stop", false},
		{"'/opt/bin/ghost' hook stop --source claude-code", false},
		{"'/opt/bin/ghost' hook stop --source codex-extra", false},
		{"env X=1 '/opt/bin/ghost' hook stop --source codex", true},
		{"cd /d && '/opt/bin/ghost' hook stop --source codex", true},
		{"cd /d && '/opt/bin/ghost' hook stop && /usr/local/bin/tool --source codex", false},
		{"cd /d && '/opt/bin/ghost' hook stop --source codex && /usr/local/bin/tool", true},
		{`/bin/sh -c "'/opt/bin/ghost' hook stop --source codex"`, true},
		{"'/opt/bin/othertool' hook stop --source codex", false},
		{`/usr/local/bin/othertool --note "ghost hook stop --source codex"`, false},
		{`/bin/sh -c "echo ghost hook stop --source codex"`, false},
	}
	for _, tc := range cases {
		if got := codexContractHookWired(rule(tc.cmd), "stop", "codex", ghostBin); got != tc.want {
			t.Errorf("codexContractHookWired(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}

// TestCodexHooksJSONBOMIsRefusedByName pins #1379: codex parses hooks.json
// with serde_json, which rejects a leading byte-order mark, so both the init
// decode and the status reader refuse it with a message that names the mark
// rather than a bare parse error, and init leaves the file as written.
func TestCodexHooksJSONBOMIsRefusedByName(t *testing.T) {
	home, _ := setupCodexTestEnv(t)
	path := codexHooksJSON(home)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	seed := "\ufeff{\"hooks\":{}}\n"
	if err := os.WriteFile(path, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	t.Run("init", func(t *testing.T) {
		var out bytes.Buffer
		err := RunCodex(&out, false)
		if err == nil {
			t.Fatalf("a BOM-prefixed hooks.json must be refused; output:\n%s", out.String())
		}
		if !strings.Contains(err.Error(), "byte-order mark") {
			t.Errorf("refusal must name the byte-order mark, got: %v", err)
		}
		got, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(got) != seed {
			t.Errorf("a refused file must be left untouched, got:\n%q", got)
		}
	})
	t.Run("status reader", func(t *testing.T) {
		_, err := loadCodexHooksRules()
		if err == nil {
			t.Fatal("the status reader must refuse a BOM-prefixed hooks.json")
		}
		if !strings.Contains(err.Error(), "byte-order mark") {
			t.Errorf("status refusal must name the byte-order mark, got: %v", err)
		}
	})
}

// TestCodexRefusedLineIsRendered holds the refused config line to the rule every
// other refusal follows: a value read from a user-owned file is rendered, never
// echoed raw. A token-shaped value on the line is withheld (the token guard) and
// a control character is escaped, while the line number stays so the entry can
// still be found.
func TestCodexRefusedLineIsRendered(t *testing.T) {
	const token = "sk-ant-api03-FAKEFAKEFAKEFAKEFAKEFAKEFAKE"
	cases := map[string]struct {
		line    string
		mention string
	}{
		"dotted or inline key with a token-shaped value": {
			line:    `mcp_servers.ghost = { command = "ghost", args = ["mcp", "--key=` + token + `"] }`,
			mention: "dotted or inline",
		},
		"dotted or inline key with a control character": {
			line:    "mcp_servers.ghost = { command = \"ghost\", args = [\"mcp\", \"\x1b[31mred\x07\"] }",
			mention: "dotted or inline",
		},
		"unparseable header with a token-shaped value": {
			line:    `[mcp_servers.ghost."` + token,
			mention: "table header ghost cannot parse",
		},
		"unparseable header with a control character": {
			line:    "[mcp_servers.ghost.\"\x1b[31m",
			mention: "table header ghost cannot parse",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			home, _ := setupCodexTestEnv(t)
			seed := "model = \"gpt-5\"\n\n" + tc.line + "\n"
			if err := os.MkdirAll(filepath.Dir(codexConfigToml(home)), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(codexConfigToml(home), []byte(seed), 0644); err != nil {
				t.Fatal(err)
			}

			var initOut, statusOut bytes.Buffer
			if err := RunCodex(&initOut, false); err != nil {
				t.Fatalf("RunCodex: %v", err)
			}
			if _, err := StatusCodex(&statusOut); err != nil {
				t.Fatalf("StatusCodex: %v", err)
			}
			for surface, out := range map[string]string{"init": initOut.String(), "status": statusOut.String()} {
				if !strings.Contains(out, tc.mention) {
					t.Errorf("%s: expected the refusal to be reported, got:\n%s", surface, out)
				}
				if !strings.Contains(out, "line 3") {
					t.Errorf("%s: the line number must stay so the entry can be found, got:\n%s", surface, out)
				}
				if strings.Contains(out, token) || strings.Contains(out, "FAKEFAKE") {
					t.Errorf("%s: a token-shaped value reached the output:\n%s", surface, out)
				}
				if strings.ContainsAny(out, "\x1b\x07") {
					t.Errorf("%s: a control character reached the output raw:\n%q", surface, out)
				}
			}
		})
	}
}
