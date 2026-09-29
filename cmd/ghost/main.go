package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/repo"
)

var version = "dev"

func main() {
	os.Exit(runCLI(os.Args[1:], dispatchCommand))
}

// runCLI answers -h/--help for argv before consulting the command dispatch,
// then defers to it, and returns the process exit code. Help first is the
// #630 contract: a help request that reached the dispatch would be the side
// effect it exists to prevent. The dispatch is a parameter so a test can
// assert it is never consulted. Commands keep reading os.Args themselves —
// argv is the same slice, minus the program name.
func runCLI(argv []string, dispatch func([]string) int) int {
	if handleHelp(argv) {
		return 0
	}
	return dispatch(argv)
}

// commandGroup is a command that dispatches on a subcommand word. A command
// that takes an OPERAND instead — `ghost history <memory-id>`, `ghost reflect
// <project>`, `ghost import <file>` — is not a group: the word after it is the
// data the command was asked about, and its own parser already reports one it
// cannot use ("a memory id is required", "unknown flag %q"). Only a group has a
// position where a word can be a command, and therefore a position where a
// mistyped one can be told apart from an operand.
type commandGroup struct {
	// path is the usage-table path this group is dispatched on ("mcp").
	path string
	// subs are the subcommand words it dispatches on. Every one of them is
	// either a path usageByCommand registers under path ("mcp init") or named
	// in path's own usage text (obsidian registers one usage for both of its
	// modes), which TestCommandGroupsMatchTheUsageTable holds it to.
	subs []string
	// defaultAction marks a group whose bare invocation runs its own action
	// instead of reporting a missing subcommand. There are two, and each is one
	// because its bare form is an invocation a user is told to type: `ghost mcp`
	// starts the MCP server, which is what every client spawns, and
	// `ghost backup` takes the snapshot. The field is why "no subcommand given"
	// is not a blanket rule.
	defaultAction bool
}

// commandGroups is every command that dispatches on a subcommand word, in the
// order the usage table documents them. It is the table the unknown-subcommand
// test drives, so a new group registered in the usage table has to be added
// here — and adding it here without routing an unknown word in the dispatch
// fails that test rather than reproducing #691 for the new group.
var commandGroups = []commandGroup{
	{path: "mcp", subs: []string{"init", "status"}, defaultAction: true},
	{path: "backup", subs: []string{"verify"}, defaultAction: true},
	{path: "maintenance", subs: []string{"status", "clean-scratch"}},
	{path: "obsidian", subs: []string{"export", "sync"}},
	{path: "opencode", subs: []string{"cleanup-sessions"}},
	{path: "project", subs: []string{"bind", "delete", "merge"}},
}

// backupSubcommand classifies the word after `ghost backup`, the one place a
// group's subcommand word and its command's own flags share a position.
//
// It returns routable=false only for a word that is neither the subcommand nor a
// flag: `ghost backup veriy` is a command line that cannot be acted on, and the
// caller answers it with the usage error that names the word, rather than with
// the backup's own "unknown argument" — which would report a flag problem for a
// mistyped verb.
//
// A flag-shaped word is the command's own and is always routable. That is the
// case every other group gets for free because they have no flags: `ghost backup
// --out x` is the common invocation, and reading `--out` as a subcommand would
// turn a working command into a usage error over its most-used flag.
//
// An EMPTY word is given rather than absent, and is refused rather than taken for
// the bare invocation, for the reason the mcp group refuses the same shape: a
// wrapper running `ghost backup "$SUB"` with an unset $SUB passes one, and
// answering that with a backup over a path nobody chose would be the exit-0
// failure #691 is about.
//
// It is a function rather than three lines inline because both branches end in a
// run* that exits the process, so a test cannot reach them through the dispatch —
// and the decision they make is the whole of what `ghost backup …` is for.
//
// Note that the unroutable word is reported by the group, not by the backup's own
// parser. `parseBackupArgs` would also reject it, as an unknown argument, but its
// message names a flag and the reader mistyped a verb.
func backupSubcommand(word string, given bool) (sub string, routable bool) {
	switch {
	case word == "verify":
		return word, true
	case given && !strings.HasPrefix(word, "-"):
		return word, false
	default:
		return "", true
	}
}

// exitUsage is the exit code for a command line the CLI cannot act on: an
// unrecognised command word, or a command group invoked with no subcommand (or
// with one that is not one of its subcommands). 2 is the conventional code for
// a usage error, and it is distinct from the 1 every command here uses for a
// run that started and then failed — a mistyped subcommand and a database that
// will not open are different answers to a script, and #691 was a mistyped one
// that answered like neither. A help request is not a usage error and stays 0.
const exitUsage = 2

// applyBuildVersion hands this binary's version to the config package, which is
// where the GHOST_DEV_FORBID_DATA_DIR refusal is decided (#721). It is a named
// function because the wiring is a fact worth asserting: a guard applied with a
// different version than the binary reports is a guard nobody can reason about.
func applyBuildVersion() { config.SetBuildVersion(version) }

// dispatchCommand routes one invocation to its subcommand and returns the
// exit code. Subcommands that exit on their own (every failure path) never
// return here. A command line this cannot route is not one of them: it ends in
// usageError and exitUsage, at whichever level the word that matched nothing was
// typed (#691) — an unrecognised command, an unrecognised or missing
// subcommand, or no command at all.
func dispatchCommand(argv []string) int {
	// Repository identity needs git, which internal/memory deliberately never
	// invokes — the capability is injected so the store stays a pure storage
	// layer, tests can pin it, and a store built without one resolves exactly
	// as it did before. Wired once here because dispatch covers the MCP
	// server, the lifecycle hooks and every CLI subcommand, so a single line
	// covers the whole binary.
	memory.SetDetectRemote(repo.DetectRemote)
	// The build version, for the same reason and by the same route: config's
	// data-directory resolvers decide whether this is a release build, and every
	// path into the data directory reaches them from here — the CLI, the MCP
	// server, the lifecycle hooks (#721). One line covers the whole binary, and
	// the value is the same `version` the binary prints.
	applyBuildVersion()

	if len(argv) > 0 {
		switch argv[0] {
		case "-v", "--version", "version":
			fmt.Printf("ghost %s\n", version)
			return 0
		case "help", "--help", "-h":
			return runHelpCommand(argv[1:])
		case "mcp":
			// Presence, not a non-empty word: `ghost mcp "$SUB"` with an unset
			// $SUB passes an EMPTY word, and taking the bare invocation for it
			// starts the server — a typo that produces a long-lived process,
			// which is the exit-0 shape #691 is about. subArg reports both.
			if sub, given := subArg(argv, 1); given {
				switch sub {
				case "init":
					runMCPInit()
					return 0
				case "status":
					runMCPStatus()
					return 0
				}
				return usageError("mcp", sub, true, mcpUsage)
			}
			// Bare `ghost mcp` is the server, which is what every client spawns:
			// the one command group whose bare invocation runs its own action
			// (commandGroups.defaultAction).
			runMCP()
			return 0
		case "hook":
			runHook()
			return 0
		case "reflect":
			runReflect()
			return 0
		case "supersede":
			runSupersede()
			return 0
		case "resolve":
			runResolve()
			return 0
		case "lifecycle":
			runLifecycle()
			return 0
		case "project":
			sub, given := subArg(argv, 1)
			switch sub {
			case "delete":
				runProjectDelete()
				return 0
			case "merge":
				runProjectMerge()
				return 0
			case "bind":
				runProjectBind()
				return 0
			}
			return usageError("project", sub, given, projectUsage)
		case "upgrade":
			runUpgrade(argv[1:])
			return 0
		case "backup":
			sub, routable := backupSubcommand(subArg(argv, 1))
			switch {
			case !routable:
				return usageError("backup", sub, true, backupUsage)
			case sub == "verify":
				runBackupVerify(argv[2:])
				return 0
			}
			runBackup()
			return 0
		case "export":
			runExport()
			return 0
		case "history":
			runHistory()
			return 0
		case "prune":
			runPrune(argv[1:])
			return 0
		case "import":
			runImport()
			return 0
		case "obsidian":
			sub, given := subArg(argv, 1)
			switch sub {
			case "export", "sync":
				runObsidian(sub, argv[2:])
				return 0
			}
			return usageError("obsidian", sub, given, obsidianUsage)
		case "opencode":
			sub, given := subArg(argv, 1)
			switch sub {
			case "cleanup-sessions":
				runOpenCodeCleanupSessions(argv[2:])
				return 0
			}
			return usageError("opencode", sub, given, opencodeUsage)
		case "bench":
			runBench()
			return 0
		case "context":
			runContext()
			return 0
		case "maintenance":
			sub, given := subArg(argv, 1)
			switch sub {
			case "status":
				runMaintenanceStatus()
				return 0
			case "clean-scratch":
				runMaintenanceCleanScratch(argv[2:])
				return 0
			}
			return usageError("maintenance", sub, given, maintenanceUsage)
		}
	}
	// Nothing above matched the command word. `ghost` with no arguments at all
	// and `ghost <not-a-command>` are both command lines that cannot be acted
	// on, and both are answered with the command list a reader (or a script) has
	// always been sent to — plus exit 2, so a caller branching on the exit code
	// can tell a mistyped command from a successful run. That is the whole of
	// #691: the text was right, the code said it had succeeded. A help request
	// never arrives here, because runCLI answered it first (#630).
	if len(argv) == 0 {
		return usageError("", "", false, topLevelUsage())
	}
	return usageError("", argv[0], true, topLevelUsage())
}

// subArg returns argv[i] and whether a word was given there at all: the word
// after a command, which is a subcommand for a command group and an operand for
// everything else. One accessor so every group reads its subcommand — and the
// difference between "none was given" and "that one does not exist" — from the
// same place.
//
// The two are reported separately because an EMPTY word is given, not absent:
// a wrapper running `ghost mcp "$SUB"` with an unset $SUB passes one, and
// treating it as no word at all starts the MCP server. The word is returned
// unquoted here; usageError quotes it.
func subArg(argv []string, i int) (word string, given bool) {
	if i < len(argv) {
		return argv[i], true
	}
	return "", false
}

// usageError reports a command line the CLI cannot act on and returns
// exitUsage: the diagnostic naming the mistake, then the usage of the level the
// word was typed at, both on stderr. level is the command group the word
// followed, or "" for the CLI itself; name is the word that matched no
// subcommand, and given says whether one was given at all. The two cases are
// different mistakes — a word that is not a subcommand and no word — and saying
// which is the reason the diagnostic leads.
//
// Nothing goes to stdout: that is where a subcommand's own help lands (#630),
// so a caller reading a command's output sees an empty stream rather than the
// command list where its results should have been.
func usageError(level, name string, given bool, usage string) int {
	where := "ghost"
	if level != "" {
		where += " " + level
	}
	switch {
	case given:
		fmt.Fprintf(os.Stderr, "%s: unknown command: %q\n\n", where, name)
	case level != "":
		fmt.Fprintf(os.Stderr, "%s: no subcommand given\n\n", where)
	default:
		fmt.Fprintf(os.Stderr, "%s: no command given\n\n", where)
	}
	fmt.Fprint(os.Stderr, usage)
	return exitUsage
}

// versionUsage is `ghost version`'s help. It goes to stdout for -h/--help
// like every other subcommand's usage (see handleHelp).
const versionUsage = `Usage: ghost version

Prints the binary version.
`

// topLevelUsage is the CLI's own usage: the command list. `ghost help` and the
// top-level -h/--help print it on stderr, where it has always gone, and a
// command line that cannot be acted on is answered with the same text through
// usageError — so a reader who mistyped a command is sent to the same list
// whether they asked for it or not.
func topLevelUsage() string {
	return fmt.Sprintf(`ghost %s — MCP memory server for Claude Code

Usage:
  ghost <command>

Commands:
  mcp                         Start MCP server on stdio (used by Claude Code)
  mcp init [--client claude|opencode|codex|goose|all] [--dry-run]  Configure MCP client integration
                                                         (auto-detects when --client is omitted)
  mcp status [--client claude|opencode|codex|goose]                Check MCP client integration health
  hook <event> [--source <host>]  Lifecycle hook for MCP clients (session-start, stop, session-end)
  reflect <project> [flags]   Memory consolidation (dry-run by default, --apply to save)
  supersede <project> [flags] Link superseded memories (dry-run by default, --apply to write)
  resolve <project> [flags]   Mark resolved evidence memories (dry-run by default, --apply to write)
  project delete <name> [flags]  Permanently delete a project and everything under it
                              (dry-run by default, --apply + name re-type to confirm)
  project merge <old> <new>   Merge one project into another; child records move to the
                              survivor with memory IDs, links, and pin state preserved
  project bind <id> <path>    Record a checkout for a project so a session in that
                              directory resolves it (ghost mcp status reports
                              projects that have no usable path and no remote)
  obsidian export [flags]     Mirror memories to an Obsidian vault (one-way)
  obsidian sync [flags]       Keep the vault mirror fresh (polls for DB changes)
  context [--cwd <dir>]       Print the passive session-start context block (for opencode)
  history <memory-id> [--limit N] [--json]
                              Print one memory's append-only history: every write,
                              what it changed, and which phase or agent made it
                              (survives the memory itself)
  prune [--project <name>] [--grace 168h] [--apply]
                              Remove expired session-tier memories — nothing
                              durable, and a persistent row never. Dry-run by
                              default; never run for you, no lifecycle pass or
                              hook calls it, and each removal is recorded in
                              memory_history as a delete
  maintenance status          Show live scratch usage and recent hygiene runs
  maintenance clean-scratch   Report pre-scratch-root legacy debris
                              (dry-run by default, --apply to remove strict matches)
  opencode cleanup-sessions [flags]
                              One-shot cleanup of lifecycle sessions titled
                              exactly "[ghost]" (dry-run by default; --apply
                              to delete, --grace 1h, --limit 20000)
  bench [--sweep]             Run the retrieval-quality benchmark (built-in dataset);
                              --sweep grid-searches the fusion parameters
  backup [--out <path>]       Snapshot the live database — consistent, safe
                               while the MCP server runs; prints path, row
                               counts and the sidecar manifest it wrote
  backup verify <file>        Check a backup before restoring it: sha256 against
                               its manifest, SQLite's integrity_check, the schema
                               version, and the recorded row counts
  export [--project <name>]   Write memories, tasks, decisions and projects as JSONL
    [--out <file.jsonl>]      (--out - for stdout; embeddings are not exported)
  import <file.jsonl> [--apply] [--trust-provenance]
                              Load a JSONL artifact (dry-run by default over a
                               read-only connection; never overwrites a record
                               whose id already exists; imported memories are
                               downgraded to source "onboarding" and unpinned
                               unless --trust-provenance)
  upgrade [--allow-downgrade] [--allow-prerelease] [--allow-unattested]
                              Update ghost to the latest release (refuses an older
                              release unless --allow-downgrade is given, a
                              prerelease unless --allow-prerelease is, and from
                              v0.43.0 a release whose build attestation cannot
                              be confirmed unless --allow-unattested is)
  version                     Print version

Flags (reflect):
  --tier string   Consolidation tier: auto, cli, opencode, sqlite (default "auto")
  --apply         Save results
  --restore       Undo last consolidation

Environment:
  GHOST_OPENCODE_MODEL        Pin the model for opencode-backed tiers (e.g. "big-pickle")
  GHOST_DEBUG                 Enable debug logging
`, version)
}

// printUsage displays the top-level help. It is the help path's writer; the
// unusable-command-line path prints the same text through usageError, which
// also reports the mistake and returns the exit code.
func printUsage() {
	fmt.Fprint(os.Stderr, topLevelUsage())
}
