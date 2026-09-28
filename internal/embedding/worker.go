package embedding

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// memoryStore is the subset of provider.MemoryStore needed by the worker.
// UnembeddedMemoryIDs is asked for the worker's own identity, so a vector
// written by a previous model (or a previous task-prefix setting) comes back as
// unembedded and is rewritten — the worker needs no separate refresh path.
type memoryStore interface {
	ListProjects(ctx context.Context) ([]memory.Project, error)
	UnembeddedMemoryIDs(ctx context.Context, projectID, identity string, limit int) ([]string, error)
	GetMemoryContent(ctx context.Context, id string) (string, error)
	StoreEmbedding(ctx context.Context, memoryID string, vec []float32, model string) error
}

// OllamaDownMarkerFilename is the sentinel file checkAlive writes into
// dataDir the first time it observes Ollama unreachable, and removes once
// Ollama answers reachable again. `ghost mcp status` (internal/mcpinit) reads
// it to report outage duration (see issue #287). It's a plain file rather
// than a ghost_state database column because Ollama connectivity is a single
// global infra fact, not project-scoped data, and because `ghost mcp status`
// runs as a separate, short-lived CLI process from the long-lived MCP server
// that hosts this worker — an in-process-only counter here would be
// invisible to it, so the fact has to be persisted to disk. The file holds an
// RFC3339 UTC timestamp of when Ollama was first observed down.
const OllamaDownMarkerFilename = "ollama-down-since"

// Worker periodically embeds memories that don't yet have vectors.
type Worker struct {
	client   *Client
	store    memoryStore
	logger   *slog.Logger
	interval time.Duration
	dataDir  string

	// mu guards inconclusive, which checkAlive carries between probes.
	mu sync.Mutex
	// inconclusive counts consecutive probes that did not answer. It is the
	// evidence a single probe cannot be: see inconclusiveStampsAfter.
	inconclusive int
}

// inconclusiveStampsAfter is how many consecutive probes must fail to answer
// before the down-since marker is written for an endpoint that is not REFUSING
// connections.
//
// One probe is not enough, and the asymmetry is the point: a probe that missed
// its own 2s deadline is far more often a busy machine than a dead endpoint,
// and a marker written on the first of those is reported by `ghost mcp status`
// for as long as the machine stays busy. But a marker written only on a
// conclusive refusal is a regression in the other direction (#287's
// diagnostic), because the endpoints that need it least are the ones that
// answer fastest: a wedged accept loop, a remote or tunnelled URL whose packets
// a firewall drops, and a host that has stopped scheduling all look like "the
// request did not come back", never like a refusal. Those then report
// "Ollama unreachable" with no duration beside it, forever.
//
// So the marker follows the SIGNAL rather than the single probe: three
// unanswered probes in a row is a pattern, and a streak is spent by any
// answered probe (see checkAlive), so a stall spread over minutes cannot
// accumulate into an outage that was never established. Three is roughly two
// sweep intervals at the daemon's 2 minutes (cmd/ghost/mcp.go), so the
// shortest real outage this can report is about six minutes — which is the
// trade a duration line is worth, against a busy machine never reporting one.
const inconclusiveStampsAfter = 3

// NewWorker creates a background embedding worker. dataDir is the ghost data
// directory (config.DataDir()) where the Ollama-down marker file (see
// OllamaDownMarkerFilename and checkAlive) is written and removed as
// reachability changes; pass "" to disable that bookkeeping entirely (e.g.
// tests that don't exercise it, or a caller whose own config.DataDir() call
// failed).
func NewWorker(client *Client, store memoryStore, logger *slog.Logger, interval time.Duration, dataDir string) *Worker {
	return &Worker{
		client:   client,
		store:    store,
		logger:   logger,
		interval: interval,
		dataDir:  dataDir,
	}
}

// checkAlive reports whether Ollama is currently reachable and, as a side
// effect, maintains the on-disk down-since marker (OllamaDownMarkerFilename)
// that `ghost mcp status` reads to report outage duration, returning the
// tri-state the probe found.
//
// The marker is the reason this is not a bool, and it is written on EITHER of
// the two negative answers — but for different evidence, which is what
// inconclusiveStampsAfter is for. A conclusive Unreachable writes it at once
// (only if one doesn't already exist, so a still-down Ollama doesn't keep
// resetting its own "down since" clock on every poll). An Inconclusive writes it
// only once several have gone unanswered in a row, because a single one is a
// busy machine far more often than a dead endpoint, and the endpoints a refusal
// never describes — a wedged accept loop, a dropped remote URL — are exactly
// the ones that need the duration (see the constant for the full argument).
//
// Reachable removes it and SPENDS the streak, so a machine that stalls twice and
// then answers starts the next stall from zero, and a stall spread over minutes
// cannot accumulate into an outage that was never established. An inconclusive
// probe never removes it: not answering is not evidence the outage is over.
//
// A blank dataDir (set by callers that don't care about the marker) disables
// this bookkeeping and behaves exactly like calling client.Probe(ctx)
// directly. Best-effort: a failure to write or remove the marker is logged at
// debug level and never changes the reported reachability.
func (w *Worker) checkAlive(ctx context.Context) Reachability {
	got := w.client.Probe(ctx)

	// The streak is tracked whether or not there is a marker path, so a worker
	// built without one behaves identically on this axis rather than
	// accumulating a count nothing reads.
	w.mu.Lock()
	if got == Inconclusive {
		w.inconclusive++
	} else {
		w.inconclusive = 0
	}
	streak := w.inconclusive
	w.mu.Unlock()

	if w.dataDir == "" {
		return got
	}

	markerPath := filepath.Join(w.dataDir, OllamaDownMarkerFilename)
	switch {
	case got == Reachable:
		if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
			w.logger.Debug("embed: remove ollama-down marker", "error", err)
		}
	case got == Unreachable || streak >= inconclusiveStampsAfter:
		if _, err := os.Stat(markerPath); os.IsNotExist(err) {
			ts := time.Now().UTC().Format(time.RFC3339)
			if err := os.WriteFile(markerPath, []byte(ts), 0o600); err != nil {
				w.logger.Debug("embed: write ollama-down marker", "error", err)
			}
		}
	}
	return got
}

// Run starts the worker loop. Blocks until ctx is cancelled.
// Project IDs sent on the channel are processed immediately (new saves);
// the periodic sweep covers ALL projects so pre-existing memories backfill
// even when nothing is saved in the current session.
func (w *Worker) Run(ctx context.Context, projectIDs <-chan string) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case pid, ok := <-projectIDs:
			if !ok {
				return
			}
			w.safeProcessProject(ctx, pid)

		case <-ticker.C:
			w.safeSweepOnce(ctx)
		}
	}
}

// safeProcessProject wraps processProject with panic recovery. A panic here
// must not escape into Run's select loop: unwinding past Run itself would
// leave nothing left to fire the loop's next tick/message, so embedding
// would stop for good even after the panic is otherwise "handled". Recovering
// inside this small function means control returns to Run — still live —
// once this call completes, so the loop keeps going on the next iteration.
func (w *Worker) safeProcessProject(ctx context.Context, projectID string) {
	defer func() {
		if r := recover(); r != nil {
			w.logger.Error("panic in embedding processProject, recovered", "panic", r, "project_id", projectID)
		}
	}()
	w.processProject(ctx, projectID)
}

// safeSweepOnce wraps SweepOnce with panic recovery for the same reason as
// safeProcessProject above.
func (w *Worker) safeSweepOnce(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			w.logger.Error("panic in embedding sweep, recovered", "panic", r)
		}
	}()
	w.SweepOnce(ctx)
}

// SweepOnce embeds unembedded memories across all projects.
func (w *Worker) SweepOnce(ctx context.Context) {
	if w.checkAlive(ctx) == Unreachable {
		return
	}
	projects, err := w.store.ListProjects(ctx)
	if err != nil {
		w.logger.Error("embed: list projects", "error", err)
		return
	}
	for _, p := range projects {
		if ctx.Err() != nil {
			return
		}
		w.processProject(ctx, p.ID)
	}
}

// EmbedOne embeds a single memory immediately. Useful after Create/Upsert.
func (w *Worker) EmbedOne(ctx context.Context, memoryID string) {
	content, err := w.store.GetMemoryContent(ctx, memoryID)
	if err != nil {
		w.logger.Debug("embed: get content", "error", err, "memory_id", memoryID)
		return
	}

	vec, err := w.client.EmbedDocument(ctx, content)
	if err != nil {
		w.logger.Debug("embed: ollama", "error", err, "memory_id", memoryID)
		return
	}

	if err := w.store.StoreEmbedding(ctx, memoryID, vec, w.client.Identity()); err != nil {
		w.logger.Error("embed: store", "error", err, "memory_id", memoryID)
	}
}

// projectBatch is how many memories one project sweep embeds. Bounded because
// a sweep runs unattended over a whole store, and a model change can leave every
// row in a project unembedded at once.
const projectBatch = 50

// EmbedPending embeds up to limit of a project's memories that have no vector in
// this client's identity, and returns how many it wrote. It is the worker's
// per-project body, exported so a caller that must READ the vector index to do
// its job can fill it in the same process, synchronously, instead of waiting for
// a daemon that may never be running.
//
// That caller is `ghost supersede` (see cmd/ghost/lifecycle.go): its candidate
// scan proposes a pair by cosine, so a memory with no vector is not a candidate
// for anything, and the vectors are written by the worker inside `ghost mcp` —
// a different, long-lived process. A pass started seconds after a save would
// otherwise report an empty result for a corpus the user could see, with nothing
// anywhere saying the index had not caught up. The rule in supersede's
// SelectCandidates is right and stays; what changes is that the pass no longer
// has to guess whether the index is ready before it reads it.
//
// Best effort, exactly as the sweep is: an unreachable endpoint, a model that is
// not pulled, or a failed write leaves that memory unembedded, and the caller's
// own skip rule handles it — so nothing here is an error. The bound keeps a
// first pass after a model change from embedding a whole corpus in one go; the
// rest is the sweep's work, and the caller reports what it did not embed.
//
// The gate below is reached only on an ESTABLISHED outage. An Inconclusive probe
// — this machine being too busy to answer a two-second-old liveness check — is
// not an outage, and skipping the batch on it is what made the index depend on
// the machine's mood: `ghost supersede` calls this to fill the corpus it is
// about to scan (issue #716), so a stall here left it reporting a clean
// "0 candidate pairs" for a pair the operator could see, and the daemon's own
// sweep stopped filling the index for the same reason at the same moment. The
// cost of trying is bounded and small — one request, capped by the client's own
// timeout, and the loop below stops at the first failure — so the two errors are
// not symmetric and this is the one worth making.
func (w *Worker) EmbedPending(ctx context.Context, projectID string, limit int) int {
	// Check whether Ollama is really not there, which is a different question
	// from whether it answered quickly (see the doc comment above).
	if w.checkAlive(ctx) == Unreachable {
		return 0
	}
	if limit <= 0 {
		limit = projectBatch
	}

	// Asking for this client's own identity is what makes a model change
	// self-healing: rows whose vector was written by another model come back
	// here and are re-embedded in bounded batches, off the startup path.
	ids, err := w.store.UnembeddedMemoryIDs(ctx, projectID, w.client.Identity(), limit)
	if err != nil {
		w.logger.Error("embed: list unembedded", "error", err, "project_id", projectID)
		return 0
	}

	if len(ids) == 0 {
		return 0
	}

	w.logger.Info("embedding memories", "project_id", projectID, "count", len(ids))

	embedded, failed := 0, 0
	var lastErr error
	for _, id := range ids {
		if ctx.Err() != nil {
			return embedded
		}

		content, err := w.store.GetMemoryContent(ctx, id)
		if err != nil {
			w.logger.Debug("embed: get content", "error", err, "memory_id", id)
			continue
		}

		vec, err := w.client.EmbedDocument(ctx, content)
		if err != nil {
			failed++
			lastErr = err
			w.logger.Debug("embed: ollama", "error", err, "memory_id", id)
			// If Ollama went down mid-batch, stop. This one takes anything but
			// Reachable, which is the opposite of the gate above and deliberately
			// so: the batch is already part-done, so a second request at a
			// machine that is not answering is 30s each for nothing, and a stall
			// is as good a reason to stop as an outage.
			if got := w.checkAlive(ctx); got != Reachable {
				// The verdict is named because the two are not the same event and
				// this line is where a reader works out which one happened: an
				// outage is the endpoint's, an inconclusive is this machine's, and
				// the second is a reason to look at load rather than at Ollama.
				w.logger.Info("ollama unavailable, pausing embedding",
					"embedded", embedded, "reachability", got.String())
				return embedded
			}
			continue
		}

		if err := w.store.StoreEmbedding(ctx, id, vec, w.client.Identity()); err != nil {
			w.logger.Error("embed: store", "error", err, "memory_id", id)
			continue
		}
		embedded++
	}

	// Surface persistent embed failures once per sweep — a missing Ollama
	// model otherwise fails silently at debug level forever.
	if failed > 0 {
		w.logger.Warn("embedding failures this sweep — check `ghost mcp status`",
			"project_id", projectID, "failed", failed, "embedded", embedded, "last_error", lastErr)
	}

	if embedded > 0 {
		w.logger.Info("embedding batch complete", "project_id", projectID, "embedded", embedded)
	}
	return embedded
}

func (w *Worker) processProject(ctx context.Context, projectID string) {
	w.EmbedPending(ctx, projectID, projectBatch)
}
