package embedding

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// --- Mock store and fake endpoints ---

type mockStore struct {
	mu         sync.Mutex
	projects   []string
	memories   map[string]string    // id -> content
	embeddings map[string][]float32 // id -> vector
	embModel   map[string]string    // id -> identity
	// projectsOf, when non-nil, is id -> project and scopes UnembeddedMemoryIDs
	// the way the real store does.
	projectsOf map[string]string
	// embedded, if non-nil, receives a memory ID each time an embedding is stored.
	embedded chan string
}

func newMockStore() *mockStore {
	return &mockStore{
		memories:   make(map[string]string),
		embeddings: make(map[string][]float32),
		embModel:   make(map[string]string),
	}
}

// storeWith returns a store whose projects each hold perProject pending rows.
func storeWith(perProject int, projects ...string) *mockStore {
	s := newMockStore()
	s.projects = projects
	s.projectsOf = map[string]string{}
	for _, p := range projects {
		for i := 0; i < perProject; i++ {
			id := fmt.Sprintf("%s-mem-%d", p, i)
			s.memories[id] = "content " + id
			s.projectsOf[id] = p
		}
	}
	return s
}

func (m *mockStore) ListProjects(context.Context) ([]memory.Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]memory.Project, len(m.projects))
	for i, id := range m.projects {
		out[i] = memory.Project{ID: id, Name: id}
	}
	return out, nil
}

// UnembeddedMemoryIDs mirrors the store's identity-aware selection, sorted so a
// test can place one row first by its id.
func (m *mockStore) UnembeddedMemoryIDs(_ context.Context, projectID, identity string, limit int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for id := range m.memories {
		if m.projectsOf != nil && m.projectsOf[id] != projectID {
			continue
		}
		if model, ok := m.embModel[id]; !ok || (identity != "" && model != identity) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) > limit {
		ids = ids[:limit]
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

func (m *mockStore) StoreEmbeddingForContent(_ context.Context, memoryID string, vec []float32, model, content string) (bool, error) {
	m.mu.Lock()
	if m.memories[memoryID] != content {
		m.mu.Unlock()
		return false, nil
	}
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
	return true, nil
}

func (m *mockStore) hasEmbedding(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.embeddings[id]) > 0
}

// panicOnceStore panics once, on the first call of the named method.
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

func okProbe(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

func okEmbed(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"embeddings":[[0.1,0.2,0.3]]}`))
}

func refuseEmbed(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "embedding backend unavailable", http.StatusServiceUnavailable)
}

// newEndpoint serves the probe at "/" and the embed at /api/embed.
func newEndpoint(t *testing.T, probe, embed http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			probe(w, r)
		case "/api/embed":
			embed(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// wedgedEndpoint never answers the probe and refuses every embed at once; embeds
// reports how many embed requests it received.
func wedgedEndpoint(t *testing.T) (srv *httptest.Server, embeds func() int64) {
	t.Helper()
	release := make(chan struct{})
	var n atomic.Int64
	srv = newEndpoint(t, func(http.ResponseWriter, *http.Request) { <-release },
		func(w http.ResponseWriter, r *http.Request) { n.Add(1); refuseEmbed(w, r) })
	t.Cleanup(func() { close(release) })
	return srv, n.Load
}

// answerOnceThenWedge answers the first probe and wedges every later one: a busy
// machine from the sweep's point of view.
func answerOnceThenWedge(t *testing.T, embed http.HandlerFunc) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	var probes atomic.Int64
	srv := newEndpoint(t, func(w http.ResponseWriter, _ *http.Request) {
		if probes.Add(1) > 1 {
			<-release
		}
		w.WriteHeader(http.StatusOK)
	}, embed)
	t.Cleanup(func() { close(release) })
	return srv
}

// stalledEmbedEndpoint answers the probe at once and hangs every /api/embed:
// the "pacified" endpoint. It counts the embed requests it received.
func stalledEmbedEndpoint(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	release := make(chan struct{})
	embeds := new(atomic.Int64)
	srv := newEndpoint(t, okProbe, func(_ http.ResponseWriter, r *http.Request) {
		embeds.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })
	return srv, embeds
}

// slowRowEndpoint answers every embed at once except one whose input contains
// slowMarker (held until the request gives up) and one containing "bad row"
// (a 400). It records the request bodies in arrival order.
func slowRowEndpoint(t *testing.T, slowMarker string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var order []string
	srv := newEndpoint(t, okProbe, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		order = append(order, string(body))
		mu.Unlock()
		switch {
		case strings.Contains(string(body), slowMarker):
			<-r.Context().Done()
		case strings.Contains(string(body), "bad row"):
			http.Error(w, "bad request", http.StatusBadRequest)
		default:
			okEmbed(w, r)
		}
	})
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), order...)
	}
}

// Probe deadlines are package-global; the restore is registered on t, so a test
// that lowers it (even repeatedly) leaves the shipped value behind it.
const (
	wedgeProbeDeadline     = 20 * time.Millisecond  // a probe meant to miss
	answeringProbeDeadline = 500 * time.Millisecond // a probe that must arrive
)

func setProbeDeadline(t *testing.T, d time.Duration) {
	t.Helper()
	prev := aliveProbeTimeout
	aliveProbeTimeout = d
	t.Cleanup(func() { aliveProbeTimeout = prev })
}

func setEmbedPendingBudget(t *testing.T, d time.Duration) {
	t.Helper()
	prev := embedPendingBudget
	embedPendingBudget = d
	t.Cleanup(func() { embedPendingBudget = prev })
}

// syncBuffer is a bytes.Buffer safe for a logger and a reader on different goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func slogTo(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func markerPath(dataDir string) string { return filepath.Join(dataDir, OllamaDownMarkerFilename) }

func newTestWorker(url string, store memoryStore, dataDir string) *Worker {
	return NewWorker(NewClient(url, "test-model", 3), store, discardLogger(), time.Minute, dataDir)
}

// withShortTimeout bounds one embed request so a stalled row costs 120ms.
func withShortTimeout(w *Worker) *Worker {
	w.client.httpClient.Timeout = 120 * time.Millisecond
	return w
}

// --- Probe ---

// TestProbe_DistinguishesAnOutageFromASlowMachine: a refused connection and a
// non-200 are Unreachable, a 200 is Reachable, and a probe that misses its own
// deadline is Inconclusive (a busy machine, not "there is no Ollama").
func TestProbe_DistinguishesAnOutageFromASlowMachine(t *testing.T) {
	nonOK := newEndpoint(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "not ollama", 500) }, refuseEmbed)
	ok := newEndpoint(t, okProbe, refuseEmbed)
	for name, tc := range map[string]struct {
		url  string
		want Reachability
	}{
		"refused": {"http://localhost:0", Unreachable},
		"non-200": {nonOK.URL, Unreachable},
		"200":     {ok.URL, Reachable},
	} {
		if got := NewClient(tc.url, "m", 3).Probe(context.Background()); got != tc.want {
			t.Errorf("Probe at %s = %v, want %v", name, got, tc.want)
		}
	}

	wedged, _ := wedgedEndpoint(t)
	setProbeDeadline(t, wedgeProbeDeadline)
	if got := NewClient(wedged.URL, "m", 3).Probe(context.Background()); got != Inconclusive {
		t.Errorf("Probe at an endpoint that misses the deadline = %v, want Inconclusive", got)
	}
	// Alive, the two-valued form, reads a stall as not-reachable.
	if NewClient(wedged.URL, "m", 3).Alive(context.Background()) {
		t.Error("Alive = true at an endpoint that misses the deadline, want false")
	}
	if NewClient("http://localhost:0", "m", 3).Alive(context.Background()) {
		t.Error("Alive = true for an unreachable server")
	}
	if _, err := NewClient("http://localhost:0", "m", 3).EmbedDocument(context.Background(), "x"); err == nil {
		t.Error("EmbedDocument should fail for an unreachable server")
	}
}

// --- Sweep gating ---

// TestSweepOnce_AHungEndpointMakesNoEmbedAttempts: the daemon's strict gate must
// not walk a hung endpoint through every project (each paying the client's embed
// timeout), so a sweep makes zero embed attempts and costs about one probe.
func TestSweepOnce_AHungEndpointMakesNoEmbedAttempts(t *testing.T) {
	setProbeDeadline(t, wedgeProbeDeadline)
	srv, embeds := wedgedEndpoint(t)
	worker := newTestWorker(srv.URL, storeWith(1, "proj-a", "proj-b", "proj-c"), "")

	started := time.Now()
	worker.SweepOnce(context.Background())

	if n := embeds(); n != 0 {
		t.Errorf("a sweep against an endpoint that never answered made %d embed attempt(s), want 0", n)
	}
	if elapsed := time.Since(started); elapsed > 4*wedgeProbeDeadline {
		t.Errorf("sweep took %v, want roughly one probe deadline, not one per project", elapsed)
	}
}

// TestSweepOnce_StopsWhenTheEndpointDiesMidSweep: an endpoint that answers the
// sweep's own gate and then stops answering is noticed by the first project's
// batch, and the sweep does not walk on to the others.
func TestSweepOnce_StopsWhenTheEndpointDiesMidSweep(t *testing.T) {
	setProbeDeadline(t, answeringProbeDeadline)
	var embeds atomic.Int64
	srv := answerOnceThenWedge(t, func(w http.ResponseWriter, r *http.Request) { embeds.Add(1); refuseEmbed(w, r) })

	newTestWorker(srv.URL, storeWith(1, "proj-a", "proj-b", "proj-c"), "").SweepOnce(context.Background())

	if n := embeds.Load(); n > 1 {
		t.Errorf("sweep made %d embed attempts across 3 projects after the endpoint died; want at most the first project's", n)
	}
}

// TestSweepOnce_AProjectWithNoWorkDoesNotEndTheTick: an empty project whose
// probe is Inconclusive is not evidence the endpoint died, so later projects
// are still embedded.
func TestSweepOnce_AProjectWithNoWorkDoesNotEndTheTick(t *testing.T) {
	setProbeDeadline(t, answeringProbeDeadline)
	srv := answerOnceThenWedge(t, okEmbed)
	store := storeWith(1, "proj-a", "proj-b")
	store.embeddings["proj-a-mem-0"] = []float32{0.1, 0.2, 0.3}
	store.embModel["proj-a-mem-0"] = NewClient(srv.URL, "test-model", 3).Identity()

	newTestWorker(srv.URL, store, "").SweepOnce(context.Background())

	if !store.hasEmbedding("proj-b-mem-0") {
		t.Error("a project with nothing pending ended the tick: proj-b was never embedded")
	}
}

// TestSweepOnce_BackfillsWithoutSaves is a regression test: the periodic sweep
// embeds memories in all projects, not only ones seen on the save channel.
func TestSweepOnce_BackfillsWithoutSaves(t *testing.T) {
	srv := newEndpoint(t, okProbe, okEmbed)
	store := storeWith(1, "proj-a")
	newTestWorker(srv.URL, store, t.TempDir()).SweepOnce(context.Background())
	if !store.hasEmbedding("proj-a-mem-0") {
		t.Fatal("sweep did not embed a memory in a project never seen on the channel")
	}
}

// --- Down-since marker ---

// TestCheckAlive_Marker covers the ollama-down-since marker (#287): written on
// the first conclusive Unreachable, removed once reachable again, and never
// re-written (a still-down endpoint must not reset its own clock).
func TestCheckAlive_Marker(t *testing.T) {
	const backdated = "2020-01-01T00:00:00Z"
	ok := newEndpoint(t, okProbe, okEmbed)

	t.Run("written when down", func(t *testing.T) {
		dataDir := t.TempDir()
		w := newTestWorker("http://localhost:0", newMockStore(), dataDir)
		if got := w.checkAlive(context.Background()); got != Unreachable {
			t.Fatalf("checkAlive = %v, want Unreachable", got)
		}
		data, err := os.ReadFile(markerPath(dataDir))
		if err != nil {
			t.Fatalf("marker file was not created: %v", err)
		}
		since, err := time.Parse(time.RFC3339, string(data))
		if err != nil || time.Since(since) < 0 || time.Since(since) > 10*time.Second {
			t.Errorf("marker %q is not a timestamp close to now (err %v)", data, err)
		}
	})

	t.Run("removed when reachable", func(t *testing.T) {
		dataDir := t.TempDir()
		if err := os.WriteFile(markerPath(dataDir), []byte(backdated), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := newTestWorker(ok.URL, newMockStore(), dataDir).checkAlive(context.Background()); got != Reachable {
			t.Fatalf("checkAlive = %v, want Reachable", got)
		}
		if _, err := os.Stat(markerPath(dataDir)); !os.IsNotExist(err) {
			t.Errorf("marker still present after Ollama became reachable: err=%v", err)
		}
	})

	// Neither a conclusive outage nor an unanswered probe may touch a marker
	// already on record: the first must not reset the clock, the second is not
	// evidence the outage is over.
	t.Run("kept when still down", func(t *testing.T) {
		setProbeDeadline(t, wedgeProbeDeadline)
		wedged, _ := wedgedEndpoint(t)
		for name, url := range map[string]string{"unreachable": "http://localhost:0", "inconclusive": wedged.URL} {
			dataDir := t.TempDir()
			if err := os.WriteFile(markerPath(dataDir), []byte(backdated), 0o600); err != nil {
				t.Fatal(err)
			}
			w := newTestWorker(url, storeWith(0, "proj-a"), dataDir)
			for range inconclusiveStampsAfter - 1 {
				w.SweepOnce(context.Background())
				if data, err := os.ReadFile(markerPath(dataDir)); err != nil || string(data) != backdated {
					t.Fatalf("%s: marker = %q (err %v), want unchanged %q", name, data, err, backdated)
				}
			}
		}
	})
}

// TestCheckAlive_StampsTheMarkerOnARepeatedlyInconclusiveProbe: an endpoint that
// never answers still gets a down-since duration, but only after
// inconclusiveStampsAfter consecutive SWEEP TICKS, never within one tick (a
// tick probes 1 + len(projects) times, so a per-probe count would stamp a busy
// machine at once), and an answered sweep spends the streak.
func TestCheckAlive_StampsTheMarkerOnARepeatedlyInconclusiveProbe(t *testing.T) {
	wedge, _ := wedgedEndpoint(t)
	ok := newEndpoint(t, okProbe, okEmbed)
	setProbeDeadline(t, wedgeProbeDeadline)
	absent := func(dataDir string) bool { _, err := os.Stat(markerPath(dataDir)); return os.IsNotExist(err) }

	// One tick over three projects with pending rows probes past the threshold.
	oneTick := t.TempDir()
	newTestWorker(wedge.URL, storeWith(1, "proj-a", "proj-b", "proj-c"), oneTick).SweepOnce(context.Background())
	if !absent(oneTick) {
		t.Error("one sweep of an endpoint that never answered wrote the down-since marker")
	}

	// Ticks below the threshold write nothing; the threshold tick stamps now.
	dataDir := t.TempDir()
	w := newTestWorker(wedge.URL, storeWith(1, "proj-a", "proj-b", "proj-c"), dataDir)
	for sweep := 1; sweep < inconclusiveStampsAfter; sweep++ {
		w.SweepOnce(context.Background())
		if !absent(dataDir) {
			t.Fatalf("sweep %d wrote the marker for an endpoint that has not answered once", sweep)
		}
	}
	// An answered sweep spends the streak.
	setProbeDeadline(t, answeringProbeDeadline)
	w.client = NewClient(ok.URL, "test-model", 3)
	w.SweepOnce(context.Background())
	setProbeDeadline(t, wedgeProbeDeadline)
	w.client = NewClient(wedge.URL, "test-model", 3)
	w.SweepOnce(context.Background())
	if !absent(dataDir) {
		t.Fatal("the first stall's count carried over into a second stall")
	}
	for range inconclusiveStampsAfter - 1 {
		w.SweepOnce(context.Background())
	}
	since, err := os.ReadFile(markerPath(dataDir))
	if err != nil {
		t.Fatalf("an endpoint that never answered left no marker after %d sweeps: %v", inconclusiveStampsAfter, err)
	}
	if ts, err := time.Parse(time.RFC3339, string(since)); err != nil || time.Since(ts) > time.Minute {
		t.Errorf("marker %q is not a recent RFC3339 timestamp (err %v)", since, err)
	}
}

// --- Run loop ---

// startRun runs w.Run and returns a stop func that cancels it and waits for exit.
func startRun(t *testing.T, w *Worker, ch chan string) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx, ch); close(done) }()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Run did not exit")
		}
	}
}

// TestWorkerRun_SurvivesPanic: a panic raised deep in one unit of work (a
// ticker sweep, or a save notification) must not end Run's select loop.
func TestWorkerRun_SurvivesPanic(t *testing.T) {
	for _, tc := range []struct {
		name, method string
		interval     time.Duration
		notify       bool
	}{
		{"ticker sweep", "ListProjects", 20 * time.Millisecond, false},
		{"channel process", "UnembeddedMemoryIDs", 24 * time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newEndpoint(t, okProbe, okEmbed)
			base := storeWith(1, "proj-a")
			base.embedded = make(chan string, 1)
			w := NewWorker(NewClient(srv.URL, "test-model", 3), &panicOnceStore{mockStore: base, method: tc.method}, discardLogger(), tc.interval, "")
			ch := make(chan string, 2)
			stop := startRun(t, w, ch)
			defer stop()
			if tc.notify {
				ch <- "proj-a" // triggers the panic
				ch <- "proj-a" // must still be processed
			}
			select {
			case id := <-base.embedded:
				if id != "proj-a-mem-0" {
					t.Fatalf("embedded %q", id)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("worker did not embed after a panic: the loop likely died")
			}
		})
	}
}

func TestWorkerRun_Exits(t *testing.T) {
	t.Run("context cancellation", func(t *testing.T) {
		startRun(t, newTestWorker("http://localhost:0", newMockStore(), t.TempDir()), make(chan string, 1))()
	})
	t.Run("channel close", func(t *testing.T) {
		w := newTestWorker("http://localhost:0", newMockStore(), t.TempDir())
		ch := make(chan string)
		done := make(chan struct{})
		go func() { w.Run(context.Background(), ch); close(done) }()
		close(ch)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Run did not exit after channel close")
		}
	})
}

// --- EmbedPending: stalls, budget, slow rows ---

// TestEmbedPending_EmbedsWhenTheLivenessProbeDoesNotAnswer is the regression
// test for #736: a probe that misses its own deadline (a machine too busy to
// schedule a round trip) is not an outage, so the batch embeds anyway and writes
// no down-since marker.
func TestEmbedPending_EmbedsWhenTheLivenessProbeDoesNotAnswer(t *testing.T) {
	setProbeDeadline(t, wedgeProbeDeadline)
	release := make(chan struct{})
	srv := newEndpoint(t, func(http.ResponseWriter, *http.Request) { <-release }, okEmbed)
	t.Cleanup(func() { close(release) })

	dataDir := t.TempDir()
	store := newMockStore()
	store.memories["mem-1"] = "a memory the index does not have yet"
	if n, _ := newTestWorker(srv.URL, store, dataDir).EmbedPending(context.Background(), "proj-a", 10); n != 1 {
		t.Errorf("EmbedPending wrote %d vector(s), want 1", n)
	}
	if _, err := os.Stat(markerPath(dataDir)); !os.IsNotExist(err) {
		t.Errorf("a probe that did not answer wrote the ollama-down marker: err=%v", err)
	}
}

// TestEmbedPending_StopsAfterTheFirstStalledEmbed is the regression test for
// #1089: an endpoint that answers GET / but hangs /api/embed costs two
// consecutive timeouts, not one per remaining row, and the verdict is
// Inconclusive so the sweep (checked here too) does not start the next project.
func TestEmbedPending_StopsAfterTheFirstStalledEmbed(t *testing.T) {
	setProbeDeadline(t, answeringProbeDeadline)
	srv, embeds := stalledEmbedEndpoint(t)

	n, verdict := withShortTimeout(newTestWorker(srv.URL, storeWith(8, "proj-a"), t.TempDir())).EmbedPending(context.Background(), "proj-a", 0)
	if n != 0 || verdict != Inconclusive || embeds.Load() != 2 {
		t.Errorf("embedded %d, verdict %v, %d embed attempts; want 0, Inconclusive, 2", n, verdict, embeds.Load())
	}

	before := embeds.Load()
	withShortTimeout(newTestWorker(srv.URL, storeWith(3, "proj-a", "proj-b", "proj-c"), t.TempDir())).SweepOnce(context.Background())
	if got := embeds.Load() - before; got != 2 {
		t.Errorf("sweep made %d embed request(s) against a stalled endpoint, want 2 (the first project's stall ends it)", got)
	}
}

// TestEmbedPending_StopsAtItsBudget pins the time bound: the budget reaches the
// embed in flight, so a stalled endpoint costs the budget, not a client
// timeout, and the line logged says the budget was spent.
func TestEmbedPending_StopsAtItsBudget(t *testing.T) {
	setProbeDeadline(t, answeringProbeDeadline)
	setEmbedPendingBudget(t, 150*time.Millisecond)
	srv, embeds := stalledEmbedEndpoint(t)

	var logs syncBuffer
	w := newTestWorker(srv.URL, storeWith(4, "proj-a"), t.TempDir())
	w.logger = slogTo(&logs)
	w.client.httpClient.Timeout = 20 * time.Second // only the budget can end the request soon

	started := time.Now()
	n, verdict := w.EmbedPending(context.Background(), "proj-a", 0)

	if elapsed := time.Since(started); n != 0 || verdict != Inconclusive || embeds.Load() != 1 || elapsed > 5*time.Second {
		t.Errorf("embedded %d, verdict %v, %d attempts in %v; want 0, Inconclusive, 1, bounded by the 150ms budget", n, verdict, embeds.Load(), elapsed)
	}
	out := logs.String()
	if !strings.Contains(out, "embed budget spent") {
		t.Errorf("no budget line logged:\n%s", out)
	}
	for _, wrong := range []string{"ollama unavailable", "embed stalled"} {
		if strings.Contains(out, wrong) {
			t.Errorf("a spent budget logged %q:\n%s", wrong, out)
		}
	}
}

// TestSweepOnce_MovesOnAfterABudgetThatMadeProgress: a slow but healthy endpoint
// spends one project's budget while still embedding, which must not end the
// sweep or the first project holds every other one back.
func TestSweepOnce_MovesOnAfterABudgetThatMadeProgress(t *testing.T) {
	setProbeDeadline(t, answeringProbeDeadline)
	setEmbedPendingBudget(t, 250*time.Millisecond)
	var mu sync.Mutex
	requested := map[string]int{}
	srv := newEndpoint(t, okProbe, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		for _, p := range []string{"proj-a", "proj-b"} {
			if strings.Contains(string(body), p) {
				requested[p]++
			}
		}
		mu.Unlock()
		select {
		case <-time.After(60 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		okEmbed(w, r)
	})

	// proj-a holds far more rows than one budget can embed.
	store := storeWith(2, "proj-a", "proj-b")
	for i := 2; i < 40; i++ {
		id := fmt.Sprintf("proj-a-mem-%d", i)
		store.memories[id] = "content " + id
		store.projectsOf[id] = "proj-a"
	}
	newTestWorker(srv.URL, store, t.TempDir()).SweepOnce(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if requested["proj-a"] < 2 || requested["proj-a"] >= 40 {
		t.Fatalf("proj-a got %d embed request(s), want its budget to end the batch after some progress", requested["proj-a"])
	}
	if requested["proj-b"] < 1 {
		t.Error("proj-b got no embed request: a budget spent while embedding ended the sweep")
	}
}

// TestEmbedPending_ASlowRowDoesNotBlockTheRest: a row whose embed times out on
// every attempt is skipped (not a stalled endpoint), the rest are embedded, and
// the next batch tries it last.
func TestEmbedPending_ASlowRowDoesNotBlockTheRest(t *testing.T) {
	setProbeDeadline(t, answeringProbeDeadline)
	srv, requests := slowRowEndpoint(t, "slow row")
	store := newMockStore()
	store.memories["mem-0"] = "slow row" // sorts first, so first in every batch
	for i := 1; i <= 3; i++ {
		store.memories[fmt.Sprintf("mem-%d", i)] = fmt.Sprintf("content %d", i)
	}
	w := withShortTimeout(newTestWorker(srv.URL, store, t.TempDir()))

	if n, verdict := w.EmbedPending(context.Background(), "proj-a", 0); n != 3 || verdict != Reachable {
		t.Errorf("first batch embedded %d, verdict %v; want 3, Reachable", n, verdict)
	}
	store.memories["mem-4"], store.memories["mem-5"] = "content 4", "content 5"
	before := len(requests())
	if n, _ := w.EmbedPending(context.Background(), "proj-a", 0); n != 2 {
		t.Errorf("second batch embedded %d, want 2", n)
	}
	if second := requests()[before:]; len(second) == 0 || !strings.Contains(second[len(second)-1], "slow row") {
		t.Errorf("second batch did not try the slow row last: %q", second)
	}
}

// TestEmbedPending_ALoneTimedOutRowIsAStall: a batch whose only row times out
// reports Inconclusive; once that row is known slow its timeout is not a stall.
func TestEmbedPending_ALoneTimedOutRowIsAStall(t *testing.T) {
	setProbeDeadline(t, answeringProbeDeadline)
	srv, embeds := stalledEmbedEndpoint(t)
	w := withShortTimeout(newTestWorker(srv.URL, storeWith(1, "proj-a"), t.TempDir()))

	if _, verdict := w.EmbedPending(context.Background(), "proj-a", 0); verdict != Inconclusive {
		t.Errorf("verdict after a lone timed-out row = %v, want Inconclusive", verdict)
	}
	if _, verdict := w.EmbedPending(context.Background(), "proj-a", 0); verdict != Reachable {
		t.Errorf("verdict after the same, now known-slow, row = %v, want Reachable", verdict)
	}
	if got := embeds.Load(); got != 2 {
		t.Errorf("made %d embed attempt(s), want 2", got)
	}
}

// TestEmbedPending_AnAnsweredRowResetsTheTimeoutCount: two slow rows are not a
// stall when the endpoint answered a row between them, with a vector or a 400.
func TestEmbedPending_AnAnsweredRowResetsTheTimeoutCount(t *testing.T) {
	setProbeDeadline(t, answeringProbeDeadline)
	for _, between := range []string{"content fast", "bad row"} {
		t.Run(between, func(t *testing.T) {
			srv, requests := slowRowEndpoint(t, "slow row")
			store := newMockStore()
			store.memories["mem-0"] = "slow row one"
			store.memories["mem-1"] = between
			store.memories["mem-2"] = "slow row two"
			store.memories["mem-3"] = "content 3"

			_, verdict := withShortTimeout(newTestWorker(srv.URL, store, t.TempDir())).EmbedPending(context.Background(), "proj-a", 0)
			if verdict != Reachable || !store.hasEmbedding("mem-3") {
				t.Errorf("verdict %v, mem-3 embedded %v; want Reachable and true (the timeouts were not consecutive): %q",
					verdict, store.hasEmbedding("mem-3"), requests())
			}
		})
	}
}

// TestSweepOnce_ASingleSlowRowDoesNotStarveLaterProjects: a project whose only
// pending row always times out ends the first sweep, but the row is then known
// slow, so the next sweep reaches the later projects.
func TestSweepOnce_ASingleSlowRowDoesNotStarveLaterProjects(t *testing.T) {
	setProbeDeadline(t, answeringProbeDeadline)
	srv, _ := slowRowEndpoint(t, "slow row")
	store := storeWith(1, "proj-a", "proj-b")
	store.memories["proj-a-mem-0"] = "slow row"
	w := withShortTimeout(newTestWorker(srv.URL, store, t.TempDir()))

	w.SweepOnce(context.Background())
	w.SweepOnce(context.Background())

	if !store.hasEmbedding("proj-b-mem-0") {
		t.Error("proj-b was not embedded after two sweeps: proj-a's one slow row ends every sweep")
	}
}
