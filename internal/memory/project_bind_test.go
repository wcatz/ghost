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

// TestBindProjectPathRefusalsNameTheProject is the #608 shape: a project made
// over MCP is stored under a derived id, so a refusal that names the id alone
// tells the reader nothing they can act on. The name is what they called the
// project and what `ghost project merge` and `ghost project bind` take, so all
// three refusals carry it.
//
// One test for all three because the name has to reach every branch of the same
// switch: a fix on the branch the reporter happened to hit is not a fix.
func TestBindProjectPathRefusalsNameTheProject(t *testing.T) {
	// The id is a hash and the name is not, exactly as a project created over
	// MCP is stored — so a refusal naming only the id fails here.
	const (
		innerID   = "6bdc098af7f5"
		innerName = "roller"
	)

	t.Run("contains another project's checkout", func(t *testing.T) {
		ctx := context.Background()
		s := bindStore(t)
		sentinelProject(t, s, innerID, innerName)
		sentinelProject(t, s, "outer", "outer")
		parent := t.TempDir()
		inner := filepath.Join(parent, "checkout")
		if err := os.Mkdir(inner, 0o700); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		if _, err := s.BindProjectPath(ctx, innerID, inner, ""); err != nil {
			t.Fatalf("binding the inner project: %v", err)
		}
		_, err := s.BindProjectPath(ctx, "outer", parent, "")
		if !errors.Is(err, ErrBindPathContainsOther) {
			t.Fatalf("err = %v, want ErrBindPathContainsOther", err)
		}
		if !strings.Contains(err.Error(), innerName) {
			t.Errorf("refusal = %q, want it to name the project by its name %q", err, innerName)
		}
	})

	t.Run("path already records", func(t *testing.T) {
		ctx := context.Background()
		s := bindStore(t)
		sentinelProject(t, s, innerID, innerName)
		sentinelProject(t, s, "second", "second")
		real := t.TempDir()
		if _, err := s.BindProjectPath(ctx, innerID, real, ""); err != nil {
			t.Fatalf("binding the first project: %v", err)
		}
		_, err := s.BindProjectPath(ctx, "second", real, "")
		if !errors.Is(err, ErrBindPathClaimed) {
			t.Fatalf("err = %v, want ErrBindPathClaimed", err)
		}
		if !strings.Contains(err.Error(), innerName) {
			t.Errorf("refusal = %q, want it to name the project by its name %q", err, innerName)
		}
	})

	t.Run("path is inside a project with no remote", func(t *testing.T) {
		ctx := context.Background()
		s := bindStore(t)
		sentinelProject(t, s, innerID, innerName)
		sentinelProject(t, s, "outer", "outer")
		outer := t.TempDir()
		nested := filepath.Join(outer, "vendored")
		if err := os.Mkdir(nested, 0o700); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		if _, err := s.BindProjectPath(ctx, innerID, outer, ""); err != nil {
			t.Fatalf("binding the outer project: %v", err)
		}
		_, err := s.BindProjectPath(ctx, "outer", nested, "")
		if !errors.Is(err, ErrBindPathInsideOther) {
			t.Fatalf("err = %v, want ErrBindPathInsideOther", err)
		}
		if !strings.Contains(err.Error(), innerName) {
			t.Errorf("refusal = %q, want it to name the project by its name %q", err, innerName)
		}
	})
}

// shortExistingDir returns an existing directory whose resolved path is ten
// characters or fewer, or skips: the temp root of a CI host is usually deeper,
// so a short path has to come from the system directories.
func shortExistingDir(t *testing.T) string {
	t.Helper()
	for _, c := range []string{"/etc", "/usr/bin", "/usr/lib", "/var", "/opt", "/srv", "/dev/shm", "/usr"} {
		resolved, err := filepath.EvalSymlinks(c)
		if err != nil {
			continue
		}
		if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
			continue
		}
		if pathRankLength(resolved) <= 10 {
			return resolved
		}
	}
	t.Skip("no existing directory with a path of ten characters or fewer on this host")
	return ""
}

// TestBindAndLookupTreatAShortPathTheSame — resolution used to ignore recorded
// paths of ten characters or fewer through a textual length filter while bind
// refused to record them, and after the resolver moved to resolved form it
// accepted them, so a row made another way (import, restore) routed callers
// that bind would never have created. Both now ask storedPathIsUsable and
// compare in resolved form: a short directory is bindable, and lookup finds
// what was bound, so neither side disagrees with the other.
func TestBindAndLookupTreatAShortPathTheSame(t *testing.T) {
	ctx := context.Background()
	dir := shortExistingDir(t)

	// A row written by SQL, as an import would: lookup routes it.
	imported := bindStore(t)
	if _, err := imported.db.ExecContext(ctx,
		`INSERT INTO projects (id, path, name) VALUES ('short', ?, 'short')`, dir); err != nil {
		t.Fatalf("insert short row: %v", err)
	}
	if id, _, err := imported.ResolveProject(ctx, dir); err != nil || id != "short" {
		t.Fatalf("lookup of an imported short path = (%q, %v), want short", id, err)
	}

	// The same path through bind: accepted, and lookup finds it.
	bound := bindStore(t)
	sentinelProject(t, bound, "short", "short")
	if _, err := bound.BindProjectPath(ctx, "short", dir, ""); err != nil {
		t.Fatalf("bind refused a short path the resolver accepts: %v", err)
	}
	if id, _, err := bound.ResolveProject(ctx, dir); err != nil || id != "short" {
		t.Fatalf("lookup of a bound short path = (%q, %v), want short", id, err)
	}
}

// TestBindRefusesASecondSpellingAndLookupStaysUnambiguous — a symlinked parent
// gives one recorded directory a second spelling. Resolution compares in
// resolved form, so a row recorded under that spelling would tie with the
// first and every caller beneath it would get the ambiguity error. Bind
// applies the same comparison and refuses, so lookup never reaches
// ErrAmbiguousProject through a bind.
func TestBindRefusesASecondSpellingAndLookupStaysUnambiguous(t *testing.T) {
	ctx := context.Background()
	s := bindStore(t)
	sentinelProject(t, s, "first", "first")
	sentinelProject(t, s, "second", "second")

	realParent := t.TempDir()
	real := filepath.Join(realParent, "checkout")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	linkParent := symlinkTo(t, realParent)
	second := filepath.Join(linkParent, "checkout")

	if _, err := s.BindProjectPath(ctx, "first", real, ""); err != nil {
		t.Fatalf("binding the first project: %v", err)
	}
	_, err := s.BindProjectPath(ctx, "second", second, "")
	if !errors.Is(err, ErrBindPathClaimed) {
		t.Fatalf("err = %v, want ErrBindPathClaimed for a second spelling of a recorded directory", err)
	}
	if !strings.Contains(err.Error(), "first") {
		t.Errorf("the refusal should name the project that records the directory, got %q", err)
	}
	if got := projectPath(t, s, "second"); got != "second" {
		t.Errorf("the refused bind wrote path %q", got)
	}
	for _, from := range []string{real, second} {
		id, _, err := s.ResolveProject(ctx, from)
		if errors.Is(err, ErrAmbiguousProject) {
			t.Fatalf("lookup from %q reached the ambiguity error after a refused bind: %v", from, err)
		}
		if err != nil || id != "first" {
			t.Errorf("lookup from %q = (%q, %v), want first", from, id, err)
		}
	}
}

// TestCheckPathResolvableRefusesAResolvedTie — the check bind and
// BindNewProjectToCheckout run on the written row ranks in resolved form, so
// two rows that are two spellings of one directory tie even when some other
// writer put them there, and the project that would lose the tie is refused.
func TestCheckPathResolvableRefusesAResolvedTie(t *testing.T) {
	ctx := context.Background()
	s := bindStore(t)
	realParent := t.TempDir()
	real := filepath.Join(realParent, "checkout")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(symlinkTo(t, realParent), "checkout")
	if err := s.EnsureProject(ctx, "first", real, "first"); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureProject(ctx, "second", second, "second"); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck
	err = checkPathResolvable(ctx, tx, "second", physical(t, real), "")
	if !errors.Is(err, ErrBindPathUnmatchable) || !strings.Contains(err.Error(), "ties") {
		t.Fatalf("err = %v, want a tie refusal for two spellings of one directory", err)
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

// TestBindProjectPathRefusesAPathTheImporterWouldRefuse is #840.
//
// #824 refused an unexportable project at both CREATION routes, on the importer's
// own predicate. `projects.path` has exactly one writer — the UPDATE inside
// BindProjectPath — and it had no such rule, so bind was a way into the same state
// through an update: record a checkout called `«ghost»`, and `ghost export` then
// refuses that project, and with it every memory, task and decision beneath it,
// under a named non-zero exit. Nothing on the way in had said the directory was
// ineligible.
//
// Every leaf below is a legal POSIX directory name, which is the point: «, » and a
// backtick all survive `os.Mkdir`, `storedPathIsUsable` asks only whether the path
// is absolute and is not a bare root, and a checkout inside a directory whose owner
// likes «» is an ordinary thing to have on a disk. The credential case is the same
// hole with a token in it, and it is asked first so a path that is BOTH comes back
// as the answer an operator can act on.
func TestBindProjectPathRefusesAPathTheImporterWouldRefuse(t *testing.T) {
	for _, tc := range []struct {
		name, leaf string
		// wantFormat is the guard's finding for the credential cases, empty for the
		// shape ones. Asserted only where the guard is the rule that fires, so the
		// test states which of the two sentences is the one being read.
		wantFormat string
		wantRule   string
	}{
		{name: "«data delimiter»", leaf: "«ghost»", wantRule: "project path must hold no control character"},
		{name: "a closing delimiter", leaf: "checkout»", wantRule: "project path must hold no control character"},
		{name: "a backtick", leaf: "ghost`checkout", wantRule: "project path must hold no control character"},
		{name: "a control character", leaf: "ghost\ncheckout", wantRule: "project path must hold no control character"},
		{
			name: "a credential", leaf: credentialToken,
			wantFormat: "GitHub personal access token",
			wantRule:   "refusing to store credential-shaped content",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := bindStore(t)
			sentinelProject(t, s, "infra", "infrastructure")
			dir := filepath.Join(t.TempDir(), tc.leaf)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Skipf("this filesystem will not hold %q in a directory name: %v", tc.leaf, err)
			}

			_, err := s.BindProjectPath(ctx, "infra", dir, "")

			if !errors.Is(err, ErrBindPathUnprintable) {
				t.Fatalf("err = %v, want ErrBindPathUnprintable", err)
			}
			if got := projectPath(t, s, "infra"); got != "infra" {
				t.Errorf("the refused bind wrote path %q, want the id sentinel unchanged", got)
			}
			// The refusal is the IMPORTER's sentence, verbatim, because the operator
			// who hits it is the one running `ghost export` and the two explanations
			// of one decision have to be the same explanation. It carries no value,
			// so the directory is not in it either — which is the property the CLI
			// boundary adds its own named rendering beside, and this asserts the half
			// that makes that necessary.
			if !strings.Contains(err.Error(), tc.wantRule) {
				t.Errorf("refusal does not carry the importer's own sentence %q: %v", tc.wantRule, err)
			}
			if strings.Contains(err.Error(), tc.leaf) {
				t.Errorf("the refusal quoted the directory it refused: %v", err)
			}
			if tc.wantFormat != "" && !strings.Contains(err.Error(), tc.wantFormat) {
				t.Errorf("refusal names the shape rule rather than the %s it found: %v", tc.wantFormat, err)
			}
		})
	}
}

// TestABoundProjectWithAnUnprintablePathStaysUsable is the other half of #840, and
// the half that decides whether the rule above is a fix or a regression.
//
// A project already carrying a path `ghost export` refuses — planted before the
// guard existed, restored from an artifact, written by an older Ghost — must keep
// working, and above all must stay REPAIRABLE. Judging the stored value at bind
// time would refuse the one command that gets a user out of the state, and would do
// it on the project the rule exists to protect. So: the planted path resolves, a
// save into it lands, and binding a clean directory rewrites the column.
//
// The project is planted with SQL rather than through a bind because that is how
// such a row actually arrives, and because the test would otherwise be testing the
// guard with the guard.
func TestABoundProjectWithAnUnprintablePathStaysUsable(t *testing.T) {
	ctx := context.Background()
	s := bindStore(t)
	hostile := filepath.Join(t.TempDir(), "«ghost»")
	if err := os.MkdirAll(hostile, 0o755); err != nil {
		t.Skipf("this filesystem will not hold a data delimiter in a directory name: %v", err)
	}
	stored := physical(t, hostile)
	plantRawProject(t, s, "infra", stored, "infrastructure")

	// The state the guard protects against, asserted directly: the exporter
	// refuses this project today, which is why the bind has to stop producing one.
	if err := CheckImportedProject(PortableProject{ID: "infra", Name: "infrastructure", Path: stored}); err == nil {
		t.Fatalf("the fixture is supposed to be a project `ghost export` refuses; it now passes")
	}

	id, _, err := s.ResolveProject(ctx, hostile)
	if err != nil {
		t.Fatalf("resolving a session directory inside an unexportable project: %v", err)
	}
	if id != "infra" {
		t.Fatalf("resolved %q, want the project that records the path", id)
	}
	if _, _, _, err := s.Upsert(ctx, id, "convention", "the path column is judged on write, not on read", "mcp", 0.5, nil); err != nil {
		t.Errorf("a save into an unexportable project is now refused: %v", err)
	}

	clean := t.TempDir()
	binding, err := s.BindProjectPath(ctx, "infra", clean, "")
	if err != nil {
		t.Fatalf("rebinding to a clean directory is the documented remedy and must not be refused: %v", err)
	}
	if !binding.PathChanged {
		t.Errorf("the rebind reported no path change: %+v", binding)
	}
	if got, want := projectPath(t, s, "infra"), physical(t, clean); got != want {
		t.Errorf("path = %q, want the clean directory %q", got, want)
	}
	if err := CheckImportedProject(PortableProject{ID: "infra", Name: "infrastructure", Path: projectPath(t, s, "infra")}); err != nil {
		t.Errorf("the project is still unexportable after the rebind: %v", err)
	}
}
