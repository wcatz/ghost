package mcpserver

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/provider"
)

const (
	recallProjectPrefaceMaxBytes = 512

	projectContextMemoriesCap = 20
	projectContextGlobalsCap  = 15
)

const (
	memorySectionHeading = "## Memories"
	globalSectionHeading = "## Global (applies to all projects)"
)

func projectContextBudget(projectID string, limit int) assemble.Budget {
	return assemble.Budget{Slices: []assemble.Slice{{
		Bucket:                projectID,
		IncludeGlobal:         projectID != memory.GlobalProjectID,
		MaxItems:              limit,
		OverFetch:             limit * 2,
		Order:                 memory.OrderDecay,
		DemoteOnlyWhenOverCap: true,
	}}}
}

func projectContextGlobalBudget(limit int) assemble.Budget {
	return assemble.Budget{Slices: []assemble.Slice{{
		Bucket:                memory.GlobalProjectID,
		MaxItems:              limit,
		OverFetch:             limit * 2,
		Order:                 memory.OrderDecay,
		DemoteOnlyWhenOverCap: true,
	}}}
}

func assembleProjectContext(ctx context.Context, s *Server, req assemble.Request) (assemble.Result, error) {
	candidates, ok := s.store.(assembleCapableStore)
	if !ok {
		return assemble.Result{}, fmt.Errorf("this store does not support candidate retrieval, so the project context " +
			"cannot be assembled")
	}
	req.Source = assemble.SourceProjectCtx
	req.Condition = assemble.CondHybrid
	if req.Now.IsZero() {
		req.Now = s.now()
	}

	req.Record = s.recordSink()
	req.Logger = s.logger
	if req.SessionID == "" {
		req.SessionID = s.hostSessionID
	}
	return assemble.Run(ctx, candidates, req)
}

func recallProjectPrefaceName(rawID string) string {
	if len(rawID) <= recallProjectPrefaceMaxBytes {
		return memory.ProjectArg("project_id", rawID)
	}
	return memory.RefusalArg("project_id", rawID)
}

func itemLines(items []assemble.Item) []string {
	lines := make([]string, 0, len(items))
	for _, it := range items {
		lines = append(lines, it.Line()+"\n")
	}
	return lines
}

func projectContextPinnedLine(n int, whose string, shown int) string {
	if n <= 0 {
		return ""
	}
	if shown == 0 {
		return fmt.Sprintf("(%d pinned %smemories cut: the pinned rows alone exceed the cap, and none of them is shown in this block. Call ghost_memories_list to see the rest.)\n", n, whose)
	}
	return fmt.Sprintf("(%d pinned %smemories cut: the pinned rows alone exceed the cap, and the ones shown are the "+
		"best-ranked of them. Call ghost_memories_list to see the rest.)\n", n, whose)
}

func projectContextPinnedLines(res assemble.Result, projectID string, own, globals []assemble.Item) (ownLine, foldedOwn, globalLine string) {
	if res.Trace == nil {
		return "", "", ""
	}
	ownCut := res.Trace.PinnedCut[projectID]
	if projectID == memory.GlobalProjectID {
		ownCut = 0
	}
	ownLine = projectContextPinnedLine(ownCut, "", len(own))
	globalLine = projectContextPinnedLine(res.Trace.PinnedCut[memory.GlobalProjectID], "global ", len(globals))
	if len(own) == 0 {
		foldedOwn, ownLine = "", ""
		if ownCut > 0 {
			foldedOwn = fmt.Sprintf("(%d pinned memories cut: none of them is in the block above. Call ghost_memories_list to see them.)\n", ownCut)
		}
	}
	return ownLine, foldedOwn, globalLine
}

func projectContextSplit(items []assemble.Item) (own, globals []assemble.Item) {
	for _, it := range items {
		if it.ProjectID == memory.GlobalProjectID {
			globals = append(globals, it)
		} else {
			own = append(own, it)
		}
	}
	return own, globals
}

func splitMemoriesByProject(rows []memory.AsOfRow) (own, globals []memory.Memory) {
	for _, r := range rows {
		if r.ProjectID == memory.GlobalProjectID {
			globals = append(globals, r.Memory)
		} else {
			own = append(own, r.Memory)
		}
	}
	return own, globals
}

func projectContextSection(sb *strings.Builder, heading, body string) {
	if body == "" {
		return
	}
	if sb.Len() > 0 {
		sb.WriteString("\n\n")
	}
	sb.WriteString(heading)
	sb.WriteString("\n\n")
	sb.WriteString(body)
}

func projectNotRegistered(rendered string) string {
	return fmt.Sprintf("Project %s is not registered with Ghost yet — nothing has ever been saved for it. "+
		"Call ghost_memory_save to create it.", rendered)
}

func projectNotRegisteredAsOf(asked string, asOf time.Time) string {
	return fmt.Sprintf("Project %s is not registered with Ghost yet, so there is nothing to read as of %s: "+
		"Ghost has never held a row for it, at any instant. Call ghost_memory_save to create it.",
		memory.RefusalArg("project_id", asked), asOf.Format(time.RFC3339))
}

func (s *Server) projectContextGlobalRows(ctx context.Context, limit int, alreadyShown, carried []assemble.Item, carriedLine string) (rows []assemble.Item, line string) {
	rows = carried
	line = carriedLine
	if globals, err := s.projectContextGlobals(ctx, limit); err == nil {
		seen := make(map[string]bool, len(alreadyShown))
		for _, it := range alreadyShown {
			seen[it.ID] = true
		}
		inSecond := make(map[string]bool, len(globals.Items))
		for _, g := range globals.Items {
			inSecond[g.ID] = true
		}
		rows = make([]assemble.Item, 0, len(carried)+len(globals.Items))
		rows = append(rows, carried...)
		shownElsewhere := 0
		for _, c := range carried {
			if c.Pinned && !inSecond[c.ID] {
				shownElsewhere++
			}
		}
		for _, g := range globals.Items {
			if !seen[g.ID] {
				rows = append(rows, g)
			}
		}
		cut := 0
		if globals.Trace != nil {
			cut = globals.Trace.PinnedCut[memory.GlobalProjectID] - shownElsewhere
		}
		line = projectContextPinnedLine(cut, "global ", len(rows))
	}
	return rows, line
}

func projectContextEmptyNote(res assemble.Result) string {
	return assemble.EmptyNote(res)
}

func (s *Server) projectWindowRowCount(ctx context.Context, projectID string) (int, error) {
	counter, ok := s.store.(windowCountCapableStore)
	if !ok {
		return 0, nil
	}
	return counter.CountActiveMemories(ctx, projectID)
}

func (s *Server) projectContextOwnRowsNote(ctx context.Context, projectID string, res assemble.Result) string {
	if projectID == "" || projectID == memory.GlobalProjectID {
		return ""
	}
	n, err := s.store.CountMemories(ctx, projectID)
	if err != nil {
		return ""
	}
	if len(res.Items) == 0 {
		if n == 0 {
			return "" // the project holds nothing at all, so there is nothing to report
		}
		if inWindow, err := s.projectWindowRowCount(ctx, projectID); err == nil && inWindow > 0 {
			if note := projectContextEmptyNote(res); note != "" {
				return note
			}
		}
	}
	for _, it := range res.Items {
		if it.ProjectID == projectID {
			return "" // a row of the project's own is in the block, so there is no gap to report
		}
	}
	if n == 0 {
		return "Ghost holds no memories for this project; the memory rows above are the cross-project ones."
	}
	if n == 1 {
		return "Ghost holds 1 memory for this project and none of it is in the block above. Call " +
			"ghost_memories_list to browse it: a browse is not capped at what fits in a context block, and it " +
			"shows each row's validity window."
	}
	return fmt.Sprintf("Ghost holds %d memories for this project and none of them is in the block above. Call "+
		"ghost_memories_list to browse them: a browse is not capped at what fits in a context block, and it "+
		"shows each row's validity window.", n)
}

func (s *Server) projectContextBlock(ctx context.Context, projectID, asked string, limit int, refused, failed, notRegular []string) (string, error) {
	var parts []responsePart
	var err error
	var memories assemble.Result
	var own, globals []assemble.Item
	if projectID != "" {
		memories, err = s.projectContextMemories(ctx, projectID, limit)
		if err != nil {
			return "", err
		}
		own, globals = projectContextSplit(memories.Items)
		ownLine, foldedOwn, globalLine := projectContextPinnedLines(memories, projectID, own, globals)
		if len(globals) == 0 && globalLine != "" {
			ownLine += fmt.Sprintf("(%d pinned global memories cut: none of them is in the block above. Call ghost_memories_list to see them.)\n",
				memories.Trace.PinnedCut[memory.GlobalProjectID])
			globalLine = ""
		}
		parts = append(parts, responseSection(responsePartsLen(parts), memorySectionHeading, itemLines(own), itemPins(own), ownLine)...)
		parts = append(parts, responseSection(responsePartsLen(parts), globalSectionHeading, itemLines(globals), itemPins(globals), foldedOwn+globalLine)...)
	}

	learned, err := s.store.GetLearnedContext(ctx, projectID)
	if err != nil {
		return "", fmt.Errorf("get learned context: %w", err)
	}
	if learned != "" {
		parts = append(parts, responsePart{text: "\n\n## Learned Context\n\n" +
			dataDelimiterNote + "\n\n" + quoteData(learned)})
	}

	text := joinResponseParts(parts)
	if text == "" {
		if note := s.projectContextOwnRowsNote(ctx, projectID, memories); note != "" {
			return note + s.skippedImportNote(refused, failed, notRegular), nil
		}
		if projectID == memory.GlobalProjectID {
			if note := projectContextEmptyNote(memories); note != "" {
				return note + s.skippedImportNote(refused, failed, notRegular), nil
			}
			return "No memories found among the cross-project rows." + s.skippedImportNote(refused, failed, notRegular), nil
		}
		exists, existsErr := s.projectExists(ctx, projectID)
		switch {
		case existsErr != nil:
			text = "Project lookup failed — unable to determine whether it is registered. Try again or call ghost_memory_save to create it."
		case exists:
			text = "Project is registered but has no memories or learned context yet — nothing has been saved for it."
		default:
			text = projectNotRegistered(recallProjectPrefaceName(asked))
		}
		return text + s.skippedImportNote(refused, failed, notRegular), nil
	}
	if note := s.projectContextOwnRowsNote(ctx, projectID, memories); note != "" {
		parts = append(parts, responsePart{text: "\n\n" + note})
	}
	if sn := s.skippedImportNote(refused, failed, notRegular); sn != "" {
		parts = append(parts, responsePart{text: sn})
	}
	text, _ = capResponseParts(parts, s.searchResponseCap(), projectContextCutAdvice)
	return text, nil
}

func (s *Server) now() time.Time {
	if s.clock != nil {
		return s.clock().UTC()
	}
	return time.Now().UTC()
}

// ProjectContextAt returns what ghost_project_context answers for projectID at
// the given limit, reading the store at the instant now.
func ProjectContextAt(ctx context.Context, store provider.MemoryStore, projectID string, limit int, now time.Time) (string, error) {
	s := newMeasuredServer(store, now)
	return s.projectContextBlock(ctx, projectID, projectID, limit, nil, nil, nil)
}

// ProjectResourceAt is ProjectContextAt for the `ghost://project/{id}/context`
// resource, which the recall_project prompt shares: the same memory read at the
// resource's fixed cap, and a second request for the Global section.
func ProjectResourceAt(ctx context.Context, store provider.MemoryStore, projectID string, now time.Time) (string, error) {
	s := newMeasuredServer(store, now)
	return s.buildProjectContext(ctx, projectID, "", s.searchResponseCap())
}

func newMeasuredServer(store provider.MemoryStore, now time.Time) *Server {
	return &Server{
		store:          store,
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		searchMaxBytes: searchResponseMaxBytes,
		clock:          func() time.Time { return now },
	}
}

func (s *Server) skippedImportNote(refused, failed, notRegular []string) string {
	if len(refused) == 0 && len(failed) == 0 && len(notRegular) == 0 {
		return ""
	}
	var out string
	if len(refused) > 0 {
		out += "\n\nClaude memory import skipped files (refused by the token guard): " + renderSkippedImportFiles(refused)
	}
	if len(failed) > 0 {
		out += "\n\nClaude memory import could not store (failed; a later ghost mcp init run retries): " + renderSkippedImportFiles(failed)
	}
	if len(notRegular) > 0 {
		out += "\n\nClaude memory import skipped files (not a regular file; never read): " + renderSkippedImportFiles(notRegular)
	}
	return out
}

func renderSkippedImportFiles(skipped []string) string {
	const maxNames = 10
	names := skipped
	more := 0
	if len(names) > maxNames {
		more = len(names) - maxNames
		names = names[:maxNames]
	}
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = quoteData(name)
	}
	out := strings.Join(quoted, ", ")
	if more > 0 {
		out += fmt.Sprintf(" (+%d more)", more)
	}
	return out
}
