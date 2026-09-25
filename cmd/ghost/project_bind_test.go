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
	if !strings.Contains(out.String(), dir) {
		t.Errorf("output should report the bound path, got:\n%s", out.String())
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
}
