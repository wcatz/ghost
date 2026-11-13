package main

import (
	"bytes"
	"context"
	"errors"
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

// failingWriter fails the nth Write and succeeds before it, so a test can aim
// at one specific line of a multi-write report.
type failingWriter struct {
	failOn int
	writes int
}

func (w *failingWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == w.failOn {
		return 0, errors.New("write failed")
	}
	return len(p), nil
}

// TestWriteUnboundProjectNoticeReportsWriteFailure — the caveat line is the
// part of the notice that tells the user what it cannot see, so a truncated
// notice is worse than a missing one: the caller has to be able to tell a
// complete report from a partial one, which means every write's error is
// returned, including the last.
func TestWriteUnboundProjectNoticeReportsWriteFailure(t *testing.T) {
	ctx := context.Background()
	store := bindStore(t)
	for _, id := range []string{"infra", "ledger"} {
		if err := store.EnsureProject(ctx, id, "", id); err != nil {
			t.Fatalf("EnsureProject(%q): %v", id, err)
		}
	}

	// One write per line: the header, one per project, then the two closing
	// lines (the limit of the check, and the follow-up command).
	for failOn := 1; failOn <= 5; failOn++ {
		out := &failingWriter{failOn: failOn}
		if err := writeUnboundProjectNotice(ctx, out, store); err == nil {
			t.Errorf("write %d failed but writeUnboundProjectNotice returned nil", failOn)
		}
	}
}

// TestOpenDiagnosticStoreDoesNotCreateTheDatabase — `ghost mcp status` reports
// on the store, so it must not be the command that brings one into being. A
// bootstrap() here would create ghost.db, stamp its schema version and seed the
// builtin rows, and the next status run would report a healthy database where
// it should report none — the check invalidating its own result. The data
// directory counts too: a helper that promises to create nothing must not leave
// an empty ~/.local/share/ghost behind either.
func TestOpenDiagnosticStoreDoesNotCreateTheDatabase(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	dataDir := filepath.Join(dataHome, "ghost")
	dbPath := filepath.Join(dataDir, "ghost.db")

	if store := openDiagnosticStore(); store != nil {
		store.Close() //nolint:errcheck
		t.Fatal("a diagnostic read opened a store that did not exist")
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("the diagnostic read created %s", dbPath)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("the diagnostic read created the data directory %s", dataDir)
	}

	// With a real database present it does read, and it reports the projects.
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
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

// TestRunProjectBindCoreNamesTheDirectoryItRefused is #840 at the boundary.
//
// The store's refusal for an unprintable path is deliberately the exporter's own
// sentence, which carries no value — correct for `ghost export`, where the caller's
// text is noise, and wrong for a person who just typed a directory and needs to be
// told which one was refused. So the command names it, through `assemble.Label`,
// and this holds both halves of that: the directory IS named (escaped: a
// guillemet is replaced, and a backtick is shown with a backslash in front of it)
// and nothing else is.
//
// The credential case is the reason the naming is skipped there, and it is the half
// that is easy to get wrong in the other direction: `assemble.Label` renders a value
// for a line, it does not withhold a secret, so naming the path beside
// `refusing to store credential-shaped content in path: …` would put the token back
// into the one answer that says Ghost never stores credential values. That refusal
// must come back naming the field and the format and nothing else.
func TestRunProjectBindCoreNamesTheDirectoryItRefused(t *testing.T) {
	const token = "ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, tc := range []struct {
		name, leaf string
		// wantNamed is the escaped leaf the refusal has to carry; empty means it
		// must not name the directory at all.
		wantNamed string
		wantRule  string
	}{
		{name: "a data delimiter", leaf: "«ghost»", wantNamed: `ghost`, wantRule: "must hold no control character"},
		{name: "a backtick", leaf: "ghost`checkout", wantNamed: "ghost\\`checkout", wantRule: "must hold no control character"},
		{name: "a credential", leaf: token, wantRule: "GitHub personal access token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := bindStore(t)
			if err := store.EnsureProject(ctx, "infra", "", "infrastructure"); err != nil {
				t.Fatalf("EnsureProject: %v", err)
			}
			dir := filepath.Join(t.TempDir(), tc.leaf)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Skipf("this filesystem will not hold %q in a directory name: %v", tc.leaf, err)
			}

			var out bytes.Buffer
			err := runProjectBindCore(ctx, store, &out, "infra", dir, noRemote)

			if !errors.Is(err, memory.ErrBindPathUnprintable) {
				t.Fatalf("err = %v, want ErrBindPathUnprintable", err)
			}
			if !strings.Contains(err.Error(), tc.wantRule) {
				t.Errorf("refusal %q does not carry the rule that fired (%q)", err, tc.wantRule)
			}
			if out.Len() != 0 {
				t.Errorf("a refusal must not print a success report, got:\n%s", out.String())
			}
			// The directory is named through the safe renderer, so the escaped leaf
			// is there and none of the refused characters are.
			if tc.wantNamed != "" {
				if !strings.Contains(err.Error(), tc.wantNamed) {
					t.Errorf("refusal %q does not name the directory it refused", err)
				}
				if strings.Contains(err.Error(), tc.leaf) {
					t.Errorf("refusal %q quoted a directory it is refusing", err)
				}
			}
			// And whatever happened, the token is not in the answer and the column
			// did not move.
			if strings.Contains(err.Error(), token) {
				t.Errorf("refusal %q echoed the credential", err)
			}
			projects, listErr := store.ListProjects(ctx)
			if listErr != nil {
				t.Fatalf("ListProjects: %v", listErr)
			}
			for _, p := range projects {
				if p.ID == "infra" && p.Path != "infra" {
					t.Errorf("a refused bind wrote path %q", p.Path)
				}
			}
		})
	}
}
