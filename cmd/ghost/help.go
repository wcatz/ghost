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

// helpValueFlagsByCommand lists, per subcommand path, every flag THAT command's
// parser consumes the following argument for. The help scan has to know them
// because the token after one of these is a VALUE, never a flag: a project may
// be named "-h" — dash-leading names travel through the verbatim --project form
// — so `ghost reflect --project -h` has to run reflect for that project instead
// of printing usage.
//
// The keys are the same subcommand paths as usageByCommand, and the table is
// per command rather than one list for the whole CLI because a flag that only
// some commands take cannot be assumed by the rest. `ghost upgrade --cwd -h` is
// the case that forced it: --cwd belongs to `ghost context`, upgrade has never
// heard of it and errors on it, but a global list swallowed the -h as --cwd's
// value and the upgrade ran — with the help flag the user typed, on a command
// that replaces the running binary. A command that is not a key here takes no
// value flags, so a stray --flag in front of -h is a typo the user should see
// rather than a value. A command's own flags are the mirror of its parser, and
// TestHelp_ValueFlagsCoverEveryParser holds the two to each other in both
// directions, per command.
//
// A flag that takes TWO operands is registered like one that takes a single
// value, and the scan skips one token after it — the most it can skip. So
// `ghost supersede --withdraw <id> -h` prints usage rather than reading the help
// token as the pair's second id, and `ghost supersede --withdraw -h` reaches the
// parser, which reports the missing operands. Both are the same contract as a
// project named "-h".
var helpValueFlagsByCommand = map[string]map[string]bool{
	"backup":                    {"--out": true},
	"context":                   {"--cwd": true, "--as-of": true},
	"export":                    {"--out": true, "--project": true},
	"history":                   {"--limit": true},
	"hook":                      {"--source": true},
	"lifecycle":                 {"--project": true, "--source": true},
	"mcp init":                  {"--client": true},
	"mcp status":                {"--client": true},
	"obsidian":                  {"--out": true, "--project": true, "--interval": true},
	"opencode cleanup-sessions": {"--grace": true, "--limit": true},
	"reflect":                   {"--project": true, "--source": true, "--tier": true},
	"resolve":                   {"--project": true, "--source": true, "--only": true, "--only-file": true, "--mark": true, "--mark-file": true},
	"supersede":                 {"--project": true, "--source": true, "--threshold": true, "--withdraw": true},
}

// isHelpToken reports whether arg is one of the two spellings of a help request.
// One predicate, because the scan and `ghost help` have to agree on what a help
// request looks like: the second reading "help" as a command name to report as
// missing, which is a lie about a token the reader typed on purpose.
func isHelpToken(arg string) bool {
	return arg == "-h" || arg == "--help"
}

// wantsHelp reports whether args — one subcommand's own arguments, without the
// command words — request that command's usage via -h or --help. command is the
// subcommand path usageFor resolved, and it selects which value flags apply.
//
// A bare "--" ends the options, so a flag-shaped token after it is an operand
// and not a help request: `ghost reflect -- --help` runs reflect rather than
// answering a question the reader did not ask. What happens to that operand is
// the command's own parser's business and is NOT the same as a help request —
// none of the parsers here implements "--" (some reject it as an unknown flag,
// some drop it and end up with no project), and a name that looks like a flag is
// still addressed with the verbatim --project form above. What the scan owes the
// reader is only that it does not answer for a command: a help request that
// reached the dispatch was a side effect, and the command that ran would not be
// the one the reader asked about.
func wantsHelp(command string, args []string) bool {
	valueFlags := helpValueFlagsByCommand[command]
	for i := 0; i < len(args); i++ {
		switch {
		case isHelpToken(args[i]):
			return true
		case args[i] == "--":
			return false
		case valueFlags[args[i]]:
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
	"mcp":                       mcpUsage,
	"mcp init":                  mcpInitUsage,
	"mcp status":                mcpStatusUsage,
	"hook":                      hookUsage,
	"reflect":                   reflectUsage,
	"supersede":                 supersedeUsage,
	"resolve":                   resolveUsage,
	"lifecycle":                 lifecycleUsage,
	"obsidian":                  obsidianUsage,
	"opencode":                  opencodeUsage,
	"opencode cleanup-sessions": opencodeUsage,
	"backup":                    backupUsage,
	"backup verify":             backupVerifyUsage,
	"export":                    exportUsage,
	"history":                   historyUsage,
	"import":                    importUsage,
	"bench":                     benchUsage,
	"upgrade":                   upgradeUsage,
	"context":                   contextUsage,
	"maintenance":               maintenanceUsage,
	"project":                   projectUsage,
	"project delete":            projectDeleteUsage,
	"project merge":             projectMergeUsage,
	"project bind":              projectBindUsage,
	"version":                   versionUsage,
}

// usageFor resolves argv's registered subcommand path, returning that path, its
// usage, and the arguments that follow the path. ok is false when argv opens
// with no registered command — an unknown command, or the top-level -h/--help,
// which main's own case prints. The path comes back because the help scan needs
// it: which flags take a value is a property of the command, not of the CLI.
func usageFor(argv []string) (path, usage string, rest []string, ok bool) {
	for n := len(argv); n >= 1; n-- {
		p := strings.Join(argv[:n], " ")
		if u, found := usageByCommand[p]; found {
			return p, u, argv[n:], true
		}
	}
	return "", "", nil, false
}

// handleHelp answers -h/--help for every registered subcommand and reports
// whether argv was a help request: true means the usage is already on stdout
// and the caller must exit 0 without dispatching; false means dispatch
// proceeds exactly as it did before this existed.
func handleHelp(argv []string) bool {
	command, usage, rest, ok := usageFor(argv)
	if !ok || !wantsHelp(command, rest) {
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

// runHelpCommand answers `ghost help [command]`, and the same dispatch case
// answers `ghost -h [command]`. With a command, it prints that command's own
// usage — the same text `ghost <command> -h` prints, on stdout, because the
// reader asked a question and the command is a subcommand's rather than the
// CLI's. `ghost help` with no command, a help token in place of one, and any
// name that is not a registered command path, all print the top-level summary on
// stderr exactly as before; a name that matched nothing is reported there first,
// so a typo is visible rather than answered with a list that does not contain
// it, while a help token is not reported at all — `ghost help -h` is one
// question asked twice, not a misspelling. Every leading help token is dropped
// before the lookup rather than just the first, so `ghost help -h upgrade` names
// the command exactly as `ghost -h upgrade` does, `ghost help -h -h` is still
// only a question, and what is left in the name position is a name a reader
// typed. The exit code stays 0 throughout: a question with no answer is not a
// mistake in the invocation.
func runHelpCommand(args []string) int {
	for len(args) > 0 && isHelpToken(args[0]) {
		args = args[1:]
	}
	if _, usage, _, ok := usageFor(args); ok {
		if _, err := fmt.Fprint(os.Stdout, usage); err != nil {
			fmt.Fprintf(os.Stderr, "warning: cannot print usage: %v\n", err)
		}
		return 0
	}
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "ghost help: no command %q in this build; showing the command list\n", strings.Join(args, " "))
	}
	printUsage()
	return 0
}
