package mcpinit

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupGooseTestEnv isolates a RunGoose/StatusGoose test: HOME points at a
// temp dir (the plugin installs to <HOME>/.agents/plugins/ghost), XDG dirs
// point at temp dirs, PATH contains only a stub ghost binary (no claude, no
// goose), and embeddings are disabled so the Ollama check stays offline.
func setupGooseTestEnv(t *testing.T) (home, xdg string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	xdg = t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("GHOST_EMBEDDING_ENABLED", "false")

	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeStub(t, binDir, "ghost")
	t.Setenv("PATH", binDir)
	return home, xdg
}

// goosePackagePath resolves a file inside the installed package for tests.
func goosePackagePath(home, rel string) string {
	return filepath.Join(home, ".agents", "plugins", "ghost", rel)
}

func TestRunGoose_FreshInstall(t *testing.T) {
	home, _ := setupGooseTestEnv(t)
	ghostBin := filepath.Join(home, "bin", "ghost")
	dir := goosePackagePath(home, "")

	var out bytes.Buffer
	if err := RunGoose(&out, false); err != nil {
		t.Fatalf("RunGoose: %v", err)
	}

	// Every package file must exist byte-identical to its rendered content.
	for _, f := range goosePackageFiles(ghostBin) {
		data, err := os.ReadFile(filepath.Join(dir, f.rel))
		if err != nil {
			t.Fatalf("expected %s installed: %v", f.rel, err)
		}
		if string(data) != f.want {
			t.Errorf("%s should match the rendered content", f.rel)
		}
	}

	// plugin.json: minimal Agent Plugins v1.0.0 manifest ($schema + name).
	var manifest struct {
		Schema string `json:"$schema"`
		Name   string `json:"name"`
	}
	if err := json.Unmarshal([]byte(renderGoosePluginManifest()), &manifest); err != nil {
		t.Fatalf("plugin.json invalid JSON: %v", err)
	}
	if manifest.Schema != "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json" || manifest.Name != "ghost" {
		t.Errorf("manifest wrong: %+v", manifest)
	}

	// mcp.json: stdio ghost server with the resolved absolute binary baked in.
	var mcp struct {
		Schema     string `json:"$schema"`
		MCPServers map[string]struct {
			Type    string   `json:"type"`
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	mcpData, err := os.ReadFile(goosePackagePath(home, "mcp.json"))
	if err != nil {
		t.Fatalf("read mcp.json: %v", err)
	}
	if err := json.Unmarshal(mcpData, &mcp); err != nil {
		t.Fatalf("mcp.json invalid JSON: %v", err)
	}
	srv, ok := mcp.MCPServers["ghost"]
	if !ok || srv.Type != "stdio" || srv.Command != ghostBin || len(srv.Args) != 1 || srv.Args[0] != "mcp" {
		t.Errorf("mcp.json ghost server wrong: %+v", mcp)
	}

	// hooks/hooks.json: all three lifecycle events wired to the contract
	// with --source goose.
	var hooks struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	hooksData, err := os.ReadFile(goosePackagePath(home, filepath.Join("hooks", "hooks.json")))
	if err != nil {
		t.Fatalf("read hooks.json: %v", err)
	}
	if err := json.Unmarshal(hooksData, &hooks); err != nil {
		t.Fatalf("hooks.json invalid JSON: %v", err)
	}
	for event, argv := range map[string]string{
		"SessionStart": "session-start",
		"Stop":         "stop",
		"SessionEnd":   "session-end",
	} {
		rules := hooks.Hooks[event]
		if len(rules) != 1 || len(rules[0].Hooks) != 1 {
			t.Errorf("%s: expected one rule with one action, got %+v", event, rules)
			continue
		}
		action := rules[0].Hooks[0]
		wantCmd := "'" + ghostBin + "' hook " + argv + " --source goose"
		if action.Type != "command" || action.Command != wantCmd {
			t.Errorf("%s command = %+v, want type=command cmd=%q", event, action, wantCmd)
		}
	}

	output := out.String()
	for _, want := range []string{
		"+ installed plugin.json",
		"+ installed mcp.json",
		"+ installed hooks/hooks.json",
		"Restart goose to activate.",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("expected %q in output, got:\n%s", want, output)
		}
	}
}

func TestRunGoose_Idempotent(t *testing.T) {
	home, _ := setupGooseTestEnv(t)
	ghostBin := filepath.Join(home, "bin", "ghost")

	var first bytes.Buffer
	if err := RunGoose(&first, false); err != nil {
		t.Fatalf("RunGoose (first): %v", err)
	}
	snapshots := make(map[string][]byte)
	for _, f := range goosePackageFiles(ghostBin) {
		data, err := os.ReadFile(goosePackagePath(home, f.rel))
		if err != nil {
			t.Fatalf("read %s: %v", f.rel, err)
		}
		snapshots[f.rel] = data
	}

	var second bytes.Buffer
	if err := RunGoose(&second, false); err != nil {
		t.Fatalf("RunGoose (second): %v", err)
	}
	out2 := second.String()
	if strings.Contains(out2, "+ installed") {
		t.Errorf("second run must not reinstall, got:\n%s", out2)
	}
	if !strings.Contains(out2, "already current") {
		t.Errorf("second run should report 'already current', got:\n%s", out2)
	}
	for _, f := range goosePackageFiles(ghostBin) {
		data, err := os.ReadFile(goosePackagePath(home, f.rel))
		if err != nil {
			t.Fatalf("re-read %s: %v", f.rel, err)
		}
		if !bytes.Equal(data, snapshots[f.rel]) {
			t.Errorf("second run must not alter an identical %s", f.rel)
		}
	}
}

func TestRunGoose_DriftRepaired(t *testing.T) {
	home, _ := setupGooseTestEnv(t)
	ghostBin := filepath.Join(home, "bin", "ghost")

	var install bytes.Buffer
	if err := RunGoose(&install, false); err != nil {
		t.Fatalf("RunGoose (install): %v", err)
	}
	if err := os.WriteFile(goosePackagePath(home, "mcp.json"), []byte("{ corrupted"), 0644); err != nil {
		t.Fatalf("clobber mcp.json: %v", err)
	}
	hooksPath := goosePackagePath(home, filepath.Join("hooks", "hooks.json"))
	if err := os.WriteFile(hooksPath, []byte(`{"hooks":{}}`), 0644); err != nil {
		t.Fatalf("clobber hooks.json: %v", err)
	}

	var repair bytes.Buffer
	if err := RunGoose(&repair, false); err != nil {
		t.Fatalf("RunGoose (repair): %v", err)
	}
	for _, f := range goosePackageFiles(ghostBin) {
		data, err := os.ReadFile(goosePackagePath(home, f.rel))
		if err != nil {
			t.Fatalf("re-read %s: %v", f.rel, err)
		}
		if string(data) != f.want {
			t.Errorf("drifted %s should be restored to the rendered content", f.rel)
		}
	}
	out := repair.String()
	if !strings.Contains(out, "+ installed mcp.json") || !strings.Contains(out, "+ installed hooks/hooks.json") {
		t.Errorf("drift repair should report reinstallation of drifted files, got:\n%s", out)
	}
	if !strings.Contains(out, "✓ plugin.json already current") {
		t.Errorf("undrifted plugin.json must stay untouched, got:\n%s", out)
	}
}

func TestRunGoose_DryRunWritesNothing(t *testing.T) {
	home, xdg := setupGooseTestEnv(t)

	var out bytes.Buffer
	if err := RunGoose(&out, true); err != nil {
		t.Fatalf("RunGoose dry run: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "~ would install") {
		t.Errorf("dry run should say 'would install', got:\n%s", output)
	}
	if !strings.Contains(output, "No changes made (dry run).") {
		t.Errorf("dry run footer missing, got:\n%s", output)
	}
	if _, err := os.Stat(filepath.Join(home, ".agents")); !os.IsNotExist(err) {
		t.Error("dry run must not create ~/.agents/plugins/ghost/")
	}
	cfgPath := filepath.Join(xdg, "ghost", "config.yaml")
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Error("dry run must not write the ghost config file")
	}
}

func TestStatusGoose_HealthyAfterInstall(t *testing.T) {
	home, _ := setupGooseTestEnv(t)
	ghostBin := filepath.Join(home, "bin", "ghost")

	var install bytes.Buffer
	if err := RunGoose(&install, false); err != nil {
		t.Fatalf("RunGoose: %v", err)
	}

	var out bytes.Buffer
	healthy, err := StatusGoose(&out)
	if err != nil {
		t.Fatalf("StatusGoose: %v", err)
	}
	if !healthy {
		t.Errorf("StatusGoose: healthy = false, want true after install, got:\n%s", out.String())
	}
	output := out.String()
	for _, want := range []string{
		"✓ ghost binary: " + ghostBin,
		"✓ goose plugin package installed: " + goosePackagePath(home, ""),
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

func TestStatusGoose_UnhealthyWithoutPackage(t *testing.T) {
	cases := map[string]func(t *testing.T, home string){
		"absent package": func(t *testing.T, home string) {},
		"drifted file": func(t *testing.T, home string) {
			path := goosePackagePath(home, "plugin.json")
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(`{"name":"someone-elses"}`), 0644); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, seed := range cases {
		t.Run(name, func(t *testing.T) {
			home, _ := setupGooseTestEnv(t)
			seed(t, home)

			var out bytes.Buffer
			healthy, err := StatusGoose(&out)
			if err != nil {
				t.Fatalf("StatusGoose: %v", err)
			}
			if healthy {
				t.Errorf("%s: healthy = true, want false without a valid package", name)
			}
			output := out.String()
			if !strings.Contains(output, "✗ goose plugin package missing or outdated") {
				t.Errorf("%s: expected failed package check, got:\n%s", name, output)
			}
			if strings.Contains(output, "All checks passed.") {
				t.Errorf("%s: must not report \"All checks passed.\", got:\n%s", name, output)
			}
		})
	}
}

func TestStatusGoose_GhostMissing(t *testing.T) {
	_, _ = setupGooseTestEnv(t)
	t.Setenv("PATH", t.TempDir())

	var out bytes.Buffer
	healthy, err := StatusGoose(&out)
	if err != nil {
		t.Fatalf("StatusGoose: %v", err)
	}
	if healthy {
		t.Error("StatusGoose: healthy = true, want false when the ghost binary is missing")
	}
	if !strings.Contains(out.String(), "✗ ghost binary not found in PATH") {
		t.Errorf("expected a failed ghost binary check, got:\n%s", out.String())
	}
}
