package bench

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"

	"github.com/wcatz/ghost/internal/memory"
)

const (
	// RankedStateProject is the project the suite seeds and searches.
	RankedStateProject = "rankedstate"
	withStateMemories  = "withstate_memories.jsonl"
	withStateQueries   = "withstate_queries.jsonl"
	withStateVectors   = "withstate_embeddings.json"
)

// LoadRankedStateDataset reads the suite's corpus and questions.
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

// SeedRankedState loads and seeds the suite from dir.
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

// RankedStateConfig is one named configuration the suite is measured under.
type RankedStateConfig struct {
	Label  string
	Params memory.SearchParams
}

// RankedStateConfigs are the configurations the suite is measured under.
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

// RankedStateAnswer is one probe's answer rank under one configuration.
type RankedStateAnswer struct {
	Probe string
	Rank  int
}

// RankedStateRun is one configuration's measurement.
type RankedStateRun struct {
	Config  RankedStateConfig
	Result  []Result
	Answers []RankedStateAnswer
}

// RunRankedState evaluates the suite under explicit SearchParams.
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

// AnswerRanks returns each probe's answer rank under one configuration.
func AnswerRanks(ctx context.Context, store *memory.Store, queries []Query, p memory.SearchParams) ([]RankedStateAnswer, error) {
	out := make([]RankedStateAnswer, 0, len(queries))
	for _, q := range queries {
		answerID, err := singleRelevant(q)
		if err != nil {
			return nil, err
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

func singleRelevant(q Query) (string, error) {
	var found []string
	for id, gain := range q.Rel {
		if gain > 0 {
			found = append(found, id)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", fmt.Errorf("probe %q grades no memory as relevant, so it has no answer to rank", q.Name)
	default:
		return "", fmt.Errorf("probe %q grades %d memories as relevant, so which one is the answer is a question "+
			"this suite cannot answer; the per-probe rank grid needs one", q.Name, len(found))
	}
}

// FormatRankedState renders the ranked-state report.
func FormatRankedState(runs []RankedStateRun) string {
	var b bytes.Buffer
	b.WriteString("ranking-state suite (graded, same vector space as the headline table)\n")
	b.WriteString("  the corpus carries distinct created_at and supersedes edges, so decay and the\n")
	b.WriteString("  supersede demote are both live here; on the headline table both are inert.\n\n")
	fmt.Fprintf(&b, "%-20s %-12s %7s %7s %8s %8s\n", "configuration", "condition", "R@1", "R@5", "MRR@10", "NDCG@10")
	printed := 0
	graded := 0
	for _, run := range runs {
		if len(run.Result) < 3 {
			continue
		}
		if printed == 0 {
			graded = run.Result[0].Queries
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
	fmt.Fprintf(&b, "\n%d graded queries. The two single legs take no SearchParams, so they are the\n", graded)
	b.WriteString("same in every configuration and are printed once. Answer rank 1 is the top result.\n")
	return b.String()
}
