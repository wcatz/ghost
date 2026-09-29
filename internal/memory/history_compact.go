package memory

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Issue #730. Before #727 every applied reflection appended a byte-identical
// `reflect` version for each memory it kept and set that memory's updated_at to
// the run's own time. This removes the ones already stored, and it is two repairs
// because the damage is two things:
//
//   - memory_history is mostly rows that changed nothing, which crowds real
//     events toward the per-memory and per-store retention caps, and
//   - memories.updated_at holds the time of the last reflect rather than the
//     time of the last real change, so supersede orientation and
//     --skip-unchanged's fingerprint stay wrong until each row is next edited.
//
// The first is a DELETE and the second is a WRITE, so they are two options and
// not one: a caller who wants the history back under its caps does not have to
// accept a stamp moving.
//
// #727 stopped the FLOOD, not every row of this shape, and the difference is the
// whole design of this file. A verbatim re-emission — content, category,
// importance, tags and scope all unchanged — is now a no-op, and that was the
// case a lifecycle pass hit on nearly every memory it kept. A consolidation
// MERGE or REWRITE that lands on a stored row's own text is not verbatim: it
// carries the union of its sources' tags, and ReplaceNonManual's reusePreservesAge
// branch writes them and files a version. That version records content, category,
// importance, resolved_at and source — none of which moved, because this table has
// no column for tags. So one writer can still produce a byte-identical `reflect`
// version, deliberately, and no state-column predicate can tell it from the damage.
//
// Three rules follow, and each is a separate question with a separate answer.
// WHICH PHASE: only `reflect` is removable, because that is the only phase a
// no-op run ever wrote — `UpdateMemory` files an `update` version for a retag with
// no no-op guard at all, and a version recording a deliberate retag is the record
// of a deliberate change. WHEN: only rows recorded before #727 shipped, because a
// row this build wrote is a current writer's business and not this repair's (see
// reflectNoOpCutoff). WHICH ROW: only one that records what the row before it
// records, is not the memory's newest version, and names no other memory.

// historyVersionColumns are the columns a history VERSION records: the state the
// memory held once that write landed, which is what makes a row a version and not
// a diff (#578). The compaction's equality predicate compares these and nothing
// else, and they are a list rather than a hand-written conjunction because
// TestCompactHistoryColumnPartitionNamesEveryColumn holds the partition complete: a
// column added to the table and to none of the three lists is compared by none of
// them, which is exactly how a state change stops being one.
var historyVersionColumns = []string{"content", "category", "importance", "resolved_at", "source"}

// historyEventColumns are the columns describing the EVENT rather than the state:
// which memory it is about, which project paid for it, which write path performed
// it, who did, and the event's other end — related_id, the id that replaced a
// deleted row or whose edge claims it, and merged_content, the wording a FoldOnly
// fold dropped. None is part of "the state the memory held", which is why the
// predicate does not compare them and why the two naming ANOTHER memory are
// checked for emptiness instead: such a row is a thread a reader follows from one
// memory's history into its successor's (#648), and it records a claim, not a
// state.
var historyEventColumns = []string{
	"memory_id", "project_id", "phase", "agent", "session_id", "related_id", "merged_content",
}

// historyRowColumns are the row's own identity and the time it was recorded.
// Neither is comparable between two versions of one memory — every row has a
// distinct id by construction, and recorded_at is precisely what DIFFERS between
// two otherwise-identical rows — so neither takes part in the predicate. They are
// listed so the partition of the table's columns is complete.
var historyRowColumns = []string{"id", "recorded_at"}

// historyCompactBatchSize is how many rows one compaction batch touches: the
// delete's LIMIT and the updated_at pass's page. Bounded for the same reason
// pruneHistoryTx bounds its statements, and with more force — prune runs on the
// write path of every save, while this runs once, deliberately, by an operator
// holding the store still. One transaction over a whole store's history would
// hold the write lock for the length of it, and OpenDB pins MaxOpenConns(1), so
// every other writer in the machine queues behind it.
//
// A var only so the batching test can lower it and drive more than one batch
// against a fixture small enough to read; nothing in production assigns to it.
var historyCompactBatchSize = 500

// compactablePhases are the phases a redundant version may be removed from, and
// it is an ALLOWLIST of ONE phase, which is a deliberate narrowing rather than an
// oversight. Every other phase records a change somebody made on purpose, and the
// state columns cannot always see it — so the phase, not the state, is what
// separates #730's damage from a legitimate event.
//
// `reflect` is the phase the damage is. A pre-#727 run appended one per kept
// memory; #727 stopped the verbatim re-emission that produced nearly all of them;
// and a merge that lands on a stored row's own text still appends one, which the
// cutoff below handles rather than this list.
//
// `save` and `baseline` record the first statement of what a memory said, and a
// redundant one of those is rare enough that leaving it costs nothing.
// `update` is the case that decides the list: UpdateMemory files its version
// UNCONDITIONALLY, with no no-op guard the way reuseChangesNothing gives the
// reflect path one, so a tags-only `ghost_memory_update` appends a row that
// restates content, category, importance, resolved_at and source byte for byte and
// moves the stamp on purpose. Deleting that row would erase the record of a
// deliberate retag, and restoring the stamp would undo its bump — the exact failure
// the phase allowlist exists to make impossible
// (TestCompactHistoryLeavesATagsOnlyEditAndItsStamp).
//
// The rest are excluded because each says something the state does not: `delete` is
// the tombstone and the only record a deleted memory left;
// `supersede`/`unsupersede` record a claim about the memory's standing, and only
// the sequence says which claim is live; `resolve`/`unresolve` say who decided;
// `merge` records the fold; `import` and `restore` say a row arrived from outside.
//
// It stays a list and an IN clause so a phase added to the schema later is not
// deletable until somebody classifies it here.
func compactablePhases() []string {
	return []string{phaseReflect}
}

// reflectNoOpCutoff is the instant #727 reached main (d60aa301, 2026-09-28
// 17:14:07 UTC), and it is the DEFAULT bound on the repair: a version recorded
// before it may have been written by a build that appended a restatement for
// every kept memory, and one recorded at or after it was written by a build that
// has the fix — or, if it looks redundant, by the merge reuse this file cannot
// distinguish from the damage. Leaving those alone is the safe direction: an
// undamaged store keeps one version per deliberate change, while a row left behind
// costs a slot under a cap that has room, and a re-run after an upgrade reaches it.
//
// The comparison is a STRING comparison, which is exact for the layout every
// writer produces (`datetime('now')`, i.e. StoredStampLayout) and for the whole-day
// form SQLite's date() leaves behind — a whole-day value sorts before any stamp
// with a time on that day, which is the right answer for a row that old. A
// recorded_at no layout can read is therefore NOT skipped on account of being
// unreadable, because the comparison is a string comparison and an unreadable one
// can land on either side: the epoch form a Unix writer left behind sorts BELOW
// the cutoff and would be treated as damage, and any other malformed value sorts
// wherever its characters put it. So the gate does not lean on the comparison for
// this. A row the run removes is one the delete's own predicate named, and
// restorableStamp separately reports a stamp it cannot read rather than guessing
// at one — an unreadable stamp is not evidence that a row is damage, and the run
// discloses what it cannot interpret.
const reflectNoOpCutoff = "2026-09-28 17:14:07"

// ResolveCompactCutoff is the cutoff a run will use, normalized to the layout the
// comparison above is exact for. Exported because the report has to NAME the bound
// that produced its numbers: a count with an unstated bound is not a number an
// operator can reason about, and a bound they cannot see is a bound they cannot
// widen.
//
// An empty before is the default (reflectNoOpCutoff). RFC 3339, the whole-day form
// and the store's own layout are all accepted, because an operator who wants a wider
// bound knows the DATE the fix shipped and not the second it did; a whole-day value
// means the start of that day in UTC, which is EARLIER than the default and so
// restores less — the conservative direction, and the one the date spelling implies.
// The store's own layout is accepted so the function is idempotent, which lets a
// caller resolve a flag once to report it and hand the resolved value straight back.
func ResolveCompactCutoff(before string) (string, error) {
	if before == "" {
		return reflectNoOpCutoff, nil
	}
	for _, layout := range []string{DateStampLayout, StoredStampLayout, time.RFC3339} {
		if at, err := time.Parse(layout, before); err == nil {
			return at.UTC().Format(StoredStampLayout), nil
		}
	}
	return "", fmt.Errorf("--before %q is neither a date (%s) nor an RFC 3339 instant",
		before, DateStampLayout)
}

// WidenedCompactCutoff reports whether a RESOLVED cutoff reaches rows that a
// current build wrote, which is what makes it wider than the default and what the
// command warns about before it is told to act on one.
//
// Strictly after the default, not at-or-after. The default IS reflectNoOpCutoff,
// so an at-or-after test would fire on every invocation of a command whose zero
// configuration is the safe one, and a warning that is always on is a warning
// nobody reads. What an operator has to be told about is a bound that goes past
// the fix on purpose.
//
// Exported rather than left to the caller to compare, because both halves of the
// comparison are the store's own: the instant and the layout. A caller doing it
// with string arithmetic gets a warning that disagrees with the bound the store
// actually used the moment either of them changes.
func WidenedCompactCutoff(cutoff string) bool {
	return cutoff > reflectNoOpCutoff
}

// placeholders is n question marks, comma separated, for an IN list.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// historyEqualPredecessorSQL is THE comparison, and the reason it is a function
// rather than a conjunction spelled into two statements is that it has three
// callers that must agree: the delete, the dry run's count, and the updated_at
// pass's "did this change anything" — and the last of those uses it NEGATED.
//
// `outer` names the row under test. The test is an EXISTS against the row before
// it of the same memory, in rowid order and never recorded_at order: recorded_at
// is second-precision, so every write one reflection makes in a pass shares a
// timestamp and ordering by it would make the sequence a coin toss (the same
// reason pruneHistoryTx ranks by rowid). A memory's FIRST version has no
// predecessor, the max is NULL, nothing matches, and the row reads as a change —
// which is the answer it has to give, since the first version is never removable.
//
// `IS` and not `=`, and the reason is load-bearing: every state column is
// nullable (a delete row is written from the last live state, and an older build
// could leave gaps), and SQL's `=` answers NULL for NULL, so two rows both holding
// no resolved_at would compare UNEQUAL and every such pair would read as a change.
// That is the opposite of the damage this repairs.
func historyEqualPredecessorSQL(outer string) string {
	eq := make([]string, 0, len(historyVersionColumns))
	for _, c := range historyVersionColumns {
		eq = append(eq, "p."+c+" IS "+outer+"."+c)
	}
	return "EXISTS (SELECT 1 FROM memory_history p WHERE p.rowid = (" +
		"SELECT max(q.rowid) FROM memory_history q WHERE q.memory_id = " + outer + ".memory_id AND q.rowid < " + outer + ".rowid" +
		") AND " + strings.Join(eq, " AND ") + ")"
}

// historyNewestVersionSQL is a memory's newest version row, by rowid.
//
// Deliberately NOT narrowed to one project, unlike the rest of the scan: a memory
// that was promoted to _global or merged into another project has its earlier
// versions under the old one, so a project-narrowed "newest" would name a row in
// the middle of the memory's past and the guard below would spare the wrong one.
// The lookup is an index seek on idx_history_memory's leading column.
func historyNewestVersionSQL(memoryID string) string {
	return "(SELECT max(n.rowid) FROM memory_history n WHERE n.memory_id = " + memoryID + ")"
}

// historyRemovableRowSQL is the full predicate, and it is what #730 deletes by:
// a row that recorded exactly what the row before it of the same memory recorded,
// is not that memory's newest version, was recorded before the cutoff, names no
// other memory, and is of a phase whose only content is the state it recorded —
// and which does not belong to a memory that has since been deleted.
//
// The newest-version guard is the same rule pruneHistoryTx already applies to its
// per-memory trim: "Ranking by rowid DESC also means the predicate can never take
// a memory's newest row: it is rank 1." A memory's newest version is the statement
// of what it says now, so nothing removes it — not the growth policy, and not this.
//
// The cutoff is what makes the repair safe on a store that keeps running the
// lifecycle, and the newest-version guard is NOT. A current build still writes a
// version that restates the state byte for byte, on purpose: a consolidation merge
// whose survivor is one of its own sources carries the union of the sources' tags,
// ReplaceNonManual's reusePreservesAge branch writes them, and the version it
// appends records content, category, importance, resolved_at and source — none of
// which moved, because this table has no column for tags. #727 was right that the
// row is a real change. What no state-column predicate can do is recognise it
// (TestAReflectReuseChangesTagsWithoutChangingAnyRecordedColumn runs the path
// that writes one), so the bound is a time rather than a column: a row this build
// wrote is a current writer's business, and a row written before the fix shipped is
// this repair's.
//
// The guard still earns its place, and the case is the one the guard is the only
// answer for: a store whose clock is behind — a restored backup, a copied database,
// a machine whose clock was wrong when the rows were written — can hold a
// byte-identical reflect version older than the cutoff, and the guard is what keeps
// the compaction from taking a memory's own latest statement about itself. One
// such row per memory, never the last, which is also why a store running the
// lifecycle does not re-grow what was pruned: the rows it adds are all newer than
// any cutoff, so they are outside the repair entirely.
//
// The tombstone guard is the second retention rule, and it is here rather than in
// historyRemovableLikeSQL because it answers the same question the newest-version
// guard answers — is this row something the repair removes? — and the answer is no
// for a deleted memory. The reason is as_of rather than this file: with no
// `memories` row, asOfCreatedAt falls back to the ANSWERING version's recorded_at
// for the age a historical listing measures, so removing a version from a deleted
// memory changes what a past read computes. A live memory is unaffected — its
// created_at answers, and its as_of metadata moves to an earlier EQUIVALENT
// version, which docs/invariants.md and docs/cli.md both say in as many words. A
// memory's history is frozen the moment it is deleted, so nothing will ever write
// there again: the flood this command cleans up stays where it is, at no cost.
//
// It is stable under the deletion, which is what the fixed-point argument on
// CompactHistory needs: it reads the memory's NEWEST version, and no removable row
// is ever the newest, so removing rows cannot promote a different one into being
// newest and cannot un-retire a memory.
func historyRemovableRowSQL(outer string) string {
	return historyRemovableLikeSQL(outer) +
		" AND " + outer + ".rowid <> " + historyNewestVersionSQL(outer+".memory_id") +
		" AND NOT " + tombstonedMemorySQL(outer+".memory_id")
}

// stampMovingPhases are the phases whose writer moves a LIVE memory's updated_at in
// the same STATEMENT that files the version. Three, and every one of their writers
// is enumerated below, because membership here is a claim about code and a claim
// about code is the kind that goes stale without anything failing.
//
// The rule is that EVERY writer of a listed phase does it, not that one of them
// does. A phase with a single writer that moves the column and a second that does
// not would make the version's own phase an unreliable witness: the anchor would
// land on a row whose writer left the stamp alone, and the repair would set the
// stamp to a time the store never held. So the audit is per phase and per writer:
//
//   - phaseSave — five writers, all INSERTs that omit updated_at and therefore take
//     the column default `datetime('now')` (schema.go): Store.insertMemory behind
//     Create and CreateFromCorpus, seedMemoryTx, both of UpsertWithOptions' insert
//     branches, and RecordDecision's companion memory. The version is appended in
//     the transaction that ran the INSERT.
//   - phaseUpdate — one writer, UpdateMemoryWithOptions, whose UPDATE ends
//     `tags = ?, updated_at = datetime('now')` (store.go), with the version
//     appended in the same transaction.
//   - phaseReflect — three writers, all in ReplaceNonManual: the reusePreservesAge
//     branch and the default reuse branch, each an UPDATE ending
//     `updated_at = datetime('now')`, and the fresh INSERT, which omits the column
//     and takes the default. One append for all three at the end, in the same
//     transaction. The reuseChangesNothing branch (#727) files no version at all,
//     so it is not a writer of this phase.
//
// Every OTHER phase files a version without touching a live memory's stamp, and
// five of the nine CHANGE a column this table records, which is what makes the list
// necessary rather than merely tidy:
//
//   - setResolvedStampTx (SetResolved and MarkResolved) and ClearResolved write
//     `resolved_at` and say in as many words that updated_at is deliberately left
//     alone.
//   - Upsert's fold writes `importance = MIN(1.0, importance + ?)` and files
//     phaseMerge with the wording it dropped.
//   - CreateLink writes no memories row at all, and files supersede/unsupersede.
//   - RestoreSnapshot and ImportMemory write their rows from a snapshot and an
//     artifact; their versions are the memory's first.
//   - Delete files the tombstone, and removes the row.
//
// Getting this list wrong in the permissive direction is a silent permanent loss of
// the repair: a state-identical supersede row treated as an anchor closes the gate
// for that memory for good, and a report that then says "0 updated_at restored" is
// the same wrong answer as never having run. Getting it wrong in the OTHER
// direction invents a stamp, which is worse and is what the state-change reading
// this replaced did — 49 of 288 restored stamps on one real store were set to an
// instant the store had never held. TestCompactHistoryStillRepairsAMemoryThatWasSupersededAfterwards
// runs every phase outside this set and pins that none of them closes the gate;
// TestCompactHistoryFixUpdatedAtAnchorsOnAStampMoveNotOnAStateChange drives the two
// writers that change a recorded column through their real API and pins that
// neither becomes the answer.
var stampMovingPhases = []string{phaseSave, phaseUpdate, phaseReflect}

// historyRemovableLikeSQL is historyRemovableRowSQL WITHOUT the two retention
// guards, and the split between them is deliberate: the guards are RETENTION rules
// and the rest is a DAMAGE rule, and --fix-updated-at needs the damage rule on its
// own.
//
// A memory's newest version is spared from removal because it is the statement of
// what the memory says now. That says nothing about whether the row is a record of
// a deliberate change, which is the other question. A newest version that is a
// pre-#727 byte-identical reflect is the damage the repair exists for, and it is
// evidence the stamp was moved; a newest version that is a deliberate retag is a
// deliberate change, and it is an anchor. The guard cannot tell those two apart —
// it declines both — so the anchor has to be found without it, and spelling the
// two predicates as one function plus two conjuncts is what keeps the anchor from
// silently acquiring a guard on the next change.
//
// Reading it the other way — the newest row is always an anchor — is not a
// stricter version of the same rule, it is a dead one: a removable row is by
// definition not the newest, so the anchor would always sit above every removable
// row and `removableLast > target` would never hold. --fix-updated-at would report
// 0 on a store holding nothing but the damage.
func historyRemovableLikeSQL(outer string) string {
	return historyEqualPredecessorSQL(outer) +
		" AND " + outer + ".phase IN (" + placeholders(len(compactablePhases())) + ")" +
		" AND " + outer + ".recorded_at < ?" +
		" AND " + outer + ".related_id IS NULL AND " + outer + ".merged_content IS NULL"
}

// tombstonedMemorySQL is true for a memory whose NEWEST version is a `delete`
// tombstone and false for every other memory, and the COALESCE is what keeps the
// false case a fact rather than an unknown: a memory with no recorded version at all
// answers NULL, and `NOT NULL` is NULL, so the guard would exclude rows by the
// accident of there being nothing to compare rather than by the fact that the memory
// is retired. A memory with no version is outside this predicate for a stronger
// reason, and the guard should say so for its own reason.
//
// It reads the newest version by rowid rather than by recorded_at, like every other
// newest-version lookup here: recorded_at is second-precision, so a pass that writes
// several versions in one transaction leaves ordering by it a coin toss. The lookup
// is the same sub-select historyNewestVersionSQL names, so "newest" means one thing
// across the file.
func tombstonedMemorySQL(memoryIDExpr string) string {
	return "COALESCE((SELECT t.phase FROM memory_history t WHERE t.rowid = " +
		historyNewestVersionSQL(memoryIDExpr) + "), '') = '" + phaseDelete + "'"
}

// A compaction statement and its arguments are built by ONE function each, because
// SQLite binds positionally and a statement whose placeholder order lives apart
// from its argument list is one that can be run with the right values in the wrong
// slots — which SQLite reports as a type error, or worse accepts silently.

// compactDeleteStmt removes at most historyCompactBatchSize removable versions of
// one project's memories, oldest rowid first. The batch is the DELETE's own LIMIT
// over an ordered sub-select of rowids, so the next batch starts where the last
// stopped with no cursor argument that could disagree with it.
func compactDeleteStmt(projectID, cutoff string) (string, []any) {
	sql := `DELETE FROM memory_history WHERE rowid IN (
	    SELECT h.rowid FROM memory_history h
	    WHERE h.project_id = ? AND ` + historyRemovableRowSQL("h") + `
	    ORDER BY h.rowid
	    LIMIT ?)`
	return sql, append(compactPredicateArgs(projectID, cutoff), historyCompactBatchSize)
}

// compactCountStmt is the same predicate, counted.
func compactCountStmt(projectID, cutoff string) (string, []any) {
	sql := `SELECT count(*) FROM memory_history h
	    WHERE h.project_id = ? AND ` + historyRemovableRowSQL("h")
	return sql, compactPredicateArgs(projectID, cutoff)
}

// compactCandidatesStmt is the updated_at pass's read. Per LIVE memory of one
// project it returns the recorded_at of the ANCHOR (c.target) — the newest version
// this repair will not remove — the updated_at the row holds now, and the evidence
// the decision in restorableStamp needs: the newest removable rowid for that memory
// and how many rows this project held for it.
//
// The anchor used to be the newest version that CHANGED its state, found by
// negating the state comparison, and that is what made the gate
// `removableLast > target` answer a question nobody asked. Negating the state
// comparison cannot see a row that records the same state, whatever else is true of
// it, and a deliberate writer is exactly that: a tags-only UpdateMemory and
// ReplaceNonManual's reusePreservesAge branch both bump updated_at on purpose while
// moving no column this table has. So the anchor skipped over them, and any
// removable no-op reflect row beneath them read as proof that a reflection had moved
// a stamp it had not.
// TestCompactHistoryDoesNotRewindADeliberateBumpUnderARemovableRow runs the two
// shapes and pins the rule this predicate now states: a row this repair will not
// remove is an anchor.
//
// The evidence is the same predicate the delete used, spelled with the same three
// functions, and it is read for EVERY memory rather than only the ones this run
// removed from — the decision is made in Go and both modes make it, so the dry
// run's count and the apply's write cannot disagree, and a memory whose rows were
// removed by an earlier batch of the same pass is still judged correctly.
//
// The cutoff reaches the evidence for the same reason it reaches the delete. If it
// did not, a post-#727 byte-identical reflect version — a deliberate retag the
// state columns cannot see — would count as proof that a reflection moved the
// stamp, and the pass would rewind updated_at over the change the store just made
// and recorded. The evidence and the row that would be deleted are the same
// statement, so the gate can never read a row this run would not remove.
//
// The join to memories is what makes this the updated_at repair rather than a
// second history pass: a deleted memory has no row to stamp, and its tombstone is
// evidence about a memory that is gone. The cursor pages the scan and is stable
// across batches, because a restore only ever moves a stamp backward and never
// changes which project a memory belongs to — so a memory cannot move between
// batches while the pass runs. A cursor rather than OFFSET, for the reason
// pruneHistoryTx uses a rowid window: an OFFSET over a result set the previous
// batch's writes could resize would skip rows. The cursor appears twice because
// the statement accepts an empty one for the first page rather than carrying a
// separate first-page spelling: `? = ” OR c.memory_id > ?` is a branch SQLite folds
// away, and two statements differing only in their WHERE clause are two things to
// keep in step.
func compactCandidatesStmt(projectID, cursor, cutoff string) (string, []any) {
	// The evidence the decision needs, spelled with the delete's own predicate and
	// over the same project, so "was there a version NEWER than the last real
	// change that said nothing" is answered by the delete's rule rather than by a
	// second one. It is a correlated sub-select rather than a join because it has to
	// name the memory the GROUP BY is over, and a sub-select in a FROM clause cannot
	// see a sibling FROM item.
	evidence := `COALESCE((SELECT max(r.rowid) FROM memory_history r
	        WHERE r.memory_id = h.memory_id AND r.project_id = ? AND ` + historyRemovableRowSQL("r") + `), 0)`
	// The anchor: the newest version this repair will NOT remove whose WRITER moved
	// a live memory's updated_at in the same statement that filed it.
	//
	// Both halves are needed and neither is the other. The first is the blocker
	// (TestCompactHistoryDoesNotRewindADeliberateBumpUnderARemovableRow): a row
	// that records the state of its predecessor is invisible to
	// `NOT historyEqualPredecessorSQL` whatever else is true of it, and a
	// deliberate writer is exactly that. A tags-only UpdateMemory files one, the
	// reuse branch of ReplaceNonManual files one, and under the state-change
	// reading the anchor skipped over both, so a pre-#727 no-op reflect row
	// beneath a retag read as proof that a reflection had moved a stamp it had not.
	//
	// The second is what replaced the state-change half this used to carry, and it
	// replaced it because a change of state is not a change of stamp. Five of the
	// nine phases outside stampMovingPhases change a column this table records —
	// SetResolved writes resolved_at, Upsert's fold writes importance, a restore
	// writes the snapshot's text — and none of them touches updated_at, so under
	// the state-change reading any of them became the answer and the repair set a
	// memory's stamp to an instant the store never held on either column. 49 of 288
	// restored stamps on one real store, and one memory's first version was an
	// unresolve (a pre-v17 memory, which has no earlier version to compare against
	// and so passed the comparison trivially) with seven no-op reflects above it
	// whose stamp was "restored" to the moment that row was written.
	//
	// There is nothing else to check it against, and that is the reason for the
	// phase list rather than a cleverer predicate: `memory_history` records no
	// updated_at at all, so a version row cannot be compared with the stamp it is
	// supposed to account for. The only evidence a stamp write happened is the
	// identity of the writer, and the writer is what the phase names.
	//
	// A memory whose history holds no such row has no anchor, and COALESCE turns
	// that into a zero the caller can report. The stamp is left exactly as it is,
	// and it is counted under a name of its own rather than as an unreadable stamp:
	// an unreadable stamp is a value that exists and cannot be parsed, and this is
	// a value whose AUTHOR does not exist anywhere in the table. Merging them would
	// send an operator looking for a timestamp shape that is not wrong. Inventing a
	// target for it would be the failure the whole file exists to undo, one level
	// down. The LEFT JOIN and the COALESCE around recorded_at are there so such a
	// memory REACHES the caller instead of being dropped by an inner join: a row
	// silently missing from a repair is a memory whose stamp reads as a reflect
	// run's time with nothing to say so.
	//
	// Project-scoped, like the delete: a version filed under another project is not
	// one this run will remove and not one whose writer moved this memory's stamp,
	// so it is an anchor.
	//
	// The parens around historyRemovableLikeSQL are load-bearing, not decoration,
	// and dropping one is a silent total failure rather than a syntax error: the
	// helper opens with an EXISTS, so `NOT EXISTS (…) AND a.phase IN (…)` and
	// `NOT EXISTS (…) a.phase IN (…)` are not the same query, and only one of them
	// is the one anyone is reading. TestCompactHistoryFixUpdatedAtRestoresTheLastStampWrite
	// catches it: every memory in it has both kinds of row.
	anchor := `(SELECT max(a.rowid) FROM memory_history a
	        WHERE a.memory_id = h.memory_id AND a.project_id = ?
	          AND NOT (` + historyRemovableLikeSQL("a") + `)
	          AND a.phase IN (` + placeholders(len(stampMovingPhases)) + `))`
	sql := `SELECT c.memory_id, c.target, COALESCE(t.recorded_at, ''), m.updated_at, c.removable_last
	    FROM (
	        SELECT h.memory_id AS memory_id, COALESCE(` + anchor + `, 0) AS target, ` + evidence + ` AS removable_last
	        FROM memory_history h
	        WHERE h.project_id = ? AND NOT ` + historyEqualPredecessorSQL("h") + `
	        GROUP BY h.memory_id
	    ) c
	    LEFT JOIN memory_history t ON t.rowid = c.target
	    JOIN memories m ON m.id = c.memory_id AND m.project_id = ?
	    WHERE (? = '' OR c.memory_id > ?)
	    ORDER BY c.memory_id
	    LIMIT ?`
	// Textual order: the anchor sub-select's project, phase list and cutoff, then
	// the evidence sub-select's project, phase list and cutoff, then the
	// changed-state scan's project, then the live row's project, then the cursor and
	// the page.
	args := compactAnchorArgs(projectID, cutoff)
	args = append(args, compactPredicateArgs(projectID, cutoff)...)
	args = append(args, projectID, projectID, cursor, cursor, historyCompactBatchSize)
	return sql, args
}

// compactAnchorArgs binds the anchor sub-select: the project, then the
// removable-like predicate's phase list and cutoff, then stampMovingPhases in the
// order the statement names them. A function like compactPredicateArgs, for the
// reason those are functions: SQLite binds positionally, and an argument list that
// lives apart from the statement is one that can be shifted without anything
// noticing until a predicate starts answering a different question.
func compactAnchorArgs(projectID, cutoff string) []any {
	args := make([]any, 0, len(compactablePhases())+len(stampMovingPhases)+2)
	args = append(args, projectID)
	for _, p := range compactablePhases() {
		args = append(args, p)
	}
	args = append(args, cutoff)
	for _, p := range stampMovingPhases {
		args = append(args, p)
	}
	return args
}

// historyCompactRestoreSQL writes one memory's restored stamp. The project is bound
// in the statement as well as in the read that selected the candidate, so a memory
// another process moved between the two is not stamped by a run scoped to a project
// it no longer belongs to.
const historyCompactRestoreSQL = `UPDATE memories SET updated_at = ? WHERE id = ? AND project_id = ?`

// compactPredicateArgs binds the project predicate, then compactablePhases, then
// the cutoff, in the order historyRemovableRowSQL names them. The three statements
// that read the predicate take the cutoff as a binding rather than naming the
// constant, because it is an argument of the run (ResolveCompactCutoff) and a
// constant written into the SQL would be a second, unreported bound.
func compactPredicateArgs(projectID, cutoff string) []any {
	args := make([]any, 0, len(compactablePhases())+2)
	args = append(args, projectID)
	for _, p := range compactablePhases() {
		args = append(args, p)
	}
	return append(args, cutoff)
}

// HistoryCompactOptions is what one project's compaction does.
type HistoryCompactOptions struct {
	// Apply writes. Without it the call reads and reports, and a dry run is the
	// default at the command too: the first thing an operator wants to know about
	// a repair over a table they did not build is how much of it there is.
	Apply bool
	// FixUpdatedAt also restores each live memory's updated_at. It is a separate
	// flag from Apply because it is a separate repair — the redundant versions can
	// be removed on their own, and a caller who wants the history back under its
	// caps without a stamp moving does not have to accept a stamp moving.
	FixUpdatedAt bool
	// Before bounds the repair to versions recorded before a time, as a
	// 2006-01-02 date or an RFC 3339 instant. Empty means reflectNoOpCutoff, the
	// instant #727 shipped, and the default is the safe one: a version recorded
	// after it was written by a build that has the fix, and one that still looks
	// redundant was written by the merge reuse this repair cannot see through. See
	// ResolveCompactCutoff, which reports the bound a run actually used so a
	// caller can name it.
	Before string
}

// HistoryCompactResult is what one project's compaction found and did. The counts
// are the same numbers in a dry run and in an apply, which is what makes the dry
// run a preview rather than an estimate; see the fixed-point note on CompactHistory.
type HistoryCompactResult struct {
	// ProjectID is the project the counts are for.
	ProjectID string `json:"project_id"`
	// Before is the cutoff the run used, normalized to StoredStampLayout. It is on
	// the result rather than only in the caller's hands because the counts below are
	// meaningless without it: "18 redundant versions" answers a different question
	// at each bound, and a report that printed the number without the bound would be
	// reporting half an answer.
	Before string `json:"before"`
	// Removed is the number of redundant versions deleted, or — in a dry run — the
	// number the apply would delete.
	Removed int64 `json:"removed"`
	// UpdatedAt is the number of live memories whose updated_at was restored, or
	// would be.
	UpdatedAt int64 `json:"updated_at_restored"`
	// StampsUnreadable is the number of memories whose stamp could not be restored
	// because one of the two timestamps no layout in StampLayouts reads. Reported
	// rather than silently skipped: a row this run could not repair is a row whose
	// supersede orientation is still wrong, and a count of zero fixes would
	// otherwise read as "nothing left to do".
	StampsUnreadable int64 `json:"stamps_unreadable"`
	// StampsUnrecorded is the number of memories carrying a removable version that
	// this run left in place because no KEPT version of theirs was written by a
	// writer that moves updated_at. There is no instant to restore the stamp to, so
	// it is left exactly as it is — and reported, because the stamp is still
	// reading as a reflect run's time and an operator has to be told that rather
	// than read "nothing left to do" off a count of zero fixes.
	//
	// It is a distinct outcome from StampsUnreadable rather than another reason for
	// it: an unreadable stamp is a value that exists and cannot be parsed, and this
	// is a value whose AUTHOR does not exist anywhere in the table.
	StampsUnrecorded int64 `json:"stamps_unrecorded"`
}

// CompactHistory repairs one project's recorded history: it removes the version
// rows that changed nothing, and with FixUpdatedAt it restores each live memory's
// updated_at to the recorded_at of the last version that did change something.
//
// The scope is one project, and the per-project count is what the result is for: a
// store's history is a whole-corpus audit, so "how much of it was noise" has a
// different answer in every project and the report is per project. An empty
// projectID is refused rather than read as "all projects": the caller iterating
// projects is the caller that knows which exist (Store.ListProjects), and a
// whole-store compaction is not something a caller should get by leaving a field
// empty.
//
// #redundantRowsAreAFixedPoint
//
// The delete is safe to run in batches, and one batch's deletions cannot change
// which later rows are removable. A removable row R records the same state as its
// predecessor P; the row after R compares against R, and once R is gone it compares
// against P — the same state, and so the same answer. The newest-version guard does
// not break that: R is never a memory's newest row, so removing it cannot promote
// a different row into being newest. The set of removable rows is therefore a fixed
// point of the deletion, which is what lets the apply loop until a batch comes back
// short and lets the dry run answer with a single count and be exact rather than a
// sample.
func (s *Store) CompactHistory(ctx context.Context, projectID string, opts HistoryCompactOptions) (HistoryCompactResult, error) {
	if projectID == "" {
		return HistoryCompactResult{}, fmt.Errorf("compact history: a project is required")
	}
	cutoff, err := ResolveCompactCutoff(opts.Before)
	if err != nil {
		return HistoryCompactResult{ProjectID: projectID}, err
	}
	res := HistoryCompactResult{ProjectID: projectID, Before: cutoff}

	if !opts.Apply {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.compactHistoryPreview(ctx, projectID, cutoff, opts, res)
	}

	// The write lock, for the reason every other writer here takes it: the batches
	// below are BEGIN IMMEDIATE, and a reader admitted between them would read a
	// history that is half compacted.
	s.mu.Lock()
	defer s.mu.Unlock()

	// The updated_at pass runs FIRST, and the order is load-bearing rather than
	// cosmetic. Its decision asks whether a version that changed nothing sits NEWER
	// than the last real change (restorableStamp), so it has to see that version —
	// and the delete below is what removes it. Running the delete first would leave
	// the pass with no evidence at all and it would restore nothing, while a dry run
	// that deleted nothing would restore something: a preview and the run it
	// previews, disagreeing. The delete's own predicate does not depend on the pass,
	// which only ever writes memories.updated_at.
	//
	// The counts are carried on the error path, and that is not decoration. A store
	// with tens of thousands of redundant versions commits many batches before
	// anything can fail, and every one of them is durable; the counts are the only
	// record that this run rewrote part of the store, and a zero handed back beside
	// an error describes a store that no longer exists.
	if opts.FixUpdatedAt {
		fixed, unreadable, unrecorded, err := s.restoreUpdatedAt(ctx, projectID, cutoff, true)
		if err != nil {
			res.UpdatedAt, res.StampsUnreadable, res.StampsUnrecorded = fixed, unreadable, unrecorded
			return res, err
		}
		res.UpdatedAt, res.StampsUnreadable, res.StampsUnrecorded = fixed, unreadable, unrecorded
	}
	removed, err := s.deleteRemovableHistory(ctx, projectID, cutoff)
	if err != nil {
		res.Removed = removed
		return res, err
	}
	res.Removed = removed
	return res, nil
}

// compactHistoryPreview answers a dry run without opening a transaction: nothing is
// written, so there is no write lock to take. Both counts come from the same SQL and
// the same decision the apply makes, in the same ORDER, off the same table — a
// preview and the run it previews cannot disagree, including about a row whose clock
// is ambiguous.
func (s *Store) compactHistoryPreview(ctx context.Context, projectID, cutoff string, opts HistoryCompactOptions,
	res HistoryCompactResult) (HistoryCompactResult, error) {
	if opts.FixUpdatedAt {
		fixed, unreadable, unrecorded, err := s.restoreUpdatedAt(ctx, projectID, cutoff, false)
		if err != nil {
			res.UpdatedAt, res.StampsUnreadable, res.StampsUnrecorded = fixed, unreadable, unrecorded
			return res, err
		}
		res.UpdatedAt, res.StampsUnreadable, res.StampsUnrecorded = fixed, unreadable, unrecorded
	}
	query, args := compactCountStmt(projectID, cutoff)
	var removed int64
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&removed); err != nil {
		return res, fmt.Errorf("count removable history versions: %w", err)
	}
	res.Removed = removed
	return res, nil
}

// deleteRemovableHistory removes this project's removable versions in bounded
// BEGIN IMMEDIATE batches, and reports how many went.
func (s *Store) deleteRemovableHistory(ctx context.Context, projectID, cutoff string) (int64, error) {
	query, args := compactDeleteStmt(projectID, cutoff)
	var total int64
	for {
		// beginWrite, not s.db.BeginTx: this opens a write transaction per batch
		// and a whole pass can be dozens of them, which is the shape #671 measured
		// — a writer that loses its hand-off on SQLite's growing busy-handler
		// schedule — so the bounded retry rides on the batches too, and the wait
		// and hold are reported under their own op name rather than folded into a
		// writer's.
		tx, lock, err := s.beginWrite(ctx, "history-compact")
		if err != nil {
			return total, fmt.Errorf("begin compact history: %w", err)
		}
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			_ = tx.Rollback() //nolint:errcheck
			return total, fmt.Errorf("delete removable history versions: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			_ = tx.Rollback() //nolint:errcheck
			return total, fmt.Errorf("count deleted history versions: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return total, fmt.Errorf("commit compact history: %w", err)
		}
		// The lock is free from here, so this is where the hold ends.
		lock.reportHold("history-compact", time.Now())
		total += n
		// A batch shorter than the limit means the project held no more removable
		// rows. Not an empty one: a full final batch is the ordinary shape when the
		// count divides the limit exactly, and stopping there would leave that last
		// batch for a run that had nothing else to do.
		if n < int64(historyCompactBatchSize) {
			return total, nil
		}
	}
}

// compactStamp is one live memory's candidate restore: the anchor and the instant
// it was recorded (target, recorded), the freshness stamp the row holds now, and
// the newest rowid of this memory's history the compaction can remove.
//
// The anchor is the newest version the repair will NOT remove, NOT the newest
// version that changed state — those differ exactly where the second is invisible,
// which is every row recording the state of its predecessor, and a deliberate
// writer's row is one of those.
type compactStamp struct {
	memoryID      string
	target        int64
	recorded      string
	updatedAt     string
	removableLast int64
}

// restorableStamp decides one memory's restored updated_at, and is the ONLY place
// that decision is made: the dry run's count and the apply's write both come
// through it, so a preview and the run it previews cannot disagree.
//
// The first gate is the one that keeps the repair from undoing a change the store
// made on purpose, and it is a single condition: a REMOVABLE version must sit above
// the ANCHOR. That is the whole claim the repair makes — the reflect run that moved
// the stamp is the one whose versions said nothing, and it said nothing AFTER the
// last thing anybody actually changed.
//
// The anchor is the newest version this repair will not remove, and "removable" is
// doing the load-bearing work in that sentence. It means the cut, the phase, the
// state comparison AND not being the newest row, and a current build writes rows
// this gate has to respect for every one of those reasons:
//
//   - a consolidation merge whose survivor is one of its own sources carries the
//     union of the sources' tags, and the tags are not a column here
//     (TestAReflectReuseChangesTagsWithoutChangingAnyRecordedColumn runs that path),
//     so the row restates the state byte for byte — the cut keeps it out;
//   - a tags-only UpdateMemory files an update version on every edit with no no-op
//     guard, and its phase keeps it out.
//
// Neither is a version that changed nothing, so neither may be read as proof that a
// reflection ran. A third shape is worth naming because it is NOT one of those and
// would look like a fourth if it were left out: Upsert's importance fold files a
// state-identical phaseMerge row carrying the folded text, and it moves NO
// updated_at, so it is not a bump and must not close the gate. Neither must the
// related_id rows — delete, supersede, unsupersede, none of which touches a live
// memory's stamp. stampMovingPhases is the list that separates the two groups, and
// it is three phases long because three writers bump the column.
//
// The newest-version clause is deliberately NOT part of the anchor, and the reason
// is arithmetic rather than judgement: a removable row is by definition not the
// newest, so treating the newest row as an anchor would put the anchor above every
// removable row and `removableLast > target` would never hold. The whole flag would
// report 0 on a store holding nothing but the damage. A memory's newest version is
// the last row a reflect run wrote, so it has to be allowed to be that run's
// evidence.
//
// Then the direction. The damage moved updated_at FORWARD, to the time of a reflect
// that changed nothing, so the repair moves it BACK to the last real change and only
// ever back. A stamp already at or before that target is not damage: it may be a
// writer that moved it without filing a history row, a clock that ran ahead, or a
// row some other tool edited. Moving it FORWARD to meet a history that has stopped
// recording would invent a freshness Ghost never recorded, which is the failure this
// repair exists to undo.
//
// A stamp no layout in StampLayouts reads is reported rather than guessed at, on
// either side: a target Ghost cannot interpret is not a target, and an unreadable
// current value is not evidence that the move would be backward.
//
// The third outcome is the anchor being ABSENT, and it is separate from the two
// above rather than folded into either. A memory whose every kept version was
// written by a writer that moves no stamp has no target at all — the clearest case
// is a pre-v17 memory, which has no `save` version because migrateV17 records no
// starting row, whose first recorded version is an `unresolve` from ClearResolved,
// and which then accumulated the no-op reflects. There is nothing to restore the
// stamp to, so it stays exactly as it is and the run says so; the alternative is
// reading a row that moved no stamp as if it had, which is the invention this file
// exists to undo. The removable versions still go — the delete does not depend on
// the stamp — so a count of zero fixes alone would describe a run that had done
// everything it could, and this one names what it could not.
//
// The stamp is written in StoredStampLayout rather than in the shape it was read
// in, because the two columns are written by different statements: a whole-day
// value on either side is legal to read and illegal to write back, and copying the
// shape read would leave a row in a form no writer produces — which is the form a
// reader has to try twice to parse.
func restorableStamp(s compactStamp) (stamp string, restorable, unreadable, unrecorded bool) {
	if s.removableLast == 0 || s.removableLast <= s.target {
		return "", false, false, false
	}
	// Before the parse, because a target of zero means recorded is the empty
	// string the candidate statement substituted for a missing anchor, and
	// ParseStamp would report an absent row as an unreadable one. The two are
	// different facts and a report that merged them would send an operator looking
	// for a timestamp shape that does not exist.
	if s.target == 0 {
		return "", false, false, true
	}
	target, ok := ParseStamp(s.recorded)
	if !ok {
		return "", false, true, false
	}
	current, ok := ParseStamp(s.updatedAt)
	if !ok {
		return "", false, true, false
	}
	if !current.After(target) {
		return "", false, false, false
	}
	return target.UTC().Format(StoredStampLayout), true, false, false
}

// restoreUpdatedAt walks every live memory of the project that has a version which
// changed state, in bounded batches, and reports how many were restored, how many
// could not be read, and how many have no recorded stamp write to restore from.
//
// The write lock is the caller's, and each batch takes the database's own write lock
// in its own BEGIN IMMEDIATE, so the lock is taken historyCompactBatchSize times over
// the pass rather than held for it. apply is the only difference between the two
// modes: a dry run reads on the pool and opens no transaction at all, because there
// is nothing to write and a transaction here would take the write lock to look at a
// table nobody asked it to change.
//
// fixed counts COMMITTED stamps and nothing else, which is why a batch's decisions
// are added after its commit rather than inside the loop. A stamp is a row's
// freshness: a count that included a write whose batch rolled back would send an
// operator looking for damage that is still exactly where it was, and the count is
// the only thing they have to go on when a run over a large store failed part way
// through. unreadable and unrecorded are counted as they are decided instead,
// because each is a statement about the data rather than a write, and both survive
// the rollback of the batch that noticed them.
func (s *Store) restoreUpdatedAt(ctx context.Context, projectID, cutoff string, apply bool) (fixed, unreadable, unrecorded int64, err error) {
	cursor := ""
	for {
		// tx is nil in a dry run and this batch's transaction in an apply, and it is
		// the ONLY difference between the two modes below: one read, one decision,
		// and a write skipped when there is no transaction to write in. Three copies
		// of that loop would be three answers.
		var (
			tx   *sql.Tx
			lock writeLock
		)
		if apply {
			// beginWrite for the reason deleteRemovableHistory gives: a pass opens
			// a transaction per batch, and #671's bounded retry is what keeps a
			// writer from losing its hand-off on the busy handler's growing poll
			// schedule.
			if tx, lock, err = s.beginWrite(ctx, "history-compact-stamp"); err != nil {
				return fixed, unreadable, unrecorded, fmt.Errorf("begin restore updated_at: %w", err)
			}
		}
		var q stampQuerier = s.db
		if tx != nil {
			q = tx
		}
		batch, readErr := s.stampBatchQuery(ctx, q, projectID, cursor, cutoff)
		if readErr != nil {
			if tx != nil {
				_ = tx.Rollback() //nolint:errcheck
			}
			return fixed, unreadable, unrecorded, readErr
		}
		stamped := int64(0)
		for _, c := range batch {
			stamp, restorable, bad, absent := restorableStamp(c)
			if bad {
				unreadable++
			}
			if absent {
				unrecorded++
			}
			if !restorable {
				continue
			}
			stamped++
			if tx == nil {
				continue
			}
			if _, err := tx.ExecContext(ctx, historyCompactRestoreSQL, stamp, c.memoryID, projectID); err != nil {
				_ = tx.Rollback() //nolint:errcheck
				return fixed, unreadable, unrecorded, fmt.Errorf("restore updated_at of %s: %w", c.memoryID, err)
			}
		}
		if tx != nil {
			if err := tx.Commit(); err != nil {
				return fixed, unreadable, unrecorded, fmt.Errorf("commit restore updated_at: %w", err)
			}
			lock.reportHold("history-compact-stamp", time.Now())
		}
		fixed += stamped
		if len(batch) < historyCompactBatchSize {
			return fixed, unreadable, unrecorded, nil
		}
		cursor = batch[len(batch)-1].memoryID
	}
}

// stampBatchQuery is the one shape both modes read: the candidate statement, scoped
// and paged. An apply runs it on the transaction it will write in, so a memory that
// is deleted or moved between the read and the write cannot be stamped from a row
// the run has already read.
func (s *Store) stampBatchQuery(ctx context.Context, q stampQuerier, projectID, cursor, cutoff string) ([]compactStamp, error) {
	query, args := compactCandidatesStmt(projectID, cursor, cutoff)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read memories to restore updated_at: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var batch []compactStamp
	for rows.Next() {
		var c compactStamp
		if err := rows.Scan(&c.memoryID, &c.target, &c.recorded, &c.updatedAt, &c.removableLast); err != nil {
			return nil, fmt.Errorf("scan memory to restore updated_at: %w", err)
		}
		batch = append(batch, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate memories to restore updated_at: %w", err)
	}
	return batch, nil
}

// stampQuerier is the pool and a transaction, so one read serves both modes.
type stampQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}
