package mcpserver

import (
	"fmt"
	"strings"

	"github.com/wcatz/ghost/internal/assemble"
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
// with a notice saying how many were cut and how to see the rest. Like search
// under KeepPinned, a row marked pinned (ghost_project_context marks them) is cut
// only once every unpinned row is gone, and the notice then says how many of the
// cut rows were pinned. The budget is
// the same number search ships (searchResponseMaxBytes, one memory at the
// store's own content cap), read through the same accessor, so a caller has one
// budget to reason about across all six tools rather than six.
//
// ghost_list_projects and ghost_health (#1541) use it too, for their project
// rows: neither takes a `limit`. The ghost_list_projects notice says that no
// argument pages the list; the ghost_health notice says only its per-project
// rows were cut and points to ghost_list_projects. In ghost_health only the
// per-project rows are cuttable; every store-wide line is framing and is
// always shown.
//
// An answer that HAS rows in it always keeps at least one: the first row — the
// first pinned row when one is pinned — is never cut, so a single row larger than the budget (a decision whose 8000-byte
// decision and rationale are each clamped separately is about 16 KB) is shown
// whole with a notice rather than dropped for a notice over nothing.

// responsePart is one segment of a tool response, in the order it is rendered.
// A cuttable part is a WHOLE row: the budget may drop it to make the answer fit,
// and it drops the last unpinned cuttable part first. A non-cuttable part is framing —
// headings, count lines, learned context, a not-registered sentence — which is
// counted against the budget but never cut, because cutting it would change what
// the answer means rather than how much of it there is.
//
// A PINNED row is cut only once no unpinned row is left to cut. A pin is a slot
// guarantee ahead of ranking, and ghost_project_context renders its `## Global`
// section after the project's rows, so a cut strictly from the end removed the
// one row a pin promised to keep. The assembler's response fit does the same
// under Budget.KeepPinned.
//
// A part with section > 0 is a section HEADING, and section is the number of
// parts after it that belong to that section — its rows and its trailing notes.
// A heading whose section the cut left with nothing in it is not rendered, so a
// cut never prints a heading over nothing.
type responsePart struct {
	text     string
	cuttable bool
	pinned   bool
	section  int
}

// capResponseParts renders parts within budget, cutting WHOLE rows until the
// answer fits, and appends a notice naming how many rows went and how many of
// them were pinned. It returns the rendered answer and the number of rows cut.
//
// THE CUT ORDER. Unpinned rows go first, from the last one backward; a pinned row
// goes only once every unpinned row is gone, again from the last backward. With
// no pinned row this is exactly the end-first cut every listing tool has always
// had.
//
// The answer is BYTE-IDENTICAL to the uncut render when it already fits, and
// when the budget is not positive (an unbounded budget, the assembler's own
// reading of a zero cap).
//
// TWO ROWS THE PASS REFUSES TO LOSE, and both are about an answer that can still
// be read rather than one that merely fits:
//
//   - A FRAMING that is already at or over the budget keeps every row. Cutting
//     rows then reaches no cap and would only empty the answer, the same rule
//     the assembler's fit pass states as its framing ceiling.
//   - The LAST ROW OF THE CUT ORDER is never cut, whatever the budget: the first
//     pinned row, or the first row when none is pinned. A single row larger than
//     the budget — a decision whose two 8000-byte fields are clamped separately
//     is one — would otherwise be dropped and the answer would be framing and a
//     notice with nothing to read. The row is shown WHOLE and the notice says
//     the budget could not be met, because there is no smaller form of it a
//     listing argument can reach.
func capResponseParts(parts []responsePart, budget int, advice string) (string, int) {
	total := 0
	framing := 0
	var unpinned, pinned []int
	for i, p := range parts {
		total += len(p.text)
		switch {
		case p.cuttable && p.pinned:
			pinned = append(pinned, i)
		case p.cuttable:
			unpinned = append(unpinned, i)
		default:
			framing += len(p.text)
		}
	}
	if budget <= 0 || total <= budget {
		return joinResponseParts(parts), 0
	}
	if framing >= budget {
		return joinResponseParts(parts), 0
	}
	// The cut order: unpinned rows from the last backward, then pinned rows from
	// the last backward. Its final entry is the protected row, so the most that
	// can go is every row before it.
	order := make([]int, 0, len(parts))
	for i := len(unpinned) - 1; i >= 0; i-- {
		order = append(order, unpinned[i])
	}
	for i := len(pinned) - 1; i >= 0; i-- {
		order = append(order, pinned[i])
	}
	if len(order) == 0 {
		return joinResponseParts(parts), 0
	}
	cutSet := make([]bool, len(parts))
	pinnedCut := 0
	for cut := 1; cut < len(order); cut++ {
		at := order[cut-1]
		cutSet[at] = true
		if parts[at].pinned {
			pinnedCut++
		}
		notice := responseCapNoticeFor(cut, budget, advice, len(pinned) > 0) + responseCapPinnedNotice(pinnedCut, budget)
		kept := trimmedParts(parts, cutSet)
		if textsLen(kept)+len(notice) <= budget {
			return strings.Join(kept, "") + notice, cut
		}
	}
	// Even framing, the protected row and its notice do not fit. Keep that row
	// whole, cut every other row, and say why the budget was missed.
	cut := len(order) - 1
	rowAlone := len(parts[order[cut]].text) > budget
	notice := responseCapNoticeFirstKept(cut, budget, len(pinned) > 0, rowAlone) + responseCapPinnedNotice(pinnedCut, budget)
	return strings.Join(trimmedParts(parts, cutSet), "") + notice, cut
}

// trimmedParts is the text of every part a cut keeps, in order. A cut row is
// left out, and so is a section heading whose rows and notes are all gone. A
// heading that would open the answer drops the blank-line separator it carries
// for following an earlier section, because that section is gone.
func trimmedParts(parts []responsePart, cutSet []bool) []string {
	out := make([]string, 0, len(parts))
	written := 0
	for i, p := range parts {
		if cutSet[i] {
			continue
		}
		text := p.text
		if p.section > 0 {
			end := min(i+1+p.section, len(parts))
			keep := false
			for j := i + 1; j < end; j++ {
				if !cutSet[j] {
					keep = true
					break
				}
			}
			if !keep {
				continue
			}
			if written == 0 {
				text = strings.TrimPrefix(text, "\n\n")
			}
		}
		out = append(out, text)
		written += len(text)
	}
	return out
}

// textsLen is the summed length of rendered texts.
func textsLen(texts []string) int {
	n := 0
	for _, t := range texts {
		n += len(t)
	}
	return n
}

// capRowsToResponseBudget is capResponseParts for the common shape: a framing
// prefix that precedes the rows and is never cut, then rows, every one of them a
// whole cuttable row.
func capRowsToResponseBudget(framing string, rows []string, budget int, advice string) (string, int) {
	// The capacity is len(rows) and not len(rows) plus the framing part: the
	// addition is a size computation an analyzer reads as an overflow risk, and
	// the one extra append it costs when it reallocates is not worth silencing
	// that with arithmetic.
	parts := make([]responsePart, 0, len(rows))
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
	// The sentence is shared with the assembler's response-fit post-pass, which
	// trims a ghost_memory_search answer to the same budget: one builder is how
	// the search answer and these listings cannot spell the same cut two ways.
	return "\n" + assemble.CutNotice(cut, budget, advice) + "\n"
}

// responseCapNoticeFor is the cut notice for an answer that holds a pinned row,
// or the plain end-first notice when it holds none. A pinned row is the protected
// one, so it is always kept, and it may render AFTER the rows that were cut — the
// `## Global` section comes last — so "from the end" would be false. The
// pin-aware sentence says unpinned rows went first instead, and keeps the rest of
// the shared sentence. A pin-free answer is byte-identical to the shared notice.
func responseCapNoticeFor(cut, budget int, advice string, pinned bool) string {
	if !pinned {
		return responseCapNotice(cut, budget, advice)
	}
	verb := "rows were"
	if cut == 1 {
		verb = "row was"
	}
	return fmt.Sprintf("\n(%d %s cut from this answer, unpinned rows first, to stay within the %d-byte response "+
		"budget, which bounds the answer and not what is stored. %s)\n", cut, verb, budget, advice)
}

// responseCapNoticeFirstKept is the notice for the one over-budget answer the
// pass still returns: the protected row — the first row, or the first pinned row
// when a row is pinned — is kept whole rather than dropped, so the caller has
// something to read. rowAlone says whether that row by itself is larger than the
// budget; when it is not, the framing around it is the excess and the sentence
// says so. The sentence is the assembler's, shared with ghost_memory_search.
func responseCapNoticeFirstKept(cut, budget int, pinned, rowAlone bool) string {
	which := "first row"
	where := "from the end of this answer,"
	if pinned {
		which = "first pinned row"
		where = "from this answer, unpinned rows first,"
	}
	return "\n" + assemble.FirstKeptNotice(assemble.FirstKept{
		Cut: cut, Budget: budget, Which: which, Where: where, Tool: "listing", RowAlone: rowAlone,
	}) + "\n"
}

// responseCapPinnedNotice is the pinned-cut statement a trimmed answer carries
// after its cut notice, and "" when no pinned row was cut. A pinned row is cut
// only once every unpinned row is gone, so when one is, the pinned rows alone do
// not fit and the ones shown are the first of them.
func responseCapPinnedNotice(pinnedCut, budget int) string {
	if pinnedCut <= 0 {
		return ""
	}
	verb := "were"
	if pinnedCut == 1 {
		verb = "was"
	}
	return fmt.Sprintf("(%d of the cut rows %s pinned: the pinned rows alone exceed the %d-byte response budget, "+
		"and the ones shown are the first of them. Call ghost_memories_list to see the rest.)\n",
		pinnedCut, verb, budget)
}

// responseSection builds one `## `-headed section as response parts: the heading
// (with the blank-line separation every section after the first carries) as
// framing, then every row as a cuttable part, then the section's trailing notes
// as framing. It returns no parts for a section whose body would be empty, which
// is the rule `projectContextSection` applies to a written block: a note never
// gives a section its heading.
//
// pinned marks which rows are pinned, by index, and may be nil or shorter than
// rows for a section with no pinned row. The heading records how many parts
// follow it in the section, so a byte cut that empties the section drops the
// heading too.
//
// priorLen is the rendered length of the block before this section, which is how
// the separator knows whether this is the first section — the same `sb.Len() > 0`
// projectContextSection reads.
func responseSection(priorLen int, heading string, rows []string, pinned []bool, notes string) []responsePart {
	if len(rows) == 0 && notes == "" {
		return nil
	}
	var head strings.Builder
	if priorLen > 0 {
		head.WriteString("\n\n")
	}
	head.WriteString(heading)
	head.WriteString("\n\n")
	// len(rows) rather than len(rows) plus the heading and notes parts, for the
	// same reason as capRowsToResponseBudget: the addition is a size computation
	// an analyzer reads as an overflow risk.
	parts := make([]responsePart, 0, len(rows))
	parts = append(parts, responsePart{text: head.String()})
	for i, r := range rows {
		parts = append(parts, responsePart{text: r, cuttable: true, pinned: i < len(pinned) && pinned[i]})
	}
	if notes != "" {
		parts = append(parts, responsePart{text: notes})
	}
	parts[0].section = len(parts) - 1
	return parts
}

// itemPins is which of items are pinned, index for index with itemLines.
func itemPins(items []assemble.Item) []bool {
	pins := make([]bool, len(items))
	for i, it := range items {
		pins[i] = it.Pinned
	}
	return pins
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
	memoriesListCutAdvice      = "Ask for fewer results (a lower `limit`) or narrow with `category` or `retention` to see the rest."
	searchAllCutAdvice         = "Ask for fewer results (a lower `limit`) or narrow the query to see the rest."
	taskListCutAdvice          = "Ask for fewer results (a lower `limit`) or narrow with `status` to see the rest."
	decisionsListCutAdvice     = "Ask for fewer results (a lower `limit`) or narrow with `status` to see the rest."
	taskResourceCutAdvice      = "Use ghost_task_list to browse the rest."
	decisionsResourceCutAdvice = "Use ghost_decisions_list to browse the rest."
	listProjectsCutAdvice      = "No argument pages this list: a project left out is still reachable by passing its id or path as `project_id` to the other tools."
	healthCutAdvice            = "Only the per-project lines are cut, last first; the Projects count above is the whole store, and every other line is shown in full. `ghost_list_projects` lists the same projects within the same budget."
	projectContextCutAdvice    = "Ask for fewer results (a lower `limit`) or use ghost_memories_list to browse the rest."
)
