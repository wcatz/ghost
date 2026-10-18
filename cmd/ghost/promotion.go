package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/reflection"
	"github.com/wcatz/ghost/internal/secret"
)

// reflectionApplier is the single write boundary for `ghost reflect`. Keeping
// project replacement, explicit global promotion, and failed-candidate
// recovery behind one call makes their ordering testable and lets the store
// commit them atomically.
type reflectionApplier interface {
	ApplyReflection(ctx context.Context, projectID string, projectMems, globalMems []memory.Memory, consolidatedSince string, promoteGlobals bool, fed []memory.CorpusRow) (preserved []string, promoted int, keptMems []memory.Memory, projectDropped, globalDropped memory.EmissionDrops, deleted int, appliedByCategory map[string]int, err error)
}

// applyReflection converts reflection proposals to store rows and always
// invokes the store apply boundary. Promotion is not nested under a
// project-memory guard: a result containing only cross-project candidates is
// precisely the case where an explicit promotion request must still run.
//
// This is also where a credential-shaped proposal is dropped, and the position
// is the point. The write boundary is the only place every path passes through,
// and it is reached AFTER the drop guard's audit: the audit asks which inputs the
// output failed to account for, and executeOps emits every unclaimed input
// verbatim, so a memory the tier carried forward is byte-identical to the output
// that carried it. Dropping it earlier makes that input look unaccounted for,
// and both outcomes are wrong — RetainGuardedDrops re-adds it verbatim and the
// credential is written back (making the drop a no-op, after the audit has
// already printed its content to stderr), or some other output happens to cover
// 45% of its tokens and ReplaceNonManual deletes the stored row with no
// --allow-drops, the one deletion path the drop guard exists to close. Here the
// audit still accounts for it, so it is not re-added; and the emitted set no
// longer does, so the replace DELETES any stored memory it was carrying forward.
// Both halves are consequences of the same position, and neither is a surprise:
// the row is the value.
// `applied` is false when the round changed nothing, which is not the same as
// a round that wrote no new rows. applied is keyed on what the store actually
// WROTE or DELETED, never on its drop count: an all-dropped round — every
// emission refused because its claimed row changed or was deleted while the
// harness was answering (#1051) — can still remove the unclaimed candidates
// beside those refused claims, and a replace that changed the corpus, snapshot
// included, has to print "Applied", offer its restore hint, and record the
// learned context like any other. The caller needs the difference: a round
// that changes nothing must not print "Applied", must not offer a restore
// hint, and above all must not record the skip fingerprint over an unchanged
// corpus — because then --skip-unchanged skips the project forever, and a
// stored row holding a credential is never revisited. Two paths reach
// applied=false: nothing survived the credential filter, so the store is not
// even called, and the store refused every emission it was handed with no row
// deleted either.
// `keptProject` and `keptGlobal` are the POST-drop proposal sets, because the
// caller's "Applied: N memories consolidated" line counts them. Returning only a
// count left it counting the pre-drop lists, so a partial drop — one good
// proposal and one credential — reported 2 written when 1 was. `projectDropped`
// and `globalDropped` come back from the store and close the same gap on the
// OTHER drop path (the corpus guard): they are the emissions the store refused,
// per half, BY REASON, so the summary can subtract only what each of its
// components counts and can tell a round that wrote nothing from one that did.
// The reason split is not decoration either: a drop whose claimed row still
// stands has to keep the skip fingerprint unrecorded, and a merge dropped
// because its decision links do not fit in a source_ref would be re-proposed on
// every run if it did the same (#1228).
func applyReflection(ctx context.Context, store reflectionApplier, projectID string, projectMems, globalMems []reflection.ReflectMemory, consolidatedSince string, promoteGlobals bool, replaced map[string][]string, fed []memory.CorpusRow) (keptProject, keptGlobal []reflection.ReflectMemory, preserved []string, promoted int, keptMems []memory.Memory, projectDropped, globalDropped memory.EmissionDrops, deleted int, appliedByCategory map[string]int, applied bool, err error) {
	// Filter FIRST, on the caller's two sets, and leave the fold to the store.
	//
	// The order is load-bearing and it was the other way round here first.
	// Folding the cross-project candidates into projectMems and nilling
	// globalMems BEFORE the drop means keptGlobal comes back as the empty slice
	// the drop builds for a nil input — never the caller's cross-project set —
	// and the two things the caller needs it for then silently die: the
	// "(re-run with --promote-globals)" hint becomes unreachable, and the
	// ", N cross-project candidates kept project-scoped" component disappears
	// from the Applied line. Filtering first also drops a credential among the
	// cross-project candidates before they can be promoted, which is the point.
	//
	// The fold itself is the store's job now, not this wrapper's: ApplyReflection
	// folds the cross-project rows into the project replace when promotion is
	// off, and it does so AFTER its own corpus guard has filtered them. A fold
	// here would put those rows into the one list the store sees, so its two
	// drop counts could no longer be attributed to the half that produced them,
	// and the summary's project count and cross-project component subtract
	// different halves. The store already folded the same way on every path that
	// reached it, so this only changes who owns the slice juggling, not what is
	// written.
	keptProject, keptCrossProject, credDropped := dropCredentialProposals(projectMems, globalMems)
	projectMems = keptProject
	// Two different sets from here on, and conflating them is the failure the
	// ordering above is guarding: what the STORE gets (both lists, folded inside
	// ApplyReflection when promotion is off) and keptCrossProject, which is what
	// the caller REPORTS. Nilling the returned set to fold it here would make the
	// --promote-globals hint and the cross-project count unreachable, and a flag
	// nobody is told about is a flag nobody uses.
	projectRows := reflectMemoriesToMemory(projectID, projectMems, replaced)
	globalRows := reflectMemoriesToMemory("_global", keptCrossProject, replaced)
	if len(projectRows) == 0 && len(globalRows) == 0 {
		// Nothing to write, so nothing is replaced: no deleteIds are computed and
		// no stored row is touched. The dropped proposals are still reported
		// above, and saying so here is the point — a removal claim printed before
		// anyone knows whether a replace runs closes the incident in the
		// operator's head while the value sits in the database.
		return keptProject, keptGlobal, nil, 0, nil, memory.EmissionDrops{}, memory.EmissionDrops{}, 0, nil, false, nil
	}
	preserved, promoted, kept, projectDropped, globalDropped, deleted, appliedByCategory, err := store.ApplyReflection(ctx, projectID, projectRows, globalRows, consolidatedSince, promoteGlobals, fed)
	if err != nil {
		return keptProject, keptGlobal, nil, 0, nil, memory.EmissionDrops{}, memory.EmissionDrops{}, 0, nil, false, err
	}
	// Two things have to be true for a carried-forward row to be gone, and this
	// note says only what holds. A proposal dropped for holding a credential is
	// no longer in the emitted set, so the replace treats the stored memory it
	// was carrying as unmatched and deletes it — but only if a project replace
	// ran (len(projectMems) > 0 in the store), and only for a row the replace
	// can reach. A manual, builtin, pinned or resolved row is excluded from its
	// candidate query, and a fresh merge or rewrite carried nothing. So the note
	// states the mechanism and where to look rather than a number of deletions:
	// a count of dropped proposals is not a count of deleted rows, in either
	// direction, and the only number available here is the first.
	//
	// --allow-drops does not gate the removal: that flag is the operator
	// authorising what the MODEL chose to drop, and this is Ghost removing a row
	// that is itself the value.
	if credDropped > 0 && len(projectRows) > 0 {
		fmt.Fprintln(os.Stderr,
			"note: the project replace has just removed any stored memory the dropped proposal(s) were carrying forward, because it held a credential value. A manual, builtin, pinned or resolved row is not replaceable and would still be there: check the project")
	}
	// A successful transaction is not a successful round. When the store refused
	// every emission it was handed AND removed no stored row — their claimed rows
	// changed or were deleted while the harness was answering, and no unclaimed
	// candidate sat beside them — nothing changed, so the round is reported and
	// recorded exactly as if nothing had reached the store at all: no "Applied",
	// no restore hint, and above all no skip fingerprint over a corpus the
	// consolidator's answer never landed in. The store's own delete count, not
	// the drop count, is what separates that from an all-dropped round that
	// still changed the corpus: the latter took a snapshot the operator can
	// restore, so it is a replace and must be reported as one.
	applied = true
	if projectDropped.Total()+globalDropped.Total() == len(projectRows)+len(globalRows) && deleted == 0 {
		applied = false
	}
	return keptProject, keptCrossProject, preserved, promoted, kept, projectDropped, globalDropped, deleted, appliedByCategory, applied, nil
}

// displayProposal renders a proposal or a guarded drop for the operator's own
// eyes, substituting a description for the content whenever the content holds a
// credential.
//
// Every print site in cmd/ghost that renders stored memory text IN A REPORT — the
// lifecycle listings and `ghost history` — goes through this substitution:
// displayProposal itself, displayClaim, displayStored, and the two history
// printers. A guarantee made at the write boundary — the drop reports format, category, scope and length, never the
// content — is not a guarantee about the command's report if the same command
// prints the value somewhere else: the proposal listing and the drop-guard warning
// both printed 120 truncated characters, and 120 is far more than a GitHub PAT or a
// Docker Hub token needs. In the autonomous path that stdout is the append-only
// lifecycle.log, so the value outlives the run. The exposure the store's refusal
// exists to prevent, reached from the other direction.
//
// The class is REPORT, and saying so is load-bearing rather than pedantic:
// `ghost context` and the SessionStart hook's digest (internal/mcpinit) print the
// same stored rows — `quoteData` wraps them in «» and nothing else — and they are
// outside this substitution on purpose. They are a data FEED rather than a report
// a person reads: a memory withheld from the injected context is silently out of
// every later session, which is a worse failure than printing one, and the same
// text is already reachable through `ghost_memory_search`, whose contract is to
// return what is stored. The control on that path is the write boundary plus a
// store scan, the same answer the search tool has. docs/architecture.md records
// this under the credential guard.
//
// The lifecycle listings were the sites left out, and the argument for them was
// that the write-boundary guard already refuses their input. That argument is
// about the databases this build writes, and it says nothing about a database
// written before the guard existed: `rejectSecret` is not retroactive, a pre-guard
// row can still hold a value, and `firstLine` at 70 characters is more than a
// GitHub PAT needs. So `ghost resolve` (both listings), `resolve --mark` and
// `supersede --withdraw` route through displayStored, which is this same
// substitution over the first-line cut those lines already made.
//
// `ghost history` is the site where two layers are both correct, and neither
// replaces the other. History content is redacted at WRITE time
// (`ghost_history_content` in internal/memory), and that is the right layer for
// it: it is the only one that also covers the reads Ghost performs itself — an
// `as_of` search answers from a recorded version and feeds session injection — and
// every future reader of that table, none of which would have to remember a print
// site. But a filter installed today cannot reach the rows already on disk, and it
// does not cover `merged_content` at all, which is a plain column holding the text
// a FoldOnly fold dropped. The print site therefore carries the substitution as
// well: `ghost history` renders whatever is stored, and on the store that most
// needs purging that is a value a pre-guard build wrote. `ghost history purge` is
// the redaction path for what is left on disk; withholding is what keeps the
// command from printing it in the meantime.
//
// The substitution keeps the category and the byte length, so an operator can
// still find the row and tell how much was withheld — a report that says only
// "redacted" is indistinguishable from a report that lost the proposal.
func displayProposal(content, category string, limit int) string {
	if finding, ok := secret.Detect(content); ok {
		return withheld(finding, category, content)
	}
	return humanStoredText(content, limit)
}

// displayClaim renders text a result records without a category: a rewrite's
// replacement, a disposal claim's witness, an identifier the grounding check
// rejected. It substitutes for a credential for the same reason displayProposal
// does — a replacement is derived from stored text, so it can carry a value a
// pre-guard row held — and reports no category, because the result records none.
func displayClaim(content string, limit int) string {
	if finding, ok := secret.Detect(content); ok {
		return withheld(finding, "", content)
	}
	return humanStoredText(content, limit)
}

// withheld is the one rendering of a credential that a cmd/ghost print site
// produces: the format, the category when the caller has one, and how many bytes
// were kept. Every substitution above goes through it, so a second site cannot
// invent a second marker, and the two shapes are the two facts available — a
// caller with a category says so, and a caller without one (an edge's target, a
// fold's discarded wording) does not print an empty field where one should be.
func withheld(finding secret.Finding, category, content string) string {
	if category == "" {
		return fmt.Sprintf("<withheld: %s, bytes=%d>", finding.Label, len(content))
	}
	return fmt.Sprintf("<withheld: %s, category=%s, bytes=%d>", finding.Label, category, len(content))
}

// humanStoredText is the ONE renderer for stored text on a report line in
// cmd/ghost, and it is where the line-safety of every such site is decided.
//
// A limit above zero takes the FIRST LINE of the text, capped at the limit, and
// a limit of zero or less — what `ghost reflect --full` asks for — takes the
// whole of it. A whole field carries the «...» delimiters and a preview does
// not, and that is a property of the function rather than an omission: a reader
// can be fooled by a whole line that is not delimited, while the `--json` printer
// must never delimit at all because its consumer is a script and encoding/json
// already escapes a newline.
//
// It is one function rather than a width half and a delimiting half because two
// halves with two callers each is how the two drifted — `displayText` was the
// width half, nothing called it once its two callers moved here, and a function
// nothing calls is a second rule waiting to be the one that ships the bug.
//
// The first-line cut is `assemble.PreviewLine`, the ONE cut every other listing
// already uses, and it is a cut at the first of EITHER byte: a byte cut does not
// stop at a line break, so the first 120 bytes of a memory holding a newline
// printed the line after it too — on the default `ghost reflect` report, where
// the limit is 120 and not the `--full` zero. That is the shape #802 is about,
// and it is why the cut lives here rather than at each call site: a display flag
// spread across five sites is how three copies of one rule drifted.
func humanStoredText(content string, limit int) string {
	if limit <= 0 {
		return assemble.Data(content)
	}
	return assemble.PreviewLine(content, limit)
}

// dropCredentialProposals removes the proposals whose content holds a
// credential, from both lists.
//
// Every removal is reported, with its format, category, scope and length and
// never its content, because a discard that leaves no trace is indistinguishable
// from a proposal the model never emitted.
//
// The report also says the thing that is easy to get wrong about it: any STORED
// memory the proposal was carrying forward goes with it. ReplaceNonManual
// deletes every replaceable row the emitted set does not account for, and this
// memory is now not accounted for, so an unattended `ghost reflect --apply`
// removes the stored row. That is the correct outcome — the row IS the value,
// and leaving it is the leak — but it is a deletion, so it is named as one
// rather than described as a proposal that was merely not applied. It is a
// Ghost-owned removal rather than a model decision, which is why it does not go
// through --allow-drops: that flag is the operator authorising what the MODEL
// chose to drop.
func dropCredentialProposals(projectMems, globalMems []reflection.ReflectMemory) (keptProject, keptGlobal []reflection.ReflectMemory, dropped int) {
	// A fresh slice, not in[:0]. The caller LISTS these and COUNTS them after
	// this returns, and in-place filtering would leave it holding the pre-drop
	// length over a backing array it can no longer read correctly — so the
	// "Applied: N memories consolidated" line would count a proposal that was
	// never written.
	keep := func(in []reflection.ReflectMemory) (out []reflection.ReflectMemory, n int) {
		out = make([]reflection.ReflectMemory, 0, len(in))
		for _, m := range in {
			finding, ok := secret.Detect(m.Content)
			if !ok {
				out = append(out, m)
				continue
			}
			n++
			fmt.Fprintf(os.Stderr,
				"note: consolidation proposal not applied — it holds a credential value (format=%s category=%s scope=%s bytes=%d)\n",
				finding.Label, m.Category, m.Scope, len(m.Content))
		}
		return out, n
	}
	keptProject, n := keep(projectMems)
	keptGlobal, m := keep(globalMems)
	return keptProject, keptGlobal, n + m
}

// reflectMemoriesToMemory converts proposals to store rows. replaced maps an
// emitted memory's TEXT to the input ids that merge or rewrite consumed to produce
// it, and is carried through as Memory.ReplacesIDs so ReplaceNonManual can stamp
// the successor id on those rows' delete history: a rewrite or a merge gives the
// row a new id, and without the mapping a reader following one memory's history
// stops exactly where the memory changed.
//
// Text is the join key because it is the only handle the operation list and the
// emitted list share — the operations are resolved before the memories are, and
// nothing pairs them by id. Two emissions with identical text are the same fact
// as far as consolidation is concerned, so they legitimately share the mapping.
func reflectMemoriesToMemory(projectID string, mems []reflection.ReflectMemory, replaced map[string][]string) []memory.Memory {
	if len(mems) == 0 {
		return nil
	}
	rows := make([]memory.Memory, len(mems))
	for i, m := range mems {
		rows[i] = memory.Memory{
			ProjectID:   projectID,
			Category:    m.Category,
			Content:     m.Content,
			Importance:  m.Importance,
			Source:      "reflection",
			Tags:        m.Tags,
			ReplacesIDs: replaced[m.Content],
		}
	}
	return rows
}

// replacedIDsByText inverts the operation list into the shape
// reflectMemoriesToMemory consumes: for every merge and every rewrite (and every
// drop that named a witness), the ids it disposed of, keyed by the text that
// replaced them. A drop naming no witness contributes nothing — an obsolete drop
// has no successor, and inventing one would put a false pointer on a delete row.
//
// The key is the CLAMPED text, and that is load-bearing. clampReflectMemories runs
// before this and rewrites every emission in place, so an emission over
// memory.MaxContentLen reaches the store truncated while the operation list still
// holds the original — keying on the original matched nothing, and the ids of
// exactly the largest merges (the ones with the most sources to point at) were
// silently dropped. Clamping the key with the same function the emission went
// through puts both sides in the same space without mutating the result or
// threading a second copy of the clamped emissions through.
func replacedIDsByText(result *reflection.ReflectionResult) map[string][]string {
	out := map[string][]string{}
	if result == nil {
		return out
	}
	for _, m := range result.Merges {
		key, _ := memory.ClampContent(m.Text)
		out[key] = append(out[key], m.IDs...)
	}
	for _, r := range result.Replacements {
		key, _ := memory.ClampContent(r.Text)
		out[key] = append(out[key], r.ID)
	}
	return out
}

func reflectCategoryParts(counts map[string]int) string {
	if len(counts) == 0 {
		return ""
	}
	categories := make([]string, 0, len(counts))
	for category := range counts {
		categories = append(categories, category)
	}
	sort.Strings(categories)
	parts := make([]string, 0, len(categories))
	for _, category := range categories {
		parts = append(parts, fmt.Sprintf("%d %s", counts[category], category))
	}
	return strings.Join(parts, ", ")
}

// reflectResultLine renders the `Result:` line: what came back, broken down by
// category, and the repair turn the LLM tier had to spend to get an answer at
// all (see reflectRepairNote).
//
// It is a function taking the result rather than a Printf in runReflect because
// runReflect exits the process, so the line is otherwise unreachable from a test
// and a test that re-spelled the format string would pass with the command
// changed — the same seam reasoning as gatedLLMTier.
func reflectResultLine(result reflection.ReflectionResult) string {
	counts := make(map[string]int, len(result.Memories))
	for _, m := range result.Memories {
		counts[m.Category]++
	}
	return fmt.Sprintf("Result:       %d memories (%s)%s\n", len(result.Memories), reflectCategoryParts(counts), reflectRepairNote(result.RepairTurns))
}

// reflectRepairNote reports the run's repair turn on the `Result:` line: a run
// whose first answer the strict ops reader rejected and which succeeded on the
// re-read says so, and a run that needed no repair says nothing at all.
//
// One line of this process's own output carries it, and nothing else does, for a
// counting reason. On the unattended path that stdout IS the append-only
// lifecycle.log (the stop hook redirects it, see internal/mcpinit/stophook), so
// printing the token a second time on the other stream would double every count
// the token exists to produce. The `repair: %d` token is the stable grep for "the
// first answer was malformed and the run survived it", and the denominator is the
// `Result:` lines in the same log. A run that repaired and then failed outright is
// not in that denominator — it never printed a summary — and its rejection is on
// the WARN line the tier writes whether the run goes on to succeed or not. (A
// grep over the phase-failure MARKER as well as the log sees the same line twice,
// because the marker copies the last of the phase's output; grep the log.)
//
// The count is 0 or 1 because the tier allows one extra turn (opRepairTurns). It
// is per run rather than per tier, so it can be 1 on a result the mechanical tier
// produced — see ReflectionResult.RepairTurns — which is why the wording names
// the re-read rather than the result. If the bound ever moves, the wording here
// has to move with it rather than report one re-read for a number that is not
// one.
func reflectRepairNote(turns int) string {
	if turns <= 0 {
		return ""
	}
	return fmt.Sprintf("  repair: %d (a rejected response was re-read)", turns)
}

// crossProjectKept is the number of cross-project candidates the round actually
// left in the project: every candidate the caller handed over, minus the ones
// the store refused. With promotion off a candidate can be refused by EITHER
// guard pass — the global half's own pass, or the replace's second pass over the
// folded list — and the store attributes both to the same drop count precisely
// so this one subtraction covers them. Callers that print per-candidate
// follow-ups must count with this too: a round that refused every candidate has
// nothing to promote, and saying otherwise contradicts the applied total
// printed next to it.
func crossProjectKept(globalMems []reflection.ReflectMemory, globalDropped int) int {
	if kept := len(globalMems) - globalDropped; kept > 0 {
		return kept
	}
	return 0
}

// appliedSummary describes the rows that actually landed in the project. The
// caller passes the store's per-category applied counts — what the replace
// wrote, plus any kept-back global candidate written into the project — so the
// total and the category breakdown come from the SAME map: an emission the
// corpus guard refused is counted in neither, and the parts always sum to the
// total. Counting the caller's emitted slices instead was the bug here: they
// still hold the dropped emissions, so a partial-refusal round printed "(1
// fact)" beside "0 memories consolidated".
func appliedSummary(appliedByCategory map[string]int, globalMems []reflection.ReflectMemory, promoted, globalDropped int, promoteGlobals bool) string {
	total := 0
	for _, n := range appliedByCategory {
		total += n
	}
	summary := fmt.Sprintf("%d memories consolidated", total)
	if parts := reflectCategoryParts(appliedByCategory); parts != "" {
		summary += " (" + parts + ")"
	}
	if len(globalMems) == 0 {
		return summary
	}
	if promoteGlobals {
		return summary + fmt.Sprintf(", %d promoted to global", promoted)
	}
	return summary + fmt.Sprintf(", %d cross-project candidates kept project-scoped", crossProjectKept(globalMems, globalDropped))
}

// writeAppliedReport prints the Applied block of a round that wrote something
// and returns the summary for the learned-context record. wrote is false when
// the round wrote nothing (the caller's !applied branch already said why).
//
// A round that wrote something counts as applied, and that includes a
// candidate that could not be promoted and was written back into the project:
// it is a row the project now holds. Keying this on the project-memory count
// alone dropped the summary for a promotion-only round.
//
// deleted is the store's count of rows the replace removed. It is stated on
// the Applied line only when non-zero, because a round whose every emission
// was refused by the concurrent-edit guard still deletes the rows nothing
// claimed, and "0 memories consolidated" alone would hide that.
func writeAppliedReport(w io.Writer, applied, promoteGlobals bool, appliedByCategory map[string]int,
	keptProject, keptGlobal []reflection.ReflectMemory, keptMems []memory.Memory,
	projectForSummary []reflection.ReflectMemory, promoted, droppedGlobal, deleted int) (summary string, wrote bool) {
	if !applied || (promoted == 0 && len(keptMems) == 0 && len(keptProject) == 0 && len(keptGlobal) == 0) {
		return "", false
	}
	// With promotion off every cross-project refusal counts as the global
	// half's, so one subtraction covers both guard passes.
	summaryGlobalDropped := 0
	if !promoteGlobals {
		summaryGlobalDropped = droppedGlobal
	}
	// The REAL promoted count, so a partial promotion does not print "3
	// promoted to global" one line after "Promoted 1/3".
	summary = appliedSummary(appliedByCategory, keptGlobal, promoted, summaryGlobalDropped, promoteGlobals)
	line := summary
	if deleted > 0 {
		line += fmt.Sprintf(", %d removed", deleted)
	}
	_, _ = fmt.Fprintf(w, "Applied: %s\n", line)
	if !promoteGlobals && crossProjectKept(keptGlobal, summaryGlobalDropped) > 0 {
		_, _ = fmt.Fprintln(w, "(re-run with --promote-globals to inject them into every project)")
	}
	if len(projectForSummary) > 0 {
		_, _ = fmt.Fprintln(w, restoreHint(promoted))
	} else if promoted > 0 {
		_, _ = fmt.Fprintln(w, "(promoted globals must be removed from _global by hand; no project snapshot was created)")
	}
	return summary, true
}

func restoreHint(promoted int) string {
	if promoted > 0 {
		return "(use --restore to undo the project replace; promoted globals stay in _global with the evidence and decision links they carried from the replaced rows, and must be removed from _global by hand)"
	}
	return "(use --restore to undo)"
}

// recoveryWarning is kept separate from the apply transaction so its count is
// never printed as zero successful recoveries. ApplyReflection rolls back on
// a failed recovery, but this formatter still fails safe if a future caller
// ever supplies a partial result.
func recoveryWarning(kept, total int) string {
	if kept <= 0 {
		return fmt.Sprintf("error: none of the %d unpromoted global memories could be returned to the project either — use --restore or re-run", total)
	}
	return fmt.Sprintf("warning: %d of %d global memories could not be promoted and were kept in the project", kept, total)
}
