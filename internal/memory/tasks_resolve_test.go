package memory

import (
	"context"
	"strings"
	"testing"
)

// TestResolveTaskIDExactMatchWinsOverALongerPrefixedID pins the task resolver to
// the memory-reference rule: an id that matches a task byte for byte resolves to
// that task at any length, and only a non-exact input goes through the prefix
// scan. `ghost import` writes an artifact's ids verbatim, so a store can hold T1
// and T10, and with only the prefix scan T1 could never be completed, updated or
// read — no longer spelling of it exists.
func TestResolveTaskIDExactMatchWinsOverALongerPrefixedID(t *testing.T) {
	ctx := context.Background()

	setup := func(t *testing.T) *Store {
		t.Helper()
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		for _, id := range []string{"T1", "T10", "U42"} {
			if _, _, err := store.ImportTask(ctx, Task{
				ID: id, ProjectID: "p1", Title: "task " + id, Status: "pending", Priority: 2,
			}, true); err != nil {
				t.Fatalf("ImportTask(%s): %v", id, err)
			}
		}
		return store
	}

	t.Run("get", func(t *testing.T) {
		store := setup(t)
		got, err := store.GetTask(ctx, "T1")
		if err != nil {
			t.Fatalf("GetTask(T1): %v", err)
		}
		if got.ID != "T1" {
			t.Errorf("GetTask(T1).ID = %q, want T1", got.ID)
		}
	})

	t.Run("update", func(t *testing.T) {
		store := setup(t)
		status := "active"
		got, err := store.UpdateTask(ctx, "T1", &status, nil, nil)
		if err != nil {
			t.Fatalf("UpdateTask(T1): %v", err)
		}
		if got.ID != "T1" || got.Status != status {
			t.Errorf("UpdateTask(T1) = %+v, want T1 active", got)
		}
		other, err := store.GetTask(ctx, "T10")
		if err != nil {
			t.Fatalf("GetTask(T10): %v", err)
		}
		if other.Status != "pending" {
			t.Errorf("T10 status = %q, want pending untouched", other.Status)
		}
	})

	t.Run("complete", func(t *testing.T) {
		store := setup(t)
		if _, err := store.CompleteTask(ctx, "T1", "done"); err != nil {
			t.Fatalf("CompleteTask(T1): %v", err)
		}
		got, err := store.GetTask(ctx, "T1")
		if err != nil {
			t.Fatalf("GetTask(T1): %v", err)
		}
		if got.Status != "done" {
			t.Errorf("T1 status = %q, want done", got.Status)
		}
		other, err := store.GetTask(ctx, "T10")
		if err != nil {
			t.Fatalf("GetTask(T10): %v", err)
		}
		if other.Status != "pending" {
			t.Errorf("T10 status = %q, want pending untouched", other.Status)
		}
	})

	t.Run("a shared prefix stays ambiguous", func(t *testing.T) {
		store := setup(t)
		_, err := store.GetTask(ctx, "T")
		if err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Errorf("GetTask(T) = %v, want an ambiguity refusal", err)
		}
	})

	t.Run("a unique prefix still resolves", func(t *testing.T) {
		store := setup(t)
		got, err := store.GetTask(ctx, "U4")
		if err != nil || got.ID != "U42" {
			t.Errorf("GetTask(U4) = %+v, %v; want U42", got, err)
		}
	})
}
