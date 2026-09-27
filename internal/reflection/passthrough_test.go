package reflection

import (
	"fmt"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// seqID builds the 32-character id shape a stored memory has, ending in n. Typed
// by hand these are easy to get a character wrong, and a wrong id fails the
// parse rather than the assertion, which reads as a parser bug.
func seqID(n int) string {
	return strings.Repeat("A", 31) + string(rune('0'+n))
}

// passThroughResult runs one ops response the way the tier does, then applies the
// drop guard the way cmd/ghost does, and reports the content the apply would
// actually write. The question these tests ask is never "what did the model say"
// but "what is left in the corpus afterwards", because that is the only thing
// the guard can protect.
func passThroughResult(t *testing.T, input ReflectionInput, reply string) (written map[string]bool, drops []DroppedGuarded) {
	t.Helper()
	result := opRun(t, input, reply)
	drops = AuditGuardedDrops(input, result)
	written = make(map[string]bool, len(result.Memories)+len(drops))
	for _, d := range RetainGuardedDrops(drops) {
		written[d.Content] = true
	}
	for _, m := range result.Memories {
		written[m.Content] = true
	}
	return written, drops
}

// TestUnnamedMemoryIsCarriedThroughEvenWhenAnUnrelatedOutputExplainsIt is the
// data-loss fix. The prompt promises that an id the harness never names is kept
// verbatim, and it was not: such a memory was never emitted, so the only thing
// that could save it was the drop guard — and the guard saves a memory only when
// it FAILS to recognise a survivor. An unrelated output sharing 45% of its tokens
// was enough to make the guard call it absorbed, and a memory the guard does not
// flag is a memory the apply deletes with nothing reported. The corpus-wide shape
// is an SSH port note next to a survivor that happens to share "SSH", "bastion"
// and "port".
func TestUnnamedMemoryIsCarriedThroughEvenWhenAnUnrelatedOutputExplainsIt(t *testing.T) {
	ssh := "SSH to the bastion goes through port 2222, not 22"
	in := ReflectionInput{ExistingMemories: []memory.Memory{
		{ID: seqID(1), Category: "gotcha", Content: ssh},
		{ID: seqID(2), Category: "fact", Content: "Production runs in region fsn1 behind Cloudflare"},
		{ID: seqID(3), Category: "gotcha", Content: "the bastion firewall drops packets over 1400 bytes"},
	}}
	// The harness consolidated the two other memories and never mentioned the SSH
	// note. Nothing explains it, which is the shape a token guard would have
	// caught; the survivor sharing most of its words is a different memory.
	reply := fmt.Sprintf(`{"ops":["merge %s,%s -> the bastion firewall drops packets over 1400 bytes, so the production path to fsn1 is unreliable"]}`,
		seqID(2), seqID(3))

	written, drops := passThroughResult(t, in, reply)
	if !written[ssh] {
		t.Fatalf("the unnamed SSH memory was deleted by the apply; corpus left: %v", written)
	}
	// The SSH note must not be in the guard's findings either: it is carried
	// through by the tier, not rescued by the guard. The merge source the merge
	// under-covered is a different case and is still flagged.
	for _, d := range drops {
		if d.Content == ssh {
			t.Errorf("the unnamed memory was left to the guard instead of carried through: %+v", drops)
		}
	}
}

// TestUnnamedMemoryIsCarriedThroughVerbatim: the pass-through is a keep, not a
// paraphrase. It has to re-emit the stored row byte for byte or the exact-content
// reuse does not fire and the memory loses its id, embedding, links and age — the
// identity churn #639 opened with, reintroduced through the back door.
func TestUnnamedMemoryIsCarriedThroughVerbatim(t *testing.T) {
	in := ReflectionInput{ExistingMemories: []memory.Memory{
		{ID: seqID(1), Category: "gotcha", Content: "SSH to the bastion goes through port 2222, not 22", Importance: 0.9, Tags: []string{"ssh", "port"}},
		{ID: seqID(2), Category: "fact", Content: "Production runs in region fsn1 behind Cloudflare"},
	}}
	result := opRun(t, in, fmt.Sprintf(`{"ops":["keep %s"]}`, seqID(2)))

	var passed *ReflectMemory
	for i := range result.Memories {
		if result.Memories[i].Content == in.ExistingMemories[0].Content {
			passed = &result.Memories[i]
		}
	}
	if passed == nil {
		t.Fatalf("the unnamed memory was not carried through: %+v", result.Memories)
	}
	if passed.Category != "gotcha" || passed.Importance != 0.9 || strings.Join(passed.Tags, ",") != "ssh,port" {
		t.Errorf("the pass-through retyped the row: %+v", *passed)
	}
	// A carried-through memory was not rewritten and not merged, so it is neither
	// exempt from the guard nor scored against the union.
	if drops := AuditGuardedDrops(in, result); len(drops) != 0 {
		t.Errorf("a carried-through memory was flagged for re-add: %+v", drops)
	}
}

// TestOnlyAnExplicitOperationRemovesAMemory is the whole rule in one test: a
// memory leaves the corpus when its id is named as a merge source, named for a
// rewrite, or named in a drop with a reason — and by nothing else. Silence is not
// consent, so the ways a response can be silent (never naming an id, and naming it
// only in a drop) are the two things checked here.
func TestOnlyAnExplicitOperationRemovesAMemory(t *testing.T) {
	const ssh = "SSH to the bastion goes through port 2222, not 22"
	in := ReflectionInput{ExistingMemories: []memory.Memory{
		{ID: seqID(1), Category: "gotcha", Content: ssh},
		{ID: seqID(2), Category: "fact", Content: "Production runs in region fsn1 behind Cloudflare"},
	}}

	for _, tc := range []struct {
		name    string
		reply   string
		present bool
	}{
		{
			name:    "omitted",
			reply:   fmt.Sprintf(`{"ops":["keep %s"]}`, seqID(2)),
			present: true,
		},
		{
			// The guard still re-adds a drop it cannot corroborate, so an obsolete
			// claim on its own does not delete: a surviving memory has to explain
			// where the knowledge went.
			name:    "dropped obsolete and uncorroborated",
			reply:   fmt.Sprintf(`{"ops":["drop %s reason: obsolete","keep %s"]}`, seqID(1), seqID(2)),
			present: true,
		},
		{
			// A merge that carries the SSH note's substance really does replace
			// it: the prompt asks for exactly this, and the guard is satisfied.
			name:    "merged away",
			reply:   fmt.Sprintf(`{"ops":["merge %s,%s -> the bastion on port 2222, not 22, fronts production in region fsn1"]}`, seqID(1), seqID(2)),
			present: false,
		},
		{
			// A rewrite replaces the row whatever its wording: re-adding the old
			// text beside the new one would be a duplicate, not a save.
			name:    "rewritten away",
			reply:   fmt.Sprintf(`{"ops":["rewrite %s -> bastion access is handled by the billing service","keep %s"]}`, seqID(1), seqID(2)),
			present: false,
		},
		{
			// A merge that summarises away most of one of its sources is a merge
			// the model got wrong, and the guard puts that source back.
			name:    "merged away but the merge lost its substance",
			reply:   fmt.Sprintf(`{"ops":["merge %s,%s -> Cloudflare fronts production in region fsn1"]}`, seqID(1), seqID(2)),
			present: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			written, _ := passThroughResult(t, in, tc.reply)
			if got := written[ssh]; got != tc.present {
				t.Errorf("SSH memory present = %v, want %v; corpus left: %v", got, tc.present, written)
			}
		})
	}
}

// TestExplicitDropIsHonouredWhenTheCorpusCorroboratesIt: an obsolete drop the
// corpus can corroborate is the one deletion a consolidation pass makes without
// --allow-drops, because a surviving memory explains where the knowledge went.
// The pass-through must not quietly reinstate it, or "drop" is not an operation.
func TestExplicitDropIsHonouredWhenTheCorpusCorroboratesIt(t *testing.T) {
	in := ReflectionInput{ExistingMemories: []memory.Memory{
		{ID: seqID(1), Category: "gotcha", Content: "bastion SSH uses port 2222 with a hardware key"},
		{ID: seqID(2), Category: "fact", Content: "bastion SSH now uses port 2222 through the hardware key fob"},
	}}
	reply := fmt.Sprintf(`{"ops":["drop %s reason: obsolete","keep %s"]}`, seqID(1), seqID(2))

	written, drops := passThroughResult(t, in, reply)
	if written[in.ExistingMemories[0].Content] {
		t.Errorf("an obsolete drop the corpus explains was reinstated anyway: %v", written)
	}
	if len(drops) != 0 {
		t.Errorf("a corroborated drop was flagged: %+v", drops)
	}
}

// TestOperationIdMayEchoThePromptsFieldLabel: the prompt renders each memory as
// "- id:<id>", and a model that copies the label it was shown produces
// "keep id:01AAA". Failing a whole consolidation over a label Ghost printed itself
// would cost a pass over a cosmetic slip, so the label is accepted and stripped
// wherever an id is read.
func TestOperationIdMayEchoThePromptsFieldLabel(t *testing.T) {
	in := ReflectionInput{ExistingMemories: []memory.Memory{
		{ID: seqID(1), Category: "gotcha", Content: "bastion SSH uses port 2222 with a hardware key"},
		{ID: seqID(2), Category: "fact", Content: "operator access is now fronted by Cloudflare Access"},
	}}
	for _, reply := range []string{
		fmt.Sprintf(`{"ops":["keep id:%s","keep id: %s"]}`, seqID(1), seqID(2)),
		fmt.Sprintf(`{"ops":["keep ID:%s","drop ID:%s reason: superseded by id:%s"]}`,
			strings.ToLower(seqID(1)), seqID(2), seqID(1)),
		fmt.Sprintf(`{"ops":["merge id:%s,id:%s -> operator access is now fronted by Cloudflare Access, and bastion SSH uses port 2222"]}`,
			seqID(1), seqID(2)),
	} {
		resp, err := parseOpResponse(reply)
		if err != nil {
			t.Errorf("parseOpResponse(%s): %v", reply, err)
			continue
		}
		if _, err := executeOps(resp, in, nil); err != nil {
			t.Errorf("executeOps(%s): %v", reply, err)
		}
	}
}

// TestMergeWithCrossRepoTextIsAGlobalCandidate covers the --promote-globals half
// of the contract: a merge whose text reads as cross-repo knowledge is the only
// thing an LLM tier can offer the promotion path now, since the model no longer
// states a scope. Without it, `ghost reflect --promote-globals` would have nothing
// to promote from a harness-backed run.
func TestMergeWithCrossRepoTextIsAGlobalCandidate(t *testing.T) {
	in := ReflectionInput{ExistingMemories: []memory.Memory{
		{ID: seqID(1), Category: "convention", Content: "tabs, not spaces, in every repository we touch"},
		{ID: seqID(2), Category: "convention", Content: "run the linter before pushing from any repo"},
	}}
	result := opRun(t, in, fmt.Sprintf(
		`{"ops":["merge %s,%s -> across all repos, use tabs not spaces and run the linter before pushing"]}`,
		seqID(1), seqID(2)))

	if len(result.Memories) != 1 {
		t.Fatalf("got %d memories, want the one merge: %+v", len(result.Memories), result.Memories)
	}
	if result.Memories[0].Scope != "global" {
		t.Fatalf("scope = %q, want global — a cross-repo merge is the promotion path's only input now",
			result.Memories[0].Scope)
	}
}
