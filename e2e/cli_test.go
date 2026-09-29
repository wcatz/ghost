//go:build e2e

package e2e

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
	{path: "prune", help: "Usage: ghost prune", coveredBy: "TestCLIPruneTiers"},
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
		// The pass report travels with the count, for the reason the flaky-harness
		// case gives: a corpus the scan could not score reports the same empty
		// totals as a classifier that disagreed, and only the report says which
		// (#736).
		if len(edges) != 1 {
			t.Fatalf("the apply wrote %d supersedes link(s), want exactly 1: %v\n--- pass report ---\n%s",
				len(edges), edges, applied.stdout)
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

	// The same fixture under the condition #736 was filed against: a machine
	// busy enough that a liveness probe misses its own deadline.
	//
	// The pass reads the vector index, and a corpus saved through `ghost mcp` is
	// indexed by that server's embedding worker — another process, on its own
	// schedule. The pass fills the gap itself before it scans (issue #716), but
	// that fill was GATED on a reachability probe with a two-second deadline, and
	// a probe that did not answer in two seconds was read as one with no Ollama
	// at all. So on a loaded machine both writers stood down at once: the
	// daemon's sweep and the pass's own fill. The candidate scan then had no
	// vectors to score, `SelectCandidates` counted the whole corpus as unscored,
	// and the pass reported "0 candidate pairs" and EXITED 0 — a clean-looking
	// no-op for a pair the operator could see, which is how these three cases
	// failed a few runs in ten under load while every isolated retry passed.
	//
	// Reproduced here by holding the endpoints rather than by hoping a
	// scheduling race lands: the liveness probe answers, just 2.5s in, and the
	// embed answers 1.4s in, so the worker's own vector for the second memory
	// has not landed by the time the pass starts and the pass's own fill is
	// what has to do it. A request that took too long and an endpoint that is
	// not there must not read the same to the gate, or this corpus is only
	// readable on a fast machine.
	t.Run("a busy endpoint does not make the pass skip its own corpus", func(t *testing.T) {
		s := newSandbox(t)
		// Set BEFORE the saves, so the daemon's worker is slow at them too: the
		// point of the case is that the pass's OWN fill is what has to read this
		// corpus, and a worker that wins the race hides that by accident.
		s.ollama.slowEndpoint(2500*time.Millisecond, 1500*time.Millisecond)
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

		applied := s.mustRun("supersede", e2eProject, "--source", "opencode", "--threshold", "0.1", "--apply")
		edges := s.queryStrings(t,
			`SELECT source_id || '->' || target_id FROM memory_links WHERE relation = 'supersedes'`)
		if want := newer + "->" + older; len(edges) != 1 || edges[0] != want {
			t.Fatalf("the pass wrote %v, want [%s]: it read no corpus it was able to read, "+
				"and its report is what an operator would take for a project holding no near-duplicate pair:\n%s",
				edges, want, applied.stdout)
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
		created := s.mustRun("supersede", e2eProject, "--source", "opencode", "--threshold", "0.1", "--apply")
		live := func() int {
			return s.queryInt(t, `SELECT COUNT(*) FROM memory_links WHERE relation = 'supersedes' AND source_id = ? AND target_id = ?`, newer, older)
		}
		// The creating pass's own report, because "the fixture wrote 0
		// supersedes links" does not say WHICH half of the pass declined: a
		// corpus it could not score reads exactly like a classifier that
		// disagreed, and #736 spent its diagnosis on that ambiguity. Naming
		// the report here is what makes the next occurrence legible.
		if n := live(); n != 1 {
			t.Fatalf("the fixture wrote %d supersedes link(s), want 1\n--- pass report ---\n%s", n, created.stdout)
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
	created := s.mustRun("supersede", e2eProject, "--source", "opencode", "--threshold", "0.1", "--apply")
	if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_links WHERE relation = 'supersedes' AND source_id = ? AND target_id = ? AND invalidated_at IS NULL`,
		newer, older); n != 1 {
		t.Fatalf("the setup did not write the supersedes edge %s->%s\n--- pass report ---\n%s", newer, older, created.stdout)
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

// TestCLIBenchSweepReproducesAcrossProcesses: `ghost bench --sweep` publishes four
// decimals per grid point, and every published number in docs/benchmarks.md is
// read off a run of it. It could not reproduce its own: the benchmark seeded
// every row under the id column's `hex(randomblob(16))` default and with
// `datetime('now')` written per row, and the ranking reads BOTH — the id breaks
// tied fused scores, and the decay factor orders a tied pair of different
// categories by their ages. So a grid point weighting its two legs equally, where
// the ties actually happen, re-drew both on every run: `vec=0.50` took four
// NDCG@10 values and its paired interval crossed zero between runs of one binary,
// while the other five points were byte-identical every time (#708).
//
// This is the cross-process half of that claim, and the half only the built
// binary can make: two separate processes, each seeding its own in-memory store
// from the same committed corpus, each sweeping the FULL six-point grid over all
// 220 queries with its intervals. The in-process test in internal/bench compares
// two seeds at the one affected grid point, because a second full sweep there
// costs ~75s under -race against a package whose CI budget is nearly spent (that
// budget and its headroom are written down in its package-map bullet); here a
// whole sweep is 14s. If the two tables ever differ again, the first place to
// look is what a seeded row's id and created_at are drawn from — corpusID and
// corpusStamp in internal/bench/corpusstore.go.
func TestCLIBenchSweepReproducesAcrossProcesses(t *testing.T) {
	t.Parallel()
	s := newSandbox(t)

	first := s.mustRun("bench", "--sweep")
	second := s.mustRun("bench", "--sweep")

	// The row that moved is named by its weights rather than written down twice:
	// the affected point is the one whose two legs are weighted equally, and that
	// is a property of the grid the binary ships rather than a constant here. If
	// the grid stopped carrying it, this test would pass on a table that no longer
	// covers what it is for.
	if !strings.Contains(first.stdout, "vec=0.50") {
		t.Fatalf("the swept table has no vec=0.50 row, so this test no longer covers the point that was unreproducible:\n%s", first)
	}
	// Intervals too: a point estimate that reproduces is a weaker claim than the
	// paired interval column docs/benchmarks.md quotes off this table.
	if !intervalColumnRE.MatchString(first.stdout) {
		t.Errorf("the swept table prints no paired interval at four decimals:\n%s", first)
	}
	if first.stdout != second.stdout {
		t.Errorf("two processes sweeping the same corpus printed two different tables\nfirst:\n%s\nsecond:\n%s", first.stdout, second.stdout)
	}
}

// intervalColumnRE matches the interval a sweep row prints: a signed mean and a
// signed pair of edges, at the four decimals the report and the published table
// both use. Spelled out here rather than shared with internal/bench because e2e
// is a separate binary and cannot import a test-only regexp out of it.
var intervalColumnRE = regexp.MustCompile(`[+-]\d\.\d{4} \[[+-]\d\.\d{4}, [+-]\d\.\d{4}\]`)

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

// seedPreFixReflectHistory writes the damage #727 stopped new stores taking and
// #730 exists to remove: `versions` byte-identical `reflect` versions of one
// memory, each recorded at a moment in the run, the memory's updated_at moved to
// the run's own time, and the edit's own history row stamped at an instant of its
// own so the restored stamp is about WHICH event it came from rather than about
// the clock. The seeded rows' state columns are copied out of the live row, which
// is exactly what the removed writer's INSERT ... SELECT did — so every one of them
// is byte-identical to its neighbour over every column a version records.
//
// It opens the sandbox store READ-WRITE, which is the one place this suite does:
// every other handle here is read-only so an assertion cannot perturb what it
// observes. There is no other way to reach this state — the built binary no longer
// writes it, and that is the whole point of #727 — so the fixture has to. Every
// ASSERTION below goes back through the read-only handle.
func seedPreFixReflectHistory(t *testing.T, s *sandbox, memoryID string, versions int, editAt, runFrom, runTo string) {
	t.Helper()
	dsn := "file:" + filepath.ToSlash(s.dbPath()) + "?_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open %s read-write to seed history: %v", s.dbPath(), err)
	}
	defer db.Close() //nolint:errcheck

	if _, err := db.Exec(
		`UPDATE memory_history SET recorded_at = ? WHERE memory_id = ? AND phase = 'update'`,
		editAt, memoryID); err != nil {
		t.Fatalf("stamp the update row: %v", err)
	}
	// The LAST version carries the run's end time, so the sequence has the shape a
	// real one had rather than n rows in one instant, and the stamp the damage
	// left is that same value.
	for i := 0; i < versions; i++ {
		at := runFrom
		if i == versions-1 {
			at = runTo
		}
		if _, err := db.Exec(`
			INSERT INTO memory_history
				(memory_id, project_id, phase, recorded_at, content, category, importance, resolved_at, source)
			SELECT id, project_id, 'reflect', ?, content, category, importance, resolved_at, source
			FROM memories WHERE id = ?`, at, memoryID); err != nil {
			t.Fatalf("seed reflect version %d: %v", i, err)
		}
	}
	// And the damage to updated_at: the reflect's own time, not the last real
	// change's.
	if _, err := db.Exec(`UPDATE memories SET updated_at = ? WHERE id = ?`, runTo, memoryID); err != nil {
		t.Fatalf("stage the updated_at damage: %v", err)
	}
}

// seedUnrecordedStampHistory stages the damage #730 deliberately does NOT repair:
// a memory whose every recorded version was written by a writer that moves no
// stamp, so there is no instant to put its stamp back to.
//
// It is the pre-v17 shape — a memory that predates version recording, resolved and
// then unresolved by a build that records an `unresolve` and files no baseline
// before it — with the no-op flood above it that makes the stamp worth repairing on
// any other memory. A first version has no predecessor, so the state comparison
// every other row is judged by called it a change; that is what this stages, and it
// is the case where a repair would have set the stamp to a moment the store had
// never recorded on any column.
//
// Read-write for the same reason seedPreFixReflectHistory is, and with the same
// bargain: the state here is one no current command can produce, and the assertions
// all read back through the read-only handle.
func seedUnrecordedStampHistory(t *testing.T, s *sandbox, memoryID string, versions int, at, stamp string) {
	t.Helper()
	dsn := "file:" + filepath.ToSlash(s.dbPath()) + "?_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open %s read-write to seed history: %v", s.dbPath(), err)
	}
	defer db.Close() //nolint:errcheck

	if _, err := db.Exec(`DELETE FROM memory_history WHERE memory_id = ?`, memoryID); err != nil {
		t.Fatalf("drop the recorded history: %v", err)
	}
	// The unresolve: the first version, and the only one whose writer leaves
	// updated_at alone.
	if _, err := db.Exec(`
		INSERT INTO memory_history
			(memory_id, project_id, phase, recorded_at, content, category, importance, resolved_at, source)
		SELECT id, project_id, 'unresolve', ?, content, category, importance, resolved_at, source
		FROM memories WHERE id = ?`, at, memoryID); err != nil {
		t.Fatalf("seed the unresolve version: %v", err)
	}
	for i := range versions {
		if _, err := db.Exec(`
			INSERT INTO memory_history
				(memory_id, project_id, phase, recorded_at, content, category, importance, resolved_at, source)
			SELECT id, project_id, 'reflect', ?, content, category, importance, resolved_at, source
			FROM memories WHERE id = ?`, at, memoryID); err != nil {
			t.Fatalf("seed reflect version %d: %v", i, err)
		}
	}
	if _, err := db.Exec(`UPDATE memories SET updated_at = ? WHERE id = ?`, stamp, memoryID); err != nil {
		t.Fatalf("stage the undamaged-by-evidence stamp: %v", err)
	}
}

// TestCLIHistoryCompact drives the #730 repair against the built binary, in a
// store holding the damage a pre-#727 build left behind.
//
// What only this layer can see: that `ghost history compact` ROUTES (the word
// sits where a memory id sits, so a word the routing missed would be read as an id
// and answered with "no memory and no history recorded for compact"), that a dry
// run is the default, that the refusal reaches the user, and that the store is
// really unchanged when the command says it wrote nothing.
func TestCLIHistoryCompact(t *testing.T) {
	s := newSandbox(t)
	cs := s.mcpSession(t)
	id := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
		"project_id": e2eProject,
		"content":    "the compaction keeps this save and this edit",
		"category":   "architecture",
		"importance": 0.5,
	}))
	call(t, cs, "ghost_memory_update", map[string]any{
		"project_id": e2eProject,
		"memory_id":  id,
		"content":    "the compaction keeps this save and this edit, in production",
	})

	// The edit's own instant, the reflect run's instants, and a stamp the edit
	// reached. Distinct values, so the assertion is about WHICH event the restored
	// updated_at comes from rather than about the clock.
	const (
		updateAt = "2026-01-01 11:00:00"
		runFrom  = "2026-01-01 12:00:00"
		runTo    = "2026-01-01 12:30:00"
	)
	seedPreFixReflectHistory(t, s, id, 20, updateAt, runFrom, runTo)
	damaged := s.queryInt(t, `SELECT COUNT(*) FROM memory_history WHERE memory_id = ?`, id)
	if damaged != 22 {
		t.Fatalf("the fixture left %d history rows, want 22 (1 save + 1 update + 20 reflect)", damaged)
	}
	// And the run is the LAST write, which is what makes the stamp damage real: a
	// reflect that happened before the edit never moved updated_at past it.

	t.Run("a dry run is the default and writes nothing", func(t *testing.T) {
		before := s.queryInt(t, `SELECT COUNT(*) FROM memory_history WHERE memory_id = ?`, id)
		dry := s.mustRun("history", "compact", "--project", e2eProject)
		mustContain(t, "history compact (dry run)", dry.stdout, "dry run")
		mustContain(t, "history compact (dry run)", dry.stdout, e2eProject)
		mustMatch(t, "history compact (dry run)", dry.stdout, `19 redundant version`)
		// The bound is named, because every count is a count AT one: "19" answers a
		// different question at each instant, and an operator with a store whose
		// clock is behind has no other way to tell whether the default already
		// reached their rows.
		mustContain(t, "history compact (dry run)", dry.stdout, "before 2026-09-28 17:14:07")
		if got := s.queryInt(t, `SELECT COUNT(*) FROM memory_history WHERE memory_id = ?`, id); got != before {
			t.Errorf("the dry run left %d history rows, want the %d it started with", got, before)
		}
		if got := s.queryRow(t, `SELECT updated_at, 0 FROM memories WHERE id = ?`, id).text; got != runTo {
			t.Errorf("the dry run moved updated_at to %q, want it left at the damage (%q)", got, runTo)
		}
	})

	t.Run("--apply removes only the versions that changed nothing", func(t *testing.T) {
		applied := s.mustRun("history", "compact", "--project", e2eProject, "--apply")
		mustNotContain(t, "history compact --apply", applied.stdout, "dry run")
		// Nineteen of the twenty: the last is this memory's newest version, and a
		// memory's newest version is the statement of what it says now, which
		// nothing removes — the same rule the per-memory retention cap applies.
		mustMatch(t, "history compact --apply", applied.stdout, `19 redundant version`)

		if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_history WHERE memory_id = ?`, id); n != 3 {
			t.Fatalf("compaction left %d history rows, want 3 (the save, the edit, and the newest version)", n)
		}
		// The two EVENTS are still there, and the first still holds the wording
		// nothing else keeps. The third row is the newest version, not an event.
		phases := s.queryStrings(t, `SELECT phase FROM memory_history WHERE memory_id = ? ORDER BY rowid`, id)
		if len(phases) != 3 || phases[0] != "save" || phases[1] != "update" || phases[2] != "reflect" {
			t.Fatalf("surviving phases = %v, want [save update reflect]", phases)
		}
		mustContain(t, "history after compaction", s.mustRun("history", id).stdout, "in production")
		// No --fix-updated-at, so the stamp is exactly where the damage left it.
		if got := s.queryRow(t, `SELECT updated_at, 0 FROM memories WHERE id = ?`, id).text; got != runTo {
			t.Errorf("updated_at = %q, want the reflect run's time (%q) — no stamp moves without --fix-updated-at", got, runTo)
		}
	})

	t.Run("--fix-updated-at restores the last real change, in the same run", func(t *testing.T) {
		// A second damaged memory, because the first one's evidence is already gone:
		// the stamp repair asks whether a version that changed nothing sits ABOVE
		// the last real change, and an earlier run that removed those versions has
		// taken the evidence with them. One run, both flags, is also how an operator
		// applies this.
		second := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "the compaction restores this memory's freshness stamp",
			"category":   "architecture",
			"importance": 0.5,
		}))
		call(t, cs, "ghost_memory_update", map[string]any{
			"project_id": e2eProject,
			"memory_id":  second,
			"content":    "the compaction restores this memory's freshness stamp, once",
			"importance": 0.9,
		})
		const (
			secondUpdate = "2026-02-01 11:00:00"
			secondFrom   = "2026-02-01 12:00:00"
			secondTo     = "2026-02-01 12:30:00"
		)
		seedPreFixReflectHistory(t, s, second, 20, secondUpdate, secondFrom, secondTo)

		fixed := s.mustRun("history", "compact", "--project", e2eProject, "--apply", "--fix-updated-at")
		mustMatch(t, "history compact --fix-updated-at", fixed.stdout, `1 updated_at restored`)
		if got := s.queryRow(t, `SELECT updated_at, 0 FROM memories WHERE id = ?`, second).text; got != secondUpdate {
			t.Errorf("updated_at = %q, want the edit's own recorded_at (%q)", got, secondUpdate)
		}
		// And the first memory's already-restored, already-compacted state is not
		// disturbed by a run that had nothing to do for it.
		if got := s.queryRow(t, `SELECT updated_at, 0 FROM memories WHERE id = ?`, id).text; got != runTo {
			t.Errorf("updated_at of the first memory = %q, want it left at %q — its versions were already compacted away", got, runTo)
		}
	})

	t.Run("a second run removes 0", func(t *testing.T) {
		again := s.mustRun("history", "compact", "--project", e2eProject, "--apply", "--fix-updated-at")
		mustMatch(t, "history compact (second run)", again.stdout, `0 redundant version`)
		mustMatch(t, "history compact (second run)", again.stdout, `0 updated_at restored`)
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_history WHERE memory_id = ?`, id); n != 3 {
			t.Errorf("the second run left %d history rows for the first memory, want the 3 the first left", n)
		}
	})

	t.Run("a version a current build wrote is left to --before", func(t *testing.T) {
		// Twenty restatements recorded AFTER #727 shipped, which is what a current
		// build's own byte-identical version looks like: a consolidation merge whose
		// survivor is one of its sources carries the union of the sources' tags, and
		// this table has no column for tags. Nothing in the row says so, so the
		// default bound cannot tell these from the damage and leaves them alone —
		// which is the safe direction, and the one an operator re-runs after an
		// upgrade to reach.
		third := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "a current build's own restatement is not the damage",
			"category":   "architecture",
			"importance": 0.5,
		}))
		call(t, cs, "ghost_memory_update", map[string]any{
			"project_id": e2eProject,
			"memory_id":  third,
			"content":    "a current build's own restatement is not the damage, restated",
		})
		const (
			thirdUpdate = "2026-03-01 11:00:00"
			thirdRun    = "2026-10-01 12:00:00"
		)
		seedPreFixReflectHistory(t, s, third, 20, thirdUpdate, thirdRun, thirdRun)

		// Under the default bound the store has nothing left to remove: the first
		// memory was already compacted and these twenty are newer than the cut.
		underDefault := s.mustRun("history", "compact", "--project", e2eProject, "--fix-updated-at")
		mustMatch(t, "history compact (default bound)", underDefault.stdout, `0 redundant version`)
		mustMatch(t, "history compact (default bound)", underDefault.stdout, `0 updated_at restored`)
		mustContain(t, "history compact (default bound)", underDefault.stdout, "before 2026-09-28 17:14:07")
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_history WHERE memory_id = ?`, third); n != 22 {
			t.Errorf("the default bound left %d history rows, want all 22", n)
		}
		// And the stamp is untouched, because the twenty rows that would have been
		// its evidence are rows this repair will not remove.
		if got := s.queryRow(t, `SELECT updated_at, 0 FROM memories WHERE id = ?`, third).text; got != thirdRun {
			t.Errorf("updated_at = %q, want the run's own time (%q): a row outside the repair is not "+
				"evidence that a reflection moved the stamp", got, thirdRun)
		}

		// Move the cut past them and they are the damage after all — 19 of 20, the
		// last being the memory's newest version. Both runs are dry, so the store is
		// the same store.
		//
		// A bound that reaches past the fix is WARNED about, and this is the only
		// place the warning's stream and its presence in a dry run are observable:
		// the command prints it from runHistoryCompact, and a unit test on the plan
		// cannot see which stream it chose.
		//
		// stderr, not stdout, because stdout is what a script reads for the counts.
		// A warning on stdout ends up inside whatever parses that output, and one on
		// stderr is still seen by the operator at a terminal — which is who has to
		// act on it. And in a DRY RUN, because that is where the operator decides
		// whether to pass --apply: a risk disclosed only by the write is disclosed
		// after the decision.
		widened := s.mustRun("history", "compact", "--project", e2eProject, "--before", "2026-10-02", "--fix-updated-at")
		mustMatch(t, "history compact --before", widened.stdout, `19 redundant version`)
		mustMatch(t, "history compact --before", widened.stdout, `1 updated_at restored`)
		// The bound is the one THIS run used, not the default it did not have to
		// fall back on.
		mustContain(t, "history compact --before", widened.stdout, "before 2026-10-02 00:00:00")
		mustNotContain(t, "history compact --before", widened.stdout, "before 2026-09-28 17:14:07")
		// The warning is on stderr and nowhere else, and it names the risk rather
		// than restating the bound the operator just typed: the tags union is the
		// thing they cannot check from the counts.
		mustContain(t, "history compact --before warning", widened.stderr, "warning:")
		mustContain(t, "history compact --before warning", widened.stderr, "2026-09-28 17:14:07")
		mustContain(t, "history compact --before warning", widened.stderr, "tags")
		mustNotContain(t, "history compact --before warning", widened.stdout, "warning:")
		// The default bound does NOT warn, and this run is the same store one flag
		// narrower. A command whose zero configuration printed a warning would train
		// its reader to skip the one that matters.
		underDefaultAgain := s.mustRun("history", "compact", "--project", e2eProject, "--before", "2026-09-28")
		mustNotContain(t, "history compact (default bound, spelled out)", underDefaultAgain.stderr, "warning:")
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_history WHERE memory_id = ?`, third); n != 22 {
			t.Errorf("the widened dry run left %d history rows, want all 22 — it was a dry run", n)
		}
	})

	t.Run("a memory with no recorded stamp write is reported, not invented", func(t *testing.T) {
		// A fourth memory, in the shape a pre-v17 store has: nothing recorded until
		// an unresolve, which files a version and moves no stamp. The flood above it
		// is the same damage every other memory here carries, so the versions still
		// go — and the stamp has nowhere to go, which is a thing the report has to
		// say rather than a thing a count of zero restored can say for it.
		fourth := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "a memory whose history records no stamp write at all",
			"category":   "architecture",
			"importance": 0.5,
		}))
		const (
			preV17At   = "2026-08-01 09:00:00"
			preV17Stmp = "2026-08-01 09:30:00"
		)
		seedUnrecordedStampHistory(t, s, fourth, 20, preV17At, preV17Stmp)

		fixed := s.mustRun("history", "compact", "--project", e2eProject, "--apply", "--fix-updated-at")
		mustMatch(t, "history compact (unrecorded stamp)", fixed.stdout, `1 stamp\(s\) not restorable, no recorded stamp write`)
		// Not folded into the unreadable count, which is a different fault: an
		// unreadable stamp is a value that exists and cannot be parsed.
		mustNotContain(t, "history compact (unrecorded stamp)", fixed.stdout, "unreadable")
		// The flood is still removed — the version removal never depended on there
		// being a stamp to move — and only the memory's own newest version is kept.
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_history WHERE memory_id = ?`, fourth); n != 2 {
			t.Errorf("the memory kept %d history rows, want 2 (its unresolve and its newest version)", n)
		}
		// And the stamp is exactly where the damage left it, rather than set to the
		// instant the unresolve was recorded — the whole point of the rule.
		if got := s.queryRow(t, `SELECT updated_at, 0 FROM memories WHERE id = ?`, fourth).text; got != preV17Stmp {
			t.Errorf("updated_at = %q, want it left at %q: with no recorded stamp write there is no "+
				"instant to restore it to", got, preV17Stmp)
		}
	})

	t.Run("it refuses while the project's lifecycle lock is held", func(t *testing.T) {
		claim := filepath.Join(s.dataDir(), "lifecycle-"+e2eProject+".pid")
		if err := os.WriteFile(claim, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			t.Fatalf("write a live lifecycle claim: %v", err)
		}
		defer func() { _ = os.Remove(claim) }()

		// This test process is alive, so the claim is a claim by a live run — the
		// same thing a coordinator leaves behind.
		refused := s.mustFail("history", "compact", "--project", e2eProject, "--apply")
		mustContain(t, "history compact under a held lock", refused.stderr+refused.stdout, e2eProject)
		mustMatch(t, "history compact under a held lock", refused.stderr+refused.stdout, "(?i)lock|wait")

		// And the refusal is a refusal: nothing moved. The store here is already
		// compacted, so the check is that a refusal did not rewrite anything on its
		// way out.
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_history WHERE memory_id = ?`, id); n != 3 {
			t.Errorf("the refused run left %d history rows, want the 3 it started with", n)
		}
		if got := s.queryRow(t, `SELECT updated_at, 0 FROM memories WHERE id = ?`, id).text; got != runTo {
			t.Errorf("the refused run moved updated_at to %q, want it left at %q", got, runTo)
		}
	})

	t.Run("argument errors", func(t *testing.T) {
		// A flag that means nothing here is refused rather than ignored: a
		// dropped --fix-updated-at would report a repair that never ran.
		s.mustFail("history", "compact", "--fixit")
		s.mustFail("history", "compact", "--project")
		s.mustFail("history", "compact", "e2e-proj")
		// And a project that names nothing is an error, not an empty report: the
		// alternative is a whole-store compaction by a reader who mistyped.
		s.mustFail("history", "compact", "--project", "no-such-project")
		// A bound the command cannot read is refused before it opens a project. It
		// is not defaulted and it is not passed down: defaulting would delete the
		// rows the operator asked to spare while reporting the default's numbers,
		// and passing it down would refuse it from inside the first project, having
		// already said the store was compactable.
		refused := s.mustFail("history", "compact", "--before", "the day it shipped")
		mustContain(t, "history compact --before refusal", refused.stderr+refused.stdout, "--before")
		s.mustFail("history", "compact", "--before")
		s.mustFail("history", "compact", "--before=")
	})
}

// seedRestatementsNow appends versions byte-identical to the memory's own
// current state, recorded at the current instant, which is the row a pre-#727
// build wrote for every memory a reflection kept and the shape the no-op share
// is measured over (#729).
//
// The timestamp is `datetime('now')` rather than a literal because the report
// reads a WINDOW, not the whole table: rows stamped in January would sit in the
// table and be counted in the store total while the growth rate the line is
// about read as zero.
//
// It opens the sandbox store READ-WRITE, the one place this suite does, for the
// same reason seedPreFixReflectHistory does it: a current build no longer writes
// these rows, so the fixture has to. Every ASSERTION goes back through the
// read-only handle.
func seedRestatementsNow(t *testing.T, s *sandbox, memoryID string, versions int) {
	t.Helper()
	dsn := "file:" + filepath.ToSlash(s.dbPath()) + "?_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open %s read-write to seed history: %v", s.dbPath(), err)
	}
	defer db.Close() //nolint:errcheck

	for i := 0; i < versions; i++ {
		// The state columns are copied out of the live row, which is what the
		// removed writer's INSERT ... SELECT did, so every seeded row is
		// byte-identical to its neighbour over exactly the columns a version
		// records — including resolved_at and source, which is why `IS` rather
		// than `=` is the right comparison in the oracle below too.
		if _, err := db.Exec(`
			INSERT INTO memory_history
				(memory_id, project_id, phase, recorded_at, content, category, importance, resolved_at, source)
			SELECT id, project_id, 'reflect', datetime('now'), content, category, importance, resolved_at, source
			FROM memories WHERE id = ?`, memoryID); err != nil {
			t.Fatalf("seed restatement %d of %s: %v", i, memoryID, err)
		}
	}
	if n := countRestatements(t, s); n == 0 {
		t.Fatal("the fixture wrote no restatement rows at all")
	}
}

// countRestatements is the report's numerator, counted here as an ORACLE rather
// than as a second implementation of the product: e2e is a separate package and
// memory.historyEqualPredecessorSQL is unexported, so the only way to check the
// printed number is to spell the rule again where the test can see it. That
// makes this a copy that can disagree, which is the point — if the product's
// predicate ever changes, this count stops moving and the assertion below fails
// rather than the two drifting together silently.
func countRestatements(t *testing.T, s *sandbox) int {
	t.Helper()
	return s.queryInt(t, `SELECT count(*) FROM memory_history h
	    WHERE h.recorded_at >= datetime('now', '-24 hours') AND `+
		// The predecessor is the row before this one of the SAME memory, in rowid
		// order, and a memory's first version has none — so it reads as a change.
		`EXISTS (SELECT 1 FROM memory_history p WHERE p.rowid = (
		     SELECT max(q.rowid) FROM memory_history q WHERE q.memory_id = h.memory_id AND q.rowid < h.rowid)
		     AND p.content IS h.content AND p.category IS h.category AND p.importance IS h.importance
		     AND p.resolved_at IS h.resolved_at AND p.source IS h.source)`)
}

// historyLineRE and historyToolRE are the two surfaces' one-line reports, as
// patterns that capture the eight numbers each prints. They are separate
// patterns rather than one shared expression because the two lines read
// differently ("busiest memory 3/50 versions" against "busiest memory holds 3 of
// its 50 versions") — and the captures are compared, which is the assertion: two
// surfaces rendering one read differently is the failure this test exists for.
var (
	historyLineRE = regexp.MustCompile(
		`- history: (\d+) version rows in (\d+)h, (\d+) restatements \((\d+)%\), busiest memory (\d+)/(\d+) versions, store (\d+)/(\d+) rows`)
	historyToolRE = regexp.MustCompile(
		`\*\*History:\*\* (\d+) version rows in the last (\d+)h, (\d+) restatements \((\d+)%\) — busiest memory holds (\d+) of its (\d+) versions, store holds (\d+) of (\d+) rows`)
)

// TestCLIMCPStatusHistoryGrowth drives #729 against the built binary: the two
// surfaces that report memory_history growth, asked the same question about one
// store, before and after the damage #730 exists to remove.
//
// What only this layer can see: that `ghost mcp status` and ghost_health both
// print the line at all (the unit tests call the functions, not the two surfaces
// that put them in front of a reader), that the two agree to the digit, that the
// finding names the command that fixes it, and that the store got noisier
// without `mcp status` changing its VERDICT — the quiet run is the control that
// makes that comparison possible.
func TestCLIMCPStatusHistoryGrowth(t *testing.T) {
	s := newSandbox(t)
	cs := s.mcpSession(t)
	id := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
		"project_id": e2eProject,
		"content":    "the status line reports this memory's restatements",
		"category":   "architecture",
		"importance": 0.5,
	}))
	s.mustRun("mcp", "init", "--client", "opencode")

	// The control run: one version per memory, nothing over any threshold, so it
	// prints the history line, no finding, and — the point of asking twice — a
	// verdict the noisy run has to reproduce.
	//
	// opencode rather than claude, and for the verdict specifically. A sandbox
	// cannot be healthy for claude: the `claude` on PATH is a shell script that
	// does not answer `claude mcp get ghost`, so the registration check always
	// fails and the run is red before it says anything about history. opencode's
	// status has no such check — the lifecycle plugin `mcp init` just wrote is
	// compared byte-for-byte against what the same binary renders — so a
	// correctly wired sandbox exits 0, which is what makes the exit code below an
	// assertion rather than a constant.
	before := snapshotStore(t, s)
	quiet := s.run("mcp", "status", "--client", "opencode")
	if quiet.code != 0 {
		// The premise of the comparison below, asserted rather than assumed: a
		// verdict that is already red cannot show that a history finding did not
		// turn it red.
		t.Fatalf("the control run is not healthy, so its exit code says nothing about the history report:\n%s",
			quiet.stdout+quiet.stderr)
	}
	quietReport := quiet.stdout + quiet.stderr
	if after := snapshotStore(t, s); after != before {
		t.Fatalf("the status run wrote to the store:\nbefore %s\nafter  %s", before, after)
	}
	if m := historyLineRE.FindStringSubmatch(quietReport); m == nil {
		t.Fatalf("no history line in the quiet `mcp status`:\n%s", quietReport)
	} else if want := 0; mustAtoi(t, "quiet restatements", m[3]) != want {
		// Zero, not one: every row in this store is some memory's FIRST version,
		// and a first version has no predecessor to restate.
		t.Errorf("the quiet store reports %s restatements, want %d — a first version has no predecessor to restate", m[3], want)
	}
	if got := findWarning(quietReport); got != "" {
		t.Fatalf("a store with no restatements and one version per memory warned:\n%s", got)
	}

	// Six restatements of one state against the save that made it. The share is
	// over the threshold, and it is enough of a day's writing to put the per-memory
	// cap inside the horizon too, so this store is the one an operator would meet
	// in the field: two findings, one repair, and a verdict that does not move.
	const seeded = 6
	seedRestatementsNow(t, s, id, seeded)

	// The numbers the report must print, counted from the store over the same
	// window it uses. seedRestatementsNow's own check only proves the fixture
	// wrote something; these are the assertions.
	total := s.queryInt(t, `SELECT count(*) FROM memory_history`)
	// Over EVERY memory, not just the seeded one: the report's busiest memory is
	// the table's maximum, and the builtin global memory a fresh store carries is
	// in that table too. Scoping it to the fixture's memory would only agree by
	// luck.
	perMemory := s.queryInt(t, `SELECT max(n) FROM (SELECT count(*) n FROM memory_history GROUP BY memory_id)`)
	restatements := countRestatements(t, s)
	inWindow := s.queryInt(t, `SELECT count(*) FROM memory_history WHERE recorded_at >= datetime('now', '-24 hours')`)
	if restatements < seeded {
		t.Fatalf("the fixture seeded %d restatements but the store counts %d — a seeded row is not being read as one", seeded, restatements)
	}
	if inWindow < total {
		t.Fatalf("%d rows in the window but %d in the table", inWindow, total)
	}
	if perMemory < 2 {
		t.Fatalf("the busiest memory holds %d versions, want at least the save and its restatements", perMemory)
	}
	// The share the two lines print is a rounded whole percentage, and the capture
	// is the digits alone — so this is the rounding the report does, restated: the
	// point is that both surfaces round the same ratio the same way, not that a
	// test knows where a 75% comes from.
	wantShare := strconv.Itoa(int(float64(restatements)/float64(inWindow)*100 + 0.5))

	// Both surfaces are asked ONCE, here, so the comparison between them is over
	// the same store at the same moment and a difference between them cannot be
	// explained by a writer having run in between.
	status := s.run("mcp", "status", "--client", "opencode")
	health := call(t, cs, "ghost_health", nil)

	statusLine := historyLineRE.FindStringSubmatch(status.stdout)
	if statusLine == nil {
		t.Fatalf("no history line in `mcp status`:\n%s", status.stdout)
	}
	healthLine := historyToolRE.FindStringSubmatch(health)
	if healthLine == nil {
		t.Fatalf("no history block in ghost_health:\n%s", health)
	}

	t.Run("mcp status prints the growth line and the finding", func(t *testing.T) {
		// Captured position by position against the counts taken from the store:
		// rows in the window, restatements, the share those two make, and the two
		// pairs of (have, cap).
		assertHistoryNumbers(t, "mcp status", statusLine, inWindow, restatements, wantShare, perMemory, total)
		// The finding, and the repair. A warning that names no command is an
		// observation; this one has to name the one that removes what it reports.
		mustMatch(t, "mcp status finding", status.stdout, `! \d+% of the \d+ version rows written in the last 24h restate the version before them`)
		mustContain(t, "mcp status finding", status.stdout, "ghost history compact")
	})

	t.Run("a noisy store does not become a failing mcp status", func(t *testing.T) {
		// Same store, same wiring, same everything except six version rows that
		// said nothing, against a control that exited 0. The verdict is about
		// whether the memory features are reachable, and a table full of noise
		// does not change that — so the noisy run's exit code and its footer are
		// the control's, byte for byte. A `!` line is a finding, not a failed
		// check: had it been one, the run would also have printed the footer
		// pointing at `ghost mcp init`, which repairs wiring and does nothing for
		// a history table.
		if status.code != quiet.code {
			t.Errorf("`mcp status` exits %d with a noisy history and %d with a quiet one — the report took the verdict",
				status.code, quiet.code)
		}
		const wiringFooter = "Run `ghost mcp init` to fix issues."
		if strings.Contains(status.stdout, wiringFooter) {
			t.Errorf("the noisy run printed the wiring footer, and this store is wired:\n%s", status.stdout)
		}
		if !strings.Contains(quiet.stdout, "All checks passed.") {
			t.Errorf("the control run did not report itself healthy, so the comparison above is void:\n%s", quiet.stdout)
		}
	})

	t.Run("ghost_health prints the same line and the same finding", func(t *testing.T) {
		assertHistoryNumbers(t, "ghost_health", healthLine, inWindow, restatements, wantShare, perMemory, total)
		mustMatch(t, "ghost_health finding", health, `⚠ \d+% of the \d+ version rows written in the last 24h restate the version before them`)
		// Additive: everything the tool said before #729 still says, under the
		// same names. A renamed field breaks every agent already reading it.
		mustContain(t, "ghost_health stays additive", health, "**Projects:**")
		mustContain(t, "ghost_health stays additive", health, "**Total memories:**")
		mustContain(t, "ghost_health stays additive", health, "**Memory links:**")
	})

	t.Run("the two surfaces report the same store the same way", func(t *testing.T) {
		// Every captured number, in order, including the two caps: one read, two
		// renderings. This is the assertion the whole shared-read design exists
		// for, and it is the one a unit test on either surface alone cannot make.
		names := []string{"rows in the window", "window hours", "restatements", "the share",
			"the busiest memory's versions", "the per-memory cap", "rows in the table", "the store cap"}
		for i := 1; i <= len(names) && i < len(statusLine) && i < len(healthLine); i++ {
			if statusLine[i] != healthLine[i] {
				t.Errorf("%s: mcp status says %s, ghost_health says %s", names[i-1], statusLine[i], healthLine[i])
			}
		}
		// And the finding is the SENTENCE, not a rephrasing of it: the warning text
		// is built once in internal/memory and both surfaces print it, so a reader
		// told one thing by the terminal and another by their agent is the bug
		// this pins.
		got, want := findWarning(health), findWarning(status.stdout)
		if got == "" || want == "" {
			t.Fatalf("a surface printed no restatement finding:\n ghost_health: %q\n mcp status:    %q", got, want)
		}
		if got != want {
			t.Errorf("the two surfaces warn differently:\n mcp status:    %s\n ghost_health: %s", want, got)
		}
	})

	t.Run("neither run wrote to the store", func(t *testing.T) {
		if after := snapshotStore(t, s); after != before {
			t.Fatalf("the history report wrote to the store:\nbefore %s\nafter  %s", before, after)
		}
		if n := s.queryInt(t, `SELECT count(*) FROM memory_history`); n != total {
			t.Errorf("the history report left %d history rows, want the %d it started with", n, total)
		}
	})
}

// assertHistoryNumbers checks a captured line's eight numbers against what the
// store says, in the order both surfaces print them.
//
// The two CAPS are deliberately not asserted against a literal here. They are
// policy (historyVersionsPerMemory and historyRowsCap), they are unexported, and
// internal/memory's own tests read them where they are readable — so a number
// written down here would be a second source of truth for a constant this test
// is not about, and the failure it invites is the report disagreeing with the
// store. What IS asserted is the invariant a reader would check them for: a cap
// below what the store holds would be a cap the store is not enforcing.
func assertHistoryNumbers(t *testing.T, what string, m []string, inWindow, restatements int,
	wantShare string, perMemory, total int) {
	t.Helper()
	if len(m) != 9 {
		t.Fatalf("%s: captured %d groups from %q, want 8", what, len(m)-1, m[0])
	}
	got := []int{mustAtoi(t, what, m[1]), mustAtoi(t, what, m[3]),
		mustAtoi(t, what, m[5]), mustAtoi(t, what, m[6]),
		mustAtoi(t, what, m[7]), mustAtoi(t, what, m[8])}
	want := []int{inWindow, restatements, perMemory, -1, total, -1}
	names := []string{"rows in the window", "restatements", "busiest memory's versions",
		"the per-memory cap", "rows in the table", "the store cap"}
	for i := range want {
		if want[i] >= 0 && got[i] != want[i] {
			t.Errorf("%s: %s = %d, want %d", what, names[i], got[i], want[i])
		}
	}
	if got[3] < got[2] {
		t.Errorf("%s: the per-memory cap is %d but the busiest memory holds %d versions — a cap the store is not enforcing", what, got[3], got[2])
	}
	if got[5] < got[4] {
		t.Errorf("%s: the store cap is %d but the table holds %d rows — a cap the store is not enforcing", what, got[5], got[4])
	}
	if m[4] != wantShare {
		t.Errorf("%s: share printed as %s%%, want %s%%", what, m[4], wantShare)
	}
}

// findWarning returns a surface's restatement finding, without its `!` or `⚠`
// marker, or "" when the surface printed none. Both surfaces print the same
// sentence, built once in internal/memory, so the marker is the only thing
// allowed to differ between them — and a missing finding is the empty string, so
// a caller can tell "this store is quiet" from "this surface is broken" by
// asking rather than by a helper that fails for both.
func findWarning(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "restate the version before them") {
			return strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "!⚠ "))
		}
	}
	return ""
}

func mustAtoi(t *testing.T, what, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("%s: %q is not a number: %v", what, s, err)
	}
	return n
}
