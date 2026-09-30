package main

import (
	"context"
	"fmt"
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
	ApplyReflection(ctx context.Context, projectID string, projectMems, globalMems []memory.Memory, consolidatedSince string, promoteGlobals bool) (preserved []string, promoted int, keptMems []memory.Memory, err error)
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
// `applied` is false when there was nothing to write, which is not the same as
// a successful write of nothing. The caller needs the difference: an
// unattendented round that writes nothing must not print "Applied", must not
// offer a restore hint, and above all must not record the skip fingerprint over
// an unchanged corpus — because then --skip-unchanged skips the project
// forever, and a stored row holding a credential is never revisited.
// `keptProject` and `keptGlobal` are the POST-drop proposal sets, because the
// caller's "Applied: N memories consolidated" line counts them. Returning only a
// count left it counting the pre-drop lists, so a partial drop — one good
// proposal and one credential — reported 2 written when 1 was.
func applyReflection(ctx context.Context, store reflectionApplier, projectID string, projectMems, globalMems []reflection.ReflectMemory, consolidatedSince string, promoteGlobals bool, replaced map[string][]string) (keptProject, keptGlobal []reflection.ReflectMemory, preserved []string, promoted int, keptMems []memory.Memory, applied bool, err error) {
	// Filter FIRST, on the caller's two sets, and fold afterwards.
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
	keptProject, keptCrossProject, dropped := dropCredentialProposals(projectMems, globalMems)
	projectMems = keptProject
	// Two different sets from here on, and conflating them is the failure the
	// ordering above is guarding: the fold is what the STORE gets, and
	// keptCrossProject is what the caller REPORTS. Nilling the returned set to
	// fold it would make the --promote-globals hint and the cross-project count
	// unreachable, and a flag nobody is told about is a flag nobody uses.
	writeGlobals := keptCrossProject
	if !promoteGlobals && len(keptCrossProject) > 0 {
		projectMems = append(append([]reflection.ReflectMemory(nil), projectMems...), keptCrossProject...)
		writeGlobals = nil
	}
	projectRows := reflectMemoriesToMemory(projectID, projectMems, replaced)
	globalRows := reflectMemoriesToMemory("_global", writeGlobals, replaced)
	if len(projectRows) == 0 && len(globalRows) == 0 {
		// Nothing to write, so nothing is replaced: no deleteIds are computed and
		// no stored row is touched. The dropped proposals are still reported
		// above, and saying so here is the point — a removal claim printed before
		// anyone knows whether a replace runs closes the incident in the
		// operator's head while the value sits in the database.
		return keptProject, keptGlobal, nil, 0, nil, false, nil
	}
	preserved, promoted, kept, err := store.ApplyReflection(ctx, projectID, projectRows, globalRows, consolidatedSince, promoteGlobals)
	if err != nil {
		return keptProject, keptGlobal, nil, 0, nil, false, err
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
	if dropped > 0 && len(projectRows) > 0 {
		fmt.Fprintln(os.Stderr,
			"note: the project replace has just removed any stored memory the dropped proposal(s) were carrying forward, because it held a credential value. A manual, builtin, pinned or resolved row is not replaceable and would still be there: check the project")
	}
	return keptProject, keptCrossProject, preserved, promoted, kept, true, nil
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

func reflectCategoryParts(mems []reflection.ReflectMemory) string {
	counts := make(map[string]int)
	for _, m := range mems {
		counts[m.Category]++
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
	return fmt.Sprintf("Result:       %d memories (%s)%s\n", len(result.Memories), reflectCategoryParts(result.Memories), reflectRepairNote(result.RepairTurns))
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

// appliedSummary describes the rows actually written to the project. The
// caller passes the post-fold project slice, so a global candidate kept
// project-scoped is counted in both the total and category breakdown.
func appliedSummary(projectMems, globalMems []reflection.ReflectMemory, promoted int, promoteGlobals bool) string {
	summary := fmt.Sprintf("%d memories consolidated", len(projectMems))
	if parts := reflectCategoryParts(projectMems); parts != "" {
		summary += " (" + parts + ")"
	}
	if len(globalMems) == 0 {
		return summary
	}
	if promoteGlobals {
		return summary + fmt.Sprintf(", %d promoted to global", promoted)
	}
	return summary + fmt.Sprintf(", %d cross-project candidates kept project-scoped", len(globalMems))
}

func restoreHint(promoted int) string {
	if promoted > 0 {
		return "(use --restore to undo the project replace; promoted globals must be removed from _global by hand)"
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
