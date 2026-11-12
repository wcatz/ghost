package main

import (
	"fmt"
	"os/exec"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/supersede"
)

func buildClassifier() (*supersede.RelationClassifier, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	binary := "opencode"
	if cfg.CLI.OpenCodeBinary != "" {
		binary = cfg.CLI.OpenCodeBinary
	}
	if _, err := exec.LookPath(binary); err != nil {
		hint := "set cli.opencode_binary in the Ghost user config file"
		if cfg.CLI.OpenCodeBinary != "" {
			hint = fmt.Sprintf("check that cli.opencode_binary (%s) is a valid, executable path", binary)
		}
		return nil, fmt.Errorf("memoryagentbench requires the `%s` binary on PATH (or %s): %w", binary, hint, err)
	}
	return supersede.NewRelationClassifier(ai.NewOpenCodeClientWithBinary(binary)), nil
}
