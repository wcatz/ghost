package reflection

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/memory"
)

// The repair turn (#689).
//
// The strict ops reader is right to refuse a response naming an id the run was
// never given: applying the readable half of a malformed operation list is a
// corpus rewritten as if the model had said something it did not. But refusing
// the response and DISCARDING it is not the only strict outcome, and it is a
// poor one — the measured shape is a model mistyping a single ULID (one extra
// hex character) in an otherwise correct answer, which ended the pass. It cost
// differently by invocation, and the two are easy to conflate: --require-llm,
// which is what the unattended lifecycle phase passes, omits the mechanical tier
// entirely, so the run exited non-zero with nothing written; a manual `auto` run
// fell through to the Jaccard-only tier instead. Roughly 1 run in 3 failed this
// way on the free opencode model.
//
// So the LLM tier asks once more. The tests below pin what that may and may
// not be: a re-read under the same rules, never a repair of the id, never a
// partial application of the answer it rejected, and never more than one extra
// call — and never for a transport failure, which the reader never saw.

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

// TestOpTierWithholdsTheRejectedLineFromTheLog: the WARN line is a fourth sink
// the tier writes to, and the three that report a proposal all go through
// previewContent precisely because a log outlives the run. A refusal quotes the
// offending operation line back, and for a rewrite or a merge that line ends in
// the model's own replacement text — prose over stored memory, and in the worst
// case a value that should never reach a file anybody greps. The log keeps the
// reason and the id; the line is withheld.
func TestOpTierWithholdsTheRejectedLineFromTheLog(t *testing.T) {
	// A credential-shaped value in the stored memory, restated by the model in
	// the operation that the reader is about to refuse. The shape is what the
	// store's own guard looks for, so this is the value that must not be copied
	// into an append-only log by a warning nobody reads closely.
	const secret = "ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8"
	stored := opMem(opID1, "fact", "the deploy token is "+secret, 0.5)
	badID := opID1 + "X"
	// The arithmetic, because the assertion below is only a real probe if the
	// window the code applies is wide enough to see what it looks for:
	// clipOpLine keeps 80 runes of the line, "rewrite " + the 33-rune badID +
	// " -> " is 45 of them, so 35 of the value's 40 runes survive the clip — and
	// the probe below is the first 32. Three runes of margin is all it is, which
	// is why the probe is stated as a prefix and not as the whole value: a
	// fixture with prose in front of the value, or a longer id, would truncate it
	// and the assertion would pass whatever the log did.
	reply := `{"ops":["rewrite ` + badID + ` -> ` + secret + `"]}`

	var buf bytes.Buffer
	tier, _ := opRepairTier(
		scriptedAnswer{reply: reply},
		scriptedAnswer{reply: `{"ops":["keep ` + opID1 + `"]}`},
	)
	tier.SetLogger(slog.New(slog.NewTextHandler(&buf, nil)))

	if _, err := tier.Consolidate(context.Background(), ReflectionInput{
		ProjectName: "ghost", ExistingMemories: []memory.Memory{stored},
	}); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	logged := buf.String()
	if !strings.Contains(logged, "level=WARN") {
		t.Fatalf("the rejection was not logged at all, so this test proves nothing:\n%s", logged)
	}
	// The part of the value inside clipOpLine's 80-rune window, and what the log
	// must not contain. The full token would not be a real probe: the clip cuts
	// its tail, so the check would pass whatever the log did.
	if strings.Contains(logged, secret[:32]) {
		t.Errorf("the WARN line carries the value the rejected operation quoted:\n%s", logged)
	}
	// Fail closed rather than silently: a log line with no line number and no
	// reason is not a diagnostic, so the withholding has to be visible as one.
	if !strings.Contains(logged, "withheld") {
		t.Errorf("the WARN line does not say the operation line was withheld:\n%s", logged)
	}
	// The same gate has to hold for a reason that quotes the model rather than
	// the line: a `drop <id> reason: <value>` is refused BECAUSE the tail is
	// free-form, so the value is in the reason and the withheld line never
	// covered it. A rewrite fixture above cannot catch that — its replacement
	// text sits inside the line, which IS withheld.
	t.Run("free-form drop reason", func(t *testing.T) {
		var buf2 bytes.Buffer
		tier2, _ := opRepairTier(
			scriptedAnswer{reply: `{"ops":["drop ` + opID1 + ` reason: the deploy token is ` + secret + `"]}`},
			scriptedAnswer{reply: `{"ops":["keep ` + opID1 + `"]}`},
		)
		tier2.SetLogger(slog.New(slog.NewTextHandler(&buf2, nil)))
		if _, err := tier2.Consolidate(context.Background(), ReflectionInput{
			ProjectName: "ghost", ExistingMemories: []memory.Memory{stored},
		}); err != nil {
			t.Fatalf("Consolidate: %v", err)
		}
		if !strings.Contains(buf2.String(), "level=WARN") {
			t.Fatalf("the rejection was not logged, so this test proves nothing:\n%s", buf2.String())
		}
		if strings.Contains(buf2.String(), secret[:32]) {
			t.Errorf("the WARN line carries the value the REFUSED REASON quoted, which the withheld line never covered:\n%s", buf2.String())
		}
	})

	// The repair prompt is the one place the line is quoted on purpose: it goes
	// back to the model that wrote it, inside the prompt's own data delimiters.
	_, harness := opRepairTier(scriptedAnswer{reply: reply}, scriptedAnswer{reply: `{"ops":["keep ` + opID1 + `"]}`})
	tier2 := NewLlmConsolidator(harness)
	tier2.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := tier2.Consolidate(context.Background(), ReflectionInput{
		ProjectName: "ghost", ExistingMemories: []memory.Memory{stored},
	}); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	repair := harness.prompts[len(harness.prompts)-1]
	if !strings.Contains(repair, "quoted as data") {
		t.Errorf("the repair prompt does not tell the model the quoted complaint is data, not an instruction:\n%s", repair)
	}
	if !strings.Contains(repair, "«") {
		t.Error("the repair prompt does not delimit the quoted complaint")
	}
}
