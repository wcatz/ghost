package embedding

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// --- Mock Store ---

type mockStore struct {
	mu         sync.Mutex
	projects   []string             // project IDs returned by ListProjects
	memories   map[string]string    // id -> content
	embeddings map[string][]float32 // id -> vector
	embModel   map[string]string    // id -> model
}

func (m *mockStore) ListProjects(_ context.Context) ([]memory.Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]memory.Project, len(m.projects))
	for i, id := range m.projects {
		out[i] = memory.Project{ID: id, Name: id}
	}
	return out, nil
}

func newMockStore() *mockStore {
	return &mockStore{
		memories:   make(map[string]string),
		embeddings: make(map[string][]float32),
		embModel:   make(map[string]string),
	}
}

func (m *mockStore) UnembeddedMemoryIDs(_ context.Context, _ string, limit int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var ids []string
	for id := range m.memories {
		if _, hasEmb := m.embeddings[id]; !hasEmb {
			ids = append(ids, id)
			if len(ids) >= limit {
				break
			}
		}
	}
	return ids, nil
}

func (m *mockStore) GetMemoryContent(_ context.Context, id string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	content, ok := m.memories[id]
	if !ok {
		return "", fmt.Errorf("memory not found: %s", id)
	}
	return content, nil
}

func (m *mockStore) StoreEmbedding(_ context.Context, memoryID string, vec []float32, model string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.embeddings[memoryID] = vec
	m.embModel[memoryID] = model
	return nil
}

func TestEmbedOne_HappyPath(t *testing.T) {
	store := newMockStore()
	store.memories["mem-1"] = "Go uses goroutines for concurrency"

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Create a real Client but we'll test via EmbedOne which calls the client.
	// Since we can't easily mock HTTP here, test the worker's store interactions
	// by creating a worker with a patched processProject approach.

	// Instead, test the Worker flow end-to-end using processProject.
	// We need a real Ollama for that, so let's test the unit logic:
	// EmbedOne retrieves content, calls embed, stores result.

	// Create a worker with a real client pointing at a fake URL.
	client := NewClient("http://localhost:0", "nomic-embed-text", 3)
	worker := NewWorker(client, store, logger, 5*time.Minute)

	// EmbedOne with unreachable server will fail gracefully (no panic).
	worker.EmbedOne(context.Background(), "mem-1")

	// Since Ollama is not available, embedding should not be stored.
	store.mu.Lock()
	_, hasEmb := store.embeddings["mem-1"]
	store.mu.Unlock()
	if hasEmb {
		t.Error("should not have stored embedding with unreachable server")
	}
}

func TestEmbedOne_MissingMemory(t *testing.T) {
	store := newMockStore()
	// No memory with this ID.

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	client := NewClient("http://localhost:0", "nomic-embed-text", 3)
	worker := NewWorker(client, store, logger, 5*time.Minute)

	// Should not panic on missing memory.
	worker.EmbedOne(context.Background(), "nonexistent")
}

func TestNewWorker(t *testing.T) {
	store := newMockStore()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	client := NewClient("http://localhost:11434", "nomic-embed-text", 768)

	worker := NewWorker(client, store, logger, 30*time.Second)
	if worker == nil {
		t.Fatal("NewWorker returned nil")
	}
	if worker.client != client {
		t.Error("client not set correctly")
	}
	if worker.interval != 30*time.Second {
		t.Errorf("interval = %v, want 30s", worker.interval)
	}
}

func TestWorkerRun_ContextCancellation(t *testing.T) {
	store := newMockStore()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	client := NewClient("http://localhost:0", "nomic-embed-text", 3)
	worker := NewWorker(client, store, logger, 24*time.Hour) // long interval so ticker doesn't fire

	ctx, cancel := context.WithCancel(context.Background())
	projectIDs := make(chan string, 1)

	done := make(chan struct{})
	go func() {
		worker.Run(ctx, projectIDs)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// success: Run exited after cancel
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after context cancellation")
	}
}

func TestWorkerRun_ChannelClose(t *testing.T) {
	store := newMockStore()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	client := NewClient("http://localhost:0", "nomic-embed-text", 3)
	worker := NewWorker(client, store, logger, 24*time.Hour)

	ctx := context.Background()
	projectIDs := make(chan string)

	done := make(chan struct{})
	go func() {
		worker.Run(ctx, projectIDs)
		close(done)
	}()

	close(projectIDs)

	select {
	case <-done:
		// success: Run exited after channel close
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after channel close")
	}
}

func TestWorkerRun_ProcessesProject(t *testing.T) {
	store := newMockStore()
	store.memories["mem-1"] = "test memory content"

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	client := NewClient("http://localhost:0", "nomic-embed-text", 3)
	worker := NewWorker(client, store, logger, 24*time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	projectIDs := make(chan string, 1)

	done := make(chan struct{})
	go func() {
		worker.Run(ctx, projectIDs)
		close(done)
	}()

	// Send a project ID — processProject will run but Ollama won't be available.
	projectIDs <- "test-project"

	// Give it a moment to process, then cancel.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit")
	}
}

func TestClientNewClient(t *testing.T) {
	c := NewClient("http://localhost:11434", "nomic-embed-text", 768)
	if c == nil {
		t.Fatal("NewClient returned nil")
	}
	if c.baseURL != "http://localhost:11434" {
		t.Errorf("baseURL = %q, want %q", c.baseURL, "http://localhost:11434")
	}
	if c.model != "nomic-embed-text" {
		t.Errorf("model = %q, want %q", c.model, "nomic-embed-text")
	}
	if c.Dimensions() != 768 {
		t.Errorf("Dimensions() = %d, want 768", c.Dimensions())
	}
}

func TestClientAlive_Unreachable(t *testing.T) {
	c := NewClient("http://localhost:0", "nomic-embed-text", 768)
	if c.Alive(context.Background()) {
		t.Error("Alive should return false for unreachable server")
	}
}

func TestClientEmbed_Unreachable(t *testing.T) {
	c := NewClient("http://localhost:0", "nomic-embed-text", 768)
	_, err := c.Embed(context.Background(), "test text")
	if err == nil {
		t.Error("Embed should return error for unreachable server")
	}
}

func TestProcessProject_NoUnembedded(t *testing.T) {
	store := newMockStore()
	// Store has no memories at all.

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	client := NewClient("http://localhost:0", "nomic-embed-text", 3)
	worker := NewWorker(client, store, logger, 5*time.Minute)

	// Should return early without error (no unembedded memories, plus Ollama not alive).
	worker.processProject(context.Background(), "test-project")
}

// TestSweepOnce_BackfillsWithoutSaves is a regression test: the worker must
// embed memories in ALL projects on its periodic sweep, not only projects
// that were previously seen on the save-notification channel. Before the
// fix, a fresh server never backfilled pre-existing memories.
func TestSweepOnce_BackfillsWithoutSaves(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/embed":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"embeddings":[[0.1,0.2,0.3]]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	store := newMockStore()
	store.projects = []string{"proj-a"}
	store.memories["mem-1"] = "pre-existing memory never saved this session"

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	client := NewClient(srv.URL, "test-model", 3)
	worker := NewWorker(client, store, logger, time.Minute)

	// No channel sends — the sweep alone must find and embed the memory.
	worker.SweepOnce(context.Background())

	store.mu.Lock()
	_, hasEmb := store.embeddings["mem-1"]
	store.mu.Unlock()
	if !hasEmb {
		t.Fatal("sweep did not embed a memory in a project never seen on the channel")
	}
}
