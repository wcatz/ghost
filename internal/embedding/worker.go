package embedding

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
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
	// StoreEmbeddingForContent stores the vector only if the memory's content is
	// still the text it was computed from, and reports whether it did.
	StoreEmbeddingForContent(ctx context.Context, memoryID string, vec []float32, model, content string) (bool, error)
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

	// mu guards inconclusive, which the sweep carries between intervals, and
	// slowRows.
	mu sync.Mutex
	// inconclusive counts consecutive SWEEPS that did not answer. It is the
	// evidence a single probe cannot be: see inconclusiveStampsAfter.
	inconclusive int

	// slowRows holds the ids of memories whose embed timed out, so later
	// batches try them last (see EmbedPending). In memory only and bounded by
	// maxSlowRows; guarded by mu.
	slowRows map[string]struct{}
}

// maxSlowRows bounds Worker.slowRows. Past it a newly timed-out row is not
// recorded, which only means it is tried in the store's order again.
const maxSlowRows = 1024

// stallAfterTimeouts is how many consecutive embed timeouts in one batch are
// read as a stalled endpoint rather than slow rows. One is not enough: the
// client's timeout firing on one over-long document looks exactly like a
// stall, and that row, never embedded, comes back first on every pass. Two
// costs a pacified endpoint one extra request per batch.
const stallAfterTimeouts = 2

// isSlowRow reports whether id timed out in an earlier batch.
func (w *Worker) isSlowRow(id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.slowRows[id]
	return ok
}

// noteSlowRow records that id's embed timed out.
func (w *Worker) noteSlowRow(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.slowRows == nil {
		w.slowRows = make(map[string]struct{})
	}
	if len(w.slowRows) < maxSlowRows {
		w.slowRows[id] = struct{}{}
	}
}

// forgetSlowRow drops id once it has been embedded.
func (w *Worker) forgetSlowRow(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.slowRows, id)
}

// slowRowsLast reorders ids so the rows that timed out before come after every
// other row, keeping the store's order within each group.
func (w *Worker) slowRowsLast(ids []string) []string {
	out := make([]string, 0, len(ids))
	var slow []string
	for _, id := range ids {
		if w.isSlowRow(id) {
			slow = append(slow, id)
			continue
		}
		out = append(out, id)
	}
	return append(out, slow...)
}

// inconclusiveStampsAfter is how many consecutive sweep TICKS must fail to
// answer before the down-since marker is written for an endpoint that is not
// REFUSING connections.
//
// One observation is not enough, and the asymmetry is the point: a probe that
// missed its own 2s deadline is far more often a busy machine than a dead
// endpoint, and a marker written on the first of those is reported by
// `ghost mcp status` for as long as the machine stays busy. But a marker written
// only on a conclusive refusal is a regression in the other direction (#287's
// diagnostic), because the endpoints that need it least are the ones that answer
// fastest: a wedged accept loop, a remote or tunnelled URL whose packets a
// firewall drops, and a host that has stopped scheduling all look like "the
// request did not come back", never like a refusal. Those then report "Ollama
// unreachable" with no duration beside it, forever.
//
// So the marker follows the SIGNAL rather than a single probe: consecutive ticks
// that all went unanswered is a pattern, and any tick that answers SPENDS the
// streak (see SweepOnce), so a stall spread over minutes cannot accumulate into
// an outage that was never established.
//
// It counts TICKs, and that is the whole difficulty — the count is advanced once
// per `SweepOnce` call, never per probe, because `SweepOnce` probes at the top
// AND `EmbedPending` re-probes after every failed embed, so a wedged endpoint
// can produce a dozen probes inside one tick. Counting probes would satisfy the
// threshold within a single tick and stamp the marker on exactly the busy
// machine the streak exists to protect against.
//
// A tick counts whether or not the sweep gets PAST its gate, and that is what
// keeps the duration reachable for a hung endpoint at all: `SweepOnce` gates
// strictly and skips every project, but it still probes once and still calls
// noteSweep, so the clock runs for as long as the hang lasts. Three ticks is
// three worker intervals, so at the daemon's 2 minutes (cmd/ghost/mcp.go) the
// shortest hang this reports a duration for is about six minutes: the shortest
// real outage worth a duration line, against a busy machine never getting one.
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
// Reachable removes the marker. An inconclusive probe never does: not answering
// is not evidence the outage is over.
//
// A blank dataDir (set by callers that don't care about the marker) disables
// this bookkeeping and behaves exactly like calling client.Probe(ctx)
// directly. Best-effort: a failure to write or remove the marker is logged at
// debug level and never changes the reported reachability.
func (w *Worker) checkAlive(ctx context.Context) Reachability {
	got := w.client.Probe(ctx)
	if w.dataDir == "" {
		return got
	}

	// Only the CONCLUSIVE answers, and only where the marker can exist at all.
	// The blank-dataDir early return above is what confines this to the daemon:
	// `embedSupersedeCorpus` builds its worker with "" precisely because the
	// marker is the daemon's outage bookkeeping, and a one-shot pass has no
	// outage to keep time, so no `ghost resolve` or `ghost supersede` run can
	// reach this. Within the daemon a refusal needs no corroboration — it is an
	// answer — so it stamps on the spot rather than waiting for a sweep. Reachable
	// removes it. An inconclusive probe does neither: it cannot end a real
	// outage, and turning a stall into one is what the streak exists to prevent,
	// so see noteSweep.
	markerPath := filepath.Join(w.dataDir, OllamaDownMarkerFilename)
	switch got {
	case Reachable:
		if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
			w.logger.Debug("embed: remove ollama-down marker", "error", err)
		}
	case Unreachable:
		w.writeDownMarker(markerPath)
	}
	return got
}

// writeDownMarker stamps the down-since file if it is not already there, so a
// still-down endpoint does not reset its own clock on every observation. The
// write is idempotent for the same reason: the marker's value is when the
// endpoint went away, and re-stamping it on every poll would reset that to
// "now" for as long as the outage lasts.
func (w *Worker) writeDownMarker(markerPath string) {
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		return
	}
	if err := os.WriteFile(markerPath, []byte(time.Now().UTC().Format(time.RFC3339)), 0o600); err != nil {
		w.logger.Debug("embed: write ollama-down marker", "error", err)
	}
}

// noteSweep records ONE scheduled observation of the endpoint and stamps the
// marker once the streak it produces is long enough. It is separate from
// checkAlive because COUNTING has to happen where the clock is, and the clock is
// the sweep interval — not the probe.
//
// The split is the point. `checkAlive` runs from three places and more than once
// per tick: `SweepOnce` at the top, and inside `EmbedPending`'s loop after every
// failed embed, so a wedged endpoint can produce a dozen probes in one tick. A
// counter advanced there reaches its threshold within a single tick, and the
// busy machine the streak exists to protect against gets the outage marker
// anyway. So the streak is per SWEEP, and this is the only caller that has one.
func (w *Worker) noteSweep(got Reachability) {
	// Counted whether or not there is a marker path, so a worker built without
	// one behaves identically on this axis rather than keeping state nothing
	// reads.
	w.mu.Lock()
	if got == Inconclusive {
		w.inconclusive++
	} else {
		w.inconclusive = 0
	}
	streak := w.inconclusive
	w.mu.Unlock()

	if w.dataDir == "" || got != Inconclusive || streak < inconclusiveStampsAfter {
		return
	}
	w.writeDownMarker(filepath.Join(w.dataDir, OllamaDownMarkerFilename))
}

// withoutEvidence turns a probe's verdict into one a caller may ACT on when
// nothing was attempted behind it. A refusal stands — it is an answer about the
// endpoint, whatever the work — and an inconclusive does not, because it is
// evidence of nothing and would cost a sweep every remaining project.
func withoutEvidence(got Reachability) Reachability {
	if got == Inconclusive {
		return Reachable
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
			w.logger.Error("panic in embedding processProject, recovered", "panic", r, "project_id", memory.RefusalArg("project_id", projectID))
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

// SweepOnce embeds unembedded memories across all projects. Its probe is the
// worker's SCHEDULED observation of the endpoint, so it is the one that feeds
// the down-since streak (noteSweep) — see inconclusiveStampsAfter for why the
// count is per sweep and not per probe.
//
// Its GATE is strict, on anything but Reachable, and that is the opposite of
// EmbedPending's on purpose. The two callers of the same body want different
// things from an unanswered probe. A one-shot pass (`ghost supersede`) and a
// save-driven processProject have no second chance: skipping there is what made
// #736 report a clean "0 candidate pairs" for a pair the operator could see. The
// daemon has a second chance two minutes later, and its own probe has already
// told it everything it needs to know.
//
// The cost is why this cannot simply inherit the lenient gate. A hung endpoint —
// accepts connections, never answers — would otherwise be walked through every
// project, and each project pays its own probe plus up to 30s for an
// EmbedDocument on the client's blanket timeout: 2 + 2·P + 32·P_pending seconds
// for a sweep that exists to make vectors. Worse, SweepOnce runs INSIDE
// Worker.Run's select loop and projectCh is buffered at 16 with non-blocking
// sends, so a sweep taking half a minute per project silently drops the save
// notifications the worker exists to service. The daemon's answer to a busy
// machine is to skip this tick, not to spend it.
//
// A "pacified" endpoint — one that answers GET / but stalls /api/embed — costs
// stallAfterTimeouts stalled embed requests per sweep, not that many per
// project: EmbedPending stops its batch on that many consecutive timeouts and
// reports Inconclusive, as it does for a batch that spent embedPendingBudget
// without embedding anything, and the check below ends the sweep on that. One
// slow row is not a stall: the batch skips it, tries it last from then on, and
// the sweep goes on. The remaining projects wait for the next
// tick. A batch that spent its budget while still embedding reports Reachable,
// so a slow but working endpoint costs at most one budget per project and the
// sweep reaches every project.
//
// The streak is still counted, so a hung endpoint still accrues the outage clock
// noteSweep maintains: the strictness changes what the sweep DOES, not what it
// concludes about the endpoint.
func (w *Worker) SweepOnce(ctx context.Context) {
	got := w.checkAlive(ctx)
	w.noteSweep(got)
	if got != Reachable {
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
		// Belt and braces for an endpoint that answered at the top of this sweep
		// and has not answered since. The batch's own re-probe is the evidence —
		// a batch really did attempt an embed — and it is the only thing that can
		// produce one, because a gate probe with no work behind it collapses to
		// "nothing to act on" (see EmbedPending's second return). So we stop here
		// rather than starting the next project against an endpoint that is about
		// to cost another 30s. This project's work is finished; the rest waits for
		// the next tick.
		//
		// The line says what was DECIDED and names the verdict rather than
		// diagnosing the endpoint, because the two are not the same event: an
		// Unreachable means nothing is listening, an Inconclusive means this
		// machine did not get an answer in time, which is a reason to look at
		// load. Calling both "stopped answering" would make the second look like
		// the first, which is the misreading this whole change is about.
		if _, last := w.processProject(ctx, p.ID); last != Reachable {
			w.logger.Info("embed: endpoint did not answer; ending this sweep here",
				"project_id", memory.RefusalArg("project_id", p.ID), "reachability", last.String(), "next_attempt", "next tick")
			return
		}
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

	stored, err := w.store.StoreEmbeddingForContent(ctx, memoryID, vec, w.client.Identity(), content)
	if err != nil {
		w.logger.Error("embed: store", "error", err, "memory_id", memoryID)
		return
	}
	if !stored {
		w.logger.Debug("embed: content changed while embedding, left pending", "memory_id", memoryID)
	}
}

// projectBatch is how many memories one project sweep embeds. Bounded because
// a sweep runs unattended over a whole store, and a model change can leave every
// row in a project unembedded at once.
const projectBatch = 50

// embedPendingBudget is the per-batch time budget for EmbedPending. Like
// supersedeEmbedBudget, it bounds time even when the row count bound would
// allow many attempts against a stalled endpoint.
//
// A var for the reason aliveProbeTimeout is one: a test hands it milliseconds
// rather than sitting out the shipped minute.
var embedPendingBudget = time.Minute

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
// "0 candidate pairs" for a pair the operator could see. The cost of trying is
// bounded and small — one request, capped by the client's own timeout — so the
// two errors are not symmetric and this is the one worth making.
//
// The batch is bounded in time as well as rows (embedPendingBudget, in the
// spirit of supersedeEmbedBudget). The budget is the deadline of every embed
// request in the batch, so a batch never runs past it by more than the store
// calls around one embed; when it is spent the batch stops and logs "embed
// budget spent", reporting Inconclusive if it embedded nothing and Reachable if
// it made progress (see budgetSpent).
//
// An embed that times out while the liveness re-check is still Reachable is
// either one slow row (the client's timeout on an over-long document) or the
// "pacified endpoint" shape where the server answers GET / but hangs
// /api/embed. The row is recorded on the Worker as slow, so this and every
// later batch try it after every other row, and the batch moves on. It stops
// and reports Inconclusive only on stallAfterTimeouts consecutive timeouts, or
// when it ends on a timeout having embedded nothing; a row already recorded as
// slow does not count toward either, so one row that always times out cannot
// end every sweep. Ordinary per-row errors (a 400 for an over-long or
// malformed input, a model error for that one row) reset the count and
// continue to the next row, so one bad memory does not block the rest.
// The caller's own context ending stops the batch without either line.
//
// `SweepOnce` gates STRICTLY on the same probe, for the cost reason its own
// comment gives. This leniency is for the callers with no second chance, and a
// caller that has one should keep it.
//
// The second return is the verdict `SweepOnce` may ACT ON, and it is not quite
// "the endpoint's last answer", because a probe with nothing behind it is not
// evidence. The in-batch re-probe's verdict is reported as itself: a batch
// actually attempted an embed, so an endpoint that then stopped answering HAS said
// something. A batch stopped as a stalled endpoint, or by a budget spent without
// embedding anything, reports Inconclusive although the re-probe answered: the
// endpoint did not do the work in time, and a Reachable there would send the
// sweep's next project into the same wait. A budget spent while embedding
// reports Reachable, so the sweep moves on. On the two no-work paths — the unembedded list failed, or there was
// nothing to embed — a CONCLUSIVE verdict is still reported (a refusal stops a
// sweep whatever the work behind it) and an INCONCLUSIVE one collapses to
// Reachable, because `client.go` says a busy machine reaches that deadline
// routinely and a sweep breaking on it would skip every remaining project for a
// tick, over a probe it was going to make anyway.
//
// So Reachable there means "nothing to act on", not "the endpoint answered", and
// the one caller that reads this value says exactly that. Reporting the raw
// Inconclusive instead is the cheaper-looking mistake: it costs one tick of the
// remaining projects on every busy machine, which is the failure this whole
// change exists to stop.
func (w *Worker) EmbedPending(ctx context.Context, projectID string, limit int) (int, Reachability) {
	// Check whether Ollama is really not there, which is a different question
	// from whether it answered quickly (see the doc comment above).
	got := w.checkAlive(ctx)
	if got == Unreachable {
		return 0, got
	}
	if limit <= 0 {
		limit = projectBatch
	}

	// Asking for this client's own identity is what makes a model change
	// self-healing: rows whose vector was written by another model come back
	// here and are re-embedded in bounded batches, off the startup path.
	ids, err := w.store.UnembeddedMemoryIDs(ctx, projectID, w.client.Identity(), limit)
	if err != nil {
		w.logger.Error("embed: list unembedded", "error", err, "project_id", memory.RefusalArg("project_id", projectID))
		return 0, withoutEvidence(got)
	}

	if len(ids) == 0 {
		return 0, withoutEvidence(got)
	}

	w.logger.Info("embedding memories", "project_id", memory.RefusalArg("project_id", projectID), "count", len(ids))

	embedded, failed := 0, 0
	var lastErr error
	// The verdict as of the most recent evidence: the gate's answer until an
	// embed proves the endpoint is answering, and whatever the re-probe found
	// after that.
	got = Reachable
	budgetCtx, budgetCancel := context.WithTimeout(ctx, embedPendingBudget)
	defer budgetCancel()
	// timeouts counts consecutive embed timeouts on rows not already known to
	// be slow; stallAfterTimeouts of them is a stalled endpoint.
	timeouts := 0
	for _, id := range w.slowRowsLast(ids) {
		if ctx.Err() != nil {
			return embedded, got
		}
		if budgetCtx.Err() != nil {
			return embedded, w.budgetSpent(embedded)
		}

		content, err := w.store.GetMemoryContent(ctx, id)
		if err != nil {
			w.logger.Debug("embed: get content", "error", err, "memory_id", id)
			continue
		}

		vec, err := w.client.EmbedDocument(budgetCtx, content)
		if err != nil {
			failed++
			lastErr = err
			w.logger.Debug("embed: ollama", "error", err, "memory_id", id)
			// The caller's own context ending is a shutdown or the caller's
			// budget, not news about the endpoint: return without a verdict line.
			if ctx.Err() != nil {
				return embedded, got
			}
			// This batch's budget ending is checked before the re-probe, so a
			// spent budget is not overshot by another probe, and before the stall
			// test below, which the same deadline error would otherwise match.
			if budgetCtx.Err() != nil {
				return embedded, w.budgetSpent(embedded)
			}
			// If Ollama went down mid-batch, stop. This one takes anything but
			// Reachable, which is the opposite of the gate above and deliberately
			// so: the batch is already part-done, so a second request at a
			// machine that is not answering is 30s each for nothing, and a stall
			// is as good a reason to stop as an outage.
			if got = w.checkAlive(ctx); got != Reachable {
				// The verdict is named because the two are not the same event and
				// this line is where a reader works out which one happened: an
				// outage is the endpoint's, an inconclusive is this machine's, and
				// the second is a reason to look at load rather than at Ollama.
				w.logger.Info("ollama unavailable, pausing embedding",
					"embedded", embedded, "reachability", got.String())
				return embedded, got
			}
			// A timeout on an embed whose liveness re-check is still Reachable is
			// either one slow row (the client's timeout firing on an over-long
			// document) or an endpoint that answers / but stalls /api/embed (a
			// "pacified" endpoint), and one timeout cannot tell them apart. So the
			// row is recorded as slow, to be tried last from now on, and the batch
			// moves on; stallAfterTimeouts in a row is the stall, which stops the
			// batch and reports Inconclusive so SweepOnce ends the sweep instead
			// of sending the next project into it. A row already known to be slow
			// is not counted: its timeout is that row's, and counting it would let
			// one such row end every sweep. Ordinary per-row errors (a 400 for an
			// over-long or malformed input, a model error for that one row) prove
			// the endpoint answered, reset the count and continue.
			var urlErr *url.Error
			if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &urlErr) && urlErr.Timeout()) {
				if w.isSlowRow(id) {
					continue
				}
				w.noteSlowRow(id)
				timeouts++
				if timeouts >= stallAfterTimeouts {
					return embedded, w.embedStalled(embedded, id)
				}
				continue
			}
			timeouts = 0
			continue
		}
		timeouts = 0

		stored, err := w.store.StoreEmbeddingForContent(ctx, id, vec, w.client.Identity(), content)
		if err != nil {
			w.logger.Error("embed: store", "error", err, "memory_id", id)
			continue
		}
		w.forgetSlowRow(id)
		if !stored {
			// An edit landed while the endpoint was computing: the vector is the
			// old text's, so it is dropped and the row stays pending (#1159).
			w.logger.Debug("embed: content changed while embedding, left pending", "memory_id", id)
			continue
		}
		embedded++
	}

	// A batch that ended on a timeout and embedded nothing had no next row to
	// tell a slow row from a stall, and nothing it did shows the endpoint
	// working: read it as a stall. Only a row not known to be slow counts, so
	// a project whose one pending row always times out ends the sweep once, not
	// on every tick.
	if timeouts > 0 && embedded == 0 {
		return embedded, w.embedStalled(embedded, "")
	}

	// Surface persistent embed failures once per sweep — a missing Ollama
	// model otherwise fails silently at debug level forever.
	if failed > 0 {
		w.logger.Warn("embedding failures this sweep — check `ghost mcp status`",
			"project_id", memory.RefusalArg("project_id", projectID), "failed", failed, "embedded", embedded, "last_error", lastErr)
	}

	if embedded > 0 {
		w.logger.Info("embedding batch complete", "project_id", memory.RefusalArg("project_id", projectID), "embedded", embedded)
	}
	return embedded, got
}

// embedStalled logs a batch stopped on a stalled endpoint and returns the
// verdict it reports: Inconclusive, which ends SweepOnce's sweep.
func (w *Worker) embedStalled(embedded int, lastID string) Reachability {
	w.logger.Info("embed stalled, pausing batch",
		"embedded", embedded, "memory_id", lastID, "reachability", Inconclusive.String())
	return Inconclusive
}

// budgetSpent logs a batch that ran out of embedPendingBudget and returns the
// verdict it reports. Running out is not an outage, so the line says what
// happened rather than reusing the "ollama unavailable" one.
//
// The verdict depends on whether the batch made progress. A batch that
// embedded nothing in its whole budget reports Inconclusive, which ends
// SweepOnce's sweep: the next project would be waiting on the same endpoint. A
// batch that embedded rows reports Reachable: the endpoint is slow but working,
// the budget has already bounded this project, and ending the sweep there would
// hold every later project behind the first one's backlog for as many ticks as
// that backlog takes to drain.
func (w *Worker) budgetSpent(embedded int) Reachability {
	verdict := Reachable
	if embedded == 0 {
		verdict = Inconclusive
	}
	w.logger.Info("embed budget spent, pausing batch",
		"embedded", embedded, "budget", embedPendingBudget.String(), "reachability", verdict.String())
	return verdict
}

// processProject is the sweep's per-project body, and it passes the endpoint's
// last answer back so SweepOnce can act on it.
func (w *Worker) processProject(ctx context.Context, projectID string) (int, Reachability) {
	return w.EmbedPending(ctx, projectID, projectBatch)
}
