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
func loadSessionPassive(ctx context.Context, store *memory.Store, cfg *config.Config, projectID string, now time.Time) (memories, globals []sessionMemory) {
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
		Budget:    sessionPassiveBudget(cfg, projectID),
	})
	if err != nil {
		// Warn, not Debug, and the reason the default handler is enough: nothing
		// on the hook path calls slog.SetDefault, so the process default runs at
		// Info and a Debug record is dropped without ever being formatted. A
		// Debug line here would have been a diagnostic that never fires, while
		// the block below it renders with no memories and nothing saying why.
		slog.Warn("ghost: session-start assembly failed, so the block has no memory rows", "error", err)
		return nil, nil
	}
	// The buckets are separated by the row's OWN project rather than by the order
	// it arrived in: the retriever interleaves nothing, but a caller that ordered
	// its slices differently would, and a global row rendered in the project
	// section would be a claim the block never made.
	for _, it := range res.Items {
		row := sessionMemory{
			ID: it.ID, Category: it.Category, Content: it.Content, Pinned: it.Pinned,
			ProjectID: it.ProjectID, Scope: it.Scope, Source: it.Source,
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
	return memories, globals
}

// globalCount is the count the block's "N of M" line divides by for the global
// section, read the way both callers read it: one COUNT over the live rows of the
// global project. A failed count is reported as UNKNOWN rather than as zero,
// because "0 of 0" is a claim that there is nothing and "total unknown" is a
// claim that the count did not run — and the block's wording for the second is
// the honest one.
func globalCount(db *sql.DB) (total int, known bool) {
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE project_id = ? AND resolved_at IS NULL`, memory.GlobalProjectID,
	).Scan(&total); err == nil {
		return total, true
	}
	return 0, false
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
	return memory.NewStoreWithRead(db, db, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	})))
}
