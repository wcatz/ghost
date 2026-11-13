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

// TestProcessProjectReembedsStaleIdentity: a row whose vector was written by
// another model counts as unembedded and is rewritten in the configured space
// with the document prefix; a row already in that space is left alone.
func TestProcessProjectReembedsStaleIdentity(t *testing.T) {
	for _, stale := range []bool{true, false} {
		rec := newEmbedRecorder(3)
		client := NewClient(rec.server(t).URL, "nomic-embed-text:v1.5", 3)
		store := newMockStore()
		store.projects = []string{"proj-a"}
		store.memories["mem-1"] = "a memory"
		store.embeddings["mem-1"] = []float32{9, 9, 9}
		store.embModel["mem-1"] = client.Identity()
		if stale {
			store.embModel["mem-1"] = "previous-model:768"
		}

		worker := NewWorker(client, store, discardLogger(), time.Minute, t.TempDir())
		worker.processProject(context.Background(), "proj-a")

		sent := rec.sent()
		if !stale {
			if len(sent) != 0 {
				t.Errorf("a current-identity vector was re-embedded anyway: %v", sent)
			}
			continue
		}
		store.mu.Lock()
		model, vec := store.embModel["mem-1"], store.embeddings["mem-1"]
		store.mu.Unlock()
		if model != client.Identity() || len(vec) != 3 || vec[0] == 9 {
			t.Errorf("stale vector not rewritten: identity %q vector %v", model, vec)
		}
		if len(sent) != 1 || sent[0] != NomicTaskDocument+"a memory" {
			t.Errorf("re-embed sent %q, want one document-prefixed request", sent)
		}
	}
}

// TestEmbedOneStampsClientIdentity: the single-memory path stamps the same
// identity as the batch path, or a fresh memory would look stale to the next sweep.
func TestEmbedOneStampsClientIdentity(t *testing.T) {
	rec := newEmbedRecorder(2)
	store := newMockStore()
	store.memories["mem-1"] = "just saved"
	client := NewClient(rec.server(t).URL, "nomic-embed-text:v1.5", 2)
	NewWorker(client, store, discardLogger(), time.Minute, t.TempDir()).EmbedOne(context.Background(), "mem-1")

	store.mu.Lock()
	model := store.embModel["mem-1"]
	store.mu.Unlock()
	if model != client.Identity() {
		t.Errorf("EmbedOne stored identity = %q, want %q", model, client.Identity())
	}
}
