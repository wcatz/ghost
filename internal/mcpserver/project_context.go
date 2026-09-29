package mcpserver

// The project-context surface's memory rows, as a caller of the context
// assembler. It is the last reader in the tree that chose its own rows outside
// `assemble.Run` (#581): `Store.GetTopMemories` ranked and trimmed them in SQL,
// and the assembler's stages then had nothing left to decide.
//
// This file is the ONE statement of what that surface selects, in the same sense
// `sessionPassiveBudget` is the session start's: the caps, the over-fetches, the
// order and the near-duplicate policy are policy about a surface, and two
// statements of them are how they drift.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/memory"
)

// The two fixed caps `buildProjectContext` has always used. They are named
// because the tool's cap is the CALLER's and the resource's is not, and a reader
// comparing the two paths needs to see that they are different numbers rather
// than infer it from a literal.
const (
	// projectContextMemoriesCap bounds the `## Memories` section of the
	// ghost://project/{id}/context resource and the recall_project prompt.
	projectContextMemoriesCap = 20
	// projectContextGlobalsCap bounds its `## Global (applies to all projects)`
	// section.
	projectContextGlobalsCap = 15
)

// projectContextBudget is the policy for ONE read, and the two callers use it
// differently on purpose:
//
//   - the TOOL sends one slice for the whole `## Memories` section, capped at the
//     caller's `limit`.
//   - the RESOURCE sends one for `## Memories` at the fixed cap above, and a
//     SECOND REQUEST for `## Global` — see projectContextGlobalBudget.
//
// The caps are unchanged from `GetTopMemories`: `limit`, over-fetching `limit*2`,
// the decay composite order, the supersede lookup always and the near-duplicate
// one only when the window is wider than the cap, and no two-pass reservation
// (GetTopMemories has none, so a policy that set one would be a different
// selection rather than a re-expression of this one).
//
// Budget.MaxItems and MaxBytes stay 0, for the reason `Result` states and the
// session-start plan repeated: MaxBytes bounds the complete rendered RESPONSE,
// `Run`'s Response is the SEARCH framing, and this surface frames its own block
// around Item.Line(). The per-slice caps are the whole bound, exactly as they
// have always been.
//
// DemotionThreshold is left at 0, which the store reads as "use my configured
// one" rather than as "every related edge is a near-duplicate". That is not a
// detail: `nearDuplicatePenaltyRows` binds it as `strength >= ?`, so a literal 0.0
// would make EVERY edge a near-duplicate. Leaving it unstated is also the
// parity-preserving choice — GetTopMemories used the store's configured value,
// and the fallback is the same number.
func projectContextBudget(projectID string, limit int) assemble.Budget {
	return assemble.Budget{Slices: []assemble.Slice{{
		Bucket: projectID,
		// The one policy field the passive seam needed for this surface, and the
		// reason it cannot be two buckets. GetTopMemories read
		// `project_id = ? OR project_id = '_global'` with ONE cap over the union;
		// a project slice at `limit` plus a `_global` slice at `limit` would admit
		// twice the rows the caller asked for, and the tool's argument says "max
		// memories to return".
		//
		// NOT set when the bucket IS `_global`, because that is not a union: the
		// bucket already reads exactly those rows, and the flag would only trip
		// both seams' overlap refusal — a policy that fetches `_global` while
		// admitting it — on a request that asks for nothing more. So
		// `project_id: "_global"` is a supported call here, which it was before
		// the move and is the same listing `ghost://memories/global` serves.
		IncludeGlobal: projectID != memory.GlobalProjectID,
		MaxItems:      limit,
		OverFetch:     limit * 2,
		Order:         memory.OrderDecay,
		// A demotion is a REORDER, and on a selected set that fits under the cap it
		// can only shuffle rows the answer shows in full. GetTopMemories skipped it
		// there too, on exactly this condition.
		DemoteOnlyWhenOverCap: true,
	}}}
}

// projectContextGlobalBudget is the SECOND read, for the resource's Global
// section: `_global` alone, over-fetching 2×.
//
// It is a separate REQUEST and not a second slice of the first, for two reasons
// that are both about correctness rather than tidiness. The first: a request that
// admits `_global` into the project bucket AND fetches it under its own name
// returns every global row twice, which both seams now refuse. The second is
// subtler and is why this could not be fixed by simply allowing the overlap: the
// section boundary runs AFTER the cap today, so a caller that filtered the
// already-shown rows out of the STORE's read would admit up to 15 NEW rows where
// the shipped code admitted 15 rows of which some were repeats. Doing it here —
// one request per section, the boundary applied by the caller over the admitted
// rows — is what keeps the second section's membership identical.
//
// The cap is a PARAMETER because the same read serves an UNRESOLVED project name,
// where the caller has asked for a different number: the tool asks for its own
// `limit`, and the resource and prompt ask for projectContextMemoriesCap. Both are
// parity with origin/main, which read `GetTopMemories(ctx, "", limit)` and
// `GetTopMemories(ctx, "", 20)` respectively for a name that resolves to nothing.
// A hard-coded cap here quietly overrode the tool's own argument — the defect
// review found — and the `## Global` section's cap is the one thing that is NOT
// a parity target for that case, so the two numbers are stated at the call sites
// rather than guessed here.
func projectContextGlobalBudget(limit int) assemble.Budget {
	return assemble.Budget{Slices: []assemble.Slice{{
		Bucket:                memory.GlobalProjectID,
		MaxItems:              limit,
		OverFetch:             limit * 2,
		Order:                 memory.OrderDecay,
		DemoteOnlyWhenOverCap: true,
	}}}
}

// assembleProjectContext runs one passive assembly for this surface and returns
// the admitted items.
//
// It is PASSIVE, and passive is keyed on the ABSENCE OF A QUERY rather than on
// SourceProjectCtx. That is the whole reason the call has no query: with one, the
// retriever would fuse legs and answer a relevance question this surface never
// asked, and a project listing has no text to rank against — it has a bucket and
// a policy.
//
// The caller does not read Result.Response, for the reason `Result` documents: it
// is the SEARCH framing, and this surface has its own headings and its own empty
// cases. It reads Result.Items and Result.Reason — the second because the empty
// block has to distinguish "the window was empty" from "rows were found and
// withheld", and only the reason knows which.
func assembleProjectContext(ctx context.Context, s *Server, req assemble.Request) (assemble.Result, error) {
	candidates, ok := s.store.(assembleCapableStore)
	if !ok {
		// The same capability assertion ghost_memory_search makes, and for the
		// same reason: a provider that cannot retrieve candidates must say so
		// rather than answer with rows some other reader chose.
		return assemble.Result{}, fmt.Errorf("this store does not support candidate retrieval, so the project context " +
			"cannot be assembled")
	}
	req.Source = assemble.SourceProjectCtx
	req.Condition = assemble.CondHybrid
	if req.Now.IsZero() {
		req.Now = time.Now().UTC()
	}
	return assemble.Run(ctx, candidates, req)
}

// projectContextItems renders the admitted rows as this surface's memory lines:
// `Item.Line()` and a newline each, which is what `formatMemories` emitted for
// every row it printed.
//
// The renderer is the assembler's, and the two agree field for field — that is
// not luck. `formatMemories` already called assemble.ScopeLabel,
// ValidityLabel, ConfidenceLabel, AgentLabel and SourceRefLabel in the order
// `Line()` prints them, so this surface was already converged on the LABELS and
// had only not been converged on the selection and the stages. What the stages
// add is the only behavioural difference between the two renderers, and it is the
// point: a row this one reached is a row stage 2 judged current.
func projectContextItems(items []assemble.Item) string {
	var sb strings.Builder
	for _, it := range items {
		sb.WriteString(it.Line())
		sb.WriteString("\n")
	}
	return sb.String()
}

// projectNotRegistered is the sentence for a project name no `projects` row
// matches, and there is ONE of it for three callers.
//
// `ResolveProject` answers an unknown name with `("", "", nil)`, so every surface
// that resolves before reading can be handed an empty id. The old loader was
// handed it too and read `project_id = ” OR project_id = '_global'`, which lists
// the GLOBAL rows under a heading naming a project that does not exist — and falls
// through to this sentence only when the store happens to hold no globals. The
// assembler refuses a project context with no project, so without a guard here all
// three surfaces hard-error instead, and an error is worse than the old
// inconsistency: a caller cannot act on "something went wrong" by saving a memory
// to the project it named.
//
// The RAW name is the argument, not the resolved id, and that is the whole reason
// this is a function rather than a format string at each call site: the resolved id
// is `""` here, and `Project "" is not registered` names nothing the caller can
// act on.
func projectNotRegistered(asked string) string {
	return fmt.Sprintf("Project %q is not registered with Ghost yet — nothing has ever been saved for it. "+
		"Call ghost_memory_save to create it.", asked)
}

// projectNotRegisteredAsOf is the `as_of` sibling of projectNotRegistered, and it
// is a REFUSAL rather than the same sentence, for the reason the two branches of the
// tool answer an unknown name differently.
//
// A present-tense call appends the cross-project section: those rows do not depend
// on a project, and the SessionStart instructions send an agent here precisely when
// the directory matched nothing. A caller who asked for an INSTANT cannot be
// handed today's rows — it would be stating one thing and being silently given
// another, with no `as_of` note in the payload to detect it, which is the slip the
// seam's own rule refuses rather than clamps. And there is no set to show: a past
// reading of a project Ghost has never seen is not a reading of anything.
//
// So the answer names the project AND the instant, and offers no rows. The instant
// is in the sentence rather than in a disclosure prefix because there is no block
// for a disclosure to lead — the reader needs to see that the requested instant was
// never consulted, and that is the same sentence's job.
func projectNotRegisteredAsOf(asked string, asOf time.Time) string {
	return fmt.Sprintf("Project %q is not registered with Ghost yet, so there is nothing to read as of %s: "+
		"Ghost has never held a row for it, at any instant. Call ghost_memory_save to create it.",
		asked, asOf.Format(time.RFC3339))
}

// projectContextWithNotRegistered appends the not-registered sentence to whatever
// the surface could still render for an unresolved project, which is the
// `_global` section and nothing else.
//
// It is an APPEND rather than a replacement because those rows do not depend on
// the project. The base ref delivered them for an unknown name — under
// `## Memories`, which is the mislabelling this migration removes — and a first
// session in a project Ghost has never seen is exactly when the cross-project
// preferences and conventions matter. The server's own SessionStart instructions
// tell the agent to call these surfaces when the directory matched nothing, and to
// look for a Global section, so returning the sentence ALONE would leave the
// answer contradicting the instructions shipped with it.
func projectContextWithNotRegistered(text, asked string) string {
	note := projectNotRegistered(asked)
	if strings.TrimSpace(text) == "" {
		return note
	}
	return strings.TrimRight(text, "\n") + "\n\n" + note
}

// projectContextGlobalSection renders the `## Global (applies to all projects)`
// block for this surface, or "" when there is nothing to show under it.
//
// It is the ONE render of that section, shared by the tool and by
// buildProjectContext, so the two cannot spell one heading differently. The
// `alreadyShown` filter is applied AFTER the cap, and that is the whole reason the
// section is a second REQUEST rather than a second slice: it is a section
// boundary, not a selection rule. Filtering it into the store's read would run
// before the cap and admit up to 15 NEW rows where the shipped code admitted 15
// rows of which some were repeats. `SlicePolicy.ExcludeSeen` stays unread for
// that reason.
//
// `limit` is the cap the CALLER asked for, and the two call sites pass different
// ones on purpose — see projectContextGlobalBudget.
func (s *Server) projectContextGlobalSection(ctx context.Context, limit int, alreadyShown []assemble.Item) string {
	seen := make(map[string]bool, len(alreadyShown))
	for _, it := range alreadyShown {
		seen[it.ID] = true
	}
	globals, err := s.projectContextGlobals(ctx, limit)
	if err != nil {
		// A failed global read is silence, exactly as it was when this ran inline:
		// the block is the answer, and a block without a global section beats an
		// error here.
		return ""
	}
	var extra []assemble.Item
	for _, g := range globals.Items {
		if !seen[g.ID] {
			extra = append(extra, g)
		}
	}
	if len(extra) == 0 {
		return ""
	}
	return "## Global (applies to all projects)\n\n" + projectContextItems(extra)
}

// projectContextEmptyNote is what the surface says about a project whose memory
// rows are all gone, and the reason it is not the sentence the section used to
// carry.
//
// The block used to fall through to a CENSUS — "nothing has been saved for it" —
// which was true of a loader that only ever lost rows to its own cap. Stage 2
// introduces a second way for the section to be empty, and on a project whose
// every memory has retired the census becomes a lie: Ghost would report that
// nothing was ever saved about a project it holds a full history for.
//
// So the two cases are separated by the verdict, and the separation is the point
// rather than a refinement of it. `no_memories` means the over-fetched window came
// back EMPTY, which is the only absence this surface can honestly claim about a
// store: the read is `LIMIT limit*2` rows of one project plus `_global`, never a
// count. Any other reason means rows were FOUND and withheld, and then the block
// says so through the assembler's own abstention sentence — the one fact both
// halves agree on, rendered in words that fit a listing rather than a search —
// and names the surface that still shows the retired rows.
//
// An empty return means the census applies and the caller keeps its own.
func projectContextEmptyNote(res assemble.Result) string {
	if res.Outcome != assemble.OutcomeEmpty || res.Reason == assemble.ReasonNoMemories {
		return ""
	}
	note := res.Abstention
	if note == "" {
		// Unreachable while every reason below renders a sentence, and falling
		// back to the census would reintroduce the lie this function exists to
		// prevent — so the fallback states the fact in the fewest words that
		// cannot be wrong.
		return "Ghost holds memories for this project, but none of them is current."
	}
	return note + " Call ghost_memories_list to see them, still marked with the window they carry."
}

// projectContextOwnRowsNote is the SAME census, moved off the gate it was on.
//
// `projectContextEmptyNote` answers only when the whole block is empty, which was
// the right gate for a loader whose block could only go empty with the project and
// the wrong gate for a MIXED bucket: `projectContextBudget` sets `IncludeGlobal`, so
// the block carries `_global` rows whenever the store holds any — and
// `cmd/ghost/bootstrap.go` seeds the global memories on every real store. So on a
// project whose every memory has retired, the block was a list of cross-project
// preferences under a `## Memories` heading, saying nothing at all about the
// project's own rows. Strictly less than the base reader gave, since
// `GetTopMemories` did not filter validity and listed them marked `expired`.
//
// The gate here is the PROJECT's own rows, which is the question a caller actually
// asked: is anything above mine? An empty block is a different question and
// `projectContextEmptyNote` still answers it, because for an empty block the
// assembler's own verdict is the sharper instrument — it knows WHY the rows are gone.
//
// The population split has to happen HERE rather than in the assembler, because with
// one bucket holding two populations nothing above the caller can tell them apart:
// the live global is an admitted item, so the outcome is `answerable` and the reason
// is empty even when the project's every row was withheld. That is the price of
// expressing "one cap over the union" as a union instead of two buckets, and two
// buckets at `limit` each would admit twice the rows the caller asked for.
//
// The count is `CountMemories`, which covers rows the block dropped for ANY reason —
// validity, the cap, dedup, resolution — so the sentence names no cause and stays
// true in all of them. It is not `len(items)`: a project whose only row lost the cap
// holds a row, and the same sentence is the honest one for it.
//
// Silence on an error, like everywhere else on these surfaces: a failed count is not
// evidence that the project holds nothing, and the census is the one claim this
// surface may only make from a verdict.
func (s *Server) projectContextOwnRowsNote(ctx context.Context, projectID string, res assemble.Result) string {
	// `_global` IS a project, not a bucket that borrowed one, and an unresolved name
	// has no project to count rows for — it gets the not-registered sentence, which
	// says something stronger than this.
	if projectID == "" || projectID == memory.GlobalProjectID {
		return ""
	}
	// An empty block is projectContextEmptyNote's, and its verdict beats a count:
	// it can say the rows were found and withheld, which is the useful half.
	if len(res.Items) == 0 {
		return ""
	}
	for _, it := range res.Items {
		if it.ProjectID == projectID {
			return "" // a row of the project's own is in the block, so there is no gap to report
		}
	}
	n, err := s.store.CountMemories(ctx, projectID)
	if err != nil {
		return ""
	}
	if n == 0 {
		// The empty half of the same gate, and it was the same misattribution with
		// one clause fewer: a registered project holding nothing, on a store with any
		// global row, was answered with the global row under `## Memories` and told
		// nothing. This is a parity CHANGE where the withheld half is a parity fix —
		// `GetTopMemories(ctx, "", 20)` returned exactly this — and it is here
		// because repairing the gate halfway would leave the same defect with fewer
		// words. The sentence names only the absence, because a `## Learned Context`
		// or `## Recent Decisions` section above may well hold something.
		return "Ghost holds no memories for this project; every row above applies to all projects."
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
