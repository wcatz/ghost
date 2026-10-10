package memory

import (
	"context"
	"testing"
)

// A reflection merge that re-states one of its sources verbatim reuses that row
// instead of inserting a new one. The other sources are deleted at the end of the
// replace, and the foreign key takes their evidence with them, so the reuse has to
// carry it the way the fresh insert does.
func TestReplaceNonManualReuseCarriesTheFoldedRowsEvidence(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "p", "/p", "p"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	keep, err := s.Create(ctx, "p", Memory{Category: "fact", Content: "run go vet before committing a change", Source: "reflection", Agent: "keeper"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	folded, err := s.Create(ctx, "p", Memory{Category: "fact", Content: "run go vet before committing any change", Source: "reflection", Agent: "folded"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err = s.ReplaceNonManual(ctx, "p", []Memory{{
		ProjectID: "p", Category: "fact", Content: "run go vet before committing a change",
		Source: "reflection", ReplacesIDs: []string{keep, folded},
	}}, "")
	if err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	ev, err := s.MemoryProvenance(ctx, keep)
	if err != nil {
		t.Fatalf("MemoryProvenance: %v", err)
	}
	carried := false
	for _, e := range ev {
		if e.Agent == "folded" && e.CarriedFrom == folded {
			carried = true
		}
	}
	if !carried {
		t.Fatalf("the reused row carries no evidence from the row folded into it: %+v", ev)
	}
}
