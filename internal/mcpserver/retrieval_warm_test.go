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

// warmingStore can be warmed and CAN record, and counts whether it was — the
// shape a store whose key is momentarily unavailable arrives in, which is
// different from warmOnlyStore below and from a store that cannot record at all.
type warmingStore struct {
	provider.MemoryStore
	warm    atomic.Int32
	warmErr error
}

func (s *warmingStore) WarmQueryKey() error {
	s.warm.Add(1)
	return s.warmErr
}

func (s *warmingStore) Candidates(context.Context, memory.CandidateRequest) (*memory.CandidateSet, error) {
	return &memory.CandidateSet{}, nil
}

func (s *warmingStore) RecordRetrieval(context.Context, memory.RetrievalRecord) error { return nil }

func (s *warmingStore) DigestQuery(string) (string, error) { return "", nil }

// warmOnlyStore can warm but NOT record, which is a real shape for a provider
// whose search works and whose audit does not exist.
//
// New() must NOT warm it, and the reason is that warming CREATES the key file: a
// startup that resolved a per-install secret on behalf of a store that will never
// digest anything has written that store's secret to disk for no reader. The
// server asks "can this store record?" before it asks "can it warm?", so the two
// capabilities cannot be used to infer one another.
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

// TestAStoreThatCanWarmButNotRecordIsNotWarmed: the key is only created for a
// store that will use it.
func TestAStoreThatCanWarmButNotRecordIsNotWarmed(t *testing.T) {
	store := &warmOnlyStore{}
	if _, ok := any(store).(assemble.RecordSink); ok {
		t.Fatal("the fixture is a store WITH the record sink, so this test proves nothing")
	}
	if _, ok := any(store).(queryKeyWarmer); !ok {
		t.Fatal("the fixture is a store WITHOUT the warmer, so this test proves nothing")
	}

	New(store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test")

	if got := store.warm.Load(); got != 0 {
		t.Errorf("New() warmed a store that cannot record %d time(s) — that creates a per-install key file "+
			"for a store that will never digest a query", got)
	}
}
