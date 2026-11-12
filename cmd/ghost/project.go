package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/config"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/repo"
)

func resolveProjectOrExit(ctx context.Context, store *memory.Store, projectName string) string {
	projectID, _, err := store.ResolveProject(ctx, projectName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if projectID == "" {
		names, listErr := store.ListProjectNames(ctx)
		if listErr != nil || len(names) == 0 {
			fmt.Fprintf(os.Stderr, "error: project %s not found\n", memory.RefusalArg("project", projectName))
		} else {
			fmt.Fprintf(os.Stderr, "error: project %s not found. Known projects: %s\n", memory.RefusalArg("project", projectName), knownProjectsSentence(names))
		}
		os.Exit(1)
	}
	return projectID
}

func knownProjectsSentence(names []string) string {
	labeled := make([]string, 0, len(names))
	for _, n := range names {
		labeled = append(labeled, assemble.Label(n))
	}
	return strings.Join(labeled, ", ")
}

func confirmProjectDeleteName(typed, expected string) bool {
	return strings.TrimSpace(typed) == expected
}

func printDeleteSummary(out io.Writer, summary memory.DeleteProjectSummary, verb string) error {
	if _, err := fmt.Fprintf(out, "%s %s (%s):\n", verb, assemble.Label(summary.ProjectName), assemble.Token(summary.ProjectID)); err != nil {
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
	if _, err := fmt.Fprintf(out, "  retrievals:   %d\n", summary.RetrievalRecords); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "  flags:        %d\n", summary.MemoryFlags); err != nil {
		return err
	}
	return nil
}

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

	summary, err = store.DeleteProject(ctx, summary.ProjectID, true)
	if err != nil {
		return err
	}
	if err := printDeleteSummary(out, summary, "Deleted"); err != nil {
		return err
	}
	return nil
}

const projectUsage = `Usage: ghost project delete <name-or-id> [--apply]
       ghost project merge <old-name-or-id> <new-name-or-id>
       ghost project bind <project-id> <checkout-directory>
`

const projectDeleteUsage = `Usage: ghost project delete <name-or-id> [flags]

Flags:
  --apply   Actually delete (default is dry-run/preview)

Permanently removes a project: memories, tags, embeddings, links, tasks,
decisions, learned context, reflection snapshots, and cost/audit history.
Irreversible. Refuses to delete _global.
`

func parseProjectDeleteArgs(args []string) (projectName string, apply, showUsage bool, err error) {
	projectSeen := false
	for _, a := range args {
		switch {
		case a == "--apply":
			apply = true
		case !strings.HasPrefix(a, "-"):
			if projectSeen {
				return "", false, false, errors.New("expected exactly one project")
			}
			if a == "" {
				return "", false, false, errors.New("project name must not be empty")
			}
			projectName = a
			projectSeen = true
		default:
			return "", false, false, fmt.Errorf("unknown flag %q", a)
		}
	}
	if !projectSeen {
		return "", apply, true, nil
	}
	return projectName, apply, false, nil
}

func runProjectDelete() {
	projectName, apply, showUsage, err := parseProjectDeleteArgs(os.Args[3:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if showUsage {
		fmt.Fprint(os.Stderr, projectDeleteUsage)
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

func runProjectMergeCore(ctx context.Context, store *memory.Store, out io.Writer, oldArg, newArg string) error {
	oldID, oldName := resolveForMerge(ctx, store, oldArg)
	newID, newName := resolveForMerge(ctx, store, newArg)
	if oldID == "" {
		return fmt.Errorf("project %s not found; known projects: %s", memory.RefusalArg("project", oldArg), knownProjectsSentence(knownProjectNames(ctx, store)))
	}
	if newID == "" {
		return fmt.Errorf("project %s not found; known projects: %s", memory.RefusalArg("project", newArg), knownProjectsSentence(knownProjectNames(ctx, store)))
	}
	if oldID == newID {
		return fmt.Errorf("refusing to merge a project into itself (%s)", memory.RefusalArg(memory.ProjectIdentifierLabel, oldID))
	}
	if _, err := fmt.Fprintf(out, "Merging %q (%s) into %q (%s)\n",
		oldName, memory.RefusalArg(memory.ProjectIdentifierLabel, oldID),
		newName, memory.RefusalArg(memory.ProjectIdentifierLabel, newID)); err != nil {
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

func resolveProjectBindID(ctx context.Context, store *memory.Store, projectID string) (string, error) {
	if _, ok, err := store.ResolveExactProjectID(ctx, projectID); err != nil {
		return "", err
	} else if !ok {
		if names := knownProjectNames(ctx, store); len(names) > 0 {
			return "", fmt.Errorf("project %s not found (bind takes a project id). Known projects: %s",
				memory.RefusalArg("project", projectID), knownProjectsSentence(names))
		}
		return "", fmt.Errorf("project %s not found (bind takes a project id)", memory.RefusalArg("project", projectID))
	}
	return projectID, nil
}

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
	if !memory.StoredPathIsUsable(abs) {
		return fmt.Errorf("refusing to bind %s: it contains every directory, so sessions anywhere would resolve to this project", abs)
	}

	remote := ""
	if detectRemote != nil {
		remote = detectRemote(abs)
	}

	binding, err := store.BindProjectPath(ctx, id, abs, remote)
	if err != nil {
		if errors.Is(err, memory.ErrBindPathUnprintable) && !errors.Is(err, memory.ErrSecretContent) {
			return fmt.Errorf("%w — path %s", err, assemble.Label(abs))
		}
		return err
	}
	return printBinding(out, binding)
}

func printBinding(out io.Writer, binding memory.ProjectBinding) error {
	label := projectLabel(binding.Name, binding.ProjectID)
	path := assemble.Label(binding.Path)
	if !binding.Changed() {
		_, err := fmt.Fprintf(out, "already bound: %s → %s\n", label, path)
		return err
	}
	if _, err := fmt.Fprintf(out, "bound %s → %s\n", label, path); err != nil {
		return err
	}
	if binding.PathChanged {
		if _, err := fmt.Fprintf(out, "  path: %s → %s\n", assemble.Label(binding.PreviousPath), path); err != nil {
			return err
		}
	}
	if binding.RemoteSet {
		if _, err := fmt.Fprintf(out, "  repo_remote: (none) → %s\n", assemble.Label(binding.RepoRemote)); err != nil {
			return err
		}
	}
	if binding.PathChanged {
		if _, err := fmt.Fprintln(out, "  next: on the standalone Claude Code integration (not the ghost plugin), run\n"+
			"        `ghost mcp init` to write this checkout's memory redirect\n"+
			"        (init overwrites only a MEMORY.md it recognises as its own\n"+
			"         stale redirect — merge or remove any other one first)"); err != nil {
			return err
		}
	}
	return nil
}

func projectLabel(name, id string) string {
	if name == "" || name == id {
		return assemble.Token(id)
	}
	return fmt.Sprintf("%s (%s)", assemble.Label(name), assemble.Token(id))
}

func openDiagnosticStore() *memory.Store {
	dataDir, err := config.DataDirPath()
	if err != nil {
		return nil
	}
	db, err := memory.OpenReadDB(filepath.Join(dataDir, "ghost.db"))
	if err != nil {
		return nil
	}
	return memory.NewStore(db, nil)
}

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
			projectLabel(p.Name, p.ID), assemble.Token(p.ID)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(out, "  A recorded path that no longer exists is not detected here — re-bind it with the new path."); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, "  On the standalone Claude Code integration (not the ghost plugin), re-run\n"+
		"  `ghost mcp init` afterwards to write each new checkout's memory redirect\n"+
		"  (init overwrites only a MEMORY.md it recognises as its own stale\n"+
		"   redirect — merge or remove any other one first).")
	return err
}

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

func runProjectBind() {
	project, path, showHelp, err := parseProjectBindArgs(os.Args[3:])
	if showHelp {
		if _, err := fmt.Fprint(os.Stdout, projectBindUsage); err != nil {
			fmt.Fprintf(os.Stderr, "warning: cannot print usage: %v\n", err)
		}
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n\n%s", err, projectBindUsage)
		os.Exit(1)
	}

	_, _, store := bootstrap(os.Stderr, cliLogLevel(), failOnConfig)
	defer store.Close() //nolint:errcheck

	if err := runProjectBindCore(context.Background(), store, os.Stdout, project, path, repo.DetectRemote); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

const projectMergeUsage = `Usage: ghost project merge <old-name-or-id> <new-name-or-id>
`

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
		fmt.Fprint(os.Stderr, projectMergeUsage)
		os.Exit(1)
	}

	_, _, store := bootstrap(os.Stderr, cliLogLevel(), failOnConfig)
	defer store.Close() //nolint:errcheck

	if err := runProjectMergeCore(context.Background(), store, os.Stdout, positional[0], positional[1]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
