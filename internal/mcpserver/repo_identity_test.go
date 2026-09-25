package mcpserver

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/repo"
)

// repoDir creates a directory that git reports as an origin remote. The
// repository content is irrelevant — detection reads `git config
// remote.origin.url`, and two directories configured with the same origin are
// two checkouts of one repository as far as identity is concerned.
func repoDir(t *testing.T, name, origin string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	init := exec.Command("git", "init", "-q")
	init.Dir = dir
	if out, err := init.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	add := exec.Command("git", "remote", "add", "origin", origin)
	add.Dir = dir
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v\n%s", err, out)
	}
	return dir
}

// TestSaveAcrossCheckoutsOfOneRepositoryIsOneProject is the end-to-end proof
// that repository identity is reachable from the product's normal write path.
//
// Schema alone would not be: a column nothing populates and a resolution step
// nothing reaches are inert. This drives two real MCP saves from two different
// absolute paths that share one origin remote, and asserts the second save
// lands in the first project rather than opening a second one.
func TestSaveAcrossCheckoutsOfOneRepositoryIsOneProject(t *testing.T) {
	// main() wires this for the real binary; tests build stores directly.
	memory.SetDetectRemote(repo.DetectRemote)
	t.Cleanup(func() { memory.SetDetectRemote(nil) })
	const origin = "https://github.com/wcatz/ghost.git"
	checkoutA := repoDir(t, "checkout-a", origin)
	checkoutB := repoDir(t, "checkout-b", origin)

	_, session := newCapSession(t)

	for i, args := range []map[string]any{
		{"project_id": checkoutA, "content": "saved from the first checkout", "category": "fact"},
		{"project_id": checkoutB, "content": "saved from the second checkout", "category": "fact"},
	} {
		res := callTool(t, session, "ghost_memory_save", args)
		if res.IsError {
			t.Fatalf("save %d failed: %s", i, resultText(res))
		}
	}

	// Everything must be reachable from the FIRST checkout: if the two paths
	// had become separate projects, the memory saved from the second would be
	// invisible here, and the agent would be working from half the context.
	out := resultText(callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": checkoutA,
		"query":      "saved from checkout",
		"limit":      10,
	}))
	for _, want := range []string{"saved from the first checkout", "saved from the second checkout"} {
		if !strings.Contains(out, want) {
			t.Errorf("search from the first checkout did not return %q — the two checkouts became separate projects:\n%s", want, out)
		}
	}

	// And the reverse direction, so a merge that only works one way is caught.
	back := resultText(callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": checkoutB,
		"query":      "saved from checkout",
		"limit":      10,
	}))
	for _, want := range []string{"saved from the first checkout", "saved from the second checkout"} {
		if !strings.Contains(back, want) {
			t.Errorf("search from the second checkout did not return %q:\n%s", want, back)
		}
	}
}

// TestPathSaveJoinsExistingNamedProjectAcrossCheckoutNames covers the common
// lifecycle that repository identity missed: a project is created under its
// plain name, and only later does an agent save by absolute checkout path.
// The checkout directories deliberately do not share the project name, so
// basename resolution cannot accidentally make this pass. The detected
// repository identity is what must join the existing project.
func TestPathSaveJoinsExistingNamedProjectAcrossCheckoutNames(t *testing.T) {
	// main() wires this for the real binary; tests build stores directly.
	memory.SetDetectRemote(repo.DetectRemote)
	t.Cleanup(func() { memory.SetDetectRemote(nil) })

	const origin = "https://github.com/wcatz/ghost.git"
	checkoutA := repoDir(t, "checkout-a", origin)
	checkoutB := repoDir(t, "checkout-b", origin)
	srv, session := newCapSession(t)

	saves := []struct {
		projectID string
		content   string
	}{
		{"ghost", "saved under the plain project name"},
		{checkoutA, "saved from the first differently named checkout"},
		{checkoutB, "saved from the second differently named checkout"},
	}
	for i, save := range saves {
		res := callTool(t, session, "ghost_memory_save", map[string]any{
			"project_id": save.projectID,
			"content":    save.content,
			"category":   "fact",
		})
		if res.IsError {
			t.Fatalf("save %d failed: %s", i, resultText(res))
		}
	}

	projects, err := srv.store.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	// newCapSession starts with test-project. The three saves above must add
	// only the existing ghost project, not one or more path-named duplicates.
	if len(projects) != 2 {
		var names []string
		for _, project := range projects {
			names = append(names, project.Name)
		}
		t.Fatalf("path-based saves created duplicate project identities: got %d projects %v, want test-project and ghost", len(projects), names)
	}
	for _, project := range projects {
		if project.ID == checkoutA || project.ID == checkoutB {
			t.Errorf("checkout path became project id %q; repository identity was not attached to the named project", project.ID)
		}
	}

	out := resultText(callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "ghost",
		"query":      "saved",
		"limit":      10,
	}))
	for _, save := range saves {
		if !strings.Contains(out, save.content) {
			t.Errorf("named project cannot see %q after path-based saves:\n%s", save.content, out)
		}
	}
}

// TestRelativeCheckoutSaveBindsExistingNamedProject pins the same contract for
// a path-shaped relative checkout. PR #565 deliberately treats both '/' and
// '\\' as path evidence rather than relying on filepath.IsAbs, which is false
// for relative and Windows root-relative paths. Save-side repository detection
// must use the same boundary or those saves silently create path projects.
func TestRelativeCheckoutSaveBindsExistingNamedProject(t *testing.T) {
	// main() wires this for the real binary; tests build stores directly.
	memory.SetDetectRemote(repo.DetectRemote)
	t.Cleanup(func() { memory.SetDetectRemote(nil) })

	const origin = "https://github.com/wcatz/ghost.git"
	checkout := repoDir(t, "checkout-relative", origin)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	relative, err := filepath.Rel(cwd, checkout)
	if err != nil {
		t.Fatalf("make checkout path relative: %v", err)
	}
	if filepath.IsAbs(relative) || !strings.ContainsAny(relative, `/\`) {
		t.Fatalf("precondition: relative checkout %q is not a path-shaped non-absolute input", relative)
	}
	srv, session := newCapSession(t)

	for i, save := range []struct {
		projectID string
		content   string
	}{
		{"ghost", "saved before the relative checkout"},
		{relative, "saved from the relative checkout"},
	} {
		res := callTool(t, session, "ghost_memory_save", map[string]any{
			"project_id": save.projectID,
			"content":    save.content,
			"category":   "fact",
		})
		if res.IsError {
			t.Fatalf("save %d failed: %s", i, resultText(res))
		}
	}

	projects, err := srv.store.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("relative checkout created a path project: got %d projects, want test-project and ghost", len(projects))
	}
	out := resultText(callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": checkout,
		"query":      "saved",
		"limit":      10,
	}))
	for _, want := range []string{"saved before the relative checkout", "saved from the relative checkout"} {
		if !strings.Contains(out, want) {
			t.Errorf("absolute checkout cannot see %q after the relative save:\n%s", want, out)
		}
	}
}

// TestPathSaveRejectsProjectWithDifferentRemote proves the conflict guard
// survives the full write path. The save originates below the project's
// recorded checkout, so ordinary longest-prefix resolution identifies the
// parent before repository binding. A contradictory remote must fail before
// any memory is written.
func TestPathSaveRejectsProjectWithDifferentRemote(t *testing.T) {
	// main() wires this for the real binary; tests build stores directly.
	memory.SetDetectRemote(repo.DetectRemote)
	t.Cleanup(func() { memory.SetDetectRemote(nil) })

	const (
		origin = "https://github.com/wcatz/ghost.git"
		other  = "https://github.com/someone/checkout.git"
	)
	checkout := repoDir(t, "checkout", origin)
	savePath := filepath.Join(checkout, "subdir")
	if err := os.MkdirAll(savePath, 0o755); err != nil {
		t.Fatalf("create child checkout path: %v", err)
	}
	srv, session := newCapSession(t)
	ctx := context.Background()
	if err := srv.store.EnsureProjectWithRepo(ctx, checkout, "", checkout, other); err != nil {
		t.Fatalf("seed conflicting path project: %v", err)
	}
	if err := srv.store.EnsureProject(ctx, "ghost", "", "ghost"); err != nil {
		t.Fatalf("seed same-name project: %v", err)
	}

	res := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": savePath,
		"content":    "must not enter a project from another repository",
		"category":   "fact",
	})
	if !res.IsError {
		t.Fatalf("save into a project with a different recorded remote succeeded: %s", resultText(res))
	}
	if out := resultText(res); !strings.Contains(out, "different repository") {
		t.Errorf("save error does not explain the repository conflict: %s", out)
	}
	for _, projectID := range []string{checkout, "ghost"} {
		count, err := srv.store.CountMemories(ctx, projectID)
		if err != nil {
			t.Fatalf("CountMemories(%q): %v", projectID, err)
		}
		if count != 0 {
			t.Errorf("conflicting save wrote %d memories into %q, want 0", count, projectID)
		}
	}
}

// TestPathSaveRecordsRepositoryForLaterCheckout closes the ordering gap: the
// checkout being saved from has the same basename as the project, so the old
// pre-resolution returns its id before repository detection runs. A different
// checkout of the same repository must nevertheless be able to read that save
// immediately; otherwise identity was not recorded and a later path save is
// still required to repair it.
func TestPathSaveRecordsRepositoryForLaterCheckout(t *testing.T) {
	// main() wires this for the real binary; tests build stores directly.
	memory.SetDetectRemote(repo.DetectRemote)
	t.Cleanup(func() { memory.SetDetectRemote(nil) })

	const origin = "https://github.com/wcatz/ghost.git"
	checkout := repoDir(t, "ghost", origin)
	laterCheckout := repoDir(t, "later-checkout", origin)
	_, session := newCapSession(t)

	for i, save := range []struct {
		projectID string
		content   string
	}{
		{"ghost", "saved before any checkout path"},
		{checkout, "saved from the first checkout"},
	} {
		res := callTool(t, session, "ghost_memory_save", map[string]any{
			"project_id": save.projectID,
			"content":    save.content,
			"category":   "fact",
		})
		if res.IsError {
			t.Fatalf("save %d failed: %s", i, resultText(res))
		}
	}

	out := resultText(callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": laterCheckout,
		"query":      "saved",
		"limit":      10,
	}))
	for _, want := range []string{"saved before any checkout path", "saved from the first checkout"} {
		if !strings.Contains(out, want) {
			t.Errorf("later checkout cannot see %q; the first path save did not record repository identity:\n%s", want, out)
		}
	}
}

// TestSaveWithNamedProjectDoesNotSpawnGit guards the cost of the feature: a
// caller that names a project ("ghost", "platform-ops") gives no path to
// inspect, so detection must not run at all. Without this, every ordinary
// save would pay for a git process.
func TestSaveWithNamedProjectDoesNotSpawnGit(t *testing.T) {
	var mcpDetections, storeDetections int
	detectRemoteForSave = func(string) string {
		mcpDetections++
		return ""
	}
	t.Cleanup(func() { detectRemoteForSave = repo.DetectRemote })
	memory.SetDetectRemote(func(string) string {
		storeDetections++
		return ""
	})
	t.Cleanup(func() { memory.SetDetectRemote(nil) })
	_, session := newCapSession(t)

	res := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project",
		"content":    "a memory saved under a plain project name",
		"category":   "fact",
	})
	if res.IsError {
		t.Fatalf("save failed: %s", resultText(res))
	}
	out := resultText(callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "plain project name",
		"limit":      5,
	}))
	if !strings.Contains(out, "a memory saved under a plain project name") {
		t.Errorf("named-project save was not retrievable:\n%s", out)
	}
	if mcpDetections != 0 || storeDetections != 0 {
		t.Errorf("named save crossed a repository detector: MCP=%d store=%d, want 0/0", mcpDetections, storeDetections)
	}
}
