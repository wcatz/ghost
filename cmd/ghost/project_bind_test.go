package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// bindStore returns an in-memory store with no projects, so each subtest
// creates exactly the rows it needs.
func bindStore(t *testing.T) *memory.Store {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return memory.NewStore(db, logger)
}

// noRemote is the fake detector for a plain directory: a checkout of nothing
// in particular. It never spawns git.
func noRemote(string) string { return "" }

// physicalDir is the path a bind is expected to record and print: the
// symlink-resolved directory a session would actually be standing in.
func physicalDir(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", dir, err)
	}
	return resolved
}

// TestRunProjectBindCoreResolvesCheckout is the reason the command exists: a
// project upgraded from a v9 database records its bare name as its path, so a
// session standing in the real checkout resolves nothing and gets no
// session-start injection. After binding, the same directory resolves.
func TestRunProjectBindCoreResolvesCheckout(t *testing.T) {
	ctx := context.Background()
	store := bindStore(t)
	if err := store.EnsureProject(ctx, "infra", "", "infrastructure"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	dir := t.TempDir()

	var out bytes.Buffer
	if err := runProjectBindCore(ctx, store, &out, "infra", dir, noRemote); err != nil {
		t.Fatalf("runProjectBindCore: %v", err)
	}
	if id, _, err := store.ResolveProject(ctx, dir); err != nil || id != "infra" {
		t.Fatalf("ResolveProject(%q) = (%q, %v), want the bound project", dir, id, err)
	}
	if !strings.Contains(out.String(), physicalDir(t, dir)) {
		t.Errorf("output should report the bound path, got:\n%s", out.String())
	}
}

// TestRunProjectBindCorePrintsStoredPath — binding through a symlink must print
// the path that was actually stored, not the alias that was typed, or the user
// reads back a path Ghost does not hold.
func TestRunProjectBindCorePrintsStoredPath(t *testing.T) {
	ctx := context.Background()
	store := bindStore(t)
	if err := store.EnsureProject(ctx, "infra", "", "infrastructure"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	var out bytes.Buffer
	if err := runProjectBindCore(ctx, store, &out, "infra", link, noRemote); err != nil {
		t.Fatalf("runProjectBindCore: %v", err)
	}
	if !strings.Contains(out.String(), physicalDir(t, real)) {
		t.Errorf("output should report the stored (physical) path, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), link) {
		t.Errorf("output should not report the alias that was typed, got:\n%s", out.String())
	}
}

// TestParseProjectBindArgs covers the argument contract, including -h/--help:
// a user who cannot remember the syntax types `ghost project bind -h`, and a
// usage line printed to stderr with exit 1 is not an answer to that.
func TestParseProjectBindArgs(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		project  string
		path     string
		help     bool
		wantFail bool
	}{
		{name: "two positionals", args: []string{"infra", "/x/infra"}, project: "infra", path: "/x/infra"},
		{name: "short help", args: []string{"-h"}, help: true},
		{name: "long help", args: []string{"--help"}, help: true},
		{name: "help with a project", args: []string{"infra", "-h"}, help: true},
		{name: "no arguments", args: nil, wantFail: true},
		{name: "one argument", args: []string{"infra"}, wantFail: true},
		{name: "three arguments", args: []string{"a", "b", "c"}, wantFail: true},
		{name: "unknown flag", args: []string{"--apply", "infra", "/x"}, wantFail: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			project, path, help, err := parseProjectBindArgs(tc.args)
			if tc.wantFail {
				if err == nil {
					t.Fatalf("expected an error, got project=%q path=%q help=%v", project, path, help)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if help != tc.help {
				t.Errorf("help = %v, want %v", help, tc.help)
			}
			if project != tc.project || path != tc.path {
				t.Errorf("got project=%q path=%q, want %q / %q", project, path, tc.project, tc.path)
			}
		})
	}
}

// TestRunProjectBindCoreRecordsDetectedRemote — binding a checkout also gives
// the project its repository identity, so a second worktree of the same
// repository resolves to the same project.
func TestRunProjectBindCoreRecordsDetectedRemote(t *testing.T) {
	ctx := context.Background()
	store := bindStore(t)
	if err := store.EnsureProject(ctx, "infra", "", "infrastructure"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	dir := t.TempDir()

	detect := func(got string) string {
		if got != dir {
			t.Errorf("detector asked about %q, want the bind target %q", got, dir)
		}
		return "git@github.com:owner/infra.git"
	}
	var out bytes.Buffer
	if err := runProjectBindCore(ctx, store, &out, "infra", dir, detect); err != nil {
		t.Fatalf("runProjectBindCore: %v", err)
	}
	if !strings.Contains(out.String(), "github.com/owner/infra") {
		t.Errorf("output should report the recorded remote, got:\n%s", out.String())
	}
}

// TestRunProjectBindCoreIdempotent — a user who re-runs the fix from the
// status hint must not have to know whether it already succeeded.
func TestRunProjectBindCoreIdempotent(t *testing.T) {
	ctx := context.Background()
	store := bindStore(t)
	if err := store.EnsureProject(ctx, "infra", "", "infrastructure"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	dir := t.TempDir()

	var first bytes.Buffer
	if err := runProjectBindCore(ctx, store, &first, "infra", dir, noRemote); err != nil {
		t.Fatalf("first bind: %v", err)
	}
	var second bytes.Buffer
	if err := runProjectBindCore(ctx, store, &second, "infra", dir, noRemote); err != nil {
		t.Fatalf("re-binding must succeed, got %v", err)
	}
	if !strings.Contains(second.String(), "already bound") {
		t.Errorf("re-bind should say so, got:\n%s", second.String())
	}
	if !strings.Contains(first.String(), "bound") {
		t.Errorf("first bind output unexpected:\n%s", first.String())
	}
}

// TestRunProjectBindCoreRefusals covers every case that must write nothing and
// exit non-zero: the global bucket, a project that does not exist, a path that
// is not a directory, a path another project already claims, and a repository
// another project already claims.
func TestRunProjectBindCoreRefusals(t *testing.T) {
	ctx := context.Background()
	store := bindStore(t)
	if err := store.SeedGlobalMemories(ctx); err != nil {
		t.Fatalf("SeedGlobalMemories: %v", err)
	}
	for _, id := range []string{"owner", "rival"} {
		if err := store.EnsureProject(ctx, id, "", id); err != nil {
			t.Fatalf("EnsureProject(%q): %v", id, err)
		}
	}

	ownerDir, rivalDir, file := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Two checkouts of one repository: the detector reports the same remote
	// wherever it is asked, which is the situation the remote-claim rule
	// exists for.
	remote := func(string) string { return "git@github.com:owner/shared.git" }
	var out bytes.Buffer
	if err := runProjectBindCore(ctx, store, &out, "owner", ownerDir, remote); err != nil {
		t.Fatalf("seeding the owner project: %v", err)
	}
	out.Reset()

	cases := []struct {
		name     string
		project  string
		path     string
		detect   func(string) string
		wantWant string
	}{
		{"global", "_global", t.TempDir(), noRemote, "_global"},
		{"unknown project", "missing", t.TempDir(), noRemote, "missing"},
		{"nonexistent path", "rival", filepath.Join(t.TempDir(), "absent"), noRemote, "absent"},
		{"path is a file", "rival", file, noRemote, "not-a-dir"},
		{"path claimed by another project", "rival", ownerDir, noRemote, "owner"},
		{"remote claimed by another project", "rival", rivalDir, remote, "github.com/owner/shared"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out.Reset()
			err := runProjectBindCore(ctx, store, &out, tc.project, tc.path, tc.detect)
			if err == nil {
				t.Fatalf("expected a refusal, got none (output:\n%s)", out.String())
			}
			if !strings.Contains(err.Error(), tc.wantWant) {
				t.Errorf("error %q should mention %q", err, tc.wantWant)
			}
			if out.Len() != 0 {
				t.Errorf("a refusal must not print a success report, got:\n%s", out.String())
			}
		})
	}

	// Nothing above was allowed to move the rival project's sentinel path.
	projects, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	for _, p := range projects {
		if p.ID == "rival" && p.Path != "rival" {
			t.Errorf("a refused bind wrote path %q on the rival project", p.Path)
		}
	}
}

// TestRunProjectBindCoreMakesPathAbsolute — a user who types a relative path
// from inside the checkout must get the same binding as one who types the
// absolute form, and a path Ghost could not compare a session directory
// against (the filesystem root, which contains every directory on the machine)
// is refused rather than stored.
func TestRunProjectBindCoreMakesPathAbsolute(t *testing.T) {
	ctx := context.Background()
	store := bindStore(t)
	if err := store.EnsureProject(ctx, "infra", "", "infrastructure"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	dir := t.TempDir()
	// A relative path is resolved against the process's working directory,
	// so chdir into the checkout and name a subdirectory of it.
	if err := os.Mkdir(filepath.Join(dir, "inner"), 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	t.Chdir(dir)

	var out bytes.Buffer
	if err := runProjectBindCore(ctx, store, &out, "infra", "inner", noRemote); err != nil {
		t.Fatalf("runProjectBindCore: %v", err)
	}
	if id, _, err := store.ResolveProject(ctx, filepath.Join(dir, "inner")); err != nil || id != "infra" {
		t.Fatalf("ResolveProject of the absolute form = (%q, %v), want the bound project", id, err)
	}

	out.Reset()
	if err := runProjectBindCore(ctx, store, &out, "infra", string(filepath.Separator), noRemote); err == nil {
		t.Error("binding the filesystem root must be refused")
	}
}

// TestOpenDiagnosticStoreDoesNotCreateTheDatabase — `ghost mcp status` reports
// on the store, so it must not be the command that brings one into being. A
// bootstrap() here would create ghost.db, stamp its schema version and seed the
// builtin rows, and the next status run would report a healthy database where
// it should report none — the check invalidating its own result.
func TestOpenDiagnosticStoreDoesNotCreateTheDatabase(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	dbPath := filepath.Join(dataHome, "ghost", "ghost.db")

	if store := openDiagnosticStore(); store != nil {
		store.Close() //nolint:errcheck
		t.Fatal("a diagnostic read opened a store that did not exist")
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("the diagnostic read created %s", dbPath)
	}

	// With a real database present it does read, and it reports the projects.
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	rw, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if err := memory.NewStore(rw, nil).EnsureProject(t.Context(), "infra", "", "infrastructure"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if err := rw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	store := openDiagnosticStore()
	if store == nil {
		t.Fatal("a diagnostic read refused an existing database")
	}
	defer store.Close() //nolint:errcheck

	var out bytes.Buffer
	if err := writeUnboundProjectNotice(t.Context(), &out, store); err != nil {
		t.Fatalf("writeUnboundProjectNotice: %v", err)
	}
	if !strings.Contains(out.String(), "ghost project bind infra /path/to/checkout") {
		t.Errorf("notice should list the unbound project, got:\n%s", out.String())
	}
}

// TestUnboundProjectNotice covers the `ghost mcp status` line: it must name
// every project a session directory cannot resolve, print the exact fix
// command, and stay silent when there is nothing to fix.
func TestUnboundProjectNotice(t *testing.T) {
	ctx := context.Background()
	store := bindStore(t)
	if err := store.SeedGlobalMemories(ctx); err != nil {
		t.Fatalf("SeedGlobalMemories: %v", err)
	}

	var out bytes.Buffer
	if err := writeUnboundProjectNotice(ctx, &out, store); err != nil {
		t.Fatalf("writeUnboundProjectNotice: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("no projects, no notice, got:\n%s", out.String())
	}

	for _, id := range []string{"infra", "ledger"} {
		if err := store.EnsureProject(ctx, id, "", id+"-name"); err != nil {
			t.Fatalf("EnsureProject(%q): %v", id, err)
		}
	}
	out.Reset()
	if err := writeUnboundProjectNotice(ctx, &out, store); err != nil {
		t.Fatalf("writeUnboundProjectNotice: %v", err)
	}
	got := out.String()
	for _, id := range []string{"infra", "ledger"} {
		if !strings.Contains(got, "ghost project bind "+id+" /path/to/checkout") {
			t.Errorf("notice should carry the fix command for %q, got:\n%s", id, got)
		}
	}
	if strings.Contains(got, "_global") {
		t.Errorf("the global bucket is not an unbound project, got:\n%s", got)
	}

	// Once bound, the project stops being reported.
	if err := runProjectBindCore(ctx, store, &out, "infra", t.TempDir(), noRemote); err != nil {
		t.Fatalf("bind infra: %v", err)
	}
	out.Reset()
	if err := writeUnboundProjectNotice(ctx, &out, store); err != nil {
		t.Fatalf("writeUnboundProjectNotice: %v", err)
	}
	after := out.String()
	if strings.Contains(after, "ghost project bind infra") {
		t.Errorf("a bound project must not be reported, got:\n%s", after)
	}
	if !strings.Contains(after, "ghost project bind ledger") {
		t.Errorf("the still-unbound project must remain, got:\n%s", after)
	}
	// The limit of the check is stated, not implied: a project bound to a
	// checkout that has since moved is not detectable from a stored path, and a
	// user who hits that needs to know the status line will not mention it.
	if !strings.Contains(after, "no longer exists") {
		t.Errorf("the notice should state what it cannot detect, got:\n%s", after)
	}
}
