package memory

import (
	"context"
	"testing"
)

// The pairs are the storyline eval's own texts (eval/storyline), so the rule is
// held to the replacements that exposed the defect, and the must-fold rows are
// the restatements the dedup tests in this package already require to fold.
func TestStatesDifferentValue(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"version upgrade",
			"go.mod pins github.com/jackc/pgx/v4 at v4.18.1; the pool is built with pgxpool.Connect and the code imports pgx/v4 everywhere.",
			"go.mod now pins github.com/jackc/pgx/v5 at v5.6.0; the pool is built with pgxpool.New and every import moved to pgx/v5.",
			true},
		{"renamed endpoint",
			"Partners submit shipment events with POST /v1/ingest, served by ingestHandler in api/ingest.go; the body is a JSON array of up to 500 events.",
			"The shipment-event endpoint was renamed: partners now call POST /v2/events, served by eventsHandler in api/events.go, and the old /v1/ingest path returns 410 Gone.",
			true},
		{"reversed decision",
			"Per-session state lives in Redis: the bootstrap sets SESSION_STORE=redis and every session key carries a 3600s TTL.",
			"Per-session state lives in Postgres, not Redis: the bootstrap sets SESSION_STORE=postgres and rows in the public.sessions table are the sessions.",
			true},
		{"changed endpoint URL",
			"The ledger API base URL is https://ledger-v1.corp.example/api.",
			"The ledger API base URL is https://ledger-v2.corp.example/api.",
			true},
		{"changed number", "The database pool is capped at max_conns=24.", "The database pool is capped at max_conns=180.", true},
		{"two units on one number", "the cache holds 16gb", "the cache holds 16mb", true},

		{"identical", "cache TTL is 300 seconds", "cache TTL is 300 seconds", false},
		{"unit written two ways", "cache TTL is 300s", "the cache TTL is set to 300 seconds in config", false},
		{"unit written two ways, reversed", "the cache TTL is set to 300 seconds", "cache TTL is 300s", false},
		{"restatement with punctuation", "default session cache TTL: 300 seconds, configured in the server config", "the session cache TTL is 300 seconds", false},
		{"one side adds a value", "the session cache TTL is 300 seconds", "the session cache TTL is 300 seconds, set in config.yaml", false},
		{"no values at all", "Ollama embedding worker sweeps unembedded memories every two minutes",
			"Ollama embedding worker sweeps the unembedded memories every two minutes for cluster projects", false},
		{"reworded prose, same values", "The pool is built with pgxpool.New in go.mod v5.6.0.", "pgxpool.New builds the pool; go.mod is at v5.6.0", false},
		{"capitalised sentence start is not a value", "The cache key is the request path", "Cache key is the request path, per the router", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := statesDifferentValue(tt.a, tt.b); got != tt.want {
				t.Errorf("statesDifferentValue = %v, want %v\n a=%q %v\n b=%q %v",
					got, tt.want, tt.a, valueTokens(tt.a), tt.b, valueTokens(tt.b))
			}
			if got := statesDifferentValue(tt.b, tt.a); got != tt.want {
				t.Errorf("statesDifferentValue is not symmetric: (b, a) = %v, want %v", got, tt.want)
			}
		})
	}
}

// A save that replaces a claim — the same sentence with a different version —
// scores above the fold bar (0.54 for this pair), and the fold linked the
// replacement to the claim it replaces as a 'duplicate' and strengthened the
// stale row: importance 0.70 -> 0.84, access_count 0 -> 1. The control arm is a
// true restatement of the replacement, which must still fold, so a guard that
// refused every fold cannot pass this test either.
func TestUpsertDoesNotFoldAReplacementIntoTheClaimItReplaces(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const (
		pin     = "go.mod pins github.com/jackc/pgx/v4 at v4.18.1; the pool is built with pgxpool.Connect and the code imports pgx/v4 everywhere."
		upgrade = "go.mod now pins github.com/jackc/pgx/v5 at v5.6.0; the pool is built with pgxpool.New and every import moved to pgx/v5."
		restate = "go.mod pins github.com/jackc/pgx/v5 at v5.6.0; the pool is built with pgxpool.New and the code imports pgx/v5 everywhere."
	)
	if s := mergeScore(tokenizeContent(pin), tokenizeContent(upgrade)); s == 0 {
		t.Fatalf("fixture no longer clears the fold bar (mergeScore 0); the test would assert nothing")
	}

	pinID, _, _, err := s.Upsert(ctx, testProject, "dependency", pin, "mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert (pin): %v", err)
	}
	upID, dupOf, _, err := s.Upsert(ctx, testProject, "dependency", upgrade, "mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert (upgrade): %v", err)
	}
	if dupOf != "" {
		t.Errorf("the upgrade was folded as a duplicate of %q; a replacement is a distinct claim", dupOf)
	}
	var links int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memory_links WHERE relation = 'duplicate' AND source_id = ? AND target_id = ?`,
		upID, pinID).Scan(&links); err != nil {
		t.Fatalf("count links: %v", err)
	}
	if links != 0 {
		t.Errorf("found %d 'duplicate' link(s) from the upgrade to the pin it replaces", links)
	}
	var importance float32
	var access int
	if err := s.db.QueryRowContext(ctx,
		`SELECT importance, access_count FROM memories WHERE id = ?`, pinID).Scan(&importance, &access); err != nil {
		t.Fatalf("read pin: %v", err)
	}
	if importance != 0.7 || access != 0 {
		t.Errorf("the stale pin was strengthened by its replacement: importance %.2f (want 0.70), access_count %d (want 0)", importance, access)
	}

	// Control: restating the CURRENT value is a second wording and still folds.
	_, dupOf, _, err = s.Upsert(ctx, testProject, "dependency", restate, "mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert (restatement): %v", err)
	}
	if dupOf != upID {
		t.Errorf("a restatement of the upgrade folded into %q, want the upgrade %q", dupOf, upID)
	}
}

// The cross-category probe folds at a higher bar, and a replacement clears it as
// easily: one number swapped in a ten-word sentence is Jaccard 0.82. Saved under
// another category it reached the cross-category fold, so the guard is asked
// there too. The control is the same sentence restated, which still folds.
func TestUpsertCrossCategoryDoesNotFoldAReplacement(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const (
		old     = "the metrics exporter listens on port 9100 behind the cluster ingress"
		changed = "the metrics exporter listens on port 9200 behind the cluster ingress"
		restate = "The metrics exporter listens on port 9100, behind the cluster ingress."
	)
	if j := jaccard(tokenizeContent(old), tokenizeContent(changed)); j < upsertCrossCategoryThreshold {
		t.Fatalf("fixture Jaccard %.2f is under the cross-category bar; the test would assert nothing", j)
	}

	oldID, _, _, err := s.Upsert(ctx, testProject, "fact", old, "mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert (old): %v", err)
	}
	if _, dupOf, _, err := s.Upsert(ctx, testProject, "gotcha", changed, "mcp", 0.7, nil); err != nil {
		t.Fatalf("Upsert (changed): %v", err)
	} else if dupOf != "" {
		t.Errorf("a changed port was folded across categories into %q", dupOf)
	}
	var importance float32
	if err := s.db.QueryRowContext(ctx, `SELECT importance FROM memories WHERE id = ?`, oldID).Scan(&importance); err != nil {
		t.Fatalf("read old: %v", err)
	}
	if importance != 0.7 {
		t.Errorf("the old port was strengthened by its replacement: importance %.2f, want 0.70", importance)
	}

	if _, dupOf, _, err := s.Upsert(ctx, testProject, "pattern", restate, "mcp", 0.7, nil); err != nil {
		t.Fatalf("Upsert (restatement): %v", err)
	} else if dupOf != oldID {
		t.Errorf("a cross-category restatement folded into %q, want %q", dupOf, oldID)
	}
}
