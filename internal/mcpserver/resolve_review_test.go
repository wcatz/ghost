package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/repo"
)

func TestSaveRejectsAmbiguousProjectName(t *testing.T) {
	srv, session := newCapSession(t)
	ctx := context.Background()
	if err := srv.store.EnsureProject(ctx, "first", "", "shared"); err != nil {
		t.Fatalf("EnsureProject first: %v", err)
	}
	if err := srv.store.EnsureProject(ctx, "second", "", "shared"); err != nil {
		t.Fatalf("EnsureProject second: %v", err)
	}

	res := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "shared",
		"content":    "must not be auto-created",
		"category":   "fact",
	})
	if !res.IsError {
		t.Fatalf("ambiguous save unexpectedly succeeded: %s", resultText(res))
	}

	projects, err := srv.store.ListProjects(ctx)
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	count := 0
	for _, project := range projects {
		if project.Name == "shared" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("ambiguous save changed the project set to %d rows, want 2", count)
	}
}

func TestSaveDoesNotReclassifyExactPathShapedProjectID(t *testing.T) {
	srv, session := newCapSession(t)
	ctx := context.Background()
	memory.SetDetectRemote(repo.DetectRemote)
	t.Cleanup(func() { memory.SetDetectRemote(nil) })

	const (
		originA = "https://github.com/example/one.git"
		originB = "https://github.com/example/two.git"
	)
	checkoutA := repoDir(t, "checkout-a", originB)
	checkoutB := repoDir(t, "checkout-b", originB)
	if err := srv.store.EnsureProjectWithRepo(ctx, checkoutA, "", checkoutA, originA); err != nil {
		t.Fatalf("EnsureProjectWithRepo A: %v", err)
	}
	if err := srv.store.EnsureProjectWithRepo(ctx, checkoutB, "", checkoutB, originB); err != nil {
		t.Fatalf("EnsureProjectWithRepo B: %v", err)
	}

	res := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": checkoutA,
		"content":    "belongs to the exact project id",
		"category":   "fact",
	})
	if res.IsError {
		t.Fatalf("save failed: %s", resultText(res))
	}

	mems, err := srv.store.GetAll(ctx, checkoutA, 10)
	if err != nil {
		t.Fatalf("GetAll exact project: %v", err)
	}
	if len(mems) != 1 || !strings.Contains(mems[0].Content, "belongs to the exact project id") {
		t.Fatalf("exact path-shaped project was redirected; memories = %+v", mems)
	}
}
