// Command longmemeval runs Ghost's retrieval stack against the LongMemEval-S
// benchmark (cleaned variant) and reports session-level retrieval metrics
// against the dataset's official evidence labels — no LLM judge, fully
// deterministic given a fixed embedding cache.
//
// Per question, every haystack turn is ingested as a memory into a fresh
// in-memory store, Ghost's production search ranks memories for the question,
// ranked memories map back to their sessions (first occurrence wins), and the
// unique-session ranking is scored against answer_session_ids with the same
// judge-free IR metrics `ghost bench` uses. The 30 abstention questions
// (question_id suffix "_abs") carry no evidence labels and are excluded, per
// the benchmark's own retrieval-evaluation protocol.
//
// Usage:
//
//	go run ./bench/longmemeval --data ~/.cache/ghost-bench/longmemeval_s_cleaned.json \
//	    --condition fts|vector|hybrid [--questions N] [--out per-question.jsonl] \
//	    [--retrieval-out ranked.jsonl] \
//	    [--ollama http://localhost:11434] [--embed-cache ~/.cache/ghost-bench/nomic-cache.jsonl] \
//	    [--floors "r5=0.74,ndcg10=0.72"]
//
// --retrieval-out emits, per scored question, the full untruncated ranked
// session list in the official LongMemEval retrieval_results format
// ({question_id, ranked_items:[{corpus_id, text}]}), the input the Phase 4
// (end-to-end, leaderboard-comparable) generation stage merges into the
// dataset. See docs/benchmarks.md.
//
// The fts condition needs no embeddings and runs in minutes. vector/hybrid
// embed every turn and question through local Ollama (nomic-embed-text:v1.5),
// memoized in an append-only content-hash cache so shared sessions across
// questions and repeat runs cost nothing.
//
// --floors takes a comma-separated metric=min spec over the OVERALL
// (all-questions) aggregate — accepted keys r1, r5, r10, mrr10, ndcg10,
// case-insensitive. After the normal table prints, any violated floor logs a
// "FLOOR VIOLATION: metric = got < min" line to stderr and the process exits
// 1, for use as a CI regression gate. An unknown key or malformed spec is
// validated before the benchmark runs and exits 1 immediately.
//
// See docs/benchmarks.md ("Phase 1") for methodology and reporting rules.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/bench"
	"github.com/wcatz/ghost/internal/memory"
)

type turn struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	HasAnswer bool   `json:"has_answer"`
}

type question struct {
	QuestionID       string   `json:"question_id"`
	QuestionType     string   `json:"question_type"`
	Question         string   `json:"question"`
	AnswerSessionIDs []string `json:"answer_session_ids"`
	SessionIDs       []string `json:"haystack_session_ids"`
	Sessions         [][]turn `json:"haystack_sessions"`
}

// rankedItem is one entry of the official LongMemEval retrieval_results format
// (src/generation/run_generation.py reads entry['retrieval_results']
// ['ranked_items'] as [{corpus_id, text}]). Ghost's corpus_id is the haystack
// session id verbatim; text is unused by the flat-session reader (merge=none)
// but kept for schema fidelity.
type rankedItem struct {
	CorpusID string `json:"corpus_id"`
	Text     string `json:"text"`
}

// retrievalLine is one line of the --retrieval-out log: a question's full,
// untruncated ranked session list in the official retrieval_results shape,
// ready to merge into the dataset as the --in_file for the Phase 4 (end-to-end,
// leaderboard-comparable) generation stage. Unlike perQuestion.TopSessions
// (capped at 10 for the metrics log), this keeps every ranked session so a
// downstream topk_context up to 50 has enough to slice.
type retrievalLine struct {
	QuestionID     string       `json:"question_id"`
	RankedSessions []rankedItem `json:"ranked_items"`
}

// perQuestion is one line of the --out log, per the honest-reporting rules
// (raw per-question results ship with any published number).
type perQuestion struct {
	QuestionID   string   `json:"question_id"`
	QuestionType string   `json:"question_type"`
	Recall1      float64  `json:"recall@1"`
	Recall5      float64  `json:"recall@5"`
	Recall10     float64  `json:"recall@10"`
	MRR10        float64  `json:"mrr@10"`
	NDCG10       float64  `json:"ndcg@10"`
	TopSessions  []string `json:"top_sessions"`
}

type agg struct {
	n                      int
	r1, r5, r10, mrr, ndcg float64
}

func (a *agg) add(r1, r5, r10, mrr, ndcg float64) {
	a.n++
	a.r1 += r1
	a.r5 += r5
	a.r10 += r10
	a.mrr += mrr
	a.ndcg += ndcg
}

// floorMetricKeys are the only metric names -floors accepts, matching the
// per-question-type table's columns.
var floorMetricKeys = map[string]bool{
	"r1": true, "r5": true, "r10": true, "mrr10": true, "ndcg10": true,
}

// overallMetrics averages an agg's sums into the canonical, floor-key-named
// metrics map. agg accumulates sums across questions (see add); this is the
// only place those sums become means.
func overallMetrics(a *agg) map[string]float64 {
	if a.n == 0 {
		return map[string]float64{"r1": 0, "r5": 0, "r10": 0, "mrr10": 0, "ndcg10": 0}
	}
	n := float64(a.n)
	return map[string]float64{
		"r1":     a.r1 / n,
		"r5":     a.r5 / n,
		"r10":    a.r10 / n,
		"mrr10":  a.mrr / n,
		"ndcg10": a.ndcg / n,
	}
}

// parseFloors parses a comma-separated "metric=min" spec (e.g.
// "r5=0.74,ndcg10=0.72") into a floor map. Metric keys are case-insensitive
// and must be one of floorMetricKeys. Returns an error on any unknown key or
// malformed pair — called before the benchmark runs, so a bad spec fails
// fast. An empty (or all-whitespace) spec parses to an empty, non-nil map.
func parseFloors(spec string) (map[string]float64, error) {
	floors := make(map[string]float64)
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return floors, nil
	}
	for _, pair := range strings.Split(spec, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("malformed floor %q: expected metric=min", pair)
		}
		key := strings.ToLower(strings.TrimSpace(parts[0]))
		if !floorMetricKeys[key] {
			return nil, fmt.Errorf("unknown floor metric %q: must be one of r1, r5, r10, mrr10, ndcg10", key)
		}
		val, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if err != nil {
			return nil, fmt.Errorf("malformed floor value for %q: %w", key, err)
		}
		floors[key] = val
	}
	return floors, nil
}

// checkFloors compares overall metrics against floors and returns one
// "FLOOR VIOLATION: ..." message per breach (overall < floor), in stable
// key-sorted order. Exact equality is not a violation. Empty floors always
// yields no violations.
func checkFloors(overall map[string]float64, floors map[string]float64) []string {
	keys := make([]string, 0, len(floors))
	for k := range floors {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var violations []string
	for _, k := range keys {
		floor := floors[k]
		got := overall[k]
		if got < floor {
			violations = append(violations, fmt.Sprintf("FLOOR VIOLATION: %s = %.3f < %.3f", k, got, floor))
		}
	}
	return violations
}

func main() {
	dataPath := flag.String("data", "", "path to longmemeval_s_cleaned.json (required)")
	condition := flag.String("condition", "fts", "fts | vector | hybrid")
	maxQuestions := flag.Int("questions", 0, "limit scored questions (0 = all)")
	outPath := flag.String("out", "", "per-question JSONL results log")
	retrievalOutPath := flag.String("retrieval-out", "", "JSONL of full ranked sessions per question in official retrieval_results format (Phase 4 generation input)")
	ollamaURL := flag.String("ollama", "http://localhost:11434", "Ollama URL for vector/hybrid")
	embedCache := flag.String("embed-cache", "", "append-only embedding cache JSONL (vector/hybrid)")
	floorsSpec := flag.String("floors", "", "comma-separated metric=min floors over OVERALL metrics, e.g. \"r5=0.74,ndcg10=0.72\" (keys: r1, r5, r10, mrr10, ndcg10)")
	flag.Parse()

	// Parse -floors before any other validation: an unknown key or malformed
	// spec must fail fast, before the (potentially long) benchmark runs.
	floors, err := parseFloors(*floorsSpec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if *dataPath == "" {
		fmt.Fprintln(os.Stderr, "error: --data is required")
		os.Exit(1)
	}
	if *condition != "fts" && *condition != "vector" && *condition != "hybrid" {
		fmt.Fprintf(os.Stderr, "error: unknown --condition %q\n", *condition)
		os.Exit(1)
	}

	// Search diagnostics (FTS term-cap warnings etc.) would swamp the report.
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	start := time.Now()
	questions, err := loadQuestions(*dataPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	var embedder *cachedEmbedder
	if *condition != "fts" {
		embedder, err = newCachedEmbedder(*ollamaURL, *embedCache)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		defer embedder.Close() //nolint:errcheck
	}

	var outFile *os.File
	if *outPath != "" {
		outFile, err = os.Create(*outPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		defer outFile.Close() //nolint:errcheck
	}

	var retrievalOutFile *os.File
	if *retrievalOutPath != "" {
		retrievalOutFile, err = os.Create(*retrievalOutPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		defer retrievalOutFile.Close() //nolint:errcheck
	}

	ctx := context.Background()
	overall := &agg{}
	byType := map[string]*agg{}
	scored, skippedAbstention := 0, 0

	for _, q := range questions {
		if isAbstention(q.QuestionID) {
			skippedAbstention++
			continue
		}
		if *maxQuestions > 0 && scored >= *maxQuestions {
			break
		}

		rankedSessions, err := rankSessionsForQuestion(ctx, q, *condition, embedder)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: question %s: %v\n", q.QuestionID, err)
			os.Exit(1)
		}

		rel := bench.Relevance{}
		for _, sid := range q.AnswerSessionIDs {
			rel[sid] = 1
		}
		r1 := bench.RecallAtK(rankedSessions, rel, 1)
		r5 := bench.RecallAtK(rankedSessions, rel, 5)
		r10 := bench.RecallAtK(rankedSessions, rel, 10)
		mrr := bench.ReciprocalRankAtK(rankedSessions, rel, 10)
		ndcg := bench.NDCGAtK(rankedSessions, rel, 10)

		overall.add(r1, r5, r10, mrr, ndcg)
		if byType[q.QuestionType] == nil {
			byType[q.QuestionType] = &agg{}
		}
		byType[q.QuestionType].add(r1, r5, r10, mrr, ndcg)
		scored++

		if outFile != nil {
			top := rankedSessions
			if len(top) > 10 {
				top = top[:10]
			}
			line, _ := json.Marshal(perQuestion{
				QuestionID: q.QuestionID, QuestionType: q.QuestionType,
				Recall1: r1, Recall5: r5, Recall10: r10, MRR10: mrr, NDCG10: ndcg,
				TopSessions: top,
			})
			_, _ = fmt.Fprintf(outFile, "%s\n", line)
		}
		if retrievalOutFile != nil {
			items := make([]rankedItem, len(rankedSessions))
			for i, sid := range rankedSessions {
				items[i] = rankedItem{CorpusID: sid, Text: ""}
			}
			line, _ := json.Marshal(retrievalLine{QuestionID: q.QuestionID, RankedSessions: items})
			_, _ = fmt.Fprintf(retrievalOutFile, "%s\n", line)
		}
		if scored%50 == 0 {
			fmt.Fprintf(os.Stderr, "progress: %d questions scored (%.0fs elapsed)\n", scored, time.Since(start).Seconds())
		}
	}

	fmt.Printf("LongMemEval-S (cleaned) — session-level retrieval, condition=%s\n\n", *condition)
	fmt.Printf("%-28s %5s %7s %7s %7s %8s %8s\n", "question type", "n", "R@1", "R@5", "R@10", "MRR@10", "NDCG@10")
	typeNames := make([]string, 0, len(byType))
	for name := range byType {
		typeNames = append(typeNames, name)
	}
	sort.Strings(typeNames)
	for _, name := range typeNames {
		printAgg(name, byType[name])
	}
	printAgg("OVERALL", overall)
	fmt.Printf("\n%d questions scored (%d abstention excluded). Wall clock %s.\n",
		scored, skippedAbstention, time.Since(start).Round(time.Second))
	if embedder != nil {
		hits, misses := embedder.Stats()
		fmt.Printf("Embedding cache: %d hits, %d computed.\n", hits, misses)
	}

	if violations := checkFloors(overallMetrics(overall), floors); len(violations) > 0 {
		for _, v := range violations {
			fmt.Fprintln(os.Stderr, v)
		}
		os.Exit(1)
	}
}

func printAgg(name string, a *agg) {
	if a.n == 0 {
		return
	}
	n := float64(a.n)
	fmt.Printf("%-28s %5d %7.3f %7.3f %7.3f %8.3f %8.3f\n",
		name, a.n, a.r1/n, a.r5/n, a.r10/n, a.mrr/n, a.ndcg/n)
}

func loadQuestions(path string) ([]question, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck
	var qs []question
	if err := json.NewDecoder(f).Decode(&qs); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	for _, q := range qs {
		if len(q.Sessions) != len(q.SessionIDs) {
			return nil, fmt.Errorf("question %s: %d sessions but %d session ids", q.QuestionID, len(q.Sessions), len(q.SessionIDs))
		}
	}
	return qs, nil
}

func isAbstention(questionID string) bool {
	return len(questionID) > 4 && questionID[len(questionID)-4:] == "_abs"
}

// memoryRetrievalLimit is how many memories are retrieved before collapsing to
// unique sessions — deep enough that 10 distinct sessions are always reachable
// (haystack turns per session average ~15, so 10 sessions of consecutive hits
// would need at most ~150).
const memoryRetrievalLimit = 150

// rankSessionsForQuestion ingests one question's haystack into a fresh store,
// runs the requested search condition, and returns unique session IDs in
// retrieval-rank order (a session's rank is its best-ranked turn).
func rankSessionsForQuestion(ctx context.Context, q question, condition string, embedder *cachedEmbedder) ([]string, error) {
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		return nil, err
	}
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer store.Close() //nolint:errcheck

	const project = "lme"
	if err := store.EnsureProject(ctx, project, "/bench/lme", project); err != nil {
		return nil, err
	}

	// Batch-resolve every embedding this question needs before ingestion, so a
	// cold cache costs a handful of batched Ollama calls instead of one round
	// trip per turn.
	if condition != "fts" {
		texts := []string{q.Question}
		for _, session := range q.Sessions {
			for _, t := range session {
				if t.Content != "" {
					texts = append(texts, t.Content)
				}
			}
		}
		if err := embedder.EnsureBatch(ctx, texts); err != nil {
			return nil, fmt.Errorf("embed question %s: %w", q.QuestionID, err)
		}
	}

	memToSession := make(map[string]string)
	for i, session := range q.Sessions {
		sid := q.SessionIDs[i]
		for _, t := range session {
			if t.Content == "" {
				continue
			}
			id, err := store.Create(ctx, project, memory.Memory{
				Category: "fact", Content: t.Content, Importance: 0.7, Source: "mcp",
			})
			if err != nil {
				return nil, fmt.Errorf("ingest: %w", err)
			}
			if condition != "fts" {
				vec, err := embedder.Embed(ctx, t.Content)
				if err != nil {
					return nil, fmt.Errorf("embed turn: %w", err)
				}
				if err := store.StoreEmbedding(ctx, id, vec, embedModel); err != nil {
					return nil, err
				}
			}
			memToSession[id] = sid
		}
	}

	var rankedMemoryIDs []string
	switch condition {
	case "fts":
		ms, err := store.SearchFTS(ctx, project, q.Question, memoryRetrievalLimit)
		if err != nil {
			return nil, err
		}
		for _, m := range ms {
			rankedMemoryIDs = append(rankedMemoryIDs, m.ID)
		}
	case "vector":
		qv, err := embedder.Embed(ctx, q.Question)
		if err != nil {
			return nil, fmt.Errorf("embed question: %w", err)
		}
		sms, err := store.SearchVector(ctx, project, qv, memoryRetrievalLimit)
		if err != nil {
			return nil, err
		}
		for _, sm := range sms {
			rankedMemoryIDs = append(rankedMemoryIDs, sm.MemoryID)
		}
	case "hybrid":
		qv, err := embedder.Embed(ctx, q.Question)
		if err != nil {
			return nil, fmt.Errorf("embed question: %w", err)
		}
		ms, err := store.SearchHybrid(ctx, project, q.Question, qv, memoryRetrievalLimit)
		if err != nil {
			return nil, err
		}
		for _, m := range ms {
			rankedMemoryIDs = append(rankedMemoryIDs, m.ID)
		}
	}

	return sessionsInRankOrder(rankedMemoryIDs, memToSession), nil
}

// sessionsInRankOrder collapses a ranked memory list to unique session IDs,
// preserving first-occurrence order.
func sessionsInRankOrder(rankedMemoryIDs []string, memToSession map[string]string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, id := range rankedMemoryIDs {
		sid, ok := memToSession[id]
		if !ok || seen[sid] {
			continue
		}
		seen[sid] = true
		out = append(out, sid)
	}
	return out
}
