package audit

// #646 part 3: the REPORT — what the stored verdicts mean, per source.
//
// Part 2 recorded one row per (call, kept memory). This reads them back and
// prints figures an operator can act on, and it is the surface where a mistake is
// hardest to catch from the output: a number that is wrong here looks exactly like
// a number that is right and unflattering.
//
// So the shape is chosen to make the wrong readings impossible rather than to
// make the report short:
//
//   - Per source, never pooled. A search and a session-start injection answer
//     different questions — "did the agent use what it looked up" against "did it
//     use what it was handed" — and a ratio over both is a number about neither.
//     Report.Pooled does not exist as a number: it returns nil, always, and says
//     why. That is deliberately a function that CANNOT be wrong, because the one
//     test that matters is whether a future caller can get a pooled figure out of
//     this package at all.
//   - Precision is used / KEPT, and kept counts MEMORIES, not calls. One call can
//     keep twenty, so a call denominator would understate every figure.
//   - A source with no rows says "no rows" and reports no percentage. "0% used"
//     about a source that has never run is a measurement of nothing, and it is
//     the reading a fresh install gets first.
//   - Every known source is named even when it is empty, so a source this build
//     knows about and a source it does not can be told apart. That is #850's
//     case: before the passive injections write their records, session_start and
//     project_context have no rows, and a silent report would look like a healthy
//     one.
//   - The undetectable half of "missed" prints as NOT MEASURED, spelled out in
//     those words. It is never 0: no heuristic here can tell a fact the agent
//     re-derived in-session from one it worked out, so printing a zero would be the
//     one claim in this report that is certainly false, and a bare "not measured"
//     is itself easy to skim past as a footnote.
//   - Ids and counts only. Contradicted memories are named by id, through
//     assemble.Token; no memory's content and no query text can reach the output,
//     because the rows this reads do not hold any and the renderer has no other
//     source to draw them from.
//
// --since has only recorded_at to work with, on BOTH tables, and it is applied to
// both halves of every figure. A window that filtered the verdicts but not the
// calls would report calls that kept nothing under a source whose memory figures
// were computed over a different window, and the resulting "this source admits
// memories nothing used" is precisely the wrong reading this file is built to
// prevent.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/memory"
)

// errNoReportProject is the one input this cannot do anything with.
//
// Refused for the reason Run refuses a missing project: every figure here is a
// claim about ONE project's retrieval. Pooling two projects' rows would produce a
// number about neither — and unlike pooling the SOURCES, which this package
// refuses to do at the type level, this is refused at the boundary so the empty
// scope cannot arrive at all. A store-wide report is available by iterating the
// projects, which is what `ghost project` lists and what a caller asking for one
// number over everything should have to write down.
var errNoReportProject = fmt.Errorf("audit: a project id is required to report on")

// KnownSources is every source this build knows how to name, in the order a
// report prints them.
//
// A closed list rather than whatever the rows happen to contain, for the reason
// the empty state matters most: before #850's passive injections write their
// records, those two sources have no rows at all, and a report built only from
// the rows present would say nothing about them. Naming them with "no rows" is
// what distinguishes "this source has never run here" from "this build cannot
// see it".
//
// Drawn from assemble.Source because that is where the vocabulary is defined, and
// a copy of it here would be a list free to drift from the writers that fill it.
// all_projects and bench are deliberately absent: neither is an agent's retrieval
// of Ghost's memory — one is a cross-project read and the other is this repo's own
// harness — so neither is a call whose value to the agent this audit measures.
var KnownSources = []string{
	string(assemble.SourceSearch),
	string(assemble.SourceSessionStart),
	string(assemble.SourceProjectCtx),
}

// ReportOptions is one report's request.
//
// ProjectID is required; Since is the window, zero meaning everything the store
// still holds.
type ReportOptions struct {
	ProjectID string
	// Since bounds both tables by recorded_at. It is a WINDOW (a duration) rather
	// than an instant because the store's only per-row clock on these two tables
	// is recorded_at, and a report that took an instant would be answering a
	// question about a moment the tables cannot represent.
	Since time.Duration
	// Now is the instant the window is measured back from, and is a field so a
	// test can pin it. Zero means time.Now().
	Now time.Time
}

// Report is every figure for one project over one window.
//
// There is no Project-wide Used or Kept field, and no method that computes one:
// the numbers are only ever per source, which is the whole point of the type.
type Report struct {
	ProjectID string
	// Since echoes the window, so a saved report says what it measured rather
	// than leaving the reader to assume the default.
	Since time.Duration
	// Sources holds one entry per source, known or seen, sorted. Every known
	// source is present even when empty, so a reader can tell an absent source
	// from an unrecognised one.
	Sources []SourceReport
}

// SourceReport is one source's figures over the window.
type SourceReport struct {
	// Source is the recorded call's own source label. It is rendered through
	// assemble.Label rather than printed raw: the column is unconstrained text, a
	// row can carry a newline, and a label that broke the report's line structure
	// would forge a figure of its own.
	Source string
	// Calls is how many recorded calls came from this source, including the ones
	// that kept nothing. The grain of retrieval_record is the call, so this is the
	// count the table can answer exactly.
	Calls int
	// Kept is how many (call, memory) pairs this source admitted. It is read off
	// the recorded calls' own kept rows, so it is a fact about what was SHOWN and
	// it is available in a store nobody has audited yet — which is the whole reason
	// it is not the same number as Scored.
	//
	// It is NOT Calls: one call can keep twenty memories, and a figure with the
	// two confused is off by an order of magnitude on exactly the searches that
	// worked.
	Kept int
	// Scored is how many of the admitted pairs a run reached a verdict on, and it
	// is the DENOMINATOR of precision.
	//
	// The two genuinely differ, in two directions that both matter. A call can be
	// recorded and never judged — no lifecycle run yet, or a call older than the
	// judging window — which makes Kept the larger number. And a kept memory can
	// be deleted between the call and the run, which makes the verdict count the
	// smaller one. Dividing by Kept in either case would report a precision over
	// memories no verdict was ever filed against: a number that DROPS when an audit
	// is merely incomplete, which is backwards, because a fresh install with
	// searches but no lifecycle run would report 0% used and read as a verdict on
	// its corpus.
	Scored int
	// Used, Ignored, Superseded and Contradicted partition Scored. They do not sum
	// to Calls, for Kept's reason, and they do not necessarily sum to Kept, for
	// Scored's.
	Used         int
	Ignored      int
	Superseded   int
	Contradicted int
	// KeptNothing counts calls from this source that admitted no memory at all.
	// It is the DETECTABLE half of "missed": a lookup the agent made that
	// returned nothing. The other half is not countable and is reported as not
	// measured, never as zero.
	KeptNothing int
	// DegradedVerdicts is how many of this source's verdicts were filed under a
	// partial transcript read, and DegradedReasons names the reasons.
	//
	// Counted AND named rather than either alone: a degraded verdict is still a
	// verdict and stays in Kept, so a reader who sees the denominator without the
	// count reads a clean figure, and a reader who sees only the count cannot tell
	// which reasons produced it.
	DegradedVerdicts int
	DegradedReasons  []string
	// ContradictedIDs is every memory id this source's calls were judged to
	// contradict, deduplicated and sorted. Ids only: the renderer has no content
	// to print and no other field could reach it.
	ContradictedIDs []string
}

// NoRows reports whether this source has nothing to report at all.
//
// On Calls, not on Kept or Scored: the calls are the table's grain and the only
// figure that exists before any audit has run, so a source with recorded calls and
// no verdicts is NOT empty — it is unaudited, which the renderer says separately.
func (s SourceReport) NoRows() bool {
	return s.Calls == 0
}

// Unscored is how many admitted memories no verdict was filed against, clamped at
// zero.
//
// Clamped because the two counts can also disagree in the other direction — a kept
// memory deleted before the run could read it is a verdict that will never exist —
// and a negative "missing" would be read as a figure rather than as the absence of
// one.
func (s SourceReport) Unscored() int {
	if s.Kept <= s.Scored {
		return 0
	}
	return s.Kept - s.Scored
}

// Precision is used over SCORED, and ok is false when nothing was scored.
//
// ok is the second result rather than a sentinel like -1 or a 0%, because both of
// those are answers: "0% of nothing were used" and "-1" are claims a caller can
// print by forgetting to check. Here a caller has to ask.
func (s SourceReport) Precision() (percent int, ok bool) {
	if s.Scored == 0 {
		return 0, false
	}
	return s.Used * 100 / s.Scored, true
}

// PrecisionPercent is Precision for callers that have already established the
// source has rows, and reports 0 when it has not.
//
// It exists for the compact health line, which is a single line per source and has
// no room for a second shape — and it is the ONE place a zero stands for "not
// measured". That is a risk, so it is only reachable where the line has already
// established the source has rows.
func (s SourceReport) PrecisionPercent() int {
	percent, _ := s.Precision()
	return percent
}

// Report builds the figures for one project over one window.
//
// Read-only by construction: the two readers it uses are the store's, and neither
// writes. The caller opens the store read-only (see the CLI's
// openReadOnlyTransferStore) because this must not migrate a store whose schema is
// behind — a report that changed the thing it reported on would be worse than one
// that refused.
// BuildReport is Report's builder. It is named rather than called Report because
// the TYPE is what callers hold: `Report` the value is the artifact, `Report` the
// function would have been the act of making it, and one name for both reads as a
// redeclaration.
func BuildReport(ctx context.Context, store *memory.Store, opts ReportOptions) (Report, error) {
	if opts.ProjectID == "" {
		return Report{}, errNoReportProject
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	// The window's floor, computed once and applied to BOTH tables. Zero means no
	// floor, and a filter function with no floor returns true for every row —
	// including rows whose recorded_at is unreadable, which is the safe direction:
	// an unstamped row is kept rather than dropped from a report the operator did
	// not ask to lose anything.
	var floor time.Time
	if opts.Since > 0 {
		floor = now.Add(-opts.Since).UTC()
	}
	inWindow := func(stamp string) bool {
		if floor.IsZero() {
			return true
		}
		at, ok := memory.ParseStamp(stamp)
		if !ok {
			return true
		}
		return !at.Before(floor)
	}

	rep := Report{ProjectID: opts.ProjectID, Since: opts.Since}

	// The CALLS and the KEPT-NOTHING half, read off retrieval_record. Rows are
	// counted, never joined to the verdicts: #852 (an orphan record_rowid after a
	// history purge) means a join could attribute a verdict to the wrong call, and
	// this report has no need to join — both tables carry the source label
	// themselves.
	records, err := store.RetrievalRecordsForProject(ctx, opts.ProjectID, 0)
	if err != nil {
		return Report{}, fmt.Errorf("audit: read the recorded calls: %w", err)
	}
	bySource := map[string]*SourceReport{}
	source := func(name string) *SourceReport {
		s := bySource[name]
		if s == nil {
			s = &SourceReport{Source: name}
			bySource[name] = s
		}
		return s
	}
	// Every known source gets an entry before any row is counted, so an empty one
	// is reported rather than absent. See KnownSources.
	for _, name := range KnownSources {
		source(name)
	}
	for _, rec := range records {
		if !inWindow(rec.RecordedAt) {
			continue
		}
		s := source(rec.Source)
		s.Calls++
		// De-duplicated WITHIN the call, for the same reason Run does it: one
		// memory kept by two stages of one call is one (call, memory) pair, and
		// this table's grain is the pair.
		//
		// NOT deduplicated across calls, deliberately and for the same reason Run
		// leaves it alone: a memory two calls both kept is two pairs, which is the
		// grain every figure here is counted over. A window-wide dedupe would
		// understate Kept and break the correspondence with the verdict count.
		kept := map[string]bool{}
		for _, v := range rec.Verdicts {
			if v.Kept && !kept[v.ID] {
				kept[v.ID] = true
			}
		}
		s.Kept += len(kept)
		if len(kept) == 0 {
			s.KeptNothing++
		}
	}

	// The VERDICTS half, read off retrieval_audit. It lands in Scored and the four
	// buckets, and never in Kept: the records above already counted what was
	// admitted, and counting it again here would double it.
	rows, err := store.RetrievalAudits(ctx, opts.ProjectID, "")
	if err != nil {
		return Report{}, fmt.Errorf("audit: read the recorded verdicts: %w", err)
	}
	reasons := map[string]map[string]bool{}
	contradicted := map[string]map[string]bool{}
	for _, row := range rows {
		if !inWindow(row.RecordedAt) {
			continue
		}
		s := source(row.Source)
		s.Scored++
		switch Outcome(row.Outcome) {
		case OutcomeUsed:
			s.Used++
		case OutcomeIgnored:
			s.Ignored++
		case OutcomeSuperseded:
			s.Superseded++
		case OutcomeContradicted:
			s.Contradicted++
			ids := contradicted[row.Source]
			if ids == nil {
				ids = map[string]bool{}
				contradicted[row.Source] = ids
			}
			ids[row.MemoryID] = true
		default:
			// An outcome this build does not know — a store a later Ghost extended
			// with a bucket this one has no name for. It is counted in Scored and in
			// no bucket, so the buckets do not sum to Scored, and that gap is the
			// honest reading: the row IS a verdict, so it belongs in the
			// denominator, and this build cannot say which bucket it falls in.
			// Dropping it instead would report a stranger store's precision as
			// better than it is.
		}
		if row.Degraded != "" {
			s.DegradedVerdicts++
			set := reasons[row.Source]
			if set == nil {
				set = map[string]bool{}
				reasons[row.Source] = set
			}
			set[row.Degraded] = true
		}
	}

	for _, name := range sortedKeys(bySource) {
		s := *bySource[name]
		s.DegradedReasons = sortedKeys(reasons[name])
		s.ContradictedIDs = sortedKeys(contradicted[name])
		rep.Sources = append(rep.Sources, s)
	}
	// Known sources first, in their declared order, then anything the rows named
	// that this build does not know — so a hand-written or newer row is still
	// reported instead of being dropped by a closed list.
	rep.Sources = orderSources(rep.Sources)
	return rep, nil
}

// orderSources puts the known sources in KnownSources' order and appends the rest
// sorted.
//
// A closed list must not be able to HIDE rows, so an unrecognised source is
// reported too. It is placed after the known ones because a build reading a
// store a later Ghost wrote should see its own sources in the order it documents
// them and the extra one clearly below.
func orderSources(sources []SourceReport) []SourceReport {
	rank := map[string]int{}
	for i, name := range KnownSources {
		rank[name] = i
	}
	sort.SliceStable(sources, func(i, j int) bool {
		ri, iKnown := rank[sources[i].Source]
		rj, jKnown := rank[sources[j].Source]
		switch {
		case iKnown && jKnown:
			return ri < rj
		case iKnown:
			return true
		case jKnown:
			return false
		default:
			return sources[i].Source < sources[j].Source
		}
	})
	return sources
}

// Source returns one source's figures, or nil when the report has no entry for
// it.
//
// nil rather than a zero SourceReport, because the empty state and the absent one
// are the same question asked twice: an empty source means "nothing was recorded
// under this name", and a nil one means the caller asked for a source this report
// does not describe. Reporting them identically would make a typo'd source name
// read as a real measurement of nothing.
func (r Report) Source(name string) *SourceReport {
	for i := range r.Sources {
		if r.Sources[i].Source == name {
			return &r.Sources[i]
		}
	}
	return nil
}

// Pooled returns nil, always, and says why.
//
// It exists so that the rule is enforced rather than merely documented: a caller
// wanting one number over every source has a method to call, and this one cannot
// give them a number. A comment saying "never pool these" is a rule a later
// writer can talk themselves out of; a method that returns nil cannot be talked
// out of anything.
func (r Report) Pooled() *int { return nil }

// AddInto accumulates another source's figures into this one, for the same source.
//
// It exists so that a caller pooling over PROJECTS cannot get the addition wrong,
// and the fields that add are the ones that count. The id lists merge as sets
// because a memory two projects both contradicted is two findings and one id;
// counting it twice would print a duplicate under a line whose counts said once.
//
// This is the ONE permitted direction for combining figures, and the distinction is
// the whole rule: adding two reports of the same source over different projects is
// still one figure about one source, while adding two SOURCES is a number about
// neither question. Report.Pooled cannot be made to do the latter.
func (s *SourceReport) AddInto(other SourceReport) {
	s.Calls += other.Calls
	s.Kept += other.Kept
	s.Scored += other.Scored
	s.Used += other.Used
	s.Ignored += other.Ignored
	s.Superseded += other.Superseded
	s.Contradicted += other.Contradicted
	s.KeptNothing += other.KeptNothing
	s.DegradedVerdicts += other.DegradedVerdicts
	s.DegradedReasons = mergeNames(s.DegradedReasons, other.DegradedReasons)
	s.ContradictedIDs = mergeNames(s.ContradictedIDs, other.ContradictedIDs)
}

// mergeNames unions two sorted name lists, keeping the result sorted.
func mergeNames(a, b []string) []string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, name := range append(append([]string{}, a...), b...) {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// MergeProjects combines per-project reports into one per-source view, for a
// caller reporting over the whole store.
//
// It pools PROJECTS, never SOURCES, and that is the only difference from the rule
// above: a search is a search whichever project ran it, so summing two projects'
// search figures answers the question the per-source line is asking. The returned
// Report carries an empty ProjectID on purpose — it is not a report about any
// project, so nothing may print it as one — and its Sources are still one entry per
// source, in the same order, with the same known-source entries even when empty.
// Report.Pooled still returns nil.
//
// A source named only by some of the reports is still reported: a closed list must
// not hide rows, so an unrecognised source in any input survives the merge.
func MergeProjects(reports []Report) Report {
	bySource := map[string]*SourceReport{}
	for _, rep := range reports {
		for _, src := range rep.Sources {
			cur := bySource[src.Source]
			if cur == nil {
				// Copied, not aliased: the merge must not be able to reach back into
				// a caller's report and change it.
				entry := src
				entry.DegradedReasons = mergeNames(nil, src.DegradedReasons)
				entry.ContradictedIDs = mergeNames(nil, src.ContradictedIDs)
				bySource[src.Source] = &entry
				continue
			}
			cur.AddInto(src)
		}
	}
	merged := Report{}
	for _, name := range sortedKeys(bySource) {
		merged.Sources = append(merged.Sources, *bySource[name])
	}
	// The same ordering the single-project path uses, so a merged block and a
	// per-project report read identically and a known source stays first.
	return Report{Sources: orderSources(merged.Sources)}
}

// Summary renders one source as a SINGLE line, for a compact per-source block.
//
// It is line() without the wrapped sub-lines and without the degraded note, which
// is not the same thing: the sub-lines are two-line shapes and a block that claims
// one line per source has to keep that promise, and the degraded verdict is worth a
// warning line of its own in a tool that already has a glyph for one.
//
// The three shapes are the three from line(), and for the same reason — no rows is
// a source that never ran here, no verdict recorded yet is a source nobody has
// audited, and only the third is a measurement.
func (s SourceReport) Summary() string {
	label := assemble.Label(s.Source)
	if s.NoRows() {
		return fmt.Sprintf("%s: no rows — this source has recorded no calls", label)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d call(s), %d kept, ", label, s.Calls, s.Kept)
	if percent, ok := s.Precision(); ok {
		fmt.Fprintf(&b, "%d%% used (%d of %d scored), ", percent, s.Used, s.Scored)
	} else {
		fmt.Fprintf(&b, "no verdict recorded yet for the %d kept, ", s.Kept)
	}
	fmt.Fprintf(&b, "%d ignored, %d superseded in session, %d contradicted, %d kept nothing",
		s.Ignored, s.Superseded, s.Contradicted, s.KeptNothing)
	return b.String()
}

// String renders the report for an operator.
//
// It does NOT name the project, and that is a rule rather than an omission: the
// scope is the caller's to state, because a merged store-wide report (see
// MergeProjects) carries no project id at all, and a renderer that printed
// `r.ProjectID` unconditionally would label that one "retrieval audit for " — a
// line that reads as a report whose subject failed to load. The CLI states it
// (`ghost context --audit` prints "retrieval audit report for project …") and the
// figures below are identical either way.
//
// The lines that are not figures are the ones this report exists to get right:
// they are printed rather than returned as fields because they are properties of
// the METHOD, not data that can go stale, and a caller rendering figures could
// drop a field and keep the numbers.
func (r Report) String() string {
	var b strings.Builder
	if r.Since > 0 {
		fmt.Fprintf(&b, "retrieval audit — window: the last %s\n", r.Since)
	} else {
		b.WriteString("retrieval audit — window: everything the store still holds\n")
	}
	for _, src := range r.Sources {
		b.WriteString(src.line())
	}
	// The contradicted ids, grouped under the source that produced them, so a
	// reader can act on one source's findings without reading another's.
	for _, src := range r.Sources {
		if len(src.ContradictedIDs) == 0 {
			continue
		}
		fmt.Fprintf(&b, "  contradicted (%s):\n", assemble.Label(src.Source))
		for _, id := range src.ContradictedIDs {
			fmt.Fprintf(&b, "    %s\n", assemble.Token(id))
		}
	}
	if len(r.Sources) > 1 {
		b.WriteString("  figures are per source and are never pooled: a search and an injection " +
			"answer different questions\n")
	}
	b.WriteString("  \"ignored\" means the agent's own words never mentioned the memory; " +
		"it is not a relevance or usefulness score\n")
	// The NOT MEASURED half, said as such. It is the line most likely to be
	// misread as a number, because every other line here is one.
	b.WriteString("  missed: searches that kept nothing are counted above; the other half of \"missed\" " +
		"— a fact the agent re-derived in-session and was never shown — is not measured and is " +
		"reported as no figure at all: no heuristic can tell one from a fact it worked out, so read " +
		"no number into it\n")
	for _, src := range r.Sources {
		if src.DegradedVerdicts == 0 {
			continue
		}
		fmt.Fprintf(&b,
			"  %s: %d of its %d verdict(s) were judged against a partly-read transcript (%s), so an ignored verdict there is a claim about the text that was read\n",
			assemble.Label(src.Source), src.DegradedVerdicts, src.Kept, strings.Join(src.DegradedReasons, ", "))
	}
	return b.String()
}

// line renders one source, and the empty and the unaudited states are each their
// own shape.
//
// Three shapes rather than one with zeros in it, because they are three different
// facts and only one of them is a measurement. No rows is a source that never ran
// here. No verdicts is a source that ran and has not been audited — which is every
// source on a store whose lifecycle has not judged a session yet, and is NOT a
// finding. The figures themselves are the third shape. A line of zeroes would make
// all three read as a precision the report does not have.
func (s SourceReport) line() string {
	label := assemble.Label(s.Source)
	if s.NoRows() {
		return fmt.Sprintf("  %s: no rows — this source has recorded no calls in this window\n", label)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  %s: %d call(s), %d kept, ", label, s.Calls, s.Kept)
	if percent, ok := s.Precision(); ok {
		fmt.Fprintf(&b, "%d%% used (%d of %d scored), ", percent, s.Used, s.Scored)
	} else {
		fmt.Fprintf(&b, "no verdict recorded yet for the %d kept, ", s.Kept)
	}
	fmt.Fprintf(&b, "%d ignored, %d superseded in session, %d contradicted, %d kept nothing\n",
		s.Ignored, s.Superseded, s.Contradicted, s.KeptNothing)
	if unscored := s.Unscored(); unscored > 0 {
		fmt.Fprintf(&b,
			"    %d of the %d kept memory/memories have no verdict recorded, so they are in no bucket and in no percentage\n",
			unscored, s.Kept)
	}
	if s.DegradedVerdicts > 0 {
		fmt.Fprintf(&b, "    %d of those verdicts are degraded (%s)\n",
			s.DegradedVerdicts, strings.Join(s.DegradedReasons, ", "))
	}
	return b.String()
}
