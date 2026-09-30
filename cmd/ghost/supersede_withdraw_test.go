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
	project, source, apply, reassess, threshold, _, withdraw, _, err := parseSupersedeArgs([]string{
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
		_, _, _, _, _, _, withdraw, _, err := parseSupersedeArgs(args)
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
	_, _, _, _, _, _, _, _, err := parseSupersedeArgs([]string{"ghost", "--reassess", "--withdraw", "a1b2c3d4", "e5f6a7b8"})
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
			{SourceID: "A1B2C3D4E5F60718293A4B5C6D7E8F90", TargetID: "00112233445566778899AABBCCDDEEFF0", Relation: "supersedes", LinkSource: "llm", Withdrawn: true},
			{SourceID: "11112222333344445555666677778888", TargetID: "00112233445566778899AABBCCDDEEFF1", Relation: "supersedes", LinkSource: "llm", WithdrawalFailed: true},
			{SourceID: "22222222333344445555666677778888", TargetID: "00112233445566778899AABBCCDDEEFF2", Relation: "supersedes", LinkSource: "llm", NotAttempted: true},
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
			{SourceID: "A1B2C3D4E5F60718293A4B5C6D7E8F90", TargetID: "00112233445566778899AABBCCDDEEFF0", TargetText: "The restore path on one spindle is safe.", Relation: "supersedes", LinkSource: "llm", Strength: 0.95},
			{SourceID: "FFEEDDCCBBAA99887766554433221100", TargetID: "00112233445566778899AABBCCDDEEFF1", TargetText: "The ingest service runs Redis 6.2.", Relation: "supersedes", LinkSource: "manual", Strength: 0.8},
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
	// The report itself does NOT name the resolve pass, and that is deliberate:
	// the caller prints the follow-up through the same formatter --reassess uses,
	// so both repairs emit one scoped command for it and there is no second
	// command string here to fall behind. #702 made the unscoped one wrong to
	// suggest, so a hint re-introduced here is a hint that is already stale.
	if strings.Contains(out, "resolve") {
		t.Errorf("the report names the resolve pass; the shared follow-up owns that:\n%s", out)
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
			{SourceID: "A1B2C3D4E5F60718293A4B5C6D7E8F90", TargetID: "00112233445566778899AABBCCDDEEFF0", TargetText: "The restore path on one spindle is safe.", Relation: "supersedes", LinkSource: "llm", Withdrawn: true},
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
	if strings.Contains(out, "resolve") {
		t.Errorf("the report names the resolve pass; the shared follow-up owns that:\n%s", out)
	}
}

// TestSupersedeWithdrawReportNamesTheRelationItResolved: --withdraw reaches a
// 'causes' edge as well as a 'supersedes' one (#833), so the report's two places
// that name the edge have to follow. The HEADER aggregates and the ROW is
// per-edge, and both are load-bearing: the header's number is the one an operator
// quotes back when they ask why a 'causes' pair could not be withdrawn, and the
// relation is what decides whether the row's withdrawal also cleared a resolution
// — only a 'supersedes' edge ever stamped the `resolved_at` the follow-up clears.
//
// The mixed case is the one a single-relation header gets wrong in a way nobody
// reads as a bug: one request, two relations, and a header that named only the
// first would still look right on the first row.
func TestSupersedeWithdrawReportNamesTheRelationItResolved(t *testing.T) {
	causes := supersede.WithdrawnLink{
		SourceID: "A1B2C3D4E5F60718293A4B5C6D7E8F90", TargetID: "00112233445566778899AABBCCDDEEFF0",
		TargetText: "The restore path on one spindle is safe.", Relation: "causes", LinkSource: "llm",
		Strength: 0.81, Withdrawn: true,
	}
	sup := supersede.WithdrawnLink{
		SourceID: "FFEEDDCCBBAA99887766554433221100", TargetID: "00112233445566778899AABBCCDDEEFF1",
		TargetText: "The ingest service runs Redis 6.2.", Relation: "supersedes", LinkSource: "llm",
		Strength: 0.8, Withdrawn: true,
	}

	out := supersedeWithdrawReport("ghost", supersede.WithdrawResult{Resolved: 1, Withdrawn: 1, Links: []supersede.WithdrawnLink{causes}}, true)
	if !strings.Contains(out, "1 causes edge(s) named") {
		t.Errorf("a 'causes' withdrawal is reported as a 'supersedes' one:\n%s", out)
	}
	if !strings.Contains(out, "[causes, source llm, strength 0.81]") {
		t.Errorf("the row does not name the relation and the edge's own source:\n%s", out)
	}

	mixed := supersedeWithdrawReport("ghost", supersede.WithdrawResult{Resolved: 2, Withdrawn: 2, Links: []supersede.WithdrawnLink{causes, sup}}, true)
	if !strings.Contains(mixed, "2 supersedes and causes edge(s) named") {
		t.Errorf("a request holding both relations names one of them:\n%s", mixed)
	}

	// A row whose relation is empty is the ZERO VALUE of a struct a caller may
	// have built by hand, and it renders 'supersedes' rather than a blank bracket:
	// a report printing an empty bracket over one of its own rows is a report that
	// cannot be matched against the follow-up rule.
	unset := supersede.WithdrawnLink{
		SourceID: "A1B2C3D4E5F60718293A4B5C6D7E8F90", TargetID: "00112233445566778899AABBCCDDEEFF0",
		LinkSource: "llm", Strength: 0.81,
	}
	blank := supersedeWithdrawReport("ghost", supersede.WithdrawResult{Resolved: 1, Links: []supersede.WithdrawnLink{unset}}, false)
	if !strings.Contains(blank, "1 supersedes edge(s) named") {
		t.Errorf("a request resolving nothing did not fall back to the default relation:\n%s", blank)
	}
	if !strings.Contains(blank, "[supersedes, source llm, strength 0.81]") {
		t.Errorf("an unset relation renders as an empty bracket rather than 'supersedes':\n%s", blank)
	}
}

// TestParseSupersedeArgsRelation is the CLI half of #833, and it was the one
// half with no test at all: three mutations — accepting any word, dropping the
// relation on the way to the core, and deleting the "needs --withdraw" refusal —
// were all GREEN.
//
// Both spellings have to work and have to refuse identically, which is the same
// pairing `checkSupersedeConsensus` already has, and for the same reason: two
// parsers for one flag means two answers to "what relation did I just pin", and an
// unknown word that fell through to the default would withdraw the OTHER edge of a
// pair that holds both.
func TestParseSupersedeArgsRelation(t *testing.T) {
	for _, args := range [][]string{
		{"ghost", "--withdraw", "a1b2c3d4", "e5f6a7b8", "--relation", "causes"},
		{"ghost", "--withdraw", "a1b2c3d4", "e5f6a7b8", "--relation=causes"},
	} {
		_, _, _, _, _, _, withdraw, relation, err := parseSupersedeArgs(args)
		if err != nil {
			t.Fatalf("parseSupersedeArgs(%v): %v", args, err)
		}
		if relation != "causes" {
			t.Errorf("parseSupersedeArgs(%v) relation = %q, want %q: both spellings are one flag", args, relation, "causes")
		}
		if len(withdraw) != 1 {
			t.Fatalf("parseSupersedeArgs(%v) resolved %d pair(s), want 1", args, len(withdraw))
		}
		// The flag is a property of the REQUEST, so it reaches every pair, carried
		// onto each one by the same mapping the CLI uses to build the call.
		pairs := toWithdrawPairs(withdraw, relation)
		if len(pairs) != 1 || pairs[0].Relation != "causes" {
			t.Errorf("toWithdrawPairs = %+v, want one pair carrying relation %q", pairs, "causes")
		}
	}

	// 'supersedes' is accepted by name, and it is the DEFAULT — so accepting it
	// explicitly has to be a no-op rather than a second path.
	_, _, _, _, _, _, _, relation, err := parseSupersedeArgs([]string{"ghost", "--withdraw", "a1b2c3d4", "e5f6a7b8", "--relation", "supersedes"})
	if err != nil || relation != "supersedes" {
		t.Errorf("the explicit default = %q, %v; want %q and no error", relation, err, "supersedes")
	}

	// No flag at all means auto-select, which the core reads as the empty relation.
	_, _, _, _, _, _, _, relation, err = parseSupersedeArgs([]string{"ghost", "--withdraw", "a1b2c3d4", "e5f6a7b8"})
	if err != nil || relation != "" {
		t.Errorf("relation = %q, %v; want the empty auto-select relation", relation, err)
	}

	// An unknown word is refused by BOTH spellings with the SAME sentence, and the
	// sentence names the two that exist: a refusal listing the accepted set is the
	// difference between a typo and a dead end.
	var refusals []string
	for _, args := range [][]string{
		{"ghost", "--withdraw", "a1b2c3d4", "e5f6a7b8", "--relation", "supersede"},
		{"ghost", "--withdraw", "a1b2c3d4", "e5f6a7b8", "--relation=contradicts"},
	} {
		_, _, _, _, _, _, _, _, perr := parseSupersedeArgs(args)
		if perr == nil {
			t.Errorf("parseSupersedeArgs(%v) accepted an unknown relation", args)
			continue
		}
		if !strings.Contains(perr.Error(), "supersedes or causes") {
			t.Errorf("parseSupersedeArgs(%v) refusal does not name the accepted relations: %v", args, perr)
		}
		refusals = append(refusals, perr.Error())
	}
	// The two spellings share one SENTENCE, which is compared with the offending
	// word factored out — echoing the word is the message's job, so the strings
	// cannot be equal and comparing them whole would be a test that could only
	// ever pass for inputs that happen to use the same misspelling.
	if len(refusals) == 2 {
		skeleton := func(s string) string {
			if i := strings.Index(s, "not "); i >= 0 {
				return s[:i+4] + "<word>"
			}
			return s
		}
		if skeleton(refusals[0]) != skeleton(refusals[1]) {
			t.Errorf("the two spellings refuse differently:\n%s\n%s", refusals[0], refusals[1])
		}
	}

	// Without --withdraw there is no edge for the flag to choose between, and it is
	// REFUSED rather than ignored: a command that took it, said nothing, and ran
	// the ordinary pass is a command whose argv does not describe it.
	_, _, _, _, _, _, _, _, err = parseSupersedeArgs([]string{"ghost", "--relation", "causes"})
	if err == nil {
		t.Error("parseSupersedeArgs accepted --relation without --withdraw")
	} else if !strings.Contains(err.Error(), "--withdraw") {
		t.Errorf("the refusal does not say what --relation needs: %v", err)
	}
}

// TestSupersedeUsageStatesBothWithdrawalOutcomes: the help is what an operator
// reads before typing the command, so a rule stated in the flag table and
// contradicted four lines below it is worse than a rule left out. #833 added
// --relation to this string and left its two neighbours describing only the
// 'supersedes' withdrawal — the flag entry promising "--apply writes the
// unsupersede history row" directly above the new --relation entry saying a
// 'causes' withdrawal writes none, and the long help saying BOTH runs print a
// follow-up.
//
// Nothing pinned this string at all, which is how it came to contradict itself.
func TestSupersedeUsageStatesBothWithdrawalOutcomes(t *testing.T) {
	usage := supersedeUsage
	for _, want := range []string{
		"--relation",          // the new flag is documented at all
		"causes",              // and its relation is named
		"unsupersede history", // the audit row is still promised, for the right relation
	} {
		if !strings.Contains(usage, want) {
			t.Errorf("the help omits %q", want)
		}
	}
	// The claims that are FALSE for a 'causes' withdrawal must not be stated
	// unconditionally. Each is matched as a sentence, because a word is not a
	// claim, and the old wording is quoted exactly so this test fails if it comes
	// back rather than merely passing while something else is wrong.
	if strings.Contains(usage, "Withdrawing an edge (--withdraw --apply, or --reassess --apply) writes the") {
		t.Error("the help says a withdrawal always writes the history row; only a 'supersedes' one does")
	}
	// "BOTH runs" is allowed to appear — it is TRUE once its subject is scoped to a
	// 'supersedes' withdrawal — so what is pinned is the subject it hangs off. The
	// unscoped sentence is quoted exactly, because a test that merely forbade the
	// phrase would fail if it came back and pass if the sentence were reworded into
	// a new false claim.
	if i := strings.Index(usage, "BOTH runs therefore print their own follow-up"); i >= 0 {
		subject := usage[:i]
		if !strings.Contains(subject, "Withdrawing a SUPERSEDES edge") {
			t.Error("the help claims BOTH runs print a follow-up without scoping it to a 'supersedes' withdrawal")
		}
	}
	// And the relation-dependent fact is stated affirmatively, so the help is not
	// merely silent about it.
	if !strings.Contains(usage, "prints NO follow-up") {
		t.Error("the help never says that a 'causes' withdrawal prints no follow-up")
	}
}

// TestWithdrawFollowUpRendersTheSharedRepairableSet: the follow-up is a SCOPED
// resolve repair, so the ids it carries decide which memories it may clear.
//
// WHICH targets those are is no longer decided here — `supersede.RepairableTargets`
// is the one rule both surfaces read, and it is pinned there
// (`TestRepairableTargets*`), because two copies of it is two answers to which
// memories a repair can clear. What is left to this package is the last step:
// that the CLI's follow-up renders that set as a scoped command, carrying every
// repairable id and no id whose edge is still live.
func TestWithdrawFollowUpRendersTheSharedRepairableSet(t *testing.T) {
	links := []supersede.WithdrawnLink{
		{SourceID: "A", TargetID: "T1", TargetProjectID: "myproj", Withdrawn: true},
		{SourceID: "B", TargetID: "T1", TargetProjectID: "myproj", Withdrawn: true}, // a second edge, one target
		{SourceID: "C", TargetID: "T2", TargetProjectID: "myproj", Withdrawn: true},
		{SourceID: "D", TargetID: "T3", TargetProjectID: "myproj", NotAttempted: true},     // never reached: still live
		{SourceID: "E", TargetID: "T4", TargetProjectID: "myproj", WithdrawalFailed: true}, // the write errored
		{SourceID: "F", TargetID: "T5", TargetProjectID: "myproj"},                         // a concurrent pass took it
	}
	got := supersede.RepairableTargets(links)
	if len(got) != 1 {
		t.Fatalf("RepairableTargets = %+v, want one project group", got)
	}
	// The group is what the shared formatter turns into a command naming exactly
	// these memories, and it is keyed by the project that owns them — the same
	// project here, and the whole difference for a `_global` call over a target
	// in a project (#786).
	followup := supersedeReassessFollowup(got[0].ProjectID, got[0].Targets, "")
	if !strings.Contains(followup, "T1") || !strings.Contains(followup, "T5") {
		t.Errorf("the follow-up does not name every repairable target:\n%s", followup)
	}
	for _, unwanted := range []string{"T3", "T4"} {
		if strings.Contains(followup, unwanted) {
			t.Errorf("the follow-up names %s, whose edge is still live and still holds it down:\n%s", unwanted, followup)
		}
	}
	if !strings.Contains(followup, "--only") {
		t.Errorf("the follow-up is not a SCOPED repair, which is the whole reason it is printed:\n%s", followup)
	}
}
