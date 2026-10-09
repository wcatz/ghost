package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/scratch"
)

// maintenanceStatusView is everything `ghost maintenance status` prints: the
// live scratch-root measurement plus the recorded hygiene events. Kept as a
// value the printer takes so the output contract is testable without a
// database or a real root.
type maintenanceStatusView struct {
	Root       string
	RootBytes  int64
	RootFiles  int
	Budget     int64
	Runs       []memory.MaintenanceRun
	NoDatabase bool
}

// printMaintenanceStatus renders the status view: one header line with the
// live scratch bytes against the configured budget, then one human-readable
// line per recorded run (timestamp, kind, scratch bytes, reaped counts,
// note). Empty states — no database, no runs, disabled budget — print
// explicit sentences instead of nothing.
func printMaintenanceStatus(w io.Writer, v maintenanceStatusView) error {
	budget := "budget disabled (scratch.max_bytes = 0)"
	if v.Budget > 0 {
		budget = fmt.Sprintf("budget %d bytes", v.Budget)
	}
	if _, err := fmt.Fprintf(w, "scratch root: %s (%d bytes in %d files; %s)\n",
		v.Root, v.RootBytes, v.RootFiles, budget); err != nil {
		return err
	}
	switch {
	case v.NoDatabase:
		_, err := fmt.Fprintln(w, "no Ghost database yet (run ghost first) — no maintenance runs recorded")
		return err
	case len(v.Runs) == 0:
		_, err := fmt.Fprintln(w, "no maintenance runs recorded yet")
		return err
	}
	if _, err := fmt.Fprintln(w, "maintenance runs (most recent first):"); err != nil {
		return err
	}
	for _, r := range v.Runs {
		if _, err := fmt.Fprintf(w, "  %s  %s  scratch=%d bytes  reaped=%d (%d bytes)  %s\n",
			r.RecordedAt, r.Kind, r.ScratchBytes, r.ScratchReapedCount,
			r.ScratchReapedBytes, r.Note); err != nil {
			return err
		}
	}
	return nil
}

// maintenanceUsage is the help for `ghost maintenance`: stderr for a missing or
// unrecognised subcommand (the dispatch's usage error, exit 2), stdout for
// -h/--help (see handleHelp). One text for both, so the two can never drift.
const maintenanceUsage = `Usage: ghost maintenance status
       ghost maintenance clean-scratch [--apply]
       ghost maintenance consolidate-global [--apply]
`

// runMaintenanceStatus implements `ghost maintenance status`: live scratch
// usage + the most recent hygiene runs. Best-effort on the scratch side (an
// unusable root is reported in the header, not fatal); the database decides
// between "runs" and the explicit no-database state.
// parseConsolidateGlobalArgs parses `ghost maintenance consolidate-global` arguments.
func parseConsolidateGlobalArgs(args []string) (apply bool, err error) {
	for _, arg := range args {
		switch arg {
		case "--apply":
			apply = true
		default:
			return false, fmt.Errorf("unknown argument %q (usage: ghost maintenance consolidate-global [--apply])", arg)
		}
	}
	return apply, nil
}

// runMaintenanceConsolidateGlobal implements `ghost maintenance consolidate-global`:
// dry-run by default — lists the near-duplicate clusters found in _global —
// and applies the folds only behind an explicit --apply.
func runMaintenanceConsolidateGlobal(args []string) {
	apply, err := parseConsolidateGlobalArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	dataDir, err := config.DataDirPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	dbPath := filepath.Join(dataDir, "ghost.db")
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "no database found (run ghost first)\n")
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "error: database: %v\n", err)
		os.Exit(1)
	}

	db, err := memory.OpenDB(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close() //nolint:errcheck

	store := memory.NewStore(db, nil)

	ctx := context.Background()

	fmt.Printf("Scanning _global for near-duplicate memories...\n\n")

	result, err := store.ConsolidateGlobal(ctx, memory.GlobalConsolidationOptions{DryRun: !apply})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: global consolidation failed: %v\n", err)
		os.Exit(1)
	}

	if len(result.Clusters) == 0 {
		fmt.Println("No near-duplicate clusters found in _global.")
		return
	}

	fmt.Printf("Found %d cluster(s) covering %d folded memories:\n\n", len(result.Clusters), countFoldedInClusters(result.Clusters))

	for i, cluster := range result.Clusters {
		fmt.Printf("Cluster %d (similarity >= %.2f):\n", i+1, cluster.Similarity)
		fmt.Printf("  SURVIVOR: [%s] (imp:%.1f, pinned:%v) %s\n",
			cluster.Survivor.Category, cluster.Survivor.Importance, cluster.Survivor.Pinned,
			truncateForDisplay(cluster.Survivor.Content, 120))
		for _, folded := range cluster.Folded {
			fmt.Printf("  folded:   [%s] (imp:%.1f, pinned:%v) %s\n",
				folded.Category, folded.Importance, folded.Pinned,
				truncateForDisplay(folded.Content, 120))
		}
		fmt.Println()
	}

	if !apply {
		fmt.Println("DRY RUN — pass --apply to execute the folds above")
		return
	}

	fmt.Printf("Applied: folded %d memories into %d survivors.\n", result.Folded, len(result.Clusters))
}

// countFoldedInClusters returns the total number of folded memories across all clusters.
func countFoldedInClusters(clusters []memory.GlobalCluster) int {
	count := 0
	for _, c := range clusters {
		count += len(c.Folded)
	}
	return count
}

// truncateForDisplay truncates a string for display, adding ellipsis if truncated.
func truncateForDisplay(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

func runMaintenanceStatus() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load config: %v\n", err)
		os.Exit(1)
	}
	view := maintenanceStatusView{Budget: cfg.Scratch.MaxBytes}

	// DataDirPath, not DataDir: the report below opens the store read-write when
	// it exists, and it must not create the data directory on a machine that has
	// none — the "no database" branch is what says so. A GHOST_DEV_FORBID_DATA_DIR
	// refusal arrives from this same call (#721), reported like any other
	// unresolvable data dir.
	dataDir, err := config.DataDirPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	dbPath := filepath.Join(dataDir, "ghost.db")
	if _, err := os.Stat(dbPath); err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "error: database: %v\n", err)
			os.Exit(1)
		}
		view.NoDatabase = true
		view.Root = "(not created yet)"
	} else {
		// The database existing means the data dir exists, so Root() creates
		// no phantom directory — only the scratch subdir any spawn would make.
		if root, rErr := scratch.Root(); rErr == nil {
			view.Root = root
			if b, f, sErr := scratch.Size(root); sErr == nil {
				view.RootBytes, view.RootFiles = b, f
			}
		} else {
			view.Root = fmt.Sprintf("(unavailable: %v)", rErr)
		}

		db, err := memory.OpenDB(dbPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: database: %v\n", err)
			os.Exit(1)
		}
		runs, err := memory.RecentMaintenanceRuns(context.Background(), db, 20)
		db.Close() //nolint:errcheck
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		view.Runs = runs
	}

	if err := printMaintenanceStatus(os.Stdout, view); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// parseCleanScratchArgs parses `ghost maintenance clean-scratch` arguments.
// Only --apply is recognized; anything else is an error rather than being
// ignored, because a silently misparsed flag here could turn a report into a
// removal (or hide one).
func parseCleanScratchArgs(args []string) (apply bool, err error) {
	for _, arg := range args {
		switch arg {
		case "--apply":
			apply = true
		default:
			return false, fmt.Errorf("unknown argument %q (usage: ghost maintenance clean-scratch [--apply])", arg)
		}
	}
	return apply, nil
}

// runMaintenanceCleanScratch implements `ghost maintenance clean-scratch`:
// report-only by default — counts, bytes, and exact paths of the two legacy
// debris locations — and removal only behind an explicit --apply, where
// CleanLegacy's rails (strict signature, regular files only, open-file probe)
// decide what may actually be deleted.
func runMaintenanceCleanScratch(args []string) {
	apply, err := parseCleanScratchArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: locate home directory: %v\n", err)
		os.Exit(1)
	}
	// The pre-scratch-root cache dir and the shared system temp the #465-era
	// droppings leaked into. os.TempDir() (not a literal /tmp) keeps this
	// correct on Windows, where the droppings' signature simply never matches.
	report := scratch.ScanLegacy(filepath.Join(home, ".cache", "ghost-tmp"), os.TempDir())

	if err := scratch.WriteLegacyReport(os.Stdout, report); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if !apply {
		if _, err := fmt.Fprintln(os.Stdout, "nothing removed — pass --apply to remove the strict-signature files listed above"); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}
	removed, removedBytes, skipped, err := scratch.CleanLegacy(os.Stdout, report)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if _, err := fmt.Fprintf(os.Stdout, "done: removed %d file(s), %d bytes (%d skipped)\n", removed, removedBytes, skipped); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
