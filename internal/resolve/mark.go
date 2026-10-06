// The targeted mark: stamp resolved_at on the memories a caller NAMES, on an
// operator's say-so, without asking a model whether they should be.
//
// `Run` (resolve.go) is the forward pass and it is right for what it does: it
// proposes, a classifier adjudicates, and the rows that come back RESOLVED with
// a `closed-by:` are stamped. The case this file exists for is the one no amount
// of re-running reaches. An operator has read two notes in the same project and
// seen that the newer one states the older one's fix landed, so the older one is
// finished work. Nothing in the pass can see that: the note may hold no
// resolution keyword at all, so the prefilter never proposes it, and even where
// it does, a KEEP-biased classifier asked about a note whose claim was superseded
// by a DIFFERENT memory answers KEEP about the note, not about the project.
// `--reassess` is no help either — it judges what is ALREADY resolved, and this
// row is not, so it is not in that pool.
//
// So the two options were: leave a note Ghost has been told is finished in every
// session's ranked context, or write `resolved_at` with SQL. The second is not
// an option — it bypasses the `memory_history` row every writer appends, so the
// one artefact that records how a memory reached its current state would show
// this memory appearing from nowhere, resolved, with nothing saying who said so.
// The issue is what the reader of that history cannot then answer: which of the
// resolve rows in it were a pass's judgement and which were a person naming a
// memory they had read. `MarkResolved` writes the performer for exactly that
// reason, and it is the only resolve writer that does.
//
// The difference from `Run` is the whole point, and it is the same difference
// internal/supersede.Withdraw draws: nothing is judged here. The caller has
// decided, the rows are named, and the write goes through the ordinary store
// path — the same guard, the same `resolve` history row, the same transaction —
// so a marked memory is indistinguishable from a resolved one to every reader
// except the history's `agent` column. A dry run is the default, because the
// judgement being trusted is the caller's and the corpus is the thing that has
// to survive being wrong about it.
//
// The refs are the ones `ghost supersede --withdraw` takes, through
// internal/memref and not a second copy of its rules: a full id, or an
// unambiguous prefix of at least eight characters, counted in characters rather
// than bytes because that is what every report prints. Two consequences follow
// from the shared query that this file has to state rather than discover:
//
//   - It reaches `_global` as well as the project, because a promotion moves a
//     row while keeping the links pointing at it and both ends of an edge have to
//     stay nameable. A ref that lands on a `_global` row is therefore a REFUSAL
//     here, not a stamp: the project asked about memories it owns, and a row
//     every project shares is not one of them. A caller holding a ref it cannot
//     use is told which half of the store it named, rather than being left to
//     wonder why the id it pasted was "missing".
//   - It says nothing about eligibility. Whether a row is pinnable, in a standing
//     category, or already resolved is the store's guard to answer, and the
//     answer is reported per row rather than guessed at here, because the three
//     refusals mean different things to whoever reads them.
//
// A request is settled before anything is written, and a refusal writes NOTHING
// — not even the rows that were fine. Several refs in one command are an
// operator correcting a list they read off a report, and marking four of five
// while reporting an error about the fifth is a corpus state nobody asked for and
// cannot get back without knowing which four moved. Every problem is reported,
// not just the first.
//
// The inverse already exists and this file names it: `ghost resolve --reassess
// --only` clears a stamp, and every report ends by printing that command, scoped
// to exactly the rows this one buried. An unscoped repair re-judges every
// resolved memory in the project, which #698 measured proposing to un-hide 143
// rows of which about 35% were stale, so the follow-up is rendered by
// internal/followup — the same renderer the supersede repairs use, so the two
// halves of a repair cannot print different commands for the same situation.
package resolve

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/memref"
)

// MarkStore is the subset of *memory.Store a targeted mark needs; narrowed for
// testability. It is the same shape as reassessStore minus the classifier, the
// evidence read and the two pool reads: nothing here asks a model anything, and
// the only writes go through MarkResolved, which is the ordinary store path.
type MarkStore interface {
	memref.Store
	GetByIDs(ctx context.Context, ids []string) ([]memory.Memory, error)
	MarkResolved(ctx context.Context, projectID string, ids []string, prov memory.Provenance) ([]string, error)
}

// MarkRequest is one operator's request: the refs as typed, the project they are
// read in, the provenance the `resolve` history rows carry, and whether to
// write. It is a value rather than seven parameters because the two callers —
// the CLI and the MCP tool — have to name the same seven, and a transposition
// between them would stamp rows in one project from a ref resolved in another.
type MarkRequest struct {
	// ProjectID scopes both the ref resolution and the write. It is required:
	// a mark with no project is a request about every project, which is not a
	// thing this operation is.
	ProjectID string
	// Refs are memory ids or 8-or-more-character prefixes, as typed and in the
	// order they were given. A repeated ref names one row and is marked once.
	Refs []string
	// Provenance is the PERFORMER recorded on the `resolve` history row, and it
	// is what distinguishes an operator's mark from a pass's verdict in the one
	// place the audit can tell them apart. An empty Provenance is accepted and
	// writes an empty agent, which is the honest record of a caller that knows
	// no more than that — the same contract the history's own comment states.
	Provenance memory.Provenance
	// Apply writes. False is the dry run: every ref is resolved, every row is
	// loaded, every refusal is produced, and nothing is stamped.
	Apply bool
}

// MarkedMemory is one ref resolved to the row it names, and what happened to it.
//
// The row's own text is carried because that is what makes the report checkable:
// an operator burying a memory has to be able to confirm from the output that it
// was the one they meant, which is the same reason the withdrawal report prints
// the memory its edge was burying.
type MarkedMemory struct {
	// ID is the resolved full id, and Ref the spelling the caller used, so a
	// report can show which of five ids on a line the refusal was about.
	ID  string
	Ref string
	// Content and Category are the row's own, as stored.
	Content  string
	Category string
	// Marked is true only for a stamp THIS call wrote. A row already carrying
	// resolved_at is AlreadyResolved instead, which is a no-op and not a mark:
	// the two are separate states because a report that called the second a
	// success would claim a write that did not happen.
	Marked bool
	// AlreadyResolved marks the row this call found already stamped. The issue's
	// "reported as a no-op" is exactly this: the operator is told the memory is
	// already resolved rather than being handed a success, and nothing is
	// written on its account — including no history row, because a history row
	// is a record of a write and no write happened.
	AlreadyResolved bool
	// Pinned and ExemptCategory are the other two reasons the store's guard
	// declines a row, carried so the report can name the one that applies instead
	// of lumping all three under "not marked". A pin is an explicit instruction
	// to keep a memory visible and resolve does not overrule it; convention and
	// preference are standing-knowledge categories the pass never stamps. Both
	// are states a reader of the report can act on, and neither is visible in
	// the resolved_at the request did not set.
	Pinned         bool
	ExemptCategory bool
	// Declined marks a row that was ELIGIBLE when this call read it and was not
	// stamped anyway: the store re-checks its guard at write time, and a pin, a
	// recategorisation or a move to another project that lands between the two
	// reads makes the row ineligible without anything failing.
	//
	// It is its own state rather than the absence of Marked because the store
	// declines such a row silently — it is somebody else's decision, not an error
	// — so nothing else in the result says it happened. Without it a report's
	// default marker ("marked") claims a stamp that was never written, which is
	// the one thing a report about a change must never do: the memory is not
	// buried, and the operator who reads that it is will not look for it again.
	Declined bool
	// MarkFailed marks every row in a request whose write errored. It is
	// all-or-nothing by construction rather than by reporting convention:
	// MarkResolved is ONE transaction over every batch, so an error is a rollback
	// and no row moved. The field exists so the report can say the stamp did not
	// happen, instead of printing a request of several rows as though the ones it
	// happened to reach were marked — the same claim the AlreadyResolved no-op
	// exists not to make.
	MarkFailed bool
}

// MarkResult summarizes a request. Resolved counts the refs that named a row this
// project owns; Marked counts the stamps THIS call wrote, which is 0 in a dry run
// and can be lower than Resolved for any of the three per-row no-ops.
//
// The no-ops are counted separately rather than as one "not marked" because they
// are three different situations with three different remedies, and a report that
// summed them would leave an operator unable to tell a row they may retry from one
// they may not: AlreadyResolved is a memory the previous pass already buried (the
// issue's "reported as a no-op"), Pinned is a row an explicit instruction keeps
// visible, and ExemptCategory is a standing-knowledge category resolve never
// stamps. The counts are read off the same rows Memories holds, so a count and the
// list cannot disagree.
type MarkResult struct {
	Resolved        int
	Marked          int
	AlreadyResolved int
	Pinned          int
	ExemptCategory  int
	// Declined counts rows the store's write-time guard refused, which is only
	// ever non-zero under --apply. It is counted and not left to the difference
	// between Resolved and the other four, because that difference is also
	// where a row this code failed to think of would land.
	Declined int
	Memories []MarkedMemory
}

// Mark stamps resolved_at on the memories the request names and returns them
// resolved to full ids. A dry run (Apply=false) resolves, loads and reports
// without writing.
//
// Nothing here asks a model, so no harness is involved and nothing is billed: a
// machine that cannot spawn one at all can still mark a memory, which is the same
// property that makes internal/supersede.Withdraw usable from a stop hook.
//
// The whole request is settled before anything is written, and a refusal writes
// nothing. A row that resolves to `_global` is a refusal and not a mark, because
// the ref query reaches it for the sake of the supersede endpoints' sake and a
// project's mark is about rows the project owns.
//
// The write is all-or-nothing, and that is a property rather than a reporting
// choice: MarkResolved runs the stamp, the KEEP-cache clear and the `resolve`
// history row in ONE transaction, so an error rolls the whole request back. This
// is the difference from a repair that stamps rows one transaction at a time
// (internal/supersede.Withdraw), where a partial outcome has to be reported as
// itself. Here there is no partial outcome to report, so the result comes back
// with the error and every row marked as failed.
func Mark(ctx context.Context, store MarkStore, req MarkRequest, logger *slog.Logger) (MarkResult, error) {
	var res MarkResult
	if req.ProjectID == "" {
		return res, fmt.Errorf("resolve --mark: a project is required, so no memory can be named from it")
	}
	if len(req.Refs) == 0 {
		return res, fmt.Errorf("no memory to mark: name one as --mark <id> (a full id, or 8 or more characters of one)")
	}

	// Settle every ref first, collecting ALL the problems: an operator
	// correcting five ids learns from all five that they are wrong at once, and
	// none of the five may be written while four of them are known to be wrong.
	var problems []string
	rows := make([]MarkedMemory, 0, len(req.Refs))
	seen := make(map[string]bool, len(req.Refs))
	for _, ref := range req.Refs {
		row, err := resolveMarkRef(ctx, store, req.ProjectID, ref)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if seen[row.ID] {
			// A repeated ref names one row, so it is marked once. Silently,
			// because the operator's intent is unambiguous either way, and a
			// second history row for one stamp would be a record of a write that
			// did not happen.
			continue
		}
		seen[row.ID] = true
		rows = append(rows, row)
	}
	if len(problems) > 0 {
		return MarkResult{}, fmt.Errorf("nothing marked: %s", strings.Join(problems, "; "))
	}
	res.Resolved = len(rows)

	// The rows' own state, read once for the whole request and only after the
	// refs themselves are known to be sound. The report quotes each memory it
	// marked or declined, and it also decides the per-row no-ops from what the
	// store holds rather than from a second guess at the guard.
	if err := attachMarkState(ctx, store, req.ProjectID, rows); err != nil {
		return MarkResult{}, err
	}
	res.AlreadyResolved = countMarked(rows, func(m *MarkedMemory) bool { return m.AlreadyResolved })
	res.Pinned = countMarked(rows, func(m *MarkedMemory) bool { return m.Pinned })
	res.ExemptCategory = countMarked(rows, func(m *MarkedMemory) bool { return m.ExemptCategory })

	// The rows and their counts are what a dry run reports, so they are
	// attached before the write rather than only on the success path: a preview
	// that resolved a ref and then printed no row for it is not a preview.
	res.Memories = rows
	if !req.Apply {
		return res, nil
	}

	// The write. Every resolved row is passed, not only the ones this call
	// believes it should stamp: the store owns the eligibility guard, and
	// pre-filtering it here would be the second copy of a decision that has to
	// agree with the one in the statement. The ids it returns are what it wrote.
	stampable := make([]string, 0, len(rows))
	for _, m := range rows {
		stampable = append(stampable, m.ID)
	}
	stamped, err := store.MarkResolved(ctx, req.ProjectID, stampable, req.Provenance)
	if err != nil {
		// Nothing landed, and the result says so per row rather than implying a
		// partial mark. MarkResolved is ONE transaction over every batch, so its
		// error is a rollback: a stamp it committed would not be a state a later
		// statement's failure could leave behind. Reporting some rows as marked
		// here would be claiming a write that did not happen, which is the same
		// error the already-resolved no-op exists to avoid — and the reason this
		// path has no NotAttempted rows to report either.
		for i := range rows {
			rows[i].MarkFailed = true
		}
		res.Memories = rows
		return res, fmt.Errorf("mark resolved in %s: %w", req.ProjectID, err)
	}
	written := make(map[string]bool, len(stamped))
	for _, id := range stamped {
		written[id] = true
	}
	for i := range rows {
		switch {
		case written[rows[i].ID]:
			rows[i].Marked = true
		case rows[i].AlreadyResolved || rows[i].Pinned || rows[i].ExemptCategory:
			// Already named from the read, with the reason it applies.
		default:
			// Eligible on the read, absent from the write: the store's write-time
			// guard declined it and nothing failed. Said explicitly, because the
			// row is otherwise indistinguishable from one that was written.
			rows[i].Declined = true
		}
	}
	res.Marked = len(stamped)
	res.Declined = countMarked(rows, func(m *MarkedMemory) bool { return m.Declined })
	if logger != nil {
		logger.Info("resolve marked named memories",
			"project", req.ProjectID, "resolved", res.Resolved, "marked", res.Marked,
			"already_resolved", res.AlreadyResolved, "pinned", res.Pinned, "exempt", res.ExemptCategory,
			"declined", res.Declined)
	}
	return res, nil
}

// resolveMarkRef turns one ref into the row it names, or explains why there is
// none. It reads only: every problem a request can have is found here, before
// Mark writes anything.
//
// which is "id" rather than supersede's "source"/"target" because this request
// has one kind of ref and several of them, and the refusal has to name which
// spelling on the line was wrong.
func resolveMarkRef(ctx context.Context, store MarkStore, projectID, ref string) (MarkedMemory, error) {
	var row MarkedMemory
	id, err := memref.Resolve(ctx, store, projectID, "id", ref)
	if err != nil {
		return row, err
	}
	row.ID, row.Ref = id, ref
	return row, nil
}

// attachMarkState fills each row's text, category and no-op markers in place
// from one read of the whole request, and refuses the request when the read
// turns up a row this project does not own.
//
// It reads only, so it can run before any write — which is what lets a `_global`
// row be a REFUSAL of the whole request rather than a row MarkResolved then
// declines: `MemoryIDsByIDPrefix` reaches `_global` because a promotion moves a
// row while keeping the links that point at it, and both ends of an edge have to
// stay nameable from the project that owns it. That reason does not apply to a
// mark, and a row every project shares is not one a project asked to bury. The
// guard lives here, on the read that can see the row's own project_id, rather
// than being re-argued at the write — the write binds the project too, and a
// caller that reached this with a row in another project has been told so before
// anything was written.
//
// A ref that resolved and then cannot be loaded is the same kind of refusal: the
// id came from the store, so a row that is not there was deleted between the two
// reads, and describing that as a memory with no text would be a report about a
// row this call never saw.
func attachMarkState(ctx context.Context, store MarkStore, projectID string, rows []MarkedMemory) error {
	if len(rows) == 0 {
		return nil
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	mems, err := store.GetByIDs(ctx, ids)
	if err != nil {
		return fmt.Errorf("load the memories to mark: %w", err)
	}
	byID := make(map[string]memory.Memory, len(mems))
	for _, m := range mems {
		byID[m.ID] = m
	}
	var problems []string
	for i := range rows {
		m, ok := byID[rows[i].ID]
		if !ok {
			problems = append(problems, fmt.Sprintf("the id ref %q named a memory that is not there to mark — it was deleted between resolving the ref and loading it; re-run to see what %s holds now",
				rows[i].Ref, projectID))
			continue
		}
		if m.ProjectID != projectID {
			problems = append(problems, fmt.Sprintf("the id ref %q is memory %s, which belongs to %s and not to %s: only a memory in the project you named can be marked there",
				rows[i].Ref, memref.Short(m.ID), m.ProjectID, projectID))
			continue
		}
		rows[i].Content, rows[i].Category = m.Content, m.Category
		rows[i].AlreadyResolved = m.ResolvedAt != nil && *m.ResolvedAt != ""
		rows[i].Pinned = m.Pinned
		rows[i].ExemptCategory = m.Category == "convention" || m.Category == "preference"
	}
	if len(problems) > 0 {
		return fmt.Errorf("nothing marked: %s", strings.Join(problems, "; "))
	}
	return nil
}

// countMarked counts the rows a predicate selects, which is how the result's
// three per-row no-op counts are read off the same rows the report prints. One
// helper because three copies of the same loop would be three places for the
// count and the list to disagree.
func countMarked(rows []MarkedMemory, pred func(*MarkedMemory) bool) int {
	n := 0
	for i := range rows {
		if pred(&rows[i]) {
			n++
		}
	}
	return n
}
