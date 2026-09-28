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
	// rankedStateMemories, rankedStateQueries and rankedStateVectors name the
	// committed corpus, questions and embedding fixture. Only the directory is
	// a constant: the loader is handed a directory so a test can read the same
	// suite from a fixture it built itself.
	rankedStateMemories = "ranked_memories.jsonl"
	rankedStateQueries  = "ranked_queries.jsonl"
	rankedStateVectors  = "ranked_embeddings.json"
)

// LoadRankedStateDataset reads the suite's corpus and questions. It has its own
// loader rather than going through LoadDatasetFiles because there is no
// no-answer half here: this suite grades retrieval over a corpus carrying
// maintenance state, and the abstention baseline belongs to the corpus the
// headline table uses.
func LoadRankedStateDataset(dir string) (Dataset, error) {
	mems, err := loadFile(dir+"/"+rankedStateMemories, LoadMemories)
	if err != nil {
		return Dataset{}, err
	}
	qs, err := loadFile(dir+"/"+rankedStateQueries, LoadQueries)
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
	f, err := os.Open(dir + "/" + rankedStateVectors)
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

// FormatRankedState renders the suite as one row per configuration, with the
// single legs printed once as the reference they do not depend on.
func FormatRankedState(configs []RankedStateConfig, results [][]Result) string {
	var b bytes.Buffer
	b.WriteString("ranking-state suite (graded, same vector space as the headline table)\n")
	b.WriteString("  the corpus carries distinct created_at and supersedes edges, so decay and the\n")
	b.WriteString("  supersede demote are both live here; on the headline table both are inert.\n\n")
	fmt.Fprintf(&b, "%-20s %-12s %7s %7s %8s %8s\n", "configuration", "condition", "R@1", "R@5", "MRR@10", "NDCG@10")
	for i, cfg := range configs {
		if i >= len(results) {
			break
		}
		if i == 0 {
			for _, ref := range results[i][:2] {
				fmt.Fprintf(&b, "%-20s %-12s %7.3f %7.3f %8.3f %8.3f\n",
					"reference (no params)", ref.Condition, ref.Recall1, ref.Recall5, ref.MRR10, ref.NDCG10)
			}
		}
		h := results[i][2]
		fmt.Fprintf(&b, "%-20s %-12s %7.3f %7.3f %8.3f %8.3f\n",
			cfg.Label, CondHybrid, h.Recall1, h.Recall5, h.MRR10, h.NDCG10)
	}
	if len(results) > 0 {
		fmt.Fprintf(&b, "\n%d graded queries. The two single legs take no SearchParams, so they are the\n", results[0][0].Queries)
		b.WriteString("same in every configuration and are printed once.\n")
	}
	return b.String()
}
