package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/repo"
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

	_, _, store := bootstrap(os.Stderr, cliLogLevel(), failOnConfig)
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

// resolveProjectBindID checks the project argument of `ghost project bind`
// names an existing project id, or reports the known names and fails. It is
// the exact-id lookup rather than ResolveProject on purpose: bind is the
// command a user runs *because* a project does not resolve from its directory,
// so the name and path steps of the full resolver are the ones least likely to
// produce the project they meant, and a name that matches two projects would be
// a coin flip. A miss therefore names the argument as wrong instead of
// silently binding whichever project looked closest.
func resolveProjectBindID(ctx context.Context, store *memory.Store, projectID string) (string, error) {
	if _, ok, err := store.ResolveExactProjectID(ctx, projectID); err != nil {
		return "", err
	} else if !ok {
		if names := knownProjectNames(ctx, store); len(names) > 0 {
			return "", fmt.Errorf("project %q not found (bind takes a project id). Known projects: %s",
				projectID, strings.Join(names, ", "))
		}
		return "", fmt.Errorf("project %q not found (bind takes a project id)", projectID)
	}
	return projectID, nil
}

// runProjectBindCore implements `ghost project bind <project> <path>` against
// an already-open store and prints what changed. detectRemote is injected
// because it is the only step that has to ask git, and the command's tests
// must not spawn a process: the CLI passes repo.DetectRemote, the same detector
// main wires into the store for resolution.
//
// The path is resolved here rather than in the store because this package is
// the one allowed to look at the filesystem. It is made absolute and cleaned,
// and must be an existing directory — binding a path that does not exist would
// record a location no session can ever stand in, which is the state the
// command exists to leave. Everything after that (the recorded-path rules, the
// claim checks, the write) belongs to the store, which owns them.
//
// Pulled out of runProjectBind — which owns arg parsing, bootstrap() and
// os.Exit — so the refusals are testable without process-exit paths.
func runProjectBindCore(ctx context.Context, store *memory.Store, out io.Writer, projectID, path string, detectRemote func(string) string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("no path given — usage: ghost project bind <project-id> <checkout-directory>")
	}
	id, err := resolveProjectBindID(ctx, store, projectID)
	if err != nil {
		return err
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("cannot resolve %q to an absolute path: %w", path, err)
	}
	abs = filepath.Clean(abs)

	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no such directory: %s", abs)
		}
		return fmt.Errorf("cannot read %s: %w", abs, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory: %s", abs)
	}
	// The filesystem root, and only the root, survives Abs+Stat: it is
	// absolute and it exists, and it is exactly the path that would claim
	// every directory on the machine.
	if !memory.StoredPathIsUsable(abs) {
		return fmt.Errorf("refusing to bind %s: it contains every directory, so sessions anywhere would resolve to this project", abs)
	}

	remote := ""
	if detectRemote != nil {
		remote = detectRemote(abs)
	}

	binding, err := store.BindProjectPath(ctx, id, abs, remote)
	if err != nil {
		return err
	}
	return printBinding(out, binding)
}

// printBinding reports the outcome in one of two shapes: the fields that moved
// for a real bind, and a single line saying so when the command was a re-run,
// so a user who cannot remember whether they already ran it learns that from
// the output instead of having to look.
func printBinding(out io.Writer, binding memory.ProjectBinding) error {
	label := projectLabel(binding.Name, binding.ProjectID)
	if !binding.Changed() {
		_, err := fmt.Fprintf(out, "already bound: %s → %s\n", label, binding.Path)
		return err
	}
	if _, err := fmt.Fprintf(out, "bound %s → %s\n", label, binding.Path); err != nil {
		return err
	}
	if binding.PathChanged {
		if _, err := fmt.Fprintf(out, "  path: %s → %s\n", binding.PreviousPath, binding.Path); err != nil {
			return err
		}
	}
	if binding.RemoteSet {
		if _, err := fmt.Fprintf(out, "  repo_remote: (none) → %s\n", binding.RepoRemote); err != nil {
			return err
		}
	}
	return nil
}

// projectLabel names a project for a human, preferring "name (id)" so the id
// the command needs stays visible even when the name is what the user knows.
func projectLabel(name, id string) string {
	if name == "" || name == id {
		return id
	}
	return fmt.Sprintf("%s (%s)", name, id)
}

// openDiagnosticStore opens the Ghost database for a command that only
// reports on it, or returns nil when there is nothing to report. It never
// creates the file: a diagnostic that bootstraps the store it is diagnosing
// makes its own "no database" line untrue on the next run. Every failure — a
// missing database, an unreadable one, a data dir that cannot be resolved —
// is nil rather than an error, because the caller has nothing to add to a
// report another check already produced.
func openDiagnosticStore() *memory.Store {
	// DataDirPath, not DataDir: the latter creates the directory, and this
	// helper's whole contract is that it leaves nothing behind. mcpinit's
	// non-creating paths use the same choice.
	dataDir, err := config.DataDirPath()
	if err != nil {
		return nil
	}
	db, err := memory.OpenDBReadOnly(filepath.Join(dataDir, "ghost.db"))
	if err != nil {
		return nil
	}
	// A nil logger is honoured as silence; this store never writes, so it has
	// nothing to report about itself.
	return memory.NewStore(db, nil)
}

// writeUnboundProjectNotice reports the projects a session directory can never
// resolve, each with the command that fixes it. It is called by `ghost mcp
// status` and prints nothing when there is nothing to fix, so a healthy install
// gains no new output.
func writeUnboundProjectNotice(ctx context.Context, out io.Writer, store *memory.Store) error {
	unbound, err := store.ListUnboundProjects(ctx)
	if err != nil {
		return fmt.Errorf("list unbound projects: %w", err)
	}
	if len(unbound) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(out, "\nProjects with no bound checkout (sessions in them get no injected context):"); err != nil {
		return err
	}
	for _, p := range unbound {
		if _, err := fmt.Fprintf(out, "  %s — run: ghost project bind %s /path/to/checkout\n",
			projectLabel(p.Name, p.ID), p.ID); err != nil {
			return err
		}
	}
	// The limit is stated rather than papered over. This notice tests the
	// recorded path's SHAPE, so a project bound to a checkout that has since
	// been moved or deleted is not listed — it needs the same command with the
	// new path, and detecting it would mean a stat whose transient failure
	// (an unmounted volume, a permission error) would invite an overwrite of a
	// path that was correct a moment ago.
	_, err = fmt.Fprintln(out, "  A recorded path that no longer exists is not detected here — re-bind it with the new path.")
	return err
}

// projectBindUsage is the help for `ghost project bind`. It goes to stdout for
// -h/--help, which a user reaches for precisely because they do not remember
// the syntax, and to stderr alongside an error otherwise.
const projectBindUsage = `Usage: ghost project bind <project-id> <checkout-directory>

Gives a project a recorded checkout, so a session in that directory resolves it.
A project created over MCP records only a name, and a project upgraded from a
v9 database recorded its name as its path, so until something records a real
path or a repository remote no directory resolves them and they get no
session-start context or lifecycle work. ghost mcp status lists them.

The directory is stored as its physical (symlink-resolved) path. Refuses
_global, an unknown project, a path that another project already records or
that contains one, a path inside another project that has no repository
remote, a path resolution could never match, and a repository another project
already claims. Safe to re-run.
`

// parseProjectBindArgs splits `ghost project bind`'s arguments. Help wins over
// everything else, so "ghost project bind -h" answers the question rather than
// complaining about the missing project.
func parseProjectBindArgs(args []string) (projectID, path string, showHelp bool, err error) {
	var positional []string
	for _, a := range args {
		switch {
		case a == "-h" || a == "--help":
			return "", "", true, nil
		case strings.HasPrefix(a, "-"):
			return "", "", false, fmt.Errorf("unknown flag %q", a)
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) != 2 {
		return "", "", false, fmt.Errorf("expected a project id and a checkout directory, got %d argument(s)", len(positional))
	}
	return positional[0], positional[1], false, nil
}

// runProjectBind implements `ghost project bind <project-id> <checkout>`.
func runProjectBind() {
	project, path, showHelp, err := parseProjectBindArgs(os.Args[3:])
	if showHelp {
		fmt.Fprint(os.Stdout, projectBindUsage)
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n\n%s", err, projectBindUsage)
		os.Exit(1)
	}

	_, _, store := bootstrap(os.Stderr, cliLogLevel())
	defer store.Close() //nolint:errcheck

	// repo.DetectRemote is the same detector main injects into the store, so
	// the remote recorded here is the remote resolution will later compare
	// against — two spellings of one repository, not two identities. It is
	// asked about the path as typed, which is the same repository: git
	// resolves the symlinks on the way in, and the store records the physical
	// path either way.
	if err := runProjectBindCore(context.Background(), store, os.Stdout, project, path, repo.DetectRemote); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
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

	_, _, store := bootstrap(os.Stderr, cliLogLevel(), failOnConfig)
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
