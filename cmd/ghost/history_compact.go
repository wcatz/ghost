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
	// of the last version that changed its state.
	FixUpdatedAt bool
	// Project scopes the run to one project, by the same identifiers every other
	// --project on this CLI takes: an id, a name, or a path.
	Project string
}

// parseHistoryCompactArgs parses `ghost history compact [flags]`. Only --apply,
// --fix-updated-at and --project are recognized, and anything else is an error
// rather than being ignored: a silently dropped --fix-updated-at would report a
// repair that never ran, and a silently dropped --project would compact a store
// the reader believed they had narrowed.
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
		case strings.HasPrefix(arg, "-"):
			return opts, fmt.Errorf("unknown flag %q", arg)
		default:
			return opts, fmt.Errorf("history compact takes flags, got %q", arg)
		}
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
	// Projects is one line per project, in the store's own order.
	Projects []memory.HistoryCompactResult
}

// printHistoryCompact renders the report: a header saying whether this run wrote,
// then one line per project naming it and both counts. The header is not
// decoration — it is the line that answers "is the store fixed yet", which the
// counts alone cannot when both are zero.
func printHistoryCompact(w io.Writer, r historyCompactReport) error {
	mode := "dry run — nothing was written; pass --apply to write"
	if r.Apply {
		mode = "compacted"
	}
	if _, err := fmt.Fprintf(w, "history compact (%s)\n", mode); err != nil {
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

	report := historyCompactReport{Apply: opts.Apply}
	for _, id := range ids {
		res, err := compactOneProject(ctx, s, id, opts)
		if err != nil {
			// The error names the project it happened in, and the report so far
			// goes back with it rather than being dropped. Each project is its own
			// set of committed batches, so by the time project N fails the projects
			// before it HAVE been rewritten, and returning an empty report here
			// would answer "this run changed nothing" about a store it changed.
			// printPartialHistoryCompact is what keeps the lines from reading as a
			// finished repair.
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
// has to carry what already happened.
var compactOneProject = func(ctx context.Context, s *memory.Store, projectID string,
	opts historyCompactOptions) (memory.HistoryCompactResult, error) {
	return s.CompactHistory(ctx, projectID, memory.HistoryCompactOptions{
		Apply: opts.Apply, FixUpdatedAt: opts.FixUpdatedAt,
	})
}

// printPartialHistoryCompact writes the projects a failed run had already reached,
// under a header that cannot be read as a completed repair. It writes nothing for a
// report with no project in it, which is the refusal case: nothing was touched, and
// there is nothing to disclose.
func printPartialHistoryCompact(w io.Writer, r historyCompactReport) error {
	if len(r.Projects) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(w, "history compact stopped partway. These projects were already %s:\n",
		map[bool]string{true: "compacted", false: "read"}[r.Apply]); err != nil {
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
