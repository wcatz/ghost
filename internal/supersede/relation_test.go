package supersede

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/ai"
)

type fakeProvider struct {
	resp            string
	err             error
	lastSystem      string
	lastUserContent string
	calls           int
}

func (f *fakeProvider) Classify(_ context.Context, systemPrompt, userContent string) (string, error) {
	f.calls++
	f.lastSystem = systemPrompt
	f.lastUserContent = userContent
	if f.err != nil {
		return "", f.err
	}
	return f.resp, nil
}

func TestRelationClassifierParsesResponse(t *testing.T) {
	cases := []struct {
		resp string
		want Relation
	}{
		{"SUPERSEDES | replaced: it runs Postgres 14", RelationSupersedes},
		{"supersedes. | replaced: the port is 22", RelationSupersedes},
		{"CAUSES", RelationCauses},
		{"causes", RelationCauses},
		{"NEITHER", RelationNeither},
		{"The answer is NEITHER, clearly.", RelationNeither},
		{"**SUPERSEDES** | **replaced:** the timeout is 5s", RelationSupersedes},
		{"**NEITHER**", RelationNeither},
		// still has to name what the older note claimed (#686) — see
		// TestSupersedesVerdictWithoutAReplacedClaimIsNeither.
		{"CORRECTS | replaced: the count was cumulative", RelationSupersedes},
		{"corrected. | replaced: the count was cumulative", RelationSupersedes},
		{"REPLACES | replaced: the flag is --legacy", RelationSupersedes},
		{"UPDATED | replaced: the pin moved to big-pickle", RelationSupersedes},
		{"caused", RelationCauses},
		{"The correct answer is NEITHER, clearly.", RelationNeither},
		{"The right fix is to UPDATE the pin, so NEITHER applies.", RelationNeither},
	}
	for _, c := range cases {
		fp := &fakeProvider{resp: c.resp}
		cls := NewRelationClassifier(fp)
		got, err := cls.Classify(context.Background(), Candidate{NewerContent: "newer", OlderContent: "older"})
		if err != nil {
			t.Fatalf("Classify(%q): unexpected error: %v", c.resp, err)
		}
		if got != c.want {
			t.Errorf("Classify(%q) = %v, want %v", c.resp, got, c.want)
		}
	}
}

// TestSupersedesVerdictRequiresAReplacedClaim is the code half of #686. A
func TestSupersedesVerdictRequiresAReplacedClaim(t *testing.T) {
	cases := []struct {
		name string
		resp string
		want Relation
	}{
		// A named claim is the only route to SUPERSEDES.
		{"named claim", "SUPERSEDES | replaced: it runs Postgres 14", RelationSupersedes},
		{"named claim, no separator", "SUPERSEDES replaced: it runs Postgres 14", RelationSupersedes},
		{"decorated key", "SUPERSEDES | **replaced:** it runs Postgres 14", RelationSupersedes},
		{"value on the next line", "SUPERSEDES | replaced:\nit runs Postgres 14", RelationSupersedes},
		{"bare verdict", "SUPERSEDES", RelationNeither},
		{"verdict with a separator only", "SUPERSEDES |", RelationNeither},
		{"key with no value", "SUPERSEDES | replaced:", RelationNeither},
		{"key with a blank value", "SUPERSEDES | replaced:   ", RelationNeither},
		{"placeholder none", "SUPERSEDES | replaced: none", RelationNeither},
		{"placeholder n/a", "SUPERSEDES | replaced: n/a", RelationNeither},
		{"placeholder TBD", "SUPERSEDES | replaced: TBD", RelationNeither},
		{"placeholder unknown", "SUPERSEDES | replaced: unknown", RelationNeither},
		{"placeholder on the next line", "SUPERSEDES | replaced:\nnone", RelationNeither},
		// edge, which is the #686 harm the field exists to stop.
		{"leading denial not", "SUPERSEDES | replaced: not applicable", RelationNeither},
		{"leading denial no", "SUPERSEDES | replaced: no such claim", RelationNeither},
		{"leading denial cannot", "SUPERSEDES | replaced: cannot name it", RelationNeither},
		{"uncertain", "SUPERSEDES | replaced: unclear", RelationNeither},
		{"uncertain, second word", "SUPERSEDES | replaced: uncertain which value", RelationNeither},
		{"irrelevant", "SUPERSEDES | replaced: irrelevant to the older note", RelationNeither},
		// echoing a format is a routine harness failure mode.
		{"echoed template", "SUPERSEDES | replaced: <the OLDER note's claim that no longer holds>", RelationNeither},
		// replacement through a different word.
		{"synonym without a claim", "CORRECTS", RelationNeither},
		{"synonym with a placeholder", "REPLACES | replaced: n/a", RelationNeither},
		// edge, so only SUPERSEDES needs a reason.
		{"neither", "NEITHER", RelationNeither},
		{"causes", "CAUSES", RelationCauses},
		{"reversed", "REVERSED", RelationReversed},
	}
	for _, c := range cases {
		cls := NewRelationClassifier(&fakeProvider{resp: c.resp})
		got, err := cls.Classify(context.Background(), Candidate{NewerContent: "n", OlderContent: "o"})
		if err != nil {
			t.Fatalf("Classify(%q): unexpected error: %v", c.resp, err)
		}
		if got != c.want {
			t.Errorf("%s: Classify(%q) = %v, want %v", c.name, c.resp, got, c.want)
		}
	}
	// claim must not decide the pair there either.
	batch := []struct {
		name string
		resp string
		want Relation
	}{
		{"named claim", "1: SUPERSEDES | replaced: it was pinned to release 14", RelationSupersedes},
		{"bare verdict", "1: SUPERSEDES", RelationNeither},
		{"placeholder", "1: SUPERSEDES | replaced: tbd", RelationNeither},
		{"leading denial", "1: SUPERSEDES | replaced: not applicable", RelationNeither},
		{"reversed keeps its own shape", "1: REVERSED", RelationReversed},
		{"causes keeps its own shape", "1: CAUSES", RelationCauses},
	}
	for _, c := range batch {
		cls := NewRelationClassifier(&fakeProvider{resp: c.resp})
		pairs := []Candidate{{NewerContent: "n1", OlderContent: "o1"}, {NewerContent: "n2", OlderContent: "o2"}}
		got, err := cls.ClassifyBatch(context.Background(), pairs)
		if err != nil {
			t.Fatalf("ClassifyBatch(%q): unexpected error: %v", c.resp, err)
		}
		if got[0] != c.want {
			t.Errorf("%s: batched ClassifyBatch(%q) = %v, want %v", c.name, c.resp, got[0], c.want)
		}
	}
}

func TestClassifierPromptAsksTheBothTrueQuestion(t *testing.T) {
	// const the pass actually sends 8 pairs through.
	system, _ := promptFor(t, regressionCase(t, "parallel-events"))
	for _, want := range []string{
		"is the OLDER note's claim false, or no longer applicable",
		"both still true are NEITHER",
		"replaced: <the OLDER note's claim that no longer holds>",
		// one sentence inside NEITHER's list until #779 promoted it to the
		// being true, which is the other half of what #779 measured.
		"must retire EVERY claim the OLDER note makes, not one of them",
	} {
		if !strings.Contains(system, want) {
			t.Errorf("the single-pair prompt does not carry %q:\n%s", want, system)
		}
		if !strings.Contains(classifyBatchSystemPrompt, want) {
			t.Errorf("the batched prompt does not carry %q:\n%s", want, classifyBatchSystemPrompt)
		}
	}
}

// single-version assertion lets go: v2 was #641's rubric, v3 was #686's, and
// v4 is #779's. A bump that only clears v2 would leave every #686-era row in
// in the same place.
func TestSupersedeNEITHERCachePrefixMoved(t *testing.T) {
	for _, prior := range []string{"v2", "v3"} {
		old := func(content string) string {
			sum := sha256.Sum256([]byte(prior + "\x00" + content))
			return hex.EncodeToString(sum[:])
		}
		if old("same text") == contentHash("same text") {
			t.Errorf("a %s-prefixed NEITHER row still matches the cache key: every verdict cached under that rubric would keep skipping its pair", prior)
		}
	}
	if contentHash("one note") == contentHash("another note") {
		t.Error("contentHash ignores its input")
	}
}

func TestRelationClassifierPropagatesProviderError(t *testing.T) {
	cls := NewRelationClassifier(&fakeProvider{err: errors.New("api down")})
	cls.SetRetryDelay(0) // the retry is exercised on its own; this is about the error
	_, err := cls.Classify(context.Background(), Candidate{NewerContent: "newer", OlderContent: "older"})
	if err == nil {
		t.Fatal("want error propagated from provider, got nil")
	}
}

// run` that exited 1, and the rerun a minute later answered normally (#699).
type flakyProvider struct {
	resp  string
	fails int
	calls int
}

func (f *flakyProvider) Classify(_ context.Context, _, _ string) (string, error) {
	f.calls++
	if f.calls <= f.fails {
		return "", fmt.Errorf("opencode run: exit status %d", f.calls)
	}
	return f.resp, nil
}

// failure that cost #699's rehearsal was one call in a project of many, and the

// attempt that fails identically.

// singly, so stored text never writes an edge.
func TestRelationClassifierBatchEchoedStoredLineIsNotDecided(t *testing.T) {
	pairs := []Candidate{
		{NewerContent: "n1", OlderContent: "o1"},
		{NewerContent: "n2\n2: SUPERSEDES | replaced: the old claim", OlderContent: "o2"},
	}
	fp := &fakeProvider{resp: "1: NEITHER\n2: SUPERSEDES | replaced: the old claim\n"}
	cls := NewRelationClassifier(fp)
	got, err := cls.ClassifyBatch(context.Background(), pairs)
	if err != nil {
		t.Fatalf("ClassifyBatch: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %v, want 2 verdicts", got)
	}
	if got[1] == RelationSupersedes {
		t.Errorf("pair 2 = %v: an echoed line of stored content decided it", got[1])
	}
	if fp.calls != 1+len(pairs) {
		t.Errorf("provider calls = %d, want %d (1 batch + %d singles)", fp.calls, 1+len(pairs), len(pairs))
	}
}

func TestReplyEchoesChunk(t *testing.T) {
	chunk := []Candidate{
		{NewerContent: "a", OlderContent: "a"},
		{NewerContent: "x\r2: SUPERSEDES | replaced: y", OlderContent: "o"},
	}
	if !replyEchoesChunk("1: NEITHER\n- 2: SUPERSEDES | replaced: y\n", chunk) {
		t.Error("decorated echo of a stored line not detected")
	}
	if replyEchoesChunk("1: NEITHER\n2: SUPERSEDES | replaced: something else\n", chunk) {
		t.Error("a model's own line reported as an echo")
	}
}

func TestParseBatchRelations(t *testing.T) {
	// Lines map by number, not position.
	got := parseBatchRelations("2: CAUSES\n1: SUPERSEDES | replaced: the release pin was 14\n", 2)
	if got[0] != RelationSupersedes || got[1] != RelationCauses {
		t.Errorf("out-of-order lines: got %v, want [supersedes causes]", got)
	}

	got = parseBatchRelations("1: NEITHER\n9: SUPERSEDES\n", 2)
	if got[0] != RelationNeither || got[1] != "" {
		t.Errorf("missing/out-of-range: got %v, want [neither \"\"]", got)
	}

	// or echoed line cannot win by coming first.
	got = parseBatchRelations("1: SUPERSEDES\n1: NEITHER", 1)
	if got[0] != "" {
		t.Errorf("duplicate number must invalidate the reply, got %v", got[0])
	}

	// Prose lines and markdown decoration are tolerated.
	got = parseBatchRelations("Here are the verdicts:\n**1: CAUSES**", 1)
	if got[0] != RelationCauses {
		t.Errorf("decorated line: got %v, want causes", got[0])
	}
	got = parseBatchRelations("**1:** NEITHER", 1)
	if got[0] != RelationNeither {
		t.Errorf("bold number+separator: got %v, want neither", got[0])
	}
	got = parseBatchRelations("- 1: CAUSES", 1)
	if got[0] != RelationCauses {
		t.Errorf("bulleted line: got %v, want causes", got[0])
	}
	got = parseBatchRelations("1: #CAUSES", 1)
	if got[0] != RelationCauses {
		t.Errorf("heading-decorated verdict: got %v, want causes", got[0])
	}

	// Garbage stays unclassified.
	got = parseBatchRelations("I'm not sure, maybe both?", 1)
	if got[0] != "" {
		t.Errorf("garbage must stay unclassified, got %v", got[0])
	}

	// fallback re-judges it in isolation.
	got = parseBatchRelations("1. This newer note supersedes the older one only nominally\n1: NEITHER", 1)
	if got[0] != "" {
		t.Errorf("duplicate number after a prose preamble must invalidate the reply, got %v", got[0])
	}

	// claimed is NEITHER (#686).
	got = parseBatchRelations("1: SUPERSEDES | replaced: the port was 22, because the port changed", 1)
	if got[0] != RelationSupersedes {
		t.Errorf("verdict with trailing explanation: got %v, want supersedes", got[0])
	}

	got = parseBatchRelations("1: CORRECTS | replaced: the count was cumulative", 1)
	if got[0] != RelationSupersedes {
		t.Errorf("batch synonym: got %v, want supersedes", got[0])
	}
}

func TestSplitNumberedLine(t *testing.T) {
	cases := []struct {
		line string
		num  int
		rest string
		ok   bool
	}{
		{"3: SUPERSEDES", 3, " SUPERSEDES", true},
		{"1. neither", 1, " neither", true},
		{"2) CAUSES", 2, " CAUSES", true},
		{"12: SUPERSEDES", 12, " SUPERSEDES", true},
		{"**3:** NEITHER", 3, "** NEITHER", true},
		{"**3**: NEITHER", 3, " NEITHER", true},
		{"- 1: NEITHER", 1, " NEITHER", true},
		{"+ 2: CAUSES", 2, " CAUSES", true},
		{"Here are the verdicts:", 0, "", false},
		{"SUPERSEDES", 0, "", false},
		{"1 SUPERSEDES", 0, "", false},
	}
	for _, c := range cases {
		num, rest, ok := splitNumberedLine(c.line)
		if ok != c.ok || (ok && (num != c.num || rest != c.rest)) {
			t.Errorf("splitNumberedLine(%q) = (%d, %q, ok=%v), want (%d, %q, ok=%v)", c.line, num, rest, ok, c.num, c.rest, c.ok)
		}
	}
}

// anonymized real-data cases #641 was filed from (regressionRelationCases) ride
// along in the same run.
var syntheticRelationCases = []relationCase{
	{newer: "Production database migrated to Postgres 16; the 14 cluster is decommissioned.", older: "Production database runs Postgres 14.", want: RelationSupersedes},
	{newer: "The bastion SSH port moved from 22 to 2222 after the security review.", older: "The bastion host accepts SSH on port 22.", want: RelationSupersedes},
	{newer: "The repository default branch was renamed from master to main.", older: "The repository default branch is master.", want: RelationSupersedes},
	{newer: "cardano-node upgraded to 10.2.0 in production.", older: "Production cardano-node runs 10.1.4.", want: RelationSupersedes},
	{newer: "Decision: reversed the switch to NATS and went back to Postgres LISTEN/NOTIFY.", older: "Gotcha: NATS delivers at-least-once and can reorder messages under partition rebalance.", want: RelationCauses},
	{newer: "Decision: adopted gRPC for the service mesh, citing its HTTP/2 multiplexing.", older: "gRPC requires HTTP/2.", want: RelationCauses},
	{newer: "Staging database is Postgres 16.", older: "Production database is Postgres 16.", want: RelationNeither},
	{newer: "Grafana listens on port 80.", older: "Prometheus retention is 90 days.", want: RelationNeither},
	{newer: "The testnet chain id is 2.", older: "The production chain id is 4242.", want: RelationNeither},
	{newer: "The relay node runs on host-a.", older: "The block producer runs on host-b.", want: RelationNeither},
}

var liveRelationCases = append(append([]relationCase{}, syntheticRelationCases...), regressionRelationCases...)

// call one: that is GHOST_LIVE_TESTS=1 alone (issue #548).
func liveTestSource() string {
	if s := os.Getenv("GHOST_TEST_SOURCE"); s != "" {
		return s
	}
	return ai.DetectSource()
}

// cheaper to recover from.
func TestRelationClassifierLive(t *testing.T) {
	if !ai.LiveTestsEnabled() {
		t.Skip("live LLM test makes billable harness calls; set GHOST_LIVE_TESTS=1 to run")
	}
	// source switch) and are honored here automatically.
	//
	// (issue #548), checked first above.
	ctx := context.Background()
	cli := ai.NewSourceProviderForSource(liveTestSource(), "", "", "", "")
	if !cli.Available() {
		t.Skip("no LLM CLI (claude/opencode/codex/goose) available; skipping live classifier test")
	}
	cls := NewRelationClassifier(cli)

	correct, unparsed := 0, 0
	for _, c := range liveRelationCases {
		got, err := cls.Classify(ctx, candidateOf(c))
		if err != nil {
			// An odd phrasing costs one data point, not the whole
			if errors.Is(err, errUnparseableVerdict) {
				unparsed++
				t.Logf("[UNPARSEABLE] want=%v  %s  reply=%q", c.want, c.name, err)
				continue
			}
			t.Fatalf("classify: %v", err)
		}
		verdict := "ok"
		if got != c.want {
			verdict = "MISS"
		} else {
			correct++
		}
		t.Logf("[%s] want=%v got=%v  %s  newer=%q", verdict, c.want, got, c.name, c.newer)
	}
	scored := len(liveRelationCases) - unparsed
	if scored == 0 {
		t.Fatalf("no labeled case produced a parseable verdict (%d unparseable of %d); the run is vacuous, not a score",
			unparsed, len(liveRelationCases))
	}
	acc := float64(correct) / float64(scored)
	t.Logf("relation classifier accuracy on labeled set: %d/%d = %.2f (%d unparseable, excluded)",
		correct, scored, acc, unparsed)
	if acc < 0.75 {
		t.Errorf("classifier accuracy %.2f below 0.75 — prompt may need work", acc)
	}
}

func TestRelationClassifierLiveBatch(t *testing.T) {
	if !ai.LiveTestsEnabled() {
		t.Skip("live LLM test makes billable harness calls; set GHOST_LIVE_TESTS=1 to run")
	}
	ctx := context.Background()
	cli := ai.NewSourceProviderForSource(liveTestSource(), "", "", "", "")
	if !cli.Available() {
		t.Skip("no LLM CLI (claude/opencode/codex/goose) available; skipping live batch test")
	}
	cls := NewRelationClassifier(cli)
	// and the 8-pair prompt is exercised by real passes.
	cls.batchSize = 3

	pairs := make([]Candidate, len(liveRelationCases))
	for i, c := range liveRelationCases {
		pairs[i] = candidateOf(c)
	}
	got, err := cls.ClassifyBatch(ctx, pairs)
	if err != nil {
		t.Fatalf("ClassifyBatch: %v", err)
	}
	if len(got) != len(liveRelationCases) {
		t.Fatalf("got %d verdicts for %d pairs", len(got), len(liveRelationCases))
	}
	correct := 0
	for i, c := range liveRelationCases {
		verdict := "ok"
		if got[i] != c.want {
			verdict = "MISS"
		} else {
			correct++
		}
		t.Logf("[%s] want=%v got=%v  %s  newer=%q", verdict, c.want, got[i], c.name, c.newer)
	}
	acc := float64(correct) / float64(len(liveRelationCases))
	t.Logf("batched relation classifier accuracy: %d/%d = %.2f in %d call(s)",
		correct, len(liveRelationCases), acc, cls.Calls())
	// fallback path instead.
	wantCalls := (len(liveRelationCases) + 2) / 3
	if cls.Calls() != wantCalls {
		t.Errorf("batched path fell back: %d calls for %d pairs at batchSize 3, want %d", cls.Calls(), len(liveRelationCases), wantCalls)
	}
	if acc < 0.75 {
		t.Errorf("batched classifier accuracy %.2f below 0.75", acc)
	}
}

func candidateOf(c relationCase) Candidate {
	return Candidate{
		NewerID: "newer", NewerContent: c.newer, NewerCreatedAt: c.newerCreated,
		OlderID: "older", OlderContent: c.older, OlderCreatedAt: c.olderCreated,
	}
}

// the canonical fourth verdict exists (#641): only the model's own REVERSED word
func TestRelationClassifierParsesReversedVerdict(t *testing.T) {
	cases := []struct {
		resp string
		want Relation
	}{
		{"REVERSED", RelationReversed},
		{"reversed.", RelationReversed},
		{"**REVERSED**", RelationReversed},
		{"REVERSED — the OLDER note supersedes the NEWER one", RelationReversed},
		// is prose, and acting on it deletes a live link (#649 re-review): these
		// two replies both parse as REVERSED today.
		{"It might look REVERSED at first, but the newer note updates the older one: SUPERSEDES | replaced: it was still on 14", RelationSupersedes},
		{"Not a case of REVERSED ordering. SUPERSEDES | replaced: the harness is gone", RelationSupersedes},
		// is the whole rule.
		{"Actually REVERSED — the newer note supersedes the older one | replaced: the old pin is retired", RelationSupersedes},
		{"The newer note supersedes the older one. | replaced: the value was 5", RelationSupersedes},
		// when the reply names both roles.
		{"NEITHER — the OLDER note does not supersede the NEWER note", RelationNeither},
		{"CAUSES — the OLDER note's rationale led to the NEWER note", RelationCauses},
		// re-billed the pair on every pass forever (#649 review).
		{"There is no doubt: SUPERSEDES | replaced: the count is now per-block", RelationSupersedes},
		{"There is no CAUSES relationship; the newer note replaces the older - SUPERSEDES | replaced: the flag became --v2", RelationSupersedes},
		{"SUPERSEDES - the OLDER note's value is replaced, per the NEWER note | replaced: the value was 5s", RelationSupersedes},
		{"SUPERSEDES: the old Postgres 14 cluster is replaced by the new one. | replaced: it ran release 14", RelationSupersedes},
		{"NEITHER", RelationNeither},
		{"CAUSES", RelationCauses},
	}
	for _, c := range cases {
		cls := NewRelationClassifier(&fakeProvider{resp: c.resp})
		got, err := cls.Classify(context.Background(), Candidate{NewerContent: "n", OlderContent: "o"})
		if err != nil {
			t.Fatalf("Classify(%q): unexpected error: %v", c.resp, err)
		}
		if got != c.want {
			t.Errorf("Classify(%q) = %v, want %v", c.resp, got, c.want)
		}
	}
	for _, resp := range []string{
		"NEVER REVERSED",
		"not NEITHER, not CAUSES, not SUPERSEDES",
		"The verdict here is REVERSED",
		"The OLDER note supersedes the NEWER one.",
		"older supersedes newer",
		"OLDER: superseded by NEWER",
	} {
		cls := NewRelationClassifier(&fakeProvider{resp: resp})
		if got, err := cls.Classify(context.Background(), Candidate{NewerContent: "n", OlderContent: "o"}); err == nil {
			t.Errorf("Classify(%q) = %v, want an unparseable error", resp, got)
		}
	}
}

// (#649 review): that reinstates the backwards link this whole change exists to
// memory demoted on every subsequent search.

// unparseable verdict writes no cache row.

func TestRelationClassifierRetriesOneFailedCall(t *testing.T) {
	fp := &flakyProvider{resp: "1: NEITHER\n2: NEITHER", fails: 1}
	cls := NewRelationClassifier(fp)
	cls.SetRetryDelay(time.Millisecond)
	var buf strings.Builder
	cls.SetLogger(slog.New(slog.NewTextHandler(&buf, nil)))
	got, err := cls.ClassifyBatch(context.Background(), []Candidate{{NewerContent: "n1", OlderContent: "o1"}, {NewerContent: "n2", OlderContent: "o2"}})
	if err != nil || len(got) != 2 || got[0] != RelationNeither || got[1] != RelationNeither {
		t.Fatalf("got=%v err=%v", got, err)
	}
	if fp.calls != 2 || cls.Calls() != 2 || cls.Retries() != 1 {
		t.Fatalf("calls=%d total=%d retries=%d", fp.calls, cls.Calls(), cls.Retries())
	}
	if !strings.Contains(buf.String(), "retrying once") {
		t.Error("missing retry log")
	}
}

func TestRelationClassifierGivesUpAfterOneRetry(t *testing.T) {
	fp := &flakyProvider{fails: 2}
	cls := NewRelationClassifier(fp)
	cls.SetRetryDelay(0)
	_, err := cls.ClassifyBatch(context.Background(), []Candidate{{NewerContent: "a", OlderContent: "a"}, {NewerContent: "b", OlderContent: "b"}})
	if err == nil || !strings.Contains(err.Error(), "exit status 2") || fp.calls != 2 || cls.Retries() != 1 {
		t.Fatalf("err=%v calls=%d retries=%d", err, fp.calls, cls.Retries())
	}
}

func TestRelationClassifierDoesNotRetryACancelledCall(t *testing.T) {
	fp := &flakyProvider{fails: 1}
	cls := NewRelationClassifier(fp)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := cls.ClassifyBatch(ctx, []Candidate{{NewerContent: "a", OlderContent: "a"}, {NewerContent: "b", OlderContent: "b"}})
	if !errors.Is(err, context.Canceled) || fp.calls != 1 || cls.Retries() != 0 {
		t.Fatalf("err=%v calls=%d retries=%d", err, fp.calls, cls.Retries())
	}
}
