package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/supersede"
)

var defaultSources = []string{"factconsolidation_sh_6k", "factconsolidation_sh_32k"}

type demoResult struct {
	Source        string  `json:"source"`
	Questions     int     `json:"questions"`
	BaselineAcc1  float64 `json:"baseline_acc@1"`
	BaselineAcc5  float64 `json:"baseline_acc@5"`
	SupersedeAcc1 float64 `json:"supersede_acc@1"`
	SupersedeAcc5 float64 `json:"supersede_acc@5"`
	Candidates    int     `json:"candidates"`
	Confirmed     int     `json:"confirmed"`
	Created       int     `json:"created"`
}

type perQuestionLine struct {
	Source        string `json:"source"`
	QAPairID      string `json:"qa_pair_id"`
	BaselineHit1  bool   `json:"baseline_hit@1"`
	BaselineHit5  bool   `json:"baseline_hit@5"`
	SupersedeHit1 bool   `json:"supersede_hit@1"`
	SupersedeHit5 bool   `json:"supersede_hit@5"`
}

const resultRowFormat = "%-28s %5d %10.3f %10.3f %12.3f %12.3f %6d %6d %6d\n"

func printResultRow(r demoResult) {
	fmt.Printf(resultRowFormat, r.Source, r.Questions, r.BaselineAcc1, r.BaselineAcc5,
		r.SupersedeAcc1, r.SupersedeAcc5, r.Candidates, r.Confirmed, r.Created)
}

func main() {
	dataPath := flag.String("data", "", "path to convert.py's JSONL output (required)")
	sourcesSpec := flag.String("sources", strings.Join(defaultSources, ","), "comma-separated demo sources to run")
	threshold := flag.Float64("threshold", 0.80, "min cosine similarity for a supersede candidate pair (matches ghost supersede's default)")
	ollamaURL := flag.String("ollama", "http://localhost:11434", "Ollama URL for embeddings")
	embedCache := flag.String("embed-cache", "", "append-only embedding cache JSONL")
	outPath := flag.String("out", "", "per-question JSONL results log")
	flag.Parse()

	if *dataPath == "" {
		fmt.Fprintln(os.Stderr, "error: --data is required")
		os.Exit(1)
	}
	wantSources := make(map[string]bool)
	for _, s := range strings.Split(*sourcesSpec, ",") {
		if s = strings.TrimSpace(s); s != "" {
			wantSources[s] = true
		}
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	demos, err := loadDemos(*dataPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	cls, err := buildClassifier()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	embedder, err := newCachedEmbedder(*ollamaURL, *embedCache)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer embedder.Close() //nolint:errcheck

	var outFile *os.File
	if *outPath != "" {
		outFile, err = os.Create(*outPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		defer outFile.Close() //nolint:errcheck
	}

	fmt.Printf("MemoryAgentBench Conflict_Resolution (fact_sh) — supersede ablation\n\n")
	fmt.Printf("%-28s %5s %10s %10s %12s %12s %6s %6s %6s\n",
		"source", "n", "base@1", "base@5", "supersede@1", "supersede@5", "cand", "conf", "made")

	ctx := context.Background()
	var results []demoResult
	for _, d := range demos {
		if !wantSources[d.Source] {
			continue
		}
		start := time.Now()
		r, err := runDemo(ctx, d, cls, embedder, *threshold, logger, outFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: demo %s: %v\n", d.Source, err)
			os.Exit(1)
		}
		results = append(results, r)
		printResultRow(r)
		fmt.Fprintf(os.Stderr, "%s done in %s\n", d.Source, time.Since(start).Round(time.Second))
	}

	if len(results) == 0 {
		fmt.Fprintln(os.Stderr, "error: no demo matched --sources")
		os.Exit(1)
	}
}

func seedDemo(ctx context.Context, store *memory.Store, db *sql.DB, project string, d Demo, embedder *cachedEmbedder) ([]string, error) {
	facts := splitFacts(d.Context)
	ids, err := seedFacts(ctx, store, db, project, facts)
	if err != nil {
		return nil, err
	}
	if err := embedder.EnsureBatch(ctx, facts); err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	for i, id := range ids {
		vec, err := embedder.EmbedDocument(ctx, facts[i])
		if err != nil {
			return nil, err
		}
		if err := store.StoreEmbedding(ctx, id, vec, embedModel); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

func runDemo(ctx context.Context, d Demo, cls *supersede.RelationClassifier, embedder *cachedEmbedder,
	threshold float64, logger *slog.Logger, outFile *os.File) (demoResult, error) {
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		return demoResult{}, err
	}
	store := memory.NewStore(db, logger)
	defer store.Close() //nolint:errcheck

	project := "mabench-" + d.Source
	if err := store.EnsureProject(ctx, project, "/bench/"+project, project); err != nil {
		return demoResult{}, err
	}

	if _, err := seedDemo(ctx, store, db, project, d, embedder); err != nil {
		return demoResult{}, err
	}

	res, _, err := supersede.Run(ctx, store, cls, project, float32(threshold), true, logger)
	if err != nil {
		return demoResult{}, fmt.Errorf("supersede.Run: %w", err)
	}

	outcomes, err := evaluateQuestions(ctx, store, project, d, embedder, memory.DefaultSearchParams())
	if err != nil {
		return demoResult{}, err
	}

	if outFile != nil {
		for _, o := range outcomes {
			line, err := json.Marshal(perQuestionLine{
				Source: d.Source, QAPairID: o.QAPairID,
				BaselineHit1: o.BaselineHit1, BaselineHit5: o.BaselineHit5,
				SupersedeHit1: o.SupersedeHit1, SupersedeHit5: o.SupersedeHit5,
			})
			if err != nil {
				return demoResult{}, err
			}
			if _, err := fmt.Fprintf(outFile, "%s\n", line); err != nil {
				return demoResult{}, err
			}
		}
	}

	return aggregateOutcomes(d.Source, outcomes, res), nil
}

func aggregateOutcomes(source string, outcomes []questionOutcome, res supersede.Result) demoResult {
	r := demoResult{Source: source, Questions: len(outcomes),
		Candidates: res.Candidates, Confirmed: res.Confirmed, Created: res.Created}
	if len(outcomes) == 0 {
		return r
	}
	var b1, b5, s1, s5 float64
	for _, o := range outcomes {
		if o.BaselineHit1 {
			b1++
		}
		if o.BaselineHit5 {
			b5++
		}
		if o.SupersedeHit1 {
			s1++
		}
		if o.SupersedeHit5 {
			s5++
		}
	}
	n := float64(len(outcomes))
	r.BaselineAcc1, r.BaselineAcc5 = b1/n, b5/n
	r.SupersedeAcc1, r.SupersedeAcc5 = s1/n, s5/n
	return r
}
