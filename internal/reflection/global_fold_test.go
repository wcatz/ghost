package reflection

import (
	"reflect"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func TestPlanGlobalFold(t *testing.T) {
	mems := []memory.Memory{
		{ID: "a", Category: "preference", Content: "always run go vet before committing any change", Importance: 0.9, CreatedAt: "2026-01-01 00:00:00", Tags: []string{"x"}},
		{ID: "b", Category: "preference", Content: "always run go vet before committing a change", Importance: 0.5, CreatedAt: "2026-02-01 00:00:00", Tags: []string{"y"}},
		{ID: "c", Category: "preference", Content: "always run go vet before committing any change to the code", Importance: 0.6, CreatedAt: "2026-03-01 00:00:00", Tags: []string{"x", "z"}},
		{ID: "d", Category: "fact", Content: "cardano preprod uses network magic 1", Importance: 0.5, CreatedAt: "2026-02-01 00:00:00"},
		// A numeric conflict is a different fact, not a duplicate.
		{ID: "e", Category: "fact", Content: "cardano preprod uses network magic 2", Importance: 0.5, CreatedAt: "2026-02-02 00:00:00"},
	}
	clusters, rows := PlanGlobalFold(mems)
	if len(clusters) != 1 {
		t.Fatalf("clusters = %d, want 1: %+v", len(clusters), clusters)
	}
	c := clusters[0]
	if c.Survivor.ID != "c" || len(c.Folded) != 2 {
		t.Fatalf("survivor %q folded %d, want the newest row c with two folded", c.Survivor.ID, len(c.Folded))
	}
	if len(rows) != 3 {
		t.Fatalf("replacement set has %d rows, want cluster survivor + 2 untouched: %+v", len(rows), rows)
	}
	var fold *memory.Memory
	for i := range rows {
		if rows[i].Content == c.Survivor.Content {
			fold = &rows[i]
		}
	}
	if fold == nil {
		t.Fatalf("no replacement row restates the survivor: %+v", rows)
	}
	if fold.Importance != 0.9 {
		t.Errorf("importance = %v, want the cluster maximum", fold.Importance)
	}
	if !reflect.DeepEqual(fold.Tags, []string{"x", "y", "z"}) {
		t.Errorf("tags = %v, want the sorted union", fold.Tags)
	}
	if !reflect.DeepEqual(append([]string(nil), fold.ReplacesIDs...), []string{"c", "b", "a"}) {
		t.Errorf("ReplacesIDs = %v, want every member, newest first", fold.ReplacesIDs)
	}
	for _, r := range rows {
		if r.Content != c.Survivor.Content && len(r.ReplacesIDs) != 0 {
			t.Errorf("an untouched row claims to replace %v", r.ReplacesIDs)
		}
	}
}

func TestPlanGlobalFoldNothingToFoldReturnsNil(t *testing.T) {
	clusters, rows := PlanGlobalFold([]memory.Memory{
		{ID: "a", Content: "use tabs not spaces"},
		{ID: "b", Content: "cardano preprod uses network magic 1"},
	})
	if clusters != nil || rows != nil {
		t.Fatalf("got (%v, %v), want nil so a caller cannot replace a corpus it has no reason to touch", clusters, rows)
	}
}

func TestPlanGlobalFoldIsDeterministicOnTies(t *testing.T) {
	mems := []memory.Memory{
		{ID: "1", Content: "run the linter before pushing", CreatedAt: "2026-01-01 00:00:00"},
		{ID: "2", Content: "run the linter before pushing", CreatedAt: "2026-01-01 00:00:00"},
	}
	clusters, _ := PlanGlobalFold(mems)
	if len(clusters) != 1 || clusters[0].Survivor.ID != "2" {
		t.Fatalf("tie on created_at must resolve by id, got %+v", clusters)
	}
}
