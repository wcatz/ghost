package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/mcpinit"
)

// contextUsage is the help for `ghost context`: stdout for -h/--help (see
// handleHelp), so a help request never runs the SessionStart side effects the
// context block mirrors (Obsidian sync, session-count bump).
const contextUsage = `Usage: ghost context [--cwd <dir>] [--as-of <RFC3339>]

Prints the passive session-start context block for a directory. This is what
the opencode adapter injects as instructions, because opencode does not
consume a stdout hook response.

--as-of <RFC3339> prints the block as the store stood at that instant instead
(issue #647): the wording each memory held then, including memories deleted
since, and without memories that did not exist yet. Tasks, decisions and
learned context are not versioned, so they are omitted rather than shown as
they are now, and no session is counted — a past reading is a diagnostic, not
a session start.
`

// contextAsOf reads the --as-of instant out of a `ghost context` argument list.
// The second result is false when the flag was absent, which is a current read
// and not an error; an unreadable value IS an error, and it is reported here
// rather than ignored because the alternative is answering a question about a
// past instant with the present.
//
// It is a function so the parser can be tested without running the command: the
// bad value ends in os.Exit(2), and a test that drove that would have to take the
// process with it.
func contextAsOf(args []string) (*time.Time, error) {
	raw := ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--as-of" && i+1 < len(args):
			raw = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--as-of="):
			raw = strings.TrimPrefix(args[i], "--as-of=")
		}
	}
	if raw == "" {
		return nil, nil
	}
	instant, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, fmt.Errorf("--as-of %q is not an RFC 3339 instant (e.g. 2026-09-20T09:00:00Z): %w", raw, err)
	}
	utc := instant.UTC()
	return &utc, nil
}

// runContext prints the passive session-start context block for a directory,
// backing opencode's plugin-injected instructions. It mirrors the SessionStart
// hook's startup side effects (Obsidian sync when configured, session-count
// bump) so opencode's injected context stays at parity with claude/codex.
// `ghost context` is the opencode-appropriate alternative to the SessionStart
// hook, which opencode cannot consume (no stdout-injection surface). See
// RenderSessionContext.
//
// --as-of turns it into a historical read, and that path runs none of the side
// effects above: they exist because this command backs a session start, and a
// request about the past is not one. The instant is parsed by contextAsOf so an
// unreadable value is refused at the boundary, with the argument in the message
// and a non-zero status, rather than rendering the present under a request that
// asked for a past instant.
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
	asOf, err := contextAsOf(os.Args[2:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "ghost context: %v\n", err)
		os.Exit(2)
	}
	if asOf == nil {
		fmt.Println(mcpinit.RenderSessionContext(cwd))
		return
	}
	fmt.Println(mcpinit.RenderSessionContextAt(cwd, asOf))
}

// hookUsage is the help for `ghost hook`: stdout for -h/--help (see
// handleHelp) — before the fail-open path below, which answers an
// unrecognized invocation rather than a question.
const hookUsage = `Usage: ghost hook <event> --source <host>

Lifecycle hook for MCP clients: events session-start, stop, session-end;
sources claude-code, opencode, codex, goose. Normally called by a client
adapter, not by hand. A missing or unknown source fails open with one
diagnostic line and exit status 0 — re-run ghost mcp init to repair wiring.
`

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
