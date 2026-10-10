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
	"log/slog"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// recordWriteBudget bounds the record write, separately from the caller's own
// context, and it is small on purpose.
//
// The measured uncontended cost is ~45us, so this is three orders of magnitude
// above the normal case and two below the store's 5s busy_timeout: a write that
// has not completed in a quarter of a second is not going to, and the only thing
// waiting longer buys is a search that has already been answered. The ten-process
// fleet puts a contended record write's p99 wait at ~179ms, so the budget sits
// just above what contention actually costs and well below the tail it must not
// inherit.
//
// Dropping the row is the right failure because the record is a measurement of a
// call that already happened: the answer is in the caller's hands either way, and
// a report with a hole in it is recoverable where a stalled tool is not. Every
// drop is logged, so the gap is visible rather than inferred.
//
// The ~45us is a store that has filed no verdicts. At the audit cap the same
// write also takes the evicted call's verdicts with it (#852), and that pairing
// is a sequential scan of retrieval_audit: ~5ms measured at
// retrievalAuditRowsCap, ~16us when the audit table is empty. It is inside this
// budget because it is inside this transaction, and the sweep's own reasoning —
// the cost, and why it is not moved to the audit pass — is at the cap eviction in
// internal/memory/retrieval_record.go.
const recordWriteBudget = 250 * time.Millisecond

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

	// DigestQuery returns the store's keyed digest of a caller's question, or ""
	// for a call that carried none.
	//
	// It is on the seam rather than a package call because the KEY is the
	// store's: a per-install secret that lives in a file beside the database, which
	// this package has no business resolving. Resolving it here meant that running
	// this package's tests CREATED A REAL KEY IN THE DEVELOPER'S DATA DIRECTORY —
	// which is not a tidiness problem, it is the assembler reaching out of the
	// process it was handed and writing to a machine it knows nothing about. The
	// store that will hold the digest is the one that owns the key, and asking it
	// makes that structural rather than a convention.
	DigestQuery(query string) (string, error)
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
func record(req Request, res Result, sink RecordSink, log *slog.Logger) memory.RetrievalRecord {
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
		QueryHash: queryHash(req.Query, sink, log),
		AsOf:      asOf,
		Outcome:   string(res.Outcome),
		Reason:    res.Reason,
		Verdicts:  out,
	}
}

// queryHash is the caller's question, reduced to a keyed digest.
//
// A record whose type could hold a query would eventually hold one — a report
// that wanted the text is a perfectly reasonable next request, and the column is
// right there. Making the text impossible to express is the cheaper half of the
// privacy rule than remembering not to.
//
// The digest is an HMAC under a per-install key, not a bare hash, and the key is
// what makes the second half hold. A plain sha256 of a short question is a
// fingerprint: the store's own memories are a dictionary of what a user would
// ask, and a hash confirms a guess as efficiently as it hides the original.
// memory.QueryDigest owns the key and says where it lives; what that costs a
// restore on another machine is in that file's header — grouping holds within one
// install and not across two.
//
// An EMPTY query is an empty hash rather than the digest of the empty string: a
// constant that looks like a question's fingerprint is worse than an honest
// "this call carried no query", and a session-start injection is exactly that.
//
// A key that cannot be read yields an empty hash and a LOG line, never an
// unsalted fallback: the record keeps its verdicts, which are the part that is not
// about the question, and the row says nothing about what was asked.
func queryHash(q string, sink RecordSink, log *slog.Logger) string {
	digest, err := sink.DigestQuery(q)
	if err != nil {
		log.Warn("no query digest recorded; the audit cannot group calls by question",
			"error", err)
		return ""
	}
	return digest
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

	// Projected before the clock starts, and the projection is nearly as cheap as
	// the Result in hand — but not entirely, and an earlier version of this comment
	// said "it reads only the result in hand", which stopped being true when the
	// query digest moved behind the RecordSink seam.
	//
	// What it reads now: the Result, and one call to the sink for the digest. That
	// call is an in-memory HMAC against the store's cached per-install key, because
	// mcpserver.New resolves the key at startup (Store.WarmQueryKey) and the key is
	// then held for the process. So the ordinary case costs no filesystem work and
	// cannot consume the budget below.
	//
	// The residual, stated rather than left to be discovered: a provider that is
	// neither *memory.Store nor able to warm pays ONE key resolution — a data
	// directory, a read, and on a first install a mkdir and a create — on its first
	// search, unbounded. DigestQuery takes no context precisely so this comment can
	// claim no bound it does not have: file I/O does not honour one, and a deadline
	// that cannot interrupt the work would be a promise the code cannot keep. The
	// two things that do keep it small are the warm at startup and the failure
	// backoff in the key's own cache, and both are tested.
	rec := record(req, res, sink, log)

	// The write gets its own deadline, and this is the property that makes it
	// safe to leave on a live tool. Recording takes the store's exclusive mutex
	// and then BEGIN IMMEDIATE, so under contention — another process holding
	// SQLite's write lock, or another tool call in this same server already in a
	// write — the caller can wait out the store's whole 5s busy_timeout TWICE,
	// and that wait would sit between the caller receiving its answer and Run
	// returning. One slow record would then delay every other concurrent tool
	// call in the same MCP server, which is the opposite of what a measurement
	// should do to the thing it measures.
	//
	// So the record write is best-effort with a short budget: on timeout the row
	// is dropped, the loss is logged, and the answer is returned unchanged. A
	// missing row is a gap in a report; a search that waits is an outage.
	writeCtx, cancel := context.WithTimeout(ctx, recordWriteBudget)
	defer cancel()
	if err := sink.RecordRetrieval(writeCtx, rec); err != nil {
		log.Warn("retrieval record not written; the audit will be missing this call",
			"project_id", rec.ProjectID, "source", rec.Source,
			"outcome", rec.Outcome, "error", err)
	}
}

// RecordResult writes the retrieval record for a Result its caller narrowed
// AFTER Run, through req.Record, with the same projection Run itself uses.
//
// It exists for a surface that makes a delivery decision Run cannot see (a
// per-session dedup, a floor of its own, a byte budget shared with another
// channel). Such a caller runs with Record nil, narrows res.Items to the rows it
// actually delivered, appends a dropped Decision for each row it withheld, and
// calls this once and only when it delivered something: a record written by Run
// itself would claim rows the caller never sent. A nil req.Record writes
// nothing, and a failure to write is logged and never returned, exactly as for
// Run.
func RecordResult(ctx context.Context, req Request, res Result) {
	emit(ctx, req.Record, req, res)
}
