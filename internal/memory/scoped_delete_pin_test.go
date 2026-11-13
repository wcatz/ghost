package memory

import (
	"context"
	"strings"
	"testing"
)

func TestScopedDeleteAndPinRefuseARowOutsideTheProject(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "the qwzp relay is on port nine", Source: "manual", Importance: 0.5})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.PromoteToGlobal(ctx, testProject, id); err != nil {
		t.Fatalf("PromoteToGlobal: %v", err)
	}
	hist := func() int {
		t.Helper()
		e, err := s.MemoryHistory(ctx, id, 0)
		if err != nil {
			t.Fatalf("MemoryHistory: %v", err)
		}
		return len(e)
	}
	before := hist()
	refusals := map[string]error{
		"delete":       s.DeleteWithOptions(ctx, id, DeleteOptions{ProjectID: testProject}),
		"delete purge": s.DeleteWithOptions(ctx, id, DeleteOptions{ProjectID: testProject, PurgeHistory: true}),
		"pin":          s.TogglePinInProject(ctx, testProject, id, true),
	}
	_, purgeErr := s.DeletePurgeInProject(ctx, testProject, id)
	refusals["DeletePurgeInProject"] = purgeErr
	for name, err := range refusals {
		if err == nil || !strings.Contains(err.Error(), "not found in project") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	mems, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(mems) != 1 {
		t.Fatalf("row gone: %v %d", err, len(mems))
	}
	if mems[0].ProjectID != GlobalProjectID || mems[0].Pinned {
		t.Errorf("row = project %q pinned %v", mems[0].ProjectID, mems[0].Pinned)
	}
	if after := hist(); after != before {
		t.Errorf("history %d -> %d", before, after)
	}
	if err := s.TogglePin(ctx, id, true); err != nil {
		t.Fatalf("unscoped TogglePin: %v", err)
	}
	if err := s.TogglePinInProject(ctx, GlobalProjectID, id, false); err != nil {
		t.Fatalf("TogglePinInProject owning: %v", err)
	}
	if _, err := s.DeletePurge(ctx, id); err != nil {
		t.Fatalf("unscoped DeletePurge: %v", err)
	}
}
