package memory

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bindStore returns an empty in-memory store: no default project, so each
// subtest creates exactly the rows it needs and a path/remote claimed by one
// row cannot come from the fixture.
func bindStore(t *testing.T) *Store {
	t.Helper()
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return NewStore(db, logger)
}

// sentinelProject creates a project the way MCP does — id and name only, no
// recorded location — which is what every project upgraded from a v9 database
// looks like: path == id, no repository remote.
func sentinelProject(t *testing.T, s *Store, id, name string) {
	t.Helper()
	if err := s.EnsureProject(context.Background(), id, "", name); err != nil {
		t.Fatalf("EnsureProject(%q): %v", id, err)
	}
}

// physical is the path a bind is expected to record for dir: EvalSymlinks, the
// same resolution the session hook applies to a reported cwd. Asserting the
// stored string against this rather than against dir is the whole point — a
// bind that records the path as typed stores a spelling a session may never
// report.
func physical(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", dir, err)
	}
	return resolved
}

// symlinkTo creates a symlink to target inside a fresh temp dir and returns it.
// Skips when symlinks are unavailable (Windows without developer mode).
func symlinkTo(t *testing.T, target string) string {
	t.Helper()
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	return link
}

// projectPath and projectRemote read the row straight from SQLite rather than
// through a store accessor: these tests assert what a bind actually persisted,
// and a reader that shared the writer's normalization would agree with a bug
// in it.
func projectPath(t *testing.T, s *Store, id string) string {
	t.Helper()
	return projectColumn(t, s, id, "path")
}

func projectRemote(t *testing.T, s *Store, id string) string {
	t.Helper()
	var remote sql.NullString
	err := s.db.QueryRowContext(context.Background(), `SELECT repo_remote FROM projects WHERE id = ?`, id).Scan(&remote)
	if err != nil {
		t.Fatalf("read repo_remote of %q: %v", id, err)
	}
	return remote.String
}

func projectColumn(t *testing.T, s *Store, id, column string) string {
	t.Helper()
	var value sql.NullString
	err := s.db.QueryRowContext(context.Background(),
		`SELECT `+column+` FROM projects WHERE id = ?`, id).Scan(&value)
	if err != nil {
		t.Fatalf("read %s of %q: %v", column, id, err)
	}
	return value.String
}

// TestBindProjectPathBindsSentinelProject is the upgrade case: a v9 project
// records its bare name as its path and no remote, so no session directory
// resolves it. Binding is the only supported way to give it a location.
func TestBindProjectPathBindsSentinelProject(t *testing.T) {
	ctx := context.Background()
	s := bindStore(t)
	sentinelProject(t, s, "infra", "infrastructure")

	dir := t.TempDir()
	got, err := s.BindProjectPath(ctx, "infra", dir, "git@github.com:owner/infra.git")
	if err != nil {
		t.Fatalf("BindProjectPath: %v", err)
	}
	if !got.PathChanged || !got.RemoteSet {
		t.Fatalf("binding a sentinel project should change path and set remote: %+v", got)
	}
	if got := projectPath(t, s, "infra"); got != physical(t, dir) {
		t.Errorf("recorded path = %q, want %q", got, physical(t, dir))
	}
	if remote := projectRemote(t, s, "infra"); remote != "github.com/owner/infra" {
		t.Errorf("repo_remote = %q, want %q", remote, "github.com/owner/infra")
	}

	// The whole point of the binding: a session in the checkout now resolves.
	if id, _, err := s.ResolveProject(ctx, dir); err != nil || id != "infra" {
		t.Errorf("ResolveProject(%q) = (%q, %v), want infra", dir, id, err)
	}
}

// TestBindProjectPathWithoutRemote records the location and nothing else: a
// plain directory is a legitimate binding, and inventing a repository for it
// would assert identity nobody reported.
func TestBindProjectPathWithoutRemote(t *testing.T) {
	ctx := context.Background()
	s := bindStore(t)
	sentinelProject(t, s, "infra", "infrastructure")
	dir := t.TempDir()

	got, err := s.BindProjectPath(ctx, "infra", dir, "")
	if err != nil {
		t.Fatalf("BindProjectPath: %v", err)
	}
	if got.RemoteSet {
		t.Errorf("no remote was detected, so none should be recorded: %+v", got)
	}
	if remote := projectRemote(t, s, "infra"); remote != "" {
		t.Errorf("repo_remote = %q, want empty", remote)
	}
	if id, _, err := s.ResolveProject(ctx, dir); err != nil || id != "infra" {
		t.Errorf("ResolveProject(%q) = (%q, %v), want infra", dir, id, err)
	}
}

// TestBindProjectPathIsIdempotent — a second bind of the same project to the
// same path succeeds, changes nothing, and must not bump updated_at or
// re-claim the remote as a conflict with itself.
func TestBindProjectPathIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := bindStore(t)
	sentinelProject(t, s, "infra", "infrastructure")
	dir := t.TempDir()

	first, err := s.BindProjectPath(ctx, "infra", dir, "github.com/owner/infra")
	if err != nil {
		t.Fatalf("first BindProjectPath: %v", err)
	}
	second, err := s.BindProjectPath(ctx, "infra", dir, "github.com/owner/infra")
	if err != nil {
		t.Fatalf("re-binding the same project to the same path must succeed, got %v", err)
	}
	if second.Changed() {
		t.Errorf("re-binding must report no change, got %+v", second)
	}
	if first.Path != second.Path {
		t.Errorf("path changed across an idempotent bind: %q then %q", first.Path, second.Path)
	}
	if second.PreviousPath != second.Path {
		t.Errorf("a no-op bind should report the same path on both sides, got %q → %q",
			second.PreviousPath, second.Path)
	}
}

// TestBindProjectPathStoresPhysicalPath — a bind records the physical path,
// not the one that was typed. A session reports the directory it is standing
// in, which is the physical one; a bind that stored a symlink's own spelling
// would leave the project resolving only for callers who happened to type the
// same alias, and would not be the path the user reads back in the output.
func TestBindProjectPathStoresPhysicalPath(t *testing.T) {
	ctx := context.Background()
	s := bindStore(t)
	sentinelProject(t, s, "infra", "infrastructure")
	real := t.TempDir()
	link := symlinkTo(t, real)

	got, err := s.BindProjectPath(ctx, "infra", link, "")
	if err != nil {
		t.Fatalf("BindProjectPath: %v", err)
	}
	if want := physical(t, real); got.Path != want {
		t.Errorf("reported path = %q, want the physical path %q", got.Path, want)
	}
	if want := physical(t, real); projectPath(t, s, "infra") != want {
		t.Errorf("stored path = %q, want the physical path %q", projectPath(t, s, "infra"), want)
	}
	if id, _, err := s.ResolveProject(ctx, physical(t, real)); err != nil || id != "infra" {
		t.Errorf("ResolveProject of the physical path = (%q, %v), want infra", id, err)
	}
}

// TestBindProjectPathRefusesSymlinkToAnotherProject — the same directory
// reached through a symlink is the same place. Textual comparison treats the
// two spellings as different projects, and the second binding would then sit
// one UNIQUE-constraint violation away from splitting a checkout in half.
func TestBindProjectPathRefusesSymlinkToAnotherProject(t *testing.T) {
	ctx := context.Background()
	s := bindStore(t)
	sentinelProject(t, s, "first", "first")
	sentinelProject(t, s, "second", "second")
	real := t.TempDir()

	if _, err := s.BindProjectPath(ctx, "first", real, ""); err != nil {
		t.Fatalf("binding the first project: %v", err)
	}
	_, err := s.BindProjectPath(ctx, "second", symlinkTo(t, real), "")
	if !errors.Is(err, ErrBindPathClaimed) {
		t.Fatalf("err = %v, want ErrBindPathClaimed for a symlink to the same directory", err)
	}
	if !strings.Contains(err.Error(), "first") {
		t.Errorf("error should name the project that already claims the directory, got %q", err)
	}
	if got := projectPath(t, s, "second"); got != "second" {
		t.Errorf("the refused bind wrote path %q", got)
	}
}

// TestBindProjectPathRefusesPhysicalPathOfAliasedProject is the same
// directory reached from both ends: the first project's recorded path is a
// symlink's own spelling, written by some earlier caller, and the second bind
// arrives with the physical path. Comparing the stored TEXT would see two
// different strings and let the physical directory be claimed a second time.
func TestBindProjectPathRefusesPhysicalPathOfAliasedProject(t *testing.T) {
	ctx := context.Background()
	s := bindStore(t)
	sentinelProject(t, s, "first", "first")
	sentinelProject(t, s, "second", "second")
	real := t.TempDir()
	link := symlinkTo(t, real)

	// Record the alias directly, the way a save over MCP with a path-shaped
	// project_id would have.
	if err := s.EnsureProject(ctx, "first", link, "first"); err != nil {
		t.Fatalf("EnsureProject with an aliased path: %v", err)
	}
	_, err := s.BindProjectPath(ctx, "second", real, "")
	if !errors.Is(err, ErrBindPathClaimed) {
		t.Fatalf("err = %v, want ErrBindPathClaimed — the alias and the physical path are one directory", err)
	}
	if got := projectPath(t, s, "second"); got != "second" {
		t.Errorf("the refused bind wrote path %q", got)
	}
}

// TestBindProjectPathIsIdempotentThroughASymlink — the same checkout re-bound
// through the same alias is the same binding. It matters because bind now
// records the physical path: comparing only the text would report the re-run
// as a change and rewrite updated_at every time a user re-ran the command
// printed by `ghost mcp status`.
func TestBindProjectPathIsIdempotentThroughASymlink(t *testing.T) {
	ctx := context.Background()
	s := bindStore(t)
	sentinelProject(t, s, "infra", "infrastructure")
	real := t.TempDir()
	link := symlinkTo(t, real)

	first, err := s.BindProjectPath(ctx, "infra", link, "")
	if err != nil {
		t.Fatalf("first bind: %v", err)
	}
	second, err := s.BindProjectPath(ctx, "infra", link, "")
	if err != nil {
		t.Fatalf("re-binding through the same symlink: %v", err)
	}
	if second.Changed() {
		t.Errorf("re-binding the same directory through the same symlink must report no change, got %+v (first %+v)", second, first)
	}
}

// TestBindProjectPathRewritesAnAliasedPath — a project recorded under a
// symlink's own spelling is rewritten to the physical path, and the rewrite is
// reported. It has to be: the resolver narrows its candidate query on the
// stored TEXT, so an alias and its target share no prefix, and a session
// reporting the physical directory — which is what a shell's working directory
// is — would find nothing. Leaving the alias in place would leave the project
// exactly as unresolvable as bind was called to fix it.
func TestBindProjectPathRewritesAnAliasedPath(t *testing.T) {
	ctx := context.Background()
	s := bindStore(t)
	sentinelProject(t, s, "infra", "infrastructure")
	real := t.TempDir()
	link := symlinkTo(t, real)
	if err := s.EnsureProject(ctx, "infra", link, "infrastructure"); err != nil {
		t.Fatalf("EnsureProject with an aliased path: %v", err)
	}

	got, err := s.BindProjectPath(ctx, "infra", real, "")
	if err != nil {
		t.Fatalf("BindProjectPath: %v", err)
	}
	if !got.PathChanged {
		t.Errorf("rewriting the alias to the physical path is a change: %+v", got)
	}
	if recorded := projectPath(t, s, "infra"); recorded != physical(t, real) {
		t.Errorf("stored path = %q, want the physical path %q", recorded, physical(t, real))
	}
	if id, _, err := s.ResolveProject(ctx, real); err != nil || id != "infra" {
		t.Errorf("ResolveProject of the physical directory = (%q, %v), want infra", id, err)
	}
}

// TestBindProjectPathRefusesUnmatchablePath — the resolver's candidate query
// skips recorded paths of ten characters or fewer, so binding one records a
// project that no session directory can ever find. The command exists to make
// a project resolvable, so a path it would leave unresolvable is a refusal,
// not a success.
func TestBindProjectPathRefusesUnmatchablePath(t *testing.T) {
	ctx := context.Background()
	s := bindStore(t)
	sentinelProject(t, s, "infra", "infrastructure")

	// The filter is on the stored path's length, so the test needs a real
	// directory whose PHYSICAL path is short. A deep temp root cannot provide
	// one, which is why this skips rather than silently passing.
	short := filepath.Join(os.TempDir(), "gb1")
	if resolved, err := filepath.EvalSymlinks(short); err == nil && len(resolved) > 10 {
		t.Skipf("no short physical path available under %s (temp root is %d chars)", os.TempDir(), len(resolved))
	}
	if err := os.Mkdir(short, 0o700); err != nil {
		t.Skipf("cannot create %s: %v", short, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(short) })

	if _, err := s.BindProjectPath(ctx, "infra", short, ""); !errors.Is(err, ErrBindPathUnmatchable) {
		t.Fatalf("err = %v, want ErrBindPathUnmatchable for a path the resolver skips", err)
	}
	if got := projectPath(t, s, "infra"); got != "infra" {
		t.Errorf("the refused bind wrote path %q", got)
	}
}

// TestBindProjectPathRefusesAncestorOfAnotherProject is the #546 shape aimed
// at bind: binding ~/git would record a prefix that matches every unregistered
// clone beneath it, so a session in one of those clones would resolve to this
// project and read its memories.
func TestBindProjectPathRefusesAncestorOfAnotherProject(t *testing.T) {
	ctx := context.Background()
	s := bindStore(t)
	sentinelProject(t, s, "inner", "inner")
	sentinelProject(t, s, "outer", "outer")
	parent := t.TempDir()
	inner := filepath.Join(parent, "checkout")
	if err := os.Mkdir(inner, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	if _, err := s.BindProjectPath(ctx, "inner", inner, ""); err != nil {
		t.Fatalf("binding the inner project: %v", err)
	}
	_, err := s.BindProjectPath(ctx, "outer", parent, "")
	if !errors.Is(err, ErrBindPathContainsOther) {
		t.Fatalf("err = %v, want ErrBindPathContainsOther for an ancestor of another checkout", err)
	}
	if !strings.Contains(err.Error(), "inner") {
		t.Errorf("error should name the project inside the path, got %q", err)
	}
	if got := projectPath(t, s, "outer"); got != "outer" {
		t.Errorf("the refused bind wrote path %q", got)
	}
}

// TestBindProjectPathRefusesDescendantOfUnidentifiedProject — a project
// recorded at ~/git claims every directory under it by path prefix, so a
// project bound inside that subtree could never be returned by the resolver.
// When the enclosing project records a repository remote it is identified by
// repository rather than by directory, and a nested checkout (a submodule, a
// vendored repo) is legitimately a different project — so that case binds and
// resolves to the nested project.
func TestBindProjectPathRefusesDescendantOfUnidentifiedProject(t *testing.T) {
	ctx := context.Background()

	setup := func(t *testing.T, enclosingRemote string) (*Store, string) {
		t.Helper()
		s := bindStore(t)
		sentinelProject(t, s, "enclosing", "enclosing")
		sentinelProject(t, s, "nested", "nested")
		enclosing := t.TempDir()
		if _, err := s.BindProjectPath(ctx, "enclosing", enclosing, enclosingRemote); err != nil {
			t.Fatalf("binding the enclosing project: %v", err)
		}
		nested := filepath.Join(enclosing, "vendor", "lib")
		if err := os.MkdirAll(nested, 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		return s, nested
	}

	t.Run("enclosing project has no remote", func(t *testing.T) {
		s, nested := setup(t, "")
		_, err := s.BindProjectPath(ctx, "nested", nested, "")
		if !errors.Is(err, ErrBindPathInsideOther) {
			t.Fatalf("err = %v, want ErrBindPathInsideOther", err)
		}
		if !strings.Contains(err.Error(), "enclosing") {
			t.Errorf("error should name the enclosing project, got %q", err)
		}
		if got := projectPath(t, s, "nested"); got != "nested" {
			t.Errorf("the refused bind wrote path %q", got)
		}
	})

	t.Run("enclosing project has a remote", func(t *testing.T) {
		s, nested := setup(t, "github.com/owner/enclosing")
		if _, err := s.BindProjectPath(ctx, "nested", nested, "github.com/owner/nested"); err != nil {
			t.Fatalf("a nested checkout under a repository-identified project must bind: %v", err)
		}
		// The longer recorded path has to win, or the nested project would be
		// unreachable and the bind would have been pointless.
		if id, _, err := s.ResolveProject(ctx, nested); err != nil || id != "nested" {
			t.Errorf("ResolveProject(%q) = (%q, %v), want nested", nested, id, err)
		}
	})
}

// TestBindProjectPathTreatsSpellingsAsOneLocation — a project written by some
// other caller may hold "/x/checkout/" while the user types "/x/checkout", and
// those are one directory. The re-run has to be recognised as the same binding
// rather than reported as a change, and a second project must not be able to
// claim the same directory under a different spelling.
func TestBindProjectPathTreatsSpellingsAsOneLocation(t *testing.T) {
	ctx := context.Background()
	s := bindStore(t)
	sentinelProject(t, s, "first", "first")
	sentinelProject(t, s, "second", "second")
	dir := t.TempDir()

	if _, err := s.BindProjectPath(ctx, "first", dir+"/", ""); err != nil {
		t.Fatalf("binding with a trailing separator: %v", err)
	}
	if got, err := s.BindProjectPath(ctx, "first", dir, ""); err != nil {
		t.Fatalf("re-binding the same directory: %v", err)
	} else if got.Changed() {
		t.Errorf("a trailing separator is not a different location: %+v", got)
	}
	// "/x/./checkout" cleans to the same directory.
	if _, err := s.BindProjectPath(ctx, "second", dir+"/./", ""); !errors.Is(err, ErrBindPathClaimed) {
		t.Errorf("binding the same directory under another spelling: err = %v, want ErrBindPathClaimed", err)
	}
}

// TestBindProjectPathRefusesGlobal — _global is the injection bucket for every
// project, not a checkout, so it never gets a path or a repository.
func TestBindProjectPathRefusesGlobal(t *testing.T) {
	ctx := context.Background()
	s := bindStore(t)
	if err := s.SeedGlobalMemories(ctx); err != nil {
		t.Fatalf("SeedGlobalMemories: %v", err)
	}
	dir := t.TempDir()

	if _, err := s.BindProjectPath(ctx, "_global", dir, ""); !errors.Is(err, ErrBindGlobalProject) {
		t.Fatalf("bind _global: err = %v, want ErrBindGlobalProject", err)
	}
	if got := projectPath(t, s, "_global"); got != "_global" {
		t.Errorf("_global path = %q, want the id sentinel unchanged", got)
	}
}

func TestBindProjectPathRefusesUnknownProject(t *testing.T) {
	s := bindStore(t)
	if _, err := s.BindProjectPath(context.Background(), "nope", t.TempDir(), ""); !errors.Is(err, ErrBindProjectNotFound) {
		t.Fatalf("err = %v, want ErrBindProjectNotFound", err)
	}
}

// TestBindProjectPathRefusesUnusablePath keeps the recorded path inside the
// shapes a session directory can be compared against. A bare root would claim
// every directory on the machine; a relative one would be resolved against the
// process's working directory.
func TestBindProjectPathRefusesUnusablePath(t *testing.T) {
	root := string(filepath.Separator)
	for _, path := range []string{"", ".", "..", "relative/checkout", root} {
		t.Run("path="+path, func(t *testing.T) {
			ctx := context.Background()
			s := bindStore(t)
			sentinelProject(t, s, "infra", "infrastructure")

			if _, err := s.BindProjectPath(ctx, "infra", path, ""); !errors.Is(err, ErrBindPathUnusable) {
				t.Fatalf("err = %v, want ErrBindPathUnusable", err)
			}
			if got := projectPath(t, s, "infra"); got != "infra" {
				t.Errorf("path = %q, want the id sentinel unchanged", got)
			}
		})
	}
}

// TestBindProjectPathRefusesPathClaimedByAnother is #546's rule stated for the
// one write that can still attach a location to a project: two projects must
// not record the same checkout, or a session there would resolve to whichever
// row the prefix ranking happened to favour.
func TestBindProjectPathRefusesPathClaimedByAnother(t *testing.T) {
	ctx := context.Background()
	s := bindStore(t)
	sentinelProject(t, s, "first", "first")
	sentinelProject(t, s, "second", "second")
	dir := t.TempDir()

	if _, err := s.BindProjectPath(ctx, "first", dir, ""); err != nil {
		t.Fatalf("binding the first project: %v", err)
	}
	_, err := s.BindProjectPath(ctx, "second", dir, "")
	if !errors.Is(err, ErrBindPathClaimed) {
		t.Fatalf("err = %v, want ErrBindPathClaimed", err)
	}
	if !strings.Contains(err.Error(), "first") {
		t.Errorf("error should name the project that already claims the path, got %q", err)
	}
	if got := projectPath(t, s, "second"); got != "second" {
		t.Errorf("the refused bind wrote path %q", got)
	}
}

// TestBindProjectPathRefusesRemoteClaimedByAnother — one repository is one
// project, so binding a second project to a checkout of a repository another
// project already claims would create a duplicate project with its own
// memories.
func TestBindProjectPathRefusesRemoteClaimedByAnother(t *testing.T) {
	ctx := context.Background()
	s := bindStore(t)
	sentinelProject(t, s, "first", "first")
	sentinelProject(t, s, "second", "second")
	firstDir, secondDir := t.TempDir(), t.TempDir()

	if _, err := s.BindProjectPath(ctx, "first", firstDir, "git@github.com:owner/shared.git"); err != nil {
		t.Fatalf("binding the first project: %v", err)
	}
	_, err := s.BindProjectPath(ctx, "second", secondDir, "https://github.com/owner/shared")
	if !errors.Is(err, ErrBindRemoteClaimed) {
		t.Fatalf("err = %v, want ErrBindRemoteClaimed", err)
	}
	if got := projectPath(t, s, "second"); got != "second" {
		t.Errorf("the refused bind wrote path %q", got)
	}
}

// TestBindProjectPathRefusesDifferentRemote — a project already identified as
// a checkout of one repository is not re-identified as a checkout of another.
func TestBindProjectPathRefusesDifferentRemote(t *testing.T) {
	ctx := context.Background()
	s := bindStore(t)
	sentinelProject(t, s, "infra", "infrastructure")
	firstDir := t.TempDir()
	if _, err := s.BindProjectPath(ctx, "infra", firstDir, "github.com/owner/infra"); err != nil {
		t.Fatalf("first bind: %v", err)
	}

	_, err := s.BindProjectPath(ctx, "infra", t.TempDir(), "github.com/owner/other")
	if !errors.Is(err, ErrBindRemoteConflict) {
		t.Fatalf("err = %v, want ErrBindRemoteConflict", err)
	}
	if got := projectPath(t, s, "infra"); got != firstDir {
		t.Errorf("the refused bind changed the path to %q", got)
	}
	if remote := projectRemote(t, s, "infra"); remote != "github.com/owner/infra" {
		t.Errorf("the refused bind changed the remote to %q", remote)
	}
}

// TestBindProjectPathKeepsExistingRemoteOnRepath — re-pointing a bound project
// at a moved checkout must not drop the repository it already claims, even
// though no remote is detected at the new location.
func TestBindProjectPathKeepsExistingRemoteOnRepath(t *testing.T) {
	ctx := context.Background()
	s := bindStore(t)
	sentinelProject(t, s, "infra", "infrastructure")
	firstDir := t.TempDir()
	if _, err := s.BindProjectPath(ctx, "infra", firstDir, "github.com/owner/infra"); err != nil {
		t.Fatalf("first bind: %v", err)
	}

	moved := t.TempDir()
	got, err := s.BindProjectPath(ctx, "infra", moved, "")
	if err != nil {
		t.Fatalf("repath: %v", err)
	}
	if !got.PathChanged || got.RemoteSet {
		t.Errorf("repath should change only the path: %+v", got)
	}
	if remote := projectRemote(t, s, "infra"); remote != "github.com/owner/infra" {
		t.Errorf("repo_remote = %q, want the original remote kept", remote)
	}
}

// TestListUnboundProjects is the `ghost mcp status` notice: projects a
// session directory can never resolve, each of which silently loses
// session-start injection and Stop-hook lifecycle work.
func TestListUnboundProjects(t *testing.T) {
	ctx := context.Background()
	s := bindStore(t)
	if err := s.SeedGlobalMemories(ctx); err != nil {
		t.Fatalf("SeedGlobalMemories: %v", err)
	}
	sentinelProject(t, s, "unbound", "unbound-name")
	sentinelProject(t, s, "bound", "bound-name")
	sentinelProject(t, s, "remote-only", "remote-only-name")
	sentinelProject(t, s, "path-only", "path-only-name")
	sentinelProject(t, s, "remote-no-path", "remote-no-path-name")
	// A repository remote is enough to resolve a project, so this one still
	// works from any checkout of the repository even though its recorded path
	// is the id sentinel — the shape #595's name-based binding produces.
	if err := s.EnsureProjectWithRepo(ctx, "remote-no-path", "", "remote-no-path-name",
		"github.com/owner/remote-no-path"); err != nil {
		t.Fatalf("EnsureProjectWithRepo: %v", err)
	}

	unboundDir, remoteDir, pathDir := t.TempDir(), t.TempDir(), t.TempDir()
	if _, err := s.BindProjectPath(ctx, "bound", unboundDir, ""); err != nil {
		t.Fatalf("bind bound: %v", err)
	}
	if _, err := s.BindProjectPath(ctx, "remote-only", remoteDir, "github.com/owner/remote-only"); err != nil {
		t.Fatalf("bind remote-only: %v", err)
	}
	if _, err := s.BindProjectPath(ctx, "path-only", pathDir, ""); err != nil {
		t.Fatalf("bind path-only: %v", err)
	}

	got, err := s.ListUnboundProjects(ctx)
	if err != nil {
		t.Fatalf("ListUnboundProjects: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d unbound projects (%+v), want only the sentinel-path one", len(got), got)
	}
	if got[0].ID != "unbound" || got[0].Name != "unbound-name" {
		t.Errorf("got %+v, want the project with a sentinel path and no remote", got[0])
	}
}
