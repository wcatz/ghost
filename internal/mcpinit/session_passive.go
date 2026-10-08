package mcpinit

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"time"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

// sessionDisplayBytes and globalsDisplayBytes are the per-item PREVIEW budgets of
// the rendered block, applied here rather than through the slice's ClampBytes
// because the two are not the same operation.
//
// ClampBytes cuts an item to exactly its budget on a rune boundary and stops. A
// preview has to SAY that it was cut, so the block appends an ellipsis and every
// shortened line reads as a shortening — a line that is merely short is a claim
// about the memory, and one silently cut short is a lie about it. The stored row
// is untouched either way: this is display truncation only, and
// ghost_memories_list reads the full content back.
//
// The assembler's clamp is also the wrong owner for a mechanical reason: a caller
// cannot tell a row the clamp shortened from one that happened to be exactly its
// budget, and the ellipsis needs that distinction to be answerable. So the byte
// cap stays with the renderer that has to render the difference.
//
// The two numbers are the caps' own arithmetic, and the reasoning is theirs: the
// bucket with more items spends fewer bytes on each, so the two sections stay
// comparable in size rather than one of them dominating the block.
const (
	sessionDisplayBytes = 200
	globalsDisplayBytes = 300
)

// sessionPassiveBudget is the ONE statement of what the session-start block
// selects, and it is the only place the two bucket policies are written down.
//
// Before this, the project's rows and `_global`'s were fetched by two private
// queries with two private over-fetches, two private two-pass selections and two
// private near-duplicate passes, and the assembler's stages ran over a block that
// had already been chosen by a second implementation of the same rules. The caps
// are unchanged — sessionMemoriesCap and globalsCap are still the admission
// bounds — but they are now BOUNDS rather than the whole of the selection.
//
// The per-item byte budgets did NOT move into these policies, and that is the one
// thing here that stayed put on purpose: they are applied by the caller, per
// bucket, because a preview has to be able to say it was cut. See the constants.
//
// The two policies disagree deliberately, and the disagreement is policy rather
// than drift: globals are capped tighter, ranked on pin/importance/recency rather
// than the decay composite, and their near-duplicate losers are DROPPED rather
// than reordered — a superseded preference is not worth eight slots of a
// cross-project block.
func sessionPassiveBudget(cfg *config.Config, projectID string) assemble.Budget {
	inj := cfg.Injection

	// MaxItems and MaxBytes stay 0: they bound the RENDERED RESPONSE, and the
	// response-fit framing is the search framing — this surface spends its budget
	// in the per-bucket caps instead, which is what the caps have always meant
	// here.
	slices := make([]assemble.Slice, 0, 2)

	// The project slice is conditional, and the condition is the no-match case:
	// an empty ProjectID is a slice bucket the assembler refuses, and a directory
	// that matched no project has no rows to fetch. The global slice below is not
	// conditional, which is the case the old shape got for free — loadGlobals ran
	// at the hook level, so an unrecognised directory still rendered a Global
	// section. A budget carrying only the project slice would drop it, and the
	// block a user gets in a directory Ghost does not know would silently lose the
	// cross-project rows it has always shown.
	if projectID != "" {
		slices = append(slices, assemble.Slice{
			Bucket: projectID, MaxItems: sessionMemoriesCap,
			OverFetch: sessionMemoriesCap * 3,
			Order:     memory.OrderDecay, TwoPass: inj.BehaviorFloor > 0,
			BehaviorFloor: inj.BehaviorFloor, BehaviorCategories: inj.BehaviorCategories,
			CategoryWeights: inj.CategoryWeights, CategoryCaps: inj.CategoryCaps,
			DemotionThreshold: cfg.Linking.DemotionThreshold,
			// A demotion is a reorder, and on a set that fits under the cap it
			// can only shuffle rows the block shows in full. The loader this
			// replaces skipped it there too.
			DemoteOnlyWhenOverCap: true,
		})
	}
	slices = append(slices, assemble.Slice{
		Bucket: memory.GlobalProjectID, MaxItems: globalsCap,
		OverFetch: globalsCap * 2,
		Order:     memory.OrderPinnedImportanceUpdated,
		// The lower threshold is the one the globals loader carried, and its
		// reason survives the move: a live near-duplicate pair of global
		// preferences was observed linking at 0.8857, just under the general
		// 0.90 the project bucket uses, and globals get no second pass.
		DemotionThreshold: globalsDemotionThreshold,
		// And the difference that is a real selection change, not a re-expression:
		// the globals loader DROPPED its demoted losers rather than reordering
		// them, because a superseded preference is not worth one of eight slots
		// in a block that competes for attention across every project.
		DropDemotedLosers: true,
	})
	return assemble.Budget{Slices: slices}
}

// sessionTally is the two buckets' fates as the session-start block needs them,
// counted from the assembler's trace rather than from a second COUNT of the
// store. It is the block's own arithmetic: the totals it divides by come from
// the same retrieval that produced the rows, so a header cannot describe a
// store the block was not assembled from.
type sessionTally struct {
	project assemble.BucketTally
	globals assemble.BucketTally
	// emptyNote is assemble.EmptyNote of the retrieval's own result, set only
	// when the project's rows were all withheld and nothing else (no `_global`
	// row was seen) shaped the verdict, so the reason it names is the project's.
	// It is what ghost_project_context prints for the same state.
	emptyNote string
	// asOfWithheld counts the rows a historical read left out because their
	// validity window had closed or not yet opened at the requested instant
	// (project and globals together). It is counted, not inferred, and it is
	// zero on every current read.
	asOfWithheld int
}

// loadSessionPassive assembles the session-start block's memory rows: the
// project's own and `_global`'s, selected by the retriever's passive policies and
// shaped by the assembler's stages, in one call.
//
// It is a PASSIVE retrieval, and passive is keyed on the ABSENCE OF A QUERY
// rather than on SourceSessionStart. That is the whole reason the call has no
// query: with one, the retriever would fuse legs and answer a relevance
// question this surface never asked, and a session start has no text to rank
// against — it has a bucket per project and a policy per bucket.
//
// Everything the loaders answered that is NOT a memory row stays where it was.
// The learned-context summary, the open tasks, the active decisions, the
// interaction count and the two total counts are separate reads with their own
// shapes, and the assembler's Item carries none of them; moving them would be a
// different migration, and doing it here would be a change nobody asked for in a
// commit about which reader selects the rows.
//
// A refused request is silence, which is what the loaders' answer to a failed
// read already was: this path runs inside a host's editor session, and a broken
// store must cost a session nothing rather than block it. What is NOT silent is
// a store this build cannot read at all, because `Ghost memory is active` is
// printed by the surrounding handler and an empty block under it reads as
// "nothing was ever saved" — so the failure is reported on stderr with its reason
// attached, and the block still renders.
//
// `record` is the retrieval record's own sink and the reason this function takes
// one: the block a session is OPENED with is a retrieval like any other, and until
// #850 it was the one the audit could not see. It is a PARAMETER rather than
// something this package opens for itself, because the handle it needs cannot be
// the one the reads use — those are read-only (memory.OpenReadDB, mode=ro) — and
// because the branch with no project to attribute the call to must pass nil and
// record nothing. See sessionRecordSink.
func loadSessionPassive(ctx context.Context, store *memory.Store, cfg *config.Config, projectID string, now time.Time, record assemble.RecordSink, sessionID string) (memories, globals []sessionMemory, tally sessionTally) {
	budget := sessionPassiveBudget(cfg, projectID)
	res, err := assemble.Run(ctx, store, assemble.Request{
		ProjectID: projectID,
		// The empty Query IS the passive shape. It is not a placeholder: it is
		// what makes the retriever take the passive branch, and a non-empty
		// query here would answer a different question with a fused window.
		Query:     "",
		Source:    assemble.SourceSessionStart,
		Condition: assemble.CondHybrid,
		Now:       now,
		Scope:     cfg.Injection.SessionScope,
		Budget:    budget,
		// The retrieval record (#850), on the seam #646 built, and the same fields
		// ghost_memory_search sets. It is the ASSEMBLER that writes the row, so
		// nothing about its shape is re-derived here and the verdicts it carries
		// are this surface's own selection rather than a second reading of it.
		//
		// nil on the branch with no project to attribute the call to (see
		// loadSessionContext), and the assembler treats nil as "record nothing".
		Record: record,
		// SessionID is the hook payload's own session id, "" for a caller with no
		// payload (`ghost context`, opencode's plugin). It is what lets the audit match
		// this block's record to the stop hook's scan of the same session; a record
		// with none is left unjudged. `source` still tells the block from a search.
		SessionID: sessionID,
		// The hook's own stderr logger, and for the reason the search path passes
		// one rather than letting emit fall back to the process default: nothing in
		// Ghost calls slog.SetDefault, so a dropped record would be routed to a
		// handler nobody reads — "logged, not silent" would be true only in a test.
		// The block below renders either way; what must not happen is a record
		// quietly ceasing to be written.
		Logger: sessionLog(),
		// SuppressRecordWhenLegsFailed is deliberately NOT set, which reads the
		// opposite way round from the search handler's choice for one reason:
		// that handler turns a leg failure with nothing admitted into an ERROR, so
		// the call never reached its caller as an answer. This surface always
		// answers — a block, an abstention sentence, or the unmatched-directory
		// form — so the call DID reach the agent and the row belongs in the
		// denominator. The leg failure itself lives in the trace, which the record
		// does not carry; that is the same division of labour the search path
		// relies on.
	})
	if err != nil {
		// Warn, not Debug, and the reason the default handler is enough: nothing
		// on the hook path calls slog.SetDefault, so the process default runs at
		// Info and a Debug record is dropped without ever being formatted. A
		// Debug line here would have been a diagnostic that never fires, while
		// the block below it renders with no memories and nothing saying why.
		slog.Warn("ghost: session-start assembly failed, so the block has no memory rows", "error", err)
		return nil, nil, sessionTally{}
	}
	// The buckets are separated by the row's OWN project rather than by the order
	// it arrived in: the retriever interleaves nothing, but a caller that ordered
	// its slices differently would, and a global row rendered in the project
	// section would be a claim the block never made.
	for _, it := range res.Items {
		row := sessionMemory{
			ID:            it.ID,
			Category:      it.Category,
			Content:       it.Content,
			Tags:          it.Tags,
			Importance:    it.Importance,
			Pinned:        it.Pinned,
			CreatedAt:     it.CreatedAt,
			Scope:         it.Scope,
			ProjectID:     it.ProjectID,
			Source:        it.Source,
			ResolvedAt:    it.ResolvedAt,
			ValidFrom:     it.ValidFrom,
			ValidUntil:    it.ValidUntil,
			VerifiedAt:    it.VerifiedAt,
			ValidityState: it.ValidityState,
			Confidence:    it.Confidence,
			Agent:         it.Agent,
			SourceRef:     it.SourceRef,
			ConflictsWith: it.ConflictsWith,
			SupersededBy:  it.SupersededBy,
		}
		if it.ProjectID == memory.GlobalProjectID {
			// 300 bytes here vs. 200 below is deliberate, not drift: globals are
			// capped at a much smaller item count (globalsCap=8), so a larger
			// per-item byte budget still keeps the total section bytes low.
			row.Content = truncateUTF8(row.Content, globalsDisplayBytes)
			globals = append(globals, row)
			continue
		}
		// 200 bytes per item, and smaller than the globals' 300 above, because
		// project memories have a larger cap (sessionMemoriesCap=15 vs.
		// globalsCap=8): a smaller per-item budget keeps total section bytes
		// comparable.
		row.Content = truncateUTF8(row.Content, sessionDisplayBytes)
		memories = append(memories, row)
	}
	// The tally is the trace's own split of each bucket into shown, ranked-out
	// and withheld rows, and it is computed HERE rather than in the renderer
	// because the trace travels with the result: a caller rendering an answer
	// must not reconstruct what the stages saw. CountsFor is nil-safe against a
	// trace that never ran, so an error path that already returned above is not
	// the only place this can be reached.
	tally = sessionTally{
		project: assemble.CountsFor(res.Trace, projectID, len(memories)),
		globals: assemble.CountsFor(res.Trace, memory.GlobalProjectID, len(globals)),
	}
	// The window is an over-fetch, so the trace counts only the rows it fetched.
	// One count per bucket, over the same predicates the window's fetch uses,
	// supplies the rows behind it; an unreadable count leaves the tally as the
	// trace made it (the header then describes the window, which is the most it
	// can honestly say).
	for _, sl := range budget.Slices {
		n, excluded, err := store.PassiveEligibleCount(ctx, memory.SlicePolicy{Bucket: sl.Bucket, IncludeGlobal: sl.IncludeGlobal}, now, cfg.Injection.SessionScope)
		if err != nil {
			slog.Warn("ghost: session-start eligible count failed, so the header counts the retrieval window", "bucket", sl.Bucket, "error", err)
			continue
		}
		switch sl.Bucket {
		case projectID:
			tally.project = tally.project.CountedAgainst(n, excluded, sl.OverFetch+windowExtra(res.Trace, sl.Bucket))
		case memory.GlobalProjectID:
			tally.globals = tally.globals.CountedAgainst(n, excluded, sl.OverFetch+windowExtra(res.Trace, sl.Bucket))
		}
	}
	if tally.globals.Total() == 0 && tally.project.WithheldNote() != "" {
		tally.emptyNote = assemble.EmptyNote(res)
	}
	return memories, globals, tally
}

// sessionStore is the ONE construction of the store the session-start read runs
// against, and its logger is the point of it.
//
// The loaders this replaces reported four failures on stderr — a schema version
// it could not read, and three demotion-lookup failures — and the read-only
// handles those diagnoses came through are the same ones the retriever seam
// reads. An `io.Discard` logger is therefore not a neutral default here: it
// deletes diagnostics the shipped code had, and the block is then rendered
// exactly as a healthy store's would be. A store whose version cannot be read
// loses its scope filter and its tier label; a store whose demotion lookup fails
// re-offers a superseded preference and can show a near-duplicate pair as two
// independent lines. Neither is a difference a user can see, which is exactly
// why the old code said so out loud.
//
// The level is Warn, not Info: the store logs its own expected states at Debug —
// a pre-provenance store skipping an evidence read is normal, not a fault — and
// an Info handler would turn a per-session-start no-op into a line on every
// session. The three failure sites are Warn in internal/memory for the same
// reason: a real failure is loud, an expected state is not.
func sessionStore(db *sql.DB) *memory.Store {
	return memory.NewStoreWithRead(db, db, sessionLog())
}

// sessionLog is the ONE logger the session-start path hands out, and the level is
// the point.
//
// Warn, not Info: the store logs its own expected states at Debug — a
// pre-provenance store skipping an evidence read is normal, not a fault — and an
// Info handler would turn a per-session-start no-op into a line on every session.
// The three failure sites it covers are Warn in internal/memory for the same
// reason: a real failure is loud, an expected state is not.
//
// It is built per call rather than cached in a package variable because the
// destination is os.Stderr READ AT CONSTRUCTION TIME, and a hook is a
// short-lived process: a cached handler would outlive whatever the caller did to
// the variable (a test's redirect, a host's redirection) and quietly keep
// writing to the old descriptor.
func sessionLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	}))
}

// sessionRecordSink opens the ONE handle the session-start record write needs, and
// returns nil when there is no store to record into.
//
// Why a second handle at all, since the reads above already have one: that handle
// is read-only. `loadSessionContext` opens memory.OpenReadDB, whose DSN is
// mode=ro, so wiring the record to it would compile, satisfy
// assemble.RecordSink, and fail on every session — with the failure swallowed by
// the very fail-open contract the record is written under, which is the worst
// combination available: every behavioural test stays green and the audit is
// simply never written. TestTheSessionStoreCarriesTheRecordToo is the source
// assertion that pins the difference.
//
// So this is the same construction bumpSessionCount uses, and deliberately not a
// near-copy of it:
//
//   - the SAME rwDSN, so `_txlock=immediate` and busy_timeout(5000) are the ones
//     the store's own guards were reasoned about — in particular the one this
//     write actually relies on, because the newer-store check runs INSIDE the
//     write transaction and a deferred BEGIN would leave the pre-BEGIN race
//     (#746). That guard is free here: the write goes through
//     memory.Store.RecordRetrieval, which runs it.
//   - the SAME os.Stat guard, so a missing database is never CREATED by the write
//     meant to describe it, and the permission pass stays after it for the same
//     reason.
//   - the same second TightenPermissions pass on close, because a clean close
//     checkpoints the -wal and -shm files away but a live `ghost mcp` holds the
//     same database, so those files outlive this function and have to be tightened
//     while it still can.
//
// It goes through sessionStore rather than memory.OpenDB on purpose: OpenDB is the
// constructor that MIGRATES, and a session hook must not migrate a store behind a
// live server's back — nor pin MaxOpenConns(1), which would serialise this pool
// against the record write's own connection.
//
// The returned close func is never nil, so the caller needs no error path: a nil
// sink with a no-op close is the whole of the "no store here" case, which is what
// an unmatched directory and a first-ever session both look like.
func sessionRecordSink(dbPath string) (assemble.RecordSink, func()) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, func() {}
	}
	// Best-effort and before the open, for the reason bumpSessionCount says: the
	// hook is often the only Ghost process to touch a database between two
	// sessions, so a mode left loose by an older build is still loose when this
	// connection lands.
	memory.TightenPermissions(dbPath)
	db, err := sql.Open("sqlite", rwDSN(dbPath))
	if err != nil {
		return nil, func() {}
	}
	return sessionStore(db), func() {
		_ = db.Close()
		memory.TightenPermissions(dbPath)
	}
}

// windowExtra is how many rows the retrieval carried past a bucket's over-fetch
// (the replacements of pinned rows), which the eligible count must treat as inside
// the window.
func windowExtra(trace *assemble.Trace, bucket string) int {
	if trace == nil {
		return 0
	}
	return trace.WindowExtra[bucket]
}
