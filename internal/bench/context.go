package bench

// This file measures the BLOCK and not the ranking.
//
// The three ablations above ask which row came first: Recall@1 and NDCG@10 are
// statements about ORDER, and a system can be perfectly good at ordering every
// row and still be bad at deciding what to put in front of a model. Nothing in
// this package could see that. NDCG is invariant under moving the last row of a
// block off the end, so a block carrying ten rows where two would have answered
// scores the same NDCG as the two-row block while costing a caller five times the
// tokens — and that tradeoff is the whole of what the context assembler manages.
// These metrics are about the block a caller RECEIVES: how much of it is
// relevant, how much of it should never have been in it, what it cost, and whether
// it fit.
//
// They are report-only, deliberately, and this is the same decision
// falsepositive.go records for the no-answer rate: every figure here is a claim
// about today's assembler against a corpus with no known-answer contamination
// rows, so a gate on them would be a floor on a fixture rather than on retrieval.
// The graded corpus holds no resolved row, no `_global` row and no supersession
// edge, and stage 2 drops an out-of-window row before it can be admitted — so its
// contamination rate is 0.000 by construction, and a gate on that would be a gate
// on a tautology. See contextFixture's doc comment in context_test.go for the
// small corpus that carries the contaminated rows this one cannot.
//
// The two things this file refuses to do are both about the same line:
//
//   - It does not re-implement the pipeline's decisions. Contamination is read
//     from assemble.Result.Leaks(), which reads the verdicts the stages
//     recorded; nothing here parses a validity stamp, re-derives a scope match,
//     or names a stage a string literal would have to guess. See
//     internal/assemble/leak.go for why that classifier lives there.
//   - It does not score a result it could not examine. A result with no trace, or
//     an admitted row the trace never reached, is a measurement whose inputs are
//     missing, and reporting a clean zero for it would be the one output this
//     report must never produce: a reader could not tell it from a pipeline that
//     really does withhold contamination. measureQuery refuses both.

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/memory"
)

// The budget the tool sends. ghost_memory_search defaults args.limit to 10 and
// caps the complete response at 2*memory.MaxContentLen bytes, and those two
// numbers are the request ContextRequest builds — a context metric measured
// against any other budget is a metric about a surface nobody ships. They are
// named constants rather than literals in the request so the report header and
// the request cannot be edited apart.
const (
	// ContextItems is the tool's default `limit`: ten rows, and the item cap
	// stage 8 applies across the project and `_global` buckets together.
	ContextItems = 10
	// ContextBytes is the tool's response cap, 2*MaxContentLen. It bounds the
	// COMPLETE rendered response, which is why the budget table reports the
	// response-fit trim separately from the item cap: a larger item limit makes
	// the byte cap harder to hit, not easier.
	ContextBytes = 2 * memory.MaxContentLen
)

// ContextInstant is the clock `ghost bench --context` measures the block at.
//
// A function rather than a package variable because it hands out a time.Time,
// which is a mutable struct: a caller that wrote to a shared var would move the
// clock under every later reader, and a benchmark whose clock can be written from
// outside cannot claim that two runs print the same bytes.
//
// The date is a constant for one reason — the report is published, and a table
// that moved with the calendar could not be compared with the previous run of it,
// let alone with the number in docs/benchmarks.md. It also has to sit well clear
// of every validity boundary the graded corpus states: the corpus deliberately
// holds one window that closed in 2020 and one that opens in 2099, so a clock
// outside those is correct rather than wrong, but a clock DAYS from a boundary is
// a published table one corpus edit away from changing for a reason that has
// nothing to do with retrieval. TestContextInstantSitsInsideEveryGradedWindow
// holds the margin.
//
// It is NOT the same wall-clock seed the ablations use, and the split is
// deliberate in both directions. The ablations' published numbers depend on a
// corpus stamped at the moment the run began — pinning that would age the corpus
// by however long ago this constant was written and move NDCG for reasons that
// have nothing to do with retrieval. And the headline dataset sets no age_days, so
// every row's age is zero either way: what a fixed clock changes HERE is the
// validity reading, which is the part that needs pinning, not the decay.
func ContextInstant() time.Time {
	return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
}

// ContextRequest is the request RunContext measures: the one ghost_memory_search
// builds for a plain search, with its source labelled as the bench.
//
// Two things differ from the tool's request, and both are about naming rather
// than about what is measured. Source is SourceBench so a trace or a log line
// that reaches a reader says the request came from a benchmark; the two sources
// produce the SAME project mode for every request this bench makes, because every
// query names a project and the mapping only diverges for an empty ProjectID —
// asserted in TestContextRequestIsTheBudgetTheToolSends, because a future Source
// change could otherwise quietly make this measure a mode the tool never runs. And
// Now is the caller's clock rather than the wall clock, which is what makes the
// report a function of (corpus, clock) rather than of when it ran.
//
// Everything else — the budget, the condition, the empty scope, the empty
// category — is the tool's request, spelled the tool's way. A bench that measured
// a hand-tuned request would be reporting on its own configuration.
func ContextRequest(q Query, at time.Time) assemble.Request {
	return assemble.Request{
		ProjectID: q.ProjectID,
		Query:     q.Text,
		QueryVec:  q.Vector,
		Source:    assemble.SourceBench,
		Budget: assemble.Budget{
			MaxItems: ContextItems,
			MaxBytes: ContextBytes,
		},
		Condition: assemble.CondHybrid,
		Now:       at,
	}
}

// Ratio is a measured fraction with its denominator carried beside it.
//
// The denominator is the point. Every ratio in this report is a claim about a
// population — the admitted rows, the answered queries — and a bare float cannot
// say how large that population was, so 0.000 over four rows and 0.000 over four
// thousand read identically and mean very different things. Printing num/den is
// what makes a thin measurement look thin.
//
// A Ratio with Den == 0 is UNDEFINED and stays that way: it is never treated as
// zero, never averaged into anything, and renders as `n/a`. The alternative —
// printing 0.000 for a corpus where nothing assembled — is a claim the report
// cannot support, and a system could improve the number by measuring less.
type Ratio struct{ Num, Den int }

// Defined reports whether the ratio has a population to be a fraction of.
func (r Ratio) Defined() bool { return r.Den > 0 }

// String renders `value (num/den)`, or `n/a` when the ratio is undefined.
func (r Ratio) String() string {
	if !r.Defined() {
		return "n/a"
	}
	return fmt.Sprintf("%.3f (%d/%d)", float64(r.Num)/float64(r.Den), r.Num, r.Den)
}

// add accumulates one measurement into a ratio, summing both sides. It is for
// the ratios whose halves count different things — graded-relevant rows out of
// admitted rows, per query — where summing the numerator alone would divide one
// query's population by another's count.
func (r Ratio) add(num, den int) Ratio {
	r.Num += num
	r.Den += den
	return r
}

// ContextReport is the measured block, pooled over a query set.
//
// Every count here counts what a caller RECEIVED, never what the ranking could
// have produced: Items is the rows the assembler admitted under the tool's budget,
// Relevant is how many of those the corpus grades relevant, and Queries/Answered
// separate the queries that returned a block from the queries that did not. The
// last pair is why ResultRate is here at all — every other ratio in this struct has
// "admitted rows" or "answered queries" in its denominator, so a system that
// returned nothing would post perfect precision and perfect cleanliness. It is
// measured, not excluded.
type ContextReport struct {
	// Project is the project these queries searched, named once because the
	// bucket table is read against it. RunContext refuses a query set that mixes
	// projects rather than labelling that table with whichever came first.
	Project string
	// Queries is how many queries were measured and Answered how many of them
	// admitted at least one row; the gap is the result rate's numerator.
	Queries  int
	Answered int
	// Items and Leaked and Relevant are counts of admitted rows, and
	// Precision is Relevant/Items while Contamination is Leaked/Items.
	Items    int
	Leaked   int
	Relevant int
	// Precision is the share of the admitted rows the corpus grades relevant. It
	// is not recall: recall asks whether a relevant row was found, and this asks
	// how much of what WAS found was worth carrying.
	Precision Ratio
	// Contamination is Items' share that assemble.Result.Leaks() flags, and Arms
	// breaks it down. The breakdown is not decoration: 0.500 from one arm is a
	// filter with a hole, 0.500 from all five is a pipeline admitting what it
	// should not, and the two have opposite remedies.
	Contamination Ratio
	Arms          []LeakArm
	ResultRate    Ratio
	Budget        ContextBudget
	Diversity     ContextDiversity
	Cost          ContextCost
}

// LeakArm is one contamination arm and how many admitted rows it flagged.
//
// The slice carries every arm in assemble.LeakArms order, zeros included. An arm
// absent from a map and an arm that fired zero times are different facts, and only
// one of them is visible in a map — so a report listing only what fired could not
// distinguish "nothing leaked" from "nothing looked".
type LeakArm struct {
	Name  string
	Count int
}

// ContextBudget is budget adherence: whether the block the tool ships fit the
// budget the tool sent, and what it cost when it did not.
//
// MaxResponse and OverByteCap are the direct reading — the largest rendered
// response any query produced, and how many responses came back over the cap. A
// non-zero OverByteCap is a defect in the response-fit pass rather than a finding,
// so it is printed as a count and never folded into a ratio that could be
// averaged away.
//
// The two trims are counted separately because they are different failures with
// opposite remedies. The item cap is the caller's own bound, and a block that hits
// it wanted more rows than the caller would take. The response-fit pass is the byte
// cap on the rendered envelope, which a LARGER item limit makes worse — so a
// report that called both "the budget" would advise raising a limit that raises
// the problem. TrimmedRelevant is the half that says whether any of it cost a
// graded-relevant row, which is the only one of these numbers that is a quality
// claim rather than a conformance reading.
type ContextBudget struct {
	// MaxItems and MaxBytes are the bounds every request carried, echoed into
	// the report so a number read here cannot outlive the budget that produced
	// it. MaxResponse is the largest complete rendered response in bytes,
	// OverByteCap how many responses exceeded MaxBytes, and MaxTokens the largest
	// token ESTIMATE over the admitted rows — bytes/4 rounded up per row
	// (assemble.Item.Tokens), with no tokenizer in the pipeline, so an estimate
	// everywhere it appears and never a cap.
	MaxItems    int
	MaxBytes    int
	MaxResponse int
	OverByteCap int
	MaxTokens   int
	// The Trimmed* triple is the item cap — answered queries it shortened, rows it
	// cut of the rows that reached the stage, graded-relevant rows among those. The
	// Fitted* triple is the same three for the response-fit post-pass.
	TrimmedQueries  Ratio
	TrimmedItems    Ratio
	TrimmedRelevant Ratio
	FittedQueries   Ratio
	FittedItems     Ratio
	FittedRelevant  Ratio
}

// BucketShare is one bucket's share of the rows the assembler admitted.
//
// Bucket is the storage bucket the row came from: the request's own project, or
// `_global`. Items and Share are pooled over answered queries; Queries is how many
// answered queries drew at least one row from here, which is what separates "one
// bucket supplies a seventh of a corpus-wide pool" from "one bucket supplies every
// row of every block".
type BucketShare struct {
	Bucket  string
	Items   int
	Queries int
	Share   Ratio
}

// ContextDiversity is how the admitted rows are spread.
//
// TopBucket is the mean number of rows an answered query's largest bucket
// contributed: the sum over answered queries of the dominant bucket's rows, over
// the answered queries. It is bounded by the item cap, so a block drawn from one
// bucket approaches 10 and a block spread over two approaches 5 — the reading is
// "how lopsided was this block", a property of the block rather than of the
// corpus, and the number that moves when diversity changes.
//
// The comparable-across-corpus figures are in Buckets: a share, per bucket, of
// every admitted row, with the count of queries that produced it.
type ContextDiversity struct {
	TopBucket Ratio
	Buckets   []BucketShare
}

// ContextCost is what one answered query's block cost the caller.
//
// TokensPerQuery uses Result.Tokens, the assembler's own estimate over the
// admitted rows. BytesPerQuery uses Result.Bytes, the complete rendered response —
// framing, verdict, caveat and machine line included, because that is what the
// caller receives and therefore what it pays for. The gap between the two is the
// cost of everything that is not a memory, which on a short block is a large share
// of the bill.
//
// Both are over ANSWERED queries, so a query that returned nothing costs nothing
// here — which is true of the bill and false of the outcome, and is why ResultRate
// is printed beside them rather than folded into them.
type ContextCost struct {
	TokensPerQuery Ratio
	BytesPerQuery  Ratio
}

// RunContext assembles one block per query through the same path
// ghost_memory_search takes, and returns the pooled measurement.
//
// The queries are the graded set, so precision is measured against the corpus's
// own relevance grades rather than against a second judgement. A query with an
// empty relevance map is not scored here, exactly as runner.go does not score it:
// there is no relevant row to be precise about. It still counts in the result
// rate's denominator — an empty block is a real outcome, and a report that
// excluded it would let returning nothing improve every other number in the table.
//
// at is the instant every request carries as Now, and the caller must take it from
// SeedAt rather than from the wall clock: it is the corpus's own stamp. The report
// is then a function of (corpus, clock), which is what lets the table be published
// and what lets the determinism test compare two seeds byte for byte.
//
// Errors are refused rather than scored around. A query the assembler cannot run is
// a hole in the measurement, and a pooled ratio with a hole in it is a number
// whose denominator nobody can account for.
func RunContext(ctx context.Context, store *memory.Store, queries []Query, at time.Time) (ContextReport, error) {
	rep := ContextReport{Queries: len(queries)}
	// Every arm, in the assembler's order, before any query runs: the report's
	// shape cannot depend on what happened to leak.
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
		res, err := assemble.Run(ctx, store, ContextRequest(q, at))
		if err != nil {
			return ContextReport{}, fmt.Errorf("assemble %q: %w", q.Name, err)
		}
		if err := measureQuery(res, q, &rep); err != nil {
			return ContextReport{}, err
		}
	}
	rep.ResultRate = Ratio{Num: rep.Answered, Den: rep.Queries}
	for i := range rep.Diversity.Buckets {
		rep.Diversity.Buckets[i].Share = Ratio{Num: rep.Diversity.Buckets[i].Items, Den: rep.Items}
	}
	// Rows descending, then name. Sorting on the count as well as the name is
	// what makes the table stable in the presence of a tie: two buckets with equal
	// counts would otherwise be ordered by insertion — i.e. by whichever query
	// happened to reach one first — which is how a published table comes to
	// disagree with a re-run.
	sort.SliceStable(rep.Diversity.Buckets, func(i, j int) bool {
		a, b := rep.Diversity.Buckets[i], rep.Diversity.Buckets[j]
		if a.Items != b.Items {
			return a.Items > b.Items
		}
		return a.Bucket < b.Bucket
	})
	return rep, nil
}

// measureQuery folds one assembled block into the report.
//
// It is a function of a Result and a Query rather than of a store, so the rule
// that matters most here — an empty block moves nothing but the result rate — is
// testable without arranging for retrieval to come back empty. On a real store
// that needs a query neither leg can match, and a fixture built on it would be
// testing the store's empty-return behaviour rather than the rule.
//
// An error here is never a scoring failure: it is the refusal documented at the
// top of this file, and it leaves the measurement unmakeable rather than clean.
func measureQuery(res assemble.Result, q Query, rep *ContextReport) error {
	// An empty block is not a precise block, a clean block or a cheap one: it has
	// no rows, so it contributes to nothing but the result rate. Folding it into
	// precision would let a system improve its score by returning nothing.
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

	// Per-query bucket tallies, so a bucket's query count counts a query once
	// however many of its rows that query's block carried.
	touched := map[string]int{}
	dominant, relevant := 0, 0
	for _, it := range res.Items {
		if _, ok := res.Trace.Signals[it.ID]; !ok {
			// An admitted row the trace never reached has no recorded verdict, and
			// reading the missing entry as "matched everything" would score an
			// unexamined row clean — the exact failure the classifier in assemble
			// exists to prevent, arriving here instead.
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

	// Contamination is the assembler's own answer, asked for once per block and
	// never re-derived here: which arms fire on which rows is assemble/leak.go's
	// decision, and a second copy of it in this package would be free to drift
	// until a leak was reported as clean by the one consumer whose job is to say
	// otherwise.
	//
	// ROWS, not arms: Leaks() returns one entry per contaminated row whatever
	// arms fired on it, so a row that is both resolved and out of date counts once
	// in the rate and twice across the two arm counters. A rate that counted it
	// twice would report a severity the block does not have.
	leaked := 0
	for _, leak := range res.Leaks() {
		leaked++
		for _, arm := range leak.Arms {
			i := armIndex(arm)
			if i < 0 {
				// Loud rather than skipped. An assembler that grows an arm
				// without this report knowing it must not have its rows quietly
				// counted as clean — the arms list is the assembler's contract and
				// a mismatch here is a build-time disagreement.
				return fmt.Errorf("query %q: row %s leaked through an arm this bench has no row for (%q), so it cannot be counted as clean", q.Name, leak.ID, arm)
			}
			rep.Arms[i].Count++
		}
	}
	rep.Leaked += leaked
	rep.Contamination = rep.Contamination.add(leaked, len(res.Items))

	// The trims, read by the assembler's own stage names rather than by this
	// package spelling "budget" and "response_fit": both lists are already
	// disjoint from the block, because a trimmed row is by definition not in it.
	capDropped, fitDropped := res.Trace.TrimmedByBudget()
	noteTrim(rep, capDropped, fitDropped, q.Rel, len(res.Items), relevant)
	return nil
}

// noteTrim folds ONE ANSWERED QUERY's two trims into the budget table. It is called
// once per answered query, whether or not a trim fired on it, and that is the whole
// point.
//
// Recording a query only when something was cut made every query ratio its own
// numerator over its own denominator: 1.000 on the graded corpus, 1.000 on a corpus
// where the budget had stopped binding, and n/a on a corpus where it never bound.
// Three different behaviours, one number, and a reader with no way to tell them
// apart. It also dropped the untrimmed query's rows out of the ROW ratios below, so
// those described a population drawn only from the queries that happened to hit the
// cap — 2/12 where the rows that reached the cap were 15, and a report that silently
// measured the queries it found interesting.
//
// The two stages also get their own populations rather than one shared one, because
// a row the response-fit pass cut has already passed the item cap on its way: the cap
// saw every row, and the fit pass only the ones the cap left. A shared denominator
// put the fit pass above the population it was ever shown.
//
// The graded denominator is the GRADED rows that reached the stage, which is the
// population a sentence like "the cap cost one graded-relevant row in eight" is a
// fraction of. `reached - cut` is not that population and is not the admitted count
// either; the two coincide only when the other trim dropped nothing, which is a
// coincidence rather than a rule.
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

// noteTrimStage folds one stage's row counts for one query into its three ratios.
//
// The query ratio's denominator is 1 UNCONDITIONALLY — that is the fix, and it is why
// this is reached for every answered query rather than only for a trimmed one. The
// row ratios' denominators are the population that stage saw, which is never zero
// for an answered query, so a trim that fired on nothing reads 0.000 rather than n/a:
// it ran, it saw rows, and it cut none. The graded ratio stays n/a when no graded row
// reached the stage, which is the one case where a share really is not a fraction of
// anything.
func noteTrimStage(queries, items, relevant *Ratio, cut, gradedCut, reached, gradedReached int) {
	trimmed := 0
	if cut > 0 {
		trimmed = 1
	}
	*queries = queries.add(trimmed, 1)
	*items = items.add(cut, reached)
	*relevant = relevant.add(gradedCut, gradedReached)
}

// bucketOf finds or creates one bucket's row in the diversity table.
func bucketOf(div *ContextDiversity, bucket string) *BucketShare {
	for i := range div.Buckets {
		if div.Buckets[i].Bucket == bucket {
			return &div.Buckets[i]
		}
	}
	div.Buckets = append(div.Buckets, BucketShare{Bucket: bucket})
	return &div.Buckets[len(div.Buckets)-1]
}

// armIndex is where an arm name from the assembler lands in this report's fixed arm
// list, or -1. See measureQuery for why -1 is an error there rather than a skip.
func armIndex(arm string) int {
	for i, a := range assemble.LeakArms {
		if a == arm {
			return i
		}
	}
	return -1
}

// FormatContext renders the context section of `ghost bench`.
//
// One section, laid out like the tables beside it: what was measured, the metric
// table with every denominator printed beside its value, the contamination
// breakdown, the budget's own adherence, and the admitted rows by bucket. Every
// ratio prints its num/den and an unmeasured one prints `n/a`, which is the
// false-positive report's convention and the whole difference between "nothing
// leaked" and "nothing was measured".
//
// The header says the section gates nothing, because a table of rates in a
// benchmark reads as a floor unless it says otherwise and these numbers are not
// one: they are a claim about today's assembler against a corpus that holds no
// contaminable rows to measure.
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

	// The cost, with its population beside it, because three separate places promise
	// it: the metric table in docs/benchmarks.md, the flag's own description in
	// docs/cli.md, and `benchUsage`. A measured field that nothing renders leaves all
	// three claims false and the reader with a section that measures a thing it never
	// shows. The maxima above are the other half of this table and neither substitutes
	// for the other: a mean is what a block usually costs and a maximum is what the
	// worst one cost, and a caller sizing a budget from the mean alone under-reserves.
	//
	// Bytes is the COMPLETE rendered response, framing and verdict line included, so it
	// is the number a caller actually receives; tokens is the assembler's own
	// bytes/4 estimate over the admitted rows, and there is no tokenizer in the
	// pipeline, so it is an estimate everywhere it appears.
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

// leakArmMeaning is the one-line gloss a report reader needs beside an arm name,
// because an arm nobody can name is an arm nobody can act on. The names are the
// assembler's; this only says what each one is short for, and an arm this bench
// does not recognise says so rather than printing a gloss it invented.
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
