package mcpserver

import (
	"context"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func TestSearchCollapsesNearDuplicatesAtALimitOfTwo(t *testing.T) {
	srv, session := newCapSession(t)
	store, ok := srv.store.(*memory.Store)
	if !ok {
		t.Fatalf("test server store = %T, want *memory.Store", srv.store)
	}

	dupA := saveMem(t, session, "the cache warmer runs on the read replica every hour", nil)
	dupB := saveMem(t, session, "the cache warmer restocks the secondary database nightly", nil)
	distinct := saveMem(t, session,
		"unrelated to the pair beyond the shared noun: the cache warmer is one "+
			"scheduled job among many in this deployment, and the rest of this entry "+
			"exists to lengthen the row so the keyword ranking puts the two short "+
			"wordings above it in bm25, which is what makes the limit-of-two case "+
			"actually exercise the collapse rather than passing by accident", nil)
	if err := store.CreateLink(context.Background(), dupA, dupB, "related", 0.95, "auto"); err != nil {
		t.Fatalf("CreateLink(related): %v", err)
	}

	ids := listingIDs(t, session, "cache warmer", map[string]any{"limit": 2})
	if len(ids) != 2 {
		t.Fatalf("limit-2 search returned %d rows (%v), want the representative and the distinct row", len(ids), ids)
	}
	if !ids[distinct] {
		t.Errorf("limit-2 search omitted the distinct row; got %v, one of the pair and %s", ids, distinct)
	}
	if ids[dupA] == ids[dupB] {
		t.Errorf("limit-2 search returned %v, want exactly one of the near-duplicate pair", ids)
	}
}
