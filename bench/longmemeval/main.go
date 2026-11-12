// Command longmemeval runs Ghost retrieval against LongMemEval-S and reports metrics.
package main

import (
	"context"
	"encoding/json"
	"errors"
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

type rankedItem struct {
	CorpusID string `json:"corpus_id"`
	Text     string `json:"text"`
}

type retrievalLine struct {
	QuestionID     string       `json:"question_id"`
	RankedSessions []rankedItem `json:"ranked_items"`
}

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

var floorMetricKeys = map[string]bool{
	"r1": true, "r5": true, "r10": true, "mrr10": true, "ndcg10": true,
}

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

const (
	exitComplete = 0
	exitFailure  = 1
	exitPartial  = 3
)

func main() { os.Exit(run()) }

func run() int {
	dataPath := flag.String("data", "", "path to longmemeval_s_cleaned.json (required)")
	condition := flag.String("condition", "fts", "fts | vector | hybrid")
	maxQuestions := flag.Int("questions", 0, "limit scored questions (0 = all)")
	outPath := flag.String("out", "", "per-question JSONL results log")
	retrievalOutPath := flag.String("retrieval-out", "", "JSONL of full ranked sessions per question in official retrieval_results format (Phase 4 generation input)")
	ollamaURL := flag.String("ollama", "http://localhost:11434", "Ollama URL for vector/hybrid")
	embedCache := flag.String("embed-cache", "", "append-only embedding cache JSONL (vector/hybrid)")
	embedDeadline := flag.Duration("embed-deadline", 0, "stop the embedding pass after this much wall clock in remote embed calls and exit "+
		"partial (cache saved, no metrics, no floor check) so a re-dispatch resumes from the cache; 0 = unbounded")
	floorsSpec := flag.String("floors", "", "comma-separated metric=min floors over OVERALL metrics, e.g. \"r5=0.74,ndcg10=0.72\" (keys: r1, r5, r10, mrr10, ndcg10)")
	includeAbstention := flag.Bool("include-abstention", false, "include abstention questions in Phase 1 scoring (not recommended for IR metrics)")
	flag.Parse()

	floors, err := parseFloors(*floorsSpec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return exitFailure
	}

	if *dataPath == "" {
		fmt.Fprintln(os.Stderr, "error: --data is required")
		return exitFailure
	}
	if *condition != "fts" && *condition != "vector" && *condition != "hybrid" {
		fmt.Fprintf(os.Stderr, "error: unknown --condition %q\n", *condition)
		return exitFailure
	}
	if *embedDeadline < 0 {
		fmt.Fprintln(os.Stderr, "error: --embed-deadline cannot be negative")
		return exitFailure
	}

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
			return exitFailure
		}
		defer embedder.Close() //nolint:errcheck
		embedder.SetDeadline(*embedDeadline)
	}

	selected, skippedAbstention := selectedQuestions(questions, *maxQuestions, *includeAbstention)

	var outFile *os.File
	if *outPath != "" {
		outFile, err = os.Create(*outPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return exitFailure
		}
		defer closeResultFiles(outFile, nil)
	}

	var retrievalOutFile *os.File
	if *retrievalOutPath != "" {
		retrievalOutFile, err = os.Create(*retrievalOutPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return exitFailure
		}
		defer closeResultFiles(retrievalOutFile, outFile)
	}

	ctx := context.Background()
	overall := &agg{}
	byType := map[string]*agg{}
	scored := 0
	partial := false

	for _, q := range selected {
		rankedSessions, err := rankSessionsForQuestion(ctx, q, *condition, embedder)
		if err != nil {
			if errors.Is(err, errEmbedDeadline) {
				// Stop before the job cap, with every vector computed so far
				// already in the cache file. No metrics, no floor check: a
				// partial pass is cache progress, not a result.
				partial = true
				break
			}
			fmt.Fprintf(os.Stderr, "error: question %s: %v\n", q.QuestionID, err)
			return exitFailure
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

	discarded, survived, discardErr := closeAndDiscardPartialOutputs(partial, []*os.File{outFile, retrievalOutFile}, *outPath, *retrievalOutPath)

	return reportPass(os.Stdout, passReport{
		condition:         *condition,
		overall:           overall,
		byType:            byType,
		scored:            scored,
		skippedAbstention: skippedAbstention,
		includeAbstention: *includeAbstention,
		embedder:          embedder,
		selected:          selected,
		floors:            floors,
		partial:           partial,
		budget:            *embedDeadline,
		elapsed:           time.Since(start),
		discarded:         discarded,
		survived:          survived,
		discardErr:        discardErr,
	})
}

func closeAndDiscardPartialOutputs(partial bool, handles []*os.File, paths ...string) (removed, survived []string, err error) {
	closeResultFiles(handles...)
	return discardPartialOutputs(partial, paths...)
}

func closeResultFiles(files ...*os.File) {
	for _, f := range files {
		if f == nil {
			continue
		}
		_ = f.Close()
	}
}

func discardPartialOutputs(partial bool, paths ...string) (removed, survived []string, err error) {
	if !partial {
		return nil, nil, nil
	}
	var errs []error
	for _, p := range paths {
		if p == "" {
			continue
		}
		if err := os.Remove(p); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			survived = append(survived, p)
			errs = append(errs, fmt.Errorf("remove partial result %s: %w", p, err))
			continue
		}
		removed = append(removed, p)
	}
	return removed, survived, errors.Join(errs...)
}

type passReport struct {
	condition         string
	overall           *agg
	byType            map[string]*agg
	scored            int
	skippedAbstention int
	includeAbstention bool
	embedder          *cachedEmbedder
	selected          []question
	floors            map[string]float64
	partial           bool
	budget            time.Duration
	elapsed           time.Duration
	discarded         []string
	survived          []string
	discardErr        error
}

func reportPass(out io.Writer, r passReport) int {
	if r.partial {
		// embedder is non-nil on this path: the deadline is only consulted
		// while embedding, which only the vector/hybrid conditions do.
		got, want := r.embedder.warmProgress(r.selected)
		_, _ = fmt.Fprintf(out, "%s\nEmbedding cache: ", partialMessage(got, want, r.budget, r.elapsed))
		hits, misses := r.embedder.Stats()
		_, _ = fmt.Fprintf(out, "%d hits, %d computed.\n", hits, misses)
		if len(r.discarded) > 0 {
			// Named, not silent: the files this pass was going to produce are
			// gone, and a bare "no such file" later would not say why.
			_, _ = fmt.Fprintf(out, "Discarded partial result file(s): %s.\n", strings.Join(r.discarded, ", "))
		}
		if r.discardErr != nil {
			// Every half-written file that outlived the pass, named: one of
			// them is the Phase 4 generation input, and a reader told about
			// only the first failure would not know it is still there. Reported
			// on the report itself, and still exit partial — this pass warmed
			// the cache and produced no result, and its caller still has to
			// re-dispatch.
			_, _ = fmt.Fprintf(out, "WARNING: could not remove partial result file(s), so half-written one(s) survive: %s\n",
				strings.Join(r.survived, ", "))
			for _, e := range strings.Split(r.discardErr.Error(), "\n") {
				_, _ = fmt.Fprintf(out, "  %s\n", e)
			}
		}
		return exitCode(true, nil)
	}

	_, _ = fmt.Fprintf(out, "LongMemEval-S (cleaned) — session-level retrieval, condition=%s\n\n", r.condition)
	_, _ = fmt.Fprintf(out, "%-28s %5s %7s %7s %7s %8s %8s\n", "question type", "n", "R@1", "R@5", "R@10", "MRR@10", "NDCG@10")
	typeNames := make([]string, 0, len(r.byType))
	for name := range r.byType {
		typeNames = append(typeNames, name)
	}
	sort.Strings(typeNames)
	for _, name := range typeNames {
		printAggTo(out, name, r.byType[name])
	}
	printAggTo(out, "OVERALL", r.overall)
	_, _ = fmt.Fprintf(out, "\n%d questions scored (%d abstention %s). Wall clock %s.\n",
		r.scored, r.skippedAbstention,
		map[bool]string{false: "excluded", true: "included"}[r.includeAbstention],
		r.elapsed.Round(time.Second))
	if r.embedder != nil {
		hits, misses := r.embedder.Stats()
		_, _ = fmt.Fprintf(out, "Embedding cache: %d hits, %d computed.\n", hits, misses)
	}

	violations := checkFloors(overallMetrics(r.overall), r.floors)
	for _, v := range violations {
		_, _ = fmt.Fprintln(os.Stderr, v)
	}
	return exitCode(r.partial, violations)
}

func selectedQuestions(questions []question, maxQuestions int, includeAbstention bool) (selected []question, skippedAbstention int) {
	for _, q := range questions {
		if isAbstention(q.QuestionID) && !includeAbstention {
			skippedAbstention++
			continue
		}
		if maxQuestions > 0 && len(selected) >= maxQuestions {
			break
		}
		selected = append(selected, q)
	}
	return selected, skippedAbstention
}

func exitCode(partial bool, violations []string) int {
	switch {
	case partial:
		return exitPartial
	case len(violations) > 0:
		return exitFailure
	default:
		return exitComplete
	}
}

func partialMessage(got, want int, budget, elapsed time.Duration) string {
	return fmt.Sprintf("Embedding pass stopped at its %s budget after %s: cache warmed %d/%d vectors. "+
		"No metrics, no floor check — re-dispatch to continue from this cache.",
		budget, elapsed.Round(time.Second), got, want)
}

func printAggTo(out io.Writer, name string, a *agg) {
	if a.n == 0 {
		return
	}
	n := float64(a.n)
	_, _ = fmt.Fprintf(out, "%-28s %5d %7.3f %7.3f %7.3f %8.3f %8.3f\n",
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

const memoryRetrievalLimit = 150

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

	if condition != "fts" {
		var texts []string
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
			id, err := store.CreateFromCorpus(ctx, project, memory.Memory{
				Category: "fact", Content: t.Content, Importance: 0.7, Source: "mcp",
			})
			if err != nil {
				return nil, fmt.Errorf("ingest: %w", err)
			}
			if condition != "fts" {
				vec, err := embedder.EmbedDocument(ctx, t.Content)
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
		qv, err := embedder.EmbedQuery(ctx, q.Question)
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
		qv, err := embedder.EmbedQuery(ctx, q.Question)
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
