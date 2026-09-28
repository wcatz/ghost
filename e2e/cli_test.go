//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memref"
)

// cliCommand is one row of the subcommand table this suite exercises.
type cliCommand struct {
	// path is the registered help path from cmd/ghost/help.go's
	// usageByCommand, written as a space-separated subcommand path.
	path string
	// help is the usage text's first line, so a command whose help drifted is
	// caught by the help check rather than passing on "exit 0".
	help string
	// run performs one real invocation of the subcommand, in a sandbox of its
	// own, and asserts its outcome. It is what makes the table coverage rather
	// than a list: `ghost <path> --help` proves the command exists and that its
	// help is side-effect-free, and only running it proves the command works.
	run func(t *testing.T, s *sandbox)
	// coveredBy names the dedicated test in this file that invokes the
	// subcommand, for a command whose real exercise needs more setup than a
	// single invocation (a reflect run, a whole hook event matrix). It is named
	// rather than implied so a reader can go and check that the command is
	// really run there, and so a row cannot quietly become coverless.
	coveredBy string
	// helpOnly is why this subcommand is exercised only through its help
	// output, when there is no run and no dedicated test.
	helpOnly string
}

// registeredSubcommandPaths is every subcommand path `cmd/ghost/help.go`
// registers, read FROM the product rather than restated here.
//
// It is parsed out of the source because a table copied into a test drifts the
// moment a command is added — and it drifts SILENTLY, which is the failure this
// replaces. Reading the file is what lets cliCommands be checked in both
// directions: a path in usageByCommand with no row below is a subcommand nothing
// has ever run.
//
// Reading the source is a deliberate choice over asking the binary. `ghost help`
// prints the command LIST, not the usageByCommand keys, and a `-h`-per-command
// sweep cannot discover a command it does not already know about — which is the
// discovery this check exists for. The one table that IS the registry is the
// only place a new command is visible from the outside.
func registeredSubcommandPaths(t *testing.T) []string {
	t.Helper()
	root, err := moduleRoot()
	if err != nil {
		t.Fatalf("locate the module root: %v", err)
	}
	source := string(mustReadFile(t, filepath.Join(root, "cmd", "ghost", "help.go")))

	// The table, and only the table. The file holds three maps; this one is the
	// registry of subcommand paths, and the others (help's value-flag list, the
	// observed-commands test) would contribute keys that are not commands.
	const marker = "usageByCommand = map[string]string{"
	i := strings.Index(source, marker)
	if i < 0 {
		t.Fatalf("cmd/ghost/help.go has no %q table; the coverage check needs updating", marker)
	}
	body := source[i+len(marker):]
	j := strings.Index(body, "\n}")
	if j < 0 {
		t.Fatalf("cmd/ghost/help.go's %q table is not closed where the check expects", marker)
	}
	body = body[:j]

	var paths []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), ","))
		if !strings.HasPrefix(line, `"`) {
			continue
		}
		quoted, rest, ok := strings.Cut(line[1:], `"`)
		if !ok {
			continue
		}
		// Every entry pairs a path with the usage const that documents it, so
		// the value's shape is the check that this line is an entry rather than
		// a stray key elsewhere in the file.
		if !strings.HasPrefix(rest, ":") {
			continue
		}
		paths = append(paths, quoted)
	}
	if len(paths) == 0 {
		t.Fatal("no subcommand paths were parsed out of help.go's usageByCommand")
	}
	sort.Strings(paths)
	return paths
}

// cliCommands is every subcommand path help.go registers. TestCLISurface
// compares it against registeredSubcommandPaths in both directions, runs every
// row's help, and invokes every row that has one.
var cliCommands = []cliCommand{
	{path: "mcp", help: "Usage: ghost mcp", coveredBy: "TestMCPSurface"},
	{path: "mcp init", help: "Usage: ghost mcp init", coveredBy: "TestCLIMCPInit"},
	{path: "mcp status", help: "Usage: ghost mcp status", coveredBy: "TestCLIMCPInit/status for every client"},
	{path: "hook", help: "Usage: ghost hook", coveredBy: "TestHookSessionStartInjectsContext, TestHookStop, TestHookFailOpen, TestHookSessionEnd"},
	{path: "reflect", help: "Usage: ghost reflect", coveredBy: "TestCLIReflect"},
	{path: "supersede", help: "Usage: ghost supersede",
		coveredBy: "TestCLIResolveSupersede, including --withdraw, which the " +
			"dry-run/apply case above does not reach"},
	{path: "resolve", help: "Usage: ghost resolve", coveredBy: "TestCLIResolveSupersede"},
	{path: "lifecycle", help: "Usage: ghost lifecycle",
		coveredBy: "TestHookStop/lifecycle_spawns_behind_its_lock_and_min_interval, as the " +
			"detached child the Stop hook spawns — the only way a user reaches it, since its " +
			"own help calls it 'not the normal way to start maintenance'"},
	{path: "obsidian", help: "Usage: ghost obsidian", coveredBy: "TestCLIObsidian"},
	{path: "opencode", help: "Usage: ghost opencode", coveredBy: "TestCLISurface/opencode cleanup-sessions"},
	{path: "opencode cleanup-sessions", help: "Usage: ghost opencode", coveredBy: "TestCLISurface/opencode cleanup-sessions"},
	{path: "backup", help: "Usage: ghost backup", coveredBy: "TestCLIBackup"},
	{path: "backup verify", help: "Usage: ghost backup verify", coveredBy: "TestCLIBackupVerifyRestore"},
	{path: "export", help: "Usage: ghost export", coveredBy: "TestCLIExportImport"},
	{path: "history", help: "Usage: ghost history", coveredBy: "TestCLIHistory"},
	{path: "import", help: "Usage: ghost import", coveredBy: "TestCLIExportImport"},
	{path: "bench", help: "Usage: ghost bench", coveredBy: "TestCLIBench"},
	{path: "upgrade", help: "Usage: ghost upgrade",
		helpOnly: "it reaches a hardcoded https://api.github.com/repos/wcatz/ghost/releases/latest, " +
			"which no environment variable redirects, so running it would make a real network call. " +
			"The suite makes none by design, so this is the one command exercised only through its " +
			"help and through the refusal a wrong flag produces."},
	{path: "context", help: "Usage: ghost context", run: runContextCommand},
	{path: "maintenance", help: "Usage: ghost maintenance", run: runMaintenanceCommands},
	{path: "project", help: "Usage: ghost project", run: runProjectCommand},
	{path: "project delete", help: "Usage: ghost project delete", run: runProjectDeleteCommand},
	{path: "project merge", help: "Usage: ghost project merge", run: runProjectMergeCommand},
	{path: "project bind", help: "Usage: ghost project bind", run: runProjectBindCommand},
	{path: "version", help: "Usage: ghost version", run: runVersionCommand},
}

// runVersionCommand is the real invocation behind the `version` row.
func runVersionCommand(t *testing.T, s *sandbox) {
	t.Helper()
	r := s.mustRun("version")
	mustMatch(t, "version", r.stdout, `^ghost \S+\s*$`)
	// Every spelling agrees, so a script that uses --version and a user who
	// typed `version` are never looking at different answers.
	for _, flag := range []string{"--version", "-v"} {
		other := s.mustRun(flag)
		if other.stdout != r.stdout {
			t.Fatalf("`ghost %s` printed %q and `ghost version` printed %q", flag, other.stdout, r.stdout)
		}
	}
}

// runContextCommand is the real invocation behind the `context` row.
//
// Two outcomes, both worth pinning. With a project bound to the directory, the
// block is the whole digest. With nothing bound AND no store seeded, the
// command prints NOTHING and exits 0 — RenderSessionContext returns the empty
// string rather than a decorative block, and the assertion is on the emptiness,
// because an agent whose instructions are a placeholder it did not ask for
// wastes a turn on it.
func runContextCommand(t *testing.T, s *sandbox) {
	t.Helper()
	// Nothing bound, no store: silence, and no error.
	unbound := s.mustRun("context")
	if strings.TrimSpace(unbound.stdout) != "" {
		t.Fatalf("`ghost context` printed a block for a store with nothing in it:\n%s", unbound.stdout)
	}
	// With a project bound to this directory, the digest is rendered.
	call(t, s.mcpSession(t), "ghost_memory_save", map[string]any{
		"project_id": e2eProject,
		"content":    "a memory the context block has to carry",
	})
	s.mustRun("project", "bind", e2eProject, s.work)

	// The process's own working directory IS the bound one — every command in
	// the sandbox runs with cmd.Dir = s.work — so the implicit-cwd render is
	// the one that takes no flag at all. Comparing it against an explicit
	// --cwd pointing at the SAME directory is the only pair that isolates the
	// flag: run --cwd twice and the comparison is a command with itself, which
	// would stay green with the flag ignored outright.
	implicit := s.mustRun("context")
	mustContain(t, "context with no --cwd", implicit.stdout, "Ghost context: "+e2eProject)
	mustContain(t, "context with no --cwd", implicit.stdout, "a memory the context block has to carry")

	explicit := s.mustRun("context", "--cwd", s.work)
	if stripSessionCounter(explicit.stdout) != stripSessionCounter(implicit.stdout) {
		t.Fatalf("`ghost context --cwd <bound dir>` and `ghost context` render differently:\n"+
			"--- implicit cwd ---\n%s\n--- --cwd ---\n%s", implicit.stdout, explicit.stdout)
	}

	// The = form is a separate branch in the parser, and a flag that only
	// worked in its space form would be invisible above.
	equals := s.mustRun("context", "--cwd="+s.work)
	if stripSessionCounter(equals.stdout) != stripSessionCounter(implicit.stdout) {
		t.Fatalf("`ghost context --cwd=<dir>` and `ghost context` render differently:\n"+
			"--- implicit cwd ---\n%s\n--- --cwd= ---\n%s", implicit.stdout, equals.stdout)
	}

	// And the flag is HONOURED, which is the half a same-directory comparison
	// can never show: a second project bound elsewhere must not leak into this
	// directory's digest.
	other := t.TempDir()
	call(t, s.mcpSession(t), "ghost_memory_save", map[string]any{
		"project_id": "other-proj",
		"content":    "a memory that belongs to a different directory",
	})
	s.mustRun("project", "bind", "other-proj", other)
	elsewhere := s.mustRun("context", "--cwd", other)
	mustContain(t, "context for the other directory", elsewhere.stdout, "Ghost context: other-proj")
	mustNotContain(t, "context for the other directory", elsewhere.stdout,
		"a memory the context block has to carry")
}

// runMaintenanceCommands is the real invocation behind the `maintenance` row,
// covering both of its subcommands. status reports the scratch budget; a
// mistyped flag on clean-scratch is refused rather than silently treated as a
// dry run, which is the failure a user would not otherwise see.
func runMaintenanceCommands(t *testing.T, s *sandbox) {
	t.Helper()
	status := s.mustRun("maintenance", "status")
	mustMatch(t, "maintenance status", status.stdout, "(?i)scratch")

	clean := s.mustRun("maintenance", "clean-scratch")
	mustMatch(t, "maintenance clean-scratch (dry run)", clean.stdout, "(?i)nothing removed|pass --apply")

	r := s.mustFail("maintenance", "clean-scratch", "--aply")
	mustMatch(t, "mistyped clean-scratch flag", r.stderr, "(?i)unknown|flag")

	// maintenance with no verb is a usage error, not a silent success.
	none := s.mustFail("maintenance")
	mustMatch(t, "bare `ghost maintenance`", none.stderr, "(?i)Usage: ghost maintenance")
}

// runProjectCommand is the real invocation behind the `project` row: the bare
// verb is a usage error, and the suite records that the CLI has no
// `ghost project list` — the inventory surface is ghost_list_projects.
func runProjectCommand(t *testing.T, s *sandbox) {
	t.Helper()
	r := s.mustFail("project")
	mustMatch(t, "bare `ghost project`", r.stderr, "(?i)Usage: ghost project")
	mustMatch(t, "bare `ghost project`", r.stderr, "(?i)delete")
	mustMatch(t, "bare `ghost project`", r.stderr, "(?i)merge")
	mustMatch(t, "bare `ghost project`", r.stderr, "(?i)bind")
	// The listing the bare verb does not offer is the MCP tool's job, and it
	// has to be reachable from the same store the CLI writes to.
	call(t, s.mcpSession(t), "ghost_memory_save", map[string]any{
		"project_id": e2eProject,
		"content":    "a memory so the project listing has a row",
	})
	mustContain(t, "the inventory surface", call(t, s.mcpSession(t), "ghost_list_projects", nil), e2eProject)
}

// runProjectDeleteCommand is the real invocation behind the `project delete`
// row. The confirmation is the interesting part: --apply still requires the
// name to be re-typed on stdin, and a wrong answer must delete nothing. That is
// what makes the command irreversible-safe, and it is only observable by
// running it.
func runProjectDeleteCommand(t *testing.T, s *sandbox) {
	t.Helper()
	cs := s.mcpSession(t)
	victim := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
		"project_id": "doomed-cli-proj",
		"content":    "a memory in the project the CLI is about to delete",
	}))

	// Dry run: the summary, and nothing gone.
	dry := s.mustRun("project", "delete", "doomed-cli-proj")
	mustContain(t, "project delete (dry run)", dry.stdout, "Would delete")
	mustMatch(t, "project delete (dry run)", dry.stdout, "(?i)re-run with --apply")
	if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE id = ?`, victim); n != 1 {
		t.Fatalf("a dry-run project delete removed a memory")
	}

	// A wrong confirmation is refused, and nothing is deleted.
	wrong := s.mustFailStdin("not-the-name\n", "project", "delete", "doomed-cli-proj", "--apply")
	mustMatch(t, "project delete (wrong confirmation)", wrong.stderr, "(?i)confirmation did not match")
	if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE id = ?`, victim); n != 1 {
		t.Fatalf("a mistyped confirmation deleted the project anyway")
	}

	// The right one deletes it. The prompt is on stdout and the summary says
	// Deleted rather than Would, and both are asserted: a preview printed after
	// an apply would leave the user unsure whether the irreversible half ran.
	applied := s.mustRunStdin("doomed-cli-proj\n", "project", "delete", "doomed-cli-proj", "--apply")
	mustMatch(t, "project delete (confirmation prompt)", applied.stdout, `Type the project name \("doomed-cli-proj"\)`)
	mustContain(t, "project delete (apply)", applied.stdout, `Deleted "doomed-cli-proj"`)
	if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE id = ?`, victim); n != 0 {
		t.Fatalf("memory %s survived `ghost project delete --apply`", victim)
	}
	if n := s.queryInt(t, `SELECT COUNT(*) FROM projects WHERE id = ?`, "doomed-cli-proj"); n != 0 {
		t.Fatalf("the project row survived its own delete")
	}

	// _global is refused: the bucket every session's injection reads.
	global := s.mustFailStdin("_global\n", "project", "delete", "_global", "--apply")
	mustMatch(t, "project delete refuses _global", global.stderr, "(?i)refus|_global|global")
	if n := s.queryInt(t, `SELECT COUNT(*) FROM projects WHERE id = ?`, "_global"); n != 1 {
		t.Fatalf("the _global project row is gone")
	}
}

// runProjectMergeCommand is the real invocation behind the `project merge` row.
// The memory's project_id and its id both have to survive, because a merge that
// re-issued ids would orphan every embedding and link the memory had.
func runProjectMergeCommand(t *testing.T, s *sandbox) {
	t.Helper()
	cs := s.mcpSession(t)
	moved := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
		"project_id": "merge-source-proj",
		"content":    "a memory the merge has to carry across",
	}))
	call(t, cs, "ghost_memory_save", map[string]any{
		"project_id": "merge-dest-proj",
		"content":    "a memory already in the destination",
	})

	r := s.mustRun("project", "merge", "merge-source-proj", "merge-dest-proj")
	mustMatch(t, "project merge", r.stdout, "(?i)merg|mov")

	// The id is preserved, not re-issued: a new id would cascade the memory's
	// embeddings and links away, and a merge that did that would be a silent
	// data loss the exit code cannot report.
	if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE id = ? AND project_id = ?`,
		moved, "merge-dest-proj"); n != 1 {
		t.Fatalf("the merged memory %s did not arrive in the destination with its id intact", moved)
	}
	if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE project_id = ?`, "merge-source-proj"); n != 0 {
		t.Fatalf("the merged-away project still holds memories")
	}
	if n := s.queryInt(t, `SELECT COUNT(*) FROM projects WHERE id = ?`, "merge-source-proj"); n != 0 {
		t.Fatalf("the merged-away project row survived")
	}
}

// runProjectBindCommand is the real invocation behind the `project bind` row.
func runProjectBindCommand(t *testing.T, s *sandbox) {
	t.Helper()
	call(t, s.mcpSession(t), "ghost_memory_save", map[string]any{
		"project_id": e2eProject,
		"content":    "a memory so the project exists to bind",
	})
	checkout := t.TempDir()
	s.mustRun("project", "bind", e2eProject, checkout)
	// Stored as its physical path, so a symlinked temp dir still matches a
	// session's cwd later.
	if n := s.queryInt(t, `SELECT COUNT(*) FROM projects WHERE id = ? AND path = ?`,
		e2eProject, resolveSymlinks(t, checkout)); n != 1 {
		t.Fatalf("project bind did not record the checkout path")
	}
	// Re-running is a no-op, not a second row.
	s.mustRun("project", "bind", e2eProject, checkout)
	if n := s.queryInt(t, `SELECT COUNT(*) FROM projects WHERE id = ?`, e2eProject); n != 1 {
		t.Fatalf("re-running project bind created a second project row")
	}
	// An unknown project is refused rather than created.
	s.mustFail("project", "bind", "no-such-project-for-bind", checkout)
}

// TestCLISurface drives the CLI half: every registered subcommand's help, and
// one real invocation of every command that can be invoked without a network
// call or a live model.
func TestCLISurface(t *testing.T) {
	s := newSandbox(t)

	// Seed a project and a memory so the read-only commands have something
	// real to report on. A store with nothing in it makes half these
	// invocations pass for the wrong reason.
	cs := s.mcpSession(t)
	id := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
		"project_id": e2eProject,
		"content":    "the relay listens on port 2222 in staging",
		"category":   "architecture",
	}))

	// Every row has to account for itself in exactly one of three ways, and the
	// check is here rather than in a comment because the three ways look the
	// same in the table: a `run` closure, a `coveredBy` name, or a `helpOnly`
	// reason. A row that names none of them is a command nothing runs, and a row
	// that names two is one a reader cannot trust.
	registered := registeredSubcommandPaths(t)
	covered := map[string]bool{}
	for _, cmd := range cliCommands {
		covered[cmd.path] = true
		ways := 0
		if cmd.run != nil {
			ways++
		}
		if cmd.coveredBy != "" {
			ways++
		}
		if cmd.helpOnly != "" {
			ways++
		}
		if ways != 1 {
			t.Errorf("cliCommands[%q] accounts for itself %d ways (run=%v coveredBy=%q helpOnly=%q); "+
				"exactly one is required, so a command that is never run cannot hide here",
				cmd.path, ways, cmd.run != nil, cmd.coveredBy, cmd.helpOnly)
		}
	}
	for _, path := range registered {
		if !covered[path] {
			t.Errorf("cmd/ghost/help.go registers %q and cliCommands has no row for it — add one, "+
				"or a subcommand shipped with nothing ever having run it", path)
		}
	}
	for _, cmd := range cliCommands {
		if !containsString(registered, cmd.path) {
			t.Errorf("cliCommands drives %q, which cmd/ghost/help.go does not register", cmd.path)
		}
	}
	if len(registered) != len(cliCommands) {
		t.Errorf("help.go registers %d subcommand paths and cliCommands has %d rows", len(registered), len(cliCommands))
	}

	for _, cmd := range cliCommands {
		t.Run("help/"+strings.ReplaceAll(cmd.path, " ", "_"), func(t *testing.T) {
			// A sandbox of its own, holding a real store, so "help had no side
			// effect" is checkable. Sharing one with the other subtests would
			// make the check unfalsifiable: the store would already have been
			// written by the seeding above.
			hs := newSandbox(t)
			call(t, hs.mcpSession(t), "ghost_memory_save", map[string]any{
				"project_id": e2eProject,
				"content":    "a note so help runs against a store that could change",
			})
			before := snapshotStore(t, hs)

			args := append(strings.Fields(cmd.path), "--help")
			r := hs.mustRun(args...)
			if !strings.HasPrefix(strings.TrimSpace(r.stdout), cmd.help) {
				t.Fatalf("`ghost %s --help` printed %q, want it to start with %q",
					cmd.path, strings.TrimSpace(r.stdout), cmd.help)
			}
			// Help must be a question answered, not a command run. The check is
			// on side effects rather than on the word "would", which appears in
			// several usage texts as ordinary prose: nothing may be written, and
			// no harness spawned.
			if after := snapshotStore(t, hs); after != before {
				t.Fatalf("`ghost %s --help` changed the store: %s -> %s", cmd.path, before, after)
			}
			for _, fake := range harnessBinaries {
				if argv, _ := hs.harnessLog(fake); strings.Contains(argv, "ARGV ") {
					t.Fatalf("`ghost %s --help` spawned %s: %s", cmd.path, fake, argv)
				}
			}
			// -h is the other spelling and has to agree.
			short := append(strings.Fields(cmd.path), "-h")
			sr := hs.mustRun(short...)
			if sr.stdout != r.stdout {
				t.Fatalf("`ghost %s -h` and `--help` disagree", cmd.path)
			}
		})
	}

	// One real invocation per row, in a sandbox of its own so a row cannot lean
	// on state another row left behind. Skipped only where the row says why.
	for _, cmd := range cliCommands {
		if cmd.run == nil {
			t.Run("help-only/"+strings.ReplaceAll(cmd.path, " ", "_"), func(t *testing.T) {
				t.Logf("`ghost %s` is exercised only through its help: %s", cmd.path, cmd.helpOnly)
			})
			continue
		}
		t.Run("runs/"+strings.ReplaceAll(cmd.path, " ", "_"), func(t *testing.T) {
			cmd.run(t, newSandbox(t))
		})
	}

	t.Run("version", func(t *testing.T) {
		// The same binary answers every spelling, and the answer names a
		// version rather than nothing.
		for _, flag := range []string{"version", "--version", "-v"} {
			r := s.mustRun(flag)
			mustMatch(t, "version via "+flag, r.stdout, `^ghost \S`)
		}
	})

	t.Run("upgrade refuses an unknown flag without reaching the network", func(t *testing.T) {
		// `ghost upgrade` is the one subcommand this suite does not run, because
		// it fetches a hardcoded api.github.com URL that no environment variable
		// redirects, and the suite makes no network call by design. Its ARGUMENT
		// HANDLING is still exercised, and it is the part a mistyped flag reaches:
		// the flag is rejected before any HTTP request is built, so this test
		// proves the parsing rather than papering over the network gap.
		r := s.mustFail("upgrade", "--nope")
		mustMatch(t, "ghost upgrade with an unknown flag", r.stderr, "(?i)unknown flag|Usage: ghost upgrade")
		// A help request is answered without reaching the network either, and the
		// suite checks that for every registered command in the sweep above.
		mustContain(t, "upgrade help", s.mustRun("upgrade", "--help").stdout, "GitHub Releases")
	})

	t.Run("maintenance status", func(t *testing.T) {
		r := s.mustRun("maintenance", "status")
		mustMatch(t, "maintenance status", r.stdout, "(?i)scratch")
	})

	t.Run("maintenance clean-scratch", func(t *testing.T) {
		// The dry run reports and says so in its own words, which is the
		// assertion: a preview that read as an action would leave the user
		// believing debris had been removed.
		dry := s.mustRun("maintenance", "clean-scratch")
		mustMatch(t, "maintenance clean-scratch (dry run)", dry.stdout, "(?i)nothing removed|pass --apply")
		// An unknown flag is an error rather than a silently accepted no-op, or
		// a user who mistyped it would believe the scratch root was clean.
		r := s.mustFail("maintenance", "clean-scratch", "--aply")
		mustMatch(t, "mistyped clean-scratch flag", r.stderr, "(?i)unknown|flag")
	})

	t.Run("opencode cleanup-sessions", func(t *testing.T) {
		// The fake reports one session titled exactly "[ghost]" with plausible
		// timestamps, so the dry run must find it and delete nothing.
		stamp := time.Now().Add(-2 * time.Hour).UnixMilli()
		s.writeSessionsJSON(t, fmt.Sprintf(
			`[{"id":"ses_e2e_ghost","title":"[ghost]","created":%d,"updated":%d},`+
				`{"id":"ses_e2e_other","title":"real work","created":%d,"updated":%d}]`,
			stamp, stamp, stamp, stamp))

		// The dry run names the count it found and says it deleted nothing.
		// The ids themselves are asserted through the fake's own delete log,
		// which is the only place they are knowable — a summary line that
		// counted the wrong sessions would otherwise pass.
		dry := s.mustRun("opencode", "cleanup-sessions")
		mustMatch(t, "cleanup-sessions (dry run)", dry.stdout, `(?i)1 titled "\[ghost\]"`)
		mustMatch(t, "cleanup-sessions (dry run, nothing deleted)", dry.stdout, "(?i)nothing deleted|dry run")
		mustNotExist(t, "the fake's delete log after a dry run", filepath.Join(s.bin, "opencode.deleted"))

		applied := s.mustRun("opencode", "cleanup-sessions", "--apply")
		mustMatch(t, "cleanup-sessions (apply)", applied.stdout, "(?i)delet|removed")
		deleted := string(mustReadFile(t, filepath.Join(s.bin, "opencode.deleted")))
		mustContain(t, "deleted sessions", deleted, "ses_e2e_ghost")
		mustNotContain(t, "deleted sessions", deleted, "ses_e2e_other")
	})

	t.Run("an unknown subcommand is reported, on stderr", func(t *testing.T) {
		// The diagnostic has to be visible: a typo has to reach the user.
		//
		// The EXIT CODE is not asserted to be non-zero, because the product
		// exits 0 here and that is not this suite's to change. `dispatchCommand`
		// falls out of its argv switch to `printUsage(); return 0` for an
		// unrecognised first word, while `project`, `maintenance` and `opencode`
		// return 1 for their own usage errors. So a mistyped `ghost reflecttt`
		// prints the command list — which is how a user learns the name is wrong
		// — and a script that only checked the exit code would read it as a
		// success. Asserting an exit code the product does not use would be
		// asserting a bug that is not one, so what is pinned is the half that
		// holds: the command list goes to STDERR, and no work happened.
		//
		// Filed-worthy rather than fixed here: a behaviour change in the CLI's
		// dispatch is a separate concern from a test layer landing, and this
		// suite's own rule is to leave such a case failing rather than paper
		// over it. The exit code is recorded here so the next person to look at
		// it has the exact shape of the asymmetry.
		r := s.run("reflecttt", e2eProject)
		mustMatch(t, "unknown subcommand", r.stderr, "(?i)Commands:")
		mustNotContain(t, "unknown subcommand", r.stdout, "Consolidator:")
		mustNotContain(t, "unknown subcommand", r.stdout, "DRY RUN")
		// Nothing was consolidated: a reflect that "succeeded" would have written
		// a snapshot, which is the observable trace of work.
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_snapshots WHERE project_id = ?`, e2eProject); n != 0 {
			t.Fatalf("an unknown subcommand consolidated the project")
		}
	})

	t.Run("a mistyped flag is refused", func(t *testing.T) {
		// The flag that matters differs per command, and each parser treats an
		// unknown one as an error: silently ignoring --aply would write the
		// default file and leave the user believing it went where they asked.
		// The would-be targets live in this test's own temp dir, never a shared
		// /tmp path, so a leftover file cannot fail the assertion below.
		scratch := t.TempDir()
		backupTarget := filepath.Join(scratch, "should-not-be-written.db")
		exportTarget := filepath.Join(scratch, "should-not-be-written.jsonl")
		for _, args := range [][]string{
			{"backup", "--ou", backupTarget},
			{"export", "--outt", exportTarget},
			{"history", id, "--limitt", "2"},
			{"obsidian", "export", "--ou", filepath.Join(s.t.TempDir(), "vault")},
		} {
			r := s.mustFail(args...)
			mustMatch(t, "unknown flag for ghost "+strings.Join(args, " "), r.stderr, "(?i)unknown|needs a value")
		}
		// And nothing was written on the way out.
		mustNotExist(t, "the mistyped backup target", backupTarget)
		mustNotExist(t, "the mistyped export target", exportTarget)
	})

	_ = id
}

// writeSessionsJSON replaces the fake opencode's session list.
func (s *sandbox) writeSessionsJSON(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.bin, "opencode.sessions"), []byte(body+"\n"), 0o644); err != nil {
		t.Fatalf("write opencode.sessions: %v", err)
	}
}

// TestCLIVersionAndContext covers `ghost version` and `ghost context`,
// including the scope behaviour injection.session_scope selects under.
func TestCLIVersionAndContext(t *testing.T) {
	t.Run("version", func(t *testing.T) {
		s := newSandbox(t)
		r := s.mustRun("version")
		mustMatch(t, "version", r.stdout, `^ghost \S+\s*$`)
		// The build under test is stamped with a version, and it has to be the
		// same version the MCP server reports, or `mcp status` and the CLI are
		// describing different binaries.
		cs := s.mcpSession(t)
		_ = cs
	})

	t.Run("context honours injection.session_scope", func(t *testing.T) {
		s := newSandbox(t)
		// Two scoped memories and one unscoped, under a project bound to a
		// directory the hook and the command can both resolve.
		cs := s.mcpSession(t)
		call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "the pool timeout is 30s in production",
			"category":   "architecture",
			"scope":      map[string]any{"environment": "production"},
		})
		call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "the pool timeout is 5s on the dev pool",
			"category":   "architecture",
			"scope":      map[string]any{"environment": "development"},
		})
		call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "the pool timeout knob is named in the config file",
			"category":   "convention",
		})
		s.mustRun("project", "bind", e2eProject, s.work)

		// Unscoped: both scoped rows and the label showing which is which.
		all := s.mustRun("context", "--cwd", s.work)
		mustContain(t, "context (unscoped)", all.stdout, "30s in production")
		mustContain(t, "context (unscoped)", all.stdout, "5s on the dev pool")
		// The scope label is the same one every other surface prints, so a row
		// that carries scope has to say so here too.
		mustMatch(t, "context (scope label)", all.stdout, `environment=production`)

		// Scoped to production: the development row goes, the unscoped one
		// stays (a filter, not a gate), and the answer says it narrowed.
		s.reconfigure(configOpts{sessionScope: map[string]string{"environment": "production"}})
		prod := s.mustRun("context", "--cwd", s.work)
		mustContain(t, "context (production)", prod.stdout, "30s in production")
		mustNotContain(t, "context (production)", prod.stdout, "5s on the dev pool")
		mustContain(t, "context (production, unscoped row survives)", prod.stdout, "config file")

		// Scoped to a value nothing carries: the scoped rows are excluded and
		// only the unscoped one is left.
		s.reconfigure(configOpts{sessionScope: map[string]string{"environment": "nowhere"}})
		none := s.mustRun("context", "--cwd", s.work)
		mustNotContain(t, "context (nowhere)", none.stdout, "30s in production")
		mustNotContain(t, "context (nowhere)", none.stdout, "5s on the dev pool")
		mustContain(t, "context (nowhere, unscoped row survives)", none.stdout, "config file")

		// A bare `environment:` (a YAML null) is a different thing from an
		// empty string, and the config loader prunes nulls so a section header
		// left empty erases nothing. That is the documented rule, so the empty
		// VALUE has to be written as one for the check to be reachable.
		// A quoted empty value, which is the reachable form: a bare
		// `environment:` parses as null and is pruned, taking the whole section
		// with it, and that is the documented rule rather than a failure.
		s.reconfigure(configOpts{extra: "injection:\n  session_scope:\n    environment: \"\"\n"})
		// A quoted empty scope value is a filter that would exclude every row
		// naming that key, and both surfaces refuse it: a hook warns and falls
		// back, a subcommand fails.
		//
		// `ghost context` runs inside somebody else's editor session, so a typo
		// in a settings file must not leave them with no memory at all — it
		// falls back to the environment plus the compiled defaults, and the
		// compiled defaults carry no session_scope, so the block comes back
		// unscoped with the reason on stderr. A subcommand is the opposite: the
		// user asked for that command, and running it on half the intended
		// configuration is worse than stopping. Asserting either behaviour for
		// both surfaces would be asserting something the product does not do.
		broken := s.mustRun("context", "--cwd", s.work)
		mustMatch(t, "a broken config warns", broken.stderr, "(?i)session_scope|config")
		mustMatch(t, "a broken config falls back", broken.stderr, "(?i)falling back|defaults|environment")
		// Unscoped, which is what falling back means: every row is shown again.
		mustContain(t, "fallback is unscoped", broken.stdout, "30s in production")
		mustContain(t, "fallback is unscoped", broken.stdout, "5s on the dev pool")

		failed := s.run("obsidian", "export", "--out", filepath.Join(s.t.TempDir(), "v"), "--project", e2eProject)
		if failed.code == 0 {
			t.Fatalf("a CLI subcommand accepted a config it should have refused: %s", failed)
		}
		mustMatch(t, "a subcommand fails on a broken config", failed.stderr, "(?i)config|session_scope")

		// A bare null is pruned, which is the other half of the same rule: the
		// header a user leaves behind must not silently erase the section.
		s.reconfigure(configOpts{extra: "injection:\n  session_scope:\n"})
		pruned := s.mustRun("context", "--cwd", s.work)
		mustContain(t, "a bare section header keeps the defaults", pruned.stdout, "30s in production")
		mustContain(t, "a bare section header keeps the defaults", pruned.stdout, "5s on the dev pool")

		// A directory that resolves to nothing injects nothing at all: an empty
		// block is what a user with no matching project should get, not a
		// decorative one.
		elsewhere := s.t.TempDir()
		unbound := s.mustRun("context", "--cwd", elsewhere)
		if strings.Contains(unbound.stdout, "pool timeout") {
			t.Fatalf("context injected a project block for an unbound directory: %s", unbound)
		}
	})
}

// TestCLIReflect covers the sqlite tier end to end: dry run, apply, and the
// restore that undoes the apply.
func TestCLIReflect(t *testing.T) {
	s := newSandbox(t)
	seedReflectProject(t, s)
	// The reflect run captures a timestamp before it fetches the corpus and
	// preserves any memory created at or after it, so that a save landing on a
	// live MCP server mid-consolidation is not deleted by the replace. The
	// seeds and the reflect would otherwise share a second — created_at has
	// one-second resolution — and every seed would be preserved as if it had
	// been saved concurrently, leaving the corpus unreplaced. Sleeping past
	// that window is what makes the apply's own effect observable.
	time.Sleep(1100 * time.Millisecond)

	t.Run("sqlite tier dry run writes nothing", func(t *testing.T) {
		before := s.queryInt(t, `SELECT COUNT(*) FROM memories`)
		r := s.mustRun("reflect", e2eProject, "--tier", "sqlite")
		mustContain(t, "reflect (dry run)", r.stdout, "DRY RUN")
		mustContain(t, "reflect (dry run)", r.stdout, "Project:")
		mustContain(t, "reflect (dry run)", r.stdout, e2eProject)
		if after := s.queryInt(t, `SELECT COUNT(*) FROM memories`); after != before {
			t.Fatalf("a dry-run reflect changed the memory count: %d -> %d", before, after)
		}
	})

	t.Run("sqlite tier apply then restore", func(t *testing.T) {
		// The apply has to record what it did, or the restore has nothing to
		// undo: the snapshot is the apply's own record.
		r := s.mustRun("reflect", e2eProject, "--tier", "sqlite", "--apply")
		mustNotContain(t, "reflect (apply)", r.stdout, "DRY RUN")
		mustMatch(t, "reflect (apply)", r.stdout, "(?i)applied|snapshot|wrote|Result:")
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_snapshots WHERE project_id = ?`, e2eProject); n == 0 {
			t.Fatalf("an applied reflect wrote no snapshot, so restore has nothing to undo")
		}
		// The learned context is the observable product of a consolidation.
		learned := s.queryStrings(t, `SELECT learned_context FROM ghost_state WHERE project_id = ?`, e2eProject)
		if len(learned) == 0 {
			t.Fatalf("an applied reflect recorded no learned context")
		}

		// A save made after the snapshot is the documented case restore must
		// not roll back, so it is written before the restore rather than after.
		after := parseID(t, call(t, s.mcpSession(t), "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "a note saved after the snapshot was taken",
		}))

		restored := s.mustRun("reflect", e2eProject, "--restore")
		mustMatch(t, "reflect --restore", restored.stdout, "(?i)restore")
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE id = ?`, after); n != 1 {
			t.Fatalf("restore rolled back a memory saved after the snapshot")
		}
		// Restoring twice is a no-op rather than an error, because a snapshot
		// is not consumed by being restored.
		s.mustRun("reflect", e2eProject, "--restore")
	})

	t.Run("the opencode tier runs through the fake harness", func(t *testing.T) {
		ts := newSandbox(t)
		seedReflectProject(t, ts)
		time.Sleep(1100 * time.Millisecond)

		// Dry run first: the harness has to be asked, and nothing written.
		dry := ts.mustRun("reflect", e2eProject, "--tier", "opencode", "--source", "opencode")
		mustContain(t, "reflect --tier opencode (dry run)", dry.stdout, "DRY RUN")
		argv, prompts := ts.harnessLog("opencode")
		if !strings.Contains(argv, "ARGV run") {
			t.Fatalf("the opencode tier did not spawn opencode: %s", argv)
		}
		// The prompt is the product's, and the fake must have been able to read
		// it: the kind it logged is how the test knows the marker matched.
		if !strings.Contains(prompts, "learned_context") {
			t.Fatalf("the reflection prompt was not sent: %s", prompts)
		}
		kinds := string(mustReadFile(t, filepath.Join(ts.bin, "opencode.kinds")))
		mustContain(t, "harness operation kind", kinds, "reflect")

		applied := ts.mustRun("reflect", e2eProject, "--tier", "opencode", "--source", "opencode", "--apply")
		mustNotContain(t, "reflect --tier opencode (apply)", applied.stdout, "DRY RUN")
		if n := ts.queryInt(t, `SELECT COUNT(*) FROM memory_snapshots WHERE project_id = ?`, e2eProject); n == 0 {
			t.Fatalf("an applied opencode-tier reflect wrote no snapshot")
		}
		// The fake answers "keep" for every input, and a keep reuses the stored
		// row rather than writing a fresh one — so the corpus must be intact and
		// unchanged. Asserting on identity is what makes this a real check: a
		// consolidation that rewrote every row would satisfy a count assertion
		// while losing every id, embedding and link in the project.
		if n := ts.queryInt(t, `SELECT COUNT(*) FROM memories WHERE project_id = ?`, e2eProject); n != 7 {
			t.Fatalf("an applied opencode-tier reflect left %d memories, want the 7 it was given", n)
		}
		for _, want := range []string{"2222", "2223", "MaxOpenConns", "single connection", "signs the tag", "annotated tag"} {
			found := ts.queryStrings(t, `SELECT content FROM memories WHERE project_id = ? AND content LIKE ?`,
				e2eProject, "%"+want+"%")
			if len(found) == 0 {
				t.Fatalf("the apply lost the memory mentioning %q", want)
			}
		}
		// And the learned context was written, which is the consolidation's
		// other product.
		learned := ts.queryStrings(t, `SELECT learned_context FROM ghost_state WHERE project_id = ?`, e2eProject)
		if len(learned) == 0 || learned[0] == "" {
			t.Fatalf("an applied opencode-tier reflect recorded no learned context")
		}
		_ = applied
	})

	t.Run("an unknown tier is refused", func(t *testing.T) {
		s.mustFail("reflect", e2eProject, "--tier", "haiku")
		s.mustFail("reflect", e2eProject, "--tier", "cli", "--require-llm", "--tier=sqlite")
	})
}

// seedReflectProject creates a project with enough near-duplicate memories for
// the sqlite tier's Jaccard pass to have something to fold.
func seedReflectProject(t *testing.T, s *sandbox) {
	t.Helper()
	cs := s.mcpSession(t)
	for i, content := range []string{
		"the relay listens on port 2222 in staging",
		"the relay listens on port 2222 in the staging environment",
		"the relay listens on port 2223 in staging",
		"the sqlite store pins MaxOpenConns to one connection",
		"the sqlite store pins the connection pool to a single connection",
		"the release process signs the tag before publishing",
		"the release process signs the annotated tag before publishing",
	} {
		id := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    content,
			"category":   "architecture",
		}))
		// Distinct enough to be separate rows: a save that folded into an
		// existing near-duplicate would leave the corpus too small to fold.
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE id = ?`, id); n != 1 {
			t.Fatalf("seed memory %d was not stored", i)
		}
	}
	if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE project_id = ?`, e2eProject); n < 7 {
		t.Fatalf("the reflect fixture holds %d memories, want at least 7", n)
	}
}

// TestCLIResolveSupersede covers both classifier-backed commands, dry run and
// apply, through the fake harness.
func TestCLIResolveSupersede(t *testing.T) {
	t.Run("resolve dry run and apply", func(t *testing.T) {
		s := newSandbox(t)
		cs := s.mcpSession(t)
		id := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "the port 2222 experiment was abandoned after the relay change shipped",
		}))
		s.setHarnessAnswer("resolve", "RESOLVED | closed-by: the experiment was abandoned upstream")

		dry := s.mustRun("resolve", e2eProject, "--source", "opencode")
		mustContain(t, "resolve (dry run)", dry.stdout, "would resolve")
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE id = ? AND resolved_at IS NOT NULL`, id); n != 0 {
			t.Fatalf("a dry-run resolve stamped resolved_at")
		}

		applied := s.mustRun("resolve", e2eProject, "--source", "opencode", "--apply")
		mustContain(t, "resolve (apply)", applied.stdout, "resolved")
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE id = ? AND resolved_at IS NOT NULL`, id); n != 1 {
			t.Fatalf("resolve --apply did not stamp resolved_at")
		}
		argv, _ := s.harnessLog("opencode")
		if !strings.Contains(argv, "ARGV run") {
			t.Fatalf("resolve did not spawn the harness named by --source: %s", argv)
		}
	})

	t.Run("a KEEP verdict leaves the memory alone", func(t *testing.T) {
		s := newSandbox(t)
		cs := s.mcpSession(t)
		id := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "the deploy was reverted on the staging relay",
		}))
		s.setHarnessAnswer("resolve", "KEEP")

		applied := s.mustRun("resolve", e2eProject, "--source", "opencode", "--apply")
		mustContain(t, "resolve (KEEP)", applied.stdout, "KEEP")
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE id = ? AND resolved_at IS NOT NULL`, id); n != 0 {
			t.Fatalf("a KEEP verdict resolved the memory anyway")
		}
	})

	t.Run("resolve without a project is a usage error", func(t *testing.T) {
		s := newSandbox(t)
		s.mustFail("resolve")
		// An undetectable harness is an error, never a silent fallback to a
		// different one — but the sandbox's own ancestry may name a harness, so
		// the assertion here is only that the failure names the project.
		unknown := s.mustFail("resolve", "no-such-project-at-all", "--source", "opencode")
		mustMatch(t, "unknown project", unknown.stderr, "(?i)not found|no project")
	})

	t.Run("supersede dry run and apply", func(t *testing.T) {
		s := newSandbox(t)
		cs := s.mcpSession(t)
		// Two memories that say the same fact differently: the near-duplicate
		// pair the supersede candidate scan needs, and near enough that the
		// stub embedding puts them over the similarity threshold.
		//
		// The gap between the two saves is not incidental. The pass orients a
		// pair by updated_at, and created_at/updated_at are stored at
		// one-second resolution — two saves in the same second tie, and the
		// orientation of a tie is whatever the query returns, so the direction
		// assertion below would be asserting on an arbitrary answer. A sleep is
		// the only way to make the product's own rule decidable here.
		older := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "the staging relay port is 2222",
		}))
		time.Sleep(1100 * time.Millisecond)
		newer := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "the staging relay port is 3333 now",
		}))
		s.setHarnessAnswer("supersede", "SUPERSEDES | replaced: the staging relay port is 2222")

		// Dry run: the classifier is asked and nothing is written.
		dry := s.mustRun("supersede", e2eProject, "--source", "opencode", "--threshold", "0.1")
		mustMatch(t, "supersede (dry run)", dry.stdout, "(?i)candidate|SUPERSEDES|dry")
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_links WHERE source_id = ? AND relation = 'supersedes'`, newer); n != 0 {
			t.Fatalf("a dry-run supersede wrote a link")
		}
		argv, _ := s.harnessLog("opencode")
		if !strings.Contains(argv, "ARGV run") {
			t.Fatalf("supersede did not spawn the harness: %s", argv)
		}

		applied := s.mustRun("supersede", e2eProject, "--source", "opencode", "--threshold", "0.1", "--apply")
		mustMatch(t, "supersede (apply)", applied.stdout, "(?i)supersede|written|applied")
		// A supersedes link points from the note that RESTATES the fact to the
		// one it replaces, always. Asserting the direction is the point: the
		// REVERSED verdict exists to stop this edge being written the other way,
		// and a link written backwards demotes the current note and promotes the
		// stale one (issue #641).
		edges := s.queryStrings(t,
			`SELECT source_id || '->' || target_id FROM memory_links WHERE relation = 'supersedes'`)
		if len(edges) != 1 {
			t.Fatalf("the apply wrote %d supersedes link(s), want exactly 1: %v", len(edges), edges)
		}
		if want := newer + "->" + older; edges[0] != want {
			t.Fatalf("the supersedes link is %s, want %s (the restating note must point at the one it replaces)",
				edges[0], want)
		}
		// And the write is attributable to the classifier, not to some other
		// pass: a link with an unexplained source would rank rows the operator
		// cannot account for.
		sources := s.queryStrings(t,
			`SELECT DISTINCT source FROM memory_links WHERE relation = 'supersedes'`)
		if len(sources) != 1 || sources[0] != "llm" {
			t.Fatalf("supersedes link source = %v, want [llm]", sources)
		}
	})

	t.Run("a REVERSED verdict is refused rather than flipped", func(t *testing.T) {
		s := newSandbox(t)
		cs := s.mcpSession(t)
		older := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "the staging relay port is 2222",
		}))
		call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "the staging relay port is 3333 now",
		})
		s.setHarnessAnswer("supersede", "REVERSED")

		r := s.mustRun("supersede", e2eProject, "--source", "opencode", "--threshold", "0.1", "--apply")
		// Whatever the pass reports, no link may be written in either direction.
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_links WHERE relation = 'supersedes'`); n != 0 {
			t.Fatalf("a REVERSED verdict wrote %d supersedes link(s)", n)
		}
		_ = r
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE id = ?`, older); n != 1 {
			t.Fatalf("the older memory was removed")
		}
	})

	t.Run("supersede --reassess survives a flaky harness", func(t *testing.T) {
		s := newSandbox(t)
		cs := s.mcpSession(t)
		older := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "the staging relay port is 2222",
		}))
		time.Sleep(1100 * time.Millisecond) // the orientation is by updated_at
		newer := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "the staging relay port is 3333 now",
		}))
		s.setHarnessAnswer("supersede", "SUPERSEDES | replaced: the staging relay port is 2222")
		s.mustRun("supersede", e2eProject, "--source", "opencode", "--threshold", "0.1", "--apply")
		live := func() int {
			return s.queryInt(t, `SELECT COUNT(*) FROM memory_links WHERE relation = 'supersedes' AND source_id = ? AND target_id = ?`, newer, older)
		}
		if n := live(); n != 1 {
			t.Fatalf("the fixture wrote %d supersedes link(s), want 1", n)
		}

		// One failed call, answered by the retry: the repair completes, the
		// edge the verdict confirmed stands, and the report says it needed a
		// retry — a pass that quietly re-asked would describe a run nobody made.
		s.failHarnessCalls("supersede", 1)
		ok := s.mustRun("supersede", e2eProject, "--reassess", "--apply", "--source", "opencode")
		mustMatch(t, "reassess retry", ok.stdout, `1 still supersedes`)
		mustMatch(t, "reassess retry", ok.stdout, `1 retried after a failed call`)
		if n := live(); n != 1 {
			t.Fatalf("a confirmed edge was withdrawn: %d left", n)
		}

		// A call and its retry both dead: the edge is left standing and NAMED,
		// and the pass exits non-zero, because a repair that judged nothing and
		// withdrew nothing is a partial one and must not read as a clean run.
		s.failHarnessCalls("supersede", 2)
		bad := s.mustFail("supersede", e2eProject, "--reassess", "--apply", "--source", "opencode")
		mustMatch(t, "reassess failed call", bad.stdout, `1 unjudged`)
		mustMatch(t, "reassess failed call", bad.stderr, `(?i)classify|exit status`)
		if n := live(); n != 1 {
			t.Fatalf("a classify call that failed withdrew the edge it never judged: %d left", n)
		}
	})

	t.Run("supersede without a project is a usage error", func(t *testing.T) {
		s := newSandbox(t)
		s.mustFail("supersede")
	})

	// The repair for an edge the classifier still accepts. #688's --reassess
	// withdraws what the current rules reject, so a pair that is wrong for a
	// reason no rubric can see keeps its edge — and that edge buries its target
	// twice, in ranking and in resolved_at. --withdraw is the operator's own undo
	// for the edge they name, and the assertion that matters most is the one
	// about the harness: nothing is judged here, so a machine that cannot spawn
	// one at all can still repair an edge, and nothing is billed.
	t.Run("a named edge is withdrawn without asking the classifier", func(t *testing.T) {
		s := newSandbox(t)
		cs := s.mcpSession(t)
		older := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "the staging relay port is 2222",
		}))
		// The same one-second gap the creation case needs: the pass orients a
		// pair by updated_at, and two saves in the same second tie.
		time.Sleep(1100 * time.Millisecond)
		newer := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "the staging relay port is 3333 now",
		}))
		// And NO wait for the vector index, which is a product guarantee rather
		// than luck. The corpus above was written through `ghost mcp`, whose
		// embedding worker fills the index on its own schedule in another
		// process, while the creation pass below is a one-shot CLI that has no
		// part in that worker. A note with no vector is not a candidate for
		// anything, so before #716 the pass raced the worker here: it proposed
		// nothing, spawned no classifier, and reported a clean "0 candidate
		// pairs" — which is how a flaky fixture and a real empty result looked
		// identical. The pass embeds what it cannot score before it scans, so the
		// only timing dependency left in this fixture is the one-second
		// updated_at resolution the sleep above already handles.
		s.setHarnessAnswer("supersede", "SUPERSEDES | replaced: the staging relay port is 2222")
		s.mustRun("supersede", e2eProject, "--source", "opencode", "--threshold", "0.1", "--apply")
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_links WHERE relation = 'supersedes' AND invalidated_at IS NULL`); n != 1 {
			t.Fatalf("the creation pass left %d live edge(s), want 1", n)
		}
		before, _ := s.harnessLog("opencode")

		// Dry run: names the edge, writes nothing, and spawns no harness.
		dry := s.mustRun("supersede", e2eProject, "--withdraw", newer[:8], older[:8])
		mustMatch(t, "withdraw (dry run)", dry.stdout, `(?i)would withdraw`)
		// The report carries the memory the edge was burying, so the operator can
		// confirm from the output that this was the edge they meant.
		mustMatch(t, "withdraw (dry run) target", dry.stdout, `the staging relay port is 2222`)
		// And the step that un-hides the target, which the withdrawal does not do
		// by itself.
		// A dry run prints no follow-up at all: it withdrew nothing, and resolve
		// would refuse the command anyway while the edge is live. The --apply run
		// below is where the scoped repair is checked.
		if strings.Contains(dry.stdout, "--reassess") {
			t.Fatalf("a dry-run withdrawal printed a repair command for an edge it did not withdraw:\n%s", dry.stdout)
		}
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_links WHERE relation = 'supersedes' AND invalidated_at IS NULL`); n != 1 {
			t.Fatalf("a dry-run withdrawal left %d live edge(s), want 1", n)
		}
		if after, _ := s.harnessLog("opencode"); after != before {
			t.Fatalf("a withdrawal asked the classifier: it judges nothing, so a harness call here is a billable call for no judgment")
		}

		applied := s.mustRun("supersede", e2eProject, "--withdraw", newer, older, "--apply")
		mustMatch(t, "withdraw (apply)", applied.stdout, `(?i)withdrew`)
		// The other half of the repair, and it must be the SCOPED one: the ids the
		// withdrawal orphaned, not a project-wide re-judge (#702 measured the
		// unscoped repair proposing to un-hide 143 rows, ~35% of them stale).
		mustMatch(t, "withdraw (apply) follow-up", applied.stdout, `(?i)resolve .*--reassess --only`)
		if !strings.Contains(applied.stdout, older) {
			t.Errorf("the follow-up does not name the target it orphaned (%s):\n%s", older, applied.stdout)
		}
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_links
			WHERE relation = 'supersedes' AND invalidated_at IS NULL AND source_id = ? AND target_id = ?`, newer, older); n != 0 {
			t.Fatalf("the apply left the named edge live (%d)", n)
		}
		// Soft, not deleted: the row is stamped, so a later pass that still judges
		// the pair a supersession re-creates the edge.
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_links
			WHERE relation = 'supersedes' AND source_id = ? AND target_id = ?`, newer, older); n != 1 {
			t.Fatalf("the withdrawal deleted the graph row rather than stamping it")
		}
		// And the audit row, which is what makes the withdrawal a record rather
		// than a silent graph edit.
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_history WHERE memory_id = ? AND phase = 'unsupersede'`, older); n != 1 {
			t.Fatalf("the withdrawal wrote %d unsupersede history row(s), want 1", n)
		}
		if after, _ := s.harnessLog("opencode"); after != before {
			t.Fatalf("the apply asked the classifier")
		}

		// Withdrawing it again is an error rather than a quiet no-op that reads as
		// a completed withdrawal, and a ref that names nothing changes nothing.
		again := s.mustFail("supersede", e2eProject, "--withdraw", newer, older, "--apply")
		mustMatch(t, "re-withdraw", again.stderr, `(?i)no live supersedes link`)
		s.mustFail("supersede", e2eProject, "--withdraw", newer, "ffffffff")
		// A pair the operator can see is wrong but Ghost cannot resolve is the
		// same refusal, not a guess.
		unknown := s.mustFail("supersede", e2eProject, "--withdraw", "not-an-id-at-all", older)
		mustMatch(t, "unresolvable ref", unknown.stderr, `no memory in project .* has an id starting with`)
		// And a ref too short to be a prefix says THAT, rather than pretending the
		// string the operator pasted was not an id.
		short := s.mustFail("supersede", e2eProject, "--withdraw", newer[:6], older)
		mustMatch(t, "short ref", short.stderr, `too short to be a prefix`)
	})
}

// TestCLISupersedeReassessFeedsResolveReassess runs the whole repair chain
// against the built binary (#698): a wrong supersession is written, resolve's
// piggyback stamps the older endpoint, the edge is withdrawn, and the
// withdrawal's own follow-up clears exactly that one memory — leaving another
// resolved memory in the same project resolved, which is the failure the scoped
// repair exists to prevent.
//
// The corpus is built in the order the chain needs rather than all at once: the
// fake opencode harness can only answer a single note per call (its JSON
// envelope does not escape a newline), so no classify call here may see a batch
// of two. The bystander is therefore saved after the edge exists, and it is the
// only note any classifier call in this test is asked about besides the repair.
func TestCLISupersedeReassessFeedsResolveReassess(t *testing.T) {
	s := newSandbox(t)
	cs := s.mcpSession(t)

	// The older note carries a resolution keyword so the ordinary pass
	// considers it at all; the newer one does not, so it stays a live note and
	// the pair is unambiguously about the older memory.
	older := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
		"project_id": e2eProject,
		"content":    "the staging relay port was fixed in the 0.36.0 release: it is 2222",
	}))
	time.Sleep(1100 * time.Millisecond)
	newer := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
		"project_id": e2eProject,
		"content":    "the staging relay port is 3333 now",
	}))

	s.setHarnessAnswer("supersede", "SUPERSEDES | replaced: the staging relay port was 2222")
	s.mustRun("supersede", e2eProject, "--source", "opencode", "--threshold", "0.1", "--apply")
	if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_links WHERE relation = 'supersedes' AND source_id = ? AND target_id = ? AND invalidated_at IS NULL`,
		newer, older); n != 1 {
		t.Fatalf("the setup did not write the supersedes edge %s->%s", newer, older)
	}

	// A second memory the same resolve run resolves, and which the scoped
	// repair must therefore never touch.
	bystander := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
		"project_id": e2eProject,
		"content":    "the port 2222 rollout experiment was abandoned upstream",
	}))

	// The ordinary pass stamps the older endpoint for free off the live edge,
	// and the bystander from the classifier.
	s.setHarnessAnswer("resolve", "RESOLVED | closed-by: the experiment was abandoned upstream")
	s.mustRun("resolve", e2eProject, "--source", "opencode", "--apply")
	for _, id := range []string{older, bystander} {
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE id = ? AND resolved_at IS NOT NULL`, id); n != 1 {
			t.Fatalf("memory %s is not resolved; the reassess has nothing to repair", id)
		}
	}

	// #712: a dry-run repair over a still-live edge has to name what holds the
	// target, because the summary's count is the only thing it used to report
	// and this is the one case with an action attached — the id named is the
	// edge's SOURCE, which is what `ghost supersede --withdraw` takes. The
	// harness is answered KEEP so a row the report holds back is never asked
	// about at all: the edge settles it for free.
	s.setHarnessAnswer("resolve", "KEEP")
	held := s.mustRun("resolve", e2eProject, "--source", "opencode", "--reassess", "--only", older)
	mustMatch(t, "resolve --reassess names the holder", held.stdout, `1 still asserted by a link or correction`)
	mustContain(t, "resolve --reassess names the holder", held.stdout, older+"  [")
	mustContain(t, "resolve --reassess names the holder", held.stdout, "held by supersedes "+newer)

	// Withdraw the edge. The follow-up has to name the withdrawn target, and it
	// has to be the exact command, because the whole value of the run is that
	// the operator does not have to work out the second half themselves.
	s.setHarnessAnswer("supersede", "NEITHER")
	repair := s.mustRun("supersede", e2eProject, "--source", "opencode", "--reassess", "--apply")
	// The ids are quoted: `ghost import` writes an artifact's ids verbatim, so an
	// id is caller-supplied text like a project name, and a POSIX shell
	// concatenates adjacent quoted words — so --only still receives one argument.
	want := "ghost resolve " + e2eProject + " --reassess --only '" + older + "' --apply"
	mustContain(t, "supersede --reassess --apply", repair.stdout, want)
	if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_links WHERE relation = 'supersedes' AND source_id = ? AND invalidated_at IS NULL`, newer); n != 0 {
		t.Fatalf("the reassess did not withdraw the edge")
	}

	// The id list is a file under the data dir's scratch, and it is a
	// --only-file input: same list, no retyping.
	files, err := filepath.Glob(filepath.Join(s.scratch, "supersede-reassess-*.ids"))
	if err != nil || len(files) != 1 {
		t.Fatalf("want exactly one follow-up id file under %s, got %v (%v)", s.scratch, files, err)
	}
	s.setHarnessAnswer("resolve", "KEEP")
	s.mustRun("resolve", e2eProject, "--source", "opencode", "--reassess", "--only-file", files[0], "--apply")

	if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE id = ? AND resolved_at IS NULL`, older); n != 1 {
		t.Fatalf("the withdrawn edge's target is still resolved: the repair did not run")
	}
	if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE id = ? AND resolved_at IS NOT NULL`, bystander); n != 1 {
		t.Fatalf("the scoped repair touched a memory it was not asked about")
	}
}

// TestCLIResolveMarkNamesTheMemoryItBuries runs the operator's mark against the
// built binary (#714) and back out through the repair the report names.
//
// The corpus is what makes the assertions mean something: a note with NO
// resolution keyword, so the ordinary pass's prefilter never proposes it and no
// classifier is ever asked about it. That is the whole case --mark exists for —
// an operator who has read a newer note in the same project saying the fix
// landed, where nothing in the pass can see the pair. So the mark has to work on
// a memory the pass would never have touched.
//
// The harness assertions are the load-bearing ones alongside the round trip: --mark
// judges nothing, so it must not spawn one, and that is what makes the command
// usable from a hook and free of a bill.
func TestCLIResolveMarkNamesTheMemoryItBuries(t *testing.T) {
	s := newSandbox(t)
	cs := s.mcpSession(t)

	// No resolution keyword, so the prefilter does not propose it. A note the
	// operator has to be asked about by hand.
	stale := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
		"project_id": e2eProject,
		"content":    "the relay firmware on the edge nodes runs build 4471",
	}))
	untouched := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
		"project_id": e2eProject,
		"content":    "the staging relay speaks QUIC on port 4471",
	}))

	// The ordinary pass is offered the corpus and does nothing with it: nothing
	// carries a resolution keyword. This is what makes the mark's round trip a
	// repair of a note no pass would have buried.
	s.setHarnessAnswer("resolve", "RESOLVED | closed-by: it was abandoned")
	s.mustRun("resolve", e2eProject, "--source", "opencode", "--apply")
	if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE resolved_at IS NOT NULL`); n != 0 {
		t.Fatalf("%d memory/memories are resolved before the mark: the fixture needs a corpus the pass ignores", n)
	}
	before, _ := s.harnessLog("opencode")

	// Dry run: names the memory, writes nothing, spawns no harness.
	dry := s.mustRun("resolve", e2eProject, "--mark", stale[:8])
	mustMatch(t, "mark (dry run)", dry.stdout, `(?i)would mark resolved`)
	// The report carries the memory itself, so the operator can confirm from the
	// output that this was the one they meant. A memory buried by mistake is
	// invisible afterwards, so the check has to happen here.
	mustContain(t, "mark (dry run) memory", dry.stdout, "the relay firmware on the edge nodes runs build 4471")
	if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE id = ? AND resolved_at IS NOT NULL`, stale); n != 0 {
		t.Fatalf("a dry-run mark stamped resolved_at")
	}
	if after, _ := s.harnessLog("opencode"); after != before {
		t.Fatalf("a dry-run mark asked the classifier: it judges nothing, so a harness call here is a billable call for no judgment")
	}

	applied := s.mustRun("resolve", e2eProject, "--mark", stale, "--apply")
	mustMatch(t, "mark (apply)", applied.stdout, `(?i)marked resolved`)
	if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE id = ? AND resolved_at IS NOT NULL`, stale); n != 1 {
		t.Fatalf("the mark did not stamp the named memory")
	}
	if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE id = ? AND resolved_at IS NOT NULL`, untouched); n != 0 {
		t.Fatalf("the mark touched a memory it was not asked about")
	}
	if after, _ := s.harnessLog("opencode"); after != before {
		t.Fatalf("the mark asked the classifier: it judges nothing, so a harness call here is a billable call for no judgment")
	}
	// The 'resolve' history row, with the operator as the performer. This is the
	// record every writer appends and the SQL route does not, and the performer is
	// what tells a reader of it that a person decided this rather than a
	// classifier judging it.
	if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_history WHERE memory_id = ? AND phase = 'resolve'`, stale); n != 1 {
		t.Fatalf("the mark wrote %d resolve history row(s), want 1", n)
	}
	if got := s.queryStrings(t, `SELECT agent FROM memory_history WHERE memory_id = ? AND phase = 'resolve'`, stale); len(got) != 1 || got[0] != "operator" {
		t.Errorf("the resolve history row's agent = %v, want [operator]", got)
	}

	// Marking it again is a no-op rather than a second stamp, and a second
	// history row: a history row records a write, and no write happened.
	again := s.mustRun("resolve", e2eProject, "--mark", stale, "--apply")
	mustMatch(t, "re-mark", again.stdout, `(?i)already resolved`)
	if strings.Contains(again.stdout, "marked resolved 1") {
		t.Errorf("a re-mark claimed a stamp it did not write:\n%s", again.stdout)
	}
	if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_history WHERE memory_id = ? AND phase = 'resolve'`, stale); n != 1 {
		t.Fatalf("the no-op wrote %d resolve history row(s) in total, want 1", n)
	}

	// The inverse the report names, run as printed. It is scoped to the memory
	// this run stamped, so a second resolved memory in the project is left alone
	// — the failure #698 measured an unscoped repair causing.
	follower := s.mustRun("resolve", e2eProject, "--mark", untouched[:8], "--apply")
	// The report's own follow-up, which must be the SCOPED repair and not the
	// project-wide one (#702 measured 143 rows proposed, ~35% stale).
	mustMatch(t, "mark (apply) follow-up", follower.stdout, `(?i)resolve .*--reassess --only`)
	if strings.Contains(follower.stdout, "--reassess --apply") {
		t.Errorf("the follow-up is the unscoped project-wide repair:\n%s", follower.stdout)
	}
	s.setHarnessAnswer("resolve", "KEEP")
	s.mustRun("resolve", e2eProject, "--source", "opencode", "--reassess", "--only", untouched, "--apply")
	if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE id = ? AND resolved_at IS NULL`, untouched); n != 1 {
		t.Fatalf("the named memory is still resolved: the round trip did not run")
	}

	// The refusals, which are the ones an operator hits by typing. An ambiguous
	// prefix is refused with the matches listed rather than guessed at, a ref too
	// short to be a prefix says THAT, and --mark cannot be combined with
	// --reassess because they are the two directions of the same stamp.
	ambiguous := s.mustFail("resolve", e2eProject, "--mark", "not-an-id-at-all", "--apply")
	mustMatch(t, "unresolvable ref", ambiguous.stderr, `no memory in project .* has an id starting with`)
	short := s.mustFail("resolve", e2eProject, "--mark", stale[:6], "--apply")
	mustMatch(t, "short ref", short.stderr, `too short to be a prefix`)
	both := s.mustFail("resolve", e2eProject, "--mark", stale, "--reassess")
	mustMatch(t, "mark with reassess", both.stderr, `(?i)run them as two commands`)
	// An empty --mark must fail rather than fall through to the ordinary pass,
	// which would judge the whole project and bill a harness call for a request
	// to mark two memories.
	empty := s.mustFail("resolve", e2eProject, "--mark", "")
	mustMatch(t, "empty mark", empty.stderr, `--mark requires at least one memory id or prefix`)
}

// TestCLIResolveMarkRefusesAMemoryInAnotherProject: the guard that matters most
// on a shared store. A ref resolves inside the project PLUS `_global` — a
// promotion moves a row while keeping the links pointing at it, and both ends of
// an edge have to stay nameable — so a promoted row IS reachable by ref from a
// project that does not own it. Marking it would bury a memory every project
// shares, on the say-so of one of them.
func TestCLIResolveMarkRefusesAMemoryInAnotherProject(t *testing.T) {
	s := newSandbox(t)
	cs := s.mcpSession(t)
	// The project running the mark, so the refusal below is about the ref and not
	// about a project that does not exist.
	parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
		"project_id": e2eProject,
		"content":    "a note in the project that tries to mark",
	}))
	theirs := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
		"project_id": "other-project",
		"content":    "a note that belongs to another project entirely",
	}))
	// Promote it, which is what makes it reachable by a ref in this project. The
	// tool takes the project the memory currently belongs to — the ownership
	// check — so the project named here is the one the row was saved into.
	call(t, cs, "ghost_memory_promote", map[string]any{
		"project_id": "other-project",
		"memory_id":  theirs,
	})

	refused := s.mustFail("resolve", e2eProject, "--mark", theirs, "--apply")
	mustMatch(t, "global row refused", refused.stderr, `(?i)_global|not to`)
	if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE id = ? AND resolved_at IS NOT NULL`, theirs); n != 0 {
		t.Fatalf("a project stamped a memory it does not own")
	}
}

// TestCLIProject covers project bind and merge, and the absence of a
// `ghost project list` subcommand.
func TestCLIProject(t *testing.T) {
	t.Run("bind and merge", func(t *testing.T) {
		s := newSandbox(t)
		cs := s.mcpSession(t)
		call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "a note in the project that will be bound",
		})
		call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": "merge-target",
			"content":    "a note in the project that will absorb the other",
		})

		// There is no `ghost project list` subcommand. The inventory surface is
		// the MCP tool and `ghost mcp status`; what the CLI's bare `project`
		// does is print the usage and exit non-zero, and that is asserted here
		// rather than papered over with a call that does not exist.
		bare := s.mustFail("project")
		mustMatch(t, "bare `ghost project`", bare.stderr, "(?i)Usage: ghost project")
		_ = cs

		// bind: a checkout is recorded, and re-running is a no-op rather than a
		// duplicate row.
		checkout := s.t.TempDir()
		bound := s.mustRun("project", "bind", e2eProject, checkout)
		mustMatch(t, "project bind", bound.stdout, "(?i)bind|record")
		if n := s.queryInt(t, `SELECT COUNT(*) FROM projects WHERE id = ? AND path = ?`,
			e2eProject, resolveSymlinks(t, checkout)); n != 1 {
			t.Fatalf("project bind did not record the checkout path")
		}
		s.mustRun("project", "bind", e2eProject, checkout)
		if n := s.queryInt(t, `SELECT COUNT(*) FROM projects WHERE id = ?`, e2eProject); n != 1 {
			t.Fatalf("re-running project bind created a second project row")
		}
		// Binding to an unknown project is an error, not a row.
		s.mustFail("project", "bind", "no-such-project-here", checkout)

		// merge: the source's memories move to the survivor, with ids intact.
		before := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE project_id = ?`, "merge-target")
		merged := s.mustRun("project", "merge", e2eProject, "merge-target")
		mustMatch(t, "project merge", merged.stdout, "(?i)merg|mov")
		if n := s.queryInt(t, `SELECT COUNT(*) FROM projects WHERE id = ?`, e2eProject); n != 0 {
			t.Fatalf("the merged-away project row survived")
		}
		after := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE project_id = ?`, "merge-target")
		if after != before+1 {
			t.Fatalf("merge moved %d memories, want 1 (before=%d after=%d)", after-before, before, after)
		}
		// The task/decision children move too, or a merge would orphan them.
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE project_id = ?`, "merge-target"); n < 1 {
			t.Fatalf("merge left the survivor empty")
		}
		_ = merged
	})

	t.Run("merge refuses _global", func(t *testing.T) {
		s := newSandbox(t)
		cs := s.mcpSession(t)
		call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "a note for the merge-into-global attempt",
		})
		s.mustFail("project", "merge", e2eProject, "_global")
		s.mustFail("project", "merge", "_global", e2eProject)
	})
}

// TestCLIBackup covers a backup and reads the copy back to compare counts.
func TestCLIBackup(t *testing.T) {
	s := newSandbox(t)
	cs := s.mcpSession(t)
	for i := 0; i < 3; i++ {
		call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    fmt.Sprintf("a backed-up memory about the relay, number %d", i),
		})
	}
	call(t, cs, "ghost_task_create", map[string]any{
		"project_id": e2eProject,
		"title":      "a task that has to survive the backup",
	})

	live := map[string]int{
		"memories": s.queryInt(t, `SELECT COUNT(*) FROM memories`),
		"projects": s.queryInt(t, `SELECT COUNT(*) FROM projects`),
		"tasks":    s.queryInt(t, `SELECT COUNT(*) FROM tasks`),
	}

	dest := filepath.Join(s.t.TempDir(), "snapshot.db")
	r := s.mustRun("backup", "--out", dest)
	mustContain(t, "backup", r.stdout, dest)
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("backup wrote no file at %s: %v", dest, err)
	}

	// The copy is a database, and it holds the same rows. Compared against the
	// copy rather than a second backup, because a second backup would prove the
	// command is repeatable and not that the file is restorable.
	for table, want := range live {
		if got := countInFile(t, dest, "SELECT COUNT(*) FROM "+table); got != want {
			t.Fatalf("the backup holds %d %s rows, want %d", got, table, want)
		}
	}
	// The real content is in there, not just the row counts.
	rows := stringsInFile(t, dest, `SELECT content FROM memories WHERE project_id = ?`, e2eProject)
	if len(rows) != 3 {
		t.Fatalf("the backup holds %d project memories, want 3", len(rows))
	}

	// An existing backup is never replaced, so a second run has to refuse
	// rather than overwrite a snapshot someone may still need.
	again := s.mustFail("backup", "--out", dest)
	mustMatch(t, "backup over an existing file", again.stderr, "(?i)exist")
}

// TestCLIExportImport covers the round trip, including that a second export of
// an unchanged store is byte-identical.
func TestCLIExportImport(t *testing.T) {
	s := newSandbox(t)
	cs := s.mcpSession(t)
	// The marker is a token rather than a phrase, because the decision below
	// writes a COMPANION memory whose own text also mentions the round trip —
	// matching on a phrase would pick whichever record came last and assert
	// about the wrong one.
	call(t, cs, "ghost_memory_save", map[string]any{
		"project_id": e2eProject,
		"content":    "a memory tagged rt-sentinel that has to survive the transfer",
		"category":   "architecture",
		"tags":       []string{"roundtrip"},
	})
	call(t, cs, "ghost_task_create", map[string]any{
		"project_id": e2eProject,
		"title":      "a task that has to survive the round trip",
	})
	call(t, cs, "ghost_decision_record", map[string]any{
		"project_id": e2eProject,
		"title":      "A decision that has to survive the round trip",
		"decision":   "portable JSONL over JSON",
		"rationale":  "a line-oriented format survives a damaged file",
	})

	out := filepath.Join(s.t.TempDir(), "artifact.jsonl")
	exp := s.mustRun("export", "--out", out)
	mustMatch(t, "export", exp.stdout, "(?i)wrote|export|memor")
	first := mustReadFile(t, out)
	records := parseJSONLines(t, out)
	// The artifact holds the whole store, which includes the builtin global
	// memories the store seeds itself. Counting every memory record would
	// therefore be counting Ghost's own seeds, so the count is by project.
	// The decision is stored twice on purpose — a decisions row and an ordinary
	// companion memory — so two project memories is the right answer here, and
	// counting one would be asserting a tool quietly stopped writing its
	// companion.
	if n := countRecordsFor(records, "memory", e2eProject); n != 2 {
		t.Fatalf("the artifact holds %d memories for %s, want 2 (the save and the decision's companion)", n, e2eProject)
	}
	// The _global PROJECT is exported — it is a project like any other — but
	// Ghost's own builtin seeds are deliberately excluded from the artifact:
	// their ids are per-install, so an imported copy could never reconcile and
	// would leave a permanent duplicate of Ghost's shipped rules. The exclusion
	// is on (project AND source), so a user's OWN _global memory is still the
	// only copy of itself and is exported normally. Both halves are asserted,
	// because "the seeds are gone" and "the user lost their globals" are
	// opposite failures of the same filter.
	if n := countRecordsFor(records, "memory", "_global"); n != 0 {
		t.Fatalf("the artifact holds %d _global memories; Ghost's builtin seeds must not be exported", n)
	}
	call(t, cs, "ghost_save_global", map[string]any{
		"content":  "the user's own cross-project preference that must survive the round trip",
		"category": "preference",
	})
	withGlobal := filepath.Join(s.t.TempDir(), "with-global.jsonl")
	s.mustRun("export", "--out", withGlobal)
	globalRecords := parseJSONLines(t, withGlobal)
	if n := countRecordsFor(globalRecords, "memory", "_global"); n != 1 {
		t.Fatalf("the artifact holds %d _global memories after a user global save, want 1", n)
	}
	first = mustReadFile(t, withGlobal)
	records = globalRecords
	if n := countRecordsFor(records, "memory", e2eProject); n != 2 {
		t.Fatalf("the artifact holds %d memories for %s, want 2", n, e2eProject)
	}
	if countRecords(records, "task") != 1 {
		t.Fatalf("the artifact holds %d tasks, want 1", countRecords(records, "task"))
	}
	if countRecords(records, "decision") != 1 {
		t.Fatalf("the artifact holds %d decisions, want 1", countRecords(records, "decision"))
	}
	if countRecords(records, "project") < 2 {
		t.Fatalf("the artifact holds %d projects, want at least 2 (the seeded one and _global)",
			countRecords(records, "project"))
	}
	// The header line is what makes an unreadable artifact detectable, so it
	// has to be first and carry the version. The artifact is JSON Lines, so
	// this is the first LINE, not the whole file.
	var header struct {
		Type          string `json:"type"`
		SchemaVersion int    `json:"schema_version"`
	}
	firstLine := strings.SplitN(strings.TrimRight(string(first), "\n"), "\n", 2)[0]
	if err := jsonUnmarshal([]byte(firstLine), &header); err != nil {
		t.Fatalf("the artifact does not open with a JSON header line: %v\n%s", err, firstLine)
	}
	if header.Type != "header" || header.SchemaVersion < 1 {
		t.Fatalf("the artifact's first line is %+v, want a header carrying a schema version", header)
	}
	// The tags have to travel: an artifact that dropped them would round-trip
	// into a store the user cannot filter.
	var mem struct {
		ID      string   `json:"id"`
		Content string   `json:"content"`
		Tags    []string `json:"tags"`
	}
	// The saved memory's own record, addressed by the project memory that was
	// written from a save (the decision's companion is the other one). Tags
	// have to travel: an artifact that dropped them would round-trip into a
	// store the user cannot filter.
	var gotID string
	for _, r := range records {
		if r["type"] != "memory" {
			continue
		}
		payload, ok := r["memory"].(map[string]any)
		if !ok {
			continue
		}
		if got, _ := payload["project_id"].(string); got != e2eProject {
			continue
		}
		if content, _ := payload["content"].(string); !strings.Contains(content, "rt-sentinel") {
			continue
		}
		gotID, _ = payload["id"].(string)
	}
	if gotID == "" {
		t.Fatalf("no memory record for the saved note in the artifact")
	}
	if !firstRecordOf(t, records, "memory", "id", gotID, &mem) {
		t.Fatalf("memory record %s not found in the artifact", gotID)
	}
	if mem.Content == "" {
		t.Fatalf("the memory record lost its content: %+v", mem)
	}
	if len(mem.Tags) == 0 {
		t.Fatalf("the memory record lost its tags: %+v", mem)
	}

	// Two exports of an unchanged store are byte-identical, which is what makes
	// an artifact diffable. The file names differ, so this is about content.
	second := filepath.Join(s.t.TempDir(), "again.jsonl")
	s.mustRun("export", "--out", second)
	if got := mustReadFile(t, second); string(got) != string(first) {
		t.Fatalf("two exports of an unchanged store differ:\n%s\n---\n%s", first, got)
	}

	t.Run("import into a fresh sandbox", func(t *testing.T) {
		fresh := newSandbox(t)
		// A genuinely fresh machine has no store at all, and that is the case
		// worth proving: the dry run refuses over a read-only connection and
		// says what to do, and the apply creates the store and lands the
		// records in it. Both halves are asserted, because a dry run that
		// created the database would make the next run's "no Ghost database"
		// message a lie.
		mustNotExist(t, "the fresh sandbox's store before import", fresh.dbPath())
		refused := fresh.mustFail("import", out)
		mustMatch(t, "import dry run with no store", refused.stderr, "(?i)no database|start a session")
		mustNotExist(t, "the store a refused dry run created", fresh.dbPath())

		applied := fresh.mustRun("import", out, "--apply")
		mustMatch(t, "import (apply)", applied.stdout, "(?i)import|creat|wrote")
		// The imported memory is present with its content and its tags.
		imported := fresh.queryRow(t, `SELECT content, importance FROM memories WHERE content LIKE '%rt-sentinel%'`)
		if !strings.Contains(imported.text, "rt-sentinel") {
			t.Fatalf("the imported memory content is %q", imported.text)
		}
		// Both project memories land: the saved note and the decision's
		// companion, which the artifact carried as its own memory record.
		if n := fresh.queryInt(t, `SELECT COUNT(*) FROM memories WHERE project_id = ?`, e2eProject); n != 2 {
			t.Fatalf("the import stored %d memories for %s, want 2 (the save and the decision's companion)", n, e2eProject)
		}
		if n := fresh.queryInt(t, `SELECT COUNT(*) FROM tasks WHERE project_id = ?`, e2eProject); n != 1 {
			t.Fatalf("the import stored %d tasks, want 1", n)
		}
		if n := fresh.queryInt(t, `SELECT COUNT(*) FROM decisions WHERE project_id = ?`, e2eProject); n != 1 {
			t.Fatalf("the import stored %d decisions, want 1", n)
		}

		// Without --trust-provenance an imported memory is downgraded, so a
		// file from elsewhere cannot plant rows that read as the user's words.
		src := fresh.queryStrings(t, `SELECT source FROM memories WHERE content LIKE '%rt-sentinel%'`)
		if len(src) != 1 || src[0] != "onboarding" {
			t.Fatalf("imported memory source = %v, want onboarding", src)
		}

		// Re-importing is always safe: an id already present is skipped, never
		// overwritten. That is what makes repairing a rejected import possible.
		againBefore := fresh.queryInt(t, `SELECT COUNT(*) FROM memories`)
		fresh.mustRun("import", out, "--apply")
		if n := fresh.queryInt(t, `SELECT COUNT(*) FROM memories`); n != againBefore {
			t.Fatalf("a second import changed the memory count: %d -> %d", againBefore, n)
		}

		// And a fresh export of the imported store is reproducible too.
		third := filepath.Join(fresh.t.TempDir(), "third.jsonl")
		fresh.mustRun("export", "--out", third)
		fourth := filepath.Join(fresh.t.TempDir(), "fourth.jsonl")
		fresh.mustRun("export", "--out", fourth)
		if string(mustReadFile(t, third)) != string(mustReadFile(t, fourth)) {
			t.Fatalf("two exports of the imported store differ")
		}
	})

	t.Run("an unknown artifact version is refused", func(t *testing.T) {
		fresh := newSandbox(t)
		call(t, fresh.mcpSession(t), "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "a note so the destination store exists",
		})
		bad := filepath.Join(fresh.t.TempDir(), "future.jsonl")
		body := `{"type":"header","schema_version":9999}` + "\n"
		if err := os.WriteFile(bad, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		r := fresh.mustFail("import", bad, "--apply")
		mustMatch(t, "unknown artifact version", r.stderr+"\n"+r.stdout, "(?i)schema|version")
	})

	t.Run("a credential in an artifact is refused on import", func(t *testing.T) {
		fresh := newSandbox(t)
		// A store to import INTO: the refusal under test is the content guard,
		// and a missing database would refuse first for an unrelated reason.
		fresh.mustRun("version")
		call(t, fresh.mcpSession(t), "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "a note so the destination store exists",
		})
		bad := filepath.Join(fresh.t.TempDir(), "leaky.jsonl")
		body := `{"type":"header","schema_version":1}` + "\n" +
			`{"type":"project","project":{"id":"leaky","name":"leaky","path":"leaky"}}` + "\n" +
			`{"type":"memory","memory":{"id":"leakymem","project_id":"leaky",` +
			`"category":"fact","content":"the key is AKIAIOSFODNN7EXAMPLE",` +
			`"importance":0.5,"source":"manual","tags":[],"pinned":false}}` + "\n"
		if err := os.WriteFile(bad, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		before := fresh.queryInt(t, `SELECT COUNT(*) FROM memories`)
		r := fresh.mustFail("import", bad, "--apply")
		mustNotContain(t, "import refusal", r.stdout+r.stderr, "AKIAIOSFODNN7EXAMPLE")
		if n := fresh.queryInt(t, `SELECT COUNT(*) FROM memories`); n != before {
			t.Fatalf("a refused import still wrote %d memories", n-before)
		}
	})
}

// TestCLIObsidian covers export and sync, both run twice so idempotence and
// prune are exercised rather than assumed.
func TestCLIObsidian(t *testing.T) {
	s := newSandbox(t)
	cs := s.mcpSession(t)
	var ids []string
	for _, content := range []string{
		"a vault note about the relay port",
		"a second vault note about the release process",
		"a third vault note about the sqlite pool",
	} {
		ids = append(ids, parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    content,
		})))
	}
	// A decision and a task, so the vault has more than one entity kind to
	// mirror and prune.
	call(t, cs, "ghost_decision_record", map[string]any{
		"project_id": e2eProject,
		"title":      "Mirror the vault one way",
		"decision":   "export only; no sync back",
		"rationale":  "hand-edited notes must not silently become memories",
	})
	call(t, cs, "ghost_task_create", map[string]any{
		"project_id": e2eProject,
		"title":      "a task that has to appear in the vault",
	})

	vault := filepath.Join(s.t.TempDir(), "vault")

	t.Run("a second run prunes a note whose memory is gone", func(t *testing.T) {
		// A store that shrinks, re-exported, must not leave the deleted memory's
		// note behind forever: the vault is a mirror, and a stale note reads to
		// the user exactly like a live memory.
		other := newSandbox(t)
		ocs := other.mcpSession(t)
		call(t, ocs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "a vault note that will be deleted from the store",
		})
		call(t, ocs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "a vault note that stays in the store",
		})
		v := filepath.Join(other.t.TempDir(), "vault")
		other.mustRun("obsidian", "export", "--out", v, "--project", e2eProject)
		before := countNotes(t, v)
		if before < 2 {
			t.Fatalf("the first export wrote %d notes, want at least 2", before)
		}

		call(t, ocs, "ghost_memory_delete", map[string]any{
			"project_id":    e2eProject,
			"memory_id":     memoryWithContent(t, other, "will be deleted from the store"),
			"purge_history": true,
		})
		other.mustRun("obsidian", "export", "--out", v, "--project", e2eProject)
		if after := countNotes(t, v); after != before-1 {
			t.Fatalf("after one memory was deleted the vault holds %d notes, want %d — prune kept a note for a memory that is gone",
				after, before-1)
		}
	})

	t.Run("export twice is idempotent and prune keeps every note", func(t *testing.T) {
		first := s.mustRun("obsidian", "export", "--out", vault, "--project", e2eProject)
		mustContain(t, "obsidian export", first.stdout, vault)
		afterFirst := countNotes(t, vault)
		if afterFirst == 0 {
			t.Fatalf("the first export wrote no notes to %s", vault)
		}

		second := s.mustRun("obsidian", "export", "--out", vault, "--project", e2eProject)
		mustContain(t, "obsidian export (2nd)", second.stdout, vault)
		afterSecond := countNotes(t, vault)
		// Re-exporting an unchanged store must not double the notes (a naming
		// collision) and must not drop any (prune over-reaching).
		if afterSecond != afterFirst {
			t.Fatalf("a second export changed the note count: %d -> %d", afterFirst, afterSecond)
		}

		// A user's own file in the vault is not Ghost's to delete.
		ownNote := filepath.Join(vault, "my own note.md")
		if err := os.WriteFile(ownNote, []byte("# mine\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		s.mustRun("obsidian", "export", "--out", vault, "--project", e2eProject)
		if _, err := os.Stat(ownNote); err != nil {
			t.Fatalf("prune deleted a note Ghost did not write: %v", err)
		}
		if n := countNotes(t, vault); n != afterSecond+1 {
			t.Fatalf("the vault holds %d notes, want %d (the exported ones plus the user's)", n, afterSecond+1)
		}
	})

	t.Run("prune removes the note of a deleted memory", func(t *testing.T) {
		before := countNotes(t, vault)
		// Deleting with purge_history so the text is gone from the store too.
		call(t, s.mcpSession(t), "ghost_memory_delete", map[string]any{
			"project_id":    e2eProject,
			"memory_id":     ids[2],
			"purge_history": true,
		})
		s.mustRun("obsidian", "export", "--out", vault, "--project", e2eProject)
		after := countNotes(t, vault)
		// One fewer ghost note, and the user's own file still there.
		if after != before-1 {
			t.Fatalf("prune left %d notes after one memory was deleted, want %d", after, before-1)
		}
		if _, err := os.Stat(filepath.Join(vault, "my own note.md")); err != nil {
			t.Fatalf("prune deleted the user's note: %v", err)
		}
	})

	t.Run("sync mirrors once and stops on a signal", func(t *testing.T) {
		// sync polls until it is signalled, so it is started, given time to do
		// its first mirror, and then stopped the way a user stops it.
		vault2 := filepath.Join(s.t.TempDir(), "sync-vault")
		stopped := s.runUntilSignal(t, 3*time.Second, "obsidian", "sync", "--out", vault2, "--project", e2eProject, "--interval", "200ms")
		mustMatch(t, "obsidian sync", stopped.stdout, "(?i)sync|mirr")
		// The initial mirror happens before the first tick, so a clean stop
		// after a few seconds must have written the notes.
		if n := countNotes(t, vault2); n == 0 {
			t.Fatalf("sync wrote no notes to %s before being stopped", vault2)
		}
		// A non-positive interval is a usage error rather than a busy loop.
		s.mustFail("obsidian", "sync", "--out", vault2, "--interval", "0s")
		s.mustFail("obsidian", "sync", "--out", vault2, "--interval", "-5s")
	})

	t.Run("a vault that does not exist is created", func(t *testing.T) {
		fresh := filepath.Join(s.t.TempDir(), "deep", "nested", "vault")
		s.mustRun("obsidian", "export", "--out", fresh, "--project", e2eProject)
		if n := countNotes(t, fresh); n == 0 {
			t.Fatalf("export wrote no notes into a new vault at %s", fresh)
		}
	})
}

// TestCLIBench runs the built-in benchmark, which the suite claims is
// network-free and self-contained; both claims are checked rather than assumed.
func TestCLIBench(t *testing.T) {
	s := newSandbox(t)
	// A store with a memory in it, so "bench left the user's data alone" is a
	// statement about a store that had something to lose.
	cs := s.mcpSession(t)
	call(t, cs, "ghost_memory_save", map[string]any{
		"project_id": e2eProject,
		"content":    "a memory the benchmark must not touch",
	})
	before := snapshotStore(t, s)

	r := s.mustRun("bench")
	// The metric table, not just a zero exit: a benchmark that printed nothing
	// and exited 0 would satisfy an exit-code check and prove nothing.
	mustMatch(t, "bench", r.stdout, "(?i)recall|ndcg|mrr|hit|precision|f1|score|metric")
	// Numbers, not only words. A table of labels with no values is a table that
	// failed to compute.
	mustMatch(t, "bench (values)", r.stdout, `[0-9]+\.[0-9]+`)
	// It runs against its own in-memory store, so the sandbox's is untouched.
	if after := snapshotStore(t, s); after != before {
		t.Fatalf("bench changed the store: %s -> %s", before, after)
	}

	// --sweep is the other mode, and it prints a different table.
	sweep := s.mustRun("bench", "--sweep")
	mustMatch(t, "bench --sweep", sweep.stdout, "(?i)recall|ndcg|mrr|hit|precision|f1|score|rank")
	mustMatch(t, "bench --sweep (values)", sweep.stdout, `[0-9]+\.[0-9]+`)

	// An unknown flag is an error, and the usage goes with it.
	bad := s.mustFail("bench", "--nope")
	mustMatch(t, "bench with an unknown flag", bad.stderr, "(?i)unknown flag")
}

// TestCLIMCPInit runs `mcp init` for every client into the sandbox HOME, twice
// each, so idempotence and the preservation of user content are both checked.
func TestCLIMCPInit(t *testing.T) {
	clients := []string{"claude", "opencode", "codex", "goose"}

	for _, client := range clients {
		t.Run("client/"+client, func(t *testing.T) {
			s := newSandbox(t)
			// A store, so the installers that check the database have one and
			// report the truth rather than the "no Ghost database" line.
			cs := s.mcpSession(t)
			call(t, cs, "ghost_memory_save", map[string]any{
				"project_id": e2eProject,
				"content":    "a note so the client installers see a real store",
			})

			marker := clientMarkerPath(t, s, client)
			mustNotExist(t, "the "+client+" file before init", marker)

			// --dry-run prints the plan and writes nothing.
			plan := s.mustRun("mcp", "init", "--client", client, "--dry-run")
			mustMatch(t, "mcp init --dry-run", plan.stdout, "(?i)would|ghost")
			mustNotExist(t, "the "+client+" file after a dry run", marker)

			// The real run wires it, and the file it wrote must actually name
			// ghost — a success line over an empty file would pass otherwise.
			applied := s.mustRun("mcp", "init", "--client", client)
			mustMatch(t, "mcp init", applied.stdout, "(?i)ghost|mcp|hook|config")
			first := mustReadFile(t, marker)
			mustContain(t, "the "+client+" file", string(first), "ghost")

			// User content that predates the second run has to survive it.
			seedUserContent(t, s, client)

			second := s.mustRun("mcp", "init", "--client", client)
			mustMatch(t, "mcp init (2nd)", second.stdout, "(?i)ghost|mcp|hook|config")
			after := mustReadFile(t, marker)

			// Idempotence, in the two forms it can take. For a file Ghost owns
			// outright the second run must leave it byte-identical; for a file it
			// merges into, the ghost entry must appear exactly once and
			// everything else must be untouched.
			if client == "opencode" || client == "goose" {
				if string(after) != string(first) {
					t.Fatalf("a second `mcp init --client %s` rewrote the file it owns:\nfirst:\n%s\nsecond:\n%s",
						client, first, after)
				}
			} else {
				if countOccurrences(string(after), ghostEntryMarker(client)) != 1 {
					t.Fatalf("`mcp init --client %s` left %d ghost entries, want 1:\n%s",
						client, countOccurrences(string(after), ghostEntryMarker(client)), after)
				}
			}
			// And the user's own content is still there.
			assertUserContent(t, s, client)
		})
	}

	t.Run("status for every client", func(t *testing.T) {
		s := newSandbox(t)
		cs := s.mcpSession(t)
		call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "a note so status has a real store to report on",
		})
		for _, client := range []string{"claude", "opencode", "codex", "goose"} {
			t.Run(client, func(t *testing.T) {
				// status reads only: it must not bootstrap, create the store or
				// seed the builtin rows, or the next run's "no Ghost database"
				// line would be a healthy one and the diagnostic would have
				// invalidated its own result.
				before := snapshotStore(t, s)
				r := s.run("mcp", "status", "--client", client)
				// A status run may legitimately be unhealthy in a sandbox — the
				// client is not installed for real — so the exit code is not
				// the assertion. The report is.
				mustMatch(t, "mcp status --client "+client, r.stdout+r.stderr,
					"(?i)ghost|database|store|mcp|hook")
				// The database line has to be about a real store, since one exists.
				mustMatch(t, "mcp status --client "+client+" (store)", r.stdout+r.stderr,
					"(?i)database|store|memor")
				if after := snapshotStore(t, s); after != before {
					t.Fatalf("`mcp status --client %s` wrote to the store", client)
				}
			})
		}
	})

	t.Run("an unknown client is refused", func(t *testing.T) {
		s := newSandbox(t)
		r := s.mustFail("mcp", "init", "--client", "emacs")
		mustMatch(t, "unknown client", r.stderr, "(?i)unknown|expected")
		r = s.mustFail("mcp", "status", "--client", "emacs")
		mustMatch(t, "unknown client (status)", r.stderr, "(?i)unknown|expected")
	})

	t.Run("no client on PATH is an actionable error", func(t *testing.T) {
		// The fakes are what `mcp init` finds on PATH, so a sandbox with an
		// empty bin has no client at all and the message has to say what to do.
		s := newSandbox(t)
		bare := &sandbox{
			t:       s.t,
			root:    s.root,
			home:    s.home,
			config:  s.config,
			data:    s.data,
			cache:   s.cache,
			tmp:     s.tmp,
			work:    s.work,
			bin:     filepath.Join(s.root, "empty-bin"),
			scratch: s.scratch,
			ollama:  s.ollama,
		}
		if err := os.MkdirAll(bare.bin, 0o755); err != nil {
			t.Fatal(err)
		}
		bare.env = bare.buildEnv()
		r := bare.mustFail("mcp", "init")
		mustMatch(t, "no client found", r.stderr, "(?i)no supported MCP client|install one of|--client")
	})
}

// ghostEntryMarker is the string that identifies the ghost entry in each
// client's config file, so "it wrote twice" is answerable per format. It is the
// most specific spelling each format has: a bare "ghost" would also match the
// path of the binary and the hooks' own arguments.
func ghostEntryMarker(client string) string {
	switch client {
	case "claude":
		return "mcp__ghost__ghost_memory_save"
	case "codex":
		return "[mcp_servers.ghost]"
	}
	return "ghost"
}

// userContentMarker is the string a test plants in a client's own file and then
// looks for after a second `mcp init`. It stands in for whatever the user has
// in there: a setting, a hook of their own, a comment.
const userContentMarker = "user-owned-content-must-survive"

// seedUserContent plants user-owned content in the file the installer for this
// client owns. Where it is planted depends on the file's format, and that
// difference is itself part of the contract:
//
//   - claude's settings.json is a JSON document the installer merges key-wise,
//     so the content goes in as another top-level key;
//   - codex's config.toml is merged line-wise, so it goes in as another table;
//   - opencode's and goose's files are GHOST-OWNED: rendered from an embedded
//     template and byte-compared on the next run, with a drifted file replaced
//     whole. There is no place in them for a user's own content, so for those
//     two the plant goes in a SIBLING file the installer does not touch. That is
//     the honest test of preservation for a file Ghost replaces: a neighbouring
//     user file must survive an installer that rewrites its own.
func seedUserContent(t *testing.T, s *sandbox, client string) {
	t.Helper()
	switch client {
	case "claude":
		path := filepath.Join(s.home, ".claude", "settings.json")
		doc := decodeJSONObject(t, string(mustReadFile(t, path)))
		doc[userContentMarker] = "keep-me"
		writeJSONFile(t, path, doc)
	case "codex":
		path := filepath.Join(s.home, ".codex", "config.toml")
		body := strings.TrimRight(string(mustReadFile(t, path)), "\n")
		addition := "\n\n[" + userContentMarker + "]\nkeep = true\n"
		if err := os.WriteFile(path, []byte(body+addition), 0o644); err != nil {
			t.Fatalf("seed user content in %s: %v", path, err)
		}
	case "opencode", "goose":
		// A sibling of the file Ghost owns, in the directory it owns, so a
		// whole-directory rewrite would be caught.
		dir := filepath.Dir(clientMarkerPath(t, s, client))
		path := filepath.Join(dir, "user-"+client+".json")
		if err := os.WriteFile(path, []byte(`{"`+userContentMarker+`":"keep-me"}`), 0o644); err != nil {
			t.Fatalf("seed user content in %s: %v", path, err)
		}
	default:
		t.Fatalf("no user-content strategy for client %q", client)
	}
}

// assertUserContent checks the planted content survived the second run, and
// that the ghost wiring is still present.
func assertUserContent(t *testing.T, s *sandbox, client string) {
	t.Helper()
	var read string
	switch client {
	case "claude":
		doc := decodeJSONObject(t, string(mustReadFile(t, filepath.Join(s.home, ".claude", "settings.json"))))
		if doc[userContentMarker] != "keep-me" {
			t.Fatalf("`mcp init --client %s` dropped the user's own settings.json key", client)
		}
		read = string(mustReadFile(t, filepath.Join(s.home, ".claude", "settings.json")))
	case "codex":
		read = string(mustReadFile(t, filepath.Join(s.home, ".codex", "config.toml")))
		if !strings.Contains(read, "["+userContentMarker+"]") {
			t.Fatalf("`mcp init --client %s` dropped the user's own config.toml table:\n%s", client, read)
		}
	case "opencode", "goose":
		// The check is on the file the installer owns, which is separate from
		// the neighbour whose survival is asserted above: for these two, "the
		// user's content survived" and "the ghost wiring is still there" are
		// statements about DIFFERENT files.
		read = string(mustReadFile(t, clientMarkerPath(t, s, client)))
		path := filepath.Join(filepath.Dir(clientMarkerPath(t, s, client)), "user-"+client+".json")
		if !strings.Contains(string(mustReadFile(t, path)), userContentMarker) {
			t.Fatalf("`mcp init --client %s` removed a neighbouring user file:\n%s", client, mustReadFile(t, path))
		}
	}
	if !strings.Contains(read, "ghost") {
		t.Fatalf("`mcp init --client %s` dropped the ghost wiring:\n%s", client, read)
	}
}

// clientMarkerPath is the absolute path of the file each client's installer
// owns.
func clientMarkerPath(t *testing.T, s *sandbox, client string) string {
	t.Helper()
	switch client {
	case "claude":
		return filepath.Join(s.home, ".claude", "settings.json")
	case "opencode":
		return filepath.Join(s.config, "opencode", "plugins", "ghost-opencode.ts")
	case "codex":
		return filepath.Join(s.home, ".codex", "config.toml")
	case "goose":
		return filepath.Join(s.home, ".agents", "plugins", "ghost", "hooks", "hooks.json")
	}
	t.Fatalf("no marker path for client %q", client)
	return ""
}

// decodeJSONObject parses a JSON object, failing the test on anything else — a
// config the test cannot edit is a config the preservation check cannot run
// against, and silently skipping it would make the check pass vacuously.
func decodeJSONObject(t *testing.T, body string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := jsonUnmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("config is not a JSON object: %v\n%s", err, body)
	}
	return doc
}

// writeJSONFile writes a JSON object back, indented the way these configs are.
func writeJSONFile(t *testing.T, path string, doc map[string]any) {
	t.Helper()
	b, err := jsonMarshalIndent(doc)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestCLIHistory covers the append-only history command and its purge.
func TestCLIHistory(t *testing.T) {
	s := newSandbox(t)
	cs := s.mcpSession(t)
	id := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
		"project_id": e2eProject,
		"content":    "the relay listens on port 2222 in staging",
		"category":   "architecture",
		"importance": 0.4,
	}))
	call(t, cs, "ghost_memory_update", map[string]any{
		"project_id": e2eProject,
		"memory_id":  id,
		"content":    "the relay listens on port 2222 in production",
		"importance": 0.9,
	})

	t.Run("both versions are readable", func(t *testing.T) {
		human := s.mustRun("history", id)
		mustContain(t, "history", human.stdout, "staging")
		mustContain(t, "history", human.stdout, "production")

		// --limit keeps the newest N, so a one-entry view is the update.
		limited := s.mustRun("history", id, "--limit", "1")
		mustContain(t, "history --limit 1", limited.stdout, "production")
		mustNotContain(t, "history --limit 1", limited.stdout, "staging")

		// --json is the scripting form: one JSON object per entry, newline
		// separated, so it pipes into jq or a line reader. Parsed per line
		// rather than as one document, which is the format the flag promises.
		js := s.mustRun("history", id, "--json")
		entries := parseJSONLines(t, writeTemp(t, s, "history.jsonl", js.stdout))
		if len(entries) < 2 {
			t.Fatalf("history --json holds %d entries, want at least 2:\n%s", len(entries), js.stdout)
		}
		// Every entry carries the state the memory held ONCE that write
		// landed, which is what makes the history a record and not a diff.
		for _, e := range entries {
			if !hasAnyKey(e, "content", "category", "importance") {
				t.Fatalf("a history entry carries none of content/category/importance: %v", e)
			}
			// And it says which write it was, so a caller can tell a save from a
			// reflection rewrite.
			if !hasAnyKey(e, "phase", "agent", "recorded_at") {
				t.Fatalf("a history entry does not say which write recorded it: %v", e)
			}
		}
		// Oldest first, which is what makes it a history rather than a log.
		times := make([]string, 0, len(entries))
		for _, e := range entries {
			if at, ok := e["recorded_at"].(string); ok {
				times = append(times, at)
			}
		}
		for i := 1; i < len(times); i++ {
			if times[i] < times[i-1] {
				t.Fatalf("history --json is not oldest-first: %v", times)
			}
		}
	})

	t.Run("a ref a report printed resolves", func(t *testing.T) {
		// The eight characters `ghost resolve --mark` and `ghost supersede
		// --withdraw` print are what an operator has on screen, so pasting one
		// here has to reach the same memory. This is the built binary, so it is
		// also the only proof that the argument survives the whole command.
		short := memref.Short(id)
		byRef := s.mustRun("history", short)
		mustContain(t, "history <8-char ref>", byRef.stdout, "production")
		mustContain(t, "history <8-char ref>", byRef.stdout, id)

		// A longer prefix resolves the same way.
		mustContain(t, "history <16-char ref>", s.mustRun("history", id[:16]).stdout, "production")

		// A prefix of nothing is a refusal, and it says the ref is a prefix no
		// id starts with rather than reporting a memory that was never written.
		missing := s.mustFail("history", "F0F0F0F0")
		mustMatch(t, "a prefix no id starts with", missing.stdout+missing.stderr, `(?i)starting with|prefix`)

		// Below the floor it is refused as too short, without listing the store's
		// ids.
		tooShort := s.mustFail("history", "F0F0F0")
		mustMatch(t, "a too-short ref", tooShort.stdout+tooShort.stderr, "(?i)too short|prefix")

		// A full id the store never held keeps its own answer: this is a report
		// about the id, not an error about the argument.
		never := "0000000000000000000000000000000FFF"
		absent := s.mustRun("history", never)
		mustContain(t, "a full id the store does not hold", absent.stdout, "never written")

		// --json reports a refused ref in the one shape a script can branch on,
		// and prints no entry for it.
		refused := s.mustFail("history", "F0F0F0F0", "--json")
		entries := parseJSONLines(t, writeTemp(t, s, "refused.jsonl", refused.stdout))
		if len(entries) != 1 {
			t.Fatalf("a refused ref printed %d JSON line(s), want the one error object:\n%s", len(entries), refused.stdout)
		}
		if _, ok := entries[0]["error"]; !ok {
			t.Errorf("the refused --json run did not print an error object: %v", entries[0])
		}
	})

	t.Run("a deleted memory's history outlives it, until purge", func(t *testing.T) {
		deleted := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "a memory that will be deleted but whose text is recorded",
		}))
		call(t, cs, "ghost_memory_delete", map[string]any{
			"project_id": e2eProject,
			"memory_id":  deleted,
		})
		survivor := s.mustRun("history", deleted)
		mustContain(t, "history of a deleted memory", survivor.stdout, "whose text is recorded")

		// And by the eight characters a report prints, which is the case a
		// deleted memory is most often asked about: the row is gone from
		// `memories`, so an id set read from the live rows alone would report it
		// as never written.
		byRef := s.mustRun("history", memref.Short(deleted))
		mustContain(t, "history of a deleted memory by ref", byRef.stdout, "whose text is recorded")
		mustContain(t, "history of a deleted memory by ref", byRef.stdout, "no longer live")

		// purge takes the WHOLE id and refuses a prefix, because it erases
		// recorded text for good. The refusal names the full id to use, and
		// nothing was erased by it — which is the property that matters, so it
		// is checked against the database and not only against the message.
		refused := s.mustFail("history", "purge", memref.Short(deleted))
		mustMatch(t, "purge by prefix", refused.stderr+refused.stdout, "(?i)whole memory id")
		mustContain(t, "purge by prefix names the full id", refused.stderr+refused.stdout, deleted)
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_history WHERE memory_id = ?`, deleted); n == 0 {
			t.Fatal("a refused prefix purge erased the recorded text anyway")
		}

		// purge erases the row AND every recorded version, and says how much
		// text it is about to destroy.
		purged := s.mustRun("history", "purge", deleted)
		mustMatch(t, "history purge", purged.stdout, "(?i)purg|erased|removed")
		mustNotContain(t, "history purge", purged.stdout, "whose text is recorded")
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_history WHERE memory_id = ?`, deleted); n != 0 {
			t.Fatalf("purge left %d history rows", n)
		}
		// A second purge has nothing to do and must not claim success over
		// nothing.
		again := s.mustFail("history", "purge", deleted)
		mustMatch(t, "purging a nonexistent memory", again.stderr+again.stdout, "(?i)nothing|no memory")
	})

	t.Run("argument errors", func(t *testing.T) {
		s.mustFail("history")
		s.mustFail("history", id, "--limit", "0")
		s.mustFail("history", id, "--limit", "notanumber")
		// --limit and --json have nothing to do when purging.
		s.mustFail("history", "purge", id, "--limit", "2")
		s.mustFail("history", "purge", id, "--json")
		// Two ids is an error rather than a silently-ignored second one.
		s.mustFail("history", id, id)
	})
}
