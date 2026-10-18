package reflection

import (
	"testing"
)

// TestVerbatimEmissionsNameTheRowTheyRestate: every emission that restates an
// input row — a keep, a pass-through, a source re-emitted because the
// grounding check refused its merge, and the drop guard's re-add — carries
// that row's id, and a merge's survivor carries none. cmd/ghost's text-keyed
// join of an emission to the ids it replaced reads it: a row the response did
// not consume can carry exactly a merge's text, and without the id its own keep
// inherited the merge's sources.
func TestVerbatimEmissionsNameTheRowTheyRestate(t *testing.T) {
	const mergeText = "the bastion SSH port 2222 fronts production in region fsn1"
	in := opInput()
	in.ExistingMemories = append(in.ExistingMemories,
		opMem(opID3, "fact", mergeText, 0.4),           // the merge's same-text twin
		opMem(opID4, "fact", "an unrelated fact", 0.3)) // never named

	t.Run("keep and pass-through", func(t *testing.T) {
		result := opRun(t, in, `{"ops":["keep `+opID3+`","merge `+opID1+`,`+opID2+` -> `+mergeText+`"]}`)
		want := map[string]int{opID3: 0, opID4: 0, "": 0}
		for _, m := range result.Memories {
			if _, ok := want[m.RestatesID]; !ok {
				t.Errorf("emission %q restates %q, which it was not given", m.Content, m.RestatesID)
				continue
			}
			want[m.RestatesID]++
			if m.RestatesID == "" && m.Content != mergeText {
				t.Errorf("emission %q names no row, but only the merge's survivor may", m.Content)
			}
		}
		for id, n := range want {
			if n != 1 {
				t.Errorf("%d emission(s) restate %q, want exactly 1 (the merge survivor is \"\")", n, id)
			}
		}
	})

	t.Run("grounding refusal re-emits its sources", func(t *testing.T) {
		result := opRun(t, in, `{"ops":["merge `+opID1+`,`+opID2+` -> the bastion moved to port 2299","keep `+opID3+`","keep `+opID4+`"]}`)
		if len(result.Refusals) != 1 {
			t.Fatalf("refusals = %+v, want the merge refused", result.Refusals)
		}
		seen := map[string]bool{}
		for _, m := range result.Memories {
			if m.RestatesID == "" {
				t.Errorf("emission %q names no row it restates", m.Content)
			}
			seen[m.RestatesID] = true
		}
		for _, id := range []string{opID1, opID2, opID3, opID4} {
			if !seen[id] {
				t.Errorf("no emission restates %s", id)
			}
		}
	})

	t.Run("drop guard re-add", func(t *testing.T) {
		retained := RetainGuardedDrops([]DroppedGuarded{{ID: opID2, Category: "fact", Content: "Production runs in region fsn1 behind Cloudflare"}})
		if len(retained) != 1 || retained[0].RestatesID != opID2 {
			t.Errorf("re-add = %+v, want it to restate %s", retained, opID2)
		}
	})
}
