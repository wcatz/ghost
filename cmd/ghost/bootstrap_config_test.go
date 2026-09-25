package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/config"
)

// isolateConfigEnv clears every GHOST_* variable for the duration of the test.
// The fallback under test reads the real environment, so a host that exports
// GHOST_EMBEDDING_ENABLED=false or GHOST_SCRATCH_MAX_BYTES=0 would otherwise
// make the compiled-default assertions below fail on that machine and nowhere
// else. config's own tests have the same helper; it is unexported, so it is
// repeated here rather than exported for one caller.
func isolateConfigEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, "GHOST_") {
			continue
		}
		t.Setenv(name, value) // registers the restore
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

// TestConfigOnLoadError pins the per-caller policy for a config file that
// exists but does not parse. The distinction is the whole point: a CLI
// subcommand should stop with an error the user can act on, while `ghost mcp`
// — the long-lived server the host client spawns — must stay up on the compiled
// defaults, or a typo in a file the user may not know exists would leave their
// editor with no Ghost tools at all.
func TestConfigOnLoadError(t *testing.T) {
	// Set the opt-outs BEFORE isolating, so isolating has something to clear:
	// without the isolation the fallback would see them and this test would fail,
	// which is what makes the helper's effect checkable rather than assumed. On a
	// machine that exports these it is the difference between a test that runs
	// everywhere and one that only fails where it happens to matter.
	t.Setenv("GHOST_EMBEDDING_ENABLED", "false")
	t.Setenv("GHOST_SCRATCH_MAX_BYTES", "0")
	isolateConfigEnv(t)

	parseErr := errors.New("parse /home/you/.config/ghost/config.yaml: yaml: line 3: bad")

	if _, fatal := configOnLoadError(failOnConfig, parseErr); !fatal {
		t.Error("failOnConfig must be fatal: a CLI subcommand has to stop, not run on defaults")
	}

	cfg, fatal := configOnLoadError(warnOnConfig, parseErr)
	if fatal {
		t.Error("warnOnConfig must not be fatal: exiting takes down the host session's MCP server")
	}
	if cfg == nil {
		t.Fatal("warnOnConfig fallback = nil, want the compiled defaults")
	}
	// The fallback must be config.FallbackConfig() itself, so the server and the
	// session hooks can never drift onto different values. Embedding.Enabled
	// doubles as the check that the environment really was cleared above.
	if !cfg.Embedding.Enabled || cfg.Scratch.MaxBytes != config.DefaultScratchMaxBytes ||
		cfg.Linking.DemotionThreshold != 0.90 || cfg.Injection.BehaviorFloor != 8 ||
		cfg.Obsidian.Interval != "30s" {
		t.Errorf("warnOnConfig fallback = %+v, want the compiled defaults (isolateConfigEnv must have cleared GHOST_*)", cfg)
	}
}
