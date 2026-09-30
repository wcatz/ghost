package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/reflection"
)

// tokenList renders a list of stored ids for a report line, each through the same
// renderer every other id on the command goes through. The ids are joined rather
// than printed one per line because the line is a count's worth of them and the
// reader is meant to be able to add the section up — but each one is still a
// stored value, and a comma-joined list of raw ids is exactly where a newline in
// one of them would start a line.
func tokenList(ids []string) string {
	rendered := make([]string, 0, len(ids))
	for _, id := range ids {
		rendered = append(rendered, assemble.Token(id))
	}
	return strings.Join(rendered, ", ")
}

// reflectRun is what the section needs to know about one round: the round itself,
// and the two flags a reader asked for. A struct rather than positional
// parameters because two of them are booleans with opposite effects on the same
// line, and swapping them at a call site is a silent lie about rows.
//
// It deliberately does NOT carry `--promote-globals`. A cross-project candidate
// is written to _global instead of the project, so the project replace sees one
// emission fewer — but an input no operation named is always carried by its own
// pass-through, which is project-scoped (a verbatim emission states no scope), so
// its row is accounted for the same way under both settings, and a named input is
// owned by its own line. The flag cannot change this section, and
// TestReflectSummaryIsTheSameUnderPromoteGlobals says so out loud.
type reflectRun struct {
	input   reflection.ReflectionInput
	result  reflection.ReflectionResult
	guarded []reflection.DroppedGuarded
	// allowDrops says whether a row the drop guard flagged is re-added or deleted,
	// so a drop line can state which.
	allowDrops bool
	// full is the display flag: the memory text whole rather than truncated.
	full bool
}

// reportInputAccounting prints the section that accounts for every input id of a
// consolidation, and it is the reason a dry run is worth reading (#684).
//
// The result list above it says what the corpus became. This says what became of
// the rows the run was GIVEN, which is the question a reviewer or an independent
// judge is actually asking: an input a merge consumed used to disappear from the
// report entirely, so a merge and a loss printed the same way. Every input id
// appears here in exactly one line or one count, so the section is a partition of
// the input set and the operator can check it by adding the numbers up.
//
// EVERY header and count line is a count of IDS, never of records. A merge may
// name any number of sources, so `Merges (3)` over one three-source merge is
// right where the count of merge operations is not, and a section whose numbers
// do not add up to the input total is the one thing this report cannot be. The
// line under the header is what reconciles the two: it names the ids the header
// counted.
//
// The two count lines are about ROWS, not about text, and that is a distinction
// the section had to be taught. `ReplaceNonManual` reuses stored rows by
// byte-identical content and claims ONE per emission, so two inputs holding the
// same bytes both have their text in the result and only one of them is still
// there afterwards. Keying on the text said "passed through" about the row the
// apply deleted, which is this report's own defect pointed the other way; the
// Deleted bucket below is decided by the reuse pass instead, and a duplicate says
// so in as many words.
//
// The same section is printed for a dry run and for an apply, and it is printed
// BEFORE the write, so the two are the same report and a dry run previews it
// exactly rather than approximately. That is also why a merge line names no
// successor id: the row a merge produces does not exist until the apply has
// written it, and the successor a reader wants is in `memory_history`'s
// related_id. `new` is the honest word, and `ghost history <id>` is where the
// identity becomes visible.
//
// The section is a function taking a writer because runReflect exits the
// process, and the accounting is a pure function in internal/reflection because
// the truth about the round belongs there: this formats it, and every fact it
// prints is derived where it is printed.
//
// It is called only where the result is non-empty, which is not incidental: the
// empty-set guard returns before any write, and a section printed there would
// report every input as deleted and say the replace removes it — describing a
// deletion that no run performs.
func reportInputAccounting(w io.Writer, run reflectRun) {
	limit := reflectTextLimit(run.full, reflectProposalLimit)
	acc := reflection.AccountInputs(run.input, run.result, run.guarded)

	// Discarded on purpose, as for every other report on this path: a write that
	// fails cannot be reported through the same failed write, and the report
	// lands on the stdout of a dry run a person is reading.
	_, _ = fmt.Fprintf(w, "Inputs (%d) accounted for; every count below is ids, so they add up to it:\n", acc.Inputs)

	// Merges and refusals are the two buckets whose records are not one id each,
	// so their headers count the ids the lines name rather than the lines. A
	// header that counted operations would break the only check the section asks
	// the reader to make.
	merged, refused := 0, 0
	for _, m := range acc.Merges {
		merged += len(m.IDs)
	}
	for _, r := range acc.Refusals {
		refused += len(r.IDs)
	}

	// Every section prints its count whether or not it has lines, so an operator
	// reading a preview can tell "no merges happened" from "the section is
	// missing" — and can add the counts up to the input total, which is the one
	// check that says the preview accounts for the whole corpus.
	_, _ = fmt.Fprintf(w, "Merges (%d):\n", merged)
	for _, m := range acc.Merges {
		line := fmt.Sprintf("  new <- %s   (%d B from %d B)", tokenList(m.IDs), len(m.Text), m.SourceBytes)
		if !m.In {
			line += "; the merged text is not in this result"
		}
		_, _ = fmt.Fprintln(w, line+guardClause(m.Guarded, run.allowDrops, "source"))
	}

	_, _ = fmt.Fprintf(w, "Refused by the grounding check (%d):\n", refused)
	for _, r := range acc.Refusals {
		// The identifiers are printed whole: a path or a version cut in half
		// names nothing, so the clause that explains the refusal would be the
		// one part of the report that cannot be read.
		_, _ = fmt.Fprintf(w, "  %s <- the %s introduced identifiers no source carries: %s; the sources are kept unchanged\n",
			tokenList(r.IDs), r.Kind, displayClaim(strings.Join(r.Identifiers, ", "), 0))
	}

	_, _ = fmt.Fprintf(w, "Rewrites (%d):\n", len(acc.Rewrites))
	for _, rw := range acc.Rewrites {
		line := fmt.Sprintf("  %s -> %s", assemble.Token(rw.ID), displayClaim(rw.Text, limit))
		if !rw.In {
			line += "; the replacement is not in this result"
		}
		_, _ = fmt.Fprintln(w, line+guardClause(boolCount(rw.Guarded), run.allowDrops, "row"))
	}

	_, _ = fmt.Fprintf(w, "Dropped (%d, each audited by the drop guard):\n", len(acc.Drops))
	for _, d := range acc.Drops {
		outcome := "a surviving output carries it"
		if d.Guarded {
			outcome = "nothing in the result carries it"
		}
		// The reason and its successor are rendered from the structured fields, so
		// the id on the line is the one resolved from the input rather than the
		// one the response spelled — `memIDKey` accepts any case, and the parser
		// normalises only the successor target.
		reason := d.Reason
		if d.Successor != "" {
			reason += " " + assemble.Token(d.Successor)
		}
		_, _ = fmt.Fprintf(w, "  %s reason: %s — %s%s\n", assemble.Token(d.ID), reason, outcome, guardClause(boolCount(d.Guarded), run.allowDrops, "row"))
	}

	// The rows this round removes, and the only bucket that is not an operation
	// and not a count of rows still there. Each line says WHY, because the two
	// reasons are different outcomes: a row nothing carries is a loss, and a row
	// whose identical twin was reused is a deduplication whose knowledge is still
	// in the project. Collapsing them is the confusion #684 is about, in the
	// direction the reader would not expect.
	deleted := fmt.Sprintf("Deleted (%d)", len(acc.Deleted))
	if len(acc.Deleted) > 0 {
		deleted += " — rows this round removes from the project:"
	} else {
		deleted += ":"
	}
	_, _ = fmt.Fprintln(w, deleted)
	for _, d := range acc.Deleted {
		_, _ = fmt.Fprintf(w, "  %s  %s\n", assemble.Token(d.ID), d.Reason)
	}

	_, _ = fmt.Fprintf(w, "Kept verbatim: %d    Passed through (not named): %d\n", len(acc.Kept), len(acc.Passed))
}

// guardClause is what the drop guard's verdict means for the ids one line names.
// Empty when the guard flagged none of them, because then a surviving output
// already accounts for the line and there is nothing to add — the one thing it
// must never read as is a second opinion about a verdict the guard reached on
// its own evidence.
//
// `guarded` is a count of the line's ids the guard flagged, and the clause says
// how many rather than naming them: the line already names every one of them,
// and a report that quoted an id twice would break the one-account contract the
// section exists to keep. `subject` is the word for what was flagged, so a merge
// reads as sources and a rewrite or a drop as a row, and the verb agrees with
// the count because this is the wording an operator reads on every accepted
// deletion.
func guardClause(guarded int, allowDrops bool, subject string) string {
	if guarded <= 0 {
		return ""
	}
	// "1 row has" and "2 sources have": the verb follows the count, because this
	// clause is the wording an operator reads on every accepted deletion.
	plural, verb := "s", "have"
	if guarded == 1 {
		plural, verb = "", "has"
	}
	if allowDrops {
		return fmt.Sprintf("; %d %s%s %s no surviving output, and --allow-drops accepts the deletion", guarded, subject, plural, verb)
	}
	return fmt.Sprintf("; the drop guard re-added %d %s%s verbatim", guarded, subject, plural)
}

// boolCount lifts a per-row guard verdict to the count guardClause takes, so a
// single row and a merge's several sources go through one clause.
func boolCount(guarded bool) int {
	if guarded {
		return 1
	}
	return 0
}
