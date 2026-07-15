package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	"github.com/wcatz/ghost/internal/memory"
)

// The recency-trap suite is the safety counterweight to the staleness suite.
// Staleness rewards newest-wins; the trap punishes it. Each scenario pits an
// OLD but still-correct memory against NEWER keyword-overlapping distractors,
// and a probe whose right answer is the old one. correct-wins = the correct
// memory outranks every trap. A global recency prior tuned to ace staleness
// will fail these once its weight is high enough to promote the fresh
// distractor — which is exactly the bound this suite measures. See
// docs/benchmarks.md Phase 3.

// TrapVersion is a memory with a controlled age.
type TrapVersion struct {
	Content string `json:"content"`
	AgeDays int    `json:"age_days"`
}

// TrapScenario: the correct answer is old; traps are newer distractors.
type TrapScenario struct {
	Name    string        `json:"name"`
	Correct TrapVersion   `json:"correct"`
	Traps   []TrapVersion `json:"traps"`
	Probes  []struct {
		Text string `json:"text"`
	} `json:"probes"`
}

// LoadTrapScenarios reads trap scenarios, one JSON per line.
func LoadTrapScenarios(r io.Reader) ([]TrapScenario, error) {
	var out []TrapScenario
	if err := decodeJSONL(r, func(raw json.RawMessage) error {
		var s TrapScenario
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		if len(s.Traps) == 0 {
			return fmt.Errorf("trap scenario %q needs at least one trap", s.Name)
		}
		out = append(out, s)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// TrapOutcome is the judgment for one probe.
type TrapOutcome struct {
	Scenario     string
	CorrectFound bool // the correct (old) memory was retrieved at all
	CorrectWins  bool // correct outranks every trap present in the results
}

// RunRecencyTrap seeds every scenario (correct + traps) into one shared store
// with backdated created_at, probes with SearchHybridParams over the FTS path
// (mirroring the staleness suite), and judges whether the correct old memory
// outranks its newer distractors.
func RunRecencyTrap(ctx context.Context, scenarios []TrapScenario, p memory.SearchParams) ([]TrapOutcome, error) {
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		return nil, err
	}
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer store.Close() //nolint:errcheck

	const project = "recencytrap"
	if err := store.EnsureProject(ctx, project, "/bench/recencytrap", project); err != nil {
		return nil, err
	}

	type seeded struct {
		correctID string
		trapIDs   []string
	}
	seed := make([]seeded, len(scenarios))
	for i, sc := range scenarios {
		cid, err := store.Create(ctx, project, memory.Memory{
			Category: "fact", Content: sc.Correct.Content, Importance: 0.7, Source: "mcp",
		})
		if err != nil {
			return nil, fmt.Errorf("seed %s correct: %w", sc.Name, err)
		}
		if err := backdate(ctx, db, cid, sc.Correct.AgeDays); err != nil {
			return nil, err
		}
		var tids []string
		for j, tv := range sc.Traps {
			tid, err := store.Create(ctx, project, memory.Memory{
				Category: "fact", Content: tv.Content, Importance: 0.7, Source: "mcp",
			})
			if err != nil {
				return nil, fmt.Errorf("seed %s trap%d: %w", sc.Name, j, err)
			}
			if err := backdate(ctx, db, tid, tv.AgeDays); err != nil {
				return nil, err
			}
			tids = append(tids, tid)
		}
		seed[i] = seeded{correctID: cid, trapIDs: tids}
	}

	var outcomes []TrapOutcome
	for i, sc := range scenarios {
		for _, probe := range sc.Probes {
			results, err := store.SearchHybridParams(ctx, project, probe.Text, nil, scoreK, p)
			if err != nil {
				return nil, fmt.Errorf("trap %s: %w", sc.Name, err)
			}
			ranked := make([]string, len(results))
			for k, m := range results {
				ranked[k] = m.ID
			}
			// A correct memory that beats every retrieved trap "wins"; the
			// judge reuses judgeProbe with correct as the "fresh" role.
			found, wins, _ := judgeProbe(ranked, seed[i].correctID, seed[i].trapIDs)
			outcomes = append(outcomes, TrapOutcome{
				Scenario: sc.Name, CorrectFound: found, CorrectWins: wins,
			})
		}
	}
	return outcomes, nil
}

// TrapCorrectWins is the fraction of probes where the correct old memory
// outranked every trap.
func TrapCorrectWins(outcomes []TrapOutcome) float64 {
	if len(outcomes) == 0 {
		return 0
	}
	wins := 0
	for _, o := range outcomes {
		if o.CorrectWins {
			wins++
		}
	}
	return float64(wins) / float64(len(outcomes))
}
