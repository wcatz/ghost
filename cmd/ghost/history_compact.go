package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wcatz/ghost/internal/mcpinit"
	"github.com/wcatz/ghost/internal/memory"
)

// `ghost history compact` — the repair for what the pre-#727 reflect behaviour
// left in a store (#730). #727 stopped every applied reflection from appending a
// byte-identical `reflect` version per kept memory and from moving that memory's
// updated_at to the run's own time. This removes the versions already stored and,
// with --fix-updated-at, puts updated_at back where the last real change was.
//
// The store does the work (internal/memory/history_compact.go). What lives here
// is the three things only a command can answer: which projects to touch, whether
// it is safe to touch them now, and what an operator is told.

// historyCompactOptions is `ghost history compact`'s parsed arguments. The zero
// value is a dry run over every project, and that is deliberate: the repair
// rewrites a table nobody reading the command is looking at, and the first thing
// an operator needs to know about it is how much of it there is.
type historyCompactOptions struct {
	// Apply writes. Without it nothing is removed and no stamp moves.
	Apply bool
	// FixUpdatedAt also restores each live memory's updated_at to the recorded_at
	// of its anchor — the newest version this repair will not remove, which is not
	// the same as the newest version that changed state, because a deliberate
	// writer's row records the state of its predecessor and bumps the stamp on
	// purpose.
	FixUpdatedAt bool
	// Project scopes the run to one project, by the same identifiers every other
	// --project on this CLI takes: an id, a name, or a path.
	Project string
	// Before bounds the repair to versions recorded before a time, spelled the way
	// memory.ResolveCompactCutoff reads: a 2006-01-02 date or an RFC 3339 instant.
	// Empty is the default, and the default is the safe one rather than the
	// absent-minded one: the instant #727 shipped, so a version a current build
	// wrote is never removed even where the state columns cannot tell it from the
	// damage. An operator whose store's clock is behind — a restored backup, a
	// copied database — widens it deliberately, and the report names whichever
	// bound was used.
	Before string
}

// parseHistoryCompactArgs parses `ghost history compact [flags]`. Only --apply,
// --fix-updated-at, --project and --before are recognized, and anything else is an
// error rather than being ignored: a silently dropped --fix-updated-at would report
// a repair that never ran, and a silently dropped --project would compact a store
// the reader believed they had narrowed.
//
// --before is checked here as well as in the store, and that is not a second
// parser: a bound the command cannot read is refused before it opens a single
// project, which is the only place the refusal is free — from inside the store it
// would arrive having already told the operator the first of twenty projects was
// compactable.
func parseHistoryCompactArgs(args []string) (historyCompactOptions, error) {
	var opts historyCompactOptions
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--apply":
			opts.Apply = true
		case arg == "--fix-updated-at":
			opts.FixUpdatedAt = true
		case arg == "--project":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("flag %s needs a value", arg)
			}
			i++
			if args[i] == "" {
				return opts, fmt.Errorf("flag %s needs a value", arg)
			}
			opts.Project = args[i]
		case strings.HasPrefix(arg, "--project="):
			value := strings.TrimPrefix(arg, "--project=")
			if value == "" {
				return opts, fmt.Errorf("flag --project needs a value")
			}
			opts.Project = value
		case arg == "--before":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("flag %s needs a value", arg)
			}
			i++
			if args[i] == "" {
				return opts, fmt.Errorf("flag %s needs a value", arg)
			}
			opts.Before = args[i]
		case strings.HasPrefix(arg, "--before="):
			value := strings.TrimPrefix(arg, "--before=")
			if value == "" {
				return opts, fmt.Errorf("flag --before needs a value")
			}
			opts.Before = value
		case strings.HasPrefix(arg, "-"):
			return opts, fmt.Errorf("unknown flag %q", arg)
		default:
			return opts, fmt.Errorf("history compact takes flags, got %q", arg)
		}
	}
	if _, err := memory.ResolveCompactCutoff(opts.Before); err != nil {
		return opts, err
	}
	return opts, nil
}

// historyCompactReport is what one run found, per project.
//
// Apply is carried on the report rather than inferred from the counts, because a
// dry run that removed nothing and an apply that removed nothing print the same
// numbers and mean opposite things: one is a preview and one is a finished
// repair. A reader told "0 removed" cannot tell which, and the difference is
// whether the store is fixed.
type historyCompactReport struct {
	// Apply is whether this run wrote.
	Apply bool
	// Before is the bound the run used, normalized to the store's stamp layout.
	// It is on the report rather than only in the caller's hands because every
	// count below is a count AT a bound: "18 redundant versions" is a different
	// number at each one, and a report printing the number without the bound is
	// reporting half an answer. An operator with a store whose clock is behind has
	// no other way to tell whether the default bound already reached their rows.
	Before string
	// Warnings is what the operator has to be told BEFORE the counts, and it is
	// here rather than printed as it is discovered because the plan decides and the
	// command prints: a warning on the wrong stream is one a script never sees, and
	// a warning interleaved with the report is one a reader skims past on the way
	// to the number they asked for.
	Warnings []string
	// Projects is one line per project, in the store's own order. A project that
	// FAILED is in here too, carrying the batches it committed before the failure:
	// they are committed one at a time, so a large store is already rewritten by
	// the time anything goes wrong, and dropping the project for having failed
	// would tell an operator to re-run the whole store.
	Projects []memory.HistoryCompactResult
	// FailedProject is the project the run stopped in, or "" if it finished. It
	// marks ONE line rather than colouring the whole report: the other projects'
	// lines are finished repairs, and a marker on every line would say nothing.
	FailedProject string
}

// printHistoryCompact renders the report: a header saying whether this run wrote and
// which bound it used, then one line per project naming it and both counts. The
// header is not decoration — it is the line that answers "is the store fixed yet",
// which the counts alone cannot when both are zero, and the bound is what makes the
// counts mean anything.
func printHistoryCompact(w io.Writer, r historyCompactReport) error {
	mode := "dry run — nothing was written; pass --apply to write"
	if r.Apply {
		mode = "compacted"
	}
	if _, err := fmt.Fprintf(w, "history compact (%s)\n", mode); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  removing only versions recorded before %s\n", r.Before); err != nil {
		return err
	}
	if len(r.Projects) == 0 {
		// Not silence: a run that touched no project and printed nothing reads
		// as "nothing to do", which is a different claim from "there are no
		// projects here".
		_, err := fmt.Fprintln(w, "  no projects in this store")
		return err
	}
	if err := printHistoryCompactLines(w, r); err != nil {
		return err
	}
	if !r.Apply {
		_, err := fmt.Fprintln(w, "Nothing removed and no updated_at moved. Re-run with --apply to write.")
		return err
	}
	return nil
}

// printHistoryCompactLines is one line per project, with no header and no footer. It
// is split out because a failed run needs the same lines under a different header:
// the counts must read identically whether a run finished or stopped partway, or
// the partial report is not a report the operator can compare against.
func printHistoryCompactLines(w io.Writer, r historyCompactReport) error {
	for _, p := range r.Projects {
		if _, err := fmt.Fprintf(w, "  %s  %d redundant version(s), %d updated_at restored", p.ProjectID, p.Removed, p.UpdatedAt); err != nil {
			return err
		}
		// Disclosed rather than folded into a total: a row whose stamp no layout
		// reads is a row this run could not repair, and a report whose numbers add
		// up to "nothing left to do" while one row is still wrong is a report that
		// says the repair finished.
		if p.StampsUnreadable > 0 {
			if _, err := fmt.Fprintf(w, " (%d stamp(s) unreadable, left alone)", p.StampsUnreadable); err != nil {
				return err
			}
		}
		// And the third outcome, disclosed for the same reason and named separately
		// because it is a different fact: the stamp is readable and the history
		// simply records no write that moved it, so there is nothing to restore it
		// to. A count of zero fixes beside a store full of no-op reflects is the
		// wrong reading, and this is the clause that stops it.
		if p.StampsUnrecorded > 0 {
			if _, err := fmt.Fprintf(w,
				" (%d stamp(s) not restorable, no recorded stamp write)", p.StampsUnrecorded); err != nil {
				return err
			}
		}
		// The same reasoning, for the same reason: these are the batches this
		// project committed before the run stopped, and a line that reads exactly
		// like a finished project's would be counted as a finished repair.
		if p.ProjectID == r.FailedProject {
			if _, err := fmt.Fprint(w, " (stopped partway here — these are the batches it committed)"); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
	return nil
}

// runHistoryCompact implements `ghost history compact`. The store is opened with
// memory.OpenDB, like every other report-style command and for the reason
// runHistory gives: a read-write open, so a store predating the history table is
// migrated rather than refused. This one WRITES under --apply, and the usage says
// so.
func runHistoryCompact(args []string) {
	opts, err := parseHistoryCompactArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n\n%s", err, historyUsage)
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
		// Not a refusal and not success: there is no store, so there is no
		// history to compact, and saying so is the answer.
		fmt.Fprintf(os.Stderr, "no Ghost database at %s — nothing to compact\n", dbPath)
		os.Exit(1)
	}

	db, err := memory.OpenDB(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close() //nolint:errcheck

	report, err := runHistoryCompactPlan(context.Background(), memory.NewStore(db, nil), dataDir, opts)
	if err != nil {
		// Whatever this run already compacted is printed BEFORE the error, and it
		// reaches stderr because the exit code is a failure: a run that compacted
		// two projects of five and then failed has rewritten part of the store,
		// and the one report whose job is to say how much of it this run touched
		// cannot be the report that is silent about it. Not printing a partial
		// report as if it were a finished repair is a separate concern, and it is
		// why the header says "before it failed".
		if werr := printPartialHistoryCompact(os.Stderr, report); werr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", werr)
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	// The warnings go to stderr and BEFORE the report, so they are not scrolled past
	// by it and a script reading stdout still sees them on the stream a diagnostic
	// belongs on. The partial path above reaches the same warnings through
	// printPartialHistoryCompact, which is what makes a failed run's report carry
	// the bound its numbers were taken at.
	if err := printHistoryCompactWarnings(os.Stderr, report); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if err := printHistoryCompact(os.Stdout, report); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// runHistoryCompactPlan is runHistoryCompact without the store, the streams or
// the exits: resolve the projects, refuse if one is busy, compact each, and
// return the report. It is a function because runHistoryCompact ends in
// os.Exit, and the refusal is the part of this command most worth testing.
func runHistoryCompactPlan(ctx context.Context, s *memory.Store, dataDir string,
	opts historyCompactOptions) (historyCompactReport, error) {
	// The bound first, and it is the one thing resolved before a single project is
	// opened. A run that cannot read its own --before would otherwise get as far as
	// the first project's write, and the refusal would arrive having already said
	// the store was compactable. The resolved value replaces the raw one so there
	// is exactly one bound in flight, and ResolveCompactCutoff is idempotent, so
	// the store resolving it again lands on the same stamp.
	before, err := memory.ResolveCompactCutoff(opts.Before)
	if err != nil {
		return historyCompactReport{}, err
	}
	opts.Before = before
	// The default, read from the store rather than written here, so the warning
	// cannot quote a bound the store does not use.
	defaultCut, err := memory.ResolveCompactCutoff("")
	if err != nil {
		return historyCompactReport{}, err
	}

	ids, err := compactTargetProjects(ctx, s, opts.Project)
	if err != nil {
		return historyCompactReport{}, err
	}

	// Every project checked BEFORE any project is touched. A run that compacted one
	// project and then found a second one locked has already written, and the
	// refusal that was supposed to protect the store arrived after the store
	// changed — which is the shape of a check that is not a check.
	if err := refuseLockedProjects(dataDir, ids); err != nil {
		return historyCompactReport{}, err
	}

	report := historyCompactReport{Apply: opts.Apply, Before: before}
	// The widened-bound warning, once, before any project is touched, and it is
	// emitted for a dry run as well as an apply. A dry run is where an operator
	// decides whether to apply, so a risk that only the apply realises — and
	// `--before 2026-10-01 --apply` realises it in the same command — has to be on
	// the screen while they are reading the numbers, not after.
	//
	// Not a refusal, because there is a store this is the only correct bound for:
	// one whose clock was behind when the rows were written. A refusal would leave
	// an operator with a store full of damage the command can repair and no way to
	// ask it to.
	if memory.WidenedCompactCutoff(before) {
		report.Warnings = append(report.Warnings, fmt.Sprintf(
			"warning: --before %s reaches past %s, the instant #727 shipped, so this run can remove "+
				"'reflect' versions a current build wrote — including the byte-identical ones a "+
				"consolidation merge files when the survivor carries the union of its sources' tags, "+
				"which this table has no column for and so cannot tell from the pre-#727 damage it "+
				"looks like. With --fix-updated-at it can also move a live memory's updated_at back "+
				"over one of them. Read the dry run's counts against what you expect to find before "+
				"passing --apply.", before, defaultCut))
	}
	for _, id := range ids {
		res, err := compactOneProject(ctx, s, id, opts)
		if err != nil {
			// The error names the project it happened in, and the report goes back
			// with it rather than being dropped. Each project is its own set of
			// committed batches, so by the time project N fails the projects before
			// it HAVE been rewritten — and so has N, which is why its own partial
			// counts are appended rather than discarded. Returning an empty report
			// here would answer "this run changed nothing" about a store it changed,
			// and returning N's counts without marking them would answer "this
			// project is finished".
			report.Projects = append(report.Projects, res)
			report.FailedProject = id
			return report, fmt.Errorf("project %s: %w", id, err)
		}
		report.Projects = append(report.Projects, res)
	}
	return report, nil
}

// compactOneProject is the per-project call, behind a var so the ONE path that can
// destroy part of a store is reachable from a test. A mid-run failure is
// unreachable otherwise — a store that answers for the first project and not the
// second does not exist in a fixture — and it is exactly the path where the report
// has to carry what already happened. The seam therefore returns the store's
// counts alongside the error, because that is what the store does.
var compactOneProject = func(ctx context.Context, s *memory.Store, projectID string,
	opts historyCompactOptions) (memory.HistoryCompactResult, error) {
	return s.CompactHistory(ctx, projectID, memory.HistoryCompactOptions{
		Apply: opts.Apply, FixUpdatedAt: opts.FixUpdatedAt, Before: opts.Before,
	})
}

// printHistoryCompactWarnings writes the risks the operator has to be told about
// before the counts, and it writes nothing at all for a report with none — a blank
// line where a warning would have been is its own kind of noise, and the default
// configuration has nothing to warn about.
//
// It is a function rather than an inline loop because TWO paths need it and the one
// that needed it second is the one a run reaches after it has already written:
// printPartialHistoryCompact. A widened bound is decided before any project is
// opened, so a run that fails on project 3 has removed rows under that bound and
// possibly moved stamps, and the report that comes back is the one the operator
// will read — omitting the bound from it is a report whose numbers say what a run
// did while the risk that shaped them is withheld.
func printHistoryCompactWarnings(w io.Writer, r historyCompactReport) error {
	for _, warning := range r.Warnings {
		if _, err := fmt.Fprintln(w, warning); err != nil {
			return err
		}
	}
	return nil
}

// printPartialHistoryCompact writes the projects a failed run had already reached,
// under a header that cannot be read as a completed repair. It writes nothing for a
// report with no project in it, which is the refusal case: nothing was touched, and
// there is nothing to disclose.
//
// The warnings come first, and they are the reason this function is not just a
// header plus a call to printHistoryCompactLines: a run that failed has already
// written whatever the bound allowed, so the bound is the context its counts are
// read in.
func printPartialHistoryCompact(w io.Writer, r historyCompactReport) error {
	if len(r.Projects) == 0 {
		return nil
	}
	if err := printHistoryCompactWarnings(w, r); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "history compact stopped partway. These projects were already %s:\n",
		map[bool]string{true: "compacted", false: "read"}[r.Apply]); err != nil {
		return err
	}
	// The bound, on this path too, in the same words the completed report uses.
	// A count is a count OF a set, and the set is defined by the bound: "17
	// redundant versions" beside a bound the operator cannot see is a number they
	// cannot check against their own store, and on a failed run it is the number
	// most likely to be wrong, because the failure may have been the bound being
	// unparseable in the first place. The completed report says the same line, and
	// a partial report that dropped it would make the two disagree about the same
	// run.
	if _, err := fmt.Fprintf(w, "  removing only versions recorded before %s\n", r.Before); err != nil {
		return err
	}
	return printHistoryCompactLines(w, r)
}

// compactTargetProjects is the set of project ids this run touches, in the store's
// own order, resolved from the --project selector.
//
// An unresolvable selector is an error rather than an empty set: `--project typo`
// compacting the whole store is the worst answer available here, and an empty
// report would say "no projects" rather than "you named one that does not exist".
func compactTargetProjects(ctx context.Context, s *memory.Store, project string) ([]string, error) {
	if project == "" {
		list, err := s.ListProjects(ctx)
		if err != nil {
			return nil, fmt.Errorf("list projects: %w", err)
		}
		ids := make([]string, 0, len(list))
		for _, p := range list {
			ids = append(ids, p.ID)
		}
		return ids, nil
	}
	id, _, err := s.ResolveProject(ctx, project)
	if err != nil {
		return nil, fmt.Errorf("resolve project %q: %w", project, err)
	}
	if id == "" {
		return nil, fmt.Errorf("no project resolves to %q — nothing was compacted", project)
	}
	return []string{id}, nil
}

// refuseLockedProjects turns a live per-project lifecycle claim into a refusal.
//
// Every locked project is NAMED, and the whole set is named at once: a store with
// twenty projects and one busy one has to be told which to wait for, and it has to
// be told before it starts. The check is a reader and not a claim
// (mcpinit.LifecycleLockHeld), so asking neither takes the lock the running
// lifecycle holds nor creates a file in a store this command was asked only to
// look at.
func refuseLockedProjects(dataDir string, projectIDs []string) error {
	var busy []string
	for _, id := range projectIDs {
		if mcpinit.LifecycleLockHeld(dataDir, id) {
			busy = append(busy, id)
		}
	}
	if len(busy) == 0 {
		return nil
	}
	sort.Strings(busy)
	what := "a lifecycle run holds"
	if len(busy) > 1 {
		what = "lifecycle runs hold"
	}
	return fmt.Errorf("%s the lock for %s — wait for it to finish, or scope the run with --project",
		what, strings.Join(busy, ", "))
}
