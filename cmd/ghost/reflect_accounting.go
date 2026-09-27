package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/wcatz/ghost/internal/reflection"
)

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
// report every input as absent and say the replace deletes it — describing a
// deletion that no run performs.
func reportInputAccounting(w io.Writer, input reflection.ReflectionInput, result reflection.ReflectionResult, guarded []reflection.DroppedGuarded, allowDrops, full bool) {
	limit := reflectTextLimit(full, reflectProposalLimit)
	acc := reflection.AccountInputs(input, result, guarded)

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
		line := fmt.Sprintf("  new <- %s   (%d B from %d B)", strings.Join(m.IDs, ", "), len(m.Text), m.SourceBytes)
		if !m.In {
			line += "; the merged text is not in this result"
		}
		_, _ = fmt.Fprintln(w, line+guardClause(m.Guarded, allowDrops, "source"))
	}

	_, _ = fmt.Fprintf(w, "Refused by the grounding check (%d):\n", refused)
	for _, r := range acc.Refusals {
		// The identifiers are printed whole: a path or a version cut in half
		// names nothing, so the clause that explains the refusal would be the
		// one part of the report that cannot be read.
		_, _ = fmt.Fprintf(w, "  %s <- the %s introduced identifiers no source carries: %s; the sources are kept unchanged\n",
			strings.Join(r.IDs, ", "), r.Kind, displayClaim(strings.Join(r.Identifiers, ", "), 0))
	}

	_, _ = fmt.Fprintf(w, "Rewrites (%d):\n", len(acc.Rewrites))
	for _, rw := range acc.Rewrites {
		line := fmt.Sprintf("  %s -> %s", rw.ID, displayClaim(rw.Text, limit))
		if !rw.In {
			line += "; the replacement is not in this result"
		}
		_, _ = fmt.Fprintln(w, line+guardClause(boolCount(rw.Guarded), allowDrops, "row"))
	}

	_, _ = fmt.Fprintf(w, "Dropped (%d, each audited by the drop guard):\n", len(acc.Drops))
	for _, d := range acc.Drops {
		outcome := "a surviving output carries it"
		if d.Guarded {
			outcome = "nothing in the result carries it"
		}
		// The reason and its successor are rendered from the structured fields, so
		// the id on the line is the resolved one: the response's own spelling of a
		// supersession target is upper-cased by the parser, and a stored id is
		// 32 lower-case hex digits.
		reason := d.Reason
		if d.Successor != "" {
			reason += " " + d.Successor
		}
		_, _ = fmt.Fprintf(w, "  %s reason: %s — %s%s\n", d.ID, reason, outcome, guardClause(boolCount(d.Guarded), allowDrops, "row"))
	}

	// The only bucket that is not an operation and not a count of untouched
	// rows: inputs nothing named and nothing carries. On the LLM path it is
	// empty or one row a post-filter removed the carrier for, and on the SQLite
	// tier — which names no ids at all — it is every duplicate the tier absorbed.
	// Those rows are the ones this round deletes, so the section says so rather
	// than leaving the reader to infer it from a merge that is not there.
	absent := fmt.Sprintf("Absent from the result (%d)", len(acc.Absent))
	if len(acc.Absent) > 0 {
		absent += " — nothing in this result carries them, so an apply deletes the stored row:"
	} else {
		absent += ":"
	}
	_, _ = fmt.Fprintln(w, absent)
	for _, id := range acc.Absent {
		_, _ = fmt.Fprintf(w, "  %s\n", id)
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
