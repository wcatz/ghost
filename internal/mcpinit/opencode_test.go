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

// setupOpencodeTestEnv isolates a RunOpencode test: HOME and XDG_CONFIG_HOME
// point at temp dirs, PATH contains only a stub ghost binary (no claude, no
// opencode), and embeddings are disabled so the Ollama check stays offline.
func setupOpencodeTestEnv(t *testing.T) (home, xdg string) {
	t.Helper()
	home = t.TempDir()
	setHome(t, home)
	xdg = t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("GHOST_EMBEDDING_ENABLED", "false")
	// A $OPENCODE_CONFIG_DIR leaked from the host would move every path this
	// test asserts on: opencode resolves exactly one config directory.
	t.Setenv("OPENCODE_CONFIG_DIR", "")

	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeStub(t, binDir, "ghost")
	t.Setenv("PATH", binDir)
	return home, xdg
}

// stubFileName is the file name a stub of `name` must have for
// exec.LookPath to resolve it: Windows only matches names against PATHEXT, so
// an extensionless file can never be found there; POSIX keeps the bare name.
func stubFileName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".bat"
	}
	return name
}

// stubPath is the exact path writeStub creates in dir — the form
// exec.LookPath resolves it to on this OS, and therefore the path production
// embeds when the stub is on PATH.
func stubPath(dir, name string) string {
	return filepath.Join(dir, stubFileName(name))
}

// writeStub creates an executable stub at binDir/name. The stubs exist to be
// found by exec.LookPath and echoed into rendered config; the only run they
// get is findGhostBinary's `--version` probe, which they pass by exiting 0.
func writeStub(t *testing.T, binDir, name string) string {
	t.Helper()
	content := "#!/bin/sh\n"
	if runtime.GOOS == "windows" {
		content = "@echo off\r\nexit /b 0\r\n"
	}
	path := stubPath(binDir, name)
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRenderOpencodeGhostPlugin_ConfigSessionListIsBounded guards against a
// reintroduced deadlock: the plugin's config() hook calls client.session.list()
func TestRenderOpencodeGhostPlugin_ConfigSessionListIsBounded(t *testing.T) {
	src := renderOpencodeGhostPlugin("/usr/local/bin/ghost")
	if !strings.Contains(src, "withTimeout(client.session.list()") {
		t.Error("client.session.list() in config() must be wrapped in a timeout — an unbounded await there deadlocks opencode's own startup (config() never returns, so ghost's MCP entry is never even read)")
	}
}

func TestStripJSONC(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"line comment keeps its newline": {
			in:   "{\n  // c\n  \"a\": 1\n}",
			want: "{\n  \n  \"a\": 1\n}",
		},
		"line comment at EOF adds no newline": {
			in:   "{\"a\": 1} // tail",
			want: "{\"a\": 1} ",
		},
		"block comment keeps only its newlines": {
			in:   "{\"a\": 1 /* c\nc2 */,\n\"b\": 2}",
			want: "{\"a\": 1 \n,\n\"b\": 2}",
		},
		"unterminated block comment swallows the rest": {
			in:   "{\"a\": 1 /* never closed",
			want: "{\"a\": 1 ",
		},
		"trailing commas dropped": {
			in:   `{"a": [1, 2,],}`,
			want: `{"a": [1, 2]}`,
		},
		"slashes and commas inside a string are content": {
			in:   `{"u": "http://x//y", "s": "a, b"}`,
			want: `{"u": "http://x//y", "s": "a, b"}`,
		},
		"escaped quote does not close the string": {
			in:   `{"a": "say \"// not a comment"} // gone`,
			want: `{"a": "say \"// not a comment"} `,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := string(stripJSONC([]byte(tc.in))); got != tc.want {
				t.Errorf("stripJSONC(%q)\n  =  %q\n  want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRunOpencode_Idempotent(t *testing.T) {
	_, xdg := setupOpencodeTestEnv(t)
	pluginPath := filepath.Join(xdg, "opencode", "plugins", "ghost-opencode.ts")

	var out1 bytes.Buffer
	if err := RunOpencode(&out1, false); err != nil {
		t.Fatalf("RunOpencode (first): %v", err)
	}
	first, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("read plugin: %v", err)
	}

	var out2 bytes.Buffer
	if err := RunOpencode(&out2, false); err != nil {
		t.Fatalf("RunOpencode (second): %v", err)
	}
	if !strings.Contains(out2.String(), "already installed") {
		t.Errorf("second run should report 'already installed', got:\n%s", out2.String())
	}
	second, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("re-read plugin: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Error("second run must not alter an identical plugin file")
	}
	if _, err := os.Stat(filepath.Join(xdg, "opencode", "opencode.json")); !os.IsNotExist(err) {
		t.Error("no opencode.json should exist after idempotent runs")
	}
}

func TestRunOpencode_DryRunWritesNothing(t *testing.T) {
	_, xdg := setupOpencodeTestEnv(t)

	var out bytes.Buffer
	if err := RunOpencode(&out, true); err != nil {
		t.Fatalf("RunOpencode dry run: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "would install lifecycle plugin") {
		t.Errorf("dry run should say 'would install lifecycle plugin', got:\n%s", output)
	}
	if _, err := os.Stat(filepath.Join(xdg, "opencode", "opencode.json")); err == nil {
		t.Error("dry run must not write the opencode config")
	}
	if _, err := os.Stat(filepath.Join(xdg, "opencode", "plugins", "ghost-opencode.ts")); err == nil {
		t.Error("dry run must not write the lifecycle plugin")
	}
}

func writeExitStub(t *testing.T, dir, name string, code int) string {
	t.Helper()
	content := fmt.Sprintf("#!/bin/sh\nexit %d\n", code)
	if runtime.GOOS == "windows" {
		content = fmt.Sprintf("@echo off\r\nexit /b %d\r\n", code)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := stubPath(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestFindGhostBinary_SkipsPathHitThatRefusesToRun pins #1569: a ghost on
// PATH that cannot run in this environment (a sandbox guard shim exits
// non-zero outside its sandbox) must not be written into hooks when a
// working ghost sits in a common install dir.
func TestFindGhostBinary_SkipsPathHitThatRefusesToRun(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	orig := systemBinDirs
	systemBinDirs = nil
	t.Cleanup(func() { systemBinDirs = orig })

	shimDir := t.TempDir()
	writeExitStub(t, shimDir, "ghost", 97)
	t.Setenv("PATH", shimDir)
	good := writeExitStub(t, filepath.Join(home, ".local", "bin"), "ghost", 0)

	if got := findGhostBinary(); got != good {
		t.Errorf("findGhostBinary() = %q, want %q (the PATH hit exits 97)", got, good)
	}
}

// writeOpencodeStub installs a fake opencode that answers --version with
// version and records every other invocation's argv in a marker file, so a
// test can assert which subcommands the installer ran.
func writeOpencodeStub(t *testing.T, binDir, version, mcpOutput string) (calls string) {
	t.Helper()
	calls = filepath.Join(t.TempDir(), "calls")
	var script string
	if runtime.GOOS == "windows" {
		// The sh form's contract as a runnable batch file: --version answers
		// with version and exits without logging; any other argv is appended
		// to the calls file, then mcpOutput is printed.
		script = "@echo off\r\n" +
			"if \"%~1\"==\"--version\" goto version\r\n" +
			">>\"" + calls + "\" echo %*\r\n" +
			"echo " + mcpOutput + "\r\n" +
			"exit /b 0\r\n" +
			":version\r\n" +
			"echo " + version + "\r\n" +
			"exit /b 0\r\n"
	} else {
		script = "#!/bin/sh\n" +
			"if [ \"$1\" = \"--version\" ]; then echo '" + version + "'; exit 0; fi\n" +
			"echo \"$*\" >> '" + calls + "'\n" +
			"echo '" + mcpOutput + "'\n"
	}
	if err := os.WriteFile(stubPath(binDir, "opencode"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return calls
}

// TestVerifyOpencodeRegistration_V1 keeps the V1 check: `opencode mcp ls`
// lists plugin-registered servers there, so a ghost entry verifies.
func TestVerifyOpencodeRegistration_V1(t *testing.T) {
	home, _ := setupOpencodeTestEnv(t)
	calls := writeOpencodeStub(t, filepath.Join(home, "bin"), "1.18.32", "ghost connected")

	var out bytes.Buffer
	verifyOpencodeRegistration(&out)

	if !strings.Contains(out.String(), "verified: ghost listed by `opencode mcp ls`") {
		t.Errorf("V1 should verify via mcp ls, got:\n%s", out.String())
	}
	if data, _ := os.ReadFile(calls); !strings.Contains(string(data), "mcp ls") {
		t.Errorf("V1 must run `opencode mcp ls`, calls: %q", data)
	}
}

// TestVerifyOpencodeRegistration_V2SkipsMcpList pins the V2 behavior: V2 has
// no `mcp ls`, its `mcp list` shows only config-file servers (never the
// plugin's mcp.transform registration), and without --standalone it starts
// the background service as a side effect. The installer must not shell out
// to `opencode mcp` at all on V2.
func TestVerifyOpencodeRegistration_V2SkipsMcpList(t *testing.T) {
	home, _ := setupOpencodeTestEnv(t)
	calls := writeOpencodeStub(t, filepath.Join(home, "bin"), "opencode v2.0.14", "No MCP servers configured")

	var out bytes.Buffer
	verifyOpencodeRegistration(&out)

	if data, err := os.ReadFile(calls); err == nil {
		t.Errorf("V2 must not run any opencode subcommand besides --version, calls: %q", data)
	}
	if !strings.Contains(out.String(), "opencode V2") {
		t.Errorf("V2 should explain why registration isn't verified, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "could not verify") || strings.Contains(out.String(), "not listed") {
		t.Errorf("V2 must not report a false registration failure, got:\n%s", out.String())
	}
}

// TestRenderedOpencodePluginPathIsAJSLiteral pins that the baked binary path
// is a proper string literal: a quote, backslash or newline in the path must
// neither break the plugin nor inject code, and an ordinary path renders as
// the plain quoted string it always did.
func TestRenderedOpencodePluginPathIsAJSLiteral(t *testing.T) {
	const prefix = "const GHOST_BIN_DEFAULT = "
	paths := []string{
		`/usr/local/bin/ghost`,
		`/opt/a"b/ghost`,
		`/opt/a\b/ghost`,
		`/opt/it's/ghost`,
		"/opt/a\nb/ghost",
		`/opt/x"; process.exit(1); //`,
		"/opt/a b c/ghost",
		`/opt/a&b<c>/ghost`,
	}
	for _, p := range paths {
		out := renderOpencodeGhostPlugin(p)
		_, rest, ok := strings.Cut(out, prefix)
		if !ok {
			t.Fatalf("%q: rendered plugin has no %q line", p, prefix)
		}
		line, _, _ := strings.Cut(rest, "\n")
		line = strings.TrimSuffix(line, "\r") // a CRLF checkout ends the line with \r\n
		var got string
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Errorf("%q: rendered constant %q is not a string literal: %v", p, line, err)
			continue
		}
		if got != p {
			t.Errorf("%q: constant decodes to %q", p, got)
		}
		if strings.Contains(out, ghostBinPlaceholder) {
			t.Errorf("%q: placeholder left in rendered plugin", p)
		}
	}
	_, rest, _ := strings.Cut(renderOpencodeGhostPlugin("/usr/local/bin/ghost"), prefix)
	line, _, _ := strings.Cut(rest, "\n")
	if strings.TrimSuffix(line, "\r") != `"/usr/local/bin/ghost"` {
		t.Errorf("ordinary path is not rendered as the plain quoted string: %q", line)
	}
}

// TestParseOpencodeConfig_BOMIsRefusedByName pins #1379: stripJSONC does not
// remove a leading byte-order mark, so the parse failure is named as a BOM
// rather than reported as an anonymous invalid character.
func TestParseOpencodeConfig_BOMIsRefusedByName(t *testing.T) {
	_, err := parseOpencodeConfig([]byte("\ufeff{\"mcp\":{}}"))
	if err == nil {
		t.Fatal("a BOM-prefixed opencode config must be refused")
	}
	if !strings.Contains(err.Error(), "byte-order mark") {
		t.Errorf("refusal must name the byte-order mark, got: %v", err)
	}
}
