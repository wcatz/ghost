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
			// A rewrite whose text says nothing of the old row does NOT remove
			// it (#549): the row is kept and demoting it afterwards is resolve
			// and supersede's job. The rewrite here moves the row to a service
			// that has nothing to do with a hardware key, so the audit cannot
			// show the old text is gone.
			name:    "rewritten into unrelated wording is kept",
			reply:   fmt.Sprintf(`{"ops":["rewrite %s -> bastion access is handled by the billing service","keep %s"]}`, seqID(1), seqID(2)),
			present: true,
		},
		{
			// The same rewrite, but the new text carries the row's substance
			// (the port and the bastion), so the audit finds a survivor and the
			// old row is not re-added beside it. This is the paraphrase-duplicate
			// case, and it is why the rule is a token audit rather than a
			// blanket refusal to dispose of anything.
			name:    "rewritten into wording that carries the substance",
			reply:   fmt.Sprintf(`{"ops":["rewrite %s -> bastion SSH on port 2222 is now opened with Cloudflare Access","keep %s"]}`, seqID(1), seqID(2)),
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
