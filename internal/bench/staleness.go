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

// The staleness suite measures the failure users complain about most: an
// agent retrieving a superseded fact ("prod runs Postgres 14") after the
// update ("migrated to 16") is already in the store. Modeled on the MemTrace
// Update-Error class and STALE's probe design — see docs/benchmarks.md
// ("Phase 3"). Deterministic, judge-free; expected to expose that search has
// no recency signal until supersedes-aware ranking ships.

// StalenessVersion is one temporal version of a fact; AgeDays backdates its
// created_at. Versions are ordered oldest → newest in the fixture.
type StalenessVersion struct {
	Content string `json:"content"`
	AgeDays int    `json:"age_days"`
}

// StalenessProbe queries a scenario's fact. Type "state" is neutral phrasing;
// "premise" presupposes the outdated state.
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
	FreshFound bool // newest version retrieved at all (top-k)
	FreshWins  bool // newest ranked above every older version
	FreshTop1  bool // newest is the overall top result
}

// StalenessSummary aggregates outcomes for one probe type.
type StalenessSummary struct {
	ProbeType  string
	Probes     int
	FreshFound int
	FreshWins  int
	FreshTop1  int
}

// RunStaleness seeds every scenario's versions into one shared store (all
// scenarios act as each other's clutter), backdates created_at per version,
// and probes with Ghost's production search over the FTS path (no embedding
// fixtures — the keyword path is where stale/fresh versions collide hardest,
// since both match the fact's terms). The SearchParams are passed through to
// SearchHybridParams so the recency prior can be swept: at the production
// default (RecencyWeight 0) this measures today's behavior; with a recency
// weight it measures whether the fresh version can be lifted above its
// superseded siblings.
func RunStaleness(ctx context.Context, scenarios []StalenessScenario, p memory.SearchParams) ([]ProbeOutcome, error) {
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

	// versionIDs[i][j] = store ID of scenario i's j-th version (oldest first).
	versionIDs := make([][]string, len(scenarios))
	for i, sc := range scenarios {
		versionIDs[i] = make([]string, len(sc.Versions))
		for j, v := range sc.Versions {
			id, err := store.Create(ctx, project, memory.Memory{
				Category: "fact", Content: v.Content, Importance: 0.7, Source: "mcp",
			})
			if err != nil {
				return nil, fmt.Errorf("seed %s v%d: %w", sc.Name, j, err)
			}
			if err := backdate(ctx, db, id, v.AgeDays); err != nil {
				return nil, fmt.Errorf("backdate %s v%d: %w", sc.Name, j, err)
			}
			versionIDs[i][j] = id
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

// judgeProbe evaluates one ranked result list: was the fresh version found,
// did it beat every stale sibling, and did it take the top spot overall. A
// probe where the fresh version is absent entirely fails all three — an agent
// that retrieves only the stale fact is the worst case this suite exists to
// measure.
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

// backdate rewrites a memory's timestamps; Create always stamps now, and the
// suite needs realistic version ages. Raw SQL on the bench-owned store only.
func backdate(ctx context.Context, db *sql.DB, id string, ageDays int) error {
	_, err := db.ExecContext(ctx,
		`UPDATE memories SET created_at = datetime('now', ?), updated_at = datetime('now', ?) WHERE id = ?`,
		fmt.Sprintf("-%d days", ageDays), fmt.Sprintf("-%d days", ageDays), id)
	return err
}

// SummarizeStaleness aggregates outcomes by probe type (sorted state before
// premise for stable output).
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
	for _, s := range byType { // any nonstandard probe types, appended last
		out = append(out, *s)
	}
	return out
}

// FormatStaleness renders the summary table plus every failing probe, so the
// report names exactly which facts search got wrong.
func FormatStaleness(outcomes []ProbeOutcome) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%-10s %7s %12s %12s %10s\n", "probe", "n", "fresh-found", "fresh-wins", "fresh@1")
	for _, s := range SummarizeStaleness(outcomes) {
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
