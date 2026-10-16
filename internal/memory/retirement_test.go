package memory

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// The retired-dependency storyline's pair, word for word: a dependency note, and
// the note saying that dependency was removed with nothing replacing it.
const (
	retireOriginal   = "Order emails go out through SendGrid: internal/mail wraps the sendgrid-go v3 client and reads SENDGRID_API_KEY at boot."
	retireNote       = "SendGrid was removed from parcel-notify: internal/mail is deleted, nothing reads SENDGRID_API_KEY, and order emails are no longer sent at all."
	retireNoteAgain  = "SendGrid was removed from parcel-notify: internal/mail is deleted and nothing reads SENDGRID_API_KEY; order emails are no longer sent at all."
	retireOrigReword = "Order emails go out through SendGrid; internal/mail wraps the sendgrid-go v3 client and reads SENDGRID_API_KEY at boot time."
)

// countDuplicateLinks counts the 'duplicate' edges from one row to another.
func countDuplicateLinks(t *testing.T, s *Store, from, to string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM memory_links WHERE relation = 'duplicate' AND source_id = ? AND target_id = ?`,
		from, to).Scan(&n); err != nil {
		t.Fatalf("count links: %v", err)
	}
	return n
}

// TestUpsertDoesNotFoldARetirementIntoTheClaimItRetires: a note saying a
// dependency was removed holds no value token the dependency note lacks, so the
// value guard let the same-category lookup fold it: the retirement was linked to
// the retired claim as a 'duplicate' and the retired claim was strengthened
// (importance 0.70 -> 0.84, access_count 0 -> 1). The controls are true
// restatements, of the retirement and of the original, which must still fold,
// so a guard that refused every fold cannot pass.
func TestUpsertDoesNotFoldARetirementIntoTheClaimItRetires(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if sc := mergeScore(tokenizeContent(retireOriginal), tokenizeContent(retireNote)); sc == 0 {
		t.Fatalf("fixture no longer clears the fold bar (mergeScore 0); the test would assert nothing")
	}
	if statesDifferentValue(retireOriginal, retireNote) {
		t.Fatalf("fixture is refused by the value guard already; the test would not reach the retirement rule")
	}

	origID, _, _, err := s.Upsert(ctx, testProject, "dependency", retireOriginal, "mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert (original): %v", err)
	}
	retID, dupOf, _, err := s.Upsert(ctx, testProject, "dependency", retireNote, "mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert (retirement): %v", err)
	}
	if dupOf != "" {
		t.Errorf("the retirement was folded as a duplicate of %q; a retirement is a distinct claim", dupOf)
	}
	if n := countDuplicateLinks(t, s, retID, origID); n != 0 {
		t.Errorf("found %d 'duplicate' link(s) from the retirement to the claim it retires", n)
	}
	var importance float32
	var access int
	if err := s.db.QueryRowContext(ctx,
		`SELECT importance, access_count FROM memories WHERE id = ?`, origID).Scan(&importance, &access); err != nil {
		t.Fatalf("read original: %v", err)
	}
	if importance != 0.7 || access != 0 {
		t.Errorf("the retired claim was strengthened by its retirement: importance %.2f (want 0.70), access_count %d (want 0)", importance, access)
	}

	// Control: a second wording of the retirement folds into the retirement.
	if _, dupOf, _, err := s.Upsert(ctx, testProject, "dependency", retireNoteAgain, "mcp", 0.7, nil); err != nil {
		t.Fatalf("Upsert (retirement restated): %v", err)
	} else if dupOf != retID {
		t.Errorf("a restatement of the retirement folded into %q, want the retirement %q", dupOf, retID)
	}
	// Control: a second wording of the original is not a retirement and folds.
	if _, dupOf, _, err := s.Upsert(ctx, testProject, "dependency", retireOrigReword, "mcp", 0.7, nil); err != nil {
		t.Fatalf("Upsert (original restated): %v", err)
	} else if dupOf != origID {
		t.Errorf("a restatement of the original folded into %q, want the original %q", dupOf, origID)
	}
}

// TestUpsertCrossCategoryDoesNotFoldARetirement: the cross-category lookup asks
// the same question. One retirement word added to a ten-word note clears its
// Jaccard bar, so saved under another category the retirement reached that fold.
// The control is the original restated under a third category, which folds.
func TestUpsertCrossCategoryDoesNotFoldARetirement(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const (
		old     = "the nightly ledgerExport job writes the reconciliation CSV to the finance bucket"
		retired = "the nightly ledgerExport job that writes the reconciliation CSV to the finance bucket is deprecated"
		restate = "The nightly ledgerExport job writes the reconciliation CSV to the finance bucket."
	)
	if j := jaccard(tokenizeContent(old), tokenizeContent(retired)); j < upsertCrossCategoryThreshold {
		t.Fatalf("fixture Jaccard %.2f is under the cross-category bar; the test would assert nothing", j)
	}
	if statesDifferentValue(old, retired) {
		t.Fatalf("fixture is refused by the value guard already; the test would not reach the retirement rule")
	}

	oldID, _, _, err := s.Upsert(ctx, testProject, "fact", old, "mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert (old): %v", err)
	}
	retID, dupOf, _, err := s.Upsert(ctx, testProject, "gotcha", retired, "mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert (retired): %v", err)
	}
	if dupOf != "" {
		t.Errorf("a retirement was folded across categories into %q", dupOf)
	}
	if n := countDuplicateLinks(t, s, retID, oldID); n != 0 {
		t.Errorf("found %d 'duplicate' link(s) from the retirement to the claim it retires", n)
	}
	var importance float32
	if err := s.db.QueryRowContext(ctx, `SELECT importance FROM memories WHERE id = ?`, oldID).Scan(&importance); err != nil {
		t.Fatalf("read old: %v", err)
	}
	if importance != 0.7 {
		t.Errorf("the retired claim was strengthened by its retirement: importance %.2f, want 0.70", importance)
	}

	if _, dupOf, _, err := s.Upsert(ctx, testProject, "pattern", restate, "mcp", 0.7, nil); err != nil {
		t.Fatalf("Upsert (restatement): %v", err)
	} else if dupOf != oldID {
		t.Errorf("a cross-category restatement folded into %q, want %q", dupOf, oldID)
	}
}

// TestRetires pins the retirement rule. Every row shares a value token with the
// older text (checked first), so a must-not row is refused by the rule under
// test and not by a missing anchor, except the rows that pin the anchor itself.
func TestRetires(t *testing.T) {
	cases := []struct {
		name, older, newer string
		want               bool
		noAnchor           bool // the row pins the anchor: no shared value token
	}{
		// Must trigger.
		{"storyline retirement", retireOriginal, retireNote, true, false},
		{"decommissioned host", "Nightly backups run on backup-01.internal and push to the B2 bucket.",
			"backup-01.internal was decommissioned last week.", true, false},
		{"deprecated flag", "The --legacy-auth flag enables the cookie login in internal/auth.",
			"--legacy-auth is deprecated; internal/auth only serves SSO.", true, false},
		{"no longer read", "The worker reads REDIS_URL at boot to reach the queue.",
			"Nothing reads REDIS_URL anymore and the worker no longer starts a queue client.", true, false},
		{"been dropped", "The orders table has a legacy_status column read by the export job.",
			"The legacy_status column has been dropped from the orders table.", true, false},
		{"is gone", "Metrics are scraped from the StatsD sidecar on port 8125.",
			"The StatsD sidecar is gone.", true, false},
		{"not used anymore", "The cache layer is memcached via gomemcache.Client.",
			"gomemcache.Client is not used anymore.", true, false},
		{"retired endpoint", "The /v1/orders endpoint serves the mobile app.",
			"/v1/orders was retired in the 3.0 release.", true, false},

		// Must not trigger.
		{"plan with once", "The LEGACY_AUTH flag gates the old session login path in internal/auth.",
			"The LEGACY_AUTH flag is no longer needed once SSO ships.", false, false},
		{"plan with will", "SendGrid sends order emails via SENDGRID_API_KEY.",
			"SendGrid will be removed next quarter; SENDGRID_API_KEY will no longer be read.", false, false},
		{"change log in another sentence", "The ingestHandler logs every request at info level.",
			"Fixed the ingestHandler startup. The duplicate log line was removed.", false, false},
		{"bare removed is a change-log verb", "The ingestHandler logs every request at info level.",
			"In ingestHandler, removed the duplicate log line.", false, false},
		{"both say no longer", "The worker no longer polls; it reads REDIS_URL and blocks on BRPOP.",
			"The worker no longer reads REDIS_URL; it subscribes through NATS_URL.", false, false},
		{"older carries the phrase", "SendGrid was removed from the staging stack; SENDGRID_API_KEY stays in production.",
			"SendGrid was removed from production too: nothing reads SENDGRID_API_KEY.", false, false},
		{"negated", "The SENDGRID_API_KEY variable is read at boot.",
			"SENDGRID_API_KEY is not deprecated.", false, false},
		{"negated with a curly apostrophe", "The SENDGRID_API_KEY variable is read at boot.",
			"SENDGRID_API_KEY isn’t deprecated.", false, false},
		{"never removed", "The SENDGRID_API_KEY variable is read at boot.",
			"SENDGRID_API_KEY was never removed.", false, false},
		{"present-tense behaviour", "Jobs live in the river_job table until they finish.",
			"A finished job is removed from river_job on success.", false, false},
		{"conditional behaviour", "Session keys live in Redis under SESSION_PREFIX.",
			"A session key under SESSION_PREFIX was deleted when the TTL expired.", false, false},
		{"restatement", retireOriginal, retireOrigReword, false, false},
		// The anchor: no shared value token, no retirement. The plain-word limit.
		{"plain words only", "we send order emails through the mail provider",
			"the mail provider was removed and order emails are no longer sent", false, true},
		{"name at a sentence start", "Caching uses Redis for sessions.",
			"Redis was removed.", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if shares := sharesValue(valueTokens(tc.older), valueTokens(tc.newer)); shares == tc.noAnchor {
				t.Fatalf("fixture: shared value token = %v, want %v (%v / %v)", shares, !tc.noAnchor, valueTokens(tc.older), valueTokens(tc.newer))
			}
			if got := retires(tc.newer, tc.older); got != tc.want {
				t.Errorf("retires(%q, %q) = %v, want %v", tc.newer, tc.older, got, tc.want)
			}
			// Never the other way round: the retired claim retires nothing.
			if tc.want && retires(tc.older, tc.newer) {
				t.Errorf("the retired claim reads as retiring its retirement")
			}
		})
	}
}

// TestLexicalReplacementsPairsARetirement: on the read side a retirement is a
// replacement pair, newer retirement over older claim, under every rule a value
// replacement is held to. It is directional, and a plan is no retirement.
func TestLexicalReplacementsPairsARetirement(t *testing.T) {
	now := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	const (
		oldDay = "2026-09-01 10:00:00"
		newDay = "2026-09-10 10:00:00"
	)
	cases := []struct {
		name                  string
		olderStamp, olderText string
		newerStamp, newerText string
		want                  map[string][]string
	}{
		{"retirement after the claim", oldDay, retireOriginal, newDay, retireNote,
			map[string][]string{"old": {"new"}}},
		{"retirement before the claim retires nothing newer", newDay, retireOriginal, oldDay, retireNote, nil},
		{"same second", oldDay, retireOriginal, oldDay, retireNote, nil},
		{"plan", oldDay, "The LEGACY_AUTH flag gates the old session login path in internal/auth.",
			newDay, "The LEGACY_AUTH flag is no longer needed once SSO ships; internal/auth keeps the old session login path until then.", nil},
		{"under the similarity bar", oldDay, "CI runs on GitHub Actions and publishes to GHCR.",
			newDay, "The nightly Jenkins job was removed; nothing uploads to GHCR from it.", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows := []Candidate{lexRow("old", tc.olderStamp, tc.olderText), lexRow("new", tc.newerStamp, tc.newerText)}
			if got := lexicalReplacements(rows, nil, now); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("lexicalReplacements = %v, want %v", got, tc.want)
			}
		})
	}

	// The similarity row is refused by the similarity bar, not by the rule.
	if !retires("The nightly Jenkins job was removed; nothing uploads to GHCR from it.", "CI runs on GitHub Actions and publishes to GHCR.") {
		t.Fatalf("fixture: the similarity row is not a retirement, so it pins nothing about the bar")
	}

	// The rules a value replacement is held to hold here too.
	rows := func() []Candidate {
		return []Candidate{lexRow("old", oldDay, retireOriginal), lexRow("new", newDay, retireNote)}
	}
	r := rows()
	r[0].Scope = map[string]string{"env": "staging"}
	if got := lexicalReplacements(r, nil, now); got != nil {
		t.Errorf("scopes differ: paired %v", got)
	}
	r = rows()
	resolved := newDay
	r[0].ResolvedAt = &resolved
	if got := lexicalReplacements(r, nil, now); got != nil {
		t.Errorf("resolved older row: paired %v", got)
	}
	r = rows()
	r[1].ProjectID = "q"
	if got := lexicalReplacements(r, nil, now); got != nil {
		t.Errorf("projects differ: paired %v", got)
	}
	if got := lexicalReplacements(rows(), []LinkEdge{{From: "new", To: "old", Relation: "supersedes"}}, now); got != nil {
		t.Errorf("explicit supersedes edge: paired %v", got)
	}
}
