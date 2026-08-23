package main

// Manual latency probe for the production search path at real LoCoMo scale.
// Not wired into CI: it needs the dataset and a local Ollama. Run with:
//
//	LOCOMO_DATA=.cache/locomo/locomo10.json go test ./bench/locomo -run TestZZLatencyProbe -v -timeout 30m
//
// Mirrors main(): same openStore ingestion, same rankTurns conditions,
// DefaultSearchParams via store.SearchFTS / SearchHybrid. Hybrid timing
// includes the per-query embedding call against Ollama, exactly as
// production search pays it.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"
)

func TestZZLatencyProbe(t *testing.T) {
	dataPath := os.Getenv("LOCOMO_DATA")
	if dataPath == "" {
		t.Skip("LOCOMO_DATA not set")
	}
	raw, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	var convs []locoConversation
	if err := json.Unmarshal(raw, &convs); err != nil {
		t.Fatal(err)
	}

	type q struct {
		text string
	}
	var allQs []q
	for i := range convs {
		for _, qa := range convs[i].QA {
			if qa.Category == 5 || len(qa.Evidence) == 0 {
				continue
			}
			allQs = append(allQs, q{text: qa.Question})
		}
	}

	ctx := context.Background()
	for _, scale := range []string{"1conv", "all10"} {
		sessions := loadConversation(convs[0])
		if scale == "all10" {
			var all []locoSession
			for i := range convs {
				all = append(all, loadConversation(convs[i])...)
			}
			sessions = all
		}
		emb, err := newCachedEmbedder("http://localhost:11434", "")
		if err != nil {
			t.Fatal(err)
		}
		store, memMeta, cleanup, err := openStore(ctx, "probe", sessions, "hybrid", emb)
		if err != nil {
			t.Fatal(err)
		}
		nMem := 0
		for range memMeta {
			nMem++
		}

		measure := func(name string, run func(q string) error) {
			var ds []float64
			idx := 0
			runProbe := func() {
				qq := allQs[idx%len(allQs)]
				idx++
				start := time.Now()
				if err := run(qq.text); err != nil {
					t.Fatalf("probe %s: %v", name, err)
				}
				ds = append(ds, float64(time.Since(start).Microseconds())/1000.0)
			}
			for i := 0; i < 5; i++ { // warmup
				runProbe()
			}
			ds = nil
			idx = 0
			for i := 0; i < 100; i++ {
				runProbe()
			}
			slices.Sort(ds)
			p := func(pct float64) float64 {
				return ds[int(float64(len(ds)-1)*pct)]
			}
			fmt.Printf("PROBE %s n=%d memories=%d p50=%.1fms p95=%.1fms max=%.1fms\n",
				name, len(ds), nMem, p(0.5), p(0.95), ds[len(ds)-1])
		}

		measure("fts "+scale, func(question string) error {
			_, err := store.SearchFTS(ctx, "probe", question, memoryRetrievalLimit)
			return err
		})
		measure("hybrid "+scale, func(question string) error {
			qv, err := emb.Embed(ctx, question)
			if err != nil {
				return err
			}
			_, err = store.SearchHybrid(ctx, "probe", question, qv, memoryRetrievalLimit)
			return err
		})
		cleanup()
	}
}
