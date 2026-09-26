package main

import (
	"fmt"
	"os"
	"strings"
)

// The CLI-wide -h/--help contract (#630): a help request must never have a
// side effect. `ghost mcp init --help` used to run the full installer for
// every detected client, `ghost upgrade --help` would have contacted GitHub
// Releases and replaced the running binary, and `ghost resolve --help` failed
// with an unknown-flag error. handleHelp is the one gate: runCLI consults it
// before the command dispatch, so help can never load configuration, open the
// database, write a file or spawn a harness. One dispatch point instead of a
// check inside each run* keeps a newly added subcommand from silently missing
// the contract — register it in usageByCommand and it inherits the behaviour.

// helpValueFlags lists every flag in the CLI that consumes the following
// argument. The help scan has to know them because the token after one of
// these is a VALUE, never a flag: a project may be named "-h" — dash-leading
// names travel through the verbatim --project form — so
// `ghost reflect --project -h` has to run reflect for that project instead of
// printing usage.
var helpValueFlags = map[string]bool{
	"--client":    true,
	"--cwd":       true,
	"--interval":  true,
	"--out":       true,
	"--project":   true,
	"--source":    true,
	"--threshold": true,
	"--tier":      true,
}

// wantsHelp reports whether args — one subcommand's own arguments, without
// the command words — request that command's usage via -h or --help.
func wantsHelp(args []string) bool {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-h" || args[i] == "--help":
			return true
		case helpValueFlags[args[i]]:
			i++ // the next token is this flag's value, not a flag of its own
		}
	}
	return false
}

// usageByCommand maps a subcommand path to the usage text -h/--help prints.
// The longest registered path at the front of argv wins, so "project delete"
// beats "project" and every word after the path is the command's own
// arguments. Each text lives next to the command it documents (a const beside
// its run* function), exactly as projectBindUsage did before it, so a usage
// block cannot drift from the flags printed under it.
var usageByCommand = map[string]string{
	"mcp":            mcpUsage,
	"mcp init":       mcpInitUsage,
	"mcp status":     mcpStatusUsage,
	"hook":           hookUsage,
	"reflect":        reflectUsage,
	"supersede":      supersedeUsage,
	"resolve":        resolveUsage,
	"lifecycle":      lifecycleUsage,
	"obsidian":       obsidianUsage,
	"bench":          benchUsage,
	"upgrade":        upgradeUsage,
	"context":        contextUsage,
	"maintenance":    maintenanceUsage,
	"project":        projectUsage,
	"project delete": projectDeleteUsage,
	"project merge":  projectMergeUsage,
	"project bind":   projectBindUsage,
	"version":        versionUsage,
}

// usageFor resolves argv's registered subcommand path, returning that
// command's usage and the arguments that follow the path. ok is false when
// argv opens with no registered command — an unknown command, or the
// top-level -h/--help, which main's own case prints.
func usageFor(argv []string) (usage string, rest []string, ok bool) {
	for n := len(argv); n >= 1; n-- {
		if u, found := usageByCommand[strings.Join(argv[:n], " ")]; found {
			return u, argv[n:], true
		}
	}
	return "", nil, false
}

// handleHelp answers -h/--help for every registered subcommand and reports
// whether argv was a help request: true means the usage is already on stdout
// and the caller must exit 0 without dispatching; false means dispatch
// proceeds exactly as it did before this existed.
func handleHelp(argv []string) bool {
	usage, rest, ok := usageFor(argv)
	if !ok || !wantsHelp(rest) {
		return false
	}
	// Reported rather than discarded, the `project bind` precedent (#612):
	// help that silently failed to print is indistinguishable from a command
	// that took no arguments. The exit code stays 0 either way — the user
	// asked a question, they did not make a mistake.
	if _, err := fmt.Fprint(os.Stdout, usage); err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot print usage: %v\n", err)
	}
	return true
}
