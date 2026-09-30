package mcpserver

// #646: the server warms the retrieval key at construction, and this is the test
// that pins the CALL rather than the capability.
//
// The key's own tests prove that a warmed key is served from memory, and that is
// a property of the key's cache — which any caching implementation satisfies. It
// says nothing about whether the SERVER ever warms it, so a no-op
// WarmQueryKey, or deleting the call from New(), would leave every one of those
// tests green while the search path went back to resolving a key from the
// filesystem on its first call. The placement is the fix, so the placement is what
// has to be asserted.

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/provider"
)

// warmingStore is a store that can be warmed, and counts whether it was.
type warmingStore struct {
	provider.MemoryStore
	warm    atomic.Int32
	warmErr error
}

func (s *warmingStore) WarmQueryKey() error {
	s.warm.Add(1)
	return s.warmErr
}

// A warming store that is not also a RecordSink must NOT be warmed: the two
// capabilities are separate on purpose, and a provider that can warm but not
// record is not a shape the server has.
type warmOnlyStore struct {
	provider.MemoryStore
	warm atomic.Int32
}

func (s *warmOnlyStore) WarmQueryKey() error {
	s.warm.Add(1)
	return nil
}

// recordingStore satisfies the whole retrieval seam, so a server built on it both
// warms and records.
type recordingStore struct {
	provider.MemoryStore
	set   *memory.CandidateSet
	warm  atomic.Int32
	diges atomic.Int32
}

func (s *recordingStore) Candidates(context.Context, memory.CandidateRequest) (*memory.CandidateSet, error) {
	return s.set, nil
}

func (s *recordingStore) RecordRetrieval(context.Context, memory.RetrievalRecord) error { return nil }

func (s *recordingStore) DigestQuery(string) (string, error) {
	s.diges.Add(1)
	return "", nil
}

// WarmQueryKey makes this store a full participant: it can record AND it can be
// warmed, which is the shape *memory.Store has. Without this method the server
// correctly finds no warmer and the test fails — which is what happened the first
// time this was written, and is the reason the fixture is explicit.
func (s *recordingStore) WarmQueryKey() error {
	s.warm.Add(1)
	return nil
}

// TestNewWarmsTheRetrievalKeyItWillLaterRecordWith: the server resolves the key at
// construction, so no search pays for it.
func TestNewWarmsTheRetrievalKeyItWillLaterRecordWith(t *testing.T) {
	store := &recordingStore{}
	New(store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test")

	if got := store.warm.Load(); got != 1 {
		t.Errorf("New() warmed the retrieval key %d times, want 1 — a search that resolves the key itself "+
			"pays a data-directory lookup, a read and a create on the first call of every process", got)
	}
}

// TestANonRecordingStoreIsStillServed: the record seam is optional, so the server
// must build and serve for a provider that has neither capability.
//
// New() has no path that returns nil, so the claim is about what it BUILT: the
// store it holds is the one it was handed, and it invented no warmer. A provider
// that cannot be audited must still answer a search — which is the whole reason
// the capability is an assertion rather than a requirement.
func TestANonRecordingStoreIsStillServed(t *testing.T) {
	// A store with the candidate seam and neither retrieval capability.
	var plain provider.MemoryStore = onlyCandidates{set: &memory.CandidateSet{}}
	if _, ok := any(plain).(queryKeyWarmer); ok {
		t.Fatal("the fixture is a store WITH the warmer, so this test proves nothing")
	}

	srv := New(plain, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test")

	if srv.store != plain {
		t.Error("the server did not keep the store it was handed")
	}
	if _, ok := any(srv.store).(queryKeyWarmer); ok {
		t.Error("the server invented a warmer its store does not have")
	}
	if _, ok := any(srv.store).(assemble.RecordSink); ok {
		t.Error("the fixture is supposed to be a store WITHOUT the record sink")
	}
}

// onlyCandidates is a store with the candidate seam and nothing else, so a server
// built on it has neither the record sink nor the warmer.
type onlyCandidates struct {
	provider.MemoryStore
	set *memory.CandidateSet
}

func (o onlyCandidates) Candidates(context.Context, memory.CandidateRequest) (*memory.CandidateSet, error) {
	return o.set, nil
}

// TestAStoreThatCannotWarmIsStillBuiltAndSaysSo: a failed warm is a warning, not
// a refused server. A store whose records cannot be grouped by question is a
// degraded audit; a server that will not start is an outage.
func TestAStoreThatCannotWarmIsStillBuiltAndSaysSo(t *testing.T) {
	store := &warmingStore{warmErr: context.DeadlineExceeded}
	srv := New(store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test")
	if srv == nil {
		t.Fatal("New() returned no server for a store whose key could not be warmed")
	}
	if got := store.warm.Load(); got != 1 {
		t.Errorf("New() tried to warm %d times, want 1", got)
	}
}

// compile-time reminder that the seam the server warms through is the one the
// store satisfies, so a change to either is caught here rather than at a search.
var _ queryKeyWarmer = (*memory.Store)(nil)
var _ assemble.RecordSink = (*memory.Store)(nil)
var _ = warmOnlyStore{}
