package main

import (
	"fmt"
	"os"

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

// dispatchCommand routes one invocation to its subcommand and returns the
// exit code. Subcommands that exit on their own (every failure path) never
// return here; the fall-through codes below match that behaviour.
func dispatchCommand(argv []string) int {
	// Repository identity needs git, which internal/memory deliberately never
	// invokes — the capability is injected so the store stays a pure storage
	// layer, tests can pin it, and a store built without one resolves exactly
	// as it did before. Wired once here because dispatch covers the MCP
	// server, the lifecycle hooks and every CLI subcommand, so a single line
	// covers the whole binary.
	memory.SetDetectRemote(repo.DetectRemote)

	if len(argv) > 0 {
		switch argv[0] {
		case "-v", "--version", "version":
			fmt.Printf("ghost %s\n", version)
			return 0
		case "help", "--help", "-h":
			printUsage()
			return 0
		case "mcp":
			if len(argv) > 1 {
				switch argv[1] {
				case "init":
					runMCPInit()
					return 0
				case "status":
					runMCPStatus()
					return 0
				}
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
			runLifecycle()
			return 0
		case "project":
			if len(argv) > 1 && argv[1] == "delete" {
				runProjectDelete()
				return 0
			}
			if len(argv) > 1 && argv[1] == "merge" {
				runProjectMerge()
				return 0
			}
			if len(argv) > 1 && argv[1] == "bind" {
				runProjectBind()
				return 0
			}
			fmt.Fprint(os.Stderr, projectUsage)
			return 1
		case "upgrade":
			runUpgrade(argv[1:])
			return 0
		case "backup":
			runBackup()
			return 0
		case "export":
			runExport()
			return 0
		case "import":
			runImport()
			return 0
		case "obsidian":
			runObsidian()
			return 0
		case "opencode":
			if len(os.Args) > 2 && os.Args[2] == "cleanup-sessions" {
				runOpenCodeCleanupSessions(os.Args[3:])
				return 0
			}
			fmt.Fprintln(os.Stderr, "Usage: "+usageCleanupSessions)
			return 1
		case "bench":
			runBench()
			return 0
		case "context":
			runContext()
			return 0
		case "maintenance":
			if len(argv) > 1 {
				switch argv[1] {
				case "status":
					runMaintenanceStatus()
					return 0
				case "clean-scratch":
					runMaintenanceCleanScratch(argv[2:])
					return 0
				}
			}
			fmt.Fprint(os.Stderr, maintenanceUsage)
			return 1
		}
	}
	printUsage()
	return 0
}

// versionUsage is `ghost version`'s help. It goes to stdout for -h/--help
// like every other subcommand's usage (see handleHelp).
const versionUsage = `Usage: ghost version

Prints the binary version.
`

// printUsage displays the top-level help.
func printUsage() {
	fmt.Fprintf(os.Stderr, `ghost %s — MCP memory server for Claude Code

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
                               while the MCP server runs; prints path and row counts
  export [--project <name>]   Write memories, tasks, decisions and projects as JSONL
    [--out <file.jsonl>]      (--out - for stdout; embeddings are not exported)
  import <file.jsonl> [--apply] [--trust-provenance]
                              Load a JSONL artifact (dry-run by default over a
                               read-only connection; never overwrites a record
                               whose id already exists; imported memories are
                               downgraded to source "onboarding" and unpinned
                               unless --trust-provenance)
  upgrade [--allow-downgrade]
                              Update ghost to the latest release (refuses an older
                              release unless --allow-downgrade is given)
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
