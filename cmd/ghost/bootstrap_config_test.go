package main

import (
	"errors"
	"testing"

	"github.com/wcatz/ghost/internal/config"
)

// TestConfigOnLoadError pins the per-caller policy for a config file that
// exists but does not parse. The distinction is the whole point: a CLI
// subcommand should stop with an error the user can act on, while `ghost mcp`
// — the long-lived server the host client spawns — must stay up on the
// compiled defaults, or a typo in a file the user may not know exists would
// leave their editor with no Ghost tools at all.
func TestConfigOnLoadError(t *testing.T) {
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
	// The fallback must be config.DefaultConfig() itself, so the server and the
	// session hooks can never drift onto different values.
	if !cfg.Embedding.Enabled || cfg.Scratch.MaxBytes != config.DefaultScratchMaxBytes ||
		cfg.Linking.DemotionThreshold != 0.90 || cfg.Injection.BehaviorFloor != 8 {
		t.Errorf("warnOnConfig fallback = %+v, want the compiled defaults", cfg)
	}
}
