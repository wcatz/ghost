package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/mcpinit"
	"github.com/wcatz/ghost/internal/memory"
)

type historyCompactOptions struct {
	Apply        bool
	FixUpdatedAt bool
	Project      string
	Before       string
}

func parseHistoryCompactArgs(args []string) (historyCompactOptions, error) {
	var opts historyCompactOptions
	projectSeen := false
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
			if projectSeen {
				return opts, fmt.Errorf("flag %s given more than once", arg)
			}
			i++
			if args[i] == "" {
				return opts, fmt.Errorf("flag %s needs a value", arg)
			}
			opts.Project = args[i]
			projectSeen = true
		case strings.HasPrefix(arg, "--project="):
			if projectSeen {
				return opts, fmt.Errorf("flag --project given more than once")
			}
			value := strings.TrimPrefix(arg, "--project=")
			if value == "" {
				return opts, fmt.Errorf("flag --project needs a value")
			}
			opts.Project = value
			projectSeen = true
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

type historyCompactReport struct {
	Apply         bool
	Before        string
	Warnings      []string
	Projects      []memory.HistoryCompactResult
	FailedProject string
}

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

func printHistoryCompactLines(w io.Writer, r historyCompactReport) error {
	for _, p := range r.Projects {
		rendered := assemble.Token(p.ProjectID)
		if placeholder, withheld := memory.TokenGuardPlaceholder("project", p.ProjectID); withheld {
			rendered = placeholder
		}
		if _, err := fmt.Fprintf(w, "  %s  %d redundant version(s), %d updated_at restored", rendered, p.Removed, p.UpdatedAt); err != nil {
			return err
		}
		if p.StampsUnreadable > 0 {
			if _, err := fmt.Fprintf(w, " (%d stamp(s) unreadable, left alone)", p.StampsUnreadable); err != nil {
				return err
			}
		}
		if p.StampsUnrecorded > 0 {
			if _, err := fmt.Fprintf(w,
				" (%d stamp(s) not restorable, no recorded stamp write)", p.StampsUnrecorded); err != nil {
				return err
			}
		}
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
		if werr := printPartialHistoryCompact(os.Stderr, report); werr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", werr)
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if err := printHistoryCompactWarnings(os.Stderr, report); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if err := printHistoryCompact(os.Stdout, report); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func runHistoryCompactPlan(ctx context.Context, s *memory.Store, dataDir string,
	opts historyCompactOptions) (historyCompactReport, error) {
	before, err := memory.ResolveCompactCutoff(opts.Before)
	if err != nil {
		return historyCompactReport{}, err
	}
	opts.Before = before
	defaultCut, err := memory.ResolveCompactCutoff("")
	if err != nil {
		return historyCompactReport{}, err
	}

	ids, err := compactTargetProjects(ctx, s, opts.Project)
	if err != nil {
		return historyCompactReport{}, err
	}

	if err := refuseLockedProjects(dataDir, ids); err != nil {
		return historyCompactReport{}, err
	}

	report := historyCompactReport{Apply: opts.Apply, Before: before}
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
			report.Projects = append(report.Projects, res)
			report.FailedProject = id
			return report, fmt.Errorf("project %s: %w", memory.RefusalArg("project", id), err)
		}
		report.Projects = append(report.Projects, res)
	}
	return report, nil
}

var compactOneProject = func(ctx context.Context, s *memory.Store, projectID string,
	opts historyCompactOptions) (memory.HistoryCompactResult, error) {
	return s.CompactHistory(ctx, projectID, memory.HistoryCompactOptions{
		Apply: opts.Apply, FixUpdatedAt: opts.FixUpdatedAt, Before: opts.Before,
	})
}

func printHistoryCompactWarnings(w io.Writer, r historyCompactReport) error {
	for _, warning := range r.Warnings {
		if _, err := fmt.Fprintln(w, warning); err != nil {
			return err
		}
	}
	return nil
}

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
	if _, err := fmt.Fprintf(w, "  removing only versions recorded before %s\n", r.Before); err != nil {
		return err
	}
	return printHistoryCompactLines(w, r)
}

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
		return nil, fmt.Errorf("resolve project %s: %w", memory.RefusalArg("project", project), err)
	}
	if id == "" {
		return nil, fmt.Errorf("no project resolves to %s — nothing was compacted", memory.RefusalArg("project", project))
	}
	return []string{id}, nil
}

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
	rendered := make([]string, 0, len(busy))
	for _, id := range busy {
		rendered = append(rendered, memory.RefusalArg("project", id))
	}
	what := "a lifecycle run holds"
	if len(busy) > 1 {
		what = "lifecycle runs hold"
	}
	return fmt.Errorf("%s the lock for %s — wait for it to finish, or scope the run with --project",
		what, strings.Join(rendered, ", "))
}
