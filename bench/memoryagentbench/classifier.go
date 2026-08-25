package main

import (
	"fmt"
	"os/exec"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/supersede"
)

// buildClassifier builds the real supersede classifier backed by the
// `opencode` CLI (ai.NewOpenCodeClientWithBinary) — subscription-billed, no
// ANTHROPIC_API_KEY required. cfg.CLI.OpenCodeBinary overrides the "opencode"
// PATH lookup, matching cmd/ghost/main.go's own opencode-tier resolution.
func buildClassifier() (*supersede.HaikuClassifier, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	binary := "opencode"
	if cfg.CLI.OpenCodeBinary != "" {
		binary = cfg.CLI.OpenCodeBinary
	}
	if _, err := exec.LookPath(binary); err != nil {
		hint := "set cli.opencode_binary in ~/.config/ghost/config.yaml"
		if cfg.CLI.OpenCodeBinary != "" {
			hint = fmt.Sprintf("check that cli.opencode_binary (%s) is a valid, executable path", binary)
		}
		return nil, fmt.Errorf("memoryagentbench requires the `%s` binary on PATH (or %s): %w", binary, hint, err)
	}
	provider := ai.NewFallbackProvider(ai.NewOpenCodeClientWithBinary(binary), nil, false)
	return supersede.NewHaikuClassifier(provider), nil
}
