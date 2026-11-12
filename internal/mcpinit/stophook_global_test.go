package mcpinit

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func TestSpawnLifecycleIfConfigured_RefusesGlobalThroughDefaultProject(t *testing.T) {
	dataHome := isolatedHome(t)
	home := filepath.Dir(dataHome)
	t.Setenv("GHOST_ROUTING_DEFAULT_PROJECT", memory.GlobalProjectID)
	writeGhostConfigFile(t, "reflection:\n  auto_resolve: true\n")
	seedProject(t, dataHome, memory.GlobalProjectID, memory.GlobalProjectID, "global")

	seen := captureLifecycleChild(t)
	spawnLifecycleIfConfigured(home, "", io.Discard)

	if len(*seen) != 0 {
		t.Errorf("lifecycle child was started for _global through routing.default_project: %v", *seen)
	}
	if _, err := os.Stat(filepath.Join(dataHome, "ghost", "lifecycle.log")); !os.IsNotExist(err) {
		t.Errorf("lifecycle.log exists for _global through routing.default_project, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataHome, "ghost", projectMarkerFile(memory.GlobalProjectID))); !os.IsNotExist(err) {
		t.Errorf("failure marker exists for _global through routing.default_project, stat err = %v", err)
	}
}

func TestSpawnLifecycleIfConfigured_SpawnsOrdinaryDefaultProjectFromHome(t *testing.T) {
	dataHome := isolatedHome(t)
	home := filepath.Dir(dataHome)
	t.Setenv("GHOST_ROUTING_DEFAULT_PROJECT", "infrastructure")
	writeGhostConfigFile(t, "reflection:\n  auto_resolve: true\n")
	projDir := filepath.Join(t.TempDir(), "infra")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir project path: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(projDir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	seedProject(t, dataHome, "infra", canonical, "infrastructure")

	seen := captureLifecycleChild(t)
	spawnLifecycleIfConfigured(home, "", io.Discard)

	if len(*seen) != 1 {
		t.Fatalf("expected one lifecycle child for the ordinary default project, got %v", *seen)
	}
	argv := (*seen)[0]
	found := false
	for i, a := range argv {
		if a == "--project" && i+1 < len(argv) && argv[i+1] == "infra" {
			found = true
		}
	}
	if !found {
		t.Errorf("lifecycle child argv does not carry --project infra: %v", argv)
	}
}

func TestSpawnLifecycle_ReflectOnlySkipsGlobalThroughDefaultProject(t *testing.T) {
	dataHome := isolatedHome(t)
	home := filepath.Dir(dataHome)
	t.Setenv("GHOST_ROUTING_DEFAULT_PROJECT", memory.GlobalProjectID)
	writeGhostConfigFile(t, "reflection:\n  auto_reflect: true\n")
	t.Setenv("PATH", t.TempDir())
	seedProject(t, dataHome, memory.GlobalProjectID, memory.GlobalProjectID, "global")

	seen := captureLifecycleChild(t)
	spawnLifecycleIfConfigured(home, "", io.Discard)

	if len(*seen) != 0 {
		t.Errorf("lifecycle child was started for _global through routing.default_project: %v", *seen)
	}
	if _, err := os.Stat(filepath.Join(dataHome, "ghost", "lifecycle.log")); !os.IsNotExist(err) {
		t.Errorf("lifecycle.log exists for _global through routing.default_project, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataHome, "ghost", projectMarkerFile(memory.GlobalProjectID))); !os.IsNotExist(err) {
		t.Errorf("failure marker exists for _global through routing.default_project, stat err = %v", err)
	}
}
