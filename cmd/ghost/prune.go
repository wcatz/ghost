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
  --project <name-or-id>   Only this project (default: every project)
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
func printPrune(w io.Writer, v pruneView) error {
	if v.NoDatabase {
		_, err := fmt.Fprintln(w, "no Ghost database yet (run ghost first) — nothing to prune")
		return err
	}
	headline := fmt.Sprintf("prune: %d session %s", len(v.Candidates), pluralNoun(len(v.Candidates)))
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
	for _, c := range v.Candidates {
		activity := c.ActivityAt
		if activity == "" {
			activity = "(no recorded activity)"
		}
		if _, err := fmt.Fprintf(w, "  %s  %s  %s  expired %s  last touched %s\n", c.ID, c.Category, c.Retention, c.ExpiresAt, activity); err != nil {
			return err
		}
		// The stored text, through the shared displayStored substitution: this is
		// the surface an operator reads precisely when a stale, credential-shaped
		// row is most likely to need purging, and the write-boundary guard is not
		// retroactive — a pre-guard row can still carry a value, and 160 bytes is
		// more than enough to print one whole.
		if _, err := fmt.Fprintf(w, "    %s\n", displayStored(c.Content, c.Category, 160)); err != nil {
			return err
		}
	}
	switch {
	case !v.Applied && len(v.Candidates) > 0:
		if _, err := fmt.Fprintf(w, "nothing removed — pass --apply to remove the %d %s above; each removal leaves a delete record in memory_history\n",
			len(v.Candidates), pluralNoun(len(v.Candidates))); err != nil {
			return err
		}
	case v.Applied && len(v.Candidates) > 0:
		if _, err := fmt.Fprintf(w, "removed %d %s; ghost history <id> still reports each one, with phase=delete\n",
			v.Removed, pluralNoun(v.Removed)); err != nil {
			return err
		}
	case v.Applied:
		if _, err := fmt.Fprintln(w, "no expired session memories past the grace period — nothing removed"); err != nil {
			return err
		}
	}
	return nil
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
