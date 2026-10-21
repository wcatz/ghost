package mcpserver

// #1738: the health report's embedder probes are network calls, and they run
// outside the diagnostic read's lifetime.
//
// The read handle has ONE connection and it is shared with the retrieval
// snapshot, so a report that held it across a probe would make a concurrent
// search wait for the probe — up to seven seconds of it — for a connection the
// probe does not need.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wcatz/ghost/internal/memory"
)

// blockingEmbedder is an embedder whose liveness probe waits until the test
// releases it. The probe is the network call the report must not hold the
// read handle across.
type blockingEmbedder struct {
	entered     chan struct{}
	release     chan struct{}
	enterOnce   sync.Once
	releaseOnce sync.Once
}

func newBlockingEmbedder() *blockingEmbedder {
	return &blockingEmbedder{entered: make(chan struct{}), release: make(chan struct{})}
}

// Alive blocks until the test releases it, and then reports the backend as
// unreachable — the answer the report renders as "Ollama unreachable".
func (e *blockingEmbedder) Alive(ctx context.Context) bool {
	e.enterOnce.Do(func() { close(e.entered) })
	select {
	case <-e.release:
	case <-ctx.Done():
	}
	return false
}

func (e *blockingEmbedder) HasModel(ctx context.Context) (bool, error) { return false, nil }

func (e *blockingEmbedder) Model() string { return "test-embed-model" }

func (e *blockingEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	return nil, fmt.Errorf("the test embedder does not embed")
}

// unblock releases the probe.
func (e *blockingEmbedder) unblock() {
	e.releaseOnce.Do(func() { close(e.release) })
}

// plainEmbedder is the same embedder with the probe already answered: the
// report it produces is the report the blocked one must produce.
type plainEmbedder struct{ blockingEmbedder }

func (e *plainEmbedder) Alive(ctx context.Context) bool { return false }

// TestHealthDoesNotHoldTheReadHandleAcrossTheEmbedderProbe: a search's
// retrieval snapshot takes the SAME read handle the health report uses, so a
// report that held it across the embedder's probe would make that search wait
// for the probe.
func TestHealthDoesNotHoldTheReadHandleAcrossTheEmbedderProbe(t *testing.T) {
	srv, store, _, _ := diagnosticServer(t)
	emb := newBlockingEmbedder()
	srv.embedder = emb

	session := connectedClient(t, srv)
	// The probe is released BEFORE the session closes: closing the session
	// waits for the report that is inside the probe, so a cleanup that ran
	// after it would wait for a probe nobody releases.
	t.Cleanup(emb.unblock)
	type healthResult struct {
		text string
		err  error
	}
	done := make(chan healthResult, 1)
	go func() {
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "ghost_health"})
		if err != nil {
			done <- healthResult{err: err}
			return
		}
		text, ok := res.Content[0].(*mcp.TextContent)
		if !ok {
			done <- healthResult{err: fmt.Errorf("ghost_health returned %T, want text", res.Content[0])}
			return
		}
		done <- healthResult{text: text.Text}
	}()

	// The report is inside the embedder's probe.
	select {
	case <-emb.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the health report never reached the embedder probe")
	}

	// A search reads through the same handle, and it must not wait for the
	// probe.
	searched := make(chan error, 1)
	go func() {
		_, err := store.Candidates(context.Background(), memory.CandidateRequest{
			ProjectID: "abc123",
			Mode:      memory.ProjectScoped,
			Query:     "relay listens",
			Condition: memory.CondFTSOnly,
			Params:    memory.DefaultSearchParams(),
			Now:       time.Now().UTC(),
			Fetch:     memory.Fetch{FTSTopK: 10, VectorTopK: 10, Limit: 5},
		})
		searched <- err
	}()
	select {
	case err := <-searched:
		if err != nil {
			t.Fatalf("the search read failed while the health report probed the embedder: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a search read waited for the health report's embedder probe: the report held the read handle")
	}

	// The probe is released, and the report answers.
	emb.unblock()
	res := <-done
	if res.err != nil {
		t.Fatalf("ghost_health: %v", res.err)
	}
	if !strings.Contains(res.text, "Ollama unreachable") {
		t.Errorf("ghost_health does not report the embedder's probe result:\n%s", res.text)
	}

	// The report is the same report: the probe's answer is rendered where it
	// was, and nothing else moved.
	srv.embedder = &plainEmbedder{}
	again := ghostHealthText(t, srv)
	if again != res.text {
		t.Errorf("the report changed when the probe was moved out of the read's lifetime:\n--- blocked ---\n%s\n--- plain ---\n%s", res.text, again)
	}
}
