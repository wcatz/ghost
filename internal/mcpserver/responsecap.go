package mcpserver

import (
	"fmt"
	"strings"

	"github.com/wcatz/ghost/internal/assemble"
)

type responsePart struct {
	text     string
	cuttable bool
	pinned   bool
	section  int
}

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
	cut := len(order) - 1
	rowAlone := len(parts[order[cut]].text) > budget
	notice := responseCapNoticeFirstKept(cut, budget, len(pinned) > 0, rowAlone) + responseCapPinnedNotice(pinnedCut, budget)
	return strings.Join(trimmedParts(parts, cutSet), "") + notice, cut
}

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

func textsLen(texts []string) int {
	n := 0
	for _, t := range texts {
		n += len(t)
	}
	return n
}

func capRowsToResponseBudget(framing string, rows []string, budget int, advice string) (string, int) {
	parts := make([]responsePart, 0, len(rows))
	if framing != "" {
		parts = append(parts, responsePart{text: framing})
	}
	for _, r := range rows {
		parts = append(parts, responsePart{text: r, cuttable: true})
	}
	return capResponseParts(parts, budget, advice)
}

func responseCapNotice(cut, budget int, advice string) string {
	return "\n" + assemble.CutNotice(cut, budget, advice) + "\n"
}

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

func itemPins(items []assemble.Item) []bool {
	pins := make([]bool, len(items))
	for i, it := range items {
		pins[i] = it.Pinned
	}
	return pins
}

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

func responsePartsLen(parts []responsePart) int {
	n := 0
	for _, p := range parts {
		n += len(p.text)
	}
	return n
}

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
