package assemble

// #646: what the retrieval record costs a SEARCH, measured against the same
// search with no sink.
//
// The store-side benchmark measures the write. This one measures the thing an
// operator actually feels: the seam runs inside assemble.Run, after the answer
// has been fitted, so a per-call cost in microseconds is only meaningful next to
// the run it attaches to. Both arms are the same fixture, the same candidates and
// the same request, so the difference is the record and nothing else.

import (
	"context"
	"fmt"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// discardSink records nothing and fails nothing, which is what a store that
// cannot audit looks like. Counting the record too would measure the projection
// as well as the write, and the projection is the cheap half — the point of the
// pair of arms is to separate the work the assembler does from the work the
// store does.
type countingSink struct{ n int }

func (c *countingSink) RecordRetrieval(context.Context, memory.RetrievalRecord) error {
	c.n++
	return nil
}

func benchAssembleRun(b *testing.B, sink RecordSink) {
	rows := make([]memory.Candidate, 0, 30)
	for i := range 30 {
		rows = append(rows, candidate(fmt.Sprintf("M%02d", i), "proj", "fact", "database configuration pooling note", 0.9-float64(i)/100))
	}
	req := baseRequest()
	req.Budget.MaxItems = 10
	req.Record = sink

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := Run(context.Background(), &fakeRetriever{set: setOf(rows...)}, req)
		if err != nil {
			b.Fatalf("Run: %v", err)
		}
		if len(res.Items) == 0 {
			b.Fatal("Run admitted nothing")
		}
	}
}

func BenchmarkRunWithoutARecord(b *testing.B) { benchAssembleRun(b, nil) }
func BenchmarkRunWithARecord(b *testing.B)    { benchAssembleRun(b, &countingSink{}) }
