package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/wcatz/ghost/internal/memory"
)

// Every graded metric in this package is undefined for a query nothing answers:
// recall has no denominator, NDCG has no ideal ranking, and runCondition skips
// such queries entirely. That is correct for those ratios and it is also how a
// retrieval system gets to be confidently wrong — a plausible wrong memory
// counts as a hit for whatever it displaced, so a leak is invisible to a
// recall-only suite (the reasoning behind #537's negative retrieval fixtures,
// which assert that a named memory must not surface but still give every query a
// positive answer).
//
// This file measures the missing half directly: queries with NO relevant memory,
// run through the production search path, reporting how many results come back
// and how strongly the best of them scores. The score is the vector leg's
// cosine — the only calibrated number in the pipeline, since an RRF score is a
// function of a row's rank rather than of its match, and the keyword-only
// fallback's synthesized 1/(K+rank+1) is a position, not a confidence.

// NegativeQuery is a no-answer query: the graded query shape with an empty rel
// map, plus the flavor that records how it was built. Off-domain questions share
// no vocabulary with the corpus and measure the floor; near-misses reuse corpus
// vocabulary while asking about something never recorded, which is where a
// score-based abstain rule has to earn its keep.
type NegativeQuery struct {
	QuerySpec
	Flavor string `json:"flavor"`
}

// LoadNegatives reads the no-answer queries. An empty rel map is what makes a
// query negative, so a non-empty one is a hard error rather than a silently
// dropped entry: a mislabelled positive would inflate the false-positive count.
func LoadNegatives(r io.Reader) ([]NegativeQuery, error) {
	var out []NegativeQuery
	if err := decodeJSONL(r, func(raw json.RawMessage) error {
		var q NegativeQuery
		if err := json.Unmarshal(raw, &q); err != nil {
			return err
		}
		switch {
		case q.Name == "":
			return fmt.Errorf("no-answer query with empty name: %q", q.Text)
		case q.Text == "":
			return fmt.Errorf("no-answer query %q has no text", q.Name)
		case q.Flavor == "":
			return fmt.Errorf("no-answer query %q has no flavor", q.Name)
		case len(q.Rel) > 0:
			return fmt.Errorf("no-answer query %q grades %d memories: nothing may answer it, so rel must stay empty", q.Name, len(q.Rel))
		}
		out = append(out, q)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// NegativeQueries returns the dataset's no-answer queries as runnable Queries.
// Their relevance map is empty, which is what the graded metrics already skip,
// and each needs a committed embedding like any other query.
func NegativeQueries(ds Dataset, vecs Vectors) ([]Query, error) {
	queries := make([]Query, 0, len(ds.Negatives))
	for _, nq := range ds.Negatives {
		vec, ok := vecs[nq.Name]
		if !ok {
			return nil, fmt.Errorf("no fixture vector for no-answer query %q (regenerate embeddings)", nq.Name)
		}
		queries = append(queries, Query{
			Name: nq.Name, ProjectID: ds.Project, Text: nq.Text, Vector: vec, Flavor: nq.Flavor,
		})
	}
	return queries, nil
}

// FalsePositiveFloors are the cosines the report counts results against. There
// is no configured floor to inherit — search.min_similarity ships 0, dropping
// only non-positive cosines — so the report sweeps the band a floor would have to
// live in instead of asserting one.
//
// "Above a floor" means strictly above, which is the rule production itself
// applies: memory.filterVectorFloor keeps a candidate when score > floor and
// drops the rest, so a result sitting exactly on a floor would be refused by it.
// Every count in the report follows that one convention, which is also what makes
// NoAnswerMax/Unseparable coherent: a floor set at the no-answer maximum already
// refuses that query, and therefore already refuses any answerable query whose
// best score ties it.
var FalsePositiveFloors = []float32{0.3, 0.4, 0.5}

// resultCosines returns the cosine similarity between queryVec and each of ids,
// keyed by ID.
//
// The scores come from each row's own stored vector rather than from a vector-leg
// list, because a hybrid result can arrive on the keyword leg alone: the keyword
// reservation guarantees the top keyword hits a place in the window while their
// cosines put them below any fetched vector list. Reading those scores out of a
// truncated list yields zero, and a result scored zero counts below every floor —
// so a short list undercounts precisely the rows that came from the leg with no
// score of its own, and the top result of a keyword-led window is one of them.
//
// A row absent from the result has no embedding, was embedded under a
// different model than this query, or has dimensions that do not match the
// query's — none of which gives it a comparable score — and is reported here
// as 0: below every floor this report counts, which is the right answer for a
// row with no score to report.
func resultCosines(ctx context.Context, store *memory.Store, queryVec []float32, ids []string) (map[string]float32, error) {
	scored, err := store.EmbeddingCosines(ctx, ids, queryVec)
	if err != nil {
		return nil, err
	}
	cosines := make(map[string]float32, len(ids))
	for _, id := range ids {
		cosines[id] = scored[id] // absent reads as 0; see above
	}
	return cosines, nil
}

// scoredWindow runs the production search for one query and scores every row it
// returns with that row's true cosine, together with the best of them.
//
// Scoring the returned window rather than the vector leg's own top-k is what
// makes the two halves of the report comparable: "the score of the best thing
// this query would show you" is one definition, and both the no-answer set and
// the answerable contrast have to measure it the same way or the gap between
// them means nothing. The two can genuinely differ — the keyword reservation
// admits a row the vector leg ranked far down, and that row is what the caller is
// shown.
//
// Only the answerable contrast still needs it: the no-answer side of the report
// is measured by the runner (measureNoAnswer), which scores the same way and
// hands the numbers over rather than searching twice.
func scoredWindow(ctx context.Context, store *memory.Store, q Query) ([]memory.Memory, map[string]float32, float32, error) {
	results, err := store.SearchHybrid(ctx, q.ProjectID, q.Text, q.Vector, scoreK)
	if err != nil {
		return nil, nil, 0, err
	}
	ids := make([]string, len(results))
	for i, m := range results {
		ids[i] = m.ID
	}
	cosines, err := resultCosines(ctx, store, q.Vector, ids)
	if err != nil {
		return nil, nil, 0, err
	}
	var top float32
	for _, c := range cosines {
		if c > top {
			top = c
		}
	}
	return results, cosines, top, nil
}

// FloorCount is what one candidate floor would mean for the no-answer set.
type FloorCount struct {
	Floor float32
	// Results is the mean number of returned results clearing the floor.
	Results float64
	// Queries is how many no-answer queries returned at least one result
	// clearing it — an abstention rule at this floor would have to refuse
	// Queries of the Queries measured to be right every time.
	Queries int
	// Rate is Queries as a fraction of the queries measured, which is the
	// number the report prints: the false-positive rate AT THIS FLOOR. A count
	// is not comparable across conditions with different query counts, and
	// Rate beside Queries is what says so out loud.
	Rate float64
}

// NoAnswerSummary is one condition's no-answer measurement, aggregated.
type NoAnswerSummary struct {
	Condition string
	Queries   int // no-answer queries measured
	// MeanResults is the mean number of results returned per query, at scoreK
	// with no similarity floor configured — today always a full window.
	MeanResults float64
	// MeanTop is the mean best returned cosine. It is the score-graded half of
	// the rate: with the floor at 0 every query is a false positive, so the
	// rate alone says only that Ghost never abstains, and the mean says how
	// confident the answers it returned anyway were.
	MeanTop float64
	// MaxTop is the highest cosine any no-answer query produced, so it is the
	// floor that would refuse every one of them.
	MaxTop float64
	// Floors is one row per configured floor, in FalsePositiveFloors order.
	Floors []FloorCount
}

// measureNoAnswer runs one no-answer query through a condition and scores every
// row it returns with that row's own cosine (see resultCosines for why the score
// has to come from the stored vector rather than from the leg's own list).
func measureNoAnswer(ctx context.Context, store *memory.Store, q Query, rank rankFn) (NoAnswerQuery, error) {
	ranked, err := rank(q)
	if err != nil {
		return NoAnswerQuery{}, err
	}
	cosines, err := resultCosines(ctx, store, q.Vector, ranked)
	if err != nil {
		return NoAnswerQuery{}, err
	}
	var top float32
	for _, c := range cosines {
		if c > top {
			top = c
		}
	}
	return NoAnswerQuery{
		Name: q.Name, Flavor: q.Flavor, Results: len(ranked),
		Top: float64(top), Cosines: cosines,
	}, nil
}

// SummarizeNoAnswer aggregates one condition's no-answer measurements. Every
// returned row is scored from its own vector, so the floor rows are counts over
// real scores rather than over a position.
func SummarizeNoAnswer(condition string, measured []NoAnswerQuery) NoAnswerSummary {
	sum := NoAnswerSummary{Condition: condition, Queries: len(measured)}
	if len(measured) == 0 {
		return sum
	}
	rows := make([]FloorCount, len(FalsePositiveFloors))
	for i, floor := range FalsePositiveFloors {
		rows[i] = FloorCount{Floor: floor}
	}
	n := float64(len(measured))
	for _, m := range measured {
		sum.MeanResults += float64(m.Results)
		sum.MeanTop += m.Top
		if m.Top > sum.MaxTop {
			sum.MaxTop = m.Top
		}
		for i, floor := range FalsePositiveFloors {
			hits := countHitsAboveFloor(m.Cosines, floor)
			rows[i].Results += float64(hits)
			if hits > 0 {
				rows[i].Queries++
			}
		}
	}
	sum.MeanResults /= n
	sum.MeanTop /= n
	for i := range rows {
		rows[i].Results /= n
		rows[i].Rate = float64(rows[i].Queries) / n
	}
	sum.Floors = rows
	return sum
}

// countHitsAboveFloor is how many of a query's returned rows clear floor. It is
// a named function because the same rule is counted two ways in this package —
// per floor for the rate (does the query leak at all at this floor) and per row
// for the results-per-query count — and both must use the same strict
// comparison or the two tables disagree.
func countHitsAboveFloor(cosines map[string]float32, floor float32) int {
	hits := 0
	for _, c := range cosines {
		if c > floor {
			hits++
		}
	}
	return hits
}

// FormatNoAnswer renders the per-condition false-positive table, printed
// directly under the graded table. The rate columns are per floor because with
// search.min_similarity shipping 0 the "returned anything" rate is 1.000 on
// every condition and says only that Ghost never abstains; the graded columns
// say how wrong those answers were.
func FormatNoAnswer(summaries []NoAnswerSummary) string {
	if len(summaries) == 0 || summaries[0].Queries == 0 {
		return ""
	}
	var b bytes.Buffer
	n := summaries[0].Queries
	fmt.Fprintf(&b, "\nno-answer queries (n=%d, nothing in the corpus answers these; report-only, no gate)\n", n)
	fmt.Fprintf(&b, "  A FALSE POSITIVE is a result returned for a query with no answer. The false-positive rate is the\n")
	fmt.Fprintf(&b, "  share of queries with at least one returned row above that cosine floor; production keeps a row\n")
	fmt.Fprintf(&b, "  only when score > floor, and search.min_similarity ships 0, so at the shipped setting every column\n")
	fmt.Fprintf(&b, "  reads 1.000 and the mean top cosine is the only graded part of this table.\n\n")
	floors := summaries[0].Floors
	fmt.Fprintf(&b, "  %-14s %8s %11s", "condition", "results", "mean top")
	for _, f := range floors {
		fmt.Fprintf(&b, " %11s", fmt.Sprintf("rate @%.2f", f.Floor))
	}
	b.WriteString("\n")
	for _, s := range summaries {
		fmt.Fprintf(&b, "  %-14s %8.1f %11.3f", s.Condition, s.MeanResults, s.MeanTop)
		for _, f := range s.Floors {
			fmt.Fprintf(&b, " %11.3f", f.Rate)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// NoAnswerFor returns one condition's no-answer measurement from a completed
// run, or nil when that condition measured none. It is the seam between the
// runner (which measured all three conditions) and the deeper report (which is
// about the shipped hybrid path), so the two tables cannot describe two
// different searches.
func NoAnswerFor(results []Result, condition string) []NoAnswerQuery {
	for _, r := range results {
		if r.Condition == condition {
			return r.NoAnswer
		}
	}
	return nil
}

// FlavorStat is one flavor of no-answer query, kept apart because the two are
// not interchangeable: the off-domain set is the floor, and the near-miss set is
// the case an abstain rule actually has to survive. Reporting only their pooled
// mean would let the easy flavor flatter the hard one.
type FlavorStat struct {
	Flavor      string
	Queries     int
	MeanResults float64
	MeanTop     float64
}

// FalsePositiveReport is the abstention baseline: what the production search path
// returns for queries nothing in the corpus answers, and how that compares with
// what it returns for queries something does answer.
type FalsePositiveReport struct {
	Queries    int // no-answer queries measured
	Answerable int // answerable queries measured, for contrast
	// MeanResults is the mean number of results returned per no-answer query
	// (at scoreK, with no floor configured — today always a full window).
	MeanResults float64
	// MeanTop is the mean best returned cosine over no-answer queries, and
	// AnswerableTop the same over answerable ones. The gap between them is the
	// headroom a score threshold has to work with.
	MeanTop       float64
	AnswerableTop float64
	// NoAnswerMax is the highest returned cosine any no-answer query produced, so
	// it is the floor that would refuse every one of them. Unseparable is what
	// that floor costs: the answerable queries scoring at or below it, which a
	// floor that high refuses as well — a tie included, because the production
	// floor keeps a candidate only when its score is strictly above it. A mean
	// alone would hide this: the means separate cleanly while the two
	// distributions still overlap.
	NoAnswerMax float64
	Unseparable int
	// Flavors splits the set by how each query was built, sorted by name.
	Flavors []FlavorStat
	Floors  []FloorCount
}

// FalsePositives is the deeper half of the no-answer report: the answerable
// contrast, the per-flavor split, the maximum a floor would have to clear, and
// what that floor costs. The no-answer half it reports is taken from the runner's
// own measurement of the shipped hybrid path (Result.NoAnswer) rather than
// searched a second time, so the two tables cannot describe two different runs.
//
// Each returned row was scored with its own true cosine (see resultCosines), so
// a row that reached the window on the keyword leg alone is measured rather than
// assumed to be a non-match.
//
// One caveat worth stating, because it is the difference between this report and
// the shipped flag: production applies a configured floor to the vector leg
// *before* fusion, so a keyword-only result is exempt from it entirely. The
// "results above a floor" rows here score every returned row against the floor
// anyway, which is a stricter diagnostic reading — it answers "how strong are the
// results a caller actually receives", not "what would the flag do".
func FalsePositives(ctx context.Context, store *memory.Store, noAnswer []NoAnswerQuery, answerable []Query) (FalsePositiveReport, error) {
	rep := FalsePositiveReport{Queries: len(noAnswer), Answerable: len(answerable)}
	if len(noAnswer) == 0 {
		return rep, nil
	}

	var sumResults, sumTop float64
	answerableTops := make([]float64, 0, len(answerable))
	byFlavor := map[string]*FlavorStat{}
	for _, q := range noAnswer {
		sumResults += float64(q.Results)
		sumTop += q.Top
		if q.Top > rep.NoAnswerMax {
			rep.NoAnswerMax = q.Top
		}
		stat := byFlavor[q.Flavor]
		if stat == nil {
			stat = &FlavorStat{Flavor: q.Flavor}
			byFlavor[q.Flavor] = stat
		}
		stat.Queries++
		stat.MeanResults += float64(q.Results)
		stat.MeanTop += q.Top
		for _, floor := range FalsePositiveFloors {
			above := countHitsAboveFloor(q.Cosines, floor)
			row := floorRow(&rep.Floors, floor)
			row.Results += float64(above)
			if above > 0 {
				row.Queries++
			}
		}
	}
	n := float64(len(noAnswer))
	rep.MeanResults = sumResults / n
	rep.MeanTop = sumTop / n
	rep.Flavors = flavorStats(byFlavor)
	for i := range rep.Floors {
		rep.Floors[i].Rate = float64(rep.Floors[i].Queries) / n
	}

	// The answerable contrast measures the same thing the no-answer set does —
	// the best cosine among the rows production would return — so the two
	// distributions differ only in whether the corpus has an answer.
	for _, q := range answerable {
		_, _, top, err := scoredWindow(ctx, store, q)
		if err != nil {
			return FalsePositiveReport{}, fmt.Errorf("answerable query %q: %w", q.Name, err)
		}
		answerableTops = append(answerableTops, float64(top))
		rep.AnswerableTop += float64(top)
	}
	if len(answerable) > 0 {
		rep.AnswerableTop /= float64(len(answerable))
		for _, top := range answerableTops {
			if top <= rep.NoAnswerMax {
				rep.Unseparable++
			}
		}
	}
	return rep, nil
}

// flavorStats converts the per-flavor accumulators into their reported means,
// sorted by flavor name so the table order does not depend on map iteration.
func flavorStats(byFlavor map[string]*FlavorStat) []FlavorStat {
	out := make([]FlavorStat, 0, len(byFlavor))
	for _, s := range byFlavor {
		if s.Queries > 0 {
			s.MeanResults /= float64(s.Queries)
			s.MeanTop /= float64(s.Queries)
		}
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Flavor < out[j].Flavor })
	return out
}

// floorRow returns the accumulator for one floor, appending it on first sight so
// the report's row order follows FalsePositiveFloors rather than the order the
// queries happened to clear a floor in. It takes the slice by pointer because
// appending inside the callee would otherwise grow a copy and leave the caller's
// header empty.
func floorRow(rows *[]FloorCount, floor float32) *FloorCount {
	for i := range *rows {
		if (*rows)[i].Floor == floor {
			return &(*rows)[i]
		}
	}
	*rows = append(*rows, FloorCount{Floor: floor})
	return &(*rows)[len(*rows)-1]
}

// FormatFalsePositives renders the abstention baseline for one condition. The
// header names the condition because the per-condition false-positive table above
// it reports all three, and a reader must not take the numbers here for a
// property of the legs as well as of the shipped path.
func FormatFalsePositives(rep FalsePositiveReport, condition string) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "\nabstention baseline (%s path, the shipped ranking)\n", condition)
	fmt.Fprintf(&b, "  results returned per query   %.1f (window %d, no similarity floor configured)\n", rep.MeanResults, scoreK)
	fmt.Fprintf(&b, "  mean top cosine             %.3f  vs %.3f for the %d answerable queries\n",
		rep.MeanTop, rep.AnswerableTop, rep.Answerable)
	fmt.Fprintf(&b, "  floor refusing all of them   %.3f (the no-answer maximum) costs %d/%d answerable queries\n",
		rep.NoAnswerMax, rep.Unseparable, rep.Answerable)
	if len(rep.Flavors) > 0 {
		fmt.Fprintf(&b, "\n  %-12s %4s %16s %12s\n", "flavor", "n", "results/query", "mean top")
		for _, f := range rep.Flavors {
			fmt.Fprintf(&b, "  %-12s %4d %16.1f %12.3f\n", f.Flavor, f.Queries, f.MeanResults, f.MeanTop)
		}
	}
	fmt.Fprintf(&b, "\n  %-8s %14s %18s %8s\n", "floor", "results/query", "queries w/ hit", "rate")
	for _, f := range rep.Floors {
		fmt.Fprintf(&b, "  %-8.2f %14.2f %10d/%-5d %8.3f\n", f.Floor, f.Results, f.Queries, rep.Queries, f.Rate)
	}
	return b.String()
}
