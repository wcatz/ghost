package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeEmbed stands in for Ollama: it answers every input with a deterministic
// vector, counts the batches it served, and charges each call to the embedder's
// budget, which is how a test drives the -embed-deadline wall clock without a
// model, a network or a sleep.
type fakeEmbed struct {
	emb    *cachedEmbedder
	path   string
	calls  int
	charge time.Duration
}

func (f *fakeEmbed) batch(_ context.Context, texts []string) ([][]float32, error) {
	f.calls++
	f.emb.budget.spent += f.charge
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = []float32{float32(len(t)), 0.5, 0.25, 0.125}
	}
	return out, nil
}

// newFakeEmbedder returns a cachedEmbedder whose remote batch call is a fake
// that costs `charge` of embed budget per call, writing to a real cache file so
// a test can check what an interrupted pass leaves behind on disk.
func newFakeEmbedder(t *testing.T, charge time.Duration) *fakeEmbed {
	t.Helper()
	path := filepath.Join(t.TempDir(), "embed-cache.jsonl")
	emb, err := newCachedEmbedder("", path)
	if err != nil {
		t.Fatalf("newCachedEmbedder: %v", err)
	}
	f := &fakeEmbed{emb: emb, path: path, charge: charge}
	emb.embed = f.batch
	t.Cleanup(func() { _ = emb.Close() })
	return f
}

func fakeTurns(n int) []string {
	texts := make([]string, n)
	for i := range texts {
		texts[i] = fmt.Sprintf("turn %d of the haystack", i)
	}
	return texts
}

// TestEmbedDeadlineStopsBeforeTheNextBatch: an embedding pass that spends its
// budget must stop at a batch boundary with everything it did compute already
// appended to the cache file, and say so with errEmbedDeadline so the caller
// can tell it apart from a broken run. The workflow's save step uploads exactly
// those lines, which is the whole point: a pass killed at the job cap reaches
// no save step at all and the cache never warms.
func TestEmbedDeadlineStopsBeforeTheNextBatch(t *testing.T) {
	const charge = 40 * time.Minute
	f := newFakeEmbedder(t, charge)
	f.emb.SetDeadline(50 * time.Minute) // two batches fit, the third does not

	err := f.emb.EnsureBatch(context.Background(), fakeTurns(3*embedBatchSize))
	if !errors.Is(err, errEmbedDeadline) {
		t.Fatalf("EnsureBatch error = %v, want errEmbedDeadline", err)
	}
	if f.calls != 2 {
		t.Errorf("fake embedder served %d batches, want 2 (the third must be refused before it is sent)", f.calls)
	}
	if _, misses := f.emb.Stats(); misses != 2*embedBatchSize {
		t.Errorf("misses = %d, want %d", misses, 2*embedBatchSize)
	}

	// The vectors computed before the stop are on disk, not just in memory —
	// the workflow's actions/cache/save step reads this file.
	found := 0
	file, err := os.Open(f.path)
	if err != nil {
		t.Fatalf("open embed cache: %v", err)
	}
	defer file.Close() //nolint:errcheck
	sc := bufio.NewScanner(file)
	for sc.Scan() {
		var line cacheLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("cache line is not valid JSON: %v", err)
		}
		if len(line.Vector) == 0 || line.Hash == "" {
			t.Errorf("cache line is missing its hash or vector: %s", sc.Text())
		}
		found++
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read embed cache: %v", err)
	}
	if found != 2*embedBatchSize {
		t.Errorf("embed cache holds %d lines, want %d", found, 2*embedBatchSize)
	}
}

// TestEmbedDeadlineChargesRealEmbedTime: the budget is charged by the wall
// clock the pass actually spends inside remote calls, measured around the call
// itself — not by anything the caller asserts. The fake here deliberately
// charges NOTHING and just takes time, so this fails if the harness stops
// measuring (or stops charging) the real call: the second batch would then fit
// inside a budget the first batch has already overrun by 20x.
func TestEmbedDeadlineChargesRealEmbedTime(t *testing.T) {
	const slowCall = 5 * time.Millisecond
	slow := &fakeEmbed{charge: 0}
	emb, err := newCachedEmbedder("", filepath.Join(t.TempDir(), "embed-cache.jsonl"))
	if err != nil {
		t.Fatalf("newCachedEmbedder: %v", err)
	}
	t.Cleanup(func() { _ = emb.Close() })
	slow.emb = emb
	emb.embed = func(_ context.Context, texts []string) ([][]float32, error) {
		slow.calls++
		time.Sleep(slowCall)
		out := make([][]float32, len(texts))
		for i, t := range texts {
			out[i] = []float32{float32(len(t)), 0.5, 0.25, 0.125}
		}
		return out, nil
	}
	// Two batches; the first's measured cost (>= 5ms) alone exhausts a 1ms
	// budget, so the second must be refused.
	emb.SetDeadline(time.Millisecond)
	if err := emb.EnsureBatch(context.Background(), fakeTurns(2*embedBatchSize)); !errors.Is(err, errEmbedDeadline) {
		t.Fatalf("EnsureBatch error = %v, want errEmbedDeadline", err)
	}
	if slow.calls != 1 {
		t.Errorf("fake embedder served %d batches, want 1", slow.calls)
	}
}

// TestEmbedDeadlineLeavesAWarmCacheAlone: the budget bounds the embedding
// pass, so a pass that needs no remote call at all can never be cut off by it,
// however much of the budget a previous pass spent. This is what makes
// re-dispatching terminate: a dispatch whose cache is already complete runs its
// scoring pass and reports real numbers, instead of stopping at its budget and
// making no progress forever.
func TestEmbedDeadlineLeavesAWarmCacheAlone(t *testing.T) {
	f := newFakeEmbedder(t, 40*time.Minute)
	texts := []string{"alice moved to lisbon", "bob ships on fridays"}
	if err := f.emb.EnsureBatch(context.Background(), texts); err != nil {
		t.Fatalf("cold EnsureBatch: %v", err)
	}
	callsAfterCold := f.calls

	// Budget already spent (40m against a 1m limit), so any remote call now
	// would be refused. Both a batch resolve and a single lookup have to
	// succeed from cache anyway, or a warm dispatch would stop partway through
	// scoring and re-dispatch could never finish.
	f.emb.SetDeadline(1 * time.Minute)
	if err := f.emb.EnsureBatch(context.Background(), texts); err != nil {
		t.Fatalf("a cached batch must not be cut off by the embed budget: %v", err)
	}
	if _, err := f.emb.EmbedDocument(context.Background(), texts[0]); err != nil {
		t.Fatalf("a cached document must not be cut off by the embed budget: %v", err)
	}
	if f.calls != callsAfterCold {
		t.Errorf("warm pass made %d remote calls, want 0", f.calls-callsAfterCold)
	}
	// The lookup resolved from the cache rather than falling through to the
	// (exhausted) remote path: one more miss would have been computed.
	if _, misses := f.emb.Stats(); misses != len(texts) {
		t.Errorf("misses = %d, want %d: a cached lookup was recomputed remotely", misses, len(texts))
	}
}

// TestWarmProgressCountsDistinctRolePrefixedInputs: the partial-run report says
// "cache warmed N/M", so N and M have to count the vectors this pass actually
// needs: every distinct haystack turn in the document role and every distinct
// question in the query role. Shared sessions across questions are one vector,
// and the same text in the two roles is two — the prefixes are what make them
// different vectors, and a document-role embedding of a question must not
// satisfy the question's query role.
func TestWarmProgressCountsDistinctRolePrefixedInputs(t *testing.T) {
	aliceTurn := turn{Role: "user", Content: "alice moved to lisbon"}
	bobTurn := turn{Role: "user", Content: "bob ships on fridays"}
	catTurn := turn{Role: "user", Content: "the cat sleeps"}
	q1 := question{
		QuestionID: "q1", Question: "where does alice live?",
		SessionIDs: []string{"s1", "s2"},
		Sessions:   [][]turn{{aliceTurn, bobTurn}, {bobTurn}},
	}
	// q2 shares bob's session with q1 and repeats one of its turns, so the
	// distinct-input count cannot be a per-turn tally.
	q2 := question{
		QuestionID: "q2", Question: "when does bob ship?",
		SessionIDs: []string{"s2", "s3"},
		Sessions:   [][]turn{{bobTurn, catTurn}, {aliceTurn}},
	}
	selected := []question{q1, q2}
	// 3 distinct turns (alice, bob, cat) + 2 questions = 5.
	const wantRequired = 5

	ctx := context.Background()
	f := newFakeEmbedder(t, 0)
	cached, required := f.emb.warmProgress(selected)
	if required != wantRequired {
		t.Errorf("required = %d, want %d", required, wantRequired)
	}
	if cached != 0 {
		t.Errorf("cached = %d on an empty cache, want 0", cached)
	}

	// Embedding q1's question as a DOCUMENT fills the document role only.
	if err := f.emb.EnsureBatch(ctx, []string{q1.Question}); err != nil {
		t.Fatalf("EnsureBatch: %v", err)
	}
	cached, required = f.emb.warmProgress(selected)
	if required != wantRequired {
		t.Errorf("required = %d after embedding, want %d", required, wantRequired)
	}
	if cached != 0 {
		t.Errorf("cached = %d, want 0: a document-role embedding of %q is not the question's query vector",
			cached, q1.Question)
	}

	if err := f.emb.EnsureBatch(ctx, []string{
		aliceTurn.Content, bobTurn.Content, catTurn.Content,
	}); err != nil {
		t.Fatalf("EnsureBatch: %v", err)
	}
	if _, err := f.emb.EmbedQuery(ctx, q1.Question); err != nil {
		t.Fatalf("EmbedQuery: %v", err)
	}
	cached, required = f.emb.warmProgress(selected)
	if cached != 4 || required != wantRequired {
		t.Errorf("warmProgress = %d/%d, want 4/%d", cached, required, wantRequired)
	}
}

// TestReportPassPartialPrintsNoResult: a pass stopped at its budget has scored
// some questions, and their averages are all below the floors — so the one
// thing it must not do is print a table a reader (or a log scraper) could take
// for a benchmark result. It reports cache progress, prints no metrics at all,
// and exits with the partial status rather than the floor-violation one.
func TestReportPassPartialPrintsNoResult(t *testing.T) {
	f := newFakeEmbedder(t, 0)
	selected := []question{{QuestionID: "q1", Question: "where does alice live?"}}
	// Two questions scored, both far below the floors, on a pass that stopped
	// partway through the third.
	partialAgg := &agg{n: 2, r5: 0.0, ndcg: 0.0}
	var buf bytes.Buffer
	status := reportPass(&buf, passReport{
		condition: "hybrid",
		overall:   partialAgg,
		byType:    map[string]*agg{"single-session-user": partialAgg},
		scored:    2,
		embedder:  f.emb,
		selected:  selected,
		floors:    map[string]float64{"r5": 0.91, "ndcg10": 0.89},
		partial:   true,
		budget:    5 * time.Hour,
		elapsed:   4 * time.Hour,
	})

	if status != exitPartial {
		t.Errorf("status = %d, want exitPartial=%d", status, exitPartial)
	}
	got := buf.String()
	if !strings.Contains(got, "cache warmed") {
		t.Errorf("partial report does not state cache progress: %q", got)
	}
	for _, isAResult := range []string{"OVERALL", "R@1", "question type", "questions scored", "FLOOR VIOLATION"} {
		if strings.Contains(got, isAResult) {
			t.Errorf("partial report contains %q — it reads as a benchmark result: %q", isAResult, got)
		}
	}
}

// TestReportPassCompletePrintsTheTable: the other half of the same branch —
// a pass that finished still reports its metrics and still checks its floors,
// so bounding the embedding pass changed nothing for a run that completes.
func TestReportPassCompletePrintsTheTable(t *testing.T) {
	f := newFakeEmbedder(t, 0)
	full := &agg{n: 2, r1: 0.5, r5: 1.6, r10: 1.8, mrr: 1.5, ndcg: 1.4}

	t.Run("clean pass reports OVERALL and exits 0", func(t *testing.T) {
		var buf bytes.Buffer
		status := reportPass(&buf, passReport{
			condition: "hybrid", overall: full,
			byType: map[string]*agg{"single-session-user": full},
			scored: 2, embedder: f.emb, floors: map[string]float64{"r5": 0.74},
		})
		if status != exitComplete {
			t.Errorf("status = %d, want exitComplete=%d", status, exitComplete)
		}
		if !strings.Contains(buf.String(), "OVERALL") {
			t.Errorf("complete pass did not report its metrics: %q", buf.String())
		}
	})

	t.Run("violating pass exits 1", func(t *testing.T) {
		var buf bytes.Buffer
		status := reportPass(&buf, passReport{
			condition: "hybrid", overall: full,
			byType: map[string]*agg{"single-session-user": full},
			scored: 2, embedder: f.emb, floors: map[string]float64{"r5": 0.99},
		})
		if status != exitFailure {
			t.Errorf("status = %d, want exitFailure=%d", status, exitFailure)
		}
	})
}

// TestPartialRunMessageIsNotAResult: the message a stopped pass prints is the
// only thing a cold dispatch reports, so it has to read as progress plus a next
// step — never as a score, and never as a metric a reader could floor-check.
func TestPartialRunMessageIsNotAResult(t *testing.T) {
	got := partialMessage(64000, 231904, 5*time.Hour, 4*time.Hour+58*time.Minute)
	if !strings.Contains(got, "cache warmed 64000/231904 vectors") {
		t.Errorf("partial message does not report progress: %q", got)
	}
	if !strings.Contains(got, "re-dispatch") {
		t.Errorf("partial message does not name the next step: %q", got)
	}
	for _, notAResult := range []string{"OVERALL", "R@5", "NDCG", "FLOOR"} {
		if strings.Contains(got, notAResult) {
			t.Errorf("partial message reads like a result (%q present): %q", notAResult, got)
		}
	}
}
