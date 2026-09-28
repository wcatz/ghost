package main

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/supersede"
)

// TestParseSupersedeArgsWithdraw pins the command line for the targeted
// withdrawal: two operands per flag, repeatable, and — the part that is easy to
// get wrong — the operands must not be read as the project, because parseSupersedeArgs
// takes the project from any bare word.
func TestParseSupersedeArgsWithdraw(t *testing.T) {
	project, source, apply, reassess, threshold, withdraw, err := parseSupersedeArgs([]string{
		"ghost", "--withdraw", "a1b2c3d4", "e5f6a7b8", "--withdraw", "11223344", "55667788", "--apply",
	})
	if err != nil {
		t.Fatalf("parseSupersedeArgs: %v", err)
	}
	if project != "ghost" {
		t.Errorf("project = %q, want \"ghost\": the pair's operands must not be read as the project", project)
	}
	if !apply {
		t.Error("apply = false, want true")
	}
	if reassess {
		t.Error("reassess = true, want false")
	}
	if threshold != 0.80 {
		t.Errorf("threshold = %v, want the 0.80 default", threshold)
	}
	if source != "" {
		t.Errorf("source = %q, want empty", source)
	}
	want := []supersedePair{{source: "a1b2c3d4", target: "e5f6a7b8"}, {source: "11223344", target: "55667788"}}
	if len(withdraw) != len(want) {
		t.Fatalf("withdraw = %+v, want %d pair(s)", withdraw, len(want))
	}
	for i := range want {
		if withdraw[i] != want[i] {
			t.Errorf("pair %d = %+v, want %+v", i, withdraw[i], want[i])
		}
	}
}

// TestParseSupersedeArgsWithdrawOperandErrors: a flag that takes two operands
// and finds one is a mistake the reader made, and silently treating the missing
// operand as the project would run a different command than the one typed.
func TestParseSupersedeArgsWithdrawOperandErrors(t *testing.T) {
	for _, args := range [][]string{
		{"ghost", "--withdraw"},
		{"ghost", "--withdraw", "a1b2c3d4"},
		{"ghost", "--withdraw", "a1b2c3d4", "--apply"},
	} {
		_, _, _, _, _, withdraw, err := parseSupersedeArgs(args)
		if err == nil {
			t.Errorf("parseSupersedeArgs(%v) = no error, want one; it read %d pair(s)", args, len(withdraw))
			continue
		}
		if !strings.Contains(err.Error(), "--withdraw") {
			t.Errorf("parseSupersedeArgs(%v) error %q does not name the flag", args, err)
		}
	}
}

// TestParseSupersedeArgsWithdrawRefusesReassess: --reassess and --withdraw are
// two different repairs, and one command doing both has two dry-run answers.
// The reader is told which one they asked for twice.
func TestParseSupersedeArgsWithdrawRefusesReassess(t *testing.T) {
	_, _, _, _, _, _, err := parseSupersedeArgs([]string{"ghost", "--reassess", "--withdraw", "a1b2c3d4", "e5f6a7b8"})
	if err == nil {
		t.Fatal("parseSupersedeArgs accepted --reassess with --withdraw")
	}
	if !strings.Contains(err.Error(), "--reassess") || !strings.Contains(err.Error(), "--withdraw") {
		t.Errorf("the refusal does not name both flags: %v", err)
	}
}

// TestSupersedeWithdrawReportNamesNothing: a request that resolved no edge says
// nothing. It always arrived with an error, and a header reading "0 supersedes
// edge(s) named, withdrew 0" printed above that error is a report about a graph
// nobody asked about, dressed as the answer.
func TestSupersedeWithdrawReportNamesNothing(t *testing.T) {
	for _, apply := range []bool{false, true} {
		if out := supersedeWithdrawReport("ghost", supersede.WithdrawResult{}, apply); out != "" {
			t.Errorf("apply=%v: report = %q, want nothing at all", apply, out)
		}
	}
}

// TestSupersedeWithdrawReportDistinguishesTheRowsItNeverReached: under --apply
// a row can be in four states, and three of them are claims about the graph. A
// row the run never reached is still LIVE, so it must not read as "already
// gone" — that would tell the operator a concurrent pass removed an edge this
// run never touched.
func TestSupersedeWithdrawReportDistinguishesTheRowsItNeverReached(t *testing.T) {
	res := supersede.WithdrawResult{
		Resolved:  3,
		Withdrawn: 1,
		Links: []supersede.WithdrawnLink{
			{SourceID: "A1B2C3D4E5F60718293A4B5C6D7E8F90", TargetID: "00112233445566778899AABBCCDDEEFF0", LinkSource: "llm", Withdrawn: true},
			{SourceID: "11112222333344445555666677778888", TargetID: "00112233445566778899AABBCCDDEEFF1", LinkSource: "llm", WithdrawalFailed: true},
			{SourceID: "22222222333344445555666677778888", TargetID: "00112233445566778899AABBCCDDEEFF2", LinkSource: "llm", NotAttempted: true},
		},
	}
	out := supersedeWithdrawReport("ghost", res, true)
	if !strings.Contains(out, "not reached") {
		t.Errorf("a row the run never reached is not marked as such:\n%s", out)
	}
	if !strings.Contains(out, "FAILED") {
		t.Errorf("the row whose write failed is not marked:\n%s", out)
	}
	// Exactly one row may claim "already gone", and this run has no such row.
	if n := strings.Count(out, "already gone"); n != 0 {
		t.Errorf("%d row(s) claim another pass removed them, and none did:\n%s", n, out)
	}
}

// TestSupersedeWithdrawReportDryRun: a preview has to say what it would do, in
// the past tense it did not use, and point at the flag that does it.
func TestSupersedeWithdrawReportDryRun(t *testing.T) {
	res := supersede.WithdrawResult{
		Resolved: 2,
		Links: []supersede.WithdrawnLink{
			{SourceID: "A1B2C3D4E5F60718293A4B5C6D7E8F90", TargetID: "00112233445566778899AABBCCDDEEFF0", TargetText: "The restore path on one spindle is safe.", LinkSource: "llm", Strength: 0.95},
			{SourceID: "FFEEDDCCBBAA99887766554433221100", TargetID: "00112233445566778899AABBCCDDEEFF1", TargetText: "The ingest service runs Redis 6.2.", LinkSource: "manual", Strength: 0.8},
		},
	}
	out := supersedeWithdrawReport("ghost", res, false)
	if strings.Contains(out, "withdrew  ") {
		t.Errorf("a dry run claimed a withdrawal:\n%s", out)
	}
	if !strings.Contains(out, "would withdraw") {
		t.Errorf("a dry run does not say what it would do:\n%s", out)
	}
	if !strings.Contains(out, "--apply") {
		t.Errorf("a dry run does not say how to apply it:\n%s", out)
	}
	// Every named edge, with the memory it was burying: an operator withdrawing
	// an edge they believe is wrong has to be able to check that from the output.
	for _, want := range []string{"A1B2C3D4", "FFEEDDCC", "The restore path on one spindle is safe.", "The ingest service runs Redis 6.2."} {
		if !strings.Contains(out, want) {
			t.Errorf("the report omits %q:\n%s", want, out)
		}
	}
	// The edge's own source column, because it decides the follow-up step.
	if !strings.Contains(out, "llm") || !strings.Contains(out, "manual") {
		t.Errorf("the report does not name each edge's own source:\n%s", out)
	}
	// And the follow-up: the withdrawal alone does not un-bury anything.
	if !strings.Contains(out, "ghost resolve ghost --reassess") {
		t.Errorf("the report omits the resolve step that un-hides the target:\n%s", out)
	}
}

// TestSupersedeWithdrawReportApplied: under --apply the report claims only what
// it moved. An edge a concurrent pass took first is reported as already gone —
// it is not this run's write, and a report that says otherwise is claiming a
// deletion that did not happen.
func TestSupersedeWithdrawReportApplied(t *testing.T) {
	res := supersede.WithdrawResult{
		Resolved:  2,
		Withdrawn: 1,
		Links: []supersede.WithdrawnLink{
			{SourceID: "A1B2C3D4E5F60718293A4B5C6D7E8F90", TargetID: "00112233445566778899AABBCCDDEEFF0", TargetText: "The restore path on one spindle is safe.", LinkSource: "llm", Withdrawn: true},
			{SourceID: "FFEEDDCCBBAA99887766554433221100", TargetID: "00112233445566778899AABBCCDDEEFF1", TargetText: "The ingest service runs Redis 6.2.", LinkSource: "llm"},
		},
	}
	out := supersedeWithdrawReport("ghost", res, true)
	// The summary, not just a row marker: an --apply run that reported "would
	// withdraw 2" above a row marked "withdrew" would be claiming two changes
	// while reporting one, which is the mismatch resolve's repair report calls out
	// for exactly this reason.
	if !strings.Contains(out, "2 supersedes edge(s) named, withdrew 1") {
		t.Errorf("the summary does not report what the run withdrew:\n%s", out)
	}
	if !strings.Contains(out, "already gone") {
		t.Errorf("an edge this run did not move is not marked:\n%s", out)
	}
	if strings.Contains(out, "Re-run with --apply") {
		t.Errorf("an apply report asks the reader to apply again:\n%s", out)
	}
	if !strings.Contains(out, "ghost resolve ghost --reassess") {
		t.Errorf("the report omits the resolve step that un-hides the target:\n%s", out)
	}
}
