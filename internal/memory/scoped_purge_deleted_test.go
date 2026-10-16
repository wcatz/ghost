package memory

import (
	"context"
	"strings"
	"testing"
)

// TestPurgeDeletedMemoryHistoryInProjectDecidesInsideThePurge: the scoped purge
// of an already-deleted memory refuses, inside its own transaction, an id that
// has a live row (whatever project holds it) and a tombstone filed under another
// project, and erases nothing when it does. The id it does purge is one that is
// still deleted and was the caller's project's.
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
		t.Error("an empty project id was not refused")
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

	refusals := []struct {
		name, project, id, want string
	}{
		{"live row in the project", testProject, live, "live again"},
		{"live row promoted to _global", testProject, global, "not found in project"},
		{"tombstone in another project", "other-project", deleted, "not found in project"},
		{"no history at all", testProject, "no-such-memory", "not found"},
	}
	for _, r := range refusals {
		before := hist(r.id)
		_, err := s.PurgeDeletedMemoryHistoryInProject(ctx, r.project, r.id)
		if err == nil || !strings.Contains(err.Error(), r.want) {
			t.Errorf("%s: err = %v, want a refusal containing %q", r.name, err, r.want)
		}
		if after := hist(r.id); after != before {
			t.Errorf("%s: history rows %d -> %d after a refused purge", r.name, before, after)
		}
	}
	for _, id := range []string{live, global} {
		if mems, err := s.GetByIDs(ctx, []string{id}); err != nil || len(mems) != 1 {
			t.Errorf("live row %s gone after a refused purge: %v", id, err)
		}
	}

	out, err := s.PurgeDeletedMemoryHistoryInProject(ctx, testProject, deleted)
	if err != nil {
		t.Fatalf("purge of the project's own deleted memory: %v", err)
	}
	if out.Rows == 0 || hist(deleted) != 0 {
		t.Errorf("purge removed %d rows and left %d", out.Rows, hist(deleted))
	}
}
