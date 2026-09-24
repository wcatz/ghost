package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/wcatz/ghost/internal/mcpinit"
)

// runContext prints the passive session-start context block for a directory,
// backing opencode's plugin-injected instructions. It mirrors the SessionStart
// hook's startup side effects (Obsidian sync when configured, session-count
// bump) so opencode's injected context stays at parity with claude/codex.
// `ghost context` is the opencode-appropriate alternative to the SessionStart
// hook, which opencode cannot consume (no stdout-injection surface). See
// RenderSessionContext.
func runContext() {
	cwd := ""
	for i := 2; i < len(os.Args); i++ {
		switch {
		case os.Args[i] == "--cwd" && i+1 < len(os.Args):
			cwd = os.Args[i+1]
			i++
		case strings.HasPrefix(os.Args[i], "--cwd="):
			cwd = strings.TrimPrefix(os.Args[i], "--cwd=")
		}
	}
	fmt.Println(mcpinit.RenderSessionContext(cwd))
}

// runHook dispatches host lifecycle events per the contract-v1 spec:
//
//	ghost hook <event> --source <host>
//
// The contract has no legacy mode: without --source nothing can be routed, so
// the invocation fails open (one stderr line, exit 0) with a pointer to
// `ghost mcp init`, which migrates pre-contract wiring idempotently. Every
// event — including unrecognized ones — goes through RunHostEvent so
// validation and its fail-open diagnostics live in exactly one place.
func runHook() {
	if len(os.Args) < 3 {
		os.Exit(0)
	}
	event := os.Args[2]
	var source string
	for i := 3; i < len(os.Args); i++ {
		switch {
		case os.Args[i] == "--source" && i+1 < len(os.Args):
			source = os.Args[i+1]
			i++
		case strings.HasPrefix(os.Args[i], "--source="):
			source = strings.TrimPrefix(os.Args[i], "--source=")
		}
	}
	if source == "" {
		fmt.Fprintln(os.Stderr, "ghost hook: fail-open (missing --source; re-run `ghost mcp init` to migrate hook wiring)")
		os.Exit(0)
	}
	mcpinit.RunHostEvent(event, source, os.Stdin, os.Stdout, os.Stderr)
}
