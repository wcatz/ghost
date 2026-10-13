package linking

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// editDuringScanStore is a real *memory.Store whose SearchVectorScoped edits
// the memory being scanned before answering, which is the window an edit can
// land in: the worker has already read the old vector and is comparing it
// with its neighbours. reembed additionally stores the new text's vector
// before the scan finishes, the other order the two writers can land in.
type editDuringScanStore struct {
	*memory.Store
	t       *testing.T
	target  string
	oldVec  []float32
	newVec  []float32
	reembed bool
	edited  bool
}

func (s *editDuringScanStore) SearchVectorScoped(ctx context.Context, projectID string, queryVec []float32, limit int, scope map[string]string) ([]memory.ScoredMemory, error) {
	// Only M's own scan triggers the edit: a neighbour scanned first would
	// otherwise edit M before M's scan started, and M would then be scanned
	// (correctly) with its new vector.
	if !s.edited && slices.Equal(queryVec, s.oldVec) {
		s.edited = true
		content := "rewritten text the old vector was never computed from"
		if err := s.UpdateMemory(ctx, testProject, s.target, &content, nil, nil, nil); err != nil {
			s.t.Fatalf("UpdateMemory: %v", err)
		}
		if s.reembed {
			if err := s.StoreEmbedding(ctx, s.target, s.newVec, "test"); err != nil {
				s.t.Fatalf("StoreEmbedding(new): %v", err)
			}
		}
	}
	return s.Store.SearchVectorScoped(ctx, projectID, queryVec, limit, scope)
}

// TestSweepOnceEditDuringScanLeavesMemoryQueued: a scan computed from the old
// vector says nothing about the neighbours of the new text, so an edit that
// lands while the scan is in flight must leave the memory in the link queue
// once its new vector is stored — in either order of the scan's completion
// and the re-embed. Recording the stale scan would keep only the edges of the
// old text, and nothing would ever scan the new one.
func TestSweepOnceEditDuringScanLeavesMemoryQueued(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reembed bool // true: the new vector is stored before the scan finishes
	}{
		{"reembed after scan finishes", false},
		{"reembed before scan finishes", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			oldVec := []float32{1, 0, 0}
			m := addEmbedded(t, s, "original text", oldVec)
			_ = addEmbedded(t, s, "a neighbour of the original", []float32{1, 0.1, 0})
			newVec := []float32{0, 1, 0}

			wrapped := &editDuringScanStore{Store: s, t: t, target: m, oldVec: oldVec, newVec: newVec, reembed: tc.reembed}
			w := NewWorker(wrapped, slog.New(slog.NewTextHandler(io.Discard, nil)), 0, 0.8)
			w.SweepOnce(ctx)
			if !wrapped.edited {
				t.Fatal("the sweep never searched for the memory's neighbours; the fixture did not exercise the race")
			}

			if !tc.reembed {
				if err := s.StoreEmbedding(ctx, m, newVec, "test"); err != nil {
					t.Fatalf("StoreEmbedding(new): %v", err)
				}
			}

			ids, err := s.UnscannedEmbeddedMemoryIDs(ctx, testProject, 50)
			if err != nil {
				t.Fatalf("UnscannedEmbeddedMemoryIDs: %v", err)
			}
			for _, id := range ids {
				if id == m {
					return
				}
			}
			t.Errorf("memory %s edited during its link scan is not queued for a scan of its new text; unscanned = %v", m, ids)
		})
	}
}

// TestSweepOnceRecordsScanOfUnchangedMemory is the control for the test above:
// with no edit, the vector the worker compared is still the stored one, so the
// scan is recorded and the memory leaves the queue. A non-unit vector keeps the
// comparison honest — the stored bytes must round-trip exactly through the
// worker's read for any scan to be recorded at all.
func TestSweepOnceRecordsScanOfUnchangedMemory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	m := addEmbedded(t, s, "text nobody edits", []float32{0.3, -1.7, 2.25})

	w := NewWorker(s, slog.New(slog.NewTextHandler(io.Discard, nil)), 0, 0.8)
	w.SweepOnce(ctx)

	ids, err := s.UnscannedEmbeddedMemoryIDs(ctx, testProject, 50)
	if err != nil {
		t.Fatalf("UnscannedEmbeddedMemoryIDs: %v", err)
	}
	for _, id := range ids {
		if id == m {
			t.Fatalf("memory %s is still queued after a sweep scanned its unchanged vector", m)
		}
	}
}
