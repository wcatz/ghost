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

// The two section headings, as constants, because #809 turned out to need both of
// them in three places and the two surfaces are not allowed to spell one
// differently. `## Global (applies to all projects)` in particular is not
// decoration: the server's own SessionStart instructions key their trust guidance
// on that exact heading — "Global memories under \"Global (applies to all
// projects)\" apply across every project, but they are not all the user's own" —
// so a row that loses the heading loses the guidance, and a row that gains a
// near-enough heading is guidance the agent cannot match.
const (
	memorySectionHeading = "## Memories"
	globalSectionHeading = "## Global (applies to all projects)"
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

	// The retrieval record (#850, on the seam #646 built). The same two fields
	// ghost_memory_search sets and the same sink: the assembler writes the row, so
	// nothing about its shape is duplicated here, and the verdicts it carries are
	// this surface's own selection rather than a re-derivation of it.
	//
	// Set in THIS function rather than at the call sites, because all FOUR
	// project-context surfaces — the tool, the `ghost://project/{id}/context`
	// resource, the `recall_project` prompt, and (since #581) the
	// `ghost://memories/global` resource — reach the assembler through here. A sink
	// wired at the callers would record the tool and leave the other three
	// unaudited, and a resource read is a retrieval an agent acted on exactly as
	// much as a tool call is.
	//
	// That last one is a BEHAVIOUR CHANGE rather than a wiring detail, so it is
	// stated here as well as pinned. `ghost://memories/global` read `_global`
	// through `Store.GetTopMemories` until #581, which ranked and trimmed in SQL
	// and wrote nothing; reaching the assembler gives it this row. Keeping it is
	// the decision — a listing the audit cannot see is a listing whose per-source
	// precision figures mean nothing — and `TestTheGlobalMemoriesResourceRecordsItsRead`
	// is what holds it to one row per read, attributed to `_global`.
	//
	// nil when the store cannot record, which the assembler treats as "record
	// nothing": a provider that cannot be audited still answers a listing, and the
	// missing row is a gap in a report rather than a failed call.
	req.Record = s.recordSink()
	// The server's own logger, and for the reason the search path passes it rather
	// than falling back to the process default: nothing in Ghost calls
	// slog.SetDefault, so a refusal sent there would reach a handler nobody reads,
	// and a dropped record would be silent in production while looking logged in a
	// test.
	req.Logger = s.logger
	// SuppressRecordWhenLegsFailed is deliberately NOT set here, which is the
	// opposite of the search handler's choice and reads the other way round for one
	// reason: that handler turns a leg failure with nothing admitted into an ERROR,
	// so the call never reached the caller as an answer and a row would put a
	// denominator in the audit for a call that delivered nothing. This surface
	// answers — a block, an abstention sentence, or the census — so the call DID
	// reach the agent, and the record carries the outcome the assembler actually
	// reached. The leg failure itself is in the trace, which the record does not
	// carry; that is the same division of labour the search path relies on.
	//
	// SessionID is left empty on purpose. The column is the transport's own id, and
	// over stdio — the transport Ghost ships — it is "" here and on the search path
	// alike, so the two agree; what tells a listing from a search in the audit is
	// `source`, which is exact. Threading an *mcp.CallToolRequest down to reach it
	// would also have to invent a value for the resource and prompt callers, which
	// have none — and a session id on two of three surfaces is worse than none.
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

// projectContextSplit separates an admitted window into the requested project's
// own rows and the `_global` ones, by the row's OWN project rather than by the
// order it arrived in (#809).
//
// This is the fix for the one heading the trust guidance depends on. A resolved
// project's window is a UNION — `projectContextBudget` sets `IncludeGlobal`, so the
// read is `project_id = ? OR project_id = '_global'` — and the whole union was
// rendered under a single `## Memories` heading. So a cross-project row was listed
// as one of this project's memories with nothing on the line saying otherwise, on
// the most common path, while the UNRESOLVED path and `docs/mcp.md` both put the
// same rows under `## Global (applies to all projects)`. Only the per-row `source=`
// label survived, and the SessionStart instructions say explicitly to trust the
// heading rather than the fact that a row is global.
//
// The ordering is PRESERVED within each half, and that is the whole constraint on
// the split: the window is ranked by one composite over both populations, so
// re-sorting either half would change which rows a caller sees. Splitting is a
// partition, not a selection, and the caller still admits exactly the rows the
// window admitted.
//
// It is keyed on `it.ProjectID` rather than on a comparison against the requested
// project, which is what makes the `_global` case fall out correctly: a caller that
// asks for `project_id: "_global"` gets a window of globals and therefore an empty
// `own` half and a `## Global` section for the whole block — which is what that
// request is, and is the same listing `ghost://memories/global` serves. The session
// start's `loadSessionPassive` partitions the same way for the same reason, and
// this is the project-context half of that one rule.
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

// splitMemoriesByProject is projectContextSplit for the `as_of` branch, which
// reads `memory.Memory` through `formatMemories` rather than `assemble.Item`.
//
// It is a second function rather than a generic one because the two types are
// different types: `assemble.Run` does not produce `memory.Memory` and
// `MemoriesAsOf` does not produce `assemble.Item`, and a shared generic helper over
// both would have to reach for a field neither of them exposes. The RULE is one —
// the row's own `ProjectID`, `_global` means global — and this is the same
// predicate written against the other type, which is the honest form of "one rule,
// two renderers".
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

// projectContextSection writes one `## `-headed section onto the block, with the
// blank-line separation every section after the first carries, and writes NOTHING
// for an empty body.
//
// The separator is here rather than at each call site because the two surfaces
// write their sections in different orders — the tool's Global section is
// immediately after the memories, the resource's comes after `## Recent
// Decisions` and `## Learned Context` — and a separator spelled at four call sites
// is a separator that will be spelled four ways. `sb.Len() > 0` is what makes the
// first section unseparated on both.
//
// A body that already ends in a newline is not a special case: `projectContextItems`
// ends every row with one, and that is what produces the two blank lines the block
// has always had between sections.
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
//
// It is a function for a second reason now, and it is why the value goes through
// `memory.ProjectArg` rather than `%q`: this is the sentence an UNKNOWN project
// produces, so an agent that pasted a clone URL with embedded auth into `project_id`
// arrives here with a token in it, and the answer is returned as tool text into
// its own context. Every other sentence that quotes the argument goes through the
// same renderer, and there are now enough of them that leaving this one on `%q`
// would make the rule "a refusal never quotes a credential-shaped project
// argument" true of every site except the one that fires most often for a project
// Ghost has simply never heard of.
func projectNotRegistered(asked string) string {
	return fmt.Sprintf("Project %s is not registered with Ghost yet — nothing has ever been saved for it. "+
		"Call ghost_memory_save to create it.", memory.ProjectArg("project_id", asked))
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
	return fmt.Sprintf("Project %s is not registered with Ghost yet, so there is nothing to read as of %s: "+
		"Ghost has never held a row for it, at any instant. Call ghost_memory_save to create it.",
		memory.ProjectArg("project_id", asked), asOf.Format(time.RFC3339))
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
// block onto the block being built, or writes nothing when there is nothing to
// show under it.
//
// It writes into the caller's builder rather than returning a string, because the
// heading and the blank-line separation around it are part of the section and
// `projectContextSection` is the one place either is spelled. Returning a string
// would put that decision back at two call sites with different section orders.
//
// It is the ONE render of that section's read, shared by the tool and by
// buildProjectContext, so the two cannot disagree about which rows the second read
// contributes — and after #809 the heading is load-bearing rather than cosmetic,
// because the server's own instructions key their trust guidance on it.
//
// `alreadyShown` is every row the block has rendered, and `carried` is the
// `_global` half of the mixed window (#809): the rows the first read delivered under
// `## Memories` and that now belong under this heading. They are rendered HERE,
// inside the one Global section, rather than under a second copy of the heading
// beside the memories — a block with two `## Global` headings reads as a duplicate,
// and the second one's rows look like more than the cap allows.
//
// That is also why `carried` is a parameter rather than something this function
// reads: the resource's Global section comes after `## Recent Decisions` and
// `## Learned Context`, and the tool's comes straight after the memories, so the
// rows have to be handed over rather than fetched again.
//
// The `alreadyShown` filter is applied AFTER the cap, and that is the whole reason
// the section is a second REQUEST rather than a second slice: it is a section
// boundary, not a selection rule. Filtering it into the store's read would run
// before the cap and admit up to 15 NEW rows where the shipped code admitted 15
// rows of which some were repeats. `SlicePolicy.ExcludeSeen` stays unread for that
// reason.
//
// A failed global read is silence about the EXTRA rows and nothing more: the rows
// the window already carried are in hand, and dropping them because a second read
// failed would trade a section that is true for one that is missing. The block is
// the answer, and a block with fewer globals beats an error here.
//
// `limit` is the cap the CALLER asked for, and the two call sites pass different
// ones on purpose — see projectContextGlobalBudget.
func (s *Server) projectContextGlobalSection(ctx context.Context, sb *strings.Builder, limit int, alreadyShown, carried []assemble.Item) {
	rows := carried
	if globals, err := s.projectContextGlobals(ctx, limit); err == nil {
		seen := make(map[string]bool, len(alreadyShown))
		for _, it := range alreadyShown {
			seen[it.ID] = true
		}
		// A fresh slice rather than an append onto `carried`: the caller owns that
		// one, and `append` would write into its spare capacity — the kind of alias
		// that shows up as a duplicated row in a block nobody is mutating.
		rows = make([]assemble.Item, 0, len(carried)+len(globals.Items))
		rows = append(rows, carried...)
		for _, g := range globals.Items {
			if !seen[g.ID] {
				rows = append(rows, g)
			}
		}
	}
	projectContextSection(sb, globalSectionHeading, projectContextItems(rows))
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
// An empty return means the census applies and the caller keeps its own — and the
// caller asks it on a SECOND condition, which is not about the block at all: a
// non-empty block whose memory read admitted NOTHING. `projectContextOwnRowsNote`
// defers to it there, and the reason it can is that the question this answers is
// about the rows, not about the text.
//
// IT STILL ANSWERS ABOUT A WINDOW, SO IT IS NOT A PROJECT-SCOPED CLAIM ON ITS OWN.
// `projectContextBudget` reads this project OR `_global`, so an exclusion reason on
// an empty item set can be describing rows that belong to `_global` — and the
// project itself may hold none at all.
//
// So it has FOUR callers, and every one of them establishes that the rows are
// the population before rendering what comes back — one with a count, three
// because they do not need one.
//
// `projectContextOwnRowsNote` is the one that counts, counting the project's own
// rows first, and that is not a style preference. `assemble.Result` carries no
// count and the store is not reachable from a function that only renders bytes, so
// a caller that skipped the check could not make the sentence true; it could only
// ship it. It was also the way this went wrong twice: a caller holding the count
// as a permission rather than as the sentence's own input read it, was satisfied,
// and then rendered a different and false sentence.
//
// The other three are the `_global` bucket: `buildProjectContext`'s `_global`
// branch, the `ghost://memories/global` resource, and the
// `ghost_project_context` TOOL's own `args.ProjectID == "_global"` branch —
// which is easy to miss because it lives in the tool handler rather than in
// project_context.go, and which is why the count is written out here rather
// than left for a reader to derive. All three read `_global` ALONE:
// `projectContextGlobalBudget` sets no `IncludeGlobal`, because the bucket is
// already the population — and `projectContextBudget` sets it to
// `projectID != memory.GlobalProjectID`, which is false for that bucket — so an
// exclusion reason there describes the whole window and there is no second
// population for it to be wrong about. That is the whole difference between them
// and the project case, and it is why none of them needs the count rather than a
// reason the count may be skipped: not because their verdict is sharper, but
// because there is nothing to reconcile it against.
func projectContextEmptyNote(res assemble.Result) string {
	return assemble.EmptyNote(res)
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
// asked: is anything above mine? `projectContextEmptyNote` answers the same question
// from the assembler's VERDICT rather than from a count, and it is the sharper
// instrument — it knows WHY the rows are gone — so it answers both the empty block and
// the block whose memory read admitted nothing at all (see the `len(res.Items) == 0`
// branch below). The two are asked in that order by two callers, and neither caller
// has to know which half of the function produced the sentence.
//
// A note is NEVER read off the verdict alone, and the union is why: `res` describes
// the window this project shares with `_global`, so an exclusion reason can be
// describing rows that belong to somebody else. `CountMemories` is the one
// project-scoped fact in reach, and every branch that renders a sentence about the
// project consults it.
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
//
// The count is read here, in the function that owns the choice of sentence, and by no
// other caller. Three review rounds shaped that, and each was the same mistake one
// step along: a SHARED PREDICATE was the first attempt — three call sites, each
// consulting a fact of its own — and it was right about the count and useless about
// the sentence, because a gate that only PERMITS one can be read and then ignored.
// It was: for a project whose every row `ghost resolve` had withdrawn, the count said
// one, the gate opened, `projectContextEmptyNote` returned "" for an empty window,
// and the never-saved census shipped. Reading the count where the sentence is chosen
// makes the two facts one decision rather than a permission and an act; reading it
// ONCE keeps the two branches from disagreeing with each other, since each read is
// its own snapshot and a `ghost_memory_delete` between them would render the `n == 0`
// sentence over a block with no memory rows above it.
//
// TWO COUNTS, because the two sentences are claims about different populations, and
// only one of them is about what the project HOLDS. The second is projectWindowRowCount.
//
// projectWindowRowCount is how many of the project's OWN rows a retrieval window
// could have admitted, which is the population the abstention sentence is about.
//
// It is a capability assertion and not `CountMemories`, because the difference
// between the two counts is the whole of the last review finding on this surface:
// `CountMemories` has no `resolved_at` predicate, so it counts rows `ghost resolve`
// has withdrawn and no window reads. A project whose only row was withdrawn
// therefore reports one, and a gate built on that number went on to tell the project
// its rows "were withheld as out of date" — a cause it did not have, explaining
// rows that belonged to `_global`.
//
// A store that cannot answer is `(0, nil)` rather than an error the caller must
// handle separately, because the caller's response to both is the same: fall through
// to the count sentence, which names no cause. That direction is the cheap one —
// the reader loses a sentence that would have been true, rather than being handed one
// that is not.
func (s *Server) projectWindowRowCount(ctx context.Context, projectID string) (int, error) {
	counter, ok := s.store.(windowCountCapableStore)
	if !ok {
		return 0, nil
	}
	return counter.CountActiveMemories(ctx, projectID)
}

func (s *Server) projectContextOwnRowsNote(ctx context.Context, projectID string, res assemble.Result) string {
	// `_global` IS a project, not a bucket that borrowed one, and an unresolved name
	// has no project to count rows for — it gets the not-registered sentence, which
	// says something stronger than this.
	if projectID == "" || projectID == memory.GlobalProjectID {
		return ""
	}
	// An empty block is projectContextEmptyNote's, and its verdict beats a count:
	// it can say the rows were found and withheld, which is the useful half.
	//
	// So an empty ITEM SET defers to it too, and "empty" here is the assembler
	// admitting nothing — not the block being empty (#788). LEARNED CONTEXT is the
	// section that makes the block non-empty over an empty memory read, because
	// `ghost reflect` writes it into `ghost_state` and writes no memory row at all. So
	// on a project whose every memory has retired and which reflection has already
	// summarised, the answer was the summary alone — the census never fired and the
	// exclusion that replaces it did not either, and the caller was handed a conclusion
	// derived from those very memories with nothing saying they had been retired. That
	// is the unmarked retired claim stage 2 exists to prevent, reached on the shape a
	// mature project actually has.
	//
	// `## Recent Decisions` reaches the same shape only by LOSING its companion row.
	// `RecordDecision` writes a `decision_log` MEMORY in the same transaction
	// (`internal/memory/decisions.go`) and the tool says so — "a companion memory was
	// also saved" — so a project with an active decision normally has a live memory
	// row of its own that the passive read admits, and the section is not what emptied
	// anything. It becomes the shape when that companion is deleted or withdrawn, which
	// is worth knowing for a reader deciding whether to build a fixture on it: the
	// learned-context fixture needs no such step, and a decision-only fixture would
	// put a row of the project's own in the block and answer `""` at the loop below
	// rather than at any gate. `TestTheOwnRowsNoteNeverClaimsThatARowAboveIsCrossProject
	// WhenThereIsNone` is the pre-existing statement of that, and it is why its
	// fixture is learned context.
	//
	// The verdict alone is NOT enough, and the reason is the same union that made the
	// mixed bucket a problem above: `res` describes the WINDOW, which
	// `projectContextBudget` reads as this project OR `_global`, while this sentence
	// is read as a claim about the requested project. A project holding no memory row
	// at all, on a store whose cross-project rows have all aged out, gets
	// `all_invalid` from stage 2 and no admitted item — so deferring on the verdict
	// alone would tell that project Ghost found and retired rows of its own, and
	// point at a `ghost_memories_list` that returns nothing for it. So the count is
	// consulted FIRST here, and it is not a permission but a SENTENCE SOURCE: below,
	// where it is read, both branches render from it.
	//
	// The count is read ONCE, here, and both branches below render from this `n`.
	// Reading it once per branch was a review finding: each read is its own snapshot,
	// so a `ghost_memory_delete` or a `resolve --mark` landing between them could
	// flip the answer to the `n == 0` sentence — "the memory rows above are the
	// cross-project ones" — on a block that has no memory rows above it at all, and
	// it cost an extra COUNT on every read of a project whose memory read admitted
	// nothing.
	//
	// What no test here can kill is a SECOND read, because with no concurrent writer
	// it is the same value and the only difference is the query count — a mutation
	// that re-issues the read survives every fixture, and it would take a writer
	// racing this read to fail. So the single read is stated here as the DESIGN and
	// not claimed as something the suite proves; what the suite proves is that the
	// branches agree about what to DO with a count, which is the half that was
	// wrong.
	//
	// An error is silence here, for the reason the whole function is silent on one:
	// a count nobody could read supports no sentence about the project, and not the
	// census either. `_global` is not asked — it is refused at the guard above, which
	// is the one place that knows a bucket is not a project to count rows for.
	n, err := s.store.CountMemories(ctx, projectID)
	if err != nil {
		return ""
	}
	if len(res.Items) == 0 {
		// The window is empty of admitted rows, and the two sentences that could
		// explain it need two DIFFERENT counts, because they are claims about
		// different populations.
		//
		// The COUNT sentence — "Ghost holds N memories for this project and none of
		// them is in the block above" — is about what the project holds, and `n`
		// answers that. It names no cause, so it is true of a row withdrawn by
		// `ghost resolve` as much as of one that aged out.
		//
		// The ABSTENTION names a cause, so it is a claim about which rows that cause
		// explains, and the population it can be about is the WINDOW's: the project's
		// own rows that `passiveFetchSQL` could have admitted. `n` cannot establish
		// that — it has no `resolved_at` predicate, so a project whose every row was
		// withdrawn reports one and the abstention would then blame it for rows that
		// are `_global`'s and say "out of date" about a row `ghost resolve` retired
		// on purpose. So the abstention asks the window's own question, and where
		// that count is unavailable or zero the cause belongs to somebody else and
		// the count sentence below is the honest answer.
		if n == 0 {
			return "" // the project holds nothing at all, so there is nothing to report
		}
		if inWindow, err := s.projectWindowRowCount(ctx, projectID); err == nil && inWindow > 0 {
			if note := projectContextEmptyNote(res); note != "" {
				return note
			}
		}
		// Otherwise the rows the verdict describes are not this project's, and the
		// count sentence below is what can honestly be said: the project holds `n`
		// rows and none is above. That covers both a window emptied by the FETCH's
		// `resolved_at IS NULL` (no stage withheld anything, so there is no
		// abstention to render even when the verdict names an exclusion) and an
		// exclusion that belongs entirely to `_global`.
	}
	for _, it := range res.Items {
		if it.ProjectID == projectID {
			return "" // a row of the project's own is in the block, so there is no gap to report
		}
	}
	if n == 0 {
		// The empty half of the same gate, and it was the same misattribution with
		// one clause fewer: a registered project holding nothing, on a store with any
		// global row, was answered with the global row under `## Memories` and told
		// nothing. This is a parity CHANGE where the withheld half is a parity fix —
		// `GetTopMemories(ctx, "", 20)` returned exactly this — and it is here
		// because repairing the gate halfway would leave the same defect with fewer
		// words.
		//
		// The first version of this sentence claimed "every row above applies to all
		// projects", and the guard it needed could not be here: this function sees the
		// memory read, and the sections it would be wrong about — `## Learned
		// Context` on the tool, `## Recent Decisions` and `## Learned Context` in
		// buildProjectContext — are rendered by direct reads the assembler never
		// sees. A project can hold zero memory rows and still have learned context:
		// `ghost reflect` writes it into `ghost_state` and `ghost_memory_delete`
		// removes only the `memories` row, so it survives that. It can hold a
		// decision too, but only once that decision's COMPANION memory is gone —
		// `RecordDecision` writes a `decision_log` row in the same transaction, so
		// the section is normally accompanied by a live row of the project's own.
		//
		// So the sentence claims nothing about the rows above. "every row above
		// applies to all projects" is true of a block that is only the memory
		// section, and false of one that also carries this project's own summary —
		// which is the same misattribution the note exists to remove, one section
		// further down. The narrower wording is true in BOTH shapes, and it is still
		// the fact that was missing.
		//
		// So this is a REWORDING rather than a caller-supplied flag, and that is the
		// whole reason the flag was not added: a parameter that suppresses this
		// sentence whenever a project-keyed section sits above would have to be
		// threaded through two callers that each know the answer, and the flag's
		// only job would be to make a narrower sentence unnecessary. The withheld
		// sentence below needed no flag either — "none of them is in the block above"
		// is about the project's MEMORY rows, and a learned summary is not one.
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
