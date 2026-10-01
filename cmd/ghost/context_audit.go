package main

// `ghost context --audit` (#646 part 3): the report surface.
//
// It is a MODE of `ghost context` rather than a command of its own, and that is a
// decision worth stating. The report is read-only while the block it shares a
// command with is not — printing the session-start block mirrors Obsidian and
// bumps a session count — so a reader looking at `ghost context --help` sees one
// command with two modes and has to read which one is which. A separate
// `ghost audit` would avoid that, and would also add a command, a help entry, a
// line in docs/invariants.md's inventory and a slot in this CLI's dispatch for a
// thing that is one report. The mode is the cheaper shape, and the usage text says
// plainly that the two share almost nothing.
//
// Read-only is the load-bearing property, and it is inherited rather than
// reimplemented: dataDirPath resolves the directory through config.DataDirPath, so
// GHOST_DEV_FORBID_DATA_DIR refuses this path exactly as it refuses `ghost export`
// (#721), and openReadOnlyTransferStore opens memory.OpenReadDB — no migration, no
// builtin seeding, no file creation — and then refuses a store whose user_version
// is not this build's. That refusal is inherited deliberately rather than worked
// around: a report that migrated the store would be indistinguishable from one that
// did not, right up until the user wanted their database back.
//
// The scope is ALWAYS one project, resolved rather than pooled. audit.BuildReport
// refuses an empty project id for the same reason it refuses to pool sources, and
// the CLI does not route around it: with no --project the report is for the
// project the DIRECTORY resolves to, which is the scope `ghost context` already
// works in and the one its reader is standing in. A directory that resolves to no
// project is an error naming --project, never a store-wide report — the pooled
// figure is the reading that would quietly answer a different question than the one
// asked.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/audit"
	"github.com/wcatz/ghost/internal/memory"
)

// contextAuditOptions is one `ghost context --audit` invocation's arguments.
type contextAuditOptions struct {
	// Audit marks the mode. The parser sets it, so no caller of the parser can
	// build a "parsed audit options" value that forgets it is an audit.
	Audit bool
	// Project scopes the report, by name or by id. Empty means "the project this
	// directory resolves to", which is the scope the block above already reports
	// on and the scope the reader is standing in.
	Project string
	// Cwd is the directory that scope resolves through when Project is empty. It
	// is the same --cwd the block mode takes, so one command has one answer to
	// "which directory is this about".
	Cwd string
	// Since bounds the report by recorded_at on both retrieval tables. Zero means
	// everything the store still holds, and the report prints which of the two it
	// is, because a figure without its window is a standing state and a window is
	// not.
	Since time.Duration
}

// contextAuditRequested reports whether this argument list asks for the audit mode.
//
// Only --audit itself counts, and not --project or --since: those are flags of the
// audit mode, but the MODE is what the bare flag says, and treating a --project
// alone as a request would make `ghost context --project ghost` ambiguous — one
// command, two readings, decided by which flag came first.
//
// It looks for the flag in the exact and attached forms so `--audit=false` reaches
// the parser and is refused there, naming what to do instead: the token looks like
// the flag but is not it, and silently treating it as the mode would be worse than
// saying so.
func contextAuditRequested(args []string) bool {
	for _, arg := range args {
		if arg == "--audit" || strings.HasPrefix(arg, "--audit=") {
			return true
		}
	}
	return false
}

// parseContextAuditArgs reads `ghost context --audit`'s flags.
//
// A strict parser, like every other here: an unknown flag is an error rather than
// something ignored. The cost of ignoring one is that a reader who typed --json
// gets a report that is not JSON and no indication that their flag was dropped.
//
// --as-of is refused, and that refusal carries a rule rather than syntax. It is a
// real flag of `ghost context` and it asks for a DIFFERENT report: the
// session-start block as the store stood at an instant. Combining them is a
// request for two things, and each available answer is a confident wrong one — the
// block ignores the audit request, or the report silently ignores the instant.
func parseContextAuditArgs(args []string) (contextAuditOptions, error) {
	opts := contextAuditOptions{Audit: true}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--audit" || strings.HasPrefix(arg, "--audit="):
			// The mode flag itself. A value is refused rather than accepted: the
			// flag is a MODE, not a setting, and "--audit=false" reads as a request
			// to turn the mode off, which is `ghost context` with no flags at all.
			if arg != "--audit" {
				return opts, errors.New("--audit takes no value (drop it to print the session-start block, or keep it alone for the report)")
			}
		case arg == "--project" || strings.HasPrefix(arg, "--project="):
			// Both forms are read inline, as parsePruneArgs reads --project, rather
			// than through a shared helper returning the next index. That shape is
			// deliberate: help_test.go's TestHelp_ValueFlagsCoverEveryParser proves
			// each registered value flag is consumed by a case clause that ADVANCES
			// this loop's index, and a helper would hide that from the scan — which
			// would then report this registration as stale and tell the author to
			// delete a correct one.
			value := ""
			if arg == "--project" {
				if i+1 >= len(args) {
					return opts, errors.New("flag --project needs a value (a project name or id)")
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
		case arg == "--since" || strings.HasPrefix(arg, "--since="):
			value := ""
			if arg == "--since" {
				if i+1 >= len(args) {
					return opts, errors.New("flag --since needs a value (a Go duration, e.g. 24h)")
				}
				i++
				value = args[i]
			} else {
				value = strings.TrimPrefix(arg, "--since=")
			}
			// The value is the next token whatever it looks like, and time.Parse is
			// left to reject it: a guard that only accepted a token which does not
			// look like a flag would have to guess, and the resulting error names
			// the value the reader typed rather than the flag they mistyped.
			d, err := time.ParseDuration(value)
			if err != nil {
				return opts, fmt.Errorf("--since %q is not a duration: %w (e.g. 24h, 168h, 30m)", value, err)
			}
			if d < 0 {
				return opts, fmt.Errorf("--since %s is negative: it is how far BACK to look, not how far forward", d)
			}
			opts.Since = d
		case arg == "--cwd" || strings.HasPrefix(arg, "--cwd="):
			value := ""
			if arg == "--cwd" {
				if i+1 >= len(args) {
					return opts, errors.New("flag --cwd needs a value (a directory)")
				}
				i++
				value = args[i]
			} else {
				value = strings.TrimPrefix(arg, "--cwd=")
			}
			if opts.Cwd != "" {
				return opts, fmt.Errorf("--cwd was given twice (%q and %q)", opts.Cwd, value)
			}
			opts.Cwd = value
		case arg == "--as-of" || strings.HasPrefix(arg, "--as-of="):
			return opts, errors.New("--as-of prints the session-start block as the store stood at an instant; it cannot be combined with --audit, which reports retrieval verdicts over a window instead (`ghost context --as-of ...` or `ghost context --audit`, not both)")
		case strings.HasPrefix(arg, "-"):
			return opts, fmt.Errorf("unknown flag %q", arg)
		default:
			return opts, fmt.Errorf("ghost context --audit takes flags only, got %q", arg)
		}
	}
	return opts, nil
}

// buildContextAudit opens the store read-only, resolves the project scope and
// builds the report.
//
// dataDir is a parameter rather than resolved here so the read-only seam is the
// one the rest of this package's reports use, and so a test can point it at a
// sandbox without touching the process's XDG environment. It is the value
// dataDirPath returns — the data directory ITSELF, not the XDG home above it,
// which is the distinction the GHOST_DEV_FORBID_DATA_DIR check is written
// against.
//
// Resolution goes through the store's own ResolveProject, so a name, an id and a
// path all mean what they mean everywhere else. A scope that resolves to nothing is
// an ERROR in both directions — a mistyped --project and a directory Ghost has
// never heard of — and neither falls back to a report over every project. That
// fallback is the one reading available here that would be worse than an error: an
// operator who mistyped a project name would get a plausible report about the whole
// corpus, with no indication that the scope they asked for was not the scope they
// got.
func buildContextAudit(ctx context.Context, opts contextAuditOptions, dataDir string) (audit.Report, error) {
	store, err := openReadOnlyTransferStore(dataDir)
	if err != nil {
		return audit.Report{}, err
	}
	defer store.Close() //nolint:errcheck

	scope := opts.Project
	if scope == "" {
		// Empty --cwd means the reader's own directory, which is what the block
		// mode does with it. resolveExplicitProjectRepoTx's path-prefix branch is
		// what makes this work, and it runs no write: an empty scope here cannot
		// create or bind a project, it can only fail to find one.
		dir := opts.Cwd
		if dir == "" {
			dir, err = os.Getwd()
			if err != nil {
				return audit.Report{}, fmt.Errorf("resolve the current directory: %w", err)
			}
		}
		// Absolute before resolving, because a relative --cwd names a path prefix
		// and the store matches path prefixes against the absolute path it stored.
		// The block mode's own resolution makes it absolute too; this is the same
		// rule applied at the same boundary rather than relying on that.
		if abs, aErr := filepath.Abs(dir); aErr == nil {
			dir = abs
		}
		scope = dir
	}
	resolved, _, rErr := store.ResolveProject(ctx, scope)
	if rErr != nil {
		return audit.Report{}, fmt.Errorf("resolve project %s: %w", memory.ProjectArg("project", scope), rErr)
	}
	if resolved == "" {
		return audit.Report{}, fmt.Errorf(
			"project %s not found — nothing was reported, and this command never reports over every project at once; name one explicitly (--project <name-or-id>) or run it inside a project directory",
			memory.ProjectArg("project", scope))
	}
	return audit.BuildReport(ctx, store, audit.ReportOptions{
		ProjectID: resolved,
		Since:     opts.Since,
	})
}

// printContextAudit renders the report.
//
// The scope line is here rather than on Report because Report is not always about
// one project — audit.MergeProjects returns a store-wide view whose ProjectID is
// empty on purpose, so a renderer that printed the field unconditionally would
// label that one "retrieval audit for ". This command is always about one project,
// so this is where the scope is stated. The figures are identical whichever way the
// scope was chosen; only the reader's certainty about which project they asked
// about differs.
func printContextAudit(w io.Writer, rep audit.Report) error {
	if _, err := fmt.Fprintf(w, "retrieval audit report for project %s\n\n", rep.ProjectID); err != nil {
		return err
	}
	_, err := io.WriteString(w, rep.String())
	return err
}

// runContextAudit implements the mode: parse, open read-only, print.
//
// Failures exit non-zero with the reason and nothing else on stdout, because a
// report that printed partial figures beside an error would be pasted into an issue
// as if it were the whole answer.
func runContextAudit(args []string) {
	opts, err := parseContextAuditArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ghost context --audit: %v\n", err)
		os.Exit(2)
	}
	dataDir, err := dataDirPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ghost context --audit: %v\n", err)
		os.Exit(1)
	}
	rep, err := buildContextAudit(context.Background(), opts, dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ghost context --audit: %v\n", err)
		os.Exit(1)
	}
	if err := printContextAudit(os.Stdout, rep); err != nil {
		fmt.Fprintf(os.Stderr, "ghost context --audit: %v\n", err)
		os.Exit(1)
	}
}
