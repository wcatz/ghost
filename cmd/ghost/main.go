package main

import (
	"fmt"
	"os"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/repo"
)

var version = "dev"

func main() {
	// Repository identity needs git, which internal/memory deliberately never
	// invokes — the capability is injected so the store stays a pure storage
	// layer, tests can pin it, and a store built without one resolves exactly
	// as it did before. Wired once here because main dispatches the MCP
	// server, the lifecycle hooks and every CLI subcommand, so a single line
	// covers the whole binary.
	memory.SetDetectRemote(repo.DetectRemote)

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-v", "--version", "version":
			fmt.Printf("ghost %s\n", version)
			return
		case "help", "--help", "-h":
			printUsage()
			return
		case "mcp":
			if len(os.Args) > 2 {
				switch os.Args[2] {
				case "init":
					runMCPInit()
					return
				case "status":
					runMCPStatus()
					return
				}
			}
			runMCP()
			return
		case "hook":
			runHook()
			return
		case "reflect":
			runReflect()
			return
		case "supersede":
			runSupersede()
			return
		case "resolve":
			runResolve()
			return
		case "lifecycle":
			runLifecycle()
			return
		case "project":
			if len(os.Args) > 2 && os.Args[2] == "delete" {
				runProjectDelete()
				return
			}
			if len(os.Args) > 2 && os.Args[2] == "merge" {
				runProjectMerge()
				return
			}
			if len(os.Args) > 2 && os.Args[2] == "bind" {
				runProjectBind()
				return
			}
			fmt.Fprintln(os.Stderr, "Usage: ghost project delete <name-or-id> [--apply]")
			fmt.Fprintln(os.Stderr, "       ghost project merge <old-name-or-id> <new-name-or-id>")
			fmt.Fprintln(os.Stderr, "       ghost project bind <project-id> <checkout-directory>")
			os.Exit(1)
		case "upgrade":
			runUpgrade()
			return
		case "obsidian":
			runObsidian()
			return
		case "opencode":
			if len(os.Args) > 2 && os.Args[2] == "cleanup-sessions" {
				runOpenCodeCleanupSessions(os.Args[3:])
				return
			}
			fmt.Fprintln(os.Stderr, "Usage: "+usageCleanupSessions)
			os.Exit(1)
		case "bench":
			runBench()
			return
		case "context":
			runContext()
			return
		case "maintenance":
			if len(os.Args) > 2 {
				switch os.Args[2] {
				case "status":
					runMaintenanceStatus()
					return
				case "clean-scratch":
					runMaintenanceCleanScratch(os.Args[3:])
					return
				}
			}
			fmt.Fprintln(os.Stderr, "Usage: ghost maintenance status")
			fmt.Fprintln(os.Stderr, "       ghost maintenance clean-scratch [--apply]")
			os.Exit(1)
		}
	}
	printUsage()
}

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
  upgrade                     Update ghost to the latest release
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
