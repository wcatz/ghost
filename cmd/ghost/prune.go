package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/memory"
)

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

type pruneOptions struct {
	Apply   bool
	Grace   time.Duration
	Project string
}

func parsePruneArgs(args []string) (pruneOptions, error) {
	var opts pruneOptions
	seenProject := false
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
			if seenProject {
				return opts, fmt.Errorf("--project was given twice (%s and %s)",
					memory.RefusalArg("project", opts.Project), memory.RefusalArg("project", value))
			}
			if value == "" {
				return opts, errors.New("--project was given an empty value (drop it to prune every project, or name one explicitly)")
			}
			seenProject = true
			opts.Project = value
		case strings.HasPrefix(arg, "-"):
			return opts, fmt.Errorf("unknown flag %q", arg)
		default:
			return opts, fmt.Errorf("ghost prune takes flags only, got %q", arg)
		}
	}
	return opts, nil
}

type pruneView struct {
	NoDatabase bool
	Applied    bool
	Grace      time.Duration
	Now        time.Time
	Scope      string
	Candidates []memory.PruneCandidate
	Removed    int
	RemovedIDs []string
}

func pluralNoun(n int) string {
	if n == 1 {
		return "memory"
	}
	return "memories"
}

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
	case v.Applied:
		if _, err := fmt.Fprintln(w, "no expired session memories past the grace period — nothing removed"); err != nil {
			return err
		}
	}
	return nil
}

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

func printPruneRow(w io.Writer, c memory.PruneCandidate, indent string) error {
	activity := c.ActivityAt
	if activity == "" {
		activity = "(no recorded activity)"
	}
	if _, err := fmt.Fprintf(w, "%s%s  %s  %s  expired %s  last touched %s", indent, assemble.Token(c.ID), c.Category, c.Retention, c.ExpiresAt, activity); err != nil {
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
	if _, err := fmt.Fprintf(w, "%s  %s\n", indent, displayStored(c.Content, c.Category, 160)); err != nil {
		return err
	}
	return nil
}

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

	scope := ""
	if opts.Project != "" {
		resolved, _, rErr := store.ResolveProject(ctx, opts.Project)
		if rErr != nil {
			fmt.Fprintf(os.Stderr, "error: resolve project %s: %v\n", memory.RefusalArg("project", opts.Project), rErr)
			os.Exit(1)
		}
		if resolved == "" {
			fmt.Fprintf(os.Stderr, "error: project %s not found — nothing was pruned\n", memory.RefusalArg("project", opts.Project))
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
