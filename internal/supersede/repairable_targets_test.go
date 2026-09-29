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
// Three rules, and the second is the one that is easy to get backwards.
//
// A target appears once however many edges named it — the same orphan is not a
// second claim. A row whose edge is STILL LIVE is not in the list: never reached,
// or a write that errored, means the edge still points at the target, so the
// repair would report it as still asserted and clear nothing. And a row a
// CONCURRENT pass took first IS in the list: that pass left no live edge and a
// resolved_at nothing defends any more, which is exactly the state the repair
// clears, so dropping the target would leave a repairable memory out of the list
// the operator is about to run.
func TestRepairableTargetsNamesWhatTheFollowUpMustClear(t *testing.T) {
	links := []WithdrawnLink{
		{SourceID: "A", TargetID: "T1", Withdrawn: true},
		{SourceID: "B", TargetID: "T1", Withdrawn: true}, // a second edge, one target
		{SourceID: "C", TargetID: "T2", Withdrawn: true},
		{SourceID: "D", TargetID: "T3", NotAttempted: true},     // never reached: still live
		{SourceID: "E", TargetID: "T4", WithdrawalFailed: true}, // the write errored
		{SourceID: "F", TargetID: "T5"},                         // a concurrent pass took it
	}
	got := RepairableTargets(links)
	if len(got) != 3 || got[0] != "T1" || got[1] != "T2" || got[2] != "T5" {
		t.Fatalf("RepairableTargets = %v, want [T1 T2 T5]", got)
	}
}

// RepairableTargets keeps the ORDER the rows were reported in, so the report and
// the command built from it cannot disagree about which memory is which, and it
// keeps a blank TargetID out of a selector list readRefSelectors would refuse.
func TestRepairableTargetsKeepsReportOrderAndDropsABlankTarget(t *testing.T) {
	links := []WithdrawnLink{
		{SourceID: "A"},                 // no target resolved
		{SourceID: "B", TargetID: "T2"}, //
		{SourceID: "C", TargetID: "T1"}, // reported before T2, ranked after it
		{SourceID: "D", TargetID: "T3", Withdrawn: true},
	}
	got := RepairableTargets(links)
	want := []string{"T2", "T1", "T3"}
	if len(got) != len(want) {
		t.Fatalf("RepairableTargets = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("RepairableTargets[%d] = %q, want %q (report order, not ranked order)", i, got[i], want[i])
		}
	}
	if n := len(RepairableTargets(nil)); n != 0 {
		t.Errorf("RepairableTargets(nil) = %d ids, want none", n)
	}
}
