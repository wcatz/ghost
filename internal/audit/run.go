package audit

// The runner: read the calls this session made, judge each memory it kept, and
// persist the verdicts.
//
// It is deliberately the ONLY place that touches the store on this path, and it
// is a detached process. The stop hook's own path is synchronous and does not
// open a database at all — it reads the transcript, writes a sidecar of
// fingerprints, and returns. Everything here is therefore off the agent's
// critical path: a failure costs a hole in the report, never a slow turn, and
// every error path returns rather than retries.
//
// What comes OUT is ids, counts and a fixed vocabulary. What went IN — the
// transcript's words, the query, a memory's content — is read, compared against
// fingerprints and dropped. There is no field on Summary that text could reach,
// which is why TestRunStandsOnWhatItReads can assert it on the printed report
// rather than on a type.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)

// errNoProject and errNoSignals are the two inputs a run cannot do anything with.
//
// Refused rather than defaulted, and for the reason retrieval_record's own project
// refusal gives: a run with no project would persist rows filed under nothing and
// report figures about every project at once, which is a claim about no call. A
// run with no signals would judge every kept memory against an empty transcript
// and file the lot as ignored — a confident report saying the agent used nothing,
// produced by a caller that failed to read the transcript at all.
var (
	errNoProject = errors.New("a project id is required")
	errNoSignals = errors.New("there are no signals to compare against")
)

// CallWindow is how many recent calls a run judges.
//
// A package var because the number is a POLICY — how much of the recent past one
// turn's audit is responsible for — and a policy is what a test has to be able
// to change to assert it. It is also the reason Run is idempotent: the second
// turn re-judges the same call, and the write replaces the verdicts it judged
// rather than adding to them, so a long session's table grows with its calls
// rather than with its turns.
//
// The window is bounded rather than "everything", because the transcript the
// signals come from is one turn's: a memory from a call the agent made before
// the transcript begins has no evidence either way, and judging it against
// words the agent wrote afterwards would be a claim about a conversation that is
// not the one this verdict belongs to.
var CallWindow = 50

// Summary is what one run found, in a form a report can print without reading
// the store.
//
// Verdicts is a COUNT and VerdictList is the list, and they are separate fields
// because they answer different questions: the count is what a per-project
// figure is made of, and the list is what the contradicted view is made of. A
// caller that only wants the number should not have to walk the list, and a
// caller that wants the ids should not have to make them from counts.
type Summary struct {
	ProjectID string
	// Sources holds one entry per source seen, and never a pooled total. A search
	// and a session-start injection answer different questions — "did the agent
	// use what it looked up" against "did the agent use what it was handed" — and
	// a ratio over both of them is a number about neither.
	Sources []SourceSummary
	// Verdicts is how many memories were judged in total.
	Verdicts int
	// VerdictList is every verdict, in the order the calls were read (newest
	// call first).
	VerdictList []Verdict
	// Unreadable counts kept memories that no longer exist. They are counted
	// rather than dropped because a report that silently omits them reads as a
	// clean run, and this is precisely the state an operator is in when they
	// need the report: memories deleted since the call.
	Unreadable int
	// Unfiled counts verdicts this run judged and the store then REFUSED, because
	// the call that admitted the memory had been purged since the run read it
	// (#852). They are counted rather than dropped for the same reason Unreadable
	// is: every other figure here is subtracted from them, so a run that silently
	// lost a pair would print a smaller total and no indication that it did — the
	// same shape as a clean run, and indistinguishable from one. The report's
	// figures describe what the table HOLDS; this is the difference between that
	// and what the run judged, stated rather than absorbed.
	Unfiled int
	// Degraded is the scanner's reason for a partial read, or "". Every verdict
	// in this run is about the transcript as far as it was read, so this rides
	// with the run and with each stored row.
	Degraded string
}

// placed is one verdict this run reached, with the call it belongs to and the
// surface that call came from. It is the run's own ordered list of what it is
// about to file, and dropUnfiled reconciles it against what the store actually
// stored — so it is a package type rather than a local one.
type placed struct {
	verdict Verdict
	record  int64
	source  string
	sess    string
}

// SourceSummary is one source's figures, and its denominator is CALLS.
type SourceSummary struct {
	Source string
	// Calls is how many recorded calls came from this source, including the ones
	// that kept nothing — a lookup that returned nothing is a lookup worth
	// auditing, which is why the grain of retrieval_record is the call.
	Calls int
	// Used, Ignored, Superseded and Contradicted count VERDICTS, not calls: one
	// call can keep twenty memories, so these do not sum to Calls.
	Used         int
	Ignored      int
	Superseded   int
	Contradicted int
	// KeptNothing counts the calls this source made that admitted no memory at
	// all. It is the detectable half of the issue's "missed": a lookup the agent
	// made that returned nothing it could use.
	KeptNothing int
}

// Run judges the calls this session made and persists what it found.
//
// Fail-open by construction at the two places it can fail: a read that errors
// returns the error to a detached child that logs it and exits successfully, and a
// verdict about a memory that no longer exists is counted as unreadable rather
// than filed. Nothing here is on the agent's critical path, so there is no
// version of this that should block a turn.
func Run(ctx context.Context, store *memory.Store, projectID string, s *Signals) (Summary, error) {
	if projectID == "" {
		return Summary{}, fmt.Errorf("audit: %w", errNoProject)
	}
	if s == nil {
		return Summary{}, fmt.Errorf("audit: %w", errNoSignals)
	}
	res := Summary{ProjectID: projectID}
	if reason, ok := s.Degraded(); ok {
		res.Degraded = reason
	}

	records, err := store.RetrievalRecordsForProject(ctx, projectID, CallWindow)
	if err != nil {
		return res, fmt.Errorf("audit: read retrieval records: %w", err)
	}

	// Judge per call, so a memory's verdict is filed against the call it belongs
	// to. That is what makes the write idempotent: the replacement is keyed by
	// the call's row, so a re-run replaces exactly the verdicts it judged before
	// and leaves every other call's alone.
	var kept []placed
	bySource := map[string]*SourceSummary{}

	for _, rec := range records {
		sum := bySource[rec.Source]
		if sum == nil {
			sum = &SourceSummary{Source: rec.Source}
			bySource[rec.Source] = sum
		}
		sum.Calls++

		// Only KEPT memories reached the agent, so only a kept one can have been
		// used, ignored, superseded or contradicted. A dropped row was never
		// shown, and judging it would report a retrieval failure that happened to
		// nobody.
		//
		// De-duplicated WITHIN this call, and deliberately not across calls: a
		// memory two calls both kept is two (call, memory) pairs, which is this
		// table's grain and the only thing that keeps each source's denominator
		// right. A run-wide dedupe would also have made the second call report
		// KeptNothing — "a lookup that admitted no memory at all" — about an
		// injection that admitted one, which is the opposite of what it did.
		// Idempotence does not need it: re-judging a call REPLACES that call's rows.
		ids := make([]string, 0, len(rec.Verdicts))
		withinCall := make(map[string]bool, len(rec.Verdicts))
		for _, v := range rec.Verdicts {
			if !v.Kept || withinCall[v.ID] {
				continue
			}
			withinCall[v.ID] = true
			ids = append(ids, v.ID)
		}
		if len(ids) == 0 {
			sum.KeptNothing++
			continue
		}

		mems, err := store.GetByIDs(ctx, ids)
		if err != nil {
			return res, fmt.Errorf("audit: read the memories a call kept: %w", err)
		}
		content := make(map[string]string, len(mems))
		for _, m := range mems {
			content[m.ID] = m.Content
		}
		judged := make([]Judged, 0, len(ids))
		for _, id := range ids {
			c, ok := content[id]
			if !ok {
				res.Unreadable++
				continue
			}
			judged = append(judged, Judged{MemoryID: id, Content: c})
		}
		for _, v := range Compare(s, judged) {
			res.Verdicts++
			res.VerdictList = append(res.VerdictList, v)
			kept = append(kept, placed{verdict: v, record: rec.RowID, source: rec.Source, sess: rec.SessionID})
			sum.count(v.Outcome)
		}
	}

	// Sorted by source so a report reads the same way twice, and so a test's
	// expectations do not depend on the order the calls happened to be read in.
	for _, name := range sortedKeys(bySource) {
		res.Sources = append(res.Sources, *bySource[name])
	}

	rows := make([]memory.RetrievalAuditRow, 0, len(kept))
	for _, p := range kept {
		rows = append(rows, memory.RetrievalAuditRow{
			ProjectID:   projectID,
			RecordRowID: p.record,
			SessionID:   p.sess,
			Source:      p.source,
			MemoryID:    p.verdict.MemoryID,
			Outcome:     string(p.verdict.Outcome),
			Signal:      string(p.verdict.Signal),
			Degraded:    res.Degraded,
		})
	}
	if len(rows) > 0 {
		refused, err := store.RecordRetrievalAudits(ctx, rows)
		if err != nil {
			return res, fmt.Errorf("audit: record verdicts: %w", err)
		}
		// Every figure above was counted from what this run JUDGED, and the store
		// may have stored less: it refuses a verdict whose call was purged after
		// the run read it. The sources are rebuilt from the corrected map so the
		// printed per-source lines carry the correction too, not just the total.
		res.dropUnfiled(kept, bySource, refused)
		for i := range res.Sources {
			if sum := bySource[res.Sources[i].Source]; sum != nil {
				res.Sources[i] = *sum
			}
		}
	}
	return res, nil
}

// count files one verdict under its bucket.
func (s *SourceSummary) count(o Outcome) {
	switch o {
	case OutcomeUsed:
		s.Used++
	case OutcomeIgnored:
		s.Ignored++
	case OutcomeSuperseded:
		s.Superseded++
	case OutcomeContradicted:
		s.Contradicted++
	}
}

// uncount takes one back out, and is count's exact inverse. A verdict the store
// refused was counted here before the write and is not in the table after it, so
// leaving it would make this bucket a figure about a row that does not exist.
func (s *SourceSummary) uncount(o Outcome) {
	switch o {
	case OutcomeUsed:
		s.Used--
	case OutcomeIgnored:
		s.Ignored--
	case OutcomeSuperseded:
		s.Superseded--
	case OutcomeContradicted:
		s.Contradicted--
	}
}

// unfiledKey identifies one (call, memory) pair — the grain both the table and
// the report count in. It is a key rather than a whole row because the store
// returns the row it refused, and the pair is what has to be matched back.
type unfiledKey struct {
	record int64
	memory string
}

// dropUnfiled takes the verdicts the store REFUSED back out of every figure this
// run built, and counts them.
//
// The store refuses a verdict whose call was purged after the run read it, and it
// returns exactly which rows it refused (#852). Without this, `Run` counts each
// verdict as it judges it — into Verdicts, into VerdictList and into the per-source
// buckets — and then writes them, and a refusal disappears inside the write. The
// printed report then claims a number the table does not hold, with nothing
// anywhere recording that the difference was taken: a hole nobody can see is not
// an honest hole, it is a wrong number, and it is indistinguishable from a clean
// run to the one reader who has to act on it. Every figure the report prints
// describes what the table HOLDS; the loss is reported beside them, not folded
// into them.
//
// `kept` is the run's own ordered list and is parallel to res.VerdictList, so a
// refusal is matched back by (call, memory) and taken out of both. The pair is
// unique within a batch — a call judges each memory at most once, and calls hold
// distinct rowids — so the match is exact rather than first-come.
func (r *Summary) dropUnfiled(kept []placed, bySource map[string]*SourceSummary, refused []memory.RetrievalAuditRow) {
	if len(refused) == 0 {
		return
	}
	r.Unfiled = len(refused)

	gone := make(map[unfiledKey]bool, len(refused))
	for _, row := range refused {
		gone[unfiledKey{record: row.RecordRowID, memory: row.MemoryID}] = true
	}
	dropped := make([]bool, len(kept))
	for i, p := range kept {
		if !gone[unfiledKey{record: p.record, memory: p.verdict.MemoryID}] {
			continue
		}
		dropped[i] = true
		if sum := bySource[p.source]; sum != nil {
			sum.uncount(p.verdict.Outcome)
		}
	}
	// Kept in order, because VerdictList's order is the order the calls were read
	// and a caller reading it positionally against anything else would otherwise
	// find a silent reordering rather than a shorter list.
	list := make([]Verdict, 0, len(kept)-len(refused))
	for i, p := range kept {
		if !dropped[i] {
			list = append(list, p.verdict)
		}
	}
	r.VerdictList = list
	r.Verdicts = len(list)
}

// String renders the summary for an operator, and states on its face the three
// things a reader would otherwise have to guess: that "ignored" is not a
// usefulness score, that the sources are separate denominators, and that one of
// the two halves of "missed" cannot be counted by any heuristic.
//
// The limits are printed rather than returned as fields because they are not
// numbers that can go stale: a count is data and a limit is a property of the
// method, and a caller rendering figures could drop one field and keep the other.
// A line of prose cannot be dropped by accident, because it is the same string
// every time.
func (r Summary) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "retrieval audit for %s\n", r.ProjectID)
	for _, src := range r.Sources {
		fmt.Fprintf(&b, "  %s: %d call(s), %d used, %d ignored, %d superseded in session, "+
			"%d contradicted, %d kept nothing\n",
			src.Source, src.Calls, src.Used, src.Ignored, src.Superseded, src.Contradicted, src.KeptNothing)
	}
	if len(r.Sources) > 1 {
		b.WriteString("  figures are per source and are never pooled: a search and an injection " +
			"answer different questions\n")
	}
	fmt.Fprintf(&b, "  %d verdict(s) over the memories those calls kept\n", r.Verdicts)
	if r.Unreadable > 0 {
		fmt.Fprintf(&b, "  %d kept memory/memories no longer exist and were not judged\n", r.Unreadable)
	}
	if r.Unfiled > 0 {
		// Printed, not folded into the counts above: the figures describe the
		// table, and this is what is missing from it. Naming the reason is the
		// point — a pair lost to a purge is not a lost pair, and an operator
		// reading a shortfall needs to know which of the two they are looking at.
		fmt.Fprintf(&b, "  %d verdict(s) were not filed because the call that admitted the memory was "+
			"purged after this run read it, so the figures above count %d stored verdict(s) and not the "+
			"%d judged\n", r.Unfiled, r.Verdicts, r.Verdicts+r.Unfiled)
	}
	if r.Degraded != "" {
		fmt.Fprintf(&b, "  the transcript was only partly read (%s), so an ignored verdict is a claim "+
			"about the text that was read\n", r.Degraded)
	}
	b.WriteString("  \"ignored\" means the agent's own words never mentioned the memory; " +
		"it is not a relevance or usefulness score\n")
	b.WriteString("  a fact the agent re-derived in-session that was never injected is not counted: " +
		"no heuristic can tell one from a fact it worked out, so only searches that kept " +
		"nothing are reported as missed\n")
	return b.String()
}

// sortedKeys is the map iteration made deterministic.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
