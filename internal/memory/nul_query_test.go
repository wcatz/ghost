package memory

import (
	"context"
	"testing"
)

// TestStoreSearchFTSHandlesNULByte: a NUL byte in a query must not break the
// keyword search leg. The memory below contains the tokens the NUL-separated
// query resolves to, so each reader must return it without an error.
func TestStoreSearchFTSHandlesNULByte(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, err := s.Create(ctx, testProject, Memory{
		Category:   "fact",
		Content:    "x a b y",
		Source:     "manual",
		Importance: 0.5,
		Tags:       []string{},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	for _, query := range []string{"a\x00b", "x a\x00b y"} {
		// SearchFTS
		res, err := s.SearchFTS(ctx, testProject, query, 10)
		if err != nil {
			t.Fatalf("SearchFTS(%q): %v", query, err)
		}
		if !containsMemory(res, id) {
			t.Fatalf("SearchFTS(%q): saved memory not found", query)
		}

		// SearchFTSAll
		res, err = s.SearchFTSAll(ctx, query, 10)
		if err != nil {
			t.Fatalf("SearchFTSAll(%q): %v", query, err)
		}
		if !containsMemory(res, id) {
			t.Fatalf("SearchFTSAll(%q): saved memory not found", query)
		}

		// SearchHybridAll (no embedding available in tests)
		res, err = s.SearchHybridAll(ctx, query, nil, 10)
		if err != nil {
			t.Fatalf("SearchHybridAll(%q): %v", query, err)
		}
		if !containsMemory(res, id) {
			t.Fatalf("SearchHybridAll(%q): saved memory not found", query)
		}
	}
}

func containsMemory(mems []Memory, id string) bool {
	for i := range mems {
		if mems[i].ID == id {
			return true
		}
	}
	return false
}
