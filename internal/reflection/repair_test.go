package reflection

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/ai"
)

// The repair turn (#689).
//
// The strict ops reader is right to refuse a response naming an id the run was
// never given: applying the readable half of a malformed operation list is a
// corpus rewritten as if the model had said something it did not. But refusing
// the response and DISCARDING it is not the only strict outcome, and it is a
// poor one — the measured shape is a model mistyping a single ULID (one extra
// hex character) in an otherwise correct answer, which cost the whole pass:
// --require-llm exited non-zero, and the unattended auto path fell through to
// the Jaccard-only SQLite tier, so one character threw away a consolidation of
// the entire corpus. Roughly 1 run in 3 failed this way on the free opencode
// model.
//
// So the LLM tier asks once more. The tests below pin what that may and may
// not be: a re-read under the same rules, never a repair of the id, never a
// partial application of the answer it rejected, and never more than one extra
// call — and never for a transport failure, which the reader never saw.

// opBadID is opID1 with one extra hex character: near enough to look right and
// still not one of the ids the run was given, which is the whole defect.
const opBadID = "D20E133860CC4AFE38B485AD5371BA599"

// opRepairMerge is a merge of opInput's two memories whose text introduces no
// identifier absent from its sources, so the repair answer is rejected by
// nothing at all and any failure in these tests is the repair path's.
const opRepairMerge = "the bastion SSH port 2222 fronts production in region fsn1"

// scriptedAnswer is one canned harness reply, or the failure that reply comes
// back with.
type scriptedAnswer struct {
	reply string
	err   error
}

// scriptedReflector answers a queue of canned answers, in order, and records
// every prompt it was handed. The repair turn is only observable through the
// SECOND call, so the prompts are the assertion surface: a run that never asked
// again cannot be shown to have asked. A call beyond the queue is an error
// rather than a repeat of the last answer, so an implementation that asks when
// it must not fails loudly instead of passing on a coincidence. No real CLI
// harness is ever spawned.
type scriptedReflector struct {
	answers []scriptedAnswer
	prompts []string
}

func (s *scriptedReflector) Reflect(_ context.Context, prompt string) (string, ai.TokenUsage, error) {
	s.prompts = append(s.prompts, prompt)
	i := len(s.prompts) - 1
	if i >= len(s.answers) {
		return "", ai.TokenUsage{}, fmt.Errorf("scriptedReflector: call %d was not scripted", i+1)
	}
	return s.answers[i].reply, ai.TokenUsage{}, s.answers[i].err
}

// opRepairTier builds the LLM tier over a scripted harness and hands back both,
// because a test of a second call has to read the calls.
func opRepairTier(answers ...scriptedAnswer) (*LlmConsolidator, *scriptedReflector) {
	h := &scriptedReflector{answers: answers}
	return NewLlmConsolidator(h), h
}

// TestOpTierReAsksOnceAfterARejectedResponse is the defect and the fix in one:
// the first answer names an id the run was not given, the second is a clean
// operation list, and the run SUCCEEDS on the second — reporting the repair
// turn so how often this happens is measured instead of guessed.
//
// Three things have to be true for that to be a repair rather than a loosening:
// the same prompt went back out with the reader's complaint attached, the result
// is the SECOND answer's and nothing of the first's, and the corpus still
// arrives whole (every input the accepted answer did not name is carried
// through verbatim).
func TestOpTierReAsksOnceAfterARejectedResponse(t *testing.T) {
	first := `{"learned_context":"first","ops":["merge ` + opBadID + `,` + opID2 + ` -> the bastion is reached over the mesh tunnel"]}`
	second := `{"learned_context":"second","ops":["merge ` + opID1 + `,` + opID2 + ` -> ` + opRepairMerge + `"]}`
	tier, harness := opRepairTier(scriptedAnswer{reply: first}, scriptedAnswer{reply: second})

	result, err := tier.Consolidate(context.Background(), opInput())
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if len(harness.prompts) != 2 {
		t.Fatalf("harness calls = %d, want exactly 2 (the rejected answer, then one repair)", len(harness.prompts))
	}
	if result.RepairTurns != 1 {
		t.Errorf("RepairTurns = %d, want 1: a run that had to re-read its answer has to say so", result.RepairTurns)
	}

	// The same prompt again, with the reader's exact complaint attached. Not a
	// fresh question about the corpus: the model already answered that, and the
	// only thing missing is the ids it did not copy.
	repair := harness.prompts[1]
	if !strings.HasPrefix(repair, harness.prompts[0]) {
		t.Error("the repair turn did not re-send the same prompt: it must be the original prompt, so the model sees the ids it was given")
	}
	if !strings.Contains(repair, "is not one of the memories this run was given") {
		t.Errorf("the repair prompt does not carry the reader's complaint:\n%s", repair)
	}
	if !strings.Contains(repair, opBadID) {
		t.Errorf("the repair prompt does not name the id that was refused, so the model cannot see which one to fix:\n%s", repair)
	}

	// The second answer is the result, whole, and the first answer is gone. A
	// partial application is the failure this path must not introduce, so the
	// rejected answer's claim has to be absent even though the reader's complaint
	// quoted it back to the model.
	if result.LearnedContext != "second" {
		t.Errorf("LearnedContext = %q, want the repaired answer's %q", result.LearnedContext, "second")
	}
	if _, ok := findMemory(result, opRepairMerge); !ok {
		t.Errorf("the repaired answer's merge is not in the result: %+v", result.Memories)
	}
	if _, ok := findMemory(result, "the bastion is reached over the mesh tunnel"); ok {
		t.Error("the rejected answer was applied: a repair re-reads, it does not salvage the readable half")
	}
}

// TestOpTierFailsWithTheSecondErrorWhenTheRepairFails: one turn, not a loop.
// Two malformed answers fail the run exactly as one did, and the error reported
// is the SECOND reader complaint — the one that describes the answer the user
// would see in the log — so the first complaint is reachable only through the
// WARN line.
func TestOpTierFailsWithTheSecondErrorWhenTheRepairFails(t *testing.T) {
	tier, harness := opRepairTier(
		scriptedAnswer{reply: `{"ops":["merge ` + opBadID + `,` + opID2 + ` -> x"]}`},
		scriptedAnswer{reply: `{"ops":["fold ` + opID1 + ` -> x"]}`},
	)

	_, err := tier.Consolidate(context.Background(), opInput())
	if err == nil {
		t.Fatal("two rejected answers were accepted as a consolidation")
	}
	if len(harness.prompts) != 2 {
		t.Errorf("harness calls = %d, want exactly 2 — the repair turn is one turn, not a retry loop", len(harness.prompts))
	}
	if !strings.Contains(err.Error(), `unknown operation "fold"`) {
		t.Errorf("error = %q, want the SECOND reader complaint", err)
	}
	if strings.Contains(err.Error(), "is not one of the memories this run was given") {
		t.Errorf("error = %q, want the second complaint rather than the first", err)
	}
}

// TestOpTierAsksOnceOnAValidFirstAnswer is the bound's other side: a run that
// needed no repair makes exactly one call. The extra turn is paid for by a
// rejection, never by every run.
func TestOpTierAsksOnceOnAValidFirstAnswer(t *testing.T) {
	tier, harness := opRepairTier(scriptedAnswer{
		reply: `{"learned_context":"ctx","ops":["keep ` + opID1 + `","merge ` + opID2 + `,` + opID3 + ` -> both facts"]}`,
	})

	result, err := tier.Consolidate(context.Background(), func() ReflectionInput {
		in := opInput()
		in.ExistingMemories = append(in.ExistingMemories,
			opMem(opID3, "fact", "Region fsn1 also fronts the metrics endpoint", 0.4))
		return in
	}())
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if len(harness.prompts) != 1 {
		t.Errorf("harness calls = %d, want exactly 1 for a first answer that parsed", len(harness.prompts))
	}
	if result.RepairTurns != 0 {
		t.Errorf("RepairTurns = %d, want 0 when the first answer parsed", result.RepairTurns)
	}
}

// TestOpTierDoesNotRetryATransportFailure keeps the bound honest in the
// direction that costs money: the reader never rejected anything, so there is
// nothing to re-read. A harness that died, timed out or was killed fails the
// run here, as it did before the repair turn existed — the lifecycle's own
// retry story is the stop hook's cooldown and next session, not a silent second
// billable call inside one pass.
func TestOpTierDoesNotRetryATransportFailure(t *testing.T) {
	tier, harness := opRepairTier(scriptedAnswer{err: fmt.Errorf("harness exited 137")})

	_, err := tier.Consolidate(context.Background(), opInput())
	if err == nil {
		t.Fatal("a transport failure was reported as a successful consolidation")
	}
	if !strings.Contains(err.Error(), "harness exited 137") {
		t.Errorf("error = %q, want the transport failure unchanged", err)
	}
	if len(harness.prompts) != 1 {
		t.Errorf("harness calls = %d, want exactly 1: a transport failure is not a rejected response", len(harness.prompts))
	}
}

// TestOpTierLogsTheFirstRejectionAtWarn: the repair count is a measurement, and
// a measurement nobody can see is not one. The first complaint has to reach the
// configured sink at WARN, because on the unattended path the run goes on to
// succeed and the summary's `repair: 1` says only that something was wrong, not
// what.
func TestOpTierLogsTheFirstRejectionAtWarn(t *testing.T) {
	var buf bytes.Buffer
	tier, _ := opRepairTier(
		scriptedAnswer{reply: `{"ops":["merge ` + opBadID + `,` + opID2 + ` -> x"]}`},
		scriptedAnswer{reply: `{"ops":["keep ` + opID1 + `","keep ` + opID2 + `"]}`},
	)
	tier.SetLogger(slog.New(slog.NewTextHandler(&buf, nil)))

	if _, err := tier.Consolidate(context.Background(), opInput()); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	logged := buf.String()
	if !strings.Contains(logged, "level=WARN") {
		t.Errorf("the rejected answer was not logged at WARN:\n%s", logged)
	}
	if !strings.Contains(logged, "is not one of the memories this run was given") {
		t.Errorf("the WARN line does not carry the reader's complaint:\n%s", logged)
	}
	if strings.Count(logged, "level=WARN") != 1 {
		t.Errorf("WARN records = %d, want 1: the second answer was accepted, so nothing about it is a warning:\n%s",
			strings.Count(logged, "level=WARN"), logged)
	}
}
