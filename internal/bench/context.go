package bench

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

const (
	// ContextItems is the tool's default limit.
	ContextItems = 5
	// ContextBytes is the tool's response cap.
	ContextBytes = 2 * memory.MaxContentLen
)

// ContextInstant is the clock used for context benchmark reports.
func ContextInstant() time.Time {
	return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
}

// ContextRequest is the request RunContext measures.
func ContextRequest(q Query, at time.Time) assemble.Request {
	return contextRequestAt(q, at, config.DefaultRelevanceCutoff, config.DefaultNoAnswerCosine)
}

func contextRequestWithCutoff(q Query, at time.Time, cutoff float64) assemble.Request {
	return contextRequestAt(q, at, cutoff, 0)
}

func contextRequestAt(q Query, at time.Time, cutoff, bar float64) assemble.Request {
	return assemble.Request{
		ProjectID:       q.ProjectID,
		Query:           q.Text,
		QueryVec:        q.Vector,
		Source:          assemble.SourceBench,
		RelevanceCutoff: cutoff,
		NoAnswerCosine:  bar,
		Budget: assemble.Budget{
			MaxItems: ContextItems,
			MaxBytes: ContextBytes,
		},
		Condition: assemble.CondHybrid,
		Now:       at,
	}
}

// Ratio is a measured fraction with its denominator.
type Ratio struct{ Num, Den int }

// Defined reports whether the ratio has a denominator.
func (r Ratio) Defined() bool { return r.Den > 0 }

// String renders the ratio.
func (r Ratio) String() string {
	if !r.Defined() {
		return "n/a"
	}
	return fmt.Sprintf("%.3f (%d/%d)", float64(r.Num)/float64(r.Den), r.Num, r.Den)
}

func (r Ratio) add(num, den int) Ratio {
	r.Num += num
	r.Den += den
	return r
}

// ContextReport is the measured block pooled over a query set.
type ContextReport struct {
	Project       string
	Queries       int
	Answered      int
	Items         int
	Leaked        int
	Relevant      int
	Precision     Ratio
	Contamination Ratio
	Arms          []LeakArm
	ResultRate    Ratio
	Budget        ContextBudget
	Diversity     ContextDiversity
	Cost          ContextCost
}

// LeakArm is one contamination arm count.
type LeakArm struct {
	Name  string
	Count int
}

// ContextBudget reports budget adherence.
type ContextBudget struct {
	MaxItems        int
	MaxBytes        int
	MaxResponse     int
	OverByteCap     int
	MaxTokens       int
	TrimmedQueries  Ratio
	TrimmedItems    Ratio
	TrimmedRelevant Ratio
	FittedQueries   Ratio
	FittedItems     Ratio
	FittedRelevant  Ratio
}

// BucketShare is one bucket's share of admitted rows.
type BucketShare struct {
	Bucket  string
	Items   int
	Queries int
	Share   Ratio
}

// ContextDiversity reports how admitted rows are spread.
type ContextDiversity struct {
	TopBucket Ratio
	Buckets   []BucketShare
}

// ContextCost reports estimated cost per answered query.
type ContextCost struct {
	TokensPerQuery Ratio
	BytesPerQuery  Ratio
}

// RunContext assembles one block per query and returns the pooled measurement.
func RunContext(ctx context.Context, store *memory.Store, queries []Query, at time.Time) (ContextReport, error) {
	return runContextBar(ctx, store, queries, at, config.DefaultRelevanceCutoff, config.DefaultNoAnswerCosine)
}

func runContextAt(ctx context.Context, store *memory.Store, queries []Query, at time.Time, cutoff float64) (ContextReport, error) {
	return runContextBar(ctx, store, queries, at, cutoff, 0)
}

func runContextBar(ctx context.Context, store *memory.Store, queries []Query, at time.Time, cutoff, bar float64) (ContextReport, error) {
	return runContextWith(ctx, store, queries, at, cutoff, bar, nil)
}

func runContextWith(ctx context.Context, store *memory.Store, queries []Query, at time.Time, cutoff, bar float64, refuse func(assemble.Result) bool) (ContextReport, error) {
	rep := ContextReport{Queries: len(queries)}
	rep.Arms = make([]LeakArm, len(assemble.LeakArms))
	for i, arm := range assemble.LeakArms {
		rep.Arms[i] = LeakArm{Name: arm}
	}
	if len(queries) == 0 {
		return rep, nil
	}
	rep.Project = queries[0].ProjectID
	rep.Budget.MaxItems = ContextItems
	rep.Budget.MaxBytes = ContextBytes

	for _, q := range queries {
		if q.ProjectID != rep.Project {
			return ContextReport{}, fmt.Errorf("the context report reads its bucket table against one project, and query %q is in %q", q.Name, q.ProjectID)
		}
		if q.Rel.relevantCount() == 0 {
			continue
		}
		res, err := assemble.Run(ctx, store, contextRequestAt(q, at, cutoff, bar))
		if err != nil {
			return ContextReport{}, fmt.Errorf("assemble %q: %w", q.Name, err)
		}
		if refuse != nil && refuse(res) {
			res.Items = nil
		}
		if err := measureQuery(res, q, &rep); err != nil {
			return ContextReport{}, err
		}
	}
	rep.ResultRate = Ratio{Num: rep.Answered, Den: rep.Queries}
	for i := range rep.Diversity.Buckets {
		rep.Diversity.Buckets[i].Share = Ratio{Num: rep.Diversity.Buckets[i].Items, Den: rep.Items}
	}
	sort.SliceStable(rep.Diversity.Buckets, func(i, j int) bool {
		a, b := rep.Diversity.Buckets[i], rep.Diversity.Buckets[j]
		if a.Items != b.Items {
			return a.Items > b.Items
		}
		return a.Bucket < b.Bucket
	})
	return rep, nil
}

func measureQuery(res assemble.Result, q Query, rep *ContextReport) error {
	if len(res.Items) == 0 {
		return nil
	}
	if res.Trace == nil {
		return fmt.Errorf("query %q: the assembler recorded no trace, so its per-row verdicts cannot be read and the block cannot be scored", q.Name)
	}
	rep.Answered++
	rep.Items += len(res.Items)
	rep.Cost.TokensPerQuery = rep.Cost.TokensPerQuery.add(res.Tokens, 1)
	rep.Cost.BytesPerQuery = rep.Cost.BytesPerQuery.add(res.Bytes, 1)
	if res.Bytes > rep.Budget.MaxResponse {
		rep.Budget.MaxResponse = res.Bytes
	}
	if res.Bytes > rep.Budget.MaxBytes {
		rep.Budget.OverByteCap++
	}
	if res.Tokens > rep.Budget.MaxTokens {
		rep.Budget.MaxTokens = res.Tokens
	}

	touched := map[string]int{}
	dominant, relevant := 0, 0
	for _, it := range res.Items {
		if _, ok := res.Trace.Signals[it.ID]; !ok {
			return fmt.Errorf("query %q: row %s is in the block with no recorded verdict, so whether it leaked is unmeasured rather than clean", q.Name, it.ID)
		}
		if q.Rel[it.ID] > 0 {
			relevant++
		}
		touched[it.Bucket]++
		if touched[it.Bucket] > dominant {
			dominant = touched[it.Bucket]
		}
	}
	rep.Relevant += relevant
	rep.Precision = rep.Precision.add(relevant, len(res.Items))
	rep.Diversity.TopBucket = rep.Diversity.TopBucket.add(dominant, 1)
	for bucket, n := range touched {
		b := bucketOf(&rep.Diversity, bucket)
		b.Items += n
		b.Queries++
	}

	leaked := 0
	for _, leak := range res.Leaks() {
		leaked++
		for _, arm := range leak.Arms {
			i := armIndex(arm)
			if i < 0 {
				return fmt.Errorf("query %q: row %s leaked through an arm this bench has no row for (%q), so it cannot be counted as clean", q.Name, leak.ID, arm)
			}
			rep.Arms[i].Count++
		}
	}
	rep.Leaked += leaked
	rep.Contamination = rep.Contamination.add(leaked, len(res.Items))

	capDropped, fitDropped := res.Trace.TrimmedByBudget()
	noteTrim(rep, capDropped, fitDropped, q.Rel, len(res.Items), relevant)
	return nil
}

func noteTrim(rep *ContextReport, capDropped, fitDropped []string, rel Relevance, admitted, relevant int) {
	capGraded, fitGraded := 0, 0
	for _, id := range capDropped {
		if rel[id] > 0 {
			capGraded++
		}
	}
	for _, id := range fitDropped {
		if rel[id] > 0 {
			fitGraded++
		}
	}
	noteTrimStage(&rep.Budget.TrimmedQueries, &rep.Budget.TrimmedItems, &rep.Budget.TrimmedRelevant,
		len(capDropped), capGraded, admitted+len(capDropped)+len(fitDropped), relevant+capGraded+fitGraded)
	noteTrimStage(&rep.Budget.FittedQueries, &rep.Budget.FittedItems, &rep.Budget.FittedRelevant,
		len(fitDropped), fitGraded, admitted+len(fitDropped), relevant+fitGraded)
}

func noteTrimStage(queries, items, relevant *Ratio, cut, gradedCut, reached, gradedReached int) {
	trimmed := 0
	if cut > 0 {
		trimmed = 1
	}
	*queries = queries.add(trimmed, 1)
	*items = items.add(cut, reached)
	*relevant = relevant.add(gradedCut, gradedReached)
}

func bucketOf(div *ContextDiversity, bucket string) *BucketShare {
	for i := range div.Buckets {
		if div.Buckets[i].Bucket == bucket {
			return &div.Buckets[i]
		}
	}
	div.Buckets = append(div.Buckets, BucketShare{Bucket: bucket})
	return &div.Buckets[len(div.Buckets)-1]
}

func armIndex(arm string) int {
	for i, a := range assemble.LeakArms {
		if a == arm {
			return i
		}
	}
	return -1
}

// FormatContext renders the context section of `ghost bench`.
func FormatContext(rep ContextReport) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "\ncontext assembly (ghost_memory_search's own block: %d items / %d bytes; report-only, no gate)\n",
		rep.Budget.MaxItems, rep.Budget.MaxBytes)
	if rep.Project != "" {
		fmt.Fprintf(&b, "  project %s", rep.Project)
	}
	fmt.Fprintf(&b, ", %d queries measured, %d answered, %d admitted rows\n", rep.Queries, rep.Answered, rep.Items)

	b.WriteString("\n  A CONTAMINATED row is one internal/assemble classifies as one it should not have carried\n")
	b.WriteString("  (Result.Leaks, from the verdicts its own stages recorded). A metric that re-read the corpus\n")
	b.WriteString("  instead would be a second implementation of rules the pipeline already applies, and free to\n")
	b.WriteString("  drift from them until a leak was reported as clean. These numbers are about the BLOCK a\n")
	b.WriteString("  caller receives. NDCG@10 and MRR@10 are the ordering numbers -- they are in the table plain\n")
	b.WriteString("  `ghost bench` prints, not this one -- and neither can see any of it.\n\n")
	fmt.Fprintf(&b, "  %-24s %18s  %s\n", "metric", "value", "population")
	fmt.Fprintf(&b, "  %-24s %18s  %s\n", "result rate", rep.ResultRate, "queries that admitted at least one row")
	fmt.Fprintf(&b, "  %-24s %18s  %s\n", "context precision", rep.Precision, "graded-relevant of the admitted rows")
	fmt.Fprintf(&b, "  %-24s %18s  %s\n", "contamination", rep.Contamination, "admitted rows the assembler flags")

	fmt.Fprintf(&b, "\n  %-24s %8s  %s\n", "contamination by arm", "rows", "what the arm is")
	for _, arm := range rep.Arms {
		fmt.Fprintf(&b, "  %-24s %8d  %s\n", arm.Name, arm.Count, leakArmMeaning(arm.Name))
	}
	if rep.Leaked == 0 && rep.Items > 0 {
		b.WriteString("  a zero here is a reading of THIS corpus: the graded one holds no resolved, out-of-window or\n")
		b.WriteString("  cross-bucket row, so there is nothing for an arm to catch. docs/benchmarks.md says which.\n")
	}

	fmt.Fprintf(&b, "\n  %-24s %18s  %s\n", "budget adherence", "value", "population")
	fmt.Fprintf(&b, "  %-24s %18s  %s\n", "largest response",
		fmt.Sprintf("%d bytes", rep.Budget.MaxResponse),
		fmt.Sprintf("cap %d bytes, %d responses over it", rep.Budget.MaxBytes, rep.Budget.OverByteCap))
	fmt.Fprintf(&b, "  %-24s %18s  %s\n", "largest block, tokens",
		fmt.Sprintf("%d (est.)", rep.Budget.MaxTokens),
		"bytes/4 rounded up per row; an estimate, there is no tokenizer here")
	fmt.Fprintf(&b, "  %-24s %18s  %s\n", "item cap trimmed", rep.Budget.TrimmedItems,
		fmt.Sprintf("of the rows that reached it; %s of answered queries, %s of the graded ones among them",
			rep.Budget.TrimmedQueries, rep.Budget.TrimmedRelevant))
	fmt.Fprintf(&b, "  %-24s %18s  %s\n", "response fit trimmed", rep.Budget.FittedItems,
		fmt.Sprintf("of the rows that reached it; %s of answered queries, %s of the graded ones among them",
			rep.Budget.FittedQueries, rep.Budget.FittedRelevant))

	fmt.Fprintf(&b, "\n  %-24s %18s  %s\n", "cost per answered query", "value", "population")
	fmt.Fprintf(&b, "  %-24s %18s  %s\n", "estimated tokens", rep.Cost.TokensPerQuery,
		"mean over answered queries; bytes/4 per row, an estimate, no tokenizer here")
	fmt.Fprintf(&b, "  %-24s %18s  %s\n", "rendered bytes", fmt.Sprintf("%s bytes", rep.Cost.BytesPerQuery),
		"mean over answered queries; the complete response, framing and verdict line included")

	fmt.Fprintf(&b, "\n  %-24s %8s %16s %8s\n", "admitted rows by bucket", "rows", "share", "queries")
	for _, bk := range rep.Diversity.Buckets {
		fmt.Fprintf(&b, "  %-24s %8d %16s %8d\n", bk.Bucket, bk.Items, bk.Share, bk.Queries)
	}
	if len(rep.Diversity.Buckets) == 0 {
		b.WriteString("  (no rows admitted, so there is nothing to divide)\n")
	}
	fmt.Fprintf(&b, "\n  dominant bucket, mean rows per answered query  %s\n", rep.Diversity.TopBucket)
	return b.String()
}

func leakArmMeaning(arm string) string {
	switch arm {
	case assemble.LeakResolved:
		return "resolved_at is set: retired evidence the ranking demotes rather than drops"
	case assemble.LeakExpired:
		return "its validity window has closed"
	case assemble.LeakNotYetValid:
		return "its validity window has not opened yet"
	case assemble.LeakOutOfScope:
		return "it names a different place than the request asked for"
	case assemble.LeakOtherProject:
		return "it sits in a bucket the request did not name"
	default:
		return "an arm this bench does not know: internal/assemble added one"
	}
}
