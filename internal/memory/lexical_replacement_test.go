package memory

import (
	"reflect"
	"testing"
	"time"
)

func lexRow(id, created, content string) Candidate {
	return Candidate{Memory: Memory{ID: id, ProjectID: "p", Category: "dependency", CreatedAt: created, Content: content}}
}

func lexIDs(rows []Candidate) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

// TestLexicalReplacementsArrangesAChainNewestFirst: three pins of one module,
// ranked oldest first, come back newest first, and each older row names every
// newer row that replaces it, newest first.
func TestLexicalReplacementsArrangesAChainNewestFirst(t *testing.T) {
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
	if got := lexIDs(arrangeLexicalReplacements(rows, repl)); !reflect.DeepEqual(got, []string{"v6", "v5", "v4", "other"}) {
		t.Errorf("arranged %v, want [v6 v5 v4 other]", got)
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

// TestArrangeLexicalReplacementsNeverMovesAPin: a pinned row keeps its place
// whichever side of the pair it is on.
func TestArrangeLexicalReplacementsNeverMovesAPin(t *testing.T) {
	rows := []Candidate{lexRow("old", "", ""), lexRow("x", "", ""), lexRow("new", "", "")}
	repl := map[string][]string{"old": {"new"}}
	rows[2].Pinned = true
	if got := lexIDs(arrangeLexicalReplacements(rows, repl)); !reflect.DeepEqual(got, []string{"old", "x", "new"}) {
		t.Errorf("a pinned replacement moved: %v", got)
	}
	rows[2].Pinned, rows[0].Pinned = false, true
	if got := lexIDs(arrangeLexicalReplacements(rows, repl)); !reflect.DeepEqual(got, []string{"old", "x", "new"}) {
		t.Errorf("a row moved ahead of a pinned one: %v", got)
	}
	rows[0].Pinned = false
	if got := lexIDs(arrangeLexicalReplacements(rows, repl)); !reflect.DeepEqual(got, []string{"new", "old", "x"}) {
		t.Errorf("arranged %v, want [new old x]", got)
	}
}
