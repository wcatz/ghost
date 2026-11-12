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

// NegativeQuery is a no-answer query.
type NegativeQuery struct {
	QuerySpec
	Flavor string `json:"flavor"`
}

// LoadNegatives reads the no-answer queries.
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

// FalsePositiveFloors are the cosines the report counts results against.
var FalsePositiveFloors = []float32{0.3, 0.4, 0.5}

func resultCosines(ctx context.Context, store *memory.Store, queryVec []float32, ids []string) (map[string]float32, error) {
	scored, err := store.EmbeddingCosines(ctx, ids, queryVec)
	if err != nil {
		return nil, err
	}
	cosines := make(map[string]float32, len(ids))
	for _, id := range ids {
		cosines[id] = scored[id]
	}
	return cosines, nil
}

// FloorCount is what one candidate floor would mean for the no-answer set.
type FloorCount struct {
	Floor   float32
	Results float64
	Queries int
	Rate    float64
}

// NoAnswerSummary is one condition's no-answer measurement.
type NoAnswerSummary struct {
	Condition   string
	Queries     int
	MeanResults float64
	MeanTop     float64
	MaxTop      float64
	Floors      []FloorCount
}

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

// SummarizeNoAnswer aggregates one condition's no-answer measurements.
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

func countHitsAboveFloor(cosines map[string]float32, floor float32) int {
	hits := 0
	for _, c := range cosines {
		if c > floor {
			hits++
		}
	}
	return hits
}

// FormatNoAnswer renders the per-condition false-positive table.
func FormatNoAnswer(summaries []NoAnswerSummary) string {
	measured := make([]NoAnswerSummary, 0, len(summaries))
	for _, s := range summaries {
		if s.Queries > 0 {
			measured = append(measured, s)
		}
	}
	if len(measured) == 0 {
		return ""
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, "\nno-answer queries (n=%d, nothing in the corpus answers these; report-only, no gate)\n", measured[0].Queries)
	b.WriteString("  A FALSE POSITIVE is a result returned for a query with no answer, and the false-positive rate is\n")
	b.WriteString("  the share of queries with at least one returned row above that cosine floor. search.min_similarity\n")
	b.WriteString("  ships 0, which refuses nothing, so at the shipped setting the rate is 1.000 in every condition;\n")
	b.WriteString("  the floors below are the graded reading, and they are the band an abstention rule would live in.\n\n")
	fmt.Fprintf(&b, "  %-14s %8s %11s %8s", "condition", "results", "mean top", "max top")
	for _, f := range measured[0].Floors {
		fmt.Fprintf(&b, " %11s", fmt.Sprintf("rate @%.2f", f.Floor))
	}
	b.WriteString("\n")
	for _, s := range measured {
		fmt.Fprintf(&b, "  %-14s %8.1f %11.3f %8.3f", s.Condition, s.MeanResults, s.MeanTop, s.MaxTop)
		for _, f := range s.Floors {
			fmt.Fprintf(&b, " %11.3f", f.Rate)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// NoAnswerFor returns one condition's no-answer measurement from a completed run.
func NoAnswerFor(results []Result, condition string) []NoAnswerQuery {
	for _, r := range results {
		if r.Condition == condition {
			return r.NoAnswer
		}
	}
	return nil
}

// PerQueryFor returns one condition's per-query scores from a completed run.
func PerQueryFor(results []Result, condition string) []QueryScore {
	for _, r := range results {
		if r.Condition == condition {
			return r.PerQuery
		}
	}
	return nil
}

// FlavorStat is one flavor of no-answer query.
type FlavorStat struct {
	Flavor      string
	Queries     int
	MeanResults float64
	MeanTop     float64
}

// FalsePositiveReport is the abstention baseline.
type FalsePositiveReport struct {
	Queries       int
	Answerable    int
	MeanResults   float64
	MeanTop       float64
	AnswerableTop float64
	NoAnswerMax   float64
	Unseparable   int
	Flavors       []FlavorStat
	Floors        []FloorCount
}

// FalsePositives computes the abstention baseline from measured results.
func FalsePositives(noAnswer []NoAnswerQuery, answerable []QueryScore) (FalsePositiveReport, error) {
	rep := FalsePositiveReport{Queries: len(noAnswer), Answerable: len(answerable)}
	if len(noAnswer) == 0 {
		return rep, nil
	}

	var sumResults, sumTop float64
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
		rep.Floors[i].Results /= n
		rep.Floors[i].Rate = float64(rep.Floors[i].Queries) / n
	}

	answerableTops := make([]float64, 0, len(answerable))
	for _, q := range answerable {
		answerableTops = append(answerableTops, q.TopCosine)
		rep.AnswerableTop += q.TopCosine
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

func floorRow(rows *[]FloorCount, floor float32) *FloorCount {
	for i := range *rows {
		if (*rows)[i].Floor == floor {
			return &(*rows)[i]
		}
	}
	*rows = append(*rows, FloorCount{Floor: floor})
	return &(*rows)[len(*rows)-1]
}

// FormatFalsePositives renders the abstention baseline for one condition.
func FormatFalsePositives(rep FalsePositiveReport, condition string) string {
	if rep.Queries == 0 {
		return fmt.Sprintf("\nabstention baseline (%s path): not measured, no no-answer queries in this run\n", condition)
	}
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
