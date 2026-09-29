package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// pruneUsage is the help for `ghost prune`: stdout for -h/--help (handleHelp),
// stderr for a mistyped flag. One text for both, so the two cannot drift.
//
// The two sentences that are not a flag list are the ones this command exists to
// be honest about: that it is a dry run until --apply is given, and that nothing
// in Ghost ever runs it on its own.
const pruneUsage = `Usage: ghost prune [--project <name-or-id>] [--grace <duration>] [--apply]

Removes expired session-tier memories: rows saved with retention "session",
past the expiry Ghost derived for them, that nothing has touched for the grace
period. Project and persistent rows are never candidates, however old they are,
and a persistent row is exempt from consolidation, supersede and resolve as
well.

Dry-run by default: without --apply this prints what it WOULD remove and writes
nothing at all. With --apply each removal is appended to memory_history as a
delete tombstone first, so "ghost history <id>" still reports what was lost.

Flags:
  --project <name-or-id>   Only this project (default: every project, which
                           includes _global — a global memory saved as a
                           session has an expiry like any other)
  --grace <duration>       How long past expiry an untouched row is left alone
                           (Go duration: 168h, 7d is not a unit and 0 is
                           refused; default 168h)
  --apply                  Remove the rows instead of only reporting them

This command is never run for you. No lifecycle pass, no hook and no scheduler
calls it — a prune removes memories, so it happens when a person asks for it.
`

// pruneOptions is one `ghost prune` invocation's arguments. Parsed into a value
// by a named function because runPrune calls os.Exit, which a test cannot reach
// through: the argument rules and the report are both testable without a
// process, which is the only way they are testable at all.
type pruneOptions struct {
	// Apply writes. False is the report-only run, and it is the default because
	// this command deletes.
	Apply bool
	// Grace is the parsed --grace. Zero is the store's default, and an explicit
	// zero is REFUSED rather than folded into it: the store cannot tell "the
	// caller said nothing" from "the caller said 0", so the CLI has to refuse the
	// one request that would otherwise be answered with a different request than
	// the one made. --grace 1s is as close as this command goes to "expired".
	Grace time.Duration
	// Project scopes the run, by name or by id; empty is every project.
	Project string
}

// parsePruneArgs reads the command line. Every unknown token is an error rather
// than something ignored: a mistyped --apply would otherwise turn a deletion
// into a report, or a report into a deletion.
func parsePruneArgs(args []string) (pruneOptions, error) {
	var opts pruneOptions
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--apply":
			opts.Apply = true
		case arg == "--grace" || strings.HasPrefix(arg, "--grace="):
			value := ""
			if arg == "--grace" {
				if i+1 >= len(args) {
					return opts, fmt.Errorf("flag --grace needs a value (a Go duration, e.g. 168h)")
				}
				i++
				value = args[i]
			} else {
				value = strings.TrimPrefix(arg, "--grace=")
			}
			d, err := time.ParseDuration(value)
			if err != nil {
				return opts, fmt.Errorf("--grace %q is not a duration: %w (e.g. 168h, 30m, 12h30m)", value, err)
			}
			if d < 0 {
				return opts, fmt.Errorf("--grace %s is negative: it is how long to WAIT past expiry, not how long ago", d)
			}
			if d == 0 {
				// A zero grace is a real request — prune the moment a row expires —
				// and answering it with the default week would report a different
				// run than the one asked for. The default is not expressible as a
				// zero here, so the boundary is named instead: the shortest accepted
				// grace is 1s, and the default stays what it is.
				return opts, fmt.Errorf("--grace 0 would remove a memory the instant it expires; the shortest accepted grace is 1s, and the default is %s", memory.DefaultPruneGrace)
			}
			opts.Grace = d
		case arg == "--project" || strings.HasPrefix(arg, "--project="):
			value := ""
			if arg == "--project" {
				if i+1 >= len(args) {
					return opts, fmt.Errorf("flag --project needs a value (a project name or id)")
				}
				i++
				value = args[i]
			} else {
				value = strings.TrimPrefix(arg, "--project=")
			}
			if opts.Project != "" {
				return opts, fmt.Errorf("--project was given twice (%q and %q)", opts.Project, value)
			}
			opts.Project = value
		case strings.HasPrefix(arg, "-"):
			return opts, fmt.Errorf("unknown flag %q", arg)
		default:
			return opts, fmt.Errorf("ghost prune takes flags only, got %q", arg)
		}
	}
	return opts, nil
}

// pruneView is everything `ghost prune` prints, as a value, so the output
// contract is testable without a database and without a process.
type pruneView struct {
	// NoDatabase says the store does not exist yet, which is not an error: there
	// is nothing to prune and nothing was written. It is stated rather than
	// printed as an empty list, because "0 session memories would be removed" on a
	// machine that has never run Ghost reads as a measurement.
	NoDatabase bool
	// Applied distinguishes the two runs. Every other field is identical between
	// them, so without this the report is ambiguous about whether anything was
	// destroyed.
	Applied bool
	// Grace and Now are the run's own parameters, echoed so a saved report says
	// what it measured instead of leaving the reader to assume the defaults.
	Grace time.Duration
	Now   time.Time
	// Scope is the project the run covered, empty for every project.
	Scope string
	// Candidates is every row the run selected, in the order it would remove them.
	Candidates []memory.PruneCandidate
	// Removed is the count actually removed; zero on a dry run.
	Removed int
	// RemovedIDs travels so the tombstone each removal left can be followed.
	RemovedIDs []string
}

// pluralNoun is the noun alone, for a sentence that already printed the count.
// pluralCount cannot be used for that: it renders "1 memory", so a caller with
// its own "%d" in the format string would print the number twice — which is how
// this report read "1 session 1 memory" the first time.
func pluralNoun(n int) string {
	if n == 1 {
		return "memory"
	}
	return "memories"
}

// printPrune renders the report. The shape is the dry-run/apply distinction
// first, because that is the only thing a reader has to be sure of before they
// read anything else: the headline says which of the two runs this is, and the
// closing line says what to do next in the dry-run case rather than leaving the
// reader to infer that nothing happened.
//
// An applied report is about what the run DID, and the candidate list is not that.
// It is what the PREVIEW selected, while every batch re-derives the same predicate
// under the write lock — so a row a concurrent save pinned, re-tiered or re-saved
// between the preview and its own batch stops matching and is spared, correctly,
// and it is still in that list. Counting and listing the candidates on this path
// headlined removals nobody performed: 3050 in the headline against 3049 removed,
// with the spared row live in the store and the closing line contradicting the
// headline. So the applied path splits on RemovedIDs: the removals are counted and
// listed, and the rest are named as spared, because a row the operator asked about
// has to be accounted for either way. renderApplyFailure filters the same way
// before it calls in, and this split is a no-op on the list it passes.
func printPrune(w io.Writer, v pruneView) error {
	if v.NoDatabase {
		_, err := fmt.Fprintln(w, "no Ghost database yet (run ghost first) — nothing to prune")
		return err
	}
	removed, spared := v.Candidates, []memory.PruneCandidate(nil)
	if v.Applied {
		removed, spared = splitPruneCandidates(v.Candidates, v.RemovedIDs)
	}
	headline := fmt.Sprintf("prune: %d session %s", len(removed), pluralNoun(len(removed)))
	scope := "every project"
	if v.Scope != "" {
		scope = "project " + v.Scope
	}
	if v.Applied {
		headline += fmt.Sprintf(" removed (grace %s, %s)", v.Grace, scope)
	} else {
		headline += fmt.Sprintf(" would be removed (grace %s, %s) — dry run, nothing was written", v.Grace, scope)
	}
	if _, err := fmt.Fprintln(w, headline); err != nil {
		return err
	}
	for _, c := range removed {
		if err := printPruneRow(w, c, "  "); err != nil {
			return err
		}
	}
	if len(spared) > 0 {
		// The line says the row stopped MATCHING, not that it was kept: a
		// concurrent write may have removed it outright, in which case calling it
		// spared-and-present would be the false claim in the other direction.
		them := plural(len(spared), "it", "them")
		if _, err := fmt.Fprintf(w, "  spared: %s no longer matched the prune when this run reached %s (pinned, re-tiered or re-saved since the preview listed %s)\n",
			pluralCount(len(spared), "row", "rows"), them, them); err != nil {
			return err
		}
		for _, c := range spared {
			if err := printPruneRow(w, c, "    "); err != nil {
				return err
			}
		}
	}
	switch {
	case !v.Applied && len(v.Candidates) > 0:
		if _, err := fmt.Fprintf(w, "nothing removed — pass --apply to remove the %d %s above; each removal leaves a delete record in memory_history\n",
			len(v.Candidates), pluralNoun(len(v.Candidates))); err != nil {
			return err
		}
	case v.Applied && len(removed) > 0:
		if _, err := fmt.Fprintf(w, "removed %d %s; ghost history <id> still reports each one, with phase=delete\n",
			v.Removed, pluralNoun(v.Removed)); err != nil {
			return err
		}
	case v.Applied && len(spared) > 0:
		// The spared block above is the whole statement. This run selected rows and
		// took none of them, so "no expired session memories past the grace period"
		// would be false — something WAS prunable when the run looked — and a
		// "removed 0" closing line would be a claim about a run that never
		// reported one either.
	case v.Applied:
		if _, err := fmt.Fprintln(w, "no expired session memories past the grace period — nothing removed"); err != nil {
			return err
		}
	}
	return nil
}

// splitPruneCandidates partitions a selected set into the rows an applied run
// removed and the rows it did not, by the ids the run reports. A dry run is never
// split — nothing was attempted, so the whole selection is what it would remove —
// which is why the caller decides with v.Applied rather than this function.
//
// "Spared" is not only "pinned between batches". Every term of the predicate is
// re-checked at the row's own batch, so a save that re-tiered the row, recomputed
// its expiry or moved its last write spares it too, and a concurrent write that
// removed the row outright leaves it in the spared set with nothing left to spare.
// The renderer's wording holds for all of those, which is why it names the change
// rather than the outcome.
func splitPruneCandidates(candidates []memory.PruneCandidate, removedIDs []string) (removed, spared []memory.PruneCandidate) {
	landed := make(map[string]bool, len(removedIDs))
	for _, id := range removedIDs {
		landed[id] = true
	}
	removed = make([]memory.PruneCandidate, 0, len(candidates))
	for _, c := range candidates {
		if landed[c.ID] {
			removed = append(removed, c)
			continue
		}
		spared = append(spared, c)
	}
	return removed, spared
}

// printPruneRow renders one selected row and the stored text under it. indent is
// the row's own prefix, so the removed listing and the spared listing are this one
// renderer at two depths rather than two spellings of a row.
//
// Two stamps, and they are different facts. ActivityAt is the row's last real
// activity — a recorded read, else its last write. GraceFrom is what the grace was
// measured from, which since #772 also includes the expiry. They are printed
// together and separately LABELLED, because the expiry is a value Ghost derived
// forward at save time: printing it as "last touched" would claim the row was
// touched at the instant it stopped being wanted, and on a row never edited since
// its save that instant IS the expiry — so the line would carry one timestamp
// twice under two names. An operator deciding whether to run `ghost prune --apply`
// is reading exactly this line.
//
// The grace-from stamp is omitted when it IS the expiry already shown, rather
// than printed a second time. Stated that way rather than as a rule about the
// row's stamps, because that is what the comparison below actually decides: a
// recorded last_accessed shadows the max inside GraceFrom, so a row carrying an
// OLD read takes the last_accessed branch and has its basis printed even though
// nothing on it is newer than the expiry. That is dormant (nothing writes
// last_accessed) and is named as a known limit on pruneActivitySQL itself.
//
// The stamps reading is still the one that matters for the shape this was
// written for: raiseRetentionTx refreshes expires_at on a fold and deliberately
// leaves updated_at alone, so a folded session row HAS been written to and still
// has the expiry as its newest stamp — the case a rule phrased as "nothing has
// written to this row" would get wrong.
func printPruneRow(w io.Writer, c memory.PruneCandidate, indent string) error {
	activity := c.ActivityAt
	if activity == "" {
		activity = "(no recorded activity)"
	}
	if _, err := fmt.Fprintf(w, "%s%s  %s  %s  expired %s  last touched %s", indent, c.ID, c.Category, c.Retention, c.ExpiresAt, activity); err != nil {
		return err
	}
	if c.GraceFrom != "" && c.GraceFrom != c.ExpiresAt {
		if _, err := fmt.Fprintf(w, "  grace from %s", c.GraceFrom); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	// The stored text, through the shared displayStored substitution: this is
	// the surface an operator reads precisely when a stale, credential-shaped
	// row is most likely to need purging, and the write-boundary guard is not
	// retroactive — a pre-guard row can still carry a value, and 160 bytes is
	// more than enough to print one whole.
	if _, err := fmt.Fprintf(w, "%s  %s\n", indent, displayStored(c.Content, c.Category, 160)); err != nil {
		return err
	}
	return nil
}

// renderApplyFailure prints what a failed apply actually landed, when anything
// did. PruneSessionMemories returns its report together with the error so a
// half-finished prune is visible, and the handler renders it before the non-zero
// exit: a failure that removed rows in earlier batches and then hit a busy
// database would otherwise tell the operator nothing but the error, leaving the
// destruction visible only as tombstones nobody knew to look for.
//
// The report is filtered to the rows that LANDED. The candidate list is the
// full selection, and the headline counts candidates, so printing the raw
// report would headline a removal count no one actually performed — the failed
// batch's rows are named without ever being removed.
//
// Rendered only when something landed: an apply that failed on its first batch
// destroyed nothing, and the error line already says the run failed.
func renderApplyFailure(w io.Writer, report memory.PruneReport, scope string) error {
	if report.Removed == 0 {
		return nil
	}
	landed := make(map[string]bool, len(report.RemovedIDs))
	for _, id := range report.RemovedIDs {
		landed[id] = true
	}
	kept := make([]memory.PruneCandidate, 0, report.Removed)
	for _, c := range report.Candidates {
		if landed[c.ID] {
			kept = append(kept, c)
		}
	}
	return printPrune(w, pruneView{
		Applied:    true,
		Grace:      report.Grace,
		Now:        report.Now,
		Scope:      scope,
		Candidates: kept,
		Removed:    report.Removed,
		RemovedIDs: report.RemovedIDs,
	})
}

// runPrune implements `ghost prune`: open the store, resolve the project scope,
// and run the one prune pass. It is a thin shell over the store's own
// PruneSessionMemories so the dry run and the apply are the same transaction on
// both sides of this boundary — the CLI cannot decide to prune differently from
// what the store would do.
func runPrune(args []string) {
	opts, err := parsePruneArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n\n%s", err, pruneUsage)
		os.Exit(1)
	}

	dataDir, err := dataDirPath()
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
		if err := printPrune(os.Stdout, pruneView{NoDatabase: true}); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	db, err := memory.OpenDB(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	store := memory.NewStore(db, nil)

	// The scope is resolved through the store's own project resolution, so a name,
	// an id and a path all mean what they mean everywhere else — and a name that
	// matches nothing is an error rather than a silently store-wide prune, which
	// is the one outcome a scope flag must never produce.
	scope := ""
	if opts.Project != "" {
		resolved, _, rErr := store.ResolveProject(ctx, opts.Project)
		if rErr != nil {
			fmt.Fprintf(os.Stderr, "error: resolve project %q: %v\n", opts.Project, rErr)
			os.Exit(1)
		}
		if resolved == "" {
			fmt.Fprintf(os.Stderr, "error: project %q not found — nothing was pruned\n", opts.Project)
			os.Exit(1)
		}
		scope = resolved
	}

	report, err := store.PruneSessionMemories(ctx, memory.PruneOptions{
		Apply:     opts.Apply,
		Grace:     opts.Grace,
		ProjectID: scope,
	})
	if err != nil {
		// The store returns its report alongside the error so a half-finished
		// apply is visible; print what actually committed before the non-zero
		// exit (see renderApplyFailure).
		if rerr := renderApplyFailure(os.Stdout, report, opts.Project); rerr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", rerr)
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	view := pruneView{
		Applied:    report.Applied,
		Grace:      report.Grace,
		Now:        report.Now,
		Scope:      opts.Project,
		Candidates: report.Candidates,
		Removed:    report.Removed,
		RemovedIDs: report.RemovedIDs,
	}
	if err := printPrune(os.Stdout, view); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
