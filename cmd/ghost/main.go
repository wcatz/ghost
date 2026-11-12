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

func runCLI(argv []string, dispatch func([]string) int) int {
	if handleHelp(argv) {
		return 0
	}
	return dispatch(argv)
}

type commandGroup struct {
	path string

	subs []string

	defaultAction bool
}

var commandGroups = []commandGroup{
	{path: "mcp", subs: []string{"init", "status", "wiring"}, defaultAction: true},
	{path: "backup", subs: []string{"verify"}, defaultAction: true},
	{path: "maintenance", subs: []string{"status", "clean-scratch"}},
	{path: "memory", subs: []string{"move"}},
	{path: "obsidian", subs: []string{"export", "sync"}},
	{path: "opencode", subs: []string{"cleanup-sessions"}},
	{path: "project", subs: []string{"bind", "delete", "merge"}},
}

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

const exitUsage = 2

func applyBuildVersion() { config.SetBuildVersion(version) }

func dispatchCommand(argv []string) int {

	memory.SetDetectRemote(repo.DetectRemote)

	applyBuildVersion()

	if len(argv) > 0 {
		switch argv[0] {
		case "-v", "--version", "version":
			fmt.Printf("ghost %s\n", version)
			return 0
		case "help", "--help", "-h":
			return runHelpCommand(argv[1:])
		case "mcp":

			if sub, given := subArg(argv, 1); given {
				switch sub {
				case "init":
					runMCPInit()
					return 0
				case "status":
					runMCPStatus()
					return 0
				case "wiring":
					runMCPWiring()
					return 0
				}
				return usageError("mcp", sub, true, mcpUsage)
			}

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
			return runLifecycle()
		case "memory":
			sub, given := subArg(argv, 1)
			switch sub {
			case "move":
				runMemoryMove()
				return 0
			}
			return usageError("memory", sub, given, memoryUsage)
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

			return runContext()
		case "maintenance":
			sub, given := subArg(argv, 1)
			switch sub {
			case "status":
				runMaintenanceStatus()
				return 0
			case "consolidate-global":
				runMaintenanceConsolidateGlobal(argv[2:])
				return 0
			case "clean-scratch":
				runMaintenanceCleanScratch(argv[2:])
				return 0
			}
			return usageError("maintenance", sub, given, maintenanceUsage)
		}
	}

	if len(argv) == 0 {
		return usageError("", "", false, topLevelUsage())
	}
	return usageError("", argv[0], true, topLevelUsage())
}

func subArg(argv []string, i int) (word string, given bool) {
	if i < len(argv) {
		return argv[i], true
	}
	return "", false
}

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

const versionUsage = `Usage: ghost version

Prints the binary version.
`

func topLevelUsage() string {
	return fmt.Sprintf(`ghost %s — MCP memory server for Claude Code

Usage:
  ghost <command>

Commands:
  mcp                         Start MCP server on stdio (used by Claude Code)
  mcp init [--client claude|opencode|codex|goose|all] [--dry-run]  Configure MCP client integration
                                                         (auto-detects when --client is omitted)
  mcp status [--client claude|opencode|codex|goose]                Check MCP client integration health
  mcp wiring                    Check client wiring against this binary's embedded sources
  hook <event> [--source <host>]  Lifecycle hook for MCP clients (session-start, stop, session-end, message-submit, edit)
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
  context [--cwd <dir>] [--as-of <instant>]  Print the passive session-start context block
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
                               downgraded to source "onboarding", unpinned and
                               the durable "project" tier unless
                               --trust-provenance)
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

func printUsage() {
	fmt.Fprint(os.Stderr, topLevelUsage())
}
