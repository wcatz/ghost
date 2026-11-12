package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	"github.com/wcatz/ghost/internal/memory"
)

// StalenessVersion is one temporal version of a fact.
type StalenessVersion struct {
	Content string `json:"content"`
	AgeDays int    `json:"age_days"`
}

// StalenessProbe queries a scenario's fact.
type StalenessProbe struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// StalenessScenario is one updated-fact case.
type StalenessScenario struct {
	Name     string             `json:"name"`
	Versions []StalenessVersion `json:"versions"`
	Probes   []StalenessProbe   `json:"probes"`
}

// LoadStalenessScenarios reads scenarios, one JSON per line.
func LoadStalenessScenarios(r io.Reader) ([]StalenessScenario, error) {
	var out []StalenessScenario
	if err := decodeJSONL(r, func(raw json.RawMessage) error {
		var s StalenessScenario
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		if len(s.Versions) < 2 {
			return fmt.Errorf("scenario %q needs at least 2 versions", s.Name)
		}
		out = append(out, s)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// ProbeOutcome is the judgment for one probe of one scenario.
type ProbeOutcome struct {
	Scenario   string
	ProbeType  string
	FreshFound bool
	FreshWins  bool
	FreshTop1  bool
}

// StalenessSummary aggregates outcomes for one probe type.
type StalenessSummary struct {
	ProbeType  string
	Probes     int
	FreshFound int
	FreshWins  int
	FreshTop1  int
}

func stalenessAll(outcomes []ProbeOutcome) StalenessSummary {
	all := StalenessSummary{ProbeType: "all"}
	for _, o := range outcomes {
		all.Probes++
		if o.FreshFound {
			all.FreshFound++
		}
		if o.FreshWins {
			all.FreshWins++
		}
		if o.FreshTop1 {
			all.FreshTop1++
		}
	}
	return all
}

// RunStaleness seeds and probes the staleness scenarios.
func RunStaleness(ctx context.Context, scenarios []StalenessScenario, p memory.SearchParams, seedSupersedes bool) ([]ProbeOutcome, error) {
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		return nil, err
	}
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer store.Close() //nolint:errcheck

	const project = "staleness"
	if err := store.EnsureProject(ctx, project, "/bench/staleness", project); err != nil {
		return nil, err
	}

	versionIDs := make([][]string, len(scenarios))
	stamp := newCorpusStamp()
	for i, sc := range scenarios {
		versionIDs[i] = make([]string, len(sc.Versions))
		for j, v := range sc.Versions {
			id, err := store.CreateWithIDFromCorpus(ctx, project, corpusID(project, fmt.Sprintf("%s-v%d", sc.Name, j)), memory.Memory{
				Category: "dependency", Content: v.Content, Importance: 0.7, Source: "mcp",
			})
			if err != nil {
				return nil, fmt.Errorf("seed %s v%d: %w", sc.Name, j, err)
			}
			if err := stamp.apply(ctx, db, id, v.AgeDays); err != nil {
				return nil, fmt.Errorf("stamp %s v%d: %w", sc.Name, j, err)
			}
			versionIDs[i][j] = id
		}
		if seedSupersedes {
			ids := versionIDs[i]
			for newer := len(ids) - 1; newer >= 1; newer-- {
				for older := newer - 1; older >= 0; older-- {
					if err := store.CreateLink(ctx, ids[newer], ids[older], "supersedes", 1.0, "llm"); err != nil {
						return nil, fmt.Errorf("seed supersedes %s: %w", sc.Name, err)
					}
				}
			}
		}
	}

	var outcomes []ProbeOutcome
	for i, sc := range scenarios {
		ids := versionIDs[i]
		freshID, staleIDs := ids[len(ids)-1], ids[:len(ids)-1]
		for _, probe := range sc.Probes {
			results, err := store.SearchHybridParams(ctx, project, probe.Text, nil, scoreK, p)
			if err != nil {
				return nil, fmt.Errorf("probe %s/%s: %w", sc.Name, probe.Type, err)
			}
			ranked := make([]string, len(results))
			for k, m := range results {
				ranked[k] = m.ID
			}
			found, wins, top1 := judgeProbe(ranked, freshID, staleIDs)
			outcomes = append(outcomes, ProbeOutcome{
				Scenario: sc.Name, ProbeType: probe.Type,
				FreshFound: found, FreshWins: wins, FreshTop1: top1,
			})
		}
	}
	return outcomes, nil
}

func judgeProbe(ranked []string, freshID string, staleIDs []string) (found, wins, top1 bool) {
	rank := func(id string) int {
		for i, r := range ranked {
			if r == id {
				return i
			}
		}
		return -1
	}
	fr := rank(freshID)
	if fr == -1 {
		return false, false, false
	}
	wins = true
	for _, sid := range staleIDs {
		if sr := rank(sid); sr != -1 && sr < fr {
			wins = false
			break
		}
	}
	return true, wins, fr == 0
}

// SummarizeStaleness aggregates outcomes by probe type.
func SummarizeStaleness(outcomes []ProbeOutcome) []StalenessSummary {
	byType := map[string]*StalenessSummary{}
	for _, o := range outcomes {
		s := byType[o.ProbeType]
		if s == nil {
			s = &StalenessSummary{ProbeType: o.ProbeType}
			byType[o.ProbeType] = s
		}
		s.Probes++
		if o.FreshFound {
			s.FreshFound++
		}
		if o.FreshWins {
			s.FreshWins++
		}
		if o.FreshTop1 {
			s.FreshTop1++
		}
	}
	var out []StalenessSummary
	for _, key := range []string{"state", "premise"} {
		if s, ok := byType[key]; ok {
			out = append(out, *s)
			delete(byType, key)
		}
	}
	for _, s := range byType {
		out = append(out, *s)
	}
	return out
}

// FormatStaleness renders the summary table plus every failing probe.
func FormatStaleness(outcomes []ProbeOutcome) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%-10s %7s %12s %12s %10s\n", "probe", "n", "fresh-found", "fresh-wins", "fresh@1")
	rows := append(SummarizeStaleness(outcomes), stalenessAll(outcomes))
	for _, s := range rows {
		n := float64(s.Probes)
		fmt.Fprintf(&b, "%-10s %7d %12.3f %12.3f %10.3f\n",
			s.ProbeType, s.Probes, float64(s.FreshFound)/n, float64(s.FreshWins)/n, float64(s.FreshTop1)/n)
	}
	fails := 0
	for _, o := range outcomes {
		if !o.FreshWins {
			fails++
		}
	}
	if fails > 0 {
		fmt.Fprintf(&b, "\n%d failing probes (stale version outranked fresh, or fresh not retrieved):\n", fails)
		for _, o := range outcomes {
			if !o.FreshWins {
				fmt.Fprintf(&b, "  %s/%s (found=%v)\n", o.Scenario, o.ProbeType, o.FreshFound)
			}
		}
	}
	return b.String()
}
