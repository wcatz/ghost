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

// runCodexOn writes seed as config.toml under home, runs RunCodex and returns
// the resulting file and the output.
func runCodexOn(t *testing.T, home, seed string) (got []byte, out *bytes.Buffer) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(codexConfigToml(home)), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexConfigToml(home), []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}
	out = &bytes.Buffer{}
	if err := RunCodex(out, false); err != nil {
		t.Fatalf("RunCodex: %v", err)
	}
	got, err := os.ReadFile(codexConfigToml(home))
	if err != nil {
		t.Fatal(err)
	}
	return got, out
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

			got, _ := runCodexOn(t, home, tc.seed)
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
		"nested array elements": {
			// [1, 2], ["A", "b.c"] and [true] are bracketed like headers or quoted
			// keys but are values; treating one as a header ends the span early,
			// which drops env_keep's opening bracket and duplicates command/args.
			seed: "[mcp_servers.ghost]\n" +
				"env_keep = [\n" +
				"  [1, 2],\n" +
				"  [\"A\", \"b.c\"],\n" +
				"  [true]\n" +
				"]\n" +
				"command = '/old/install/ghost'\n" +
				"args = [\"mcp\"]\n",
			want: "[mcp_servers.ghost]\n" +
				"env_keep = [\n" +
				"  [1, 2],\n" +
				"  [\"A\", \"b.c\"],\n" +
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

			got, _ := runCodexOn(t, home, tc.seed)
			if string(got) != want {
				t.Errorf("repaired config.toml mismatch:\nwant:\n%q\ngot:\n%q", want, got)
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

		got, out := runCodexOn(t, home, seed)
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
	for name, tc := range map[string]struct{ seed, warn string }{
		"dotted key":       {bom + "mcp_servers.ghost = { command = \"/bin/ghost\", args = [\"mcp\"] }\n", "dotted or inline key"},
		"ambiguous header": {bom + "[[mcp_servers.ghost]]\ncommand = \"/bin/ghost\"\n", "left unchanged"},
	} {
		t.Run(name, func(t *testing.T) {
			home, _ := setupCodexTestEnv(t)
			got, out := runCodexOn(t, home, tc.seed)
			if string(got) != tc.seed {
				t.Errorf("the definition must be left unchanged, got:\n%s", got)
			}
			if !strings.Contains(out.String(), tc.warn) {
				t.Errorf("expected a %q warning, got:\n%s", tc.warn, out.String())
			}
		})
	}
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
			got, out := runCodexOn(t, home, seed)
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

			got, _ := runCodexOn(t, home, seed)
			if string(got) != want {
				t.Errorf("repaired config.toml mismatch:\nwant:\n%q\ngot:\n%q", want, got)
			}
			if n := countCodexGhostHeaders(string(got)); n != 1 {
				t.Errorf("the ghost table must have exactly one header, got %d:\n%s", n, got)
			}
		})
	}
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

	got, _ := runCodexOn(t, home, seed)
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
	// The repair ran: the stale command is replaced with this run's binary.
	got, _ := runCodexOn(t, home, seed)
	if !strings.Contains(string(got), codexTOMLString(ghostBin)) {
		t.Fatalf("the stale-path repair did not run:\n%s", got)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		bak, _ := os.ReadFile(path + ".bak")
		t.Errorf("a repair of ghost's own output left a .bak:\n%q", bak)
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
			got, out := runCodexOn(t, home, seed)
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
