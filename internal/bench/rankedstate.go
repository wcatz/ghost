package bench

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"

	"github.com/wcatz/ghost/internal/memory"
)

// The ranking-state suite is the graded answer to "what does the headline table
// not see". That corpus seeds through store.Create, which stamps one created_at
// for every row, so the decay factor is identical across every candidate and
// cannot reorder them; and it holds no supersedes edge, so the demote is a hard
// no-op. Both are properties of that corpus rather than of the ranker, and both
// mean the headline NDCG is a measurement of relevance fusion alone — a number
// that reads the same before and after either ranking path ships.
//
// This suite is the same measurement with the state those two paths consume
// present: distinct created_at across decaying and never-decay categories, and
// eight supersession chains written through the production link writer. It
// reuses the headline dataset's grading conventions (one binary-relevant answer
// per probe) and its vector space (nomic-embed-text:v1.5 with the task prefixes
// internal/embedding applies), so its numbers are read next to the headline
// table rather than instead of it.
//
// The report prints all four configurations — both off, decay alone, demote
// alone, both on — because a single number cannot say which path moved it. Only
// the hybrid row moves: SearchFTS and SearchVector take no SearchParams, so the
// two single-leg rows are the same in every configuration and are printed once
// as the reference the fused row is read against.

const (
	// RankedStateProject is the project the suite seeds and searches.
	RankedStateProject = "rankedstate"
	// withStateMemories, withStateQueries and withStateVectors name the
	// committed corpus, questions and embedding fixture. Only the directory is
	// a constant: the loader is handed a directory so a test can read the same
	// suite from a fixture it built itself.
	//
	// The files are `withstate_*` and not `ranked_*` on purpose: .gitignore
	// carries `ranked*.jsonl` for the Phase 4 harness's LOCAL retrieval output,
	// and a fixture named `ranked_memories.jsonl` sits under that pattern and is
	// silently never committed — a failure that only shows up where the fixture
	// is absent, not where it was written.
	withStateMemories = "withstate_memories.jsonl"
	withStateQueries  = "withstate_queries.jsonl"
	withStateVectors  = "withstate_embeddings.json"
)

// LoadRankedStateDataset reads the suite's corpus and questions. It has its own
// loader rather than going through LoadDatasetFiles because there is no
// no-answer half here: this suite grades retrieval over a corpus carrying
// maintenance state, and the abstention baseline belongs to the corpus the
// headline table uses.
func LoadRankedStateDataset(dir string) (Dataset, error) {
	mems, err := loadFile(dir+"/"+withStateMemories, LoadMemories)
	if err != nil {
		return Dataset{}, err
	}
	qs, err := loadFile(dir+"/"+withStateQueries, LoadQueries)
	if err != nil {
		return Dataset{}, err
	}
	return Dataset{Project: RankedStateProject, Memories: mems, Queries: qs}, nil
}

// SeedRankedState loads and seeds the suite from dir, returning the runnable
// queries. Every cross-reference is resolved by Seed, so a corpus naming a key
// that does not exist fails here rather than scoring silently.
func SeedRankedState(ctx context.Context, store *memory.Store, db *sql.DB, dir string) ([]Query, error) {
	ds, err := LoadRankedStateDataset(dir)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(dir + "/" + withStateVectors)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck
	vecs, err := LoadVectors(f)
	if err != nil {
		return nil, err
	}
	return Seed(ctx, store, db, ds, vecs)
}

// RankedStateConfig is one named configuration the suite is measured under. A
// named type rather than a bare bool pair so a report column can be read without
// the code that produced it, and so a configuration is a value a test can hold
// and compare.
type RankedStateConfig struct {
	Label  string
	Params memory.SearchParams
}

// RankedStateConfigs are the four configurations the suite is measured under, in
// report order: both paths off, each alone, then the shipped pair.
func RankedStateConfigs() []RankedStateConfig {
	bothOff := memory.DefaultSearchParams()
	bothOff.DecayEnabled = false
	bothOff.SupersedeDemote = false
	decayOnly := memory.DefaultSearchParams()
	decayOnly.SupersedeDemote = false
	demoteOnly := memory.DefaultSearchParams()
	demoteOnly.DecayEnabled = false
	return []RankedStateConfig{
		{"both off", bothOff},
		{"decay only", decayOnly},
		{"demote only", demoteOnly},
		{"both on (shipped)", memory.DefaultSearchParams()},
	}
}

// RankedStateAnswer is one probe's answer rank under one configuration. It is
// carried per probe rather than only as R@1 because the configuration table's
// interesting column is not "how many" but "which": the shipped pair does not
// lose a uniformly distributed set of probes, it loses the ones whose answer is
// old, and a reader deciding whether that matters to them needs to see which.
type RankedStateAnswer struct {
	Probe string
	Rank  int // 1-based; 0 means the answer was not in the window at all
}

// RankedStateRun is one configuration's measurement: the aggregate Result and
// the per-probe ranks behind it.
type RankedStateRun struct {
	Config  RankedStateConfig
	Result  []Result
	Answers []RankedStateAnswer
}

// RunRankedState evaluates the suite under explicit SearchParams, per condition.
// The fused leg goes through SearchHybridParams so a configuration can be asked
// for; the two single legs take no params and are reported as the reference.
func RunRankedState(ctx context.Context, store *memory.Store, queries []Query, p memory.SearchParams) ([]Result, error) {
	fts, err := runCondition(ctx, store, CondFTS, queries, func(q Query) ([]string, error) {
		return idsFromMemories(store.SearchFTS(ctx, q.ProjectID, q.Text, scoreK))
	})
	if err != nil {
		return nil, err
	}
	vec, err := runCondition(ctx, store, CondVector, queries, func(q Query) ([]string, error) {
		return idsFromScored(store.SearchVector(ctx, q.ProjectID, q.Vector, scoreK))
	})
	if err != nil {
		return nil, err
	}
	hybrid, err := runCondition(ctx, store, CondHybrid, queries, func(q Query) ([]string, error) {
		return idsFromMemories(store.SearchHybridParams(ctx, q.ProjectID, q.Text, q.Vector, scoreK, p))
	})
	if err != nil {
		return nil, err
	}
	return []Result{fts, vec, hybrid}, nil
}

// AnswerRanks returns each probe's answer rank under one configuration, read off
// the fused leg's own per-query scores is not possible (a score is not a rank), so
// the search is repeated per probe. It is 14 searches and the point is the
// per-probe breakdown the aggregate table cannot show.
func AnswerRanks(ctx context.Context, store *memory.Store, queries []Query, p memory.SearchParams) ([]RankedStateAnswer, error) {
	out := make([]RankedStateAnswer, 0, len(queries))
	for _, q := range queries {
		var answerID string
		for id, gain := range q.Rel {
			if gain > 0 {
				answerID = id
			}
		}
		if answerID == "" {
			return nil, fmt.Errorf("probe %q has no relevant memory, so it has no answer to rank", q.Name)
		}
		results, err := store.SearchHybridParams(ctx, q.ProjectID, q.Text, q.Vector, scoreK, p)
		if err != nil {
			return nil, fmt.Errorf("probe %q: %w", q.Name, err)
		}
		rank := 0
		for i, m := range results {
			if m.ID == answerID {
				rank = i + 1
				break
			}
		}
		out = append(out, RankedStateAnswer{Probe: q.Name, Rank: rank})
	}
	return out, nil
}

// FormatRankedState renders the suite as one row per configuration, with the
// single legs printed once as the reference they do not depend on. A
// configuration with fewer than three conditions is skipped rather than indexed
// into: RunRankedState always returns three, and a formatter that panics on
// anything else is a formatter the next caller cannot use.
func FormatRankedState(runs []RankedStateRun) string {
	var b bytes.Buffer
	b.WriteString("ranking-state suite (graded, same vector space as the headline table)\n")
	b.WriteString("  the corpus carries distinct created_at and supersedes edges, so decay and the\n")
	b.WriteString("  supersede demote are both live here; on the headline table both are inert.\n\n")
	fmt.Fprintf(&b, "%-20s %-12s %7s %7s %8s %8s\n", "configuration", "condition", "R@1", "R@5", "MRR@10", "NDCG@10")
	printed := 0
	for _, run := range runs {
		if len(run.Result) < 3 {
			continue
		}
		if printed == 0 {
			for _, ref := range run.Result[:2] {
				fmt.Fprintf(&b, "%-20s %-12s %7.3f %7.3f %8.3f %8.3f\n",
					"reference (no params)", ref.Condition, ref.Recall1, ref.Recall5, ref.MRR10, ref.NDCG10)
			}
		}
		printed++
		h := run.Result[2]
		fmt.Fprintf(&b, "%-20s %-12s %7.3f %7.3f %8.3f %8.3f\n",
			run.Config.Label, CondHybrid, h.Recall1, h.Recall5, h.MRR10, h.NDCG10)
	}
	if printed == 0 {
		return b.String() + "  (no complete configuration to report)\n"
	}

	// The per-probe grid, which is the part the aggregate table cannot show: one
	// column per configuration, one row per probe, so "which probes does the
	// shipped default lose" is a thing a reader can answer.
	names := make([]string, 0, len(runs))
	for _, run := range runs {
		names = append(names, run.Config.Label)
	}
	fmt.Fprintf(&b, "\n%-24s", "answer rank by probe")
	for _, n := range names {
		fmt.Fprintf(&b, " %12s", n)
	}
	b.WriteString("\n")
	probes := map[string]struct{}{}
	order := []string{}
	for _, run := range runs {
		for _, a := range run.Answers {
			if _, seen := probes[a.Probe]; !seen {
				probes[a.Probe] = struct{}{}
				order = append(order, a.Probe)
			}
		}
	}
	byProbe := map[string]map[string]int{}
	for _, run := range runs {
		for _, a := range run.Answers {
			if byProbe[a.Probe] == nil {
				byProbe[a.Probe] = map[string]int{}
			}
			byProbe[a.Probe][run.Config.Label] = a.Rank
		}
	}
	for _, probe := range order {
		fmt.Fprintf(&b, "%-24s", probe)
		for _, n := range names {
			rank, ok := byProbe[probe][n]
			cell := "-"
			if ok {
				cell = fmt.Sprintf("%d", rank)
				if rank == 0 {
					cell = "absent"
				}
			}
			fmt.Fprintf(&b, " %12s", cell)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\n%d graded queries. The two single legs take no SearchParams, so they are the\n", runs[0].Result[0].Queries)
	b.WriteString("same in every configuration and are printed once. Answer rank 1 is the top result.\n")
	return b.String()
}
