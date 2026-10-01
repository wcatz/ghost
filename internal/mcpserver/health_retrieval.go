package mcpserver

// The retrieval-audit block in ghost_health (#646 part 3).
//
// `ghost context --audit` is the report an operator asks for by name. This is the
// same figures where an agent ALREADY is: the agent debugging "search returns the
// wrong things" calls ghost_health and never opens a terminal, so a block that
// lives only in a CLI command is a block that is read exactly when nobody is
// measuring anything.
//
// Three decisions, each with a cheaper option rejected:
//
//   - A block inside ghost_health rather than a twenty-third tool. This tool is
//     already "what is Ghost's state", every caller already fetches it, and the
//     alternative costs an agent a round trip to learn something it was already
//     being told. It also keeps the tool count in the docs at 22.
//   - Per-source lines, never one total. The same rule the report carries: a
//     search and an injection answer different questions, and a precision over
//     both is a number about neither. A health block is the worst possible place
//     for that number, because a health block's figures are the ones read without
//     reading anything else.
//   - The empty state is NAMED. Before #850's passive injections write their
//     records, session_start and project_context have no rows on any store, and a
//     block that lists only the sources with figures reads as "one source, all
//     healthy" rather than as "two sources have never been measured".
//
// The scope is the whole store, which is what this tool reports everywhere else —
// projects are pooled WITHIN a source and never sources with each other, and
// audit.MergeProjects is the only code here that combines reports. It reports
// rather than fails: a store that cannot answer is reported as unreadable, because
// a section's silence has to mean one thing.

import (
	"context"
	"fmt"
	"strings"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/audit"
	"github.com/wcatz/ghost/internal/memory"
)

// labelReasons renders a reason list for a warning line, one label per reason.
//
// The reasons come from the scanner's own fail-open vocabulary rather than from a
// transcript, but the column behind them is text and this block is a warning line
// that another line could be forged under. Each is labelled rather than joined raw
// so a reason carrying a newline costs a quoted token and not a second line of
// health output.
func labelReasons(reasons []string) string {
	out := make([]string, 0, len(reasons))
	for _, r := range reasons {
		out = append(out, assemble.Label(r))
	}
	return strings.Join(out, ", ")
}

// writeRetrievalAuditBlock renders the per-source retrieval figures.
//
// Additive by construction: it appends to what the caller has already written and
// renders its own failure, so nothing above it can be dropped by this block failing.
func (s *Server) writeRetrievalAuditBlock(ctx context.Context, sb *strings.Builder) {
	// The concrete store, type-asserted at call time. audit.BuildReport takes a
	// *memory.Store because the two readers it needs are on the concrete type and
	// not on provider.MemoryStore, so the assertion is what makes this block
	// possible at all — and a store that is not one has nothing to report here
	// rather than a reason to fail the whole tool. Reaching for the concrete type
	// is a boundary crossing rather than an escape hatch: nothing in this file
	// writes, and both readers below are read-only.
	store, ok := s.store.(*memory.Store)
	if !ok {
		return
	}
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		fmt.Fprintf(sb, "**Retrieval audit:** could not be read: %v\n", err)
		return
	}
	reports := make([]audit.Report, 0, len(projects))
	for _, p := range projects {
		if p.ID == "" {
			// Unreachable through the primary key, and skipped rather than passed on
			// anyway: BuildReport refuses an empty project id, and handing it one
			// would replace every project's figures with an error. A bucket carrying
			// no project id has no per-project retrieval rows, which is what that
			// refusal means.
			continue
		}
		rep, err := audit.BuildReport(ctx, store, audit.ReportOptions{ProjectID: p.ID})
		if err != nil {
			fmt.Fprintf(sb, "**Retrieval audit:** could not be read: %v\n", err)
			return
		}
		reports = append(reports, rep)
	}
	if len(reports) == 0 {
		// A header with no lines under it reads as a section that ran and had
		// nothing to say, which is a different claim from "there is no project to
		// report on".
		sb.WriteString("**Retrieval audit:** no project is registered, so no retrieval has been measured\n")
		return
	}
	merged := audit.MergeProjects(reports)

	// Whether any source has a verdict at all, which decides two things below: the
	// caveat is about a figure, and this store may have no figure.
	scored := false
	for _, src := range merged.Sources {
		if src.Scored > 0 {
			scored = true
			break
		}
	}

	sb.WriteString("**Retrieval audit** — per source, every call this store has recorded; a search and an injection are never pooled\n")
	for _, src := range merged.Sources {
		fmt.Fprintf(sb, "  %s\n", src.Summary())
		if src.DegradedVerdicts > 0 {
			// The glyph this tool already uses for a warning, and under the source
			// it is about: a degraded verdict stays in the denominator, so an
			// ignored verdict counted next to it is a claim about a transcript that
			// stopped early, and the percentage beside it does not say so.
			fmt.Fprintf(sb,
				"  ⚠ %s: %d of its %d scored verdict(s) are degraded — judged against a partly-read transcript (%s)\n",
				assemble.Label(src.Source), src.DegradedVerdicts, src.Scored, labelReasons(src.DegradedReasons))
		}
	}
	// The sentence the numbers most invite the reader to get wrong. "80% ignored"
	// read on its own is a judgement about the store's memory quality, and it is
	// the one field here that is not a measurement of quality at all: it is the
	// residual, after what the agent's own words restated.
	//
	// Printed only once there is a verdict to misread, and without the ⚠ this
	// tool reserves for trouble. A caveat attached to no figure is noise on a
	// fresh store — and a fresh store carrying a warning glyph is how an agent
	// learns to skip the warnings that matter.
	if scored {
		sb.WriteString("  \"ignored\" is the residual, not a relevance or usefulness score: " +
			"it means the agent's own words never mentioned the memory\n")
	}
}
