package bench

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	"github.com/wcatz/ghost/internal/memory"
)

const (
	// MaintenanceProject is the project the suite seeds and searches.
	MaintenanceProject = "maintstate"
	globalProject      = "_global"
	probeLive          = "live"
	probeGlobal        = "global"
)

// MaintenanceMemory is one corpus memory plus maintenance state.
type MaintenanceMemory struct {
	Key        string   `json:"key"`
	Project    string   `json:"project,omitempty"`
	Category   string   `json:"category"`
	Content    string   `json:"content"`
	Importance float32  `json:"importance"`
	AgeDays    int      `json:"age_days"`
	Resolved   bool     `json:"resolved,omitempty"`
	Supersedes []string `json:"supersedes,omitempty"`
}

// MaintenanceQuery is one graded question.
type MaintenanceQuery struct {
	Name        string         `json:"name"`
	Text        string         `json:"text"`
	Rel         map[string]int `json:"rel"`
	Distractors []string       `json:"distractors,omitempty"`
	Probe       string         `json:"probe,omitempty"`
}

// LoadMaintenanceMemories reads the corpus, one JSON per line.
func LoadMaintenanceMemories(r io.Reader) ([]MaintenanceMemory, error) {
	var out []MaintenanceMemory
	if err := decodeJSONL(r, func(raw json.RawMessage) error {
		var m MaintenanceMemory
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		switch {
		case m.Key == "":
			return fmt.Errorf("memory with empty key: %q", m.Content)
		case m.Content == "":
			return fmt.Errorf("memory %q has no content", m.Key)
		case m.Category == "":
			return fmt.Errorf("memory %q has no category", m.Key)
		case m.AgeDays < 0:
			return fmt.Errorf("memory %q has negative age_days %d", m.Key, m.AgeDays)
		}
		out = append(out, m)
		return nil
	}); err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(out))
	for _, m := range out {
		if seen[m.Key] {
			return nil, fmt.Errorf("duplicate memory key %q", m.Key)
		}
		seen[m.Key] = true
	}
	return out, nil
}

// LoadMaintenanceQueries reads the graded questions, one JSON per line.
func LoadMaintenanceQueries(r io.Reader) ([]MaintenanceQuery, error) {
	var out []MaintenanceQuery
	if err := decodeJSONL(r, func(raw json.RawMessage) error {
		var q MaintenanceQuery
		if err := json.Unmarshal(raw, &q); err != nil {
			return err
		}
		switch {
		case q.Name == "":
			return fmt.Errorf("query with empty name: %q", q.Text)
		case q.Text == "":
			return fmt.Errorf("query %q has no text", q.Name)
		case len(q.Rel) == 0:
			return fmt.Errorf("query %q has an empty rel map: the maintenance suite grades every question", q.Name)
		}
		answer, _, err := answerKey(q)
		if err != nil {
			return fmt.Errorf("query %q: %w", q.Name, err)
		}
		if q.Probe == "" {
			q.Probe = probeLive
		}
		if q.Probe != probeLive && q.Probe != probeGlobal {
			return fmt.Errorf("query %q has probe %q, want %q or %q", q.Name, q.Probe, probeLive, probeGlobal)
		}
		for _, d := range q.Distractors {
			if d == answer {
				return fmt.Errorf("query %q lists its own answer %q as a distractor", q.Name, d)
			}
			if g := q.Rel[d]; g > 0 {
				return fmt.Errorf("query %q lists distractor %q with gain %d: a relevant row cannot also be forbidden", q.Name, d, g)
			}
		}
		out = append(out, q)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

func answerKey(q MaintenanceQuery) (string, int, error) {
	answer, gain, tied := "", 0, false
	for key, g := range q.Rel {
		switch {
		case g <= 0:
			return "", 0, fmt.Errorf("key %q has non-positive gain %d: omit it instead of grading it as irrelevant", key, g)
		case g > gain:
			answer, gain, tied = key, g, false
		case g == gain:
			tied = true
		}
	}
	if tied {
		return "", 0, fmt.Errorf("two keys share the top gain %d, so the answer is ambiguous", gain)
	}
	return answer, gain, nil
}

// MaintenanceQuestion is a seeded question.
type MaintenanceQuestion struct {
	Name        string
	Text        string
	Probe       string
	Vector      []float32
	Rel         Relevance
	AnswerID    string
	Distractors []string
}

// MaintenanceOutcome is one question's judgment under one condition.
type MaintenanceOutcome struct {
	Query    string
	Probe    string
	Found    bool
	LiveWins bool
}

// SharedProbe aggregates questions whose answer is a shared _global row.
type SharedProbe struct {
	Queries  int
	Found    int
	LiveWins int
}

// MaintenanceResult is one condition's aggregate over the suite.
type MaintenanceResult struct {
	Condition string
	Queries   int
	Recall5   float64
	NDCG10    float64
	LiveWins  float64
	Found     float64
	Shared    SharedProbe
	Outcomes  []MaintenanceOutcome
}

// RunMaintenance evaluates the maintenance-state corpus.
func RunMaintenance(ctx context.Context, mems []MaintenanceMemory, queries []MaintenanceQuery, vecs Vectors, p memory.SearchParams) ([]MaintenanceResult, error) {
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		return nil, err
	}
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer store.Close() //nolint:errcheck

	questions, err := seedMaintenance(ctx, store, db, mems, queries, vecs)
	if err != nil {
		return nil, err
	}

	conditions := []struct {
		name string
		rank rankFn
	}{
		{CondFTS, func(q Query) ([]string, error) {
			return idsFromMemories(store.SearchFTS(ctx, MaintenanceProject, q.Text, scoreK))
		}},
		{CondVector, func(q Query) ([]string, error) {
			return idsFromScored(store.SearchVector(ctx, MaintenanceProject, q.Vector, scoreK))
		}},
		{CondHybrid, func(q Query) ([]string, error) {
			return idsFromMemories(store.SearchHybridParams(ctx, MaintenanceProject, q.Text, q.Vector, scoreK, p))
		}},
	}

	out := make([]MaintenanceResult, 0, len(conditions))
	for _, c := range conditions {
		res, err := runMaintenanceCondition(c.name, questions, c.rank)
		if err != nil {
			return nil, err
		}
		out = append(out, res)
	}
	return out, nil
}

func seedMaintenance(ctx context.Context, store *memory.Store, db *sql.DB, mems []MaintenanceMemory, queries []MaintenanceQuery, vecs Vectors) ([]MaintenanceQuestion, error) {
	if err := store.EnsureProject(ctx, MaintenanceProject, "/bench/"+MaintenanceProject, MaintenanceProject); err != nil {
		return nil, fmt.Errorf("ensure project: %w", err)
	}
	if err := store.EnsureProject(ctx, globalProject, globalProject, "global"); err != nil {
		return nil, fmt.Errorf("ensure global project: %w", err)
	}

	dim := 0
	checkDim := func(what string, v []float32) error {
		if dim == 0 {
			dim = len(v)
		} else if len(v) != dim {
			return fmt.Errorf("%s has %d-dim vector, expected %d (regenerate embeddings)", what, len(v), dim)
		}
		return nil
	}

	keyToID := make(map[string]string, len(mems))
	var resolvedIDs []string
	stamp := newCorpusStamp()
	for _, m := range mems {
		project := m.Project
		if project == "" {
			project = MaintenanceProject
		}
		vec, ok := vecs[m.Key]
		if !ok {
			return nil, fmt.Errorf("no fixture vector for memory key %q (regenerate embeddings)", m.Key)
		}
		if err := checkDim("memory "+m.Key, vec); err != nil {
			return nil, err
		}
		id, err := store.CreateWithIDFromCorpus(ctx, project, corpusID(project, m.Key), memory.Memory{
			Category: m.Category, Content: m.Content, Importance: m.Importance, Source: "mcp",
		})
		if err != nil {
			return nil, fmt.Errorf("create memory %q: %w", m.Key, err)
		}
		if err := stamp.apply(ctx, db, id, m.AgeDays); err != nil {
			return nil, fmt.Errorf("stamp %q: %w", m.Key, err)
		}
		if err := store.StoreEmbedding(ctx, id, vec, "bench"); err != nil {
			return nil, fmt.Errorf("embed memory %q: %w", m.Key, err)
		}
		keyToID[m.Key] = id
		if m.Resolved {
			resolvedIDs = append(resolvedIDs, id)
		}
	}
	if n, err := store.SetResolved(ctx, resolvedIDs); err != nil {
		return nil, fmt.Errorf("set resolved: %w", err)
	} else if n != len(resolvedIDs) {
		return nil, fmt.Errorf("SetResolved stamped %d of %d rows: a resolved row is in an exempt category (convention/preference cannot be resolved)", n, len(resolvedIDs))
	}

	for _, m := range mems {
		for _, older := range m.Supersedes {
			target, ok := keyToID[older]
			if !ok {
				return nil, fmt.Errorf("memory %q supersedes unknown key %q", m.Key, older)
			}
			if err := store.CreateLink(ctx, keyToID[m.Key], target, "supersedes", 1.0, "llm"); err != nil {
				return nil, fmt.Errorf("link supersedes %s -> %s: %w", m.Key, older, err)
			}
		}
	}

	questions := make([]MaintenanceQuestion, 0, len(queries))
	for _, q := range queries {
		answer, _, err := answerKey(q)
		if err != nil {
			return nil, fmt.Errorf("query %q: %w", q.Name, err)
		}
		rel := make(Relevance, len(q.Rel))
		for key, gain := range q.Rel {
			id, ok := keyToID[key]
			if !ok {
				return nil, fmt.Errorf("query %q references unknown memory key %q", q.Name, key)
			}
			rel[id] = gain
		}
		distractors := make([]string, 0, len(q.Distractors))
		for _, key := range q.Distractors {
			id, ok := keyToID[key]
			if !ok {
				return nil, fmt.Errorf("query %q names unknown distractor key %q", q.Name, key)
			}
			distractors = append(distractors, id)
		}
		vec, ok := vecs[q.Name]
		if !ok {
			return nil, fmt.Errorf("no fixture vector for query %q (regenerate embeddings)", q.Name)
		}
		if err := checkDim("query "+q.Name, vec); err != nil {
			return nil, err
		}
		questions = append(questions, MaintenanceQuestion{
			Name: q.Name, Text: q.Text, Probe: q.Probe, Vector: vec,
			Rel: rel, AnswerID: keyToID[answer], Distractors: distractors,
		})
	}
	return questions, nil
}

func runMaintenanceCondition(name string, questions []MaintenanceQuestion, rank rankFn) (MaintenanceResult, error) {
	res := MaintenanceResult{Condition: name, Queries: len(questions)}
	if len(questions) == 0 {
		return res, nil
	}
	var sumR5, sumNDCG, sumWins, sumFound float64
	for _, q := range questions {
		ranked, err := rank(Query{Text: q.Text, Vector: q.Vector})
		if err != nil {
			return MaintenanceResult{}, fmt.Errorf("%s: query %q: %w", name, q.Name, err)
		}
		found, wins, _ := judgeProbe(ranked, q.AnswerID, q.Distractors)
		res.Outcomes = append(res.Outcomes, MaintenanceOutcome{
			Query: q.Name, Probe: q.Probe, Found: found, LiveWins: wins,
		})
		sumR5 += RecallAtK(ranked, q.Rel, 5)
		sumNDCG += NDCGAtK(ranked, q.Rel, 10)
		sumFound += boolScore(found)
		sumWins += boolScore(wins)
		if q.Probe == probeGlobal {
			res.Shared.Queries++
			res.Shared.Found += b2i(found)
			res.Shared.LiveWins += b2i(wins)
		}
	}
	n := float64(res.Queries)
	res.Recall5 = sumR5 / n
	res.NDCG10 = sumNDCG / n
	res.LiveWins = sumWins / n
	res.Found = sumFound / n
	return res, nil
}

func boolScore(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// FormatMaintenance renders the maintenance-state report.
func FormatMaintenance(results []MaintenanceResult) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%-14s %4s %7s %8s %10s %12s\n", "condition", "n", "R@5", "NDCG@10", "live-wins", "answer-found")
	for _, r := range results {
		fmt.Fprintf(&b, "%-14s %4d %7.3f %8.3f %10.3f %12.3f\n",
			r.Condition, r.Queries, r.Recall5, r.NDCG10, r.LiveWins, r.Found)
	}
	for _, r := range results {
		if r.Shared.Queries == 0 {
			continue
		}
		fmt.Fprintf(&b, "%s: shared _global answers found %d/%d, live-wins %d/%d\n",
			r.Condition, r.Shared.Found, r.Shared.Queries, r.Shared.LiveWins, r.Shared.Queries)
	}
	for _, r := range results {
		var outranked, notFound []string
		for _, o := range r.Outcomes {
			switch {
			case !o.Found:
				notFound = append(notFound, o.Query)
			case !o.LiveWins:
				outranked = append(outranked, o.Query)
			}
		}
		if len(outranked) > 0 {
			fmt.Fprintf(&b, "\n%s: %d question(s) where a resolved/_global/superseded copy outranked the answer:\n", r.Condition, len(outranked))
			for _, n := range outranked {
				fmt.Fprintf(&b, "  %s\n", n)
			}
		}
		if len(notFound) > 0 {
			fmt.Fprintf(&b, "\n%s: %d question(s) whose answer was not retrieved at all (evicted or never matched):\n", r.Condition, len(notFound))
			for _, n := range notFound {
				fmt.Fprintf(&b, "  %s\n", n)
			}
		}
	}
	return b.String()
}
