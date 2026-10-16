package memory

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func lexRow(id, created, content string) Candidate {
	return Candidate{Memory: Memory{ID: id, ProjectID: "p", Category: "dependency", CreatedAt: created, Content: content}}
}

// TestLexicalReplacementsNamesAChainNewestFirst: three pins of one module, and
// each older row names every newer row that replaces it, newest first.
func TestLexicalReplacementsNamesAChainNewestFirst(t *testing.T) {
	rows := []Candidate{
		lexRow("v4", "2026-09-01 10:00:00", "go.mod pins github.com/jackc/pgx/v4 at v4.18.1 and the pool is built with pgxpool.Connect."),
		lexRow("other", "2026-09-02 10:00:00", "The ledger nightly job reconciles every account against the bank feed."),
		lexRow("v5", "2026-09-10 10:00:00", "go.mod pins github.com/jackc/pgx/v5 at v5.6.0 and the pool is built with pgxpool.New."),
		lexRow("v6", "2026-09-20 10:00:00", "go.mod pins github.com/jackc/pgx/v6 at v6.0.1 and the pool is built with pgxpool.NewWithConfig."),
	}
	now := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	repl := lexicalReplacements(rows, nil, now)
	if got := repl["v4"]; !reflect.DeepEqual(got, []string{"v6", "v5"}) {
		t.Errorf("v4 replaced by %v, want [v6 v5]", got)
	}
	if got := repl["v5"]; !reflect.DeepEqual(got, []string{"v6"}) {
		t.Errorf("v5 replaced by %v, want [v6]", got)
	}
	if _, ok := repl["other"]; ok {
		t.Errorf("an unrelated row is paired: %v", repl)
	}
}

// TestLexicalReplacementsNeedsADistinctChronology: two rows created in the same
// second have no order to read a replacement from, so they are not paired, and
// an unreadable stamp is no claim either.
func TestLexicalReplacementsNeedsADistinctChronology(t *testing.T) {
	now := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	for name, stamps := range map[string][2]string{
		"same second": {"2026-09-01 10:00:00", "2026-09-01 10:00:00"},
		"unreadable":  {"yesterday", "2026-09-01 10:00:00"},
	} {
		rows := []Candidate{
			lexRow("a", stamps[0], "go.mod pins github.com/jackc/pgx/v4 at v4.18.1 and the pool is built with pgxpool.Connect."),
			lexRow("b", stamps[1], "go.mod pins github.com/jackc/pgx/v5 at v5.6.0 and the pool is built with pgxpool.New."),
		}
		if repl := lexicalReplacements(rows, nil, now); len(repl) != 0 {
			t.Errorf("%s: paired %v", name, repl)
		}
	}
}

// TestLexicalReplacementsKeepsParallelFactsApart: two live notes in one sentence
// frame about two different subjects share a value and change another, which is
// exactly a value swap to the similarity and value rules. The plain word that
// names the subject differs in place (auth/billing, staging/reporting, the make
// target), so they are not a replacement. The replacements below them reword
// around their values instead, and still pair.
func TestLexicalReplacementsKeepsParallelFactsApart(t *testing.T) {
	now := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name, older, newer string
		pair               bool
	}{
		{"different service", "auth-service runs on node-3 and listens on port 8080 behind Nginx",
			"billing-service runs on node-7 and listens on port 8080 behind Nginx", false},
		{"different database", "The staging database is Postgres 15 hosted in us-east",
			"The reporting database is Postgres 16 hosted in us-east", false},
		// Decided as NOT a pair: `make lint` in place of `make test` can be a
		// changed procedure or a second step beside the first, and the text cannot
		// say which. A code-span word counts by its shape, and a plain word swapped
		// in place is a different step; an unmarked pair is the safe reading.
		{"different make target", "Run `make build` then `make test` before PRs",
			"Run `make build` then `make lint` before PRs", false},
		{"pgx upgrade", "go.mod pins github.com/jackc/pgx/v4 at v4.18.1; the pool is built with pgxpool.Connect and the code imports pgx/v4 everywhere.",
			"go.mod now pins github.com/jackc/pgx/v5 at v5.6.0; the pool is built with pgxpool.New and every import moved to pgx/v5.", true},
		{"reversed store", "Per-session state lives in Redis: the bootstrap sets SESSION_STORE=redis and every session key carries a 3600s TTL.",
			"Per-session state lives in Postgres, not Redis: the bootstrap sets SESSION_STORE=postgres and rows in the public.sessions table are the sessions.", true},
		{"terraform upgrade", "The infra repo pins Terraform 1.5.7 in .terraform-version; CI runs terraform plan on every PR.",
			"The infra repo now pins Terraform 1.6.2 in .terraform-version; CI runs terraform plan on every PR.", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows := []Candidate{lexRow("old", "2026-09-01 10:00:00", tc.older), lexRow("new", "2026-09-10 10:00:00", tc.newer)}
			got := lexicalReplacements(rows, nil, now)
			if paired := reflect.DeepEqual(got["old"], []string{"new"}); paired != tc.pair {
				t.Errorf("paired = %v (%v), want %v", paired, got, tc.pair)
			}
		})
	}
}

// worstCaseRows is the session-start shape that made the alignment expensive: one
// bucket of 45 near-identical notes of about 8KB, each ending in a different
// version, so every pair clears the similarity bar and reaches the alignment.
func worstCaseRows() []Candidate {
	var body strings.Builder
	for body.Len() < 8000 {
		body.WriteString("The deploy runbook for the ledger service covers the rollout order, the health gates and the rollback steps. ")
	}
	rows := make([]Candidate, 45)
	for i := range rows {
		created := time.Date(2026, 9, 1, 0, 0, i, 0, time.UTC).Format(StoredStampLayout)
		rows[i] = lexRow(fmt.Sprintf("r%02d", i), created, "go.mod pins the runbook tool. "+body.String()+fmt.Sprintf("The tool is pinned at v1.%d.0.", i))
	}
	return rows
}

// TestLexicalReplacementsStaysCheapOnLongNearIdenticalNotes: the pair rule runs
// on the session-start path, so its cost on the worst bucket it can be handed
// is bounded, and the pairs in it are still found.
func TestLexicalReplacementsStaysCheapOnLongNearIdenticalNotes(t *testing.T) {
	rows := worstCaseRows()
	now := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	start := time.Now()
	repl := lexicalReplacements(rows, nil, now)
	took := time.Since(start)
	t.Logf("45 x 8KB notes: %s", took)
	if took > 250*time.Millisecond {
		t.Errorf("lexicalReplacements took %s on 45 near-identical 8KB notes, want well under 250ms", took)
	}
	if got := len(repl["r00"]); got != 44 {
		t.Errorf("r00 names %d replacements, want 44: every later version replaces it", got)
	}
}

func BenchmarkLexicalReplacementsWorstCase(b *testing.B) {
	rows := worstCaseRows()
	now := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	for b.Loop() {
		lexicalReplacements(rows, nil, now)
	}
}

// The recategorised-replacement storyline's pair, word for word: the cache is
// saved as a fact, and the team's change to it as a decision, the category an
// agent records a choice in.
const (
	cacheMemcachedFact = "Lookup responses are cached in Memcached: internal/cache dials memcached-01.corp.example:11211 and every entry lives 300s."
	cacheRistrettoDec  = "Decision: lookup responses are cached in-process with ristretto instead of Memcached; internal/cache keeps a 64 MB budget per pod, entries still live 300s, and memcached-01 is being shut down."
)

func lexRowIn(id, category, created, content string) Candidate {
	c := lexRow(id, created, content)
	c.Category = category
	return c
}

// TestLexicalReplacementsPairsAReplacementSavedInAnotherCategory: a decision
// that replaces a fact was never paired, because a pair across categories was
// held to the save path's cross-category FOLD bar (token Jaccard >= 0.7), which
// exists to keep paraphrases of one rule from splitting across categories and
// which a replacement that rewords does not clear. The category says how a note
// was filed, not what it is about, so the pair is judged by the bar a pair in
// one category is judged by. The same texts in one category are the control:
// they paired before the change and still do.
func TestLexicalReplacementsPairsAReplacementSavedInAnotherCategory(t *testing.T) {
	now := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name, olderCat, newerCat, older, newer string
	}{
		{"fact replaced by a decision", "fact", "decision", cacheMemcachedFact, cacheRistrettoDec},
		{"same texts in one category", "fact", "fact", cacheMemcachedFact, cacheRistrettoDec},
		// The direction is the stamps', never the categories': the newer row is
		// the replacement whichever category it was filed in.
		{"decision replaced by a fact", "decision", "fact", cacheRistrettoDec, cacheMemcachedFact},
		{"dependency replaced by a gotcha", "dependency", "gotcha",
			"go.mod pins github.com/jackc/pgx/v4 at v4.18.1; the pool is built with pgxpool.Connect and the code imports pgx/v4 everywhere.",
			"Watch out: go.mod now pins github.com/jackc/pgx/v5 at v5.6.0, so pgxpool.Connect is gone; the pool is built with pgxpool.New and every import moved to pgx/v5."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ot, nt := tokenizeContent(tc.older), tokenizeContent(tc.newer)
			if tc.olderCat != tc.newerCat && jaccard(ot, nt) >= upsertCrossCategoryThreshold {
				t.Fatalf("fixture Jaccard %.2f clears the cross-category fold bar; the test would not reach the defect", jaccard(ot, nt))
			}
			if mergeScore(ot, nt) <= 0 {
				t.Fatalf("fixture does not clear the same-category bar; the test would assert nothing")
			}
			rows := []Candidate{
				lexRowIn("old", tc.olderCat, "2026-09-01 10:00:00", tc.older),
				lexRowIn("new", tc.newerCat, "2026-09-10 10:00:00", tc.newer),
			}
			if got := lexicalReplacements(rows, nil, now); !reflect.DeepEqual(got, map[string][]string{"old": {"new"}}) {
				t.Errorf("%s (%s) then %s (%s): pairs %v, want old replaced by new", tc.olderCat, tc.older, tc.newerCat, tc.newer, got)
			}
		})
	}
}

// TestLexicalReplacementsAcrossCategoriesKeepsEveryOtherRule: lifting the
// category bar lifts nothing else. A cross-category paraphrase that changes no
// value is a restatement, not a replacement; two parallel facts about different
// subjects in one frame stay apart; two environments' values are two settings;
// and an explicit edge wins. Every fixture clears the similarity bar, so the named rule is the one
// keeping it apart.
func TestLexicalReplacementsAcrossCategoriesKeepsEveryOtherRule(t *testing.T) {
	now := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	restate := "Decision: keep caching lookup responses in Memcached; internal/cache dials memcached-01.corp.example:11211 and entries live 300s."
	cases := []struct {
		name         string
		older, newer Candidate
		edges        []LinkEdge
		want         map[string][]string
	}{
		{name: "paraphrase, no value changed",
			older: lexRowIn("old", "fact", "2026-09-01 10:00:00", cacheMemcachedFact),
			newer: lexRowIn("new", "decision", "2026-09-10 10:00:00", restate)},
		{name: "parallel facts about two services",
			older: lexRowIn("old", "architecture", "2026-09-01 10:00:00", "auth-service runs on node-3 and listens on port 8080 behind Nginx"),
			newer: lexRowIn("new", "fact", "2026-09-10 10:00:00", "billing-service runs on node-7 and listens on port 8080 behind Nginx")},
		{name: "scopes differ",
			older: func() Candidate {
				c := lexRowIn("old", "fact", "2026-09-01 10:00:00", cacheMemcachedFact)
				c.Scope = map[string]string{"env": "staging"}
				return c
			}(),
			newer: lexRowIn("new", "decision", "2026-09-10 10:00:00", cacheRistrettoDec)},
		{name: "explicit supersedes edge",
			older: lexRowIn("old", "fact", "2026-09-01 10:00:00", cacheMemcachedFact),
			newer: lexRowIn("new", "decision", "2026-09-10 10:00:00", cacheRistrettoDec),
			edges: []LinkEdge{{From: "new", To: "old", Relation: "supersedes"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if mergeScore(tokenizeContent(tc.older.Content), tokenizeContent(tc.newer.Content)) <= 0 {
				t.Fatalf("fixture does not clear the similarity bar; the test would assert nothing")
			}
			got := lexicalReplacements([]Candidate{tc.older, tc.newer}, tc.edges, now)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("pairs %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLexicalReplacementsPairsAStandingCategoryRow: convention and preference
// are standing categories, which resolve never retires. That exemption is a rule
// about a WRITE (resolve marks nothing in them); the replacement guess writes
// nothing and withholds nothing, so a standing row is a party to a pair like any
// other, in either direction, and the reader sees it marked beside the note that
// may replace it. The superseded-convention storyline's pair, a convention
// replaced by a convention, already paired; this pins the pair across
// categories.
func TestLexicalReplacementsPairsAStandingCategoryRow(t *testing.T) {
	now := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	semver := "Release tags follow semver: tag `vMAJOR.MINOR.PATCH` from main and run `make release` to publish."
	calver := "Release tags follow calendar versioning now: tag `YYYY.MM.N` from main and run `make release` to publish."
	cases := []struct{ name, olderCat, newerCat, older, newer string }{
		{"convention replaced by a decision", "convention", "decision", semver, calver},
		{"fact replaced by a preference", "fact", "preference", cacheMemcachedFact, strings.TrimPrefix(cacheRistrettoDec, "Decision: ")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ot, nt := tokenizeContent(tc.older), tokenizeContent(tc.newer)
			if jaccard(ot, nt) >= upsertCrossCategoryThreshold || mergeScore(ot, nt) <= 0 {
				t.Fatalf("fixture Jaccard %.2f / mergeScore %.2f: it must sit between the two bars", jaccard(ot, nt), mergeScore(ot, nt))
			}
			rows := []Candidate{
				lexRowIn("old", tc.olderCat, "2026-09-01 10:00:00", tc.older),
				lexRowIn("new", tc.newerCat, "2026-09-10 10:00:00", tc.newer),
			}
			if got := lexicalReplacements(rows, nil, now); !reflect.DeepEqual(got, map[string][]string{"old": {"new"}}) {
				t.Errorf("pairs %v, want old replaced by new", got)
			}
		})
	}
}
