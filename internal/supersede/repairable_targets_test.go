package supersede

import "testing"

// TestRepairableTargetsNamesWhatTheFollowUpMustClear pins the rule BOTH surfaces
// now read, in the package that owns it. It used to be pinned twice — once in
// cmd/ghost for `ghost supersede --withdraw` and once in internal/mcpserver for
// ghost_link_withdraw, which had NO test at all — over two bodies that were
// character for character the same. Two copies is two answers to "which targets
// can this repair still clear", and the CLI's copy was proved right while the
// MCP one drifted unobserved; a wrong answer here does not print a wrong report,
// it CLEARS the wrong memories.
//
// Four rules, and the second is the one that is easy to get backwards.
//
// A target appears once however many edges named it — the same orphan is not a
// second claim. A row whose edge is STILL LIVE is not in the list: never reached,
// or a write that errored, means the edge still points at the target, so the
// repair would report it as still asserted and clear nothing. And a row a
// CONCURRENT pass took first IS in the list: that pass left no live edge and a
// resolved_at nothing defends any more, which is exactly the state the repair
// clears, so dropping the target would leave a repairable memory out of the list
// the operator is about to run.
//
// The fourth is the GROUPING, and it is not presentation. A resolve repair's pool
// is ResolvedCandidates(projectID), which filters `project_id = ?`, so a selector
// resolved against one project and repaired against another is a silent no-op.
func TestRepairableTargetsNamesWhatTheFollowUpMustClear(t *testing.T) {
	links := []WithdrawnLink{
		{SourceID: "A", TargetID: "T1", TargetProjectID: "p", Withdrawn: true},
		{SourceID: "B", TargetID: "T1", TargetProjectID: "p", Withdrawn: true}, // a second edge, one target
		{SourceID: "C", TargetID: "T2", TargetProjectID: "p", Withdrawn: true},
		{SourceID: "D", TargetID: "T3", TargetProjectID: "p", NotAttempted: true},     // never reached: still live
		{SourceID: "E", TargetID: "T4", TargetProjectID: "p", WithdrawalFailed: true}, // the write errored
		{SourceID: "F", TargetID: "T5", TargetProjectID: "p"},                         // a concurrent pass took it
	}
	got := RepairableTargets(links)
	if len(got) != 1 || got[0].ProjectID != "p" {
		t.Fatalf("RepairableTargets = %+v, want one group for p", got)
	}
	want := []string{"T1", "T2", "T5"}
	if len(got[0].Targets) != len(want) {
		t.Fatalf("RepairableTargets = %+v, want %v", got, want)
	}
	for i := range want {
		if got[0].Targets[i] != want[i] {
			t.Fatalf("RepairableTargets[0].Targets[%d] = %q, want %q", i, got[0].Targets[i], want[i])
		}
	}
}

// TestRepairableTargetsGroupsByTheProjectThatCanRepairThem is the case the
// grouping exists for, and it is the one a flat list got wrong: a
// `ghost supersede _global --withdraw` over a pair whose source was promoted into
// `_global` and whose target stayed in a project. The command printed
// `ghost resolve _global --reassess --only <target> --apply`, which resolves the
// selector (a `_global` ref scope reaches a project memory) and then finds
// nothing in the pool — so every id came back a miss under a block promising a
// clear. The group is the TARGET's project, and it is one group per project even
// when a single request spans two.
func TestRepairableTargetsGroupsByTheProjectThatCanRepairThem(t *testing.T) {
	got := RepairableTargets([]WithdrawnLink{
		{SourceID: "A", TargetID: "T1", TargetProjectID: "p", Withdrawn: true},
		{SourceID: "B", TargetID: "T2", TargetProjectID: "other", Withdrawn: true},
		{SourceID: "C", TargetID: "T3", TargetProjectID: "p", Withdrawn: true},
		{SourceID: "D", TargetID: "T4", TargetProjectID: "p", NotAttempted: true},
	})
	if len(got) != 2 {
		t.Fatalf("RepairableTargets = %+v, want two groups", got)
	}
	if got[0].ProjectID != "p" || len(got[0].Targets) != 2 || got[0].Targets[0] != "T1" || got[0].Targets[1] != "T3" {
		t.Errorf("the first group = %+v, want project p holding T1 and T3 in report order", got[0])
	}
	if got[1].ProjectID != "other" || len(got[1].Targets) != 1 || got[1].Targets[0] != "T2" {
		t.Errorf("the second group = %+v, want project other holding T2", got[1])
	}
}

// RepairableTargets keeps the ORDER the rows were reported in, so the report and
// the command built from it cannot disagree about which memory is which, and it
// keeps a blank TargetID out of a selector list readRefSelectors would refuse.
func TestRepairableTargetsKeepsReportOrderAndDropsABlankTarget(t *testing.T) {
	links := []WithdrawnLink{
		{SourceID: "A"}, // no target resolved
		{SourceID: "B", TargetID: "T2", TargetProjectID: "p"},
		{SourceID: "C", TargetID: "T1", TargetProjectID: "p"}, // reported before T2, ranked after it
		{SourceID: "D", TargetID: "T3", TargetProjectID: "p", Withdrawn: true},
	}
	got := RepairableTargets(links)
	if len(got) != 1 {
		t.Fatalf("RepairableTargets = %+v, want one group", got)
	}
	want := []string{"T2", "T1", "T3"}
	if len(got[0].Targets) != len(want) {
		t.Fatalf("RepairableTargets = %+v, want %v", got, want)
	}
	for i := range want {
		if got[0].Targets[i] != want[i] {
			t.Fatalf("RepairableTargets[0].Targets[%d] = %q, want %q (report order, not ranked order)", i, got[0].Targets[i], want[i])
		}
	}
	if n := len(RepairableTargets(nil)); n != 0 {
		t.Errorf("RepairableTargets(nil) = %d groups, want none", n)
	}
}
