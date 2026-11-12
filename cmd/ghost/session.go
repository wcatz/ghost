package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/hostevent"
	"github.com/wcatz/ghost/internal/mcpinit"
	"github.com/wcatz/ghost/internal/memory"
)

const contextUsage = `Usage: ghost context [--cwd <dir>] [--as-of <RFC3339>] [--source <host>]

Prints the passive session-start context block for a directory. This is what
the opencode adapter injects as instructions, because opencode does not
consume a stdout hook response. It is a session start: it counts a session
for the project and starts the Obsidian mirror when that is configured.

  --cwd <dir>             The directory to resolve (default: your own; an
                          empty value means your own too)
  --as-of <RFC3339>       Print the block as the store stood at an instant
  --source <host>         The host the block is for (claude-code, codex, goose,
                          opencode): rows that repeat the instruction files
                          that host loads itself are left out

This mode takes those three flags and nothing else. Anything else is refused
with exit 1 before the block is rendered, so a refusal counts no session:
an unknown flag, a stray argument, --cwd with no value after it, an unknown
--source, and --cwd, --as-of or --source given twice. --source has no effect
with --as-of.

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
`

func contextAsOf(args []string) (*time.Time, error) {
	raw := ""
	seen := false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--as-of":
			seen = true
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
		return nil, fmt.Errorf("--as-of %s is not an RFC 3339 instant (e.g. 2026-09-20T09:00:00Z)", memory.RefusalArg("as-of", raw))
	}
	utc := instant.UTC()
	return &utc, nil
}

func parseContextBlockArgs(args []string) (string, *time.Time, error) {
	cwd := ""
	var seenCwd, seenAsOf, seenSource bool
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--cwd" || strings.HasPrefix(arg, "--cwd="):
			value := ""
			if arg == "--cwd" {
				if i+1 >= len(args) {
					return "", nil, errors.New("flag --cwd needs a value (a directory)")
				}
				i++
				value = args[i]
			} else {
				value = strings.TrimPrefix(arg, "--cwd=")
			}
			if seenCwd {
				return "", nil, fmt.Errorf("--cwd was given twice (%s and %s)",
					memory.RefusalArg("cwd", cwd), memory.RefusalArg("cwd", value))
			}
			seenCwd = true
			cwd = value
		case arg == "--as-of" || strings.HasPrefix(arg, "--as-of="):
			if seenAsOf {
				return "", nil, errors.New("--as-of was given twice: a block is read at one instant")
			}
			seenAsOf = true
			if arg == "--as-of" && i+1 < len(args) {
				i++
			}
		case arg == "--source" || strings.HasPrefix(arg, "--source="):
			if seenSource {
				return "", nil, errors.New("--source was given twice: a block is for one host")
			}
			seenSource = true
			if arg == "--source" && i+1 < len(args) {
				i++
			}
		case strings.HasPrefix(arg, "-"):
			return "", nil, fmt.Errorf("unknown flag %s", memory.RefusalArg("flag", arg))
		default:
			return "", nil, fmt.Errorf("the session-start block takes flags only, got %s", memory.RefusalArg("argument", arg))
		}
	}
	asOf, err := contextAsOf(args)
	if err != nil {
		return "", nil, err
	}
	return cwd, asOf, nil
}

func contextSource(args []string) (hostevent.Source, error) {
	raw, seen := "", false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--source":
			if i+1 >= len(args) {
				return "", errors.New("--source requires a host (claude-code, codex, goose or opencode)")
			}
			raw, seen = args[i+1], true
			i++
		case strings.HasPrefix(args[i], "--source="):
			raw, seen = strings.TrimPrefix(args[i], "--source="), true
		}
	}
	if !seen {
		return "", nil
	}
	src := hostevent.Source(raw)
	if _, ok := hostevent.CapabilityFor(src); !ok {
		return "", fmt.Errorf("--source %s is not a known host (claude-code, codex, goose or opencode)", memory.RefusalArg("source", raw))
	}
	return src, nil
}

func runContext() int {
	args := os.Args[2:]
	cwd, asOf, err := parseContextBlockArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ghost context: %v\n", err)
		return 1
	}
	src, err := contextSource(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ghost context: %v\n", err)
		return 1
	}
	if asOf == nil {
		fmt.Println(mcpinit.RenderSessionContextFor(cwd, src))
		return 0
	}
	fmt.Println(mcpinit.RenderSessionContextAt(cwd, asOf))
	return 0
}

const hookUsage = `Usage: ghost hook <event> --source <host>

Lifecycle hook for MCP clients: events session-start, stop, session-end,
message-submit and edit (claude-code only: a floored memory block beside a user
message or an edit's result; silent when nothing matches);
sources claude-code, opencode, codex, goose. Normally called by a client
adapter, not by hand. A missing or unknown source fails open with one
diagnostic line and exit status 0 — re-run ghost mcp init to repair wiring.
`

func runHook() {
	event := ""
	var source string
	if len(os.Args) >= 3 {
		event = os.Args[2]
		for i := 3; i < len(os.Args); i++ {
			switch {
			case os.Args[i] == "--source" && i+1 < len(os.Args):
				source = os.Args[i+1]
				i++
			case strings.HasPrefix(os.Args[i], "--source="):
				source = strings.TrimPrefix(os.Args[i], "--source=")
			}
		}
	}
	if source == "" {
		fmt.Fprintln(os.Stderr, "ghost hook: fail-open (missing --source; re-run `ghost mcp init` to migrate hook wiring)")
		os.Exit(0)
	}
	mcpinit.RunHostEvent(event, source, os.Stdin, os.Stdout, os.Stderr)
}
