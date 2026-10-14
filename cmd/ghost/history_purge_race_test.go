package main

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// restoreAfterLiveReadStore stands in for a second process that, after
// `ghost history purge`'s reads on the pool found the memory deleted and before
// its purge, reinstates the memory (`ghost reflect --restore`) and, optionally,
// promotes it to _global.
type restoreAfterLiveReadStore struct {
	*memory.Store
	armed   atomic.Bool
	promote bool
	target  string
	hookErr error
}

func (s *restoreAfterLiveReadStore) GetByIDs(ctx context.Context, ids []string) ([]memory.Memory, error) {
	mems, err := s.Store.GetByIDs(ctx, ids)
	if err == nil && s.armed.CompareAndSwap(true, false) {
		if _, rerr := s.RestoreSnapshot(ctx, "p"); rerr != nil {
			s.hookErr = rerr
		} else if s.promote {
			s.hookErr = s.PromoteToGlobal(ctx, "p", s.target)
		}
	}
	return mems, err
}

// TestRunHistoryPurgeOfADeletedMemoryRefusesARowThatCameBack: the CLI's
// already-deleted purge decides "still deleted" inside the purge, like the MCP
// tool's. A memory restored (or restored and promoted) between the command's
// reads and its purge is refused, and its history and text survive.
func TestRunHistoryPurgeOfADeletedMemoryRefusesARowThatCameBack(t *testing.T) {
	const text = "the staging relay key sits in vault path ops/relay"
	for _, tc := range []struct {
		name, wantProject, wantRefusal string
		promote                        bool
	}{
		{"restored", "p", "live again", false},
		{"restored and promoted to _global", memory.GlobalProjectID, "not found in project", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := refTestStore(t)
			ctx := context.Background()
			id, _, _, err := s.Upsert(ctx, "p", "fact", text, "mcp", 0.5, nil)
			if err != nil {
				t.Fatalf("Upsert: %v", err)
			}
			if _, _, _, _, err := s.ReplaceNonManual(ctx, "p", []memory.Memory{{Category: "fact", Content: "a consolidated note about the relay", Importance: 0.5}}, "", nil); err != nil {
				t.Fatalf("ReplaceNonManual: %v", err)
			}
			if mems, _ := s.GetByIDs(ctx, []string{id}); len(mems) != 0 {
				t.Fatal("fixture: the reflection did not delete the memory")
			}
			w := &restoreAfterLiveReadStore{Store: s, promote: tc.promote, target: id}
			w.armed.Store(true)

			err = purgeHistoryMemory(ctx, w, id)
			if w.hookErr != nil {
				t.Fatalf("interleave: %v", w.hookErr)
			}
			if w.armed.Load() {
				t.Fatal("the interleave never ran")
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantRefusal) {
				t.Errorf("err = %v, want a refusal containing %q", err, tc.wantRefusal)
			}
			mems, gerr := s.GetByIDs(ctx, []string{id})
			if gerr != nil || len(mems) != 1 {
				t.Fatalf("the restored memory is gone after a refused purge: %v", gerr)
			}
			if mems[0].ProjectID != tc.wantProject || mems[0].Content != text {
				t.Errorf("row = project %q content %q, want %q %q", mems[0].ProjectID, mems[0].Content, tc.wantProject, text)
			}
			if entries, herr := s.MemoryHistory(ctx, id, 0); herr != nil || len(entries) == 0 {
				t.Errorf("a refused purge erased the history: %d rows, %v", len(entries), herr)
			}
		})
	}
}
