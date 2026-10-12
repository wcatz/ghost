package mcpserver

import (
	"fmt"
	"strings"
)

// This file is the response byte budget for the READ tools that are bounded by
// row count alone — ghost_memories_list, ghost_project_context, ghost_task_list,
// ghost_decisions_list and ghost_search_all (#1039). ghost_memory_search has had
// one for as long as the assembler's response-fit pass has existed; these five
// build their own text, so their rows were bounded only by the caller's `limit`,
// and 60 memories of 8 KB in one answer is over 100k tokens in an agent's
// context window.
//
// The cut is the same shape search uses: WHOLE rows, from the END of the answer,
// with a notice saying how many were cut and how to see the rest. The budget is
// the same number search ships (searchResponseMaxBytes, one memory at the
// store's own content cap), read through the same accessor, so a caller has one
// budget to reason about across all six tools rather than six.

// responsePart is one segment of a tool response, in the order it is rendered.
// A cuttable part is a WHOLE row: the budget may drop it to make the answer fit,
// and it drops the last cuttable part first. A non-cuttable part is framing —
// headings, count lines, learned context, a not-registered sentence — which is
// counted against the budget but never cut, because cutting it would change what
// the answer means rather than how much of it there is.
type responsePart struct {
	text     string
	cuttable bool
}

// capResponseParts renders parts within budget, cutting WHOLE rows from the last
// cuttable one backward until the answer fits, and appends a notice naming how
// many rows went. It returns the rendered answer and the number of rows cut.
//
// The answer is BYTE-IDENTICAL to the uncut render when it already fits, and
// when the budget is not positive (an unbounded budget, the assembler's own
// reading of a zero cap). A budget too small for even the all-cut framing plus
// its notice keeps every row instead of answering with nothing: cutting cannot
// reach the cap, and an empty answer is worse than a large one — the same rule
// the assembler's fit pass states as its framing ceiling.
func capResponseParts(parts []responsePart, budget int, advice string) (string, int) {
	total := 0
	var cutIndex []int
	for i, p := range parts {
		total += len(p.text)
		if p.cuttable {
			cutIndex = append(cutIndex, i)
		}
	}
	if budget <= 0 || total <= budget {
		return joinResponseParts(parts), 0
	}
	removed := 0
	for cut := 1; cut <= len(cutIndex); cut++ {
		removed += len(parts[cutIndex[len(cutIndex)-cut]].text)
		notice := responseCapNotice(cut, budget, advice)
		if total-removed+len(notice) <= budget {
			var b strings.Builder
			b.Grow(total - removed + len(notice))
			skip := len(cutIndex) - cut
			next := 0
			for i, p := range parts {
				if next < len(cutIndex) && cutIndex[next] == i {
					cutThis := next >= skip
					next++
					if cutThis {
						continue
					}
				}
				b.WriteString(p.text)
			}
			b.WriteString(notice)
			return b.String(), cut
		}
	}
	return joinResponseParts(parts), 0
}

// capRowsToResponseBudget is capResponseParts for the common shape: a framing
// prefix that precedes the rows and is never cut, then rows, every one of them a
// whole cuttable row.
func capRowsToResponseBudget(framing string, rows []string, budget int, advice string) (string, int) {
	parts := make([]responsePart, 0, len(rows)+1)
	if framing != "" {
		parts = append(parts, responsePart{text: framing})
	}
	for _, r := range rows {
		parts = append(parts, responsePart{text: r, cuttable: true})
	}
	return capResponseParts(parts, budget, advice)
}

// responseCapNotice is the sentence a trimmed answer ends with. It starts with a
// blank line so it reads as commentary on the rows above rather than as another
// row, and it names the budget it was fitted to so a caller can tell the row
// count from the byte limit. The advice is the caller's own, because the lever
// that shows the rest differs by tool — a lower `limit` on all of them, plus the
// filter or query that narrows this one's population.
func responseCapNotice(cut, budget int, advice string) string {
	verb := "rows were"
	if cut == 1 {
		verb = "row was"
	}
	return fmt.Sprintf("\n(%d %s cut from the end of this answer to stay within the %d-byte response "+
		"budget, which bounds the answer and not what is stored. %s)\n", cut, verb, budget, advice)
}

// responseSection builds one `## `-headed section as response parts: the heading
// (with the blank-line separation every section after the first carries) as
// framing, then every row as a cuttable part, then the section's trailing notes
// as framing. It returns no parts for a section whose body would be empty, which
// is the rule `projectContextSection` applies to a written block: a note never
// gives a section its heading.
//
// priorLen is the rendered length of the block before this section, which is how
// the separator knows whether this is the first section — the same `sb.Len() > 0`
// projectContextSection reads.
func responseSection(priorLen int, heading string, rows []string, notes string) []responsePart {
	if len(rows) == 0 && notes == "" {
		return nil
	}
	var head strings.Builder
	if priorLen > 0 {
		head.WriteString("\n\n")
	}
	head.WriteString(heading)
	head.WriteString("\n\n")
	parts := make([]responsePart, 0, len(rows)+2)
	parts = append(parts, responsePart{text: head.String()})
	for _, r := range rows {
		parts = append(parts, responsePart{text: r, cuttable: true})
	}
	if notes != "" {
		parts = append(parts, responsePart{text: notes})
	}
	return parts
}

// joinResponseParts is the uncut render: every part in order.
func joinResponseParts(parts []responsePart) string {
	var b strings.Builder
	n := 0
	for _, p := range parts {
		n += len(p.text)
	}
	b.Grow(n)
	for _, p := range parts {
		b.WriteString(p.text)
	}
	return b.String()
}

// responsePartsLen is the rendered length of parts, which a caller building
// sections needs for the next section's separator.
func responsePartsLen(parts []responsePart) int {
	n := 0
	for _, p := range parts {
		n += len(p.text)
	}
	return n
}

// The per-tool advice a trimmed answer ends with. Each names the one lever the
// caller holds: the row cap is the only argument that bounds the answer on any
// of these tools, and a filter or a query narrows the population so a smaller
// slice of it is the part worth keeping.
const (
	memoriesListCutAdvice   = "Ask for fewer results (a lower `limit`) or narrow with `category` or `retention` to see the rest."
	searchAllCutAdvice      = "Ask for fewer results (a lower `limit`) or narrow the query to see the rest."
	taskListCutAdvice       = "Ask for fewer results (a lower `limit`) or narrow with `status` to see the rest."
	decisionsListCutAdvice  = "Ask for fewer results (a lower `limit`) or narrow with `status` to see the rest."
	projectContextCutAdvice = "Ask for fewer results (a lower `limit`) or use ghost_memories_list to browse the rest."
)
