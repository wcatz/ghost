package mcpserver

import (
	"context"
	"fmt"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/repo"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func physTemp(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
func physRepoDir(t *testing.T, name, origin string) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(repoDir(t, name, origin))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
func gitInit(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Skipf("cannot create %q: %v", dir, err)
	}
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
}
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
			other := physRepoDir(t, "elsewhere", origin)
			if id, _, err := store.ResolveProject(ctx, other); err != nil || id != "notifier" {
				t.Fatalf("resolve from a second checkout = %q, %v; want notifier", id, err)
			}
		})
	}
}
func TestNameShapedSaveBindsPathWithoutARemote(t *testing.T) {
	store := testStore(t)
	dir := filepath.Join(physTemp(t), "plain")
	gitInit(t, dir)
	saveUnder(t, checkoutServer(t, store, dir), "ghost_memory_save", "notifier")
	if got, ok := projectPath(t, store, "notifier"); !ok || got != dir {
		t.Fatalf("notifier path = %q (found %v), want %q", got, ok, dir)
	}
}
func TestSaveFromAnotherDirectoryDoesNotMoveTheBinding(t *testing.T) {
	store := testStore(t)
	first := filepath.Join(physTemp(t), "first")
	second := filepath.Join(physTemp(t), "second")
	gitInit(t, first)
	gitInit(t, second)
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
		gitInit(t, dir)
		saveUnder(t, checkoutServer(t, store, dir), "ghost_memory_save", "notifier")
		if got, ok := projectPath(t, store, "notifier"); !ok || got == dir {
			t.Fatalf("notifier path = %q (found %v); a refused directory must not be recorded", got, ok)
		}
	})
}
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
func TestBindNewProjectToCheckoutNeverMerges(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	root := physTemp(t)
	dir := filepath.Join(root, "checkout")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(file, []byte("file"), 0o644); err != nil {
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
	for _, bad := range []string{"", "relative/dir", string(filepath.Separator), filepath.Join(root, "absent"), file} {
		if ok, err := store.BindNewProjectToCheckout(ctx, "other", bad, "other", ""); err != nil || ok {
			t.Fatalf("bind at unusable path %q = %v, %v; want false", bad, ok, err)
		}
	}
	again, err := store.BindNewProjectToCheckout(ctx, "notifier", filepath.Join(root, "elsewhere"), "notifier", "")
	if err != nil || again {
		t.Fatalf("rebind of an existing project = %v, %v; want false", again, err)
	}
	if got, _ := projectPath(t, store, "notifier"); got != dir {
		t.Fatalf("notifier path = %q, want %q", got, dir)
	}
}
func TestSaveKeepsItsProjectRowWhenTheDirectoryRecordsAnotherRemote(t *testing.T) {
	memory.SetDetectRemote(repo.DetectRemote)
	t.Cleanup(func() { memory.SetDetectRemote(nil) })
	store := testStore(t)
	ctx := context.Background()
	dir := filepath.Join(physTemp(t), "checkout")
	gitInit(t, dir)
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
func TestSaveBindsOnlyAGitCheckoutsTopLevel(t *testing.T) {
	t.Run("a directory inside a checkout binds the top level", func(t *testing.T) {
		store := testStore(t)
		top := physTemp(t)
		gitInit(t, top)
		sub := filepath.Join(top, "internal", "api")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		saveUnder(t, checkoutServer(t, store, sub), "ghost_memory_save", "notifier")
		if got, ok := projectPath(t, store, "notifier"); !ok || got != top {
			t.Fatalf("notifier path = %q (found %v), want the top level %q", got, ok, top)
		}
	})
	t.Run("a plain directory is declined", func(t *testing.T) {
		store := testStore(t)
		dir := filepath.Join(physTemp(t), "downloads")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		saveUnder(t, checkoutServer(t, store, dir), "ghost_memory_save", "notifier")
		if got, ok := projectPath(t, store, "notifier"); !ok || got == dir {
			t.Fatalf("notifier path = %q (found %v); a non-checkout must not be recorded", got, ok)
		}
	})
	t.Run("a directory that only holds checkouts is declined", func(t *testing.T) {
		store := testStore(t)
		parent := filepath.Join(physTemp(t), "git")
		gitInit(t, filepath.Join(parent, "one"))
		saveUnder(t, checkoutServer(t, store, parent), "ghost_memory_save", "notifier")
		if got, ok := projectPath(t, store, "notifier"); !ok || got == parent {
			t.Fatalf("notifier path = %q (found %v); the parent of checkouts must not be recorded", got, ok)
		}
	})
	t.Run("the home directory and its ancestors are declined", func(t *testing.T) {
		top := physTemp(t)
		gitInit(t, top)
		home := filepath.Join(top, "home", "wayne")
		if err := os.MkdirAll(home, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		store := testStore(t)
		saveUnder(t, checkoutServer(t, store, top), "ghost_memory_save", "notifier")
		if got, ok := projectPath(t, store, "notifier"); !ok || got == top {
			t.Fatalf("notifier path = %q (found %v); an ancestor of home must not be recorded", got, ok)
		}
		gitInit(t, home)
		store = testStore(t)
		saveUnder(t, checkoutServer(t, store, home), "ghost_memory_save", "notifier")
		if got, ok := projectPath(t, store, "notifier"); !ok || got == home {
			t.Fatalf("notifier path = %q (found %v); home must not be recorded", got, ok)
		}
	})
}
func TestBindNewProjectToCheckoutDeclinesARemoteOnlyClaim(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	const origin = "https://github.com/acme/notifier.git"
	owned := filepath.Join(physTemp(t), "owned")
	other := filepath.Join(physTemp(t), "other")
	for _, d := range []string{owned, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EnsureProjectWithRepo(ctx, "owner", owned, "owner", origin); err != nil {
		t.Fatal(err)
	}
	ok, err := store.BindNewProjectToCheckout(ctx, "billing", other, "billing", origin)
	if err != nil || ok {
		t.Fatalf("bind with a remote another project records = %v, %v; want false, nil", ok, err)
	}
	if _, found := projectPath(t, store, "billing"); found {
		t.Fatal("a declined bind wrote a row")
	}
}
func TestBindNewProjectToCheckoutHasOneWinner(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "shared.sqlite")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	const handles = 4
	stores := make([]*memory.Store, handles)
	for i := range stores {
		db, err := memory.OpenDB(dbPath)
		if err != nil {
			t.Fatalf("OpenDB: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		stores[i] = memory.NewStore(db, logger)
	}
	dir := filepath.Join(physTemp(t), "checkout")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	type result struct {
		bound bool
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, handles)
	for i, store := range stores {
		go func(store *memory.Store, id string) {
			<-start
			ok, err := store.BindNewProjectToCheckout(context.Background(), id, dir, id, "")
			results <- result{ok, err}
		}(store, fmt.Sprintf("project-%d", i))
	}
	close(start)
	winners := 0
	for range stores {
		r := <-results
		if r.err != nil {
			t.Errorf("a concurrent bind returned an error: %v", r.err)
		}
		if r.bound {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("%d binds won the checkout, want exactly 1", winners)
	}
	rows, err := stores[0].ListProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	bound := 0
	for _, p := range rows {
		if p.Path == dir {
			bound++
		}
	}
	if bound != 1 {
		t.Fatalf("%d projects record the checkout, want 1", bound)
	}
}
func TestContainsOrIsComparesAcrossSeparators(t *testing.T) {
	for _, c := range []struct {
		dir, path string
		want      bool
	}{
		{"C:/Users/x", `C:\Users\x`, true},
		{"C:/Users", `C:\Users\x`, true},
		{"C:/Users/x/repo", `C:\Users\x`, false},
		{"C:/Users/xy", `C:\Users\x`, false},
		{"/home/u", "/home/u/", true},
		{"/home", "/home/u", true},
		{"/home/u/src", "/home/u", false},
	} {
		if got := containsOrIs(c.dir, c.path); got != c.want {
			t.Errorf("containsOrIs(%q, %q) = %v, want %v", c.dir, c.path, got, c.want)
		}
	}
}
func TestDriveRelativeIdIsNotBoundAsAName(t *testing.T) {
	store := testStore(t)
	top := physTemp(t)
	gitInit(t, top)
	saveUnder(t, checkoutServer(t, store, top), "ghost_memory_save", "C:notifier")
	if got, ok := projectPath(t, store, "C:notifier"); ok && got == top {
		t.Fatalf("a drive-relative id was bound to the checkout %q", got)
	}
}
