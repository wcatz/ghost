package supersede

import "testing"

func TestRepairableTargetsNamesWhatTheFollowUpMustClear(t *testing.T) {
	got := RepairableTargets([]WithdrawnLink{
		{SourceID: "A", TargetID: "T1", TargetProjectID: "p", Withdrawn: true},
		{SourceID: "B", TargetID: "T1", TargetProjectID: "p", Withdrawn: true},
		{SourceID: "C", TargetID: "T2", TargetProjectID: "p", Withdrawn: true},
		{SourceID: "D", TargetID: "T3", TargetProjectID: "p", NotAttempted: true},
		{SourceID: "E", TargetID: "T4", TargetProjectID: "p", WithdrawalFailed: true},
		{SourceID: "F", TargetID: "T5", TargetProjectID: "p"},
	})
	if len(got) != 1 || got[0].ProjectID != "p" {
		t.Fatalf("got %+v", got)
	}
	want := []string{"T1", "T2", "T5"}
	if len(got[0].Targets) != len(want) {
		t.Fatalf("got %+v want %v", got[0].Targets, want)
	}
	for i := range want {
		if got[0].Targets[i] != want[i] {
			t.Fatalf("Targets[%d]=%q want %q", i, got[0].Targets[i], want[i])
		}
	}
}

func TestRepairableTargetsGroupsByTheProjectThatCanRepairThem(t *testing.T) {
	got := RepairableTargets([]WithdrawnLink{
		{SourceID: "A", TargetID: "T1", TargetProjectID: "p", Withdrawn: true},
		{SourceID: "B", TargetID: "T2", TargetProjectID: "other", Withdrawn: true},
		{SourceID: "C", TargetID: "T3", TargetProjectID: "p", Withdrawn: true},
		{SourceID: "D", TargetID: "T4", TargetProjectID: "p", NotAttempted: true},
	})
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	if got[0].ProjectID != "p" || len(got[0].Targets) != 2 || got[0].Targets[0] != "T1" || got[0].Targets[1] != "T3" {
		t.Errorf("group 0 %+v", got[0])
	}
	if got[1].ProjectID != "other" || len(got[1].Targets) != 1 || got[1].Targets[0] != "T2" {
		t.Errorf("group 1 %+v", got[1])
	}
}

func TestRepairableTargetsSkipsACausesWithdrawal(t *testing.T) {
	got := RepairableTargets([]WithdrawnLink{
		{SourceID: "A", TargetID: "T1", TargetProjectID: "p", Relation: "causes", Withdrawn: true},
		{SourceID: "B", TargetID: "T2", TargetProjectID: "p", Relation: "supersedes", Withdrawn: true},
		{SourceID: "C", TargetID: "T3", TargetProjectID: "p", Withdrawn: true},
		{SourceID: "D", TargetID: "T1", TargetProjectID: "p", Relation: "causes"},
	})
	if len(got) != 1 || len(got[0].Targets) != 2 || got[0].Targets[0] != "T2" || got[0].Targets[1] != "T3" {
		t.Fatalf("got %+v", got)
	}
	both := RepairableTargets([]WithdrawnLink{
		{SourceID: "A", TargetID: "T1", TargetProjectID: "p", Relation: "causes", Withdrawn: true},
		{SourceID: "B", TargetID: "T1", TargetProjectID: "p", Relation: "supersedes", Withdrawn: true},
	})
	if len(both) != 1 || len(both[0].Targets) != 1 || both[0].Targets[0] != "T1" {
		t.Errorf("both %+v", both)
	}
	if n := len(RepairableTargets([]WithdrawnLink{{SourceID: "A", TargetID: "T1", TargetProjectID: "p", Relation: "causes", Withdrawn: true}})); n != 0 {
		t.Errorf("got %d groups", n)
	}
}

func TestRepairableTargetsKeepsReportOrderAndDropsABlankTarget(t *testing.T) {
	got := RepairableTargets([]WithdrawnLink{
		{SourceID: "A"},
		{SourceID: "B", TargetID: "T2", TargetProjectID: "p"},
		{SourceID: "C", TargetID: "T1", TargetProjectID: "p"},
		{SourceID: "D", TargetID: "T3", TargetProjectID: "p", Withdrawn: true},
	})
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	want := []string{"T2", "T1", "T3"}
	for i := range want {
		if got[0].Targets[i] != want[i] {
			t.Fatalf("Targets[%d]=%q want %q", i, got[0].Targets[i], want[i])
		}
	}
	if n := len(RepairableTargets(nil)); n != 0 {
		t.Errorf("nil got %d", n)
	}
}
