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

// TestProbe_DistinguishesAnOutageFromASlowMachine is the unit-level statement
// of the same distinction TestEmbedPending_EmbedsWhenTheLivenessProbeDoesNotAnswer
// makes at the worker: the three answers come from three different situations,
// and only one of them is "there is no Ollama".
//
// The three stubs are the three situations, and none of them is a fake of the
// others: a refused connection (nothing listening), a non-200 (something
// listening, not Ollama), and a wedge the probe's own deadline expires on. The
// last is what a busy machine looks like, so `Inconclusive` has to be the answer
// for it — a false Unreachable is a caller told to skip work it could do.
func TestProbe_DistinguishesAnOutageFromASlowMachine(t *testing.T) {
	// Refused: nothing is listening. localhost:0 is the same shape the
	// unreachable-client tests use above, and it fails at connect rather than
	// waiting, so the probe's own 2s is not what ends it.
	if got := NewClient("http://localhost:0", "m", 3).Probe(context.Background()); got != Unreachable {
		t.Errorf("Probe at a refused port = %v, want Unreachable", got)
	}

	nonOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not ollama", http.StatusInternalServerError)
	}))
	defer nonOK.Close()
	if got := NewClient(nonOK.URL, "m", 3).Probe(context.Background()); got != Unreachable {
		t.Errorf("Probe at a non-200 = %v, want Unreachable", got)
	}

	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()
	if got := NewClient(ok.URL, "m", 3).Probe(context.Background()); got != Reachable {
		t.Errorf("Probe at a 200 = %v, want Reachable", got)
	}

	// The wedge. Released when the probe's own deadline has passed, so the test
	// costs one probe and not the client's 30s embed timeout.
	release := make(chan struct{})
	wedged := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer wedged.Close()
	defer close(release)
	if got := NewClient(wedged.URL, "m", 3).Probe(context.Background()); got != Inconclusive {
		t.Errorf("Probe at an endpoint that misses the deadline = %v, want Inconclusive", got)
	}

	// And Alive, the two-valued form the diagnostic callers keep, reads a stall
	// as not-reachable — the conservative direction, since a health line that
	// says "unreachable" during a stall costs a human nothing and a work-gating
	// caller everything.
	if NewClient(wedged.URL, "m", 3).Alive(context.Background()) {
		t.Error("Alive = true at an endpoint that misses the deadline, want false")
	}
}

// wedgedEndpoint is an endpoint that accepts the connection and never answers,
// which is what a hung Ollama and a firewall-dropped remote URL both look like.
// Released when the test ends, so a probe that gives up on its own deadline does
// not wedge the fake (the same reason
// TestEmbedSupersedeCorpusStopsAtItsBudget releases rather than waits on the
// request context).
func wedgedEndpoint(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})
	return srv
}

// TestCheckAlive_StampsTheMarkerOnARepeatedlyInconclusiveProbe is the second
// half of the tri-state's contract, and the review finding that found it: a
// single inconclusive probe must not start an outage clock, but an endpoint that
// NEVER answers is the case #287's marker was built for, and a rule that never
// stamps on inconclusive loses the down-since duration for exactly that
// configuration — a wedged accept loop, a remote URL behind a dropping firewall
// — which reports "Ollama unreachable" forever with no duration beside it.
//
// So the marker follows the SIGNAL rather than a single probe: consecutive
// probes that did not answer, and not a momentary stall among them, are the
// evidence. The count is what separates the two, and this test is the difference
// between them — one probe writes nothing, and the third writes a real
// timestamp.
func TestCheckAlive_StampsTheMarkerOnARepeatedlyInconclusiveProbe(t *testing.T) {
	srv := wedgedEndpoint(t)
	dataDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	worker := NewWorker(NewClient(srv.URL, "nomic-embed-text", 3), newMockStore(), logger, time.Minute, dataDir)

	for probe := 1; probe < inconclusiveStampsAfter; probe++ {
		if got := worker.checkAlive(context.Background()); got != Inconclusive {
			t.Fatalf("probe %d = %v, want Inconclusive", probe, got)
		}
		if _, err := os.Stat(markerPath(dataDir)); !os.IsNotExist(err) {
			t.Fatalf("probe %d wrote the down-since marker for an endpoint that has not answered once: err=%v", probe, err)
		}
	}

	if got := worker.checkAlive(context.Background()); got != Inconclusive {
		t.Fatalf("probe %d = %v, want Inconclusive", inconclusiveStampsAfter, got)
	}
	since, err := os.ReadFile(markerPath(dataDir))
	if err != nil {
		t.Fatalf("an endpoint that never answered left no down-since marker after %d probes: %v", inconclusiveStampsAfter, err)
	}
	ts, err := time.Parse(time.RFC3339, string(since))
	if err != nil {
		t.Fatalf("marker %q is not RFC3339: %v", since, err)
	}
	if age := time.Since(ts); age < 0 || age > 10*time.Second {
		t.Errorf("marker timestamp %v is not close to now (age %v)", ts, age)
	}
}

// TestCheckAlive_LeavesTheMarkerAloneWhenTheProbeIsInconclusive is the other
// half: a probe that did not answer must neither START an outage clock (a
// marker written here is reported, with this timestamp, for as long as the
// machine stays busy) nor END a real one (the marker's own case is that it is
// written once and left).
func TestCheckAlive_LeavesTheMarkerAloneWhenTheProbeIsInconclusive(t *testing.T) {
	srv := wedgedEndpoint(t)

	// A real outage already on record. A stall must not clear it: the endpoint
	// has not been shown to be back.
	dataDir := t.TempDir()
	const real = "2020-01-01T00:00:00Z"
	if err := os.WriteFile(markerPath(dataDir), []byte(real), 0o600); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	worker := NewWorker(NewClient(srv.URL, "nomic-embed-text", 3), newMockStore(), logger, time.Minute, dataDir)

	if got := worker.checkAlive(context.Background()); got != Inconclusive {
		t.Fatalf("checkAlive = %v, want Inconclusive", got)
	}
	data, err := os.ReadFile(markerPath(dataDir))
	if err != nil {
		t.Fatalf("marker file missing after an inconclusive probe: %v", err)
	}
	if string(data) != real {
		t.Errorf("marker = %q, want unchanged %q: a probe that did not answer is not evidence the outage is over", data, real)
	}
}

// TestCheckAlive_ForgetsInconclusiveStreakOnAnAnsweredProbe is what keeps the
// streak a streak: a machine that stalls twice and then answers is a busy
// machine, and its two slow probes must not carry into the next outage's count,
// or a stall spread over a longer window is indistinguishable from a hang.
func TestCheckAlive_ForgetsInconclusiveStreakOnAnAnsweredProbe(t *testing.T) {
	wedge := wedgedEndpoint(t)
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()

	dataDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	worker := NewWorker(NewClient(wedge.URL, "nomic-embed-text", 3), newMockStore(), logger, time.Minute, dataDir)

	for probe := 1; probe < inconclusiveStampsAfter; probe++ {
		worker.checkAlive(context.Background())
	}
	// The endpoint comes back: the count is spent.
	worker.client = NewClient(ok.URL, "nomic-embed-text", 3)
	if got := worker.checkAlive(context.Background()); got != Reachable {
		t.Fatalf("checkAlive = %v, want Reachable once the endpoint answers", got)
	}
	// And it goes back to not answering: this is a FIRST inconclusive again.
	worker.client = NewClient(wedge.URL, "nomic-embed-text", 3)
	worker.checkAlive(context.Background())
	if _, err := os.Stat(markerPath(dataDir)); !os.IsNotExist(err) {
		t.Errorf("the marker was stamped %d probes into a SECOND stall, so the first stall's count carried over: err=%v",
			inconclusiveStampsAfter, err)
	}
}

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

// UnembeddedMemoryIDs mirrors the store's identity-aware selection: a row
// whose vector was stamped with another identity counts as unembedded, which is
// what makes a model change re-embed itself.
func (m *mockStore) UnembeddedMemoryIDs(_ context.Context, _ string, identity string, limit int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var ids []string
	for id := range m.memories {
		if model, hasEmb := m.embModel[id]; !hasEmb || (identity != "" && model != identity) {
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

func (p *panicOnceStore) UnembeddedMemoryIDs(ctx context.Context, projectID, identity string, limit int) ([]string, error) {
	if p.method == "UnembeddedMemoryIDs" && p.fired.CompareAndSwap(false, true) {
		panic("simulated panic in UnembeddedMemoryIDs")
	}
	return p.mockStore.UnembeddedMemoryIDs(ctx, projectID, identity, limit)
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
	if _, err := c.EmbedDocument(context.Background(), "test text"); err == nil {
		t.Error("EmbedDocument should return error for unreachable server")
	}
	if _, err := c.EmbedQuery(context.Background(), "test text"); err == nil {
		t.Error("EmbedQuery should return error for unreachable server")
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

	if got := worker.checkAlive(context.Background()); got != Unreachable {
		t.Fatalf("checkAlive = %v, want Unreachable for an unreachable client", got)
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

	if got := worker.checkAlive(context.Background()); got != Reachable {
		t.Fatalf("checkAlive = %v, want Reachable for a reachable client", got)
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

// TestEmbedPending_EmbedsWhenTheLivenessProbeDoesNotAnswer is the regression
// test for the load-shaped hole #736 found in the e2e supersede/resolve cases.
//
// The probe here answers /api/embed promptly and does not answer "/" inside the
// probe's own 2s deadline. That is not a broken endpoint: it is what a machine
// too busy to schedule a two-second-old HTTP round trip looks like, which is
// what "the full suite under load" is. The probe's own deadline is the only
// reason the answer is missing.
//
// It matters because a liveness probe is a GATE, and a gate that misreads a
// slow machine as a dead endpoint makes the caller skip work it can do:
// `ghost supersede` embeds the project's unvectorised memories itself before
// its candidate scan, precisely because those vectors are otherwise written by
// the embedding worker in another process (issue #716). That fill is the only
// thing standing between a pass started seconds after a save and a clean
// "0 candidate pairs" report for a pair the operator can see — and the daemon's
// own sweep stands in the same place, so a stall takes out both writers.
func TestEmbedPending_EmbedsWhenTheLivenessProbeDoesNotAnswer(t *testing.T) {
	// The release channel rather than r.Context(), for the reason
	// TestEmbedSupersedeCorpusStopsAtItsBudget gives: a handler that never reads
	// the body has no way to notice the client walking away, so waiting on the
	// request context here wedges the FAKE. Defers run last-in-first-out, so the
	// wedge is released before the server is closed.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			<-release
		case "/api/embed":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"embeddings":[[0.1,0.2,0.3]]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	defer close(release)

	dataDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store := newMockStore()
	store.memories["mem-1"] = "a memory the index does not have yet"
	client := NewClient(srv.URL, "test-model", 3)
	worker := NewWorker(client, store, logger, time.Minute, dataDir)

	if n := worker.EmbedPending(context.Background(), "proj-a", 10); n != 1 {
		t.Errorf("EmbedPending wrote %d vector(s), want 1: a probe that missed its own deadline is not evidence that nothing is listening, and the caller is left with a corpus it could have read", n)
	}
	// And the outage marker is the other half of the same misreading: a stall
	// stamped "ollama-down-since" is an outage `ghost mcp status` then reports
	// for as long as the machine stays busy.
	if _, err := os.Stat(markerPath(dataDir)); !os.IsNotExist(err) {
		t.Errorf("a probe that did not answer wrote the ollama-down marker: err=%v", err)
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

	if got := worker.checkAlive(context.Background()); got != Unreachable {
		t.Errorf("checkAlive = %v, want Unreachable for an unreachable client", got)
	}
	// No dataDir means no marker path to check — the assertion above not
	// panicking (no filepath.Join(\"\", ...) misuse causing a write to an
	// unexpected location) is the behavior under test.
}
