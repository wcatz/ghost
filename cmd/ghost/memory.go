package main

import (
	"context"
	"fmt"
	"os"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/memory"
)

const memoryUsage = `Usage: ghost memory move <memory-id> --project <from> --to <to> [--confirm] [--dry-run]

Subcommands:
  move   Move a memory between projects
`

const memoryMoveUsage = `Usage: ghost memory move <memory-id> --project <from> --to <to> [--confirm] [--dry-run]

Moves a memory between projects while keeping its ID, links, history, flags,
evidence, provenance and embeddings. Works from a project to another project,
from a project to _global, and from _global to a project.

Flags:
  --project <name-or-id>   Source project the memory belongs to (required)
  --to <name-or-id>        Destination project, or _global/global (required)
  --confirm                Required to move a pinned or persistent memory
  --dry-run                Preview the move without writing any changes
`

type memoryMoveArgs struct {
	memoryID string
	from     string
	to       string
	confirm  bool
	dryRun   bool
}

func parseMemoryMoveArgs(args []string) (memoryMoveArgs, error) {
	var out memoryMoveArgs
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--project":
			if i+1 >= len(args) {
				return out, fmt.Errorf("--project requires a value")
			}
			i++
			out.from = args[i]
		case "--to":
			if i+1 >= len(args) {
				return out, fmt.Errorf("--to requires a value")
			}
			i++
			out.to = args[i]
		case "--confirm":
			out.confirm = true
		case "--dry-run":
			out.dryRun = true
		default:
			if len(a) > 0 && a[0] == '-' {
				return out, fmt.Errorf("unknown flag %q", a)
			}
			if out.memoryID != "" {
				return out, fmt.Errorf("unexpected argument %s", memory.RefusalArg("arg", a))
			}
			out.memoryID = a
		}
	}
	if out.memoryID == "" {
		return out, fmt.Errorf("memory id is required")
	}
	if out.from == "" {
		return out, fmt.Errorf("--project is required")
	}
	if out.to == "" {
		return out, fmt.Errorf("--to is required")
	}
	return out, nil
}

func resolveMoveTarget(ctx context.Context, store *memory.Store, to string) (string, error) {
	if to == memory.GlobalProjectID || to == "_global" || to == "global" {
		return memory.GlobalProjectID, nil
	}
	id, _, err := store.ResolveProject(ctx, to)
	if err != nil {
		return "", fmt.Errorf("resolve target project: %w", err)
	}
	if id == "" {
		return "", fmt.Errorf("target project %s not found", memory.RefusalArg("to", to))
	}
	return id, nil
}

func runMemoryMove() {
	args, err := parseMemoryMoveArgs(os.Args[3:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n\n%s", err, memoryMoveUsage)
		os.Exit(1)
	}

	_, _, store := bootstrap(os.Stderr, cliLogLevel(), failOnConfig)
	defer store.Close() //nolint:errcheck
	ctx := context.Background()

	resolvedFrom, _, err := store.ResolveProject(ctx, args.from)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: resolve project: %v\n", err)
		os.Exit(1)
	}
	if resolvedFrom == "" {
		fmt.Fprintf(os.Stderr, "error: project %s not found\n", memory.RefusalArg("project", args.from))
		os.Exit(1)
	}

	resolvedTo, err := resolveMoveTarget(ctx, store, args.to)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	res, err := store.MoveMemory(ctx, resolvedFrom, args.memoryID, resolvedTo, memory.MoveMemoryOptions{
		Confirm: args.confirm,
		DryRun:  args.dryRun,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if res.DryRun {
		fmt.Printf("Dry run: would move memory %s from %s to %s. No changes written.\n",
			assemble.Token(res.MemoryID), assemble.Token(res.FromProjectID), assemble.Token(res.ToProjectID))
		return
	}
	fmt.Printf("Moved memory %s from %s to %s.\n",
		assemble.Token(res.MemoryID), assemble.Token(res.FromProjectID), assemble.Token(res.ToProjectID))
}
