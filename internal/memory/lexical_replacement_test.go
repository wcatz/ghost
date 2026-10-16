package memory

import (
	"reflect"
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
