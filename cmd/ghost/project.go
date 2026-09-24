package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)

// resolveProjectOrExit resolves projectName to a project ID via store, printing
// an error (with known-project names when available) and exiting the process
// on failure or when no matching project is found.
func resolveProjectOrExit(ctx context.Context, store *memory.Store, projectName string) string {
	projectID, _, err := store.ResolveProject(ctx, projectName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if projectID == "" {
		names, listErr := store.ListProjectNames(ctx)
		if listErr != nil || len(names) == 0 {
			fmt.Fprintf(os.Stderr, "error: project %q not found\n", projectName)
		} else {
			fmt.Fprintf(os.Stderr, "error: project %q not found. Known projects: %s\n", projectName, strings.Join(names, ", "))
		}
		os.Exit(1)
	}
	return projectID
}

// confirmProjectDeleteName reports whether typed (a raw scanned line, not
// yet trimmed) matches expected exactly once surrounding whitespace is
// stripped. Pulled out of runProjectDelete as its own pure function so the
// actual confirmation decision is unit-testable without stdin/os.Exit
// plumbing.
func confirmProjectDeleteName(typed, expected string) bool {
	return strings.TrimSpace(typed) == expected
}

// printDeleteSummary writes a DeleteProjectSummary to out in the fixed-width
// format shared by both the dry-run preview and the post-apply report. It
// returns the first write error encountered, if any.
func printDeleteSummary(out io.Writer, summary memory.DeleteProjectSummary, verb string) error {
	if _, err := fmt.Fprintf(out, "%s %q (%s):\n", verb, summary.ProjectName, summary.ProjectID); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "  memories:     %d\n", summary.Memories); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "  memory_links: %d\n", summary.MemoryLinks); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "  tasks:        %d\n", summary.Tasks); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "  decisions:    %d\n", summary.Decisions); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "  token_usage:  %d\n", summary.TokenUsage); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "  audit_log:    %d\n", summary.AuditLog); err != nil {
		return err
	}
	return nil
}

// runProjectDeleteCore implements the confirmation-gated delete against an
// already-open store: prints the dry-run summary, stops there unless apply
// is set, and otherwise requires re-typing the project's name (read from in)
// before calling store.DeleteProject with apply=true. Pulled out of
// runProjectDelete — which owns CLI arg parsing, bootstrap(), and os.Exit —
// so the confirmation gate is unit-testable against a real store with
// injected stdin, without exercising process-exit paths.
func runProjectDeleteCore(ctx context.Context, store *memory.Store, out io.Writer, in io.Reader, projectName string, apply bool) error {
	summary, err := store.DeleteProject(ctx, projectName, false)
	if err != nil {
		return err
	}
	if err := printDeleteSummary(out, summary, "Would delete"); err != nil {
		return err
	}

	if !apply {
		if _, err := fmt.Fprintln(out, "\nRe-run with --apply to actually delete."); err != nil {
			return err
		}
		return nil
	}

	if _, err := fmt.Fprintf(out, "\nType the project name (%q) to confirm deletion: ", summary.ProjectName); err != nil {
		return err
	}
	scanner := bufio.NewScanner(in)
	scanner.Scan()
	if !confirmProjectDeleteName(scanner.Text(), summary.ProjectName) {
		return errors.New("confirmation did not match project name — nothing deleted")
	}

	// Reuse the ID resolved by the preview above rather than re-resolving
	// projectName: the confirmation prompt waits on a human, and re-resolving
	// a mutable name/path in that window could silently hit a different
	// project if it was renamed or recreated in the meantime.
	summary, err = store.DeleteProject(ctx, summary.ProjectID, true)
	if err != nil {
		return err
	}
	if err := printDeleteSummary(out, summary, "Deleted"); err != nil {
		return err
	}
	return nil
}

// runProjectDelete implements `ghost project delete <name-or-id> [--apply]`.
// Always prints the dry-run summary first. Without --apply it stops there.
// With --apply, it re-prints the summary and requires re-typing the
// project's name at a prompt before anything is actually deleted — this is
// irreversible and there is no undo, so the flag alone is not enough.
func runProjectDelete() {
	var projectName string
	apply := false
	for i := 3; i < len(os.Args); i++ {
		switch {
		case os.Args[i] == "--apply":
			apply = true
		case !strings.HasPrefix(os.Args[i], "-"):
			if projectName != "" {
				fmt.Fprintln(os.Stderr, "error: expected exactly one project")
				os.Exit(1)
			}
			projectName = os.Args[i]
		default:
			fmt.Fprintf(os.Stderr, "error: unknown flag %q\n", os.Args[i])
			os.Exit(1)
		}
	}
	if projectName == "" {
		fmt.Fprintln(os.Stderr, `Usage: ghost project delete <name-or-id> [flags]

Flags:
  --apply   Actually delete (default is dry-run/preview)

Permanently removes a project: memories, tags, embeddings, links, tasks,
decisions, learned context, reflection snapshots, and cost/audit history.
Irreversible. Refuses to delete _global.`)
		os.Exit(1)
	}

	_, _, store := bootstrap(os.Stderr, cliLogLevel())
	defer store.Close() //nolint:errcheck
	ctx := context.Background()

	if err := runProjectDeleteCore(ctx, store, os.Stdout, os.Stdin, projectName, apply); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// runProjectMergeCore implements the merge behind `ghost project merge`.
// Both arguments resolve through ResolveProject (id, name, path-prefix, or
// basename); merging reassigns every child record to the surviving project —
// preserving memory IDs, links, and pin state — and deletes the old project
// row. Non-destructive to records, so no confirmation gate (unlike delete).
func runProjectMergeCore(ctx context.Context, store *memory.Store, out io.Writer, oldArg, newArg string) error {
	oldID, oldName := resolveForMerge(ctx, store, oldArg)
	newID, newName := resolveForMerge(ctx, store, newArg)
	if oldID == "" {
		return fmt.Errorf("project %q not found; known projects: %s", oldArg, strings.Join(knownProjectNames(ctx, store), ", "))
	}
	if newID == "" {
		return fmt.Errorf("project %q not found; known projects: %s", newArg, strings.Join(knownProjectNames(ctx, store), ", "))
	}
	if oldID == newID {
		return fmt.Errorf("refusing to merge a project into itself (%q)", oldID)
	}
	if _, err := fmt.Fprintf(out, "Merging %q (%s) into %q (%s)\n", oldName, oldID, newName, newID); err != nil {
		return err
	}
	if err := store.MergeProject(ctx, oldID, newID); err != nil {
		return err
	}
	_, err := fmt.Fprintf(out, "merged: all records from %q now belong to %q\n", oldName, newName)
	return err
}

func resolveForMerge(ctx context.Context, store *memory.Store, arg string) (string, string) {
	id, name, err := store.ResolveProject(ctx, arg)
	if err != nil {
		return "", ""
	}
	return id, name
}

func knownProjectNames(ctx context.Context, store *memory.Store) []string {
	names, err := store.ListProjectNames(ctx)
	if err != nil {
		return nil
	}
	return names
}

// runProjectMerge implements `ghost project merge <old> <new>`.
func runProjectMerge() {
	args := os.Args[3:]
	var positional []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			fmt.Fprintf(os.Stderr, "error: unknown flag %q\n", a)
			os.Exit(1)
		}
		positional = append(positional, a)
	}
	if len(positional) != 2 {
		fmt.Fprintln(os.Stderr, "Usage: ghost project merge <old-name-or-id> <new-name-or-id>")
		os.Exit(1)
	}

	_, _, store := bootstrap(os.Stderr, cliLogLevel())
	defer store.Close() //nolint:errcheck

	if err := runProjectMergeCore(context.Background(), store, os.Stdout, positional[0], positional[1]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// firstLine returns the first line of s, truncated to at most n runes with an
// ellipsis, for compact CLI preview.
func firstLine(s string, n int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
