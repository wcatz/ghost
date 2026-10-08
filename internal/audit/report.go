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
//   - Precision is used / SCORED, and scored counts VERDICTS, not calls and not
//     kept memories. One call can keep twenty, so a call denominator would
//     understate every figure; and a kept memory no run has judged yet is not a
//     denominator either, because a fresh install with searches but no lifecycle
//     run would then report 0% used and read as a verdict on its corpus.
//   - A source with no rows says "no rows" and reports no percentage. "0% used"
//     about a source that has never run is a measurement of nothing, and it is
//     the reading a fresh install gets first.
//   - Every known source is named even when it is empty, so a source this build
//     knows about and a source it does not can be told apart. A project that has
//     only ever searched has no session_start or project_context row at all, and a
//     report built only from the rows present would look like a healthy one rather
//     than like two sources that have never been measured.
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
// --since has only recorded_at to work with, on BOTH tables, and the two columns
// are NOT the same instant: retrieval_record is stamped when the call happened and
// retrieval_audit when the detached run judged it, so a call at 23:50 judged at
// 00:05 splits across any window boundary. Filtering both by one floor is
// therefore two populations and not one, and the report divides a numerator from
// one clock by a denominator from the other — a source line reading "0 kept, 100%
// used (1 of 1 scored)". So the verdict half is intersected with the rowids of
// the calls this report COUNTS, and a verdict naming any other rowid is counted
// and named as Detached rather than dropped in silence.

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
// the empty state matters most: a project that has only ever searched records no
// session_start or project_context call, and a report built only from the rows
// present would say nothing about them. Naming them with "no rows" is what
// distinguishes "this source has never run here" from "this build cannot see it".
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
	Used int
	// UsedByID and UsedByWording split Used by what proved it and sum to it: a cited
	// id (signal = identifier) against anything weaker, a token overlap or a signal
	// this build cannot classify. Precision is the first.
	UsedByID      int
	UsedByWording int
	Ignored       int
	Superseded    int
	Contradicted  int
	// KeptNothing counts calls from this source that admitted no memory at all.
	// It is the DETECTABLE half of "missed" — but only for a source where the
	// AGENT chose to look, which is what makes the renderer name it per source
	// (see keptNothingName): a search's is a lookup that returned nothing, and an
	// injection's is Ghost having offered something the fit stage took none of,
	// which is a normal session start and not a failed retrieval. The other half
	// of "missed" is not countable and is reported as not measured, never as zero.
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
	// Unattributed is how many of this source's verdicts name NO CALL at all
	// (record_rowid = 0, a value the write accepts for a verdict about a session
	// rather than about one call).
	//
	// Counted rather than dropped, because there is no call to be in or out of a
	// window with and the verdict is real evidence about real agent text — and NAMED
	// rather than counted quietly, because it is in NO other figure: precision is a
	// ratio over (call, memory) pairs, and a verdict with no call has no pair to
	// belong to, so putting it in Scored would make the numerator and the
	// denominator two different populations.
	//
	// It is therefore NOT a way Scored can exceed Kept — the report never reports a
	// numerator above its denominator.
	Unattributed int
	// Unscoped is how many of this source's verdicts name a call but no SESSION.
	// Every verdict filed before the audit was session-scoped is one: the run that
	// filed it compared the call with whichever session the stop hook had scanned,
	// not the session that made the call, so it says nothing about the call it
	// names. Counted and named, in no figure above (Scored, Used, Ignored and the
	// rest), and never deleted from the store.
	Unscoped int
	// Detached is how many of this source's verdicts were LEFT OUT because the call
	// they belong to is not one this report counts: outside the window, or no longer
	// held (the call cap evicts at 5000 rows while the verdict cap holds 50000, and
	// a history purge removes a call's rows without every path removing its verdicts
	// — #857).
	//
	// The two tables are stamped at TWO different instants — retrieval_record when
	// the call happened, retrieval_audit when the detached run judged it — so one
	// window filter over both columns is two populations and not one. Without this
	// intersection the report divides a numerator from one clock by a denominator
	// from another, and prints a source line reading "0 kept, 100% used (1 of 1
	// scored)".
	//
	// Counted and named rather than dropped in silence: these are real verdicts, and
	// a report that quietly loses them is indistinguishable from a report over a
	// store where they were never written.
	Detached int
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

// LimitsSentence is the caveat every surface that prints these figures carries, spelled
// once so the report, the run summary and ghost_health cannot drift apart.
const LimitsSentence = "\"ignored\" means the agent's own words never mentioned the memory, " +
	"and \"restated by wording\" is a token-overlap heuristic; neither is a relevance or usefulness score"

// Precision is the share of SCORED verdicts whose `used` was proved by a cited id
// (signal = identifier), and ok is false when nothing was scored. Wording overlap is
// reported beside it as a heuristic and is never in this figure.
//
// ok is the second result rather than a sentinel like -1 or a 0%, because both of
// those are answers: "0% of nothing were used" and "-1" are claims a caller can
// print by forgetting to check. Here a caller has to ask.
func (s SourceReport) Precision() (percent int, ok bool) {
	if s.Scored == 0 {
		return 0, false
	}
	return s.UsedByID * 100 / s.Scored, true
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
	// The rowids of the calls this report COUNTS, which is what the verdict half is
	// intersected with below. Collected here rather than in the verdict loop because
	// it is a property of the records read, and a verdict naming any other rowid is a
	// verdict about a call this report is not reporting on.
	counted := map[int64]bool{}
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
		if rec.RowID > 0 {
			counted[rec.RowID] = true
		}
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
		// The intersection, and the two ways a verdict does not survive it. Being in
		// the window is NOT enough: the two tables are stamped at different instants,
		// so a verdict judged minutes after its call falls into a window its call is
		// not in (see SourceReport.Detached).
		//
		// And an UNATTRIBUTED verdict is counted and named but is in NO figure below.
		// It used to land in Scored, which put it in the numerator and the
		// denominator of a precision whose other term is a count of (call, memory)
		// pairs — a ratio of two populations under one name, so a store with sessions
		// that filed session-level verdicts reported a precision about a
		// (call, memory) corpus that its own numerator was not part of.
		switch {
		case row.RecordRowID <= 0:
			s.Unattributed++
			continue
		case row.SessionID == "":
			s.Unscoped++
			continue
		case !counted[row.RecordRowID]:
			s.Detached++
			continue
		}
		s.Scored++
		switch Outcome(row.Outcome) {
		case OutcomeUsed:
			s.Used++
			if row.Signal == string(SignalIdentifier) {
				s.UsedByID++
			} else {
				s.UsedByWording++
			}
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

// BuildStoreReport is BuildReport's whole-store sibling: the same figures, over every
// project at once, for the surfaces that report the store rather than a project.
//
// It exists because the obvious composition — BuildReport per project, summed in Go —
// makes the cost of a health check grow with the number of checkouts on the machine,
// over a pool of exactly one connection. This is one aggregate per table instead (see
// memory.RetrievalSourceTotals), so the work does not depend on how many projects are
// registered, and it REPLACED the summing function rather than joining it: a shipped
// combiner with no caller is how the next reader concludes it is the sanctioned way to
// pool, which is exactly the thing this package refuses to be able to do.
//
// It pools PROJECTS within a source, never sources with each other, and the arithmetic
// is SUM-of-counts and never an average of percentages — a project with one verdict
// counts as much as one with three hundred.
//
// The scope is the whole store and the returned Report says so on its face: it carries
// no project id, because there is no project to name. What the caller renders with it
// is the caller's decision — Summary() for the compact health line, String() for a
// report a human reads.
func BuildStoreReport(ctx context.Context, store *memory.Store, opts ReportOptions) (Report, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	var floor time.Time
	if opts.Since > 0 {
		floor = now.Add(-opts.Since).UTC()
	}
	totals, err := store.RetrievalSourceTotals(ctx, floor)
	if err != nil {
		return Report{}, fmt.Errorf("audit: read the retrieval totals: %w", err)
	}

	rep := Report{Since: opts.Since}
	// Every known source gets an entry before any row is counted, so an empty one is
	// REPORTED rather than absent — the same rule the per-project path holds, and for
	// the same reason.
	for _, name := range KnownSources {
		rep.Sources = append(rep.Sources, SourceReport{Source: name})
	}
	index := map[string]int{}
	for i := range rep.Sources {
		index[rep.Sources[i].Source] = i
	}
	for _, t := range totals {
		i, seen := index[t.Source]
		if !seen {
			index[t.Source] = len(rep.Sources)
			rep.Sources = append(rep.Sources, SourceReport{Source: t.Source})
			i = len(rep.Sources) - 1
		}
		rep.Sources[i] = SourceReport{
			Source:           t.Source,
			Calls:            t.Calls,
			Kept:             t.Kept,
			KeptNothing:      t.KeptNothing,
			Scored:           t.Scored,
			Used:             t.Used,
			UsedByID:         t.UsedByID,
			UsedByWording:    t.UsedByWording,
			Ignored:          t.Ignored,
			Superseded:       t.Superseded,
			Contradicted:     t.Contradicted,
			ContradictedIDs:  t.ContradictedIDs,
			DegradedVerdicts: t.DegradedVerdicts,
			DegradedReasons:  t.DegradedReasons,
			Unattributed:     t.Unattributed,
			Unscoped:         t.Unscoped,
			Detached:         t.Detached,
		}
	}
	rep.Sources = orderSources(rep.Sources)
	return rep, nil
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
		fmt.Fprintf(&b, "%d%% cited by id (%d of %d scored), %d restated by wording (heuristic), ",
			percent, s.UsedByID, s.Scored, s.UsedByWording)
	} else {
		fmt.Fprintf(&b, "no verdict recorded yet for the %d kept, ", s.Kept)
	}
	fmt.Fprintf(&b, "%d ignored, %d superseded in session, %d contradicted, %d %s",
		s.Ignored, s.Superseded, s.Contradicted, s.KeptNothing, keptNothingName(s.Source))
	return b.String()
}

// keptNothingName is the PHRASE a source's empty-call count is rendered under, and it
// is per source because the two renderers that print the figure are both followed by a
// sentence saying that a search's is the detectable half of "missed".
//
// One name for both would put `session_start: 40 call(s), 2 kept, ..., 30 kept
// nothing` under that sentence, and a reader — or an agent reading the health block —
// tallying missed retrievals would add an injection's silence, which is what a healthy
// session start looks like, to a count of failed lookups. The count is the same; the
// name is what says which question it answers.
//
// search is the only source where the agent chose to look, and it keeps the original
// wording, so every sample in docs/ and every existing reading of the report is
// unchanged. Every other source is an injection Ghost made, and an injection that
// admitted nothing admitted nothing BY ITS OWN DESIGN.
func keptNothingName(source string) string {
	if source == string(assemble.SourceSearch) {
		return "kept nothing"
	}
	return "admitted nothing"
}

// String renders the report for an operator.
//
// It does NOT name the project, and that is a rule rather than an omission: the
// scope is the caller's to state, because a merged store-wide report (see
// BuildStoreReport) carries no project id at all, and a renderer that printed
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
	b.WriteString("  " + LimitsSentence + "\n")
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
		// The denominator is SCORED, not Kept. This sentence is about the verdicts
		// carrying the degraded caveat, and Kept is the larger number on every store
		// whose audit is merely incomplete — which would report a verdict count the
		// store does not hold, on the one line whose job is to say how much of the
		// denominator is trustworthy. health_retrieval.go divides by Scored for the
		// same reason, and two surfaces of one figure may not disagree.
		// labelDegraded mirrors health_retrieval.labelReasons: each stored reason goes
		// through assemble.Label so a newline in the column forges a token, not a
		// line of output.
		fmt.Fprintf(&b,
			"  %s: %d of its %d scored verdict(s) were judged against a partly-read transcript (%s), so an ignored verdict there is a claim about the text that was read\n",
			assemble.Label(src.Source), src.DegradedVerdicts, src.Scored, labelDegraded(src.DegradedReasons))
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
		// The attribution notes still print under a no-rows source: a verdict left
		// out of the figures is exactly what a reader of "no rows" needs to be told,
		// or the source reads as never having been measured when in fact it was
		// judged outside the window.
		return fmt.Sprintf("  %s: no rows — this source has recorded no calls in this window\n%s",
			label, s.attributionNotes())
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  %s: %d call(s), %d kept, ", label, s.Calls, s.Kept)
	if percent, ok := s.Precision(); ok {
		fmt.Fprintf(&b, "%d%% cited by id (%d of %d scored), %d restated by wording (heuristic), ",
			percent, s.UsedByID, s.Scored, s.UsedByWording)
	} else {
		fmt.Fprintf(&b, "no verdict recorded yet for the %d kept, ", s.Kept)
	}
	fmt.Fprintf(&b, "%d ignored, %d superseded in session, %d contradicted, %d %s\n",
		s.Ignored, s.Superseded, s.Contradicted, s.KeptNothing, keptNothingName(s.Source))
	if unscored := s.Unscored(); unscored > 0 {
		fmt.Fprintf(&b,
			"    %d of the %d kept memory/memories have no verdict recorded, so they are in no bucket and in no percentage\n",
			unscored, s.Kept)
	}
	if s.DegradedVerdicts > 0 {
		fmt.Fprintf(&b, "    %d of those verdicts are degraded (%s)\n",
			s.DegradedVerdicts, labelDegraded(s.DegradedReasons))
	}
	b.WriteString(s.attributionNotes())
	return b.String()
}

// attributionNotes says what became of this source's verdicts that the figures above
// do not account for, so no row is lost without a word and no figure is left to
// explain itself.
//
// Both notes are ABSENT rather than zero by default. A source whose verdicts are all
// attributed and counted has nothing to report about them, and two lines of "0
// verdicts were dropped" under every source of every store is noise that teaches a
// reader to skip the lines which matter.
func (s SourceReport) attributionNotes() string {
	var b strings.Builder
	if s.Detached > 0 {
		fmt.Fprintf(&b,
			"    %d verdict(s) were not counted: their call is outside this window, or the store no longer holds it (a history purge, or the call cap)\n",
			s.Detached)
	}
	if s.Unattributed > 0 {
		fmt.Fprintf(&b,
			"    %d verdict(s) name no call at all, so they are counted here and in no figure above: precision is a ratio over (call, memory) pairs, and these have no call to be one of\n",
			s.Unattributed)
	}
	if s.Unscoped > 0 {
		fmt.Fprintf(&b,
			"    %d verdict(s) carry no session, so they are counted here and in no figure above: they were filed before the audit was session-scoped, by a run that compared the call with a session that may not be the one that made it\n",
			s.Unscoped)
	}
	return b.String()
}

// AttributionTotals is the count of the verdicts a report's figures do not account
// for, summed over its sources.
//
// Exported so the health block can state the same two facts from the same arithmetic
// rather than keeping its own copy — a second renderer is how two surfaces of one
// figure come to disagree, which is how this report already had one (the degraded
// note's denominator).
func (r Report) AttributionTotals() (detached, unattributed int) {
	for _, src := range r.Sources {
		detached += src.Detached
		unattributed += src.Unattributed
	}
	return detached, unattributed
}

// UnscopedTotal is the count of verdicts, summed over the sources, that name a call but
// no session and so sit in none of the figures. Separate from AttributionTotals so that
// function's two-value shape, which its callers destructure, does not change.
func (r Report) UnscopedTotal() int {
	n := 0
	for _, src := range r.Sources {
		n += src.Unscoped
	}
	return n
}

// labelDegraded mirrors health_retrieval.labelReasons: each stored reason goes
// through assemble.Label so a newline in the column forges a token, not a line
// of output. The reasons come from the scanner's fail-open vocabulary, not from
// a transcript, but the column behind them is TEXT and this block is a warning
// line that another line could be forged under.
func labelDegraded(reasons []string) string {
	out := make([]string, 0, len(reasons))
	for _, r := range reasons {
		out = append(out, assemble.Label(r))
	}
	return strings.Join(out, ", ")
}
