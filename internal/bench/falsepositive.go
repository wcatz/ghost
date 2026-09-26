package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

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
var FalsePositiveFloors = []float32{0.3, 0.4, 0.5}

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

// FalsePositiveReport is the abstention baseline: what the production search path
// returns for queries nothing in the corpus answers, and how that compares with
// what it returns for queries something does answer.
type FalsePositiveReport struct {
	Queries    int // no-answer queries measured
	Answerable int // answerable queries measured, for contrast
	// MeanResults is the mean number of results returned per no-answer query
	// (at scoreK, with no floor configured — today always a full window).
	MeanResults float64
	// MeanTop is the mean best cosine over no-answer queries, and
	// AnswerableTop the same over answerable ones. The gap between them is the
	// headroom a score threshold has to work with.
	MeanTop       float64
	AnswerableTop float64
	// NoAnswerMax is the highest top cosine any no-answer query produced, so it
	// is the floor that would refuse every one of them. Unseparable is what
	// that floor costs: the answerable queries scoring at or below it, which a
	// threshold that high would refuse as well. A mean alone would hide this —
	// the means separate cleanly while the two distributions still overlap.
	NoAnswerMax float64
	Unseparable int
	Floors      []FloorCount
}

// FalsePositives runs the production search for every no-answer query and, for
// contrast, reads the best cosine of every answerable query. Results are
// attributed a score by looking them up in the vector leg, which is the leg
// that has one.
func FalsePositives(ctx context.Context, store *memory.Store, noAnswer, answerable []Query) (FalsePositiveReport, error) {
	rep := FalsePositiveReport{Queries: len(noAnswer), Answerable: len(answerable)}
	if len(noAnswer) == 0 {
		return rep, nil
	}

	var sumResults, sumTop float64
	answerableTops := make([]float64, 0, len(answerable))
	for _, q := range noAnswer {
		scored, err := store.SearchVector(ctx, q.ProjectID, q.Vector, scoreK*2)
		if err != nil {
			return FalsePositiveReport{}, fmt.Errorf("no-answer query %q: vector leg: %w", q.Name, err)
		}
		cosine := make(map[string]float32, len(scored))
		var top float32
		for _, s := range scored {
			cosine[s.MemoryID] = s.Score
			if s.Score > top {
				top = s.Score
			}
		}
		results, err := store.SearchHybrid(ctx, q.ProjectID, q.Text, q.Vector, scoreK)
		if err != nil {
			return FalsePositiveReport{}, fmt.Errorf("no-answer query %q: hybrid search: %w", q.Name, err)
		}
		sumResults += float64(len(results))
		sumTop += float64(top)
		if float64(top) > rep.NoAnswerMax {
			rep.NoAnswerMax = float64(top)
		}
		for _, floor := range FalsePositiveFloors {
			above := 0
			for _, m := range results {
				if cosine[m.ID] >= floor {
					above++
				}
			}
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

	for _, q := range answerable {
		scored, err := store.SearchVector(ctx, q.ProjectID, q.Vector, 1)
		if err != nil {
			return FalsePositiveReport{}, fmt.Errorf("answerable query %q: vector leg: %w", q.Name, err)
		}
		top := 0.0
		if len(scored) > 0 {
			top = float64(scored[0].Score)
		}
		answerableTops = append(answerableTops, top)
		rep.AnswerableTop += top
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
	fmt.Fprintf(&b, "\n  %-8s %14s %16s\n", "floor", "results/query", "queries w/ hit")
	for _, f := range rep.Floors {
		fmt.Fprintf(&b, "  %-8.2f %14.2f %10d/%-5d\n", f.Floor, f.Results/float64(rep.Queries), f.Queries, rep.Queries)
	}
	return b.String()
}
