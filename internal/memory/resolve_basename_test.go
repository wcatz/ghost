package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveBasenameRefusesUnrelatedDirectory is issue #546.
//
// ResolveProject's last step matches filepath.Base(input) against
// projects.name, and projects.name is not unique. An unrelated clone parked
// at ~/Downloads/infra therefore resolves to the real infra project, and a
// session starting there gets that project's memories — hosts, IPs,
// topology — injected as trusted context, while anything it saves lands in
// the real project too.
//
// The directory a session reports is positive evidence about where it is. A
// project that has recorded a real path only agrees with a session actually
// inside that tree.
func TestResolveBasenameRefusesUnrelatedDirectory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Short path, so the prefix step's LENGTH(path) > 10 guard skips it and
	// the basename fallback is the only thing that can answer — which is
	// exactly the exposure.
	if err := s.EnsureProject(ctx, "infra-id", "/x/infra", "infra"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	id, name, err := s.ResolveProject(ctx, "/some/unrelated/place/infra")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "" || name != "" {
		t.Errorf("an unrelated directory claimed project %q (%q) by sharing a basename; another project's memories would be injected here", id, name)
	}
}

// TestResolveBasenameStillMatchesOwnDirectory guards the other half: the
// fallback must keep working for a session genuinely in the project, because
// the prefix step skips paths this short. Refusing everything would break
// short-path projects entirely rather than merely securing them.
func TestResolveBasenameStillMatchesOwnDirectory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	projectPath := filepath.Join(t.TempDir(), "infra")
	subdir := filepath.Join(projectPath, "sub")
	for _, dir := range []string{projectPath, subdir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	if err := s.EnsureProject(ctx, "infra-id", projectPath, "infra"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	id, name, err := s.ResolveProject(ctx, projectPath)
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "infra-id" || name != "infra" {
		t.Errorf("session inside its own project did not resolve: id=%q name=%q, want infra-id/infra", id, name)
	}

	id, name, err = s.ResolveProject(ctx, subdir)
	if err != nil {
		t.Fatalf("ResolveProject subdir: %v", err)
	}
	if id != "infra-id" || name != "infra" {
		t.Errorf("session in a project subdirectory resolved to %q (%q), want infra-id/infra", id, name)
	}
}

// TestResolveBasenameIgnoresCandidatesThatDisagree: ambiguity is decided on
// the candidates that AGREE with the evidence, not on the raw candidate set.
// Deciding it before the filters strands a session that is standing in the
// right directory because an unrelated duplicate of the name happens to
// exist — the caller loses its project for the sake of a row that the same
// evidence rules reject.
func TestResolveBasenameIgnoresCandidatesThatDisagree(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	root := t.TempDir()
	realPath := filepath.Join(root, "real", "infra")
	aliasPath := filepath.Join(root, "alias", "infra")
	if err := os.MkdirAll(realPath, 0o755); err != nil {
		t.Fatalf("mkdir real path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(aliasPath), 0o755); err != nil {
		t.Fatalf("mkdir alias parent: %v", err)
	}
	if err := os.Symlink(realPath, aliasPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	if err := s.EnsureProject(ctx, "real-id", aliasPath, "infra"); err != nil {
		t.Fatalf("EnsureProject real: %v", err)
	}
	if err := s.EnsureProject(ctx, "dup-id", "", "infra"); err != nil {
		t.Fatalf("EnsureProject duplicate: %v", err)
	}

	id, name, err := s.ResolveProject(ctx, realPath)
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "real-id" || name != "infra" {
		t.Errorf("session inside its own project got id=%q name=%q, want real-id/infra — a duplicate that disagrees with the evidence is not a competing answer", id, name)
	}
}

// TestResolveProjectByAmbiguousBareNameReturnsError prevents the save path
// from treating multiple exact-name matches as a miss and auto-creating a
// third project.
func TestResolveProjectByAmbiguousBareNameReturnsError(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.EnsureProject(ctx, "a", "", "infra"); err != nil {
		t.Fatalf("EnsureProject a: %v", err)
	}
	if err := s.EnsureProject(ctx, "b", "", "infra"); err != nil {
		t.Fatalf("EnsureProject b: %v", err)
	}

	_, _, err := s.ResolveProject(ctx, "infra")
	if !errors.Is(err, ErrAmbiguousProject) {
		t.Fatalf("ResolveProject(infra) error = %v, want ErrAmbiguousProject", err)
	}
}

// TestResolveBasenameRequiresUniqueName uses two recorded aliases of one real
// directory so the path and remote rules cannot choose a winner.
func TestResolveBasenameRequiresUniqueName(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	root := t.TempDir()
	realPath := filepath.Join(root, "real", "in")
	if err := os.MkdirAll(realPath, 0o755); err != nil {
		t.Fatalf("mkdir real path: %v", err)
	}
	aliasOne := filepath.Join(root, "alias-one", "in")
	aliasTwo := filepath.Join(root, "alias-two", "in")
	for _, alias := range []string{aliasOne, aliasTwo} {
		if err := os.MkdirAll(filepath.Dir(alias), 0o755); err != nil {
			t.Fatalf("mkdir alias parent: %v", err)
		}
		if err := os.Symlink(realPath, alias); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
	}
	if err := s.EnsureProject(ctx, "one", aliasOne, "in"); err != nil {
		t.Fatalf("EnsureProject one: %v", err)
	}
	if err := s.EnsureProject(ctx, "two", aliasTwo, "in"); err != nil {
		t.Fatalf("EnsureProject two: %v", err)
	}

	_, _, err := s.ResolveProject(ctx, realPath)
	if !errors.Is(err, ErrAmbiguousProject) {
		t.Fatalf("two consistent candidates error = %v, want ErrAmbiguousProject", err)
	}
}

// TestResolveBasenameRefusesRelativePathInput replaces a test that could not
// fail: the old gate test used "/x/infra", which filepath.IsAbs reports as
// absolute on Linux, so reverting the gate to IsAbs left it green and it
// duplicated TestResolveBasenameStillMatchesOwnDirectory. A RELATIVE
// path-shaped input is the shape that separates the two gates, and
// filepath.Base splits it on Linux and Windows alike.
func TestResolveBasenameRefusesRelativePathInput(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	stored := filepath.Join(t.TempDir(), "infra")
	if err := os.MkdirAll(stored, 0o755); err != nil {
		t.Fatalf("mkdir stored path: %v", err)
	}
	if err := s.EnsureProject(ctx, "infra-id", stored, "infra"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	id, name, err := s.ResolveProject(ctx, "some/relative/place/infra")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "" || name != "" {
		t.Errorf("a relative path claimed project %q (%q): it did not prove agreement with the recorded location", id, name)
	}
}

// TestResolveBasenameRefusesRootPathProject: samePath trims the trailing
// slash off a stored "/" and is left comparing against "", so HasPrefix(a,
// "/") accepts EVERY absolute path on the machine. A project recorded at the
// filesystem root would claim any session whose directory shares its name —
// the #546 shape, with a wildcard instead of a basename.
func TestResolveBasenameRefusesRootPathProject(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.EnsureProject(ctx, "root-id", "/", "infra"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	id, name, err := s.ResolveProject(ctx, "/x/infra")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "" || name != "" {
		t.Errorf("a project recorded at the filesystem root claimed %q (%q) — / contains every absolute path", id, name)
	}
}

// TestResolveBasenameRefusesProvenRemoteConflict: when the session directory
// carries a git remote and the candidate project carries a different one,
// both sides have asserted an identity and they disagree. That is a
// contradiction to act on, not a near-match to accept.
func TestResolveBasenameRefusesProvenRemoteConflict(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	root := t.TempDir()
	realPath := filepath.Join(root, "real", "infra")
	aliasPath := filepath.Join(root, "alias", "infra")
	if err := os.MkdirAll(realPath, 0o755); err != nil {
		t.Fatalf("mkdir real path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(aliasPath), 0o755); err != nil {
		t.Fatalf("mkdir alias parent: %v", err)
	}
	if err := os.Symlink(realPath, aliasPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := s.EnsureProjectWithRepo(ctx, "infra-id", aliasPath, "infra", "github.com/me/infra"); err != nil {
		t.Fatalf("EnsureProjectWithRepo: %v", err)
	}
	SetDetectRemote(func(string) string { return "github.com/someone-else/infra" })
	t.Cleanup(func() { SetDetectRemote(nil) })

	id, name, err := s.ResolveProject(ctx, realPath)
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "" || name != "" {
		t.Errorf("a directory in a different repository resolved to project %q (%q)", id, name)
	}
}

// TestResolveBasenameAllowsUnprovenRemote keeps the other half honest: a
// project that never recorded a remote has asserted nothing, so a session
// arriving with one is not a contradiction. Refusing here would strand every
// project created before repository identity existed behind an invisible
// wall — a duplicate project instead of a match, for want of evidence.
func TestResolveBasenameAllowsUnprovenRemote(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	root := t.TempDir()
	realPath := filepath.Join(root, "real", "infra")
	aliasPath := filepath.Join(root, "alias", "infra")
	if err := os.MkdirAll(realPath, 0o755); err != nil {
		t.Fatalf("mkdir real path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(aliasPath), 0o755); err != nil {
		t.Fatalf("mkdir alias parent: %v", err)
	}
	if err := os.Symlink(realPath, aliasPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := s.EnsureProject(ctx, "infra-id", aliasPath, "infra"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	SetDetectRemote(func(string) string { return "github.com/me/infra" })
	t.Cleanup(func() { SetDetectRemote(nil) })

	id, name, err := s.ResolveProject(ctx, realPath)
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "infra-id" || name != "infra" {
		t.Errorf("project with no recorded remote refused a session: id=%q name=%q — nothing was disproved", id, name)
	}
}

// TestResolveProjectByBareNameStillWorks: callers that name a project
// outright — the CLI, ghost_resolve, hooks passing a heading's name — are
// asserting which project they mean, not reporting a directory. A bare name
// carries no location to disagree with, so it must still resolve.
func TestResolveProjectByBareNameStillWorks(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.EnsureProject(ctx, "infra-id", "/home/u/git/infra", "infra"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	id, name, err := s.ResolveProject(ctx, "infra")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "infra-id" || name != "infra" {
		t.Errorf("bare name did not resolve: id=%q name=%q, want infra-id/infra", id, name)
	}
}

// TestResolveBasenameSentinelProjectRefusesPathShapedInput closes the gap
// review caught on #565: the path-agreement guard only ran when the stored
// path contained a separator, and a sentinel path (path == id) never does —
// so every MCP-created project still accepted any directory that merely
// shared its basename, which is issue #546 still reachable. A project that
// recorded no location cannot agree with any directory, so a path-shaped
// input gets no name-only match from it.
func TestResolveBasenameSentinelProjectRefusesPathShapedInput(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// path = id, the sentinel ensureProjectLocked normalizes empty to.
	if err := s.EnsureProject(ctx, "infra-id", "", "infra"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	var path string
	if err := s.db.QueryRowContext(ctx, `SELECT path FROM projects WHERE id = 'infra-id'`).Scan(&path); err != nil {
		t.Fatalf("read path: %v", err)
	}
	if path != "infra-id" {
		t.Fatalf("precondition: path = %q, want the id sentinel %q", path, "infra-id")
	}
	if strings.ContainsAny(path, `/\`) {
		t.Fatalf("precondition: path %q contains a separator, so this is not the sentinel case", path)
	}

	id, name, err := s.ResolveProject(ctx, "/some/other/place/infra")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "" || name != "" {
		t.Errorf("a directory claiming a project that recorded no location resolved to %q (%q) — the #546 scenario is still reachable for sentinel-path projects", id, name)
	}
}

// TestResolveBasenameProjectWithoutRealPath keeps the other half honest: a
// project created over MCP has path == id, a non-absolute value — it has
// never said where it lives. Refusing a directory that merely shares its
// basename (see TestResolveBasenameSentinelProjectRefusesPathShapedInput)
// must not strand it, because a caller holding the name alone — MCP tools,
// the CLI, ghost_resolve — is naming the project, not reporting a location.
func TestResolveBasenameProjectWithoutRealPath(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// path = id, the sentinel ensureProjectLocked normalizes empty to.
	if err := s.EnsureProject(ctx, "infra-id", "", "infra"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	var path string
	if err := s.db.QueryRowContext(ctx, `SELECT path FROM projects WHERE id = 'infra-id'`).Scan(&path); err != nil {
		t.Fatalf("read path: %v", err)
	}
	if path != "infra-id" {
		t.Fatalf("precondition: path = %q, want the id sentinel %q", path, "infra-id")
	}

	// The name, not the id, so this exercises the name-only match rather
	// than the exact-id step above it.
	id, name, err := s.ResolveProject(ctx, "infra")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "infra-id" || name != "infra" {
		t.Errorf("project that recorded no path refused a bare name: id=%q name=%q — every MCP-created project would be unreachable", id, name)
	}
}

// TestResolveRemoteDetectionDoesNotDependOnIsAbs pins the same gap for the
// repository-identity step: it gated on filepath.IsAbs, which is false for a
// drive-relative Windows path (\work\ghost) on every platform, so detection
// was skipped for exactly the shape the path guard was fixed for. Detection
// is gated on the input merely LOOKING like a path — the same separator test
// every other path-shaped step here uses — so a bare name still never spawns
// a process.
func TestResolveRemoteDetectionDoesNotDependOnIsAbs(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const remote = "github.com/me/infra"
	if err := s.EnsureProjectWithRepo(ctx, "infra-id", "/x/infra", "infra", remote); err != nil {
		t.Fatalf("EnsureProjectWithRepo: %v", err)
	}
	SetDetectRemote(func(string) string { return remote })
	t.Cleanup(func() { SetDetectRemote(nil) })

	// Drive-relative, so IsAbs reports false and an IsAbs-gated detector is
	// never asked; the remote is the only thing that can answer, because the
	// path step cannot (stored path is under the LENGTH > 10 guard) and
	// filepath.Base does not split backslashes off-Windows.
	id, name, err := s.ResolveProject(ctx, `\work\ghost`)
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "infra-id" || name != "infra" {
		t.Errorf("repository identity was skipped for a path-shaped input: id=%q name=%q, want infra-id/infra", id, name)
	}
}

// TestStoredPathIsUsable pins the shapes a recorded path must have before a
// session directory can be compared against it, on both platforms' spellings.
// It is unit-level because the relative case cannot be reached from a test on
// Linux: pathsAgree would resolve a relative stored path against the test
// binary's own working directory, so the integration fixture cannot tell a
// cwd-dependent answer from a correct one.
func TestStoredPathIsUsable(t *testing.T) {
	cases := []struct {
		name   string
		stored string
		want   bool
	}{
		{"absolute posix path", "/home/u/git/infra", true},
		{"absolute posix path, trailing slash", "/home/u/git/infra/", true},
		{"single segment below the root", "/x", true},
		{"absolute windows path", `C:\x\infra`, true},
		{"windows path spelled with slashes", "C:/x/infra", true},
		{"windows root only", `C:\`, false},
		{"posix root only", "/", false},
		{"the id sentinel", "infra-id", false},
		{"a bare project name", "infra", false},
		{"relative path with a separator", "sub/infra", false},
		{"relative windows path with a separator", `sub\infra`, false},
		{"drive-relative windows path", `C:infra`, false},
		{"empty", "", false},
	}
	for _, c := range cases {
		if got := storedPathIsUsable(c.stored); got != c.want {
			t.Errorf("%s: storedPathIsUsable(%q) = %v, want %v", c.name, c.stored, got, c.want)
		}
	}
}

// TestIsPathShaped pins the single definition of "the caller is reporting a
// location", shared by the store's path steps, the basename evidence rules
// and mcpserver's repository detection. filepath.IsAbs is the wrong test —
// false for a drive-relative Windows path — and that is what both the Windows
// fixtures in this file and the mcpserver gate depend on.
func TestIsPathShaped(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{"bare project name", "infra", false},
		{"project id", "a7293a04b38a", false},
		{"posix absolute path", "/home/u/git/infra", true},
		{"relative path", "sub/dir", true},
		{"drive-relative windows path", `\work\ghost`, true},
		{"windows absolute path", `C:\work\ghost`, true},
		{"repository url", "github.com/wcatz/ghost", true},
		{"empty", "", false},
	}
	for _, c := range cases {
		if got := IsPathShaped(c.input); got != c.want {
			t.Errorf("%s: IsPathShaped(%q) = %v, want %v", c.name, c.input, got, c.want)
		}
	}
}

// TestPathsAgreeIsSeparatorAgnostic exercises real paths so canonicalization
// is tested on both POSIX and Windows without accepting a nonexistent path.
func TestPathsAgreeIsSeparatorAgnostic(t *testing.T) {
	root := t.TempDir()
	stored := filepath.Join(root, "infra")
	child := filepath.Join(stored, "sub")
	other := filepath.Join(root, "other", "infra")
	for _, dir := range []string{stored, child, other} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	storedSlash := strings.ReplaceAll(stored, string(filepath.Separator), "/")
	childSlash := strings.ReplaceAll(child, string(filepath.Separator), "/")
	cases := []struct {
		name, input, stored string
		want                bool
	}{
		{"child matches stored", child, stored, true},
		{"slash spelling matches", childSlash, storedSlash, true},
		{"different tree", other, stored, false},
		{"segment boundary", filepath.Join(stored+"-other", "sub"), stored, false},
		{"unresolvable", filepath.Join(root, "missing"), stored, false},
	}
	for _, c := range cases {
		if got := pathsAgree(c.input, c.stored); got != c.want {
			t.Errorf("%s: pathsAgree(%q, %q) = %v, want %v", c.name, c.input, c.stored, got, c.want)
		}
	}
}
