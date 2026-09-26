package embedding

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestProcessProjectReembedsStaleIdentity is the worker half of a model
// change: a row whose vector was written by a previous model is reported as
// unembedded (the store decides that from the identity the worker asks for),
// so the sweep rewrites it in the configured space without any separate refresh
// path. The document task prefix is asserted on the same request, because a
// re-embed that skipped the prefix would land the row in yet another space.
func TestProcessProjectReembedsStaleIdentity(t *testing.T) {
	rec := newEmbedRecorder(3)
	store := newMockStore()
	store.projects = []string{"proj-a"}
	store.memories["mem-1"] = "a memory indexed by the previous model"
	store.embeddings["mem-1"] = []float32{9, 9, 9}
	store.embModel["mem-1"] = "previous-model:768"

	client := NewClient(rec.server(t).URL, "nomic-embed-text:v1.5", 3)
	worker := NewWorker(client, store, discardLogger(), time.Minute, t.TempDir())

	worker.processProject(context.Background(), "proj-a")

	store.mu.Lock()
	model, vec := store.embModel["mem-1"], store.embeddings["mem-1"]
	store.mu.Unlock()

	if model != client.Identity() {
		t.Errorf("stored identity = %q, want the client's %q: the stale vector was not rewritten", model, client.Identity())
	}
	if len(vec) != 3 || vec[0] == 9 {
		t.Errorf("stored vector = %v, want the freshly embedded one (the stale vector survived)", vec)
	}
	sent := rec.sent()
	if len(sent) != 1 {
		t.Fatalf("ollama saw %d embed calls, want 1: %v", len(sent), sent)
	}
	if want := NomicTaskDocument + "a memory indexed by the previous model"; sent[0] != want {
		t.Errorf("re-embed input = %q, want %q (documents are embedded with the document prefix)", sent[0], want)
	}
}

// TestProcessProjectLeavesCurrentIdentityAlone: the identity check must not
// re-embed rows that are already in the configured space, or every sweep would
// refetch every memory forever.
func TestProcessProjectLeavesCurrentIdentityAlone(t *testing.T) {
	rec := newEmbedRecorder(3)
	store := newMockStore()
	store.projects = []string{"proj-a"}
	client := NewClient(rec.server(t).URL, "nomic-embed-text:v1.5", 3)
	store.memories["mem-1"] = "already indexed"
	store.embeddings["mem-1"] = []float32{1, 1, 1}
	store.embModel["mem-1"] = client.Identity()

	worker := NewWorker(client, store, discardLogger(), time.Minute, t.TempDir())
	worker.processProject(context.Background(), "proj-a")

	if sent := rec.sent(); len(sent) != 0 {
		t.Errorf("ollama saw %d embed calls, want 0: a current-identity vector was re-embedded anyway (%v)", len(sent), sent)
	}
}

// TestEmbedOneStampsClientIdentity: the single-memory fast path (used right
// after a save) must stamp the same identity as the batch path, or a fresh
// memory would look stale to the next sweep.
func TestEmbedOneStampsClientIdentity(t *testing.T) {
	rec := newEmbedRecorder(2)
	store := newMockStore()
	store.memories["mem-1"] = "just saved"

	client := NewClient(rec.server(t).URL, "nomic-embed-text:v1.5", 2)
	worker := NewWorker(client, store, discardLogger(), time.Minute, t.TempDir())
	worker.EmbedOne(context.Background(), "mem-1")

	store.mu.Lock()
	model := store.embModel["mem-1"]
	store.mu.Unlock()
	if model != client.Identity() {
		t.Errorf("EmbedOne stored identity = %q, want %q", model, client.Identity())
	}
	if sent := rec.sent(); len(sent) != 1 || sent[0] != NomicTaskDocument+"just saved" {
		t.Errorf("EmbedOne sent %v, want one document-prefixed request", sent)
	}
}
