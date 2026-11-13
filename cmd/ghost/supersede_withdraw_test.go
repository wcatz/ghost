package main

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/supersede"
)

// TestParseSupersedeArgsWithdraw: two operands per repeatable flag, which must
// not be read as the project (any bare word is the project); a flag finding fewer
// than two operands, or combined with --reassess, is refused.
func TestParseSupersedeArgsWithdraw(t *testing.T) {
	project, _, apply, reassess, _, _, withdraw, _, err := parseSupersedeArgs([]string{
		"ghost", "--withdraw", "a1b2c3d4", "e5f6a7b8", "--withdraw", "11223344", "55667788", "--apply",
	})
	if err != nil {
		t.Fatalf("parseSupersedeArgs: %v", err)
	}
	want := []supersedePair{{source: "a1b2c3d4", target: "e5f6a7b8"}, {source: "11223344", target: "55667788"}}
	if project != "ghost" || !apply || reassess || len(withdraw) != 2 || withdraw[0] != want[0] || withdraw[1] != want[1] {
		t.Errorf("project/apply/reassess/withdraw = %q/%v/%v/%+v", project, apply, reassess, withdraw)
	}
	for _, args := range [][]string{
		{"ghost", "--withdraw"},
		{"ghost", "--withdraw", "a1b2c3d4"},
		{"ghost", "--withdraw", "a1b2c3d4", "--apply"},
		{"ghost", "--reassess", "--withdraw", "a1b2c3d4", "e5f6a7b8"},
	} {
		if _, _, _, _, _, _, _, _, err := parseSupersedeArgs(args); err == nil || !strings.Contains(err.Error(), "--withdraw") {
			t.Errorf("parseSupersedeArgs(%v) error = %v, want a refusal naming --withdraw", args, err)
		}
	}
}

// TestSupersedeWithdrawReportNamesNothing: a request that resolved no edge says
// nothing (it always arrived with an error).
func TestSupersedeWithdrawReportNamesNothing(t *testing.T) {
	for _, apply := range []bool{false, true} {
		if out := supersedeWithdrawReport("ghost", supersede.WithdrawResult{}, apply); out != "" {
			t.Errorf("apply=%v: report = %q, want nothing at all", apply, out)
		}
	}
}

// TestSupersedeWithdrawReportDistinguishesTheRowsItNeverReached: a row the run
// never reached is still live, so it must not read as "already gone".
func TestSupersedeWithdrawReportDistinguishesTheRowsItNeverReached(t *testing.T) {
	res := supersede.WithdrawResult{
		Resolved:  3,
		Withdrawn: 1,
		Links: []supersede.WithdrawnLink{
			{SourceID: "A1B2C3D4E5F60718293A4B5C6D7E8F90", TargetID: "00112233445566778899AABBCCDDEEFF0", Relation: "supersedes", LinkSource: "llm", Withdrawn: true},
			{SourceID: "11112222333344445555666677778888", TargetID: "00112233445566778899AABBCCDDEEFF1", Relation: "supersedes", LinkSource: "llm", WithdrawalFailed: true},
			{SourceID: "22222222333344445555666677778888", TargetID: "00112233445566778899AABBCCDDEEFF2", Relation: "supersedes", LinkSource: "llm", NotAttempted: true},
		},
	}
	out := supersedeWithdrawReport("ghost", res, true)
	if !strings.Contains(out, "not reached") || !strings.Contains(out, "FAILED") || strings.Contains(out, "already gone") {
		t.Errorf("the rows are not told apart:\n%s", out)
	}
}

// TestSupersedeWithdrawReportDryRunAndApplied: a preview says what it would do
// and how to apply it; an applied run claims only what it moved and reports an
// edge a concurrent pass took as already gone. Neither names the resolve pass,
// which the shared follow-up owns (#702).
func TestSupersedeWithdrawReportDryRunAndApplied(t *testing.T) {
	llm := supersede.WithdrawnLink{SourceID: "A1B2C3D4E5F60718293A4B5C6D7E8F90", TargetID: "00112233445566778899AABBCCDDEEFF0", TargetText: "The restore path on one spindle is safe.", Relation: "supersedes", LinkSource: "llm", Strength: 0.95}
	manual := supersede.WithdrawnLink{SourceID: "FFEEDDCCBBAA99887766554433221100", TargetID: "00112233445566778899AABBCCDDEEFF1", TargetText: "The ingest service runs Redis 6.2.", Relation: "supersedes", LinkSource: "manual", Strength: 0.8}

	dry := supersedeWithdrawReport("ghost", supersede.WithdrawResult{Resolved: 2, Links: []supersede.WithdrawnLink{llm, manual}}, false)
	if strings.Contains(dry, "withdrew  ") || strings.Contains(dry, "resolve") {
		t.Errorf("a dry run claimed a withdrawal or named the resolve pass:\n%s", dry)
	}
	for _, want := range []string{"would withdraw", "--apply", "A1B2C3D4", "FFEEDDCC", llm.TargetText, manual.TargetText, "llm", "manual"} {
		if !strings.Contains(dry, want) {
			t.Errorf("the dry run omits %q:\n%s", want, dry)
		}
	}

	llm.Withdrawn = true
	applied := supersedeWithdrawReport("ghost", supersede.WithdrawResult{Resolved: 2, Withdrawn: 1, Links: []supersede.WithdrawnLink{llm, manual}}, true)
	if !strings.Contains(applied, "2 supersedes edge(s) named, withdrew 1") || !strings.Contains(applied, "already gone") ||
		strings.Contains(applied, "Re-run with --apply") || strings.Contains(applied, "resolve") {
		t.Errorf("the applied report misstates what the run moved:\n%s", applied)
	}
}

// TestSupersedeWithdrawReportNamesTheRelationItResolved: --withdraw reaches a
// 'causes' edge as well as 'supersedes' (#833), so the header aggregates the
// relations and each row names its own; an unset relation renders 'supersedes'.
func TestSupersedeWithdrawReportNamesTheRelationItResolved(t *testing.T) {
	causes := supersede.WithdrawnLink{
		SourceID: "A1B2C3D4E5F60718293A4B5C6D7E8F90", TargetID: "00112233445566778899AABBCCDDEEFF0",
		TargetText: "The restore path on one spindle is safe.", Relation: "causes", LinkSource: "llm", Strength: 0.81, Withdrawn: true,
	}
	sup := supersede.WithdrawnLink{
		SourceID: "FFEEDDCCBBAA99887766554433221100", TargetID: "00112233445566778899AABBCCDDEEFF1",
		TargetText: "The ingest service runs Redis 6.2.", Relation: "supersedes", LinkSource: "llm", Strength: 0.8, Withdrawn: true,
	}
	unset := supersede.WithdrawnLink{SourceID: causes.SourceID, TargetID: causes.TargetID, LinkSource: "llm", Strength: 0.81}
	for _, tc := range []struct {
		links []supersede.WithdrawnLink
		want  []string
	}{
		{[]supersede.WithdrawnLink{causes}, []string{"1 causes edge(s) named", "[causes, source llm, strength 0.81]"}},
		{[]supersede.WithdrawnLink{causes, sup}, []string{"2 supersedes and causes edge(s) named"}},
		{[]supersede.WithdrawnLink{unset}, []string{"1 supersedes edge(s) named", "[supersedes, source llm, strength 0.81]"}},
	} {
		out := supersedeWithdrawReport("ghost", supersede.WithdrawResult{Resolved: len(tc.links), Links: tc.links}, false)
		for _, want := range tc.want {
			if !strings.Contains(out, want) {
				t.Errorf("the report does not say %q:\n%s", want, out)
			}
		}
	}
}

// TestParseSupersedeArgsRelation is the CLI half of #833: both spellings of
// --relation work and refuse identically, an empty value is refused (it would
// silently mean auto-select and withdraw the other edge of a pair holding both),
// and the flag without --withdraw is refused rather than ignored.
func TestParseSupersedeArgsRelation(t *testing.T) {
	pair := []string{"ghost", "--withdraw", "a1b2c3d4", "e5f6a7b8"}
	with := func(extra ...string) []string { return append(append([]string(nil), pair...), extra...) }

	for _, args := range [][]string{with("--relation", "causes"), with("--relation=causes")} {
		_, _, _, _, _, _, withdraw, relation, err := parseSupersedeArgs(args)
		if err != nil || relation != "causes" || len(withdraw) != 1 {
			t.Fatalf("parseSupersedeArgs(%v) = %q, %v, %d pair(s)", args, relation, err, len(withdraw))
		}
		if pairs := toWithdrawPairs(withdraw, relation); len(pairs) != 1 || pairs[0].Relation != "causes" {
			t.Errorf("toWithdrawPairs = %+v, want one pair carrying relation causes", pairs)
		}
	}
	for args, want := range map[string]string{"supersedes": "supersedes", "": ""} {
		extra := []string{}
		if args != "" {
			extra = []string{"--relation", args}
		}
		if _, _, _, _, _, _, _, relation, err := parseSupersedeArgs(with(extra...)); err != nil || relation != want {
			t.Errorf("relation for %q = %q, %v, want %q", args, relation, err, want)
		}
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{with("--relation", "supersede"), "supersedes or causes"},
		{with("--relation=contradicts"), "supersedes or causes"},
		{with("--relation", ""), "--relation requires a value"},
		{with("--relation="), "--relation requires a value"},
		{[]string{"ghost", "--relation", "causes"}, "--withdraw"},
	} {
		_, _, _, _, _, _, _, _, err := parseSupersedeArgs(tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseSupersedeArgs(%v) error = %v, want one containing %q", tc.args, err, tc.want)
		}
	}
}

// TestSupersedeUsageStatesBothWithdrawalOutcomes: the help must not state, for
// every withdrawal, what is only true of a 'supersedes' one (#833).
func TestSupersedeUsageStatesBothWithdrawalOutcomes(t *testing.T) {
	usage := supersedeUsage
	for _, want := range []string{"--relation", "causes", "unsupersede history", "prints NO follow-up"} {
		if !strings.Contains(usage, want) {
			t.Errorf("the help omits %q", want)
		}
	}
	if strings.Contains(usage, "Withdrawing an edge (--withdraw --apply, or --reassess --apply) writes the") {
		t.Error("the help says a withdrawal always writes the history row; only a 'supersedes' one does")
	}
	if i := strings.Index(usage, "BOTH runs therefore print their own follow-up"); i >= 0 && !strings.Contains(usage[:i], "Withdrawing a SUPERSEDES edge") {
		t.Error("the help claims BOTH runs print a follow-up without scoping it to a 'supersedes' withdrawal")
	}
}

// TestWithdrawFollowUpRendersTheSharedRepairableSet: the follow-up renders
// supersede.RepairableTargets as a scoped command carrying every repairable id
// and no id whose edge is still live (#786 scopes it to the target's project).
func TestWithdrawFollowUpRendersTheSharedRepairableSet(t *testing.T) {
	links := []supersede.WithdrawnLink{
		{SourceID: "A", TargetID: "T1", TargetProjectID: "myproj", Withdrawn: true},
		{SourceID: "B", TargetID: "T1", TargetProjectID: "myproj", Withdrawn: true},
		{SourceID: "D", TargetID: "T3", TargetProjectID: "myproj", NotAttempted: true},
		{SourceID: "E", TargetID: "T4", TargetProjectID: "myproj", WithdrawalFailed: true},
		{SourceID: "F", TargetID: "T5", TargetProjectID: "myproj"}, // a concurrent pass took it
	}
	got := supersede.RepairableTargets(links)
	if len(got) != 1 {
		t.Fatalf("RepairableTargets = %+v, want one project group", got)
	}
	out := supersedeReassessFollowup(got[0].ProjectID, got[0].Targets, "")
	if !strings.Contains(out, "T1") || !strings.Contains(out, "T5") || !strings.Contains(out, "--only") {
		t.Errorf("the follow-up is not a scoped repair naming every repairable target:\n%s", out)
	}
	for _, unwanted := range []string{"T3", "T4"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("the follow-up names %s, whose edge is still live:\n%s", unwanted, out)
		}
	}
}
