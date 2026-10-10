package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/reflection"
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

// parseConsolidateGlobalArgs parses `ghost maintenance consolidate-global`
// arguments. Only --apply is recognized: a misspelt flag must not turn a preview
// into a write, or hide that it was one.
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
// it folds near-duplicate rows in _global. Dry run by default, listing the
// clusters and writing nothing; --apply performs the fold.
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
			fmt.Println("no Ghost database yet (run ghost first) — nothing to consolidate")
			return
		}
		fmt.Fprintf(os.Stderr, "error: database: %v\n", err)
		os.Exit(1)
	}
	store, closeStore, err := openConsolidateGlobalStore(dataDir, apply)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer closeStore()
	if err := consolidateGlobal(context.Background(), store, apply, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// openConsolidateGlobalStore opens the store for a fold. A dry run opens it
// read-only, like the portable export and dry-run import: no migration runs, so
// a database whose schema is behind this build is reported and left as it was
// rather than migrated by a command that promises to write nothing. Only an
// apply opens read-write.
func openConsolidateGlobalStore(dataDir string, apply bool) (*memory.Store, func(), error) {
	if !apply {
		store, err := openReadOnlyTransferStore(dataDir, "previewing the _global fold")
		if err != nil {
			return nil, nil, err
		}
		return store, func() { _ = store.Close() }, nil
	}
	db, err := memory.OpenDB(filepath.Join(dataDir, "ghost.db"))
	if err != nil {
		return nil, nil, fmt.Errorf("database: %w", err)
	}
	return memory.NewStore(db, nil), func() { _ = db.Close() }, nil
}

// consolidateGlobal lists the near-duplicate clusters in _global and, when apply
// is set, folds them through Store.FoldRows: a targeted, id-based fold that
// snapshots, carries evidence, writes delete history naming the survivor and
// touches no row outside the cluster. Only reflection-written rows that are not
// pinned, resolved, persistent, or saved during the run are planned.
func consolidateGlobal(ctx context.Context, store *memory.Store, apply bool, w io.Writer) error {
	out := func(a ...any) { _, _ = fmt.Fprintln(w, a...) }
	outf := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	// Captured before the read, as reflect does: a row saved after this instant
	// was not in the plan and ReplaceNonManual keeps it.
	since, err := store.CurrentTimestamp(ctx)
	if err != nil {
		return fmt.Errorf("get timestamp: %w", err)
	}
	all, err := store.GetAll(ctx, "_global", -1)
	if err != nil {
		return fmt.Errorf("read _global: %w", err)
	}
	// A row stamped at or after `since` is one ReplaceNonManual keeps in place and
	// never lets an emission claim, so planning it would insert a second copy of
	// its text beside it. Leave it out of the plan; the next run sees it.
	var live []memory.Memory
	for _, m := range consolidatable(all) {
		// Only what reflection wrote is planned: the backlog is reflection's, and
		// an agent's own save is not this pass's to fold even when a near-twin
		// exists. A row at or after `since` was saved during the run.
		if m.Source == "reflection" && m.CreatedAt < since {
			live = append(live, m)
		}
	}
	clusters := reflection.PlanGlobalFold(live)

	if !apply {
		out("DRY RUN (use --apply to fold)")
		out()
	}
	folded := 0
	for _, c := range clusters {
		folded += len(c.Folded)
	}
	outf("_global: %d memories, %d reflection-written and consolidatable, %d near-duplicate cluster(s) covering %d rows\n",
		len(all), len(live), len(clusters), folded+len(clusters))
	for i, c := range clusters {
		outf("\ncluster %d: keep %s [%s, source %s] %s\n", i+1,
			assemble.Token(c.Survivor.ID), assemble.Label(c.Survivor.Category), assemble.Label(c.Survivor.Source), assemble.Label(displayStored(c.Survivor.Content, c.Survivor.Category, 120)))
		for _, f := range c.Folded {
			outf("  fold %s [%s, source %s] %s\n",
				assemble.Token(f.ID), assemble.Label(f.Category), assemble.Label(f.Source), assemble.Label(displayStored(f.Content, f.Category, 120)))
		}
	}
	if len(clusters) == 0 {
		out("nothing to fold")
		return nil
	}
	if !apply {
		outf("\nnothing written; pass --apply to fold %d row(s) into %d\n", folded, len(clusters))
		return nil
	}
	// One transaction for the whole run, naming rows by id and checking each again
	// inside it before anything is written: a row edited since the plan, or saved
	// since the run started, is skipped and reported, and no row outside a cluster
	// is written.
	asRow := func(m memory.Memory) memory.FoldRow {
		return memory.FoldRow{ID: m.ID, Content: m.Content, UpdatedAt: m.UpdatedAt}
	}
	plan := make([]memory.FoldCluster, len(clusters))
	for i, c := range clusters {
		plan[i].Survivor = asRow(c.Survivor)
		for _, f := range c.Folded {
			plan[i].Folded = append(plan[i].Folded, asRow(f))
		}
	}
	res, err := store.FoldRows(ctx, "_global", plan, since)
	if err != nil {
		return fmt.Errorf("fold _global: %w", err)
	}
	printFoldOutcome(w, res)
	return nil
}

// printFoldOutcome reports what FoldRows did, not what was planned: a survivor is
// counted only when a row was folded into it, and a run that wrote nothing says so.
func printFoldOutcome(w io.Writer, res memory.FoldResult) {
	for _, sk := range res.Skipped {
		_, _ = fmt.Fprintf(w, "skipped %s: %s\n", assemble.Token(sk.ID), assemble.Label(sk.Reason))
	}
	if len(res.Folded) == 0 {
		_, _ = fmt.Fprintf(w, "\nnothing written; %d row(s) skipped\n", len(res.Skipped))
		return
	}
	_, _ = fmt.Fprintf(w, "\nfolded %d row(s) into %d survivor(s); %d row(s) skipped\n", len(res.Folded), res.Clusters, len(res.Skipped))
}
