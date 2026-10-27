package supersede

import (
	"context"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// capStore returns only a limited set from GetAll but can still return
// neighbors via SearchVectorScoped; GetByIDs can fetch missing ones.
type capStore struct {
	*memory.Store
	all  []memory.Memory
	byID map[string]memory.Memory
}

func newCapStore(t *testing.T, base *memory.Store, getAllLimit int) *capStore {
	t.Helper()
	ctx := context.Background()
	mems, err := base.GetAll(ctx, "p", 100000)
	if err != nil {
		t.Fatal(err)
	}
	cs := &capStore{
		Store: base,
		all:   mems,
		byID:  make(map[string]memory.Memory),
	}
	for _, m := range mems {
		cs.byID[m.ID] = m
	}
	if getAllLimit >= 0 && getAllLimit < len(mems) {
		cs.all = mems[:getAllLimit]
	}
	return cs
}

func (c *capStore) GetAll(ctx context.Context, projectID string, limit int) ([]memory.Memory, error) {
	if projectID == "p" {
		return c.all, nil
	}
	return c.Store.GetAll(ctx, projectID, limit)
}

func (c *capStore) GetByIDs(ctx context.Context, ids []string) ([]memory.Memory, error) {
	var out []memory.Memory
	for _, id := range ids {
		if m, ok := c.byID[id]; ok {
			out = append(out, m)
		}
	}
	if len(out) > 0 {
		return out, nil
	}
	return c.Store.GetByIDs(ctx, ids)
}

func TestSelectCandidatesFetchesNeighborOutsideGetAllCap(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	newer := add(t, store, db, "postgres upgraded to 16", []float32{1, 0, 0}, "2026-07-10 00:00:00")
	older := add(t, store, db, "postgres runs version 14", []float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")

	cs := newCapStore(t, store, 1) // only newer in GetAll results
	sel, err := SelectCandidates(ctx, cs, "p", 0.9)
	if err != nil {
		t.Fatalf("SelectCandidates: %v", err)
	}
	if len(sel.Candidates) != 1 {
		t.Fatalf("want exactly 1 candidate (neighbor outside GetAll cap should be fetched), got %d: %+v", len(sel.Candidates), sel.Candidates)
	}
	if sel.Candidates[0].NewerID != newer || sel.Candidates[0].OlderID != older {
		t.Errorf("wrong orientation")
	}
}
