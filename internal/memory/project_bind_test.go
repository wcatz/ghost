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
	if got.Path != dir {
		t.Errorf("recorded path = %q, want %q", projectPath(t, s, "infra"), dir)
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
