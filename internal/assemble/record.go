package assemble

// #646 part 1: the seam between the assembler and the retrieval record.
//
// The record is written from HERE and not by the surface, because the verdicts it
// needs exist only inside a Run. Result.Items is the answer AFTER the
// response-fit post-pass has possibly dropped rows from it, and Trace.Decisions
// is the only place the stages recorded why. A surface that re-derived either
// would be a second implementation of the same rules — the failure mode
// Result.Leaks() and Trace.TrimmedByBudget() exist to prevent, which is why
// those two are described in this package as the ONLY way a consumer learns
// which rows it should not have carried.
//
// Three things are deliberately not here. The record carries no query text, so
// the digest is computed here and the type cannot express a question. The record
// is not written for a Run that returned an error, because an error is not an
// answer and a record for one would claim a set of retrieved memories for a call
// that returned none. And a failure to record never fails the search, because a
// record is a measurement of a call that already happened: making recording
// capable of failing the call would let the act of measuring retrieval change
// retrieval's availability, at exactly the moments the store is unhealthy that a
// report is most wanted.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"

	"github.com/wcatz/ghost/internal/memory"
)

// RecordSink receives the retrieval record for a successful Run. It is the
// request's own field rather than a package global, so two Runs on one process
// cannot record into each other's store and a caller that wants no record simply
// leaves it nil.
//
// *memory.Store satisfies it, which is the whole reason the record type lives in
// internal/memory: the assembler's own DTOs are declared there because a method
// implemented by *memory.Store cannot name a type from a package that imports
// memory, and the sink's argument has to be the row the store writes.
type RecordSink interface {
	RecordRetrieval(ctx context.Context, rec memory.RetrievalRecord) error
}

// record projects a finished Result onto the row the store writes.
//
// Kept rows come from res.Items — the answer the caller received, post-fit — and
// dropped rows from the trace's decisions, which is the only record of why. The
// two are merged on one rule: a row in the answer is KEPT, and a keep the trace
// recorded for it (validity_unparseable is the one v1 has) supplies the reason.
// A row the window CUT is absent from both and is deliberately not recorded: it
// was never judged, and a record claiming otherwise would put a verdict in the
// audit for a decision nobody made.
//
// The two halves are ordered kept-then-dropped, which is the order a reader wants
// to see them in and is stable for the same call's ranking.
func record(req Request, res Result) memory.RetrievalRecord {
	var decisions []Decision
	asOf := ""
	if res.Trace != nil {
		decisions = res.Trace.Decisions
		// The trace's own record of the binding newTrace performed, read rather
		// than reformatted here: a second formatting would be a second answer to
		// a question the trace has already answered once.
		asOf = res.Trace.AsOf
	}

	decided := make(map[string]memory.RowVerdict, len(decisions))
	for _, d := range decisions {
		// Later decisions win. A row can be kept at one stage with a reason
		// recorded and dropped at the next; its fate is the last thing decided
		// about it, and an earlier entry is a stage's opinion rather than a
		// verdict.
		decided[d.ID] = memory.RowVerdict{ID: d.ID, Kept: d.Kept, Stage: d.Stage, Reason: d.Reason}
	}

	kept := make(map[string]bool, len(res.Items))
	dropped := make(map[string]bool, len(decided))
	out := make([]memory.RowVerdict, 0, len(res.Items)+len(decided))
	for _, it := range res.Items {
		kept[it.ID] = true
		// The answer is the authority on membership, and a reason is carried
		// across only when the trace's own entry for this row is a keep's.
		if v, ok := decided[it.ID]; ok && v.Kept {
			v.ID = it.ID
			out = append(out, v)
			continue
		}
		out = append(out, memory.RowVerdict{ID: it.ID, Kept: true})
	}
	for _, d := range decisions {
		// Only a DROP, and one row once: the merged map collapses repeats, and a
		// row the answer carries cannot also be reported as removed.
		if d.Kept || kept[d.ID] || dropped[d.ID] {
			continue
		}
		dropped[d.ID] = true
		v := decided[d.ID]
		// A drop followed by a keep is a row the stages admitted after all — the
		// merged verdict is the one that decides this, not the decision being
		// walked past.
		if v.Kept {
			continue
		}
		out = append(out, v)
	}
	return memory.RetrievalRecord{
		ProjectID: req.ProjectID,
		SessionID: req.SessionID,
		Source:    string(req.Source),
		QueryHash: queryHash(req.Query),
		AsOf:      asOf,
		Outcome:   string(res.Outcome),
		Reason:    res.Reason,
		Verdicts:  out,
	}
}

// queryHash is the caller's question, reduced to a digest.
//
// A record whose type could hold a query would eventually hold one — a report
// that wanted the text is a perfectly reasonable next request, and the column is
// right there. Making the text impossible to express is the cheaper half of the
// privacy rule than remembering not to. The query is hashed and the ORIGINAL is
// not retained, so the digest groups calls by "the same question was asked",
// which is what the report needs, without the record holding a question.
//
// An EMPTY query is an empty hash rather than the digest of the empty string: a
// constant that looks like a question's fingerprint is worse than an honest
// "this call carried no query", and a session-start injection is exactly that.
func queryHash(q string) string {
	if q == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(q))
	return hex.EncodeToString(sum[:])
}

// emit writes the record and swallows the failure.
//
// Log-and-continue is the whole contract, and the log line matters as much as the
// swallow: a record that silently stops being written produces a report that is
// quietly wrong rather than one that is obviously missing.
//
// It goes to the CALLER's logger, which is the only one that reaches an operator
// in production. Nothing in Ghost calls slog.SetDefault — `ghost mcp` builds its
// own in bootstrap and passes it down, and every process that DOES set a default
// installs a discard handler — so a warning sent to slog.Default() from a live
// server goes to a handler nobody reads, and "logged, not silent" would be true
// only in tests. nil falls back to the default, which is the right behaviour for a
// caller with no logger of its own.
func emit(ctx context.Context, sink RecordSink, req Request, res Result) {
	if sink == nil {
		return
	}
	log := req.Logger
	if log == nil {
		log = slog.Default()
	}
	rec := record(req, res)
	if err := sink.RecordRetrieval(ctx, rec); err != nil {
		log.Warn("retrieval record not written; the audit will be missing this call",
			"project_id", rec.ProjectID, "source", rec.Source,
			"outcome", rec.Outcome, "error", err)
	}
}
