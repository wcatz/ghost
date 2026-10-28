package main

import (
	"strings"
	"testing"
)

// #1565: a token-value drop is counted beside the store refusals, so a round
// that holds both has to name every reason with its count instead of falling
// through to the store-refusal line and losing the remove-or-correct
// instruction the token-only line carries.

func TestAppliedNothingNamesATokenValueDropBesideEachSingleStoreRefusal(t *testing.T) {
	t.Run("beside a scope refusal", func(t *testing.T) {
		var out strings.Builder
		reportAppliedNothing(&out, reflectRefusals{scopeRefused: 1, credDropped: 1})
		got := out.String()
		want := "Applied: nothing — every consolidation proposal was refused: 1 merged memories whose scopes disagree, 0 claimed a memory that changed or was deleted while consolidation ran, 0 folded more decision links than one source_ref can hold, 1 held a credential value"
		if !strings.Contains(got, want) {
			t.Errorf("the mixed line does not name both reasons; want %q in:\n%s", want, got)
		}
		if !strings.Contains(got, "remove or correct it by hand") {
			t.Errorf("the mixed line lost the token-value instruction:\n%s", got)
		}
		if strings.Contains(got, "this round held a credential value") {
			t.Errorf("the token-value drop was reported on its own line beside a scope refusal:\n%s", got)
		}
	})

	t.Run("beside a concurrent change", func(t *testing.T) {
		var out strings.Builder
		reportAppliedNothing(&out, reflectRefusals{changed: 1, credDropped: 1})
		got := out.String()
		want := "Applied: nothing — every consolidation proposal was refused: 0 merged memories whose scopes disagree, 1 claimed a memory that changed or was deleted while consolidation ran, 0 folded more decision links than one source_ref can hold, 1 held a credential value"
		if !strings.Contains(got, want) {
			t.Errorf("the mixed line does not name both reasons; want %q in:\n%s", want, got)
		}
		if !strings.Contains(got, "remove or correct it by hand") {
			t.Errorf("the mixed line lost the token-value instruction:\n%s", got)
		}
	})

	t.Run("beside a same-second hold-back", func(t *testing.T) {
		var out strings.Builder
		reportAppliedNothing(&out, reflectRefusals{sameSecond: 1, credDropped: 1})
		got := out.String()
		want := "Applied: nothing — every consolidation proposal was refused: 0 merged memories whose scopes disagree, 0 claimed a memory that changed or was deleted while consolidation ran, 0 folded more decision links than one source_ref can hold, 1 were held back because their memories were written in the same second this pass started, 1 held a credential value"
		if !strings.Contains(got, want) {
			t.Errorf("the mixed line does not name both reasons; want %q in:\n%s", want, got)
		}
		if !strings.Contains(got, "remove or correct it by hand") {
			t.Errorf("the mixed line lost the token-value instruction:\n%s", got)
		}
	})

	t.Run("beside a decision-link overflow", func(t *testing.T) {
		var out strings.Builder
		reportAppliedNothing(&out, reflectRefusals{overflowed: 1, overflowLinks: 14, credDropped: 1})
		got := out.String()
		want := "Applied: nothing — every consolidation proposal was refused: 0 merged memories whose scopes disagree, 0 claimed a memory that changed or was deleted while consolidation ran, 1 folded more decision links than one source_ref can hold, 1 held a credential value"
		if !strings.Contains(got, want) {
			t.Errorf("the mixed line does not name both reasons; want %q in:\n%s", want, got)
		}
		if !strings.Contains(got, "remove or correct it by hand") {
			t.Errorf("the mixed line lost the token-value instruction:\n%s", got)
		}
		if strings.Contains(got, "folded more decision links than one source_ref can hold (") {
			t.Errorf("the token-value drop was reported on its own overflow line beside no store refusal:\n%s", got)
		}
	})
}
