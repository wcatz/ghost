package memory

import (
	"context"
	"strings"
	"testing"
)

func TestPurgeDeletedMemoryHistoryInProjectDecidesInsideThePurge(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	hist := func(id string) int {
		t.Helper()
		e, err := s.MemoryHistory(ctx, id, 0)
		if err != nil {
			t.Fatalf("MemoryHistory: %v", err)
		}
		return len(e)
	}
	create := func(content string) string {
		t.Helper()
		id, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: content, Source: "manual", Importance: 0.5})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		return id
	}
	if _, err := s.PurgeDeletedMemoryHistoryInProject(ctx, "", "x"); err == nil {
		t.Error("empty project not refused")
	}
	live := create("the vqzt relay is on port nine")
	global := create("the vqzt relay is on port ten")
	if err := s.PromoteToGlobal(ctx, testProject, global); err != nil {
		t.Fatalf("PromoteToGlobal: %v", err)
	}
	deleted := create("the vqzt relay is on port eleven")
	if err := s.Delete(ctx, deleted); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for _, r := range []struct {
		name, project, id, want string
	}{
		{"live in project", testProject, live, "live again"},
		{"live promoted", testProject, global, "not found in project"},
		{"other project tombstone", "other-project", deleted, "not found in project"},
		{"no history", testProject, "no-such-memory", "not found"},
	} {
		before := hist(r.id)
		_, err := s.PurgeDeletedMemoryHistoryInProject(ctx, r.project, r.id)
		if err == nil || !strings.Contains(err.Error(), r.want) {
			t.Errorf("%s: err = %v", r.name, err)
		}
		if after := hist(r.id); after != before {
			t.Errorf("%s: history %d -> %d", r.name, before, after)
		}
	}
	for _, id := range []string{live, global} {
		if mems, err := s.GetByIDs(ctx, []string{id}); err != nil || len(mems) != 1 {
			t.Errorf("live row %s gone: %v", id, err)
		}
	}
	out, err := s.PurgeDeletedMemoryHistoryInProject(ctx, testProject, deleted)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if out.Rows == 0 || hist(deleted) != 0 {
		t.Errorf("purge removed %d left %d", out.Rows, hist(deleted))
	}
}
