package memory

import (
	"context"
	"path/filepath"
	"testing"
)

// negFixture describes one retrieval case: what to seed, what must come back,
// and what must not.
//
// reject entries name a key plus the reason it is forbidden. The reason is not
// decoration — "superseded", "duplicate" and "irrelevant" are different
// failure modes with different fixes, and a leak report that only says
// "unexpected result" would leave the same guessing this suite exists to
// remove.
type negFixture struct {
	name     string
	seed     []negSeed
	links    []negLink
	query    string
	limit    int
	want     []string // keys that must be in the window, in any order
	reject   []string // keys that must not be in the window at all
	afterKey map[string]string
	// afterKey maps a key that must still be returned to a key it must rank
	// behind. Demotions here are membership-preserving — they sink a result
	// rather than evicting it — so the honest assertion for a superseded or
	// duplicate memory is its position, not its absence.
}

type negSeed struct {
	key      string
	content  string
	category string
	// importance forces the baseline FTS order. SearchFTS sorts by bm25 rank
	// and then by importance, so equal-scoring rows are ordered by it — the
	// same technique TestProductionSearchDemotesSuperseded uses. Without it a
	// demotion test is decided by token-length noise and passes or fails by
	// chance. Zero means the default 0.7.
	importance float32
}

type negLink struct {
	from, to, relation string
}

// negSearch seeds a fixture, runs the production hybrid search with no query
// vector (so the suite stays hermetic — no Ollama), and returns the returned
// keys in rank order.
func negSearch(t *testing.T, f negFixture) []string {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "neg.sqlite"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	s := NewStore(db, nil)
	if err := s.EnsureProject(ctx, testProject, "/tmp/neg", testProject); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	ids := map[string]string{}
	for _, m := range f.seed {
		imp := m.importance
		if imp == 0 {
			imp = 0.7
		}
		id, err := s.Create(ctx, testProject, Memory{
			Category: m.category, Content: m.content, Source: "manual",
			Importance: imp, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("seed %s: %v", m.key, err)
		}
		ids[m.key] = id
	}
	for _, l := range f.links {
		if err := s.CreateLink(ctx, ids[l.from], ids[l.to], l.relation, 0.95, "manual"); err != nil {
			t.Fatalf("link %s %s %s: %v", l.from, l.relation, l.to, err)
		}
	}

	got, err := s.SearchHybrid(ctx, testProject, f.query, nil, f.limit)
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	out := make([]string, 0, len(got))
	for _, m := range got {
		for key, id := range ids {
			if m.ID == id {
				out = append(out, key)
				break
			}
		}
	}
	return out
}

func negPos(keys []string, key string) int {
	for i, k := range keys {
		if k == key {
			return i
		}
	}
	return -1
}

// negativeRetrievalCases is the review's "what should Ghost NOT return"
// suite: for every query, the memories that must not surface, each with the
// reason.
//
// A memory system that retrieves too much plausible garbage is worse than one
// that retrieves slightly less — recall-only metrics cannot see this failure,
// because a leak still counts as a hit for whatever it displaced.
var negativeRetrievalCases = []negFixture{
	{
		name: "superseded memory ranks below its replacement",
		seed: []negSeed{
			{key: "grafana_current", content: "Grafana is exposed through a Tailscale hostname.", category: "architecture"},
			{key: "grafana_traefik", content: "Grafana is exposed through the Traefik ingress.", category: "architecture"},
		},
		links: []negLink{{"grafana_current", "grafana_traefik", "supersedes"}},
		// The query deliberately favours the superseded row: it names Traefik
		// and "ingress", which only the stale memory matches, so pure
		// relevance ranks it first. Only the supersede penalty can put
		// grafana_current ahead — which is what makes this a test of that
		// demotion rather than of bm25.
		//
		// Both must still survive: demotion sinks the superseded record
		// rather than erasing it, so the historical fact stays inspectable.
		query:    "grafana exposed through traefik ingress",
		limit:    2,
		want:     []string{"grafana_current", "grafana_traefik"},
		afterKey: map[string]string{"grafana_traefik": "grafana_current"},
	},
	{
		name: "duplicate sinks below an unrelated memory",
		seed: []negSeed{
			// All three match the query equally, so bm25 ties and importance
			// decides the baseline: canonical, duplicate, then the unrelated
			// timeline note. Demotion must then sink the duplicate below that
			// third memory — the contract demotion_test.go states as "A
			// (lower-ranked) sinks below the unrelated memory".
			//
			// The duplicate is never expected to fall below its own partner:
			// DemotionPenalties penalises whichever member is already ranked
			// lower, so within a pair the order is reinforced, not inverted.
			{key: "helmfile_canonical", content: "Helmfile environments are global dev and prod.", category: "convention", importance: 0.9},
			{key: "helmfile_duplicate", content: "Helmfile environments are global, dev, and prod.", category: "convention", importance: 0.5},
			{key: "helmfile_timeline", content: "Helmfile environments were discussed last quarter.", category: "fact", importance: 0.1},
		},
		links: []negLink{{"helmfile_canonical", "helmfile_duplicate", "duplicate"}},
		query: "helmfile environments",
		limit: 3,
		want:  []string{"helmfile_canonical", "helmfile_duplicate", "helmfile_timeline"},
		afterKey: map[string]string{
			"helmfile_duplicate": "helmfile_timeline",
		},
	},
	{
		name: "irrelevant-but-similar loses the window",
		seed: []negSeed{
			{key: "grafana_tailscale", content: "Grafana dashboards are served behind the Tailscale hostname.", category: "architecture"},
			{key: "prometheus_cardinality", content: "Prometheus dashboard retention and metric cardinality limits.", category: "gotcha"},
			{key: "sops_keys", content: "The SOPS age key lives in the CI secret store.", category: "fact"},
		},
		// The second shares "dashboard" with the query, so FTS legitimately
		// matches it; it still must not outrank the memory that answers the
		// question. The third shares nothing and must never surface.
		query:  "grafana dashboards",
		limit:  2,
		want:   []string{"grafana_tailscale"},
		reject: []string{"sops_keys"},
	},
	{
		name: "contradiction stays while duplication sinks",
		seed: []negSeed{
			{key: "db_postgres", content: "Production uses PostgreSQL for the primary datastore.", category: "fact", importance: 0.9},
			{key: "db_postgres_dup", content: "Production uses PostgreSQL for its primary datastore.", category: "fact", importance: 0.5},
			{key: "db_mysql", content: "Production uses MySQL for the primary datastore.", category: "fact", importance: 0.1},
		},
		links: []negLink{
			{"db_postgres", "db_mysql", "contradicts"},
			{"db_postgres", "db_postgres_dup", "duplicate"},
		},
		// The two relations are handled differently, and a system that
		// treats both as "similar" would drop the row carrying the actual
		// disagreement. db_mysql must survive at full rank while the
		// restatement of db_postgres sinks below it.
		query: "what database does production use",
		limit: 3,
		want:  []string{"db_postgres", "db_mysql", "db_postgres_dup"},
		afterKey: map[string]string{
			"db_postgres_dup": "db_mysql",
		},
	},
	{
		name: "scope difference is not suppressed",
		seed: []negSeed{
			{key: "dev_sqlite", content: "The project database for development is SQLite.", category: "fact", importance: 0.9},
			{key: "dev_sqlite_dup", content: "The project database for development uses SQLite.", category: "fact", importance: 0.5},
			{key: "prod_postgres", content: "The project database for production is PostgreSQL.", category: "fact", importance: 0.1},
		},
		links: []negLink{{"dev_sqlite", "dev_sqlite_dup", "duplicate"}},
		// Two scopes, not competing claims about one: neither row may be
		// suppressed, while the restatement still sinks.
		query: "what is the project database",
		limit: 3,
		want:  []string{"dev_sqlite", "prod_postgres", "dev_sqlite_dup"},
		afterKey: map[string]string{
			"dev_sqlite_dup": "prod_postgres",
		},
	},
}

// negRepeats is how many times each fixture runs. IDs are random hex and
// decayRank breaks equal-score ties on ID, so an ordering that rests on a
// demotion rather than on pure relevance would otherwise be decided by
// chance: a single run can pass even with the demotion disabled. Repeating
// makes the expectation hold across ID orderings, so a missing demotion
// fails reliably instead of flaking.
const negRepeats = 8

func TestNegativeRetrievalFixtures(t *testing.T) {
	for _, f := range negativeRetrievalCases {
		t.Run(f.name, func(t *testing.T) {
			for run := 0; run < negRepeats; run++ {
				got := negSearch(t, f)

				for _, key := range f.want {
					if negPos(got, key) < 0 {
						t.Errorf("run %d: expected %q in results, got %v", run, key, got)
					}
				}
				for _, key := range f.reject {
					if p := negPos(got, key); p >= 0 {
						t.Errorf("run %d: leak: %q returned at rank %d for %q, but it must not appear at all (got %v)",
							run, key, p+1, f.query, got)
					}
				}
				for key, mustBeAfter := range f.afterKey {
					ki, wi := negPos(got, key), negPos(got, mustBeAfter)
					if ki < 0 || wi < 0 {
						continue // absence already reported by want
					}
					if ki <= wi {
						t.Errorf("run %d: ranking: %q at rank %d outranks %q at rank %d (got %v) — "+
							"the demotion that separates them did not apply", run, key, ki+1, mustBeAfter, wi+1, got)
					}
				}
			}
		})
	}
}

// TestNegativeRetrievalRejectReasonsExist keeps the reject lists honest: a
// key that is neither seeded nor reachable cannot leak, so naming one would
// let a fixture pass vacuously.
func TestNegativeRetrievalRejectReasonsExist(t *testing.T) {
	for _, f := range negativeRetrievalCases {
		seeded := map[string]bool{}
		for _, m := range f.seed {
			seeded[m.key] = true
		}
		for _, key := range f.reject {
			if !seeded[key] {
				t.Errorf("%s: reject key %q is not seeded, so it can never be returned and proves nothing", f.name, key)
			}
		}
		for _, key := range f.want {
			if !seeded[key] {
				t.Errorf("%s: want key %q is not seeded", f.name, key)
			}
		}
		if len(f.want) == 0 {
			t.Errorf("%s: no positive expectation — a fixture that only forbids things cannot tell a correct empty result from a broken search", f.name)
		}
		for key := range f.afterKey {
			found := false
			for _, w := range f.want {
				if w == key {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: afterKey target %q is not in want — the position check is skipped when a row is absent, so evicting it entirely would pass unnoticed", f.name, key)
			}
		}
		if len(f.reject) == 0 && len(f.afterKey) == 0 {
			t.Errorf("%s: asserts nothing negative; it belongs in the positive suite instead", f.name)
		}
	}
}

// TestNegativeRetrievalRejectKeyIsDistinctFromPositive guards the fixture
// table itself: a key listed as both wanted and forbidden can never be
// satisfied, and the case would fail confusingly rather than at the point of
// the contradiction.
func TestNegativeRetrievalRejectKeyIsDistinctFromPositive(t *testing.T) {
	for _, f := range negativeRetrievalCases {
		for _, r := range f.reject {
			for _, w := range f.want {
				if r == w {
					t.Errorf("%s: %q is both wanted and rejected", f.name, r)
				}
			}
			if _, conflict := f.afterKey[r]; conflict {
				t.Errorf("%s: %q is rejected but also required to rank somewhere", f.name, r)
			}
		}
	}
}
