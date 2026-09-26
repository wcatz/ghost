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
// A row absent from the result has no embedding, or one whose dimensions do not
// match the query's, and is reported here as 0: below every floor this report
// counts, which is the right answer for a row with no comparable vector.
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

// countAboveFloor counts the results whose cosine is strictly above floor. It is
// a function rather than a loop so the boundary rule has a test of its own: real
// cosines essentially never land exactly on a floor, so the rule cannot be pinned
// through the corpus, only stated (see FalsePositiveFloors) and asserted here.
func countAboveFloor(cosines map[string]float32, results []memory.Memory, floor float32) int {
	above := 0
	for _, m := range results {
		if cosines[m.ID] > floor {
			above++
		}
	}
	return above
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

// FalsePositives runs the production search for every no-answer query and, for
// contrast, does the same for every answerable one. Each returned row is scored
// with its own true cosine (see resultCosines), so a row that reached the window
// on the keyword leg alone is measured rather than assumed to be a non-match.
//
// One caveat worth stating, because it is the difference between this report and
// the shipped flag: production applies a configured floor to the vector leg
// *before* fusion, so a keyword-only result is exempt from it entirely. The
// "results above a floor" rows here score every returned row against the floor
// anyway, which is a stricter diagnostic reading — it answers "how strong are the
// results a caller actually receives", not "what would the flag do".
func FalsePositives(ctx context.Context, store *memory.Store, noAnswer, answerable []Query) (FalsePositiveReport, error) {
	rep := FalsePositiveReport{Queries: len(noAnswer), Answerable: len(answerable)}
	if len(noAnswer) == 0 {
		return rep, nil
	}

	var sumResults, sumTop float64
	answerableTops := make([]float64, 0, len(answerable))
	byFlavor := map[string]*FlavorStat{}
	for _, q := range noAnswer {
		results, cosines, top, err := scoredWindow(ctx, store, q)
		if err != nil {
			return FalsePositiveReport{}, fmt.Errorf("no-answer query %q: %w", q.Name, err)
		}
		sumResults += float64(len(results))
		sumTop += float64(top)
		if float64(top) > rep.NoAnswerMax {
			rep.NoAnswerMax = float64(top)
		}
		stat := byFlavor[q.Flavor]
		if stat == nil {
			stat = &FlavorStat{Flavor: q.Flavor}
			byFlavor[q.Flavor] = stat
		}
		stat.Queries++
		stat.MeanResults += float64(len(results))
		stat.MeanTop += float64(top)
		for _, floor := range FalsePositiveFloors {
			above := countAboveFloor(cosines, results, floor)
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

// FormatFalsePositives renders the no-answer baseline.
func FormatFalsePositives(rep FalsePositiveReport) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "\nno-answer queries (n=%d, nothing in the corpus answers these; report-only, no gate)\n", rep.Queries)
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
	fmt.Fprintf(&b, "\n  %-8s %14s %16s\n", "floor", "results/query", "queries w/ hit")
	for _, f := range rep.Floors {
		fmt.Fprintf(&b, "  %-8.2f %14.2f %10d/%-5d\n", f.Floor, f.Results/float64(rep.Queries), f.Queries, rep.Queries)
	}
	return b.String()
}
