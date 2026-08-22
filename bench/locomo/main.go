// Command locomo runs Ghost's retrieval stack against the LoCoMo benchmark
// (snap-research/LoCoMo, locomo10.json) and reports turn- and session-level
// retrieval metrics against the dataset's official evidence labels — no LLM
// judge, fully deterministic given a fixed embedding cache. This is the
// comparability layer (#340): LongMemEval-S remains Ghost's primary benchmark;
// this exists because competitors publish on LoCoMo.
//
// Per conversation (sample_id), every dialogue turn is ingested as a memory
// into a fresh in-memory store ("[Session N | date] Speaker: text"), Ghost's
// production search ranks memories for each question, and the ranking is
// scored against the question's evidence dia_ids at turn level plus a
// collapsed session-level hit@5. Category 5 (adversarial) carries no evidence
// labels and is excluded from scoring, counted separately.
//
// Usage:
//
//	go run ./bench/locomo --data .cache/locomo/locomo10.json \
//	    --condition fts|vector|hybrid [--ollama http://localhost:11434] \
//	    [--embed-cache ~/.cache/ghost-bench/locomo-cache.jsonl] \
//	    [--conversations N] [--retrieval-out ranked.jsonl] [--lmeval-out ds.json] \
//	    [--floors "r5=0.60,mrr10=0.50"]
//
// --retrieval-out emits the phase4-compatible shape ({question_id,
// ranked_items:[{corpus_id, text}]}) with corpus_id = "<sample>-sNN" session
// ids. --lmeval-out writes a LongMemEval-schema dataset so phase4's
// merge_retrieval.py and phase4_run.py run VERBATIM against it for the judged
// end-to-end number.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)

const memoryRetrievalLimit = 500

type locoTurn struct {
	Speaker string `json:"speaker"`
	DiaID   string `json:"dia_id"`
	Text    string `json:"text"`
}

// flexString tolerates LoCoMo answers that are numbers (temporal counts)
// rather than strings.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if string(b) == "null" {
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	*f = flexString(b)
	return nil
}

type locoQA struct {
	Question string      `json:"question"`
	Answer   *flexString `json:"answer"`
	Evidence []string    `json:"evidence"`
	Category int         `json:"category"`
}

type locoConversation struct {
	SampleID     string                     `json:"sample_id"`
	QA           []locoQA                   `json:"qa"`
	Conversation map[string]json.RawMessage `json:"conversation"`
}

// locoSession is one content-bearing session of a conversation.
type locoSession struct {
	ID    string // "<sample>-sNN", stable across outputs
	Num   int
	Date  string
	Turns []locoTurn
}

var categoryNames = map[int]string{
	1: "single-hop",
	2: "temporal",
	3: "multi-hop",
	4: "open-domain",
}

// loadConversation extracts content-bearing sessions (sorted by number) and
// their dates from a LoCoMo conversation record. Sessions present only as a
// date_time key with no turn list are skipped.
func loadConversation(c locoConversation) []locoSession {
	dateOf := map[int]string{}
	maxNum := 0
	for k, v := range c.Conversation {
		if n, ok := strings.CutPrefix(k, "session_"); ok && !strings.HasSuffix(k, "_date_time") {
			if num, err := strconv.Atoi(n); err == nil && num > maxNum {
				maxNum = num
			}
			continue
		}
		if n, ok := strings.CutSuffix(k, "_date_time"); ok {
			n = strings.TrimPrefix(n, "session_")
			if num, err := strconv.Atoi(n); err == nil {
				var date string
				if json.Unmarshal(v, &date) == nil {
					dateOf[num] = date
				}
			}
		}
	}
	var out []locoSession
	for num := 1; num <= maxNum; num++ {
		rawJSON, ok := c.Conversation[fmt.Sprintf("session_%d", num)]
		if !ok {
			continue
		}
		var turns []locoTurn
		if json.Unmarshal(rawJSON, &turns) != nil || len(turns) == 0 {
			continue
		}
		out = append(out, locoSession{
			ID:    fmt.Sprintf("%s-s%02d", c.SampleID, num),
			Num:   num,
			Date:  dateOf[num],
			Turns: turns,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Num < out[j].Num })
	return out
}

func main() {
	dataPath := flag.String("data", ".cache/locomo/locomo10.json", "path to locomo10.json")
	condition := flag.String("condition", "fts", "fts | vector | hybrid")
	ollamaURL := flag.String("ollama", "http://localhost:11434", "Ollama base URL")
	embedCache := flag.String("embed-cache", "", "append-only embedding cache JSONL")
	convsLimit := flag.Int("conversations", 0, "limit to first N conversations (0=all)")
	retrievalOut := flag.String("retrieval-out", "", "write phase4-format ranked.jsonl here")
	lmevalOut := flag.String("lmeval-out", "", "write LongMemEval-schema dataset JSON here")
	floorsSpec := flag.String("floors", "", "comma-separated metric=min gates on OVERALL aggregates")
	flag.Parse()

	switch *condition {
	case "fts", "vector", "hybrid":
	default:
		fmt.Fprintf(os.Stderr, "error: unknown --condition %q\n", *condition)
		os.Exit(1)
	}
	floors := parseFloorsOrExit(*floorsSpec)

	raw, err := os.ReadFile(*dataPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: read data: %v\n", err)
		os.Exit(1)
	}
	var convs []locoConversation
	if err := json.Unmarshal(raw, &convs); err != nil {
		fmt.Fprintf(os.Stderr, "error: parse dataset: %v\n", err)
		os.Exit(1)
	}
	if *convsLimit > 0 && *convsLimit < len(convs) {
		convs = convs[:*convsLimit]
	}
	// Search diagnostics would swamp the report.
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	var embedder *cachedEmbedder
	if *condition != "fts" {
		embedder, err = newCachedEmbedder(*ollamaURL, *embedCache)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		defer embedder.Close() //nolint:errcheck
	}

	ctx := context.Background()
	overall := &agg{}
	perCat := map[string]*agg{}
	perConv := map[string]*agg{}
	excludedAdversarial := 0
	noEvidence := 0

	var retrievalLines, lmevalEntries []map[string]any

	for _, conv := range convs {
		sessions := loadConversation(conv)
		if len(sessions) == 0 {
			continue
		}
		sessionText := map[string]string{}
		diaToSession := map[string]string{}
		for _, s := range sessions {
			var sb strings.Builder
			for _, t := range s.Turns {
				sb.WriteString(t.Speaker)
				sb.WriteString(": ")
				sb.WriteString(t.Text)
				sb.WriteString("\n")
				diaToSession[t.DiaID] = s.ID
			}
			sessionText[s.ID] = sb.String()
		}

		store, memMeta, cleanup, err := openStore(ctx, conv.SampleID, sessions, *condition, embedder)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %s: %v\n", conv.SampleID, err)
			os.Exit(1)
		}

		for qi, q := range conv.QA {
			if q.Category == 5 {
				excludedAdversarial++
				continue
			}
			qid := fmt.Sprintf("%s-q%03d", conv.SampleID, qi)
			goldDia := map[string]bool{}
			goldSessions := map[string]bool{}
			for _, e := range q.Evidence {
				goldDia[e] = true
				if sid, ok := diaToSession[e]; ok {
					goldSessions[sid] = true
				}
			}
			if len(goldDia) == 0 || len(goldSessions) == 0 {
				noEvidence++
				continue
			}

			rankedTurns, err := rankTurns(ctx, store, conv.SampleID, q.Question, *condition, embedder, memMeta)
			if err != nil {
				cleanup()
				fmt.Fprintf(os.Stderr, "error: %s: %v\n", qid, err)
				os.Exit(1)
			}

			r1, r5, r10, mrr, ndcg := scoreTurns(rankedTurns, goldDia)
			sR5 := scoreSessions(rankedTurns, goldSessions, 5)
			name := categoryNames[q.Category]
			overall.add(r1, r5, r10, mrr, ndcg, sR5)
			aggFor(perCat, name).add(r1, r5, r10, mrr, ndcg, sR5)
			aggFor(perConv, conv.SampleID).add(r1, r5, r10, mrr, ndcg, sR5)

			retrievalLines = append(retrievalLines, map[string]any{
				"question_id":  qid,
				"ranked_items": sessionRankedItems(rankedTurns, sessionText),
			})
			lmevalEntries = append(lmevalEntries, lmevalEntry(qid, name, q, sessions, goldSessions))
		}
		cleanup()
	}

	printReport(*condition, len(convs), overall, perCat, perConv, excludedAdversarial, noEvidence, embedder)
	flushJSONL(*retrievalOut, retrievalLines)
	flushJSON(*lmevalOut, lmevalEntries)

	if violations := checkFloors(overallMetrics(overall), floors); len(violations) > 0 {
		for _, v := range violations {
			fmt.Fprintln(os.Stderr, v)
		}
		os.Exit(1)
	}
}

// --- store + retrieval -------------------------------------------------

// openStore creates a fresh in-memory store and ingests every turn as a
// memory, returning the memID→turn metadata the ranker maps back through.
func openStore(ctx context.Context, project string, sessions []locoSession, condition string, embedder *cachedEmbedder) (*memory.Store, map[string]rankedTurn, func(), error) {
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		return nil, nil, nil, err
	}
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	cleanup := func() { _ = store.Close() }
	if err := store.EnsureProject(ctx, project, "/bench/locomo/"+project, project); err != nil {
		cleanup()
		return nil, nil, nil, fmt.Errorf("ensure project: %w", err)
	}
	memMeta := make(map[string]rankedTurn)
	if condition != "fts" {
		var texts []string
		for _, s := range sessions {
			for _, t := range s.Turns {
				texts = append(texts, memoryText(s, t))
			}
		}
		if err := embedder.EnsureBatch(ctx, texts); err != nil {
			cleanup()
			return nil, nil, nil, fmt.Errorf("embed batch: %w", err)
		}
	}
	for _, s := range sessions {
		for _, t := range s.Turns {
			content := memoryText(s, t)
			id, err := store.Create(ctx, project, memory.Memory{
				Category: "fact", Content: content, Importance: 0.7, Source: "mcp",
			})
			if err != nil {
				cleanup()
				return nil, nil, nil, fmt.Errorf("ingest: %w", err)
			}
			if condition != "fts" {
				vec, err := embedder.Embed(ctx, content)
				if err != nil {
					cleanup()
					return nil, nil, nil, fmt.Errorf("embed turn: %w", err)
				}
				if err := store.StoreEmbedding(ctx, id, vec, embedModel); err != nil {
					cleanup()
					return nil, nil, nil, err
				}
			}
			memMeta[id] = rankedTurn{DiaID: t.DiaID, SessionID: s.ID, Text: content}
		}
	}
	return store, memMeta, cleanup, nil
}

func memoryText(s locoSession, t locoTurn) string {
	if s.Date != "" {
		return fmt.Sprintf("[Session %d | %s] %s: %s", s.Num, s.Date, t.Speaker, t.Text)
	}
	return fmt.Sprintf("[Session %d] %s: %s", s.Num, t.Speaker, t.Text)
}

type rankedTurn struct {
	DiaID     string
	SessionID string
	Text      string
}

// rankTurns runs the requested search condition over the pre-ingested store
// and returns turns in rank order.
func rankTurns(ctx context.Context, store *memory.Store, project, question, condition string, embedder *cachedEmbedder, memMeta map[string]rankedTurn) ([]rankedTurn, error) {
	var rankedIDs []string
	switch condition {
	case "fts":
		ms, err := store.SearchFTS(ctx, project, question, memoryRetrievalLimit)
		if err != nil {
			return nil, err
		}
		for _, m := range ms {
			rankedIDs = append(rankedIDs, m.ID)
		}
	case "vector":
		qv, err := embedder.Embed(ctx, question)
		if err != nil {
			return nil, fmt.Errorf("embed question: %w", err)
		}
		sms, err := store.SearchVector(ctx, project, qv, memoryRetrievalLimit)
		if err != nil {
			return nil, err
		}
		for _, sm := range sms {
			rankedIDs = append(rankedIDs, sm.MemoryID)
		}
	case "hybrid":
		qv, err := embedder.Embed(ctx, question)
		if err != nil {
			return nil, fmt.Errorf("embed question: %w", err)
		}
		ms, err := store.SearchHybrid(ctx, project, question, qv, memoryRetrievalLimit)
		if err != nil {
			return nil, err
		}
		for _, m := range ms {
			rankedIDs = append(rankedIDs, m.ID)
		}
	}
	out := make([]rankedTurn, 0, len(rankedIDs))
	for _, id := range rankedIDs {
		if rt, ok := memMeta[id]; ok {
			out = append(out, rt)
		}
	}
	return out, nil
}

// --- scoring --------------------------------------------------------------

func scoreTurns(ranked []rankedTurn, gold map[string]bool) (r1, r5, r10, mrr, ndcg float64) {
	hitAt := func(k int) float64 {
		n := 0
		for _, rt := range ranked {
			if n >= k {
				break
			}
			if gold[rt.DiaID] {
				return 1
			}
			n++
		}
		return 0
	}
	r1, r5, r10 = hitAt(1), hitAt(5), hitAt(10)
	for i, rt := range ranked {
		if i >= 10 {
			break
		}
		if gold[rt.DiaID] {
			mrr = 1 / float64(i+1)
			break
		}
	}
	dcg := 0.0
	for i, rt := range ranked {
		if i >= 10 {
			break
		}
		if gold[rt.DiaID] {
			dcg += 1 / math.Log2(float64(i+2))
		}
	}
	idcg := 0.0
	for i := 0; i < len(gold) && i < 10; i++ {
		idcg += 1 / math.Log2(float64(i+2))
	}
	if idcg > 0 {
		ndcg = dcg / idcg
	}
	return
}

// scoreSessions reports whether any gold session appears within the top-k
// distinct sessions of the turn ranking (a session's rank is its best turn).
func scoreSessions(ranked []rankedTurn, goldSessions map[string]bool, k int) float64 {
	seen := map[string]bool{}
	n := 0
	for _, rt := range ranked {
		if seen[rt.SessionID] {
			continue
		}
		seen[rt.SessionID] = true
		n++
		if n > k {
			break
		}
		if goldSessions[rt.SessionID] {
			return 1
		}
	}
	return 0
}

// --- aggregation / report --------------------------------------------------

// agg accumulates per-question metric sums; overallMetrics turns them into
// means. sr5 is the session-level hit@5.
type agg struct {
	n                           int
	r1, r5, r10, mrr, ndcg, sr5 float64
}

func newAgg() *agg { return &agg{} }

// aggFor returns the named accumulator, creating it on first use (perCat /
// perConv maps hold nil until a question lands in that bucket).
func aggFor(m map[string]*agg, key string) *agg {
	if m[key] == nil {
		m[key] = newAgg()
	}
	return m[key]
}

func (a *agg) add(r1, r5, r10, mrr, ndcg, sr5 float64) {
	a.n++
	a.r1 += r1
	a.r5 += r5
	a.r10 += r10
	a.mrr += mrr
	a.ndcg += ndcg
	a.sr5 += sr5
}

var floorMetricKeys = map[string]bool{
	"r1": true, "r5": true, "r10": true, "mrr10": true, "ndcg10": true, "sr5": true,
}

func overallMetrics(a *agg) map[string]float64 {
	if a.n == 0 {
		return map[string]float64{"r1": 0, "r5": 0, "r10": 0, "mrr10": 0, "ndcg10": 0, "sr5": 0}
	}
	n := float64(a.n)
	return map[string]float64{
		"r1": a.r1 / n, "r5": a.r5 / n, "r10": a.r10 / n,
		"mrr10": a.mrr / n, "ndcg10": a.ndcg / n, "sr5": a.sr5 / n,
	}
}

func parseFloorsOrExit(spec string) map[string]float64 {
	floors := map[string]float64{}
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return floors
	}
	for _, pair := range strings.Split(spec, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			fmt.Fprintf(os.Stderr, "error: malformed floor %q: expected metric=min\n", pair)
			os.Exit(1)
		}
		key := strings.ToLower(strings.TrimSpace(parts[0]))
		if !floorMetricKeys[key] {
			fmt.Fprintf(os.Stderr, "error: unknown floor metric %q (want r1|r5|r10|mrr10|ndcg10|sr5)\n", key)
			os.Exit(1)
		}
		val, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: bad floor value for %q: %v\n", key, err)
			os.Exit(1)
		}
		floors[key] = val
	}
	return floors
}

func checkFloors(overall, floors map[string]float64) []string {
	keys := make([]string, 0, len(floors))
	for k := range floors {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var violations []string
	for _, k := range keys {
		if overall[k] < floors[k] {
			violations = append(violations, fmt.Sprintf("FLOOR VIOLATION: %s = %.3f < %.3f", k, overall[k], floors[k]))
		}
	}
	return violations
}

func printReport(condition string, convs int, overall *agg, perCat, perConv map[string]*agg, excludedAdv, noEv int, embedder *cachedEmbedder) {
	fmt.Printf("LoCoMo (locomo10) — turn-level retrieval + session hit@5, condition=%s\n", condition)
	fmt.Printf("%d conversations, %d scored questions (%d adversarial excluded, %d without resolvable evidence)\n\n",
		convs, overall.n, excludedAdv, noEv)
	printAgg("OVERALL", overall)
	cats := make([]string, 0, len(perCat))
	for c := range perCat {
		cats = append(cats, c)
	}
	sort.Strings(cats)
	for _, c := range cats {
		printAgg("  "+c, perCat[c])
	}
	fmt.Println()
	for id := range perConv {
		printAgg("  "+id, perConv[id])
	}
	if embedder != nil {
		hits, misses := embedder.Stats()
		fmt.Printf("\nembed cache: %d hits, %d misses\n", hits, misses)
	}
}

func printAgg(name string, a *agg) {
	m := overallMetrics(a)
	fmt.Printf("%-14s n=%-4d R@1=%.3f R@5=%.3f R@10=%.3f MRR@10=%.3f NDCG@10=%.3f SessHit@5=%.3f\n",
		name, a.n, m["r1"], m["r5"], m["r10"], m["mrr10"], m["ndcg10"], m["sr5"])
}

// --- output shaping --------------------------------------------------------

func sessionRankedItems(ranked []rankedTurn, sessionText map[string]string) []map[string]any {
	seen := map[string]bool{}
	items := []map[string]any{}
	for _, rt := range ranked {
		if seen[rt.SessionID] {
			continue
		}
		seen[rt.SessionID] = true
		items = append(items, map[string]any{"corpus_id": rt.SessionID, "text": sessionText[rt.SessionID]})
	}
	return items
}

func lmevalEntry(qid, qType string, q locoQA, sessions []locoSession, goldSessions map[string]bool) map[string]any {
	answer := ""
	if q.Answer != nil {
		answer = string(*q.Answer)
	}
	type lmeTurn struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	sessionsOut := make([][]lmeTurn, 0, len(sessions))
	ids := make([]string, 0, len(sessions))
	dates := make([]string, 0, len(sessions))
	for _, s := range sessions {
		ids = append(ids, s.ID)
		dates = append(dates, s.Date)
		turns := make([]lmeTurn, 0, len(s.Turns))
		for _, t := range s.Turns {
			turns = append(turns, lmeTurn{Role: t.Speaker, Content: t.Text})
		}
		sessionsOut = append(sessionsOut, turns)
	}
	// question_date = last content-bearing session's date: LoCoMo questions
	// are posed at the end of the conversation, and upstream prepare_prompt
	// uses it as the prompt's "Current Date".
	questionDate := ""
	if len(sessions) > 0 {
		questionDate = sessions[len(sessions)-1].Date
	}
	return map[string]any{
		"question_id":          qid,
		"question_type":        qType,
		"question_date":        questionDate,
		"question":             q.Question,
		"answer":               answer,
		"answer_session_ids":   sortedKeys(goldSessions),
		"haystack_session_ids": ids,
		"haystack_sessions":    sessionsOut,
		"haystack_dates":       dates,
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func flushJSONL(path string, entries []map[string]any) {
	if path == "" {
		return
	}
	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: write %s: %v\n", path, err)
		os.Exit(1)
	}
	defer f.Close() //nolint:errcheck
	enc := json.NewEncoder(f)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			fmt.Fprintf(os.Stderr, "error: encode %s: %v\n", path, err)
			os.Exit(1)
		}
	}
	fmt.Printf("wrote %s (%d lines)\n", path, len(entries))
}

func flushJSON(path string, entries []map[string]any) {
	if path == "" {
		return
	}
	b, err := json.MarshalIndent(entries, "", " ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: encode %s: %v\n", path, err)
		os.Exit(1)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error: write %s: %v\n", path, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d questions)\n", path, len(entries))
}
