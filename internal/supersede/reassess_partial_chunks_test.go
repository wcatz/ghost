package supersede

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// This file is #808: what `ghost supersede --reassess` does with the verdicts the
// chunks that ANSWERED produced when a later chunk's call died.
//
// Reassess submits every open pair to one ClassifyBatch, and ClassifyBatch
// chunks it — eight pairs per harness call, because every call pays a process
// spawn and the whole rubric while each extra pair adds only its two note bodies.
// So a project with 87 live edges is eleven calls, and a transport failure in
// call 7 used to discard calls 1..6: every open pair moved to Unjudged, every
// answered verdict was thrown away, and the rerun paid for all eleven again.
//
// The all-or-nothing rule was deliberate, and the reason it was stated is the
// reason it was wrong here: "a call that failed answers nothing about anything it
// carried". That is true of the call that failed. It is not true of the calls
// before it, whose verdicts are complete, parsed and indexed by pair number —
// and nothing downstream needs the whole set. Each pair's verdict is read on its
// own (settleCycle for a cycle, a four-way switch for an ordinary edge), a cycle
// contributes exactly ONE candidate and so lives in exactly one chunk, and the
// pass already has a partial-application contract: #699 made a failed call apply
// the withdrawals a deterministic rule settled, report the rest unjudged, and
// still return an error. This widens that contract from "what a rule settled for
// free" to "what a call already answered for money". Nothing about the
// withdrawals or the graph changes shape; only the amount of a paid answer the
// pass is willing to throw away does.

// chunkFailProvider answers the first answerFirst calls and fails every call
// after it, which is what a harness that dies part way through a large store
// looks like from the pass's side. It is a different shape from flakyProvider
// (which fails the first N calls, so the retry answers) because the case here is
// the opposite one: the failure has to come AFTER a good chunk, and a provider
// that fails from the start never produces one.
type chunkFailProvider struct {
	reply       string
	answerFirst int
	calls       int
}

func (p *chunkFailProvider) Classify(_ context.Context, _, _ string) (string, error) {
	p.calls++
	if p.calls > p.answerFirst {
		return "", fmt.Errorf("opencode run: exit status %d", p.calls)
	}
	return p.reply, nil
}

// chunked builds a classifier over p whose chunk size is small enough that the
// fixture's pairs really are several calls: the shipped size is eight, so a
// four-pair fixture would fit in ONE call and the case under test would not
// exist. batchSize is a field on the classifier for exactly this (see
// classifyBatchSize), and it is set here rather than by a fixture made
// artificially large, which would also have had to keep the pairs plausible.
func chunked(p *chunkFailProvider) *RelationClassifier {
	cls := NewRelationClassifier(p)
	cls.batchSize = 2
	cls.SetRetryDelay(0)
	return cls
}

// summarising is the pass's logger writing into buf, so a test can assert on the
// one summary line the pass emits. It is a helper because three of the
// assertions below are about what that line says, and a report that counts only
// one half of a partial repair is the failure this whole change is about.
func summarising(buf *strings.Builder) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, nil))
}

// TestReassessAppliesTheChunksThatAnswered is the ordinary-edge case: the first
// chunk's verdicts are acted on, the failed chunk's pairs are the only ones
// reported unjudged, and the pass still fails so the operator is asked for a
// rerun.
func TestReassessAppliesTheChunksThatAnswered(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	// Four live edges, none vetoed and none scope-conflicting, so all four are
	// open and the two chunks hold two pairs each. The texts differ per pair
	// because a fixture whose pairs were identical would also be a fixture whose
	// verdicts could not be told apart.
	type pair struct{ newer, older string }
	var pairs []pair
	for _, texts := range [][2]string{
		{"The restore spanned two spindles and took 41 minutes in week a.", "The restore path on one spindle is safe and takes under a minute."},
		{"The ingest service now runs Redis 7.2, revision a.", "The ingest service runs Redis 6.2, revision a."},
		{"The restore spanned two spindles and took 42 minutes in week b.", "The restore path on one spindle is safe and takes under a minute, per week b."},
		{"The ingest service now runs Redis 7.4, revision b.", "The ingest service runs Redis 6.4, revision b."},
	} {
		n, o := seedEdge(t, store, db, texts[0], texts[1])
		pairs = append(pairs, pair{newer: n, older: o})
	}

	var buf strings.Builder
	// Chunk 1 answers NEITHER for both pairs it carries, so both of those edges
	// are withdrawn. Chunk 2's call dies and is retried once, so the provider
	// sees exactly three calls: one answered, two failed.
	fp := &chunkFailProvider{reply: "1: NEITHER\n2: NEITHER", answerFirst: 1}
	res, withdrawn, err := Reassess(ctx, store, chunked(fp), "p", true, summarising(&buf))
	if err == nil {
		t.Fatal("a chunk that kept failing must still fail the pass: a partial repair is partial, and a zero exit would read as \"done\"")
	}
	if !strings.Contains(err.Error(), "classify") {
		t.Errorf("error %q must name the classify failure", err)
	}
	if fp.calls != 3 {
		t.Errorf("provider calls = %d, want 3 (one answered chunk, then the failed chunk and its one retry)", fp.calls)
	}

	if res.Neither != 2 {
		t.Errorf("Neither = %d, want 2: the chunk that answered is judged like any other, and its verdicts are money already spent", res.Neither)
	}
	if res.Withdrawn != 2 {
		t.Errorf("Withdrawn = %d, want 2: the verdicts the answered chunk produced are applied, not discarded", res.Withdrawn)
	}
	if len(res.Unjudged) != 2 {
		t.Fatalf("Unjudged = %+v, want the 2 pair(s) the FAILED chunk carried and not all 4: a chunk that answered is a complete answer for its own pairs", res.Unjudged)
	}
	// The graph is the independent witness, and it says which two pairs were
	// settled: an unjudged pair's edge is still live, a settled pair's is not.
	settled := 0
	for _, p := range pairs {
		edges, gerr := store.SupersedesWithin(ctx, []string{p.newer, p.older})
		if gerr != nil {
			t.Fatal(gerr)
		}
		unjudged := false
		for _, u := range res.Unjudged {
			if newPairKey(u.NewerID, u.OlderID) == newPairKey(p.newer, p.older) {
				unjudged = true
			}
		}
		if unjudged {
			if len(edges) != 1 {
				t.Errorf("pair %s→%s is reported unjudged but has %d live edge(s), want 1: the pass decided nothing about it, so its edge stands", p.newer, p.older, len(edges))
			}
			continue
		}
		settled++
		if len(edges) != 0 {
			t.Errorf("pair %s→%s is settled as NEITHER but its edge is still live (%v)", p.newer, p.older, edges)
		}
	}
	if settled != 2 {
		t.Errorf("settled pairs = %d, want 2", settled)
	}
	// Every withdrawal is reported as one that happened, and only those.
	written := 0
	for _, w := range withdrawn {
		if w.Written {
			written++
		}
	}
	if written != 2 {
		t.Errorf("withdrawn rows = %+v, want exactly the 2 that were written", withdrawn)
	}
	// The summary says both halves, because "2 withdrawn" alone reads as a
	// complete repair over a project of four.
	if !strings.Contains(buf.String(), "unjudged=2") {
		t.Errorf("the summary must report what is still owed:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "withdrawn=2") {
		t.Errorf("the summary must report what moved:\n%s", buf.String())
	}
}

// TestReassessKeepsACyclesVerdictFromAChunkThatAnswered is the case the issue
// named as the possible reason all-or-nothing is REQUIRED: a cycle whose two
// edges span two chunks. It is not, and the reason is structural rather than
// lucky — a cycle contributes exactly ONE candidate (that is what #778 changed:
// the two edges are one question), so a cycle cannot straddle a chunk boundary
// and there is no pair whose verdict is split across two calls.
//
// So the two cycles in the answered chunk are settled to one standing edge each,
// and the two in the failed chunk are reported NO-VERDICT with both their edges
// live. That last distinction is the operator's next step, which is why the two
// are separate outcomes: a missing verdict is answered by a rerun, and only a
// pair with no knowable direction is the operator's to settle.
func TestReassessKeepsACyclesVerdictFromAChunkThatAnswered(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	// Four cyclic pairs, so every chunk holds two cycles and the whole fixture
	// is cycles: the ordinary-edge case above covers the other shape, and this
	// one is about the reporting the cycle branch owns. fixFirst alternates so
	// the store's row order is not the same for every pair.
	type pair struct{ stale, fix string }
	var pairs []pair
	for i, texts := range [][2]string{
		{"bug: the relay stalls on every consumer rebalance, week a", "the relay rebalance stall is fixed: pin the consumer, week a"},
		{"bug: the queue drains slowly on every restart, week b", "the queue drain stall is fixed: raise the batch size, week b"},
		{"bug: the cache never expires between restarts, week c", "the cache expiry gap is fixed: stamp the key, week c"},
		{"bug: the index lags behind the writer, week d", "the index lag is fixed: flush on commit, week d"},
	} {
		stale, fix := seedCycle(t, store, db, texts[0], texts[1], i%2 == 0)
		pairs = append(pairs, pair{stale: stale, fix: fix})
	}

	var buf strings.Builder
	// Chunk 1 confirms whichever direction it is asked about, so each of its two
	// cycles keeps the edge the pass asked about and withdraws its reverse.
	fp := &chunkFailProvider{
		reply:       "1: SUPERSEDES | replaced: it stalls on every rebalance\n2: SUPERSEDES | replaced: it drains slowly on every restart",
		answerFirst: 1,
	}
	res, _, err := Reassess(ctx, store, chunked(fp), "p", true, summarising(&buf))
	if err == nil {
		t.Fatal("the failed chunk's pairs are unjudged, so the repair is incomplete and the exit must say so")
	}

	if res.Withdrawn != 2 {
		t.Errorf("Withdrawn = %d, want 2: one reverse edge per cycle in the chunk that answered", res.Withdrawn)
	}
	if len(res.Unjudged) != 2 {
		t.Errorf("Unjudged = %+v, want the 2 pair(s) the failed chunk carried", res.Unjudged)
	}
	// Every cycle is reported, and the two the failed chunk carried are reported
	// as having no verdict rather than as having stood.
	if len(res.Cyclic) != 4 {
		t.Fatalf("Cyclic = %+v, want all 4 pair(s) named: a cycle is live and demoting both endpoints whatever this pass decided", res.Cyclic)
	}
	noVerdict, kept := 0, 0
	for _, c := range res.Cyclic {
		switch c.Outcome {
		case CycleNoVerdict:
			noVerdict++
		case CycleKeptFirst, CycleKeptSecond:
			kept++
		default:
			t.Errorf("outcome %q on a cycle the failed chunk carried, want no-verdict: no call answered about it", c.Outcome)
		}
	}
	if kept != 2 || noVerdict != 2 {
		t.Errorf("cycles kept=%d no-verdict=%d, want 2 and 2: the answered chunk's verdicts stand and the failed chunk's are owed", kept, noVerdict)
	}
	// The graph, read independently: a cycle the answered chunk judged keeps
	// exactly one edge and one the failed chunk carried keeps both.
	for _, p := range pairs {
		edges, gerr := store.SupersedesWithin(ctx, []string{p.stale, p.fix})
		if gerr != nil {
			t.Fatal(gerr)
		}
		if len(edges) != 1 && len(edges) != 2 {
			t.Errorf("cycle %s/%s has %d live edge(s), want 1 (judged) or 2 (unjudged): %v", p.stale, p.fix, len(edges), edges)
		}
	}
	if !strings.Contains(buf.String(), "cyclic=4") {
		t.Errorf("the summary must name every cycle:\n%s", buf.String())
	}
}

// overReportingClassifier claims more pairs were answered than were asked about,
// which no shipped classifier can do and every caller of a returned count has to
// survive anyway: the count is about to be used as a SLICE BOUND, and a
// classifier that does not add up deserves a report rather than a panic.
type overReportingClassifier struct {
	extra int
}

func (c *overReportingClassifier) ClassifyBatch(_ context.Context, pairs []Candidate) ([]Relation, error) {
	verdicts := make([]Relation, 0, len(pairs)+c.extra)
	for range pairs {
		verdicts = append(verdicts, RelationNeither)
	}
	for range c.extra {
		verdicts = append(verdicts, RelationNeither)
	}
	return verdicts, &PartialVerdictsError{Answered: len(pairs) + c.extra, Err: errors.New("opencode run: exit status 1")}
}

// TestReassessClampsAnOverReportedPrefix is the guard on that count. The pair
// list is three long; a classifier that claims five were answered must leave the
// pass settling at most three, naming at most three unjudged, and returning the
// error — never indexing past the pairs it was given.
func TestReassessClampsAnOverReportedPrefix(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	for _, texts := range [][2]string{
		{"The restore spanned two spindles and took 41 minutes in week a.", "The restore path on one spindle is safe and takes under a minute."},
		{"The ingest service now runs Redis 7.2, revision a.", "The ingest service runs Redis 6.2, revision a."},
		{"The queue drains slowly on every restart, week b.", "The queue drain stall is fixed: raise the batch size, week b."},
	} {
		seedEdge(t, store, db, texts[0], texts[1])
	}

	res, _, err := Reassess(ctx, store, &overReportingClassifier{extra: 2}, "p", true, discardLogger())
	if err == nil {
		t.Fatal("a classifier that failed must still fail the pass, however much it claims to have answered")
	}
	// Everything it DID answer is settled, and nothing beyond the three pairs it
	// was given is named or counted.
	if res.Neither != 3 {
		t.Errorf("Neither = %d, want 3: every pair it answered is a pair it was asked about", res.Neither)
	}
	if len(res.Unjudged) != 0 {
		t.Errorf("Unjudged = %+v, want none: the claimed count is clamped to the pairs asked about, so the tail is empty rather than a slice out of range", res.Unjudged)
	}
	if res.Withdrawn != 3 {
		t.Errorf("Withdrawn = %d, want 3", res.Withdrawn)
	}
}
