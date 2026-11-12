package assemble

import (
	"context"
	"log/slog"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

const recordWriteBudget = 250 * time.Millisecond

// RecordSink receives the retrieval record for a successful Run.
type RecordSink interface {
	RecordRetrieval(ctx context.Context, rec memory.RetrievalRecord) error
	DigestQuery(query string) (string, error)
}

func record(req Request, res Result, sink RecordSink, log *slog.Logger) memory.RetrievalRecord {
	var decisions []Decision
	asOf := ""
	if res.Trace != nil {
		decisions = res.Trace.Decisions
		asOf = res.Trace.AsOf
	}

	decided := make(map[string]memory.RowVerdict, len(decisions))
	for _, d := range decisions {
		decided[d.ID] = memory.RowVerdict{ID: d.ID, Kept: d.Kept, Stage: d.Stage, Reason: d.Reason}
	}

	kept := make(map[string]bool, len(res.Items))
	dropped := make(map[string]bool, len(decided))
	out := make([]memory.RowVerdict, 0, len(res.Items)+len(decided))
	for _, it := range res.Items {
		kept[it.ID] = true
		if v, ok := decided[it.ID]; ok && v.Kept {
			v.ID = it.ID
			out = append(out, v)
			continue
		}
		out = append(out, memory.RowVerdict{ID: it.ID, Kept: true})
	}
	for _, d := range decisions {
		if d.Kept || kept[d.ID] || dropped[d.ID] {
			continue
		}
		dropped[d.ID] = true
		v := decided[d.ID]
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

func queryHash(q string, sink RecordSink, log *slog.Logger) string {
	digest, err := sink.DigestQuery(q)
	if err != nil {
		log.Warn("no query digest recorded; the audit cannot group calls by question",
			"error", err)
		return ""
	}
	return digest
}

func emit(ctx context.Context, sink RecordSink, req Request, res Result) {
	if sink == nil {
		return
	}
	log := req.Logger
	if log == nil {
		log = slog.Default()
	}

	rec := record(req, res, sink, log)

	writeCtx, cancel := context.WithTimeout(ctx, recordWriteBudget)
	defer cancel()
	if err := sink.RecordRetrieval(writeCtx, rec); err != nil {
		log.Warn("retrieval record not written; the audit will be missing this call",
			"project_id", memory.RefusalArg("project_id", rec.ProjectID), "source", rec.Source,
			"outcome", rec.Outcome, "error", err)
	}
}

// RecordResult writes the retrieval record for a result narrowed after Run.
func RecordResult(ctx context.Context, req Request, res Result) {
	emit(ctx, req.Record, req, res)
}
