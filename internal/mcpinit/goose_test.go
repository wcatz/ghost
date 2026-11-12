package mcpinit

import (
	"bytes"
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
	setHome(t, home)
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

func TestRunGoose_DriftRepaired(t *testing.T) {
	home, _ := setupGooseTestEnv(t)
	ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")

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
	ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")

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
