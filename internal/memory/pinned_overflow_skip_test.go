package memory

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestPinnedOverflowsExceptSkipsRows: a row the caller says session start
// withholds does not count against the cap, so a bucket one over the cap with
// one such row reports no overflow; without the skip it still does.
func TestPinnedOverflowsExceptSkipsRows(t *testing.T) {
	s := pinnedOverflowStore(t)
	ctx := context.Background()
	now := time.Now()
	for i := 0; i <= PinnedGlobalCap; i++ {
		createPinned(t, s, GlobalProjectID, fmt.Sprintf("g%03d", i), 0.7, 10)
	}
	all, err := s.PinnedOverflowsExcept(ctx, now, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Count != PinnedGlobalCap+1 {
		t.Fatalf("without a skip: %+v, want one overflow of %d", all, PinnedGlobalCap+1)
	}
	skipped, err := s.PinnedOverflowsExcept(ctx, now, nil, func(m Memory) bool { return m.ID == "g000" })
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 {
		t.Errorf("with one row skipped: %+v, want no overflow", skipped)
	}
}
