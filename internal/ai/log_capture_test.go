package ai

import (
	"bytes"
	"log/slog"
	"sync"
	"testing"
)

// lockedBuffer is a bytes.Buffer that tolerates being written by one goroutine
// while another reads it, which a bare bytes.Buffer under a slog handler does
// not.
//
// It exists because of WHO writes here, not because of how much is written. A
// caller whose context is dead returns from codexFeaturesFor /
// claudeCapabilitiesFor without waiting for the probe it started — deliberately,
// because a caller that cannot act on a verdict must not block on one — and the
// goroutine singleflight ran that probe on keeps going. It reaches
// harnessCommand, and scratch.EnforceBudget warns there when it cannot measure
// the root, so the abandoned probe emits a slog.Warn from a goroutine the test
// has already stopped counting on. A test's t.TempDir teardown is what makes
// that warning fire at all: the root it is measuring is being removed underneath
// the walk, which is the one condition where Size reports an error instead of
// the empty answer it gives for an absent root.
//
// So the write is real and live, and the reader is unguarded. That is the data
// race #853's stack shows: bytes.(*Buffer).grow from the abandoned probe against
// bytes.(*Buffer).String from the test. It is also worse than a detector
// artefact, because a straggler that logs AFTER this test's cleanup restored the
// previous handler writes into the NEXT test's buffer instead — where a warning
// nobody wrote fails an assertion that nothing was logged at all.
//
// A locked Write is the whole of the fix, and not merely because it is easy:
// slog formats each record into its own buffer and issues exactly ONE Write per
// record (log/slog's commonHandler.handle holds its own lock across that single
// write), so the mutex here only ever has to span one complete record. Every
// read goes through the same lock, so a read cannot observe a torn record
// either.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write implements io.Writer, which is what slog.TextHandler formats into.
func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *lockedBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

// Reset is here for the same reason String is: a subtest that reuses one
// capture has to clear it without racing a write it does not own.
func (b *lockedBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

// captureProcessLogs redirects the process default logger at a lockedBuffer for
// the duration of the test and returns it.
//
// Process-wide rather than per-logger because that is the only place a warning
// emitted from deep inside a harness spawn can be caught at all: scratch's
// budget warnings and the probe warnings both go to slog.Default(). The cost is
// that the sink outlives nothing — a goroutine the test abandoned can still be
// writing here after the test's last assertion, so the buffer is locked rather
// than bare, and see lockedBuffer plus settleAbandonedProbes for the two halves
// of that.
//
// Settling the probes this test abandoned belongs HERE rather than in each test
// that abandons one, because a test cannot know it abandoned a probe at all — the
// flight belongs to codexFeaturesFor or claudeCapabilitiesFor, which walk away
// from it on purpose — so a rule every capture-installing test follows by
// construction is the only version of this that covers all of them.
//
// The ORDER inside the cleanup is the point, and settling first is the order that
// counts: a straggler's last warnings land in the capture this test is still
// holding rather than in a handler the next test has installed. Sequencing the
// settle and the restore in ONE closure is what makes that a fact about this
// function — registering them as two cleanups would make it a fact about which
// line of a test ran first, since t.Cleanup runs LIFO, and the two halves are
// registered from different helpers. A test that reads no log still settles, from
// the fake-binary helper's own registration; see registerProbeIdentity and
// markCaptureInstalled, which between them keep a test with both helpers to one
// settle.
//
// This is the same helper the codex policy tests reach through
// captureCodexWarnings, which names the verdicts those tests are actually
// asserting on.
func captureProcessLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	var logs lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	// Claims the settle for this test, and takes it over from a fake that claimed
	// it first — see markCaptureInstalled for why the capture is the better owner.
	markCaptureInstalled(t)
	t.Cleanup(func() {
		settleAbandonedProbes(t)
		slog.SetDefault(prev)
	})
	return &logs
}
