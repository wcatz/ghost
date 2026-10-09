package mcpserver

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/repo"
)

// physTemp is a temporary directory spelled as its physical path. The store
// records physical paths, so a test that compares what it recorded against what
// it created must create it that way: on a host whose temp dir sits behind a
// symlink the two spellings differ.
func physTemp(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// physRepoDir is repoDir with the physical spelling of the checkout.
func physRepoDir(t *testing.T, name, origin string) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(repoDir(t, name, origin))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// checkoutServer is a server whose working directory is dir, on a shared store.
// workingDir is captured once at construction from the process directory, so a
// test sets it the way New would have found it rather than changing the
// directory of a process that other tests share.
func checkoutServer(t *testing.T, store *memory.Store, dir string) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	srv.workingDir = dir
	return srv
}

func projectPath(t *testing.T, store *memory.Store, id string) (string, bool) {
	t.Helper()
	projects, err := store.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	for _, p := range projects {
		if p.ID == id {
			return p.Path, true
		}
	}
	return "", false
}

func saveUnder(t *testing.T, srv *Server, tool, projectID string) string {
	t.Helper()
	session := connectedClient(t, srv)
	args := map[string]any{"project_id": projectID}
	switch tool {
	case "ghost_memory_save":
		args["content"] = "the " + projectID + " retries three times"
		args["category"] = "fact"
	case "ghost_decision_record":
		args["title"] = "Retry " + projectID + " three times"
		args["decision"] = "Three attempts with exponential backoff"
		args["rationale"] = "The upstream answers 503 under load and recovers within seconds"
		args["alternatives"] = []string{"Give up immediately", "Retry forever"}
	}
	res := callTool(t, session, tool, args)
	if res.IsError {
		t.Fatalf("%s failed: %s", tool, resultText(res))
	}
	return resultText(res)
}

// TestNameShapedSaveBindsProjectToCheckout is the #957 regression: a project a
// save creates from a name must be recorded against the checkout, path and
// remote, so the session-start resolver finds it from that directory. Both tools
// open projects through ensureProjectFor.
func TestNameShapedSaveBindsProjectToCheckout(t *testing.T) {
	memory.SetDetectRemote(repo.DetectRemote)
	t.Cleanup(func() { memory.SetDetectRemote(nil) })
	const origin = "https://github.com/wcatz/notifier.git"

	for _, tool := range []string{"ghost_memory_save", "ghost_decision_record"} {
		t.Run(tool, func(t *testing.T) {
			store := testStore(t)
			checkout := physRepoDir(t, "checkout", origin)
			saveUnder(t, checkoutServer(t, store, checkout), tool, "notifier")

			if got, ok := projectPath(t, store, "notifier"); !ok || got != checkout {
				t.Fatalf("notifier path = %q (found %v), want the checkout %q", got, ok, checkout)
			}
			ctx := context.Background()
			if id, _, err := store.ResolveProject(ctx, checkout); err != nil || id != "notifier" {
				t.Fatalf("resolve from the checkout = %q, %v; want notifier", id, err)
			}
			// The remote is bound too: a second checkout of the same repository
			// resolves to the project through repository identity alone.
			other := physRepoDir(t, "elsewhere", origin)
			if id, _, err := store.ResolveProject(ctx, other); err != nil || id != "notifier" {
				t.Fatalf("resolve from a second checkout = %q, %v; want notifier", id, err)
			}
		})
	}
}

// TestNameShapedSaveBindsPathWithoutARemote pins the other half of the issue: a
// checkout with no remote still gets its physical path recorded.
func TestNameShapedSaveBindsPathWithoutARemote(t *testing.T) {
	store := testStore(t)
	dir := filepath.Join(physTemp(t), "plain")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	saveUnder(t, checkoutServer(t, store, dir), "ghost_memory_save", "notifier")
	if got, ok := projectPath(t, store, "notifier"); !ok || got != dir {
		t.Fatalf("notifier path = %q (found %v), want %q", got, ok, dir)
	}
}

// TestSaveFromAnotherDirectoryDoesNotMoveTheBinding: an existing project is
// never rebound, whichever directory a later server runs in.
func TestSaveFromAnotherDirectoryDoesNotMoveTheBinding(t *testing.T) {
	store := testStore(t)
	first := filepath.Join(physTemp(t), "first")
	second := filepath.Join(physTemp(t), "second")
	for _, d := range []string{first, second} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	saveUnder(t, checkoutServer(t, store, first), "ghost_memory_save", "notifier")
	saveUnder(t, checkoutServer(t, store, second), "ghost_memory_save", "notifier")
	saveUnder(t, checkoutServer(t, store, second), "ghost_decision_record", "notifier")

	if got, _ := projectPath(t, store, "notifier"); got != first {
		t.Fatalf("notifier path = %q after saves from elsewhere, want %q", got, first)
	}
	if _, ok := projectPath(t, store, "notifier"); !ok {
		t.Fatal("notifier missing")
	}
}

// TestSecondNewNameFromAClaimedCheckoutOpensItsOwnProject: a save for a different
// new project made from a checkout that already belongs to one must not be folded
// into it, by path or by remote.
func TestSecondNewNameFromAClaimedCheckoutOpensItsOwnProject(t *testing.T) {
	memory.SetDetectRemote(repo.DetectRemote)
	t.Cleanup(func() { memory.SetDetectRemote(nil) })
	store := testStore(t)
	checkout := physRepoDir(t, "checkout", "https://github.com/wcatz/notifier.git")
	srv := checkoutServer(t, store, checkout)

	saveUnder(t, srv, "ghost_memory_save", "notifier")
	saveUnder(t, srv, "ghost_memory_save", "billing")

	if got, _ := projectPath(t, store, "notifier"); got != checkout {
		t.Fatalf("notifier path = %q, want %q", got, checkout)
	}
	got, ok := projectPath(t, store, "billing")
	if !ok {
		t.Fatal("billing was not opened as its own project")
	}
	if got == checkout {
		t.Fatalf("billing was bound to the checkout notifier owns: %q", got)
	}
	ctx := context.Background()
	if id, _, err := store.ResolveProject(ctx, checkout); err != nil || id != "notifier" {
		t.Fatalf("the checkout resolves to %q, %v; want notifier", id, err)
	}
}

// TestNoBindingFromAClaimedRemoteOrAHostileDirectory: the save still succeeds and
// opens the project the way it always did when the directory cannot be recorded.
func TestNoBindingFromAClaimedRemoteOrAHostileDirectory(t *testing.T) {
	memory.SetDetectRemote(repo.DetectRemote)
	t.Cleanup(func() { memory.SetDetectRemote(nil) })

	t.Run("remote claimed by another project", func(t *testing.T) {
		store := testStore(t)
		const origin = "https://github.com/wcatz/notifier.git"
		first := physRepoDir(t, "first", origin)
		saveUnder(t, checkoutServer(t, store, first), "ghost_memory_save", "notifier")
		second := physRepoDir(t, "second", origin)
		saveUnder(t, checkoutServer(t, store, second), "ghost_memory_save", "billing")
		if got, _ := projectPath(t, store, "billing"); got == second {
			t.Fatalf("billing was bound to %q although its remote belongs to notifier", got)
		}
		if got, _ := projectPath(t, store, "notifier"); got != first {
			t.Fatalf("notifier path moved to %q", got)
		}
	})

	t.Run("directory the project-shape rule refuses", func(t *testing.T) {
		store := testStore(t)
		dir := filepath.Join(physTemp(t), "odd«dir")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Skipf("filesystem refuses the name: %v", err)
		}
		saveUnder(t, checkoutServer(t, store, dir), "ghost_memory_save", "notifier")
		if got, ok := projectPath(t, store, "notifier"); !ok || got == dir {
			t.Fatalf("notifier path = %q (found %v); a refused directory must not be recorded", got, ok)
		}
	})
}

// TestWorkingDirNeverTheHomeDirectoryOrRoot: neither is a checkout, so neither is
// ever bound.
func TestWorkingDirNeverTheHomeDirectoryOrRoot(t *testing.T) {
	home := physTemp(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	t.Run("home", func(t *testing.T) {
		t.Chdir(home)
		if got := workingDirFromEnv(); got != "" {
			t.Fatalf("workingDirFromEnv in the home directory = %q, want empty", got)
		}
	})
	t.Run("root", func(t *testing.T) {
		t.Chdir(string(filepath.Separator))
		if got := workingDirFromEnv(); got != "" {
			t.Fatalf("workingDirFromEnv at the root = %q, want empty", got)
		}
	})
	t.Run("a project directory under home", func(t *testing.T) {
		sub := filepath.Join(home, "src", "notifier")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(sub)
		want, err := filepath.EvalSymlinks(sub)
		if err != nil {
			t.Fatal(err)
		}
		if got := workingDirFromEnv(); got != want {
			t.Fatalf("workingDirFromEnv = %q, want the physical path %q", got, want)
		}
	})
}

// TestBindNewProjectToCheckoutNeverMerges: the claim test and the insert are one
// store transaction, so two first saves that both saw the directory unclaimed
// cannot fold the second project away. The loser is told bound=false and writes
// nothing; a project a save is about to write under always keeps its row.
func TestBindNewProjectToCheckoutNeverMerges(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	dir := filepath.Join(physTemp(t), "checkout")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	first, err := store.BindNewProjectToCheckout(ctx, "notifier", dir, "notifier", "")
	if err != nil || !first {
		t.Fatalf("first bind = %v, %v; want true", first, err)
	}
	second, err := store.BindNewProjectToCheckout(ctx, "billing", dir, "billing", "")
	if err != nil || second {
		t.Fatalf("second bind of the same directory = %v, %v; want false", second, err)
	}
	if _, ok := projectPath(t, store, "billing"); ok {
		t.Fatal("a declined bind wrote a project row")
	}
	for _, bad := range []string{"", "relative/dir", string(filepath.Separator), "/app", "/tmp/x"} {
		if ok, err := store.BindNewProjectToCheckout(ctx, "other", bad, "other", ""); err != nil || ok {
			t.Fatalf("bind at unusable path %q = %v, %v; want false", bad, ok, err)
		}
	}
	again, err := store.BindNewProjectToCheckout(ctx, "notifier", filepath.Join(physTemp(t), "elsewhere"), "notifier", "")
	if err != nil || again {
		t.Fatalf("rebind of an existing project = %v, %v; want false", again, err)
	}
	if got, _ := projectPath(t, store, "notifier"); got != dir {
		t.Fatalf("notifier path = %q, want %q", got, dir)
	}
}

// TestSaveKeepsItsProjectRowWhenTheDirectoryRecordsAnotherRemote: a project that
// records this directory with a remote the checkout no longer reports is
// invisible to the resolver but still owns the path. The save must open its own
// project, with a row, and leave the owner alone.
func TestSaveKeepsItsProjectRowWhenTheDirectoryRecordsAnotherRemote(t *testing.T) {
	memory.SetDetectRemote(repo.DetectRemote)
	t.Cleanup(func() { memory.SetDetectRemote(nil) })
	store := testStore(t)
	ctx := context.Background()
	dir := filepath.Join(physTemp(t), "checkout")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureProjectWithRepo(ctx, "owner", dir, "owner", "https://github.com/acme/old.git"); err != nil {
		t.Fatal(err)
	}

	for _, tool := range []string{"ghost_memory_save", "ghost_decision_record"} {
		saveUnder(t, checkoutServer(t, store, dir), tool, "notifier")
	}
	if _, ok := projectPath(t, store, "notifier"); !ok {
		t.Fatal("the save's project has no row")
	}
	if got, _ := projectPath(t, store, "owner"); got != dir {
		t.Fatalf("owner path = %q, want %q", got, dir)
	}
}

// TestBindNewProjectToCheckoutDeclinesAnOverlappingClaim: the containment guards
// `ghost project bind` applies. A directory holding another project's checkout
// would claim every clone beneath it, and a directory inside a project that
// records no remote is already answered for by that project.
func TestBindNewProjectToCheckoutDeclinesAnOverlappingClaim(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	parent := filepath.Join(physTemp(t), "workspace")
	child := filepath.Join(parent, "infra")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := store.EnsureProjectWithRepo(ctx, "infra", child, "infra", ""); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.BindNewProjectToCheckout(ctx, "infra2", parent, "infra2", ""); err != nil || ok {
		t.Fatalf("bind of a directory holding another project = %v, %v; want false", ok, err)
	}

	nested := filepath.Join(child, "sub", "dir-longer-than-ten")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.BindNewProjectToCheckout(ctx, "inner", nested, "inner", ""); err != nil || ok {
		t.Fatalf("bind inside a project with no remote = %v, %v; want false", ok, err)
	}
	for _, id := range []string{"infra2", "inner"} {
		if _, found := projectPath(t, store, id); found {
			t.Fatalf("a declined bind wrote a row for %s", id)
		}
	}
}

// TestBindNewProjectToCheckoutJudgesThePhysicalPath: the guards compare physical
// paths, so a symlinked spelling of a claimed directory is the same claim.
func TestBindNewProjectToCheckoutJudgesThePhysicalPath(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	root := physTemp(t)
	real := filepath.Join(root, "workspace", "infra")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link-to-workspace")
	if err := os.Symlink(filepath.Join(root, "workspace"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := store.EnsureProjectWithRepo(ctx, "infra", real, "infra", ""); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.BindNewProjectToCheckout(ctx, "infra2", link, "infra2", ""); err != nil || ok {
		t.Fatalf("bind of a symlink to a directory holding another project = %v, %v; want false", ok, err)
	}
	if ok, err := store.BindNewProjectToCheckout(ctx, "same", filepath.Join(link, "infra"), "same", ""); err != nil || ok {
		t.Fatalf("bind of a symlinked spelling of a claimed directory = %v, %v; want false", ok, err)
	}
}
