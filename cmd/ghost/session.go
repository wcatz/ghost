package main

import (
	"errors"
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
       ghost context --audit [--project <name-or-id>] [--since <duration>]

Prints the passive session-start context block for a directory. This is what
the opencode adapter injects as instructions, because opencode does not
consume a stdout hook response.

--as-of <RFC3339> prints the block as the store stood at that instant instead
(issue #647): the wording each memory held then, including memories deleted
since, and without memories that did not exist yet. Tasks, decisions and
learned context are not versioned, so they are omitted rather than shown as
they are now; a memory row's tags, scope, pin, confidence, agent, source_ref
and validity window are not versioned either, so they are the current row's.
A row's validity window is judged at the instant, as this block has since #899:
a memory whose window had closed or not yet opened then is left out, and one
valid then is shown as valid even if its window has closed since.
ghost_memory_search with as_of judges no window at all, because a bound read
from the current row is not evidence about the instant; it shows the bounds
instead and says they are the current row's.
No session is counted — a past reading is a diagnostic, not a session start.

--audit reports on what retrieval ACTUALLY did (issue #646). It is the other
mode of this command and shares almost nothing with the block above: it opens
the store read-only, runs none of the side effects (no Obsidian sync, no
session count), and prints per-source figures — calls, memories kept, used,
ignored, superseded in session, contradicted, and searches that kept nothing.

  --project <name-or-id>  The project to report on (default: the one this
                          directory resolves to, via --cwd or your own)
  --since <duration>      How far back to look, by the store's own recorded_at
                          (Go duration: 168h, 7d is not a unit; default:
                          everything the store still holds)

The report is ALWAYS about one project. A name, an id or a directory that
resolves to nothing is refused and says so, naming --project, because a
mistyped project answered with every project's figures would be a report about
a scope nobody asked for. Figures are per source and are never pooled — a
search and an injection answer different questions — and a source with no rows
says so rather than reporting 0%. The percentage is the share of verdicts
proved by a cited memory id; a match on wording alone is printed beside it as a
heuristic. "ignored" means the agent's own words never mentioned the memory, and
"restated by wording" is a token-overlap heuristic; neither is a relevance or
usefulness score. One half of
"missed" (a fact the agent re-derived and was never shown) is not measured by
anything and is reported as no figure, not as a zero.

--as-of and --audit cannot be combined: one reports the store at an instant,
the other reports retrieval over a window.
`

// contextAsOf reads the --as-of instant out of a `ghost context` argument list.
// The second result is false when the flag was absent, which is a current read
// and not an error; an unreadable value IS an error, and it is reported here
// rather than ignored because the alternative is answering a question about a
// past instant with the present.
//
// A flag with no value after it is an error too, and the distinction is the whole
// point of parsing it here: a dropped --as-of is indistinguishable from a current
// read, so `ghost context --as-of` would render a LIVE session-start block — the
// Obsidian mirror and the session-count bump included — for a request that asked
// about the past. The count is the visible damage; the silent substitution is the
// real one.
//
// It is a function so the parser can be tested without running the command: the
// bad value ends in os.Exit(2), and a test that drove that would have to take the
// process with it.
func contextAsOf(args []string) (*time.Time, error) {
	raw := ""
	seen := false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--as-of":
			seen = true
			// The value is the next token whatever it looks like. Taking it
			// unconditionally and letting time.Parse reject it is deliberate: a
			// guard that only accepts a token which does not look like a flag
			// would have to guess, and an instant that fails to parse is a better
			// answer than a flag silently consumed.
			if i+1 >= len(args) {
				return nil, errors.New("--as-of requires an RFC 3339 instant (e.g. 2026-09-20T09:00:00Z)")
			}
			raw = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--as-of="):
			seen = true
			raw = strings.TrimPrefix(args[i], "--as-of=")
		}
	}
	if !seen {
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
//
// --audit is the other mode, and it is the opposite in every respect: read-only,
// no side effects, and a report of retrieval verdicts rather than a block of
// memories. It is dispatched FIRST and on the flag alone, so an audit run never
// reaches the side effects below — those exist because this command backs a
// session start, and a report is not one. See context_audit.go.
func runContext() {
	args := os.Args[2:]
	if contextAuditRequested(args) {
		runContextAudit(args)
		return
	}
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

Lifecycle hook for MCP clients: events session-start, stop, session-end,
message-submit and edit (claude-code only: a floored memory block beside a user
message or an edit's result; silent when nothing matches);
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
