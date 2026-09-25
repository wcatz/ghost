package memory

import (
	"context"
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

	if err := s.EnsureProject(ctx, "infra-id", "/x/infra", "infra"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	id, name, err := s.ResolveProject(ctx, "/x/infra")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "infra-id" || name != "infra" {
		t.Errorf("session inside its own project did not resolve: id=%q name=%q, want infra-id/infra", id, name)
	}

	// And nothing more: a session in a SUBdirectory of a short-path project
	// does not resolve either, because the prefix step's LENGTH(path) > 10
	// guard skips it and this input's basename is the subdirectory's name,
	// which names no project. That is a separate, pre-existing gap and not
	// what this test is about — widening prefix matching to cover it is a
	// different change.
	if id, name, err := s.ResolveProject(ctx, "/x/infra/sub"); err != nil {
		t.Fatalf("ResolveProject subdir: %v", err)
	} else if id != "" || name != "" {
		t.Errorf("subdirectory of a short-path project resolved to %q (%q), want no match", id, name)
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

	// Created first, and with a path this short so the SQL path step cannot
	// answer (LENGTH(path) > 10) and the basename fallback is what answers.
	if err := s.EnsureProject(ctx, "real-id", "/x/infra", "infra"); err != nil {
		t.Fatalf("EnsureProject real: %v", err)
	}
	// The duplicate is created second on purpose: an absolute-path
	// EnsureProject auto-merges a same-name project with a non-absolute path
	// (store.go), which would delete the row this test needs to exist.
	if err := s.EnsureProject(ctx, "dup-id", "", "infra"); err != nil {
		t.Fatalf("EnsureProject duplicate: %v", err)
	}
	var dupPath string
	if err := s.db.QueryRowContext(ctx, `SELECT path FROM projects WHERE id = 'dup-id'`).Scan(&dupPath); err != nil {
		t.Fatalf("read duplicate path: %v", err)
	}
	if dupPath != "dup-id" {
		t.Fatalf("precondition: duplicate path = %q, want the id sentinel", dupPath)
	}

	id, name, err := s.ResolveProject(ctx, "/x/infra")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "real-id" || name != "infra" {
		t.Errorf("session inside its own project got id=%q name=%q, want real-id/infra — a duplicate that disagrees with the evidence is not a competing answer", id, name)
	}
}

// TestResolveProjectByAmbiguousBareNameReturnsNoMatch: rule 1 has to hold
// where MCP project_id actually enters. Every MCP tool passes a NAME, and the
// exact-name step took LIMIT 1, so a duplicated name handed the caller one
// arbitrary row — and every save it made landed there. Naming a project is
// not a location report, so the evidence rules have nothing to filter on:
// two rows means there is no answer.
func TestResolveProjectByAmbiguousBareNameReturnsNoMatch(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.EnsureProject(ctx, "a", "", "infra"); err != nil {
		t.Fatalf("EnsureProject a: %v", err)
	}
	if err := s.EnsureProject(ctx, "b", "", "infra"); err != nil {
		t.Fatalf("EnsureProject b: %v", err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE name = 'infra'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("precondition: %d projects named infra, want 2", n)
	}

	id, name, err := s.ResolveProject(ctx, "infra")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "" || name != "" {
		t.Errorf("ambiguous name resolved to %q (%q) — with two candidates the caller cannot know which project it got", id, name)
	}
}

// TestResolveBasenameRequiresUniqueName: projects.name carries no uniqueness
// constraint, and the fallback used to take LIMIT 1 — an arbitrary pick
// whenever two projects shared a name, with no way for the caller to know
// which one it got.
//
// The fixture is built so the uniqueness rule is the ONLY thing that can
// decide it. Two earlier fixtures could not: with two sentinel candidates
// the path rules reject both (0 survivors), and with one sentinel plus one
// matching candidate the sentinel is rejected too (1 survivor), so deleting
// the uniqueness check left both tests green.
//
// Two distinct recorded paths that both agree with the session is the only
// state that isolates it, and pathsAgree is deliberately spelling-tolerant,
// so two spellings of one directory qualify: "/a/in" and "/a/in/" both hold
// this session. Both paths are short, so the SQL path step cannot answer
// (LENGTH(path) > 10) and the basename fallback is what runs.
func TestResolveBasenameRequiresUniqueName(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.EnsureProject(ctx, "one", "/a/in", "in"); err != nil {
		t.Fatalf("EnsureProject one: %v", err)
	}
	if err := s.EnsureProject(ctx, "two", "/a/in/", "in"); err != nil {
		t.Fatalf("EnsureProject two: %v", err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE name = 'in'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("precondition: %d projects named in, want 2", n)
	}

	id, name, err := s.ResolveProject(ctx, "/a/in")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "" || name != "" {
		t.Errorf("two consistent candidates resolved to %q (%q) — with two survivors there is no correct answer, only a guess", id, name)
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

	// Short path, so the SQL path step cannot answer and the basename
	// fallback is the only thing that can.
	if err := s.EnsureProject(ctx, "infra-id", "/x/infra", "infra"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	id, name, err := s.ResolveProject(ctx, "some/relative/place/infra")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "" || name != "" {
		t.Errorf("a relative path claimed project %q (%q): IsAbs is false for this shape, so an IsAbs-gated guard would be off", id, name)
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

	// Short path, and the session stands exactly on it: the prefix step
	// cannot answer (LENGTH > 10) and the path guard agrees, so the remote
	// rule is the only thing left that can refuse. An earlier revision of
	// this test used a long path with the session elsewhere, and disabling
	// the remote guard still passed — the path guard was answering for it.
	if err := s.EnsureProjectWithRepo(ctx, "infra-id", "/x/infra", "infra", "github.com/me/infra"); err != nil {
		t.Fatalf("EnsureProjectWithRepo: %v", err)
	}
	SetDetectRemote(func(dir string) string { return "github.com/someone-else/infra" })
	t.Cleanup(func() { SetDetectRemote(nil) })

	id, name, err := s.ResolveProject(ctx, "/x/infra")
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

	if err := s.EnsureProject(ctx, "infra-id", "/x/infra", "infra"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	SetDetectRemote(func(dir string) string { return "github.com/me/infra" })
	t.Cleanup(func() { SetDetectRemote(nil) })

	// Stand in the project's own directory — short, so the prefix step skips
	// it and the basename fallback is what answers. Only the remote rule is
	// under test here; putting the session somewhere else would let the path
	// rule reject it first and prove nothing about remotes.
	id, name, err := s.ResolveProject(ctx, "/x/infra")
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

// TestPathsAgreeIsSeparatorAgnostic pins the exact defect review caught on
// #565: the guard compared with strings.HasPrefix(input, path+"/"), which can
// never match a native Windows path. Windows stores backslashes, and — worse —
// filepath.IsAbs reports false for a drive-relative path such as
// \some\unrelated\ghost, so the whole guard silently switched itself off on
// the platform where an unrelated directory is most likely to share a basename.
//
// These are unit-level because filepath.Base does not split on backslashes
// when the tests run on Linux, so the integration fixtures above cannot reach
// this comparison with a Windows-shaped path. The normalization being tested
// here is what makes the guard correct on Windows.
func TestPathsAgreeIsSeparatorAgnostic(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		stored string
		want   bool
	}{
		{"session inside the project, backslash input", `C:\x\infra\sub`, `C:\x\infra`, true},
		{"session inside the project, mixed separators", `C:\x\infra\sub`, `C:/x/infra`, true},
		{"stored backslash, input forward slash", `C:/x/infra/sub`, `C:\x\infra`, true},
		{"exact match either way", `C:\x\infra`, `C:/x/infra`, true},
		{"a different tree with the same basename", `C:\y\infra`, `C:\x\infra`, false},
		{"forward slash equivalent of the attack", `/some/unrelated/path/infra`, `/x/infra`, false},
		{"segment boundary is respected", `/x/infra-other`, `/x/infra`, false},
		{"segment boundary respected from either side", `/x/infra`, `/x/infra-other`, false},
		{"a path that does not exist resolves to nothing", `/definitely/not/here/a`, `/definitely/not/here/b`, false},
	}
	for _, c := range cases {
		if got := pathsAgree(c.input, c.stored); got != c.want {
			t.Errorf("%s: pathsAgree(%q, %q) = %v, want %v", c.name, c.input, c.stored, got, c.want)
		}
	}
}
