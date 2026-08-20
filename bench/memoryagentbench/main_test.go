// bench/memoryagentbench/main_test.go
package main

import (
	"testing"

	"github.com/wcatz/ghost/internal/supersede"
)

func TestAggregateOutcomes(t *testing.T) {
	outcomes := []questionOutcome{
		{QAPairID: "q0", BaselineHit1: false, BaselineHit5: true, SupersedeHit1: true, SupersedeHit5: true},
		{QAPairID: "q1", BaselineHit1: true, BaselineHit5: true, SupersedeHit1: true, SupersedeHit5: true},
		{QAPairID: "q2", BaselineHit1: false, BaselineHit5: false, SupersedeHit1: false, SupersedeHit5: true},
		{QAPairID: "q3", BaselineHit1: false, BaselineHit5: false, SupersedeHit1: false, SupersedeHit5: false},
	}
	res := supersede.Result{Candidates: 10, Confirmed: 3, Created: 3}

	got := aggregateOutcomes("demo-x", outcomes, res)

	if got.Source != "demo-x" || got.Questions != 4 {
		t.Fatalf("got source=%q questions=%d", got.Source, got.Questions)
	}
	if got.BaselineAcc1 != 0.25 {
		t.Errorf("BaselineAcc1 = %v, want 0.25", got.BaselineAcc1)
	}
	if got.BaselineAcc5 != 0.5 {
		t.Errorf("BaselineAcc5 = %v, want 0.5", got.BaselineAcc5)
	}
	if got.SupersedeAcc1 != 0.5 {
		t.Errorf("SupersedeAcc1 = %v, want 0.5", got.SupersedeAcc1)
	}
	if got.SupersedeAcc5 != 0.75 {
		t.Errorf("SupersedeAcc5 = %v, want 0.75", got.SupersedeAcc5)
	}
	if got.Candidates != 10 || got.Confirmed != 3 || got.Created != 3 {
		t.Errorf("classifier counts not passed through: %+v", got)
	}
}

func TestAggregateOutcomesEmpty(t *testing.T) {
	got := aggregateOutcomes("demo-empty", nil, supersede.Result{})
	if got.Questions != 0 || got.BaselineAcc1 != 0 || got.SupersedeAcc5 != 0 {
		t.Errorf("expected all-zero result for no outcomes, got %+v", got)
	}
}
