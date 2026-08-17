package embedding

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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

	// embedded, if non-nil, receives a memory ID each time StoreEmbedding
	// successfully stores it. Tests use this to detect — without sleeping —
	// that the worker loop is still alive and processing work.
	embedded chan string
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
	m.embeddings[memoryID] = vec
	m.embModel[memoryID] = model
	notify := m.embedded
	m.mu.Unlock()

	if notify != nil {
		select {
		case notify <- memoryID:
		default:
		}
	}
	return nil
}

// panicOnceStore wraps mockStore and panics exactly once — on the first
// invocation of the named method — before delegating to the real
// implementation on every call after that (including the retry of that same
// method). It exists to prove that Run's select loop survives a panic raised
// deep in one unit of work and keeps processing later ticks/messages, rather
// than merely proving recover() appears somewhere in the source.
type panicOnceStore struct {
	*mockStore
	method string // "ListProjects" or "UnembeddedMemoryIDs"
	fired  atomic.Bool
}

func (p *panicOnceStore) ListProjects(ctx context.Context) ([]memory.Project, error) {
	if p.method == "ListProjects" && p.fired.CompareAndSwap(false, true) {
		panic("simulated panic in ListProjects")
	}
	return p.mockStore.ListProjects(ctx)
}

func (p *panicOnceStore) UnembeddedMemoryIDs(ctx context.Context, projectID string, limit int) ([]string, error) {
	if p.method == "UnembeddedMemoryIDs" && p.fired.CompareAndSwap(false, true) {
		panic("simulated panic in UnembeddedMemoryIDs")
	}
	return p.mockStore.UnembeddedMemoryIDs(ctx, projectID, limit)
}

// embedOllamaStub returns an httptest server that answers Alive() checks at
// "/" and Embed() calls at "/api/embed", matching the real Ollama client's
// request shape (see TestSweepOnce_BackfillsWithoutSaves for the original
// use of this exact handler).
func embedOllamaStub() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
}

// TestWorkerRun_SurvivesPanicInTickerSweep proves the ticker branch of Run's
// select loop keeps firing after a panic inside SweepOnce (via ListProjects).
// Before the fix, the first tick's panic is unrecovered anywhere in the
// goroutine's call stack, which crashes the whole process; the fix must keep
// later ticks embedding normally.
func TestWorkerRun_SurvivesPanicInTickerSweep(t *testing.T) {
	srv := embedOllamaStub()
	defer srv.Close()

	base := newMockStore()
	base.projects = []string{"proj-a"}
	base.memories["mem-1"] = "pre-existing memory"
	base.embedded = make(chan string, 1)

	store := &panicOnceStore{mockStore: base, method: "ListProjects"}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	client := NewClient(srv.URL, "test-model", 3)
	worker := NewWorker(client, store, logger, 20*time.Millisecond, "")

	ctx, cancel := context.WithCancel(context.Background())
	projectIDs := make(chan string)

	done := make(chan struct{})
	go func() {
		worker.Run(ctx, projectIDs)
		close(done)
	}()

	select {
	case id := <-base.embedded:
		if id != "mem-1" {
			t.Fatalf("embedded %q, want mem-1", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not embed after a panic on the first sweep tick — ticker loop likely died")
	}

	cancel()
	<-done
}

// TestWorkerRun_SurvivesPanicInChannelProcessProject proves the projectIDs
// channel branch of Run's select loop keeps firing after a panic inside
// processProject (via UnembeddedMemoryIDs). The ticker interval is set to
// 24h so this test isolates the channel path from the sweep path.
func TestWorkerRun_SurvivesPanicInChannelProcessProject(t *testing.T) {
	srv := embedOllamaStub()
	defer srv.Close()

	base := newMockStore()
	base.memories["mem-1"] = "content triggered via save notification"
	base.embedded = make(chan string, 1)

	store := &panicOnceStore{mockStore: base, method: "UnembeddedMemoryIDs"}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	client := NewClient(srv.URL, "test-model", 3)
	worker := NewWorker(client, store, logger, 24*time.Hour, "") // ticker must not fire

	ctx, cancel := context.WithCancel(context.Background())
	projectIDs := make(chan string, 2)

	done := make(chan struct{})
	go func() {
		worker.Run(ctx, projectIDs)
		close(done)
	}()

	projectIDs <- "proj-a" // triggers the panic inside processProject
	projectIDs <- "proj-a" // must still be processed if the loop survived

	select {
	case id := <-base.embedded:
		if id != "mem-1" {
			t.Fatalf("embedded %q, want mem-1", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not embed after a panic on the first channel message — loop likely died")
	}

	cancel()
	<-done
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
	worker := NewWorker(client, store, logger, 5*time.Minute, t.TempDir())

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
	worker := NewWorker(client, store, logger, 5*time.Minute, t.TempDir())

	// Should not panic on missing memory.
	worker.EmbedOne(context.Background(), "nonexistent")
}

func TestNewWorker(t *testing.T) {
	store := newMockStore()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	client := NewClient("http://localhost:11434", "nomic-embed-text", 768)
	dataDir := t.TempDir()

	worker := NewWorker(client, store, logger, 30*time.Second, dataDir)
	if worker == nil {
		t.Fatal("NewWorker returned nil")
	}
	if worker.client != client {
		t.Error("client not set correctly")
	}
	if worker.interval != 30*time.Second {
		t.Errorf("interval = %v, want 30s", worker.interval)
	}
	if worker.dataDir != dataDir {
		t.Errorf("dataDir = %q, want %q", worker.dataDir, dataDir)
	}
}

func TestWorkerRun_ContextCancellation(t *testing.T) {
	store := newMockStore()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	client := NewClient("http://localhost:0", "nomic-embed-text", 3)
	worker := NewWorker(client, store, logger, 24*time.Hour, t.TempDir()) // long interval so ticker doesn't fire

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
	worker := NewWorker(client, store, logger, 24*time.Hour, t.TempDir())

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
	worker := NewWorker(client, store, logger, 24*time.Hour, t.TempDir())

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
	worker := NewWorker(client, store, logger, 5*time.Minute, t.TempDir())

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
	worker := NewWorker(client, store, logger, time.Minute, t.TempDir())

	// No channel sends — the sweep alone must find and embed the memory.
	worker.SweepOnce(context.Background())

	store.mu.Lock()
	_, hasEmb := store.embeddings["mem-1"]
	store.mu.Unlock()
	if !hasEmb {
		t.Fatal("sweep did not embed a memory in a project never seen on the channel")
	}
}

// markerPath returns the path checkAlive uses for the Ollama-down marker
// inside dataDir, mirroring the join in checkAlive itself.
func markerPath(dataDir string) string {
	return filepath.Join(dataDir, OllamaDownMarkerFilename)
}

// TestCheckAlive_WritesMarkerWhenDown verifies that observing Ollama
// unreachable for the first time writes the down-since marker file, with a
// parseable RFC3339 timestamp close to "now" — this is the signal `ghost mcp
// status` reads to report outage duration (issue #287).
func TestCheckAlive_WritesMarkerWhenDown(t *testing.T) {
	dataDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	client := NewClient("http://localhost:0", "nomic-embed-text", 3) // unreachable
	worker := NewWorker(client, newMockStore(), logger, time.Minute, dataDir)

	if alive := worker.checkAlive(context.Background()); alive {
		t.Fatal("checkAlive = true, want false for an unreachable client")
	}

	data, err := os.ReadFile(markerPath(dataDir))
	if err != nil {
		t.Fatalf("marker file was not created: %v", err)
	}
	since, err := time.Parse(time.RFC3339, string(data))
	if err != nil {
		t.Fatalf("marker file content %q is not a valid RFC3339 timestamp: %v", data, err)
	}
	if age := time.Since(since); age < 0 || age > 10*time.Second {
		t.Errorf("marker timestamp %v is not close to now (age %v)", since, age)
	}
}

// TestCheckAlive_RemovesMarkerWhenReachableAgain verifies that once Ollama
// answers alive, a previously-written down-since marker is removed so a
// resolved outage stops being reported.
func TestCheckAlive_RemovesMarkerWhenReachableAgain(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(markerPath(dataDir), []byte("2020-01-01T00:00:00Z"), 0o600); err != nil {
		t.Fatalf("seed marker file: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	client := NewClient(srv.URL, "nomic-embed-text", 3)
	worker := NewWorker(client, newMockStore(), logger, time.Minute, dataDir)

	if alive := worker.checkAlive(context.Background()); !alive {
		t.Fatal("checkAlive = false, want true for a reachable client")
	}

	if _, err := os.Stat(markerPath(dataDir)); !os.IsNotExist(err) {
		t.Errorf("marker file still present after Ollama became reachable: err=%v", err)
	}
}

// TestCheckAlive_DoesNotClobberExistingMarker is the regression test for the
// actual bug behind #287: if checkAlive rewrote the marker on every poll, a
// still-down Ollama would keep resetting its own "down since" clock and the
// reported duration would never grow past one poll interval. Observing "down"
// repeatedly must leave an existing marker's timestamp untouched. Exercises
// SweepOnce (one of the two real call sites named in the issue) rather than
// checkAlive directly, so the regression is pinned at the entry point the bug
// report describes.
func TestCheckAlive_DoesNotClobberExistingMarker(t *testing.T) {
	dataDir := t.TempDir()
	const backdated = "2020-01-01T00:00:00Z"
	if err := os.WriteFile(markerPath(dataDir), []byte(backdated), 0o600); err != nil {
		t.Fatalf("seed marker file: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	client := NewClient("http://localhost:0", "nomic-embed-text", 3) // unreachable
	store := newMockStore()
	worker := NewWorker(client, store, logger, time.Minute, dataDir)

	worker.SweepOnce(context.Background())

	data, err := os.ReadFile(markerPath(dataDir))
	if err != nil {
		t.Fatalf("marker file missing after sweep: %v", err)
	}
	if string(data) != backdated {
		t.Errorf("marker timestamp = %q, want unchanged %q (sweep clobbered it)", data, backdated)
	}
}

// TestCheckAlive_BlankDataDirDisablesMarker verifies that a Worker built with
// dataDir == "" (the fallback when config.DataDir() itself fails at startup —
// see cmd/ghost/main.go) behaves exactly like calling client.Alive directly,
// without attempting to write anywhere.
func TestCheckAlive_BlankDataDirDisablesMarker(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	client := NewClient("http://localhost:0", "nomic-embed-text", 3) // unreachable
	worker := NewWorker(client, newMockStore(), logger, time.Minute, "")

	if alive := worker.checkAlive(context.Background()); alive {
		t.Error("checkAlive = true, want false for an unreachable client")
	}
	// No dataDir means no marker path to check — the assertion above not
	// panicking (no filepath.Join(\"\", ...) misuse causing a write to an
	// unexpected location) is the behavior under test.
}
