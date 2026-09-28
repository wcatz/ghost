package main

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/resolve"
)

// TestResolveMarkReportDryRunIsAPreview: a dry run has to say what it WOULD do,
// in the tense it did not use, and point at the flag that does it. A dry run that
// said "marked" would be claiming a write that did not happen, which is the whole
// reason the default is a preview — the judgement being trusted is the caller's
// and the corpus is what has to survive being wrong about it.
func TestResolveMarkReportDryRunIsAPreview(t *testing.T) {
	// The rows carry Marked, which a real dry run never sets — and that is
	// deliberate here. The report has to take its "did this write?" from apply,
	// because a formatter that trusted the rows would print a repair for a run
	// that stamped nothing, and the marker a reader trusts is the one that cannot
	// be right by accident. Constructing it this way is the only way to observe
	// that: from a real dry run the assertion below would pass under either
	// implementation.
	res := resolve.MarkResult{
		Resolved: 2,
		Memories: []resolve.MarkedMemory{
			{ID: "A1B2C3D4E5F60718293A4B5C6D7E8F90", Category: "changelog", Content: "the staging relay port is 2222", Marked: true},
			{ID: "FFEEDDCCBBAA99887766554433221100", Category: "gotcha", Content: "the restore path on one spindle is safe", Marked: true},
		},
	}
	out := resolveMarkReport("myproj", res, false)
	if !strings.Contains(out, "would mark resolved 2") {
		t.Errorf("a dry run does not say what it would do:\n%s", out)
	}
	if strings.Contains(out, "marked resolved 2") {
		t.Errorf("a dry run claimed a stamp it did not write:\n%s", out)
	}
	if !strings.Contains(out, "--apply") {
		t.Errorf("a dry run does not say how to apply it:\n%s", out)
	}
	// Every named memory with its own text: an operator burying a memory has to be
	// able to confirm from the output that it was the one they meant, and a
	// memory buried by mistake is invisible afterwards.
	for _, want := range []string{"A1B2C3D4", "FFEEDDCC", "the staging relay port is 2222", "the restore path on one spindle is safe"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report omits %q:\n%s", want, out)
		}
	}
	// And NO follow-up: nothing was stamped, so a repair command would name
	// memories the run never touched.
	if strings.Contains(out, "--reassess") {
		t.Errorf("a dry run printed a repair for a stamp it did not write:\n%s", out)
	}
}

// TestResolveMarkReportNamesEachNoOpForWhatItIs: the four per-row states, each of
// which is a different thing and only one of which is the issue's "already
// resolved". A report that grouped the other three under "not marked" would leave
// an operator unable to tell a row they may retry from one they may not — a pin
// is an instruction to keep a memory visible, a standing category is knowledge
// resolve never buries, and a failed write is this run's own broken promise.
func TestResolveMarkReportNamesEachNoOpForWhatItIs(t *testing.T) {
	res := resolve.MarkResult{
		Resolved:        4,
		Marked:          1,
		AlreadyResolved: 1,
		Pinned:          1,
		ExemptCategory:  1,
		Memories: []resolve.MarkedMemory{
			{ID: "11111111111111111111111111111111", Category: "changelog", Content: "shipped in 0.36.0", Marked: true},
			{ID: "22222222222222222222222222222222", Category: "changelog", Content: "already buried by an earlier pass", AlreadyResolved: true},
			{ID: "33333333333333333333333333333333", Category: "gotcha", Content: "pinned into view on purpose", Pinned: true},
			{ID: "44444444444444444444444444444444", Category: "convention", Content: "NEVER reuse a released directory", ExemptCategory: true},
		},
	}
	out := resolveMarkReport("myproj", res, true)
	for _, want := range []string{"marked", "already resolved", "pinned", "standing category"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not mark a row %q:\n%s", want, out)
		}
	}
	// The summary counts each no-op separately rather than summing them, because
	// a single "3 not marked" is a number the reader cannot act on.
	for _, want := range []string{"1 already resolved", "1 pinned", "1 in a standing category"} {
		if !strings.Contains(out, want) {
			t.Errorf("the summary does not report %q:\n%s", want, out)
		}
	}
	// The follow-up names only the row this run stamped. A row that was already
	// resolved before the run is not something the command would change, and
	// naming it would send the operator to clear a stamp an earlier pass owns.
	if !strings.Contains(out, "11111111111111111111111111111111") {
		t.Errorf("the follow-up omits the stamped memory:\n%s", out)
	}
	if strings.Contains(out, "22222222222222222222222222222222'") {
		t.Errorf("the follow-up names a memory this run did not stamp:\n%s", out)
	}
}

// TestResolveMarkReportNamesNothingForARequestThatNamedNothing: a refused ref is
// the only way to reach an empty result, and a refusal writes nothing at all. A
// header reading "0 named, marked resolved 0" printed above that error is a
// report about a corpus nobody asked about, presented as though it were the
// answer — and it precedes the diagnostic, so it is the line a reader sees first.
// TestResolveMarkReportNeverClaimsAMarkItDidNotMake: the store re-checks
// eligibility WHERE IT WRITES, so a row that was eligible when the report's
// result was built can be ineligible by the time it is stamped — pinned or
// recategorized in between. The store declines such a row silently, because that
// is somebody else's decision rather than a failure, and nothing in the result
// distinguishes it from a row that was written.
//
// So the report's DEFAULT marker under --apply has to be the negative one. A
// report about a change that claims a change it did not make is the one thing it
// must never do: the memory is not buried, it stays in every session's ranked
// context, and an operator who was told otherwise does not go looking for it.
func TestResolveMarkReportNeverClaimsAMarkItDidNotMake(t *testing.T) {
	// Every state the switch might not know about, including a row carrying
	// none of them at all — the one a new state added tomorrow would look like.
	for _, m := range []resolve.MarkedMemory{
		{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Category: "fact", Content: "declined by the write-time guard", Declined: true},
		{ID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Category: "fact", Content: "a state this report does not know about"},
	} {
		out := resolveMarkReport("myproj", resolve.MarkResult{Resolved: 1, Memories: []resolve.MarkedMemory{m}}, true)
		if !strings.Contains(out, "not marked") {
			t.Errorf("a row the run did not stamp is not reported as unmarked:\n%s", out)
		}
		for _, claim := range []string{"  marked  ", "  would mark  "} {
			if strings.Contains(out, claim) {
				t.Errorf("the report claims %q for a row it did not stamp:\n%s", claim, out)
			}
		}
		// And no repair for it: a command to clear a stamp that was never set.
		if strings.Contains(out, "--reassess") {
			t.Errorf("the report printed a repair for a stamp it did not write:\n%s", out)
		}
	}
	// The known declines keep their own markers, which is why the default is
	// safe: nothing that names a reason falls through to it.
	res := resolve.MarkResult{Resolved: 4, Marked: 1, Pinned: 1, Declined: 1, Memories: []resolve.MarkedMemory{
		{ID: "11111111111111111111111111111111", Marked: true, Content: "written"},
		{ID: "22222222222222222222222222222222", Pinned: true, Content: "pinned"},
		{ID: "33333333333333333333333333333333", Declined: true, Content: "declined"},
		{ID: "44444444444444444444444444444444", ExemptCategory: true, Content: "standing"},
	}}
	out := resolveMarkReport("myproj", res, true)
	for _, want := range []string{"  marked  ", "  pinned  ", "  standing category  ", "not marked (no longer eligible"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not mark a row %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "1 declined") {
		t.Errorf("the summary does not count the declined row:\n%s", out)
	}
}

func TestResolveMarkReportNamesNothingForARequestThatNamedNothing(t *testing.T) {
	for _, apply := range []bool{false, true} {
		if out := resolveMarkReport("myproj", resolve.MarkResult{}, apply); out != "" {
			t.Errorf("apply=%v: report = %q, want nothing at all", apply, out)
		}
	}
}

// TestResolveMarkReportNamesAFailedWrite: a broken write is not a no-op and not a
// decline. It says FAILED, because a row that reads as anything else tells the
// operator their memory is buried when it is not.
func TestResolveMarkReportNamesAFailedWrite(t *testing.T) {
	res := resolve.MarkResult{
		Resolved: 1,
		Memories: []resolve.MarkedMemory{
			{ID: "55555555555555555555555555555555", Category: "fact", Content: "a note whose stamp failed", MarkFailed: true},
		},
	}
	out := resolveMarkReport("myproj", res, true)
	if !strings.Contains(out, "FAILED") {
		t.Errorf("a failed write is not marked as one:\n%s", out)
	}
	// And no follow-up: nothing was stamped, so there is nothing to clear.
	if strings.Contains(out, "--reassess") {
		t.Errorf("a run that stamped nothing printed a repair command:\n%s", out)
	}
}

// TestResolveMarkReportFollowUpIsScopedAndRenderedByTheSharedHelper: the inverse
// exists and the report has to name it through internal/followup, the same
// renderer the supersede repairs use. Unscoped would re-judge every resolved
// memory in the project — #698 measured that proposing to un-hide 143 rows,
// about 35% of them stale — so the command carries the ids it stamped.
func TestResolveMarkReportFollowUpIsScopedAndRenderedByTheSharedHelper(t *testing.T) {
	res := resolve.MarkResult{
		Resolved: 2,
		Marked:   2,
		Memories: []resolve.MarkedMemory{
			{ID: "A1B2C3D4E5F60718293A4B5C6D7E8F90", Category: "changelog", Content: "shipped in 0.36.0", Marked: true},
			{ID: "FFEEDDCCBBAA99887766554433221100", Category: "changelog", Content: "rolled back in 0.36.1", Marked: true},
		},
	}
	out := resolveMarkReport("myproj", res, true)
	if !strings.Contains(out, "--reassess --only") {
		t.Errorf("the follow-up is not the scoped repair:\n%s", out)
	}
	if !strings.Contains(out, "--apply") {
		t.Errorf("the follow-up does not apply the repair it names:\n%s", out)
	}
	// Both stamped ids, and the project, so the command runs as printed.
	for _, want := range []string{"ghost resolve myproj", "A1B2C3D4E5F60718293A4B5C6D7E8F90", "FFEEDDCCBBAA99887766554433221100"} {
		if !strings.Contains(out, want) {
			t.Errorf("the follow-up omits %q:\n%s", want, out)
		}
	}
}

// TestResolveMarkReportQuotesAProjectNameThatNeedsIt: a project name is free text
// — the reason internal/mcpinit sanitizes one before using it as a filename — so
// a name holding a space or a shell metacharacter is rendered through --project
// in single quotes. A bare `ghost resolve my proj --reassess` is two positionals
// and a repair that refuses; a name holding a `;` rendered bare would execute it.
func TestResolveMarkReportQuotesAProjectNameThatNeedsIt(t *testing.T) {
	res := resolve.MarkResult{
		Resolved: 1,
		Marked:   1,
		Memories: []resolve.MarkedMemory{
			{ID: "A1B2C3D4E5F60718293A4B5C6D7E8F90", Category: "changelog", Content: "shipped", Marked: true},
		},
	}
	for _, tc := range []struct{ name, project, want string }{
		{name: "a space", project: "my proj", want: "--project 'my proj'"},
		{name: "a metacharacter", project: "my;proj", want: "--project 'my;proj'"},
		{name: "a dash-leading name", project: "-x", want: "--project '-x'"},
		// One bare shell word needs no quoting, and the docs use that form
		// everywhere, so it is the form the report emits for the ordinary case.
		{name: "one word", project: "myproj", want: "ghost resolve myproj"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := resolveMarkReport(tc.project, res, true)
			if !strings.Contains(out, tc.want) {
				t.Errorf("the follow-up does not render %q:\n%s", tc.want, out)
			}
		})
	}
}

// TestResolveMarkReportNamesIdsNoFlagCanCarry: `ghost import` writes an
// artifact's ids verbatim, so an id can hold a comma or a newline. `--only`
// splits on commas, so such an id is not nameable by that flag however it is
// quoted, and the one-per-line file is the only surface that reaches it; a
// newline reaches nothing at all. An operator told nothing would run the command
// above, clear fewer memories than the run stamped, and report a repair that did
// not happen — so they are named, and which surface can reach them is said.
func TestResolveMarkReportNamesIdsNoFlagCanCarry(t *testing.T) {
	t.Run("a comma", func(t *testing.T) {
		res := resolve.MarkResult{
			Resolved: 1,
			Marked:   1,
			Memories: []resolve.MarkedMemory{
				{ID: "import,1 note", Category: "fact", Content: "shipped", Marked: true},
			},
		}
		out := resolveMarkReport("myproj", res, true)
		if !strings.Contains(out, "hold a comma") {
			t.Errorf("an id --only cannot carry is not reported as such:\n%s", out)
		}
		if !strings.Contains(out, "--only-file") {
			t.Errorf("the report does not name the surface that can carry it:\n%s", out)
		}
		if !strings.Contains(out, "import,1 note") {
			t.Errorf("the report does not print the id itself:\n%s", out)
		}
		if strings.Contains(out, "--reassess --only ") {
			t.Errorf("the report emitted an --only command for a comma-bearing id, which cannot address it:\n%s", out)
		}
	})
	t.Run("a newline", func(t *testing.T) {
		res := resolve.MarkResult{
			Resolved: 1,
			Marked:   1,
			Memories: []resolve.MarkedMemory{
				{ID: "import\n1 note", Category: "fact", Content: "shipped", Marked: true},
			},
		}
		out := resolveMarkReport("myproj", res, true)
		if !strings.Contains(out, "hold a newline") {
			t.Errorf("an id no surface can carry is not reported as such:\n%s", out)
		}
		if !strings.Contains(out, "stay") || !strings.Contains(out, "resolved") {
			t.Errorf("the report does not say the memory stays resolved:\n%s", out)
		}
		if strings.Contains(out, "--reassess --only ") {
			t.Errorf("the report emitted an --only command for a newline-bearing id:\n%s", out)
		}
	})
}

// TestMarkStampedIDsIsOnlyWhatThisRunWrote: the follow-up's list. A row that was
// already resolved before the run is not something the command would change, and
// naming it would send the operator to clear a stamp an earlier pass owns —
// which the repair would then report as a no-op, having cost a classifier call
// to learn nothing.
func TestMarkStampedIDsIsOnlyWhatThisRunWrote(t *testing.T) {
	got := markStampedIDs([]resolve.MarkedMemory{
		{ID: "aaa", Marked: true},
		{ID: "bbb", AlreadyResolved: true},
		{ID: "ccc", Pinned: true},
		{ID: "ddd", MarkFailed: true},
		{ID: "eee", Marked: true},
	})
	if len(got) != 2 || got[0] != "aaa" || got[1] != "eee" {
		t.Errorf("markStampedIDs = %v, want [aaa eee]", got)
	}
	if got := markStampedIDs(nil); len(got) != 0 {
		t.Errorf("markStampedIDs(nil) = %v, want nothing", got)
	}
}
