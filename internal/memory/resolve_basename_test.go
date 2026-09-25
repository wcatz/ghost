package memory

import (
	"context"
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
	// guard skips it and this input's basename is the subdirectory's name.
	// That is a separate, pre-existing gap and not what this test is about —
	// widening prefix matching to cover it is a different change.
}

// TestResolveBasenameRequiresUniqueName: projects.name carries no uniqueness
// constraint, and the fallback used to take LIMIT 1 — an arbitrary pick
// whenever two projects shared a name, with no way for the caller to know
// which one it got.
func TestResolveBasenameRequiresUniqueName(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Both created over MCP, so path = id: a non-absolute value. That
	// matters for what this test proves — with an absolute path on either
	// candidate the path guard would refuse the session first and the
	// uniqueness rule would never run, leaving it untested.
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

	id, name, err := s.ResolveProject(ctx, "/cc/infra")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "" || name != "" {
		t.Errorf("ambiguous name resolved to %q (%q) — with two candidates there is no correct answer, only a guess", id, name)
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

// TestResolveBasenameProjectWithoutRealPath: a project created over MCP has
// path == id, a non-absolute value — it has never said where it lives, so
// there is nothing for a directory to contradict. It must keep matching on
// name alone, or every MCP-created project becomes unreachable.
func TestResolveBasenameProjectWithoutRealPath(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// path = id, the sentinel ensureProjectLocked normalizes empty to.
	if err := s.EnsureProject(ctx, "infra", "", "infra"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	var path string
	if err := s.db.QueryRowContext(ctx, `SELECT path FROM projects WHERE id = 'infra'`).Scan(&path); err != nil {
		t.Fatalf("read path: %v", err)
	}
	if path != "infra" {
		t.Fatalf("precondition: path = %q, want the id sentinel %q", path, "infra")
	}

	id, name, err := s.ResolveProject(ctx, "/some/other/place/infra")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "infra" || name != "infra" {
		t.Errorf("project that recorded no path refused a session: id=%q name=%q — there was nothing to contradict", id, name)
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

// TestResolveBasenameGuardDoesNotDependOnIsAbs guards the other half of the
// same defect: the guard is gated on the input merely LOOKING like a path,
// the same test the path-prefix step uses, because filepath.IsAbs is false
// for drive-relative Windows paths and would have disabled it entirely.
func TestResolveBasenameGuardDoesNotDependOnIsAbs(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Stored with a separator, so the guard applies.
	if err := s.EnsureProject(ctx, "infra-id", "/x/infra", "infra"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	// On Linux filepath.IsAbs("/x/infra") is true, so this asserts the
	// positive path holds with the gate in place rather than being skipped.
	id, _, err := s.ResolveProject(ctx, "/x/infra")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "infra-id" {
		t.Errorf("own directory stopped resolving: id=%q, want infra-id", id)
	}
}
