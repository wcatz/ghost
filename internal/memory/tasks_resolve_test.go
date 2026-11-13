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
// read.
func TestResolveTaskIDExactMatchWinsOverALongerPrefixedID(t *testing.T) {
	ctx := context.Background()
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

	status := "active"
	if got, err := store.UpdateTask(ctx, "T1", &status, nil, nil); err != nil || got.ID != "T1" || got.Status != status {
		t.Fatalf("UpdateTask(T1) = %+v, %v; want T1 active", got, err)
	}
	if _, err := store.CompleteTask(ctx, "T1", "done"); err != nil {
		t.Fatalf("CompleteTask(T1): %v", err)
	}
	if got, err := store.GetTask(ctx, "T1"); err != nil || got.Status != "done" {
		t.Errorf("GetTask(T1) = %+v, %v; want done", got, err)
	}
	if got, err := store.GetTask(ctx, "T10"); err != nil || got.Status != "pending" {
		t.Errorf("GetTask(T10) = %+v, %v; want pending untouched", got, err)
	}
	if _, err := store.GetTask(ctx, "T"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("GetTask(T) = %v, want an ambiguity refusal", err)
	}
	if got, err := store.GetTask(ctx, "U4"); err != nil || got.ID != "U42" {
		t.Errorf("GetTask(U4) = %+v, %v; want U42", got, err)
	}
}
