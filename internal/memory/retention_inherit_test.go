package memory

import (
	"context"
	"testing"
	"time"
)

// ageSessionRow puts a session row into the shape both issues reproduce: a save
// old enough that the expiry the save derived is already in the past, with the
// row's last write of the same vintage.
//
// last_accessed is left NULL, which is what a real store holds: nothing in
// production writes it (Store.Touch has no caller), and prune's activity term
// PREFERS it when it is there. A fixture that filled it in would measure the
// column the term reaches past rather than the one it reaches for.
func ageSessionRow(t *testing.T, s *Store, id string, ago time.Duration) {
	t.Helper()
	at := stamp(-ago)
	if _, err := s.db.ExecContext(context.Background(), `
		UPDATE memories
		SET created_at = ?, updated_at = ?, expires_at = ?
		WHERE id = ?`, at, at, stamp(-ago+time.Hour), id); err != nil {
		t.Fatalf("age %s by %s: %v", id, ago, err)
	}
}

// recordRead is the one stamp production never writes, stated so that a test can
// prove a fold leaves it alone: a NULL there cannot tell a fold that moved it
// from one that never did.
func recordRead(t *testing.T, s *Store, id, when string) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`UPDATE memories SET last_accessed = ? WHERE id = ?`, when, id); err != nil {
		t.Fatalf("record a read on %s at %s: %v", id, when, err)
	}
}

// wantFreshSessionExpiry asserts the row holds a session expiry derived from
// about now, which is the whole of what "this write extended its life" means.
func wantFreshSessionExpiry(t *testing.T, id string, got *string, from, to time.Time) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s has no expires_at: the tier's expiry is what a fold refreshes, and a row with none of it is one prune reads for the wrong reason", id)
	}
	exp, err := time.Parse("2006-01-02 15:04:05", *got)
	if err != nil {
		t.Fatalf("%s expires_at %q does not parse: %v", id, *got, err)
	}
	if exp.Before(from.Add(SessionTTL-time.Minute)) || exp.After(to.Add(SessionTTL+time.Minute)) {
		t.Errorf("%s expires_at = %v, want about %v after the write", id, exp, SessionTTL)
	}
}

func sameStrPtr(a, b *string) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

// rowExists is GetByIDs answered as a question. GetByIDs returns an empty slice
// and no error for an id the store does not hold, so "no error" is not "still
// there" and a test that reads it that way asserts nothing.
func rowExists(t *testing.T, s *Store, id string) bool {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM memories WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", id, err)
	}
	return n == 1
}

// TestAFoldRefreshesASessionRowsExpiry is issue #772. The strengthen UPDATE
// writes importance and the validity and provenance fields and nothing else, and
// raiseRetentionTx returns early whenever the fold's tier is not a change — so a
// session→session fold, which is what "the same fact restated" looks like, left
// the expiry its own original save derived. A row that was about to expire
// therefore kept an expiry already in the past, and `ghost prune --apply`
// removed a memory that had been reinforced seconds earlier.
//
// Both fold paths are covered because they are two statements in two places and
// either one alone leaves the defect half-fixed. Neither may move updated_at or
// last_accessed: those carry the decay recency and the --skip-unchanged
// fingerprint, and the tier's life is not what they measure.
func TestAFoldRefreshesASessionRowsExpiry(t *testing.T) {
	for _, tc := range []struct {
		name     string
		saved    string
		restated string
		opts     UpsertOptions
	}{
		{
			name:     "ordinary fold",
			saved:    "a session fact this conversation keeps restating",
			restated: "a session fact this conversation keeps restating, at some length",
			opts:     UpsertOptions{Retention: RetentionSession},
		},
		{
			// FoldOnly collapses only what is equal after normalization, so its
			// restatement differs in case and spacing rather than in wording.
			name:     "fold-only fold",
			saved:    "a session fact _global already knows",
			restated: "A  Session Fact _Global Already Knows ",
			opts:     UpsertOptions{Retention: RetentionSession, FoldOnly: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			from := time.Now().UTC()

			id, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", tc.saved, "mcp", 0.6, nil,
				UpsertOptions{Retention: RetentionSession})
			if err != nil {
				t.Fatalf("save the session fact: %v", err)
			}
			ageSessionRow(t, s, id, 8*24*time.Hour)
			recordRead(t, s, id, stamp(-8*24*time.Hour))
			was := getOne(t, s, id)

			_, duplicateOf, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
				tc.restated, "mcp", 0.6, nil, tc.opts)
			if err != nil {
				t.Fatalf("fold the restatement: %v", err)
			}
			if duplicateOf != id {
				t.Fatalf("the restatement folded into %q, want %q — the fixture has to exercise a fold", duplicateOf, id)
			}

			got := getOne(t, s, id)
			if got.Retention != RetentionSession {
				t.Fatalf("surviving row retention = %q, want %q", got.Retention, RetentionSession)
			}
			wantFreshSessionExpiry(t, id, got.ExpiresAt, from, time.Now().UTC())

			if got.UpdatedAt != was.UpdatedAt {
				t.Errorf("updated_at moved from %q to %q: a fold is not a re-save, and that stamp is the decay's recency and --skip-unchanged's change proxy",
					was.UpdatedAt, got.UpdatedAt)
			}
			if !sameStrPtr(got.LastAccessed, was.LastAccessed) {
				t.Errorf("last_accessed moved from %v to %v: a fold is not a read, and nothing in production writes this column",
					was.LastAccessed, got.LastAccessed)
			}
			if got.AccessCount <= was.AccessCount {
				t.Errorf("access_count = %d, want more than %d — the fixture has to be exercising a strengthen",
					got.AccessCount, was.AccessCount)
			}

			// The outcome the refresh exists for: the row is still there after the
			// prune that would otherwise have taken it.
			report, err := s.PruneSessionMemories(ctx, PruneOptions{Apply: true})
			if err != nil {
				t.Fatalf("PruneSessionMemories: %v", err)
			}
			for _, gone := range report.RemovedIDs {
				if gone == id {
					t.Fatalf("prune removed %s minutes after it was folded into: the fold left the row's expiry in the past", id)
				}
			}
		})
	}
}

// TestAFoldTouchesTheExpiryOnlyWhileTheRowIsStillSession pins the CONDITION
// rather than the refresh. The expiry belongs to the tier, so it is refreshed
// only while the row's RESULTING tier is session — and a durable row is never
// handed one, in either direction. Each case gets its own store: a third
// near-identical row in the corpus is a row the dedup probe might choose instead
// of the one under test, which is a different writer from the one here.
func TestAFoldTouchesTheExpiryOnlyWhileTheRowIsStillSession(t *testing.T) {
	t.Run("a fold that leaves the session tier drops the expiry", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()

		promoted, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
			"a session fact that turns out to be worth keeping", "mcp", 0.6, nil,
			UpsertOptions{Retention: RetentionSession})
		if err != nil {
			t.Fatalf("save the session fact: %v", err)
		}
		ageSessionRow(t, s, promoted, 8*24*time.Hour)

		if _, dup, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
			"a session fact that turns out to be worth keeping well past this chat", "mcp", 0.6, nil,
			UpsertOptions{Retention: RetentionProject}); err != nil {
			t.Fatalf("fold as project: %v", err)
		} else if dup != promoted {
			t.Fatalf("the project restatement folded into %q, want %q", dup, promoted)
		}

		raised := getOne(t, s, promoted)
		if raised.Retention != RetentionProject {
			t.Errorf("retention = %q, want %q", raised.Retention, RetentionProject)
		}
		if raised.ExpiresAt != nil {
			t.Errorf("expires_at = %q on a row that is no longer a session memory: a durable row carrying an expiry is a claim about when the user stops wanting it that nobody made",
				*raised.ExpiresAt)
		}
	})

	// The direction a "refresh on every fold" implementation gets wrong in the
	// damaging direction: a session save folded into a durable row must not put
	// an expiry on it, because prune would one day act on that claim.
	t.Run("a session fold into a durable row does not put an expiry on it", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()

		durable, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
			"a durable fact somebody will restate as a passing detail", "mcp", 0.6, nil,
			UpsertOptions{Retention: RetentionProject})
		if err != nil {
			t.Fatalf("save the durable fact: %v", err)
		}
		if _, dup, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
			"a durable fact somebody will restate as a passing detail of today", "mcp", 0.6, nil,
			UpsertOptions{Retention: RetentionSession}); err != nil {
			t.Fatalf("fold as session: %v", err)
		} else if dup != durable {
			t.Fatalf("the session restatement folded into %q, want %q", dup, durable)
		}

		kept := getOne(t, s, durable)
		if kept.Retention != RetentionProject {
			t.Errorf("retention = %q, want %q — a fold may raise a tier, never lower it", kept.Retention, RetentionProject)
		}
		if kept.ExpiresAt != nil {
			t.Errorf("expires_at = %q: a session save put an expiry on a durable memory", *kept.ExpiresAt)
		}
	})
}

// TestAPinnedSessionRowIsRefreshedAndStillSpared is the pinned row, and its two
// halves are different facts. The refresh is not gated on the pin — the tier's
// life is a property of the tier, and a pin says "do not consolidate this" rather
// than "this is old" — and the prune is what honours the pin. The unpinned control
// is otherwise identical and IS removed, at the same instant, so a predicate that
// spared everything would not pass here.
func TestAPinnedSessionRowIsRefreshedAndStillSpared(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	from := time.Now().UTC()

	type row struct{ id, saved, restated string }
	rows := []row{
		{id: "", saved: "a pinned session fact the agent keeps restating", restated: "a pinned session fact the agent keeps restating, at length"},
		{id: "", saved: "an ordinary session fact the agent keeps restating", restated: "an ordinary session fact the agent keeps restating, at length"},
	}
	for i := range rows {
		id, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", rows[i].saved, "mcp", 0.6, nil,
			UpsertOptions{Retention: RetentionSession})
		if err != nil {
			t.Fatalf("save %q: %v", rows[i].saved, err)
		}
		rows[i].id = id
		ageSessionRow(t, s, id, 8*24*time.Hour)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE memories SET pinned = 1 WHERE id = ?`, rows[0].id); err != nil {
		t.Fatalf("pin %s: %v", rows[0].id, err)
	}

	for _, r := range rows {
		if _, dup, _, err := s.UpsertWithOptions(ctx, testProject, "fact", r.restated, "mcp", 0.6, nil,
			UpsertOptions{Retention: RetentionSession}); err != nil {
			t.Fatalf("fold the restatement into %s: %v", r.id, err)
		} else if dup != r.id {
			t.Fatalf("the restatement folded into %q, want %q", dup, r.id)
		}
		got := getOne(t, s, r.id)
		if !got.Pinned && r.id == rows[0].id {
			t.Fatalf("%s is no longer pinned; the fixture is not exercising the pin", r.id)
		}
		if got.Retention != RetentionSession {
			t.Fatalf("%s retention = %q, want %q", r.id, got.Retention, RetentionSession)
		}
		// The pin is not a reason to stop refreshing: the two rows are the same
		// tier and the same age, and a fold extends the life of both.
		wantFreshSessionExpiry(t, r.id, got.ExpiresAt, from, time.Now().UTC())
	}

	// An hour past the refreshed expiry, so the tier's own gate no longer spares
	// either row and the pin is the only thing left that can.
	now := time.Now().UTC().Add(SessionTTL + time.Hour)
	report, err := s.PruneSessionMemories(ctx, PruneOptions{Apply: true, Grace: time.Minute, Now: now})
	if err != nil {
		t.Fatalf("PruneSessionMemories: %v", err)
	}
	removed := map[string]bool{}
	for _, id := range report.RemovedIDs {
		removed[id] = true
	}
	if !removed[rows[1].id] {
		t.Errorf("the unpinned row of the same shape survived: the prune is not reaching the session tier at all, so the pinned row below proves nothing")
	}
	if removed[rows[0].id] {
		t.Errorf("prune removed the pinned %s: a pin is an explicit user override, and the session promise was made for rows without one", rows[0].id)
	}
	if !rowExists(t, s, rows[0].id) {
		t.Errorf("the pinned row is not in the store after the prune")
	}
}

// TestPruneMeasuresTheGraceFromAnExpiryAFoldRefreshed is the second half of
// #772, and it is a separate change from the refresh. The refresh moves a column;
// the grace is measured from the row's newest stamp among the ones a write moves,
// and a fold moved none of them until now — so prune.go's own claim that the term
// is "the row's last WRITE" was false for the one write that extends a session
// row's life, and a folded row became prunable the instant its fresh expiry
// arrived, however recently it had been reinforced.
//
// Both rows carry no recorded read, because that is the shape a real store has
// and the shape the term reaches for. The control is a row of the identical aged
// shape that was NOT folded: it is removed at the same instant, so what spares
// the folded one is the grace measured from the write that refreshed its expiry,
// not a prune that found nothing.
func TestPruneMeasuresTheGraceFromAnExpiryAFoldRefreshed(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const saved = "a session fact that keeps coming back after months of silence"
	folded, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", saved, "mcp", 0.6, nil,
		UpsertOptions{Retention: RetentionSession})
	if err != nil {
		t.Fatalf("save the session fact: %v", err)
	}
	ageSessionRow(t, s, folded, 30*24*time.Hour)

	control, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"a session fact that was written once and never mentioned again", "mcp", 0.6, nil,
		UpsertOptions{Retention: RetentionSession})
	if err != nil {
		t.Fatalf("save the control: %v", err)
	}
	ageSessionRow(t, s, control, 30*24*time.Hour)

	if _, dup, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"a session fact that keeps coming back after months of silence, restated", "mcp", 0.6, nil,
		UpsertOptions{Retention: RetentionSession}); err != nil {
		t.Fatalf("fold the restatement: %v", err)
	} else if dup != folded {
		t.Fatalf("the restatement folded into %q, want %q", dup, folded)
	}

	// An hour past the refreshed expiry: the row IS expired, so the activity term
	// is the only thing that can spare it, and the control's expiry was thirty
	// days past before the run started.
	now := time.Now().UTC().Add(SessionTTL + time.Hour)
	report, err := s.PruneSessionMemories(ctx, PruneOptions{Apply: true, Now: now})
	if err != nil {
		t.Fatalf("PruneSessionMemories: %v", err)
	}
	removed := map[string]bool{}
	for _, id := range report.RemovedIDs {
		removed[id] = true
	}
	if !removed[control] {
		t.Errorf("the never-restated row survived: nothing is prunable at this instant, so the row below proves nothing")
	}
	if removed[folded] {
		t.Errorf("prune removed %s an hour after it was folded into: the grace was measured from a write stamp the fold did not move, so a reinforced row was deleted the moment its fresh expiry arrived", folded)
	}
	if !rowExists(t, s, folded) {
		t.Errorf("the folded row is gone from the store")
	}
}

// TestAConsolidationSuccessorInheritsTheLongestTierOfItsSources is issue #773.
// ReplaceNonManual's reuse branches update a row in place, so they keep the tier
// the row has; the FRESH INSERT — taken for a real merge or a rewrite — named
// neither retention nor expires_at, so the successor took the column default.
// Reflection never states a tier on an emission, so a merge of two session
// memories came out project with no expiry: a fact the user scoped to one
// conversation was silently promoted to the corpus and could never be pruned.
//
// The rule is raiseRetentionTx's, over the sources rather than over a fold's two
// tiers: the longest life any source names wins, and the emission's own silence
// is never folded in as a raise to the default — that is the session+session case
// below, and it is the case the whole issue is about. The last two are the
// persistent-adjacent ones: a keep-forever source is not a consolidation input
// (retentionExemptSQL excludes it from every set this function builds) but it CAN
// be named in ReplacesIDs, and the successor must not come out shorter-lived.
func TestAConsolidationSuccessorInheritsTheLongestTierOfItsSources(t *testing.T) {
	from := time.Now().UTC()
	for _, tc := range []struct {
		name  string
		tiers [2]string
		want  string
	}{
		{name: "session and session", tiers: [2]string{RetentionSession, RetentionSession}, want: RetentionSession},
		{name: "session and project", tiers: [2]string{RetentionSession, RetentionProject}, want: RetentionProject},
		{name: "project and project", tiers: [2]string{RetentionProject, RetentionProject}, want: RetentionProject},
		{name: "session and keep-forever", tiers: [2]string{RetentionSession, RetentionPersistent}, want: RetentionPersistent},
		{name: "project and keep-forever", tiers: [2]string{RetentionProject, RetentionPersistent}, want: RetentionPersistent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()

			// Unrelated sentences, deliberately: a near-duplicate would FOLD into
			// the first, and a fold's tier is a different writer from this one.
			bodies := [2]string{
				"the staging cluster answers on port 8443",
				"deploys go out through the bastion tunnel on port 2222",
			}
			var sources []string
			for i, body := range bodies {
				id, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", body, "mcp", 0.5, nil,
					UpsertOptions{Retention: tc.tiers[i]})
				if err != nil {
					t.Fatalf("save source %d as %s: %v", i, tc.tiers[i], err)
				}
				sources = append(sources, id)
			}

			const merged = "the staging cluster answers health checks on port 8443 through the bastion"
			if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
				Category: "fact", Content: merged, Importance: 0.5,
				ReplacesIDs: sources,
			}}, ""); err != nil {
				t.Fatalf("ReplaceNonManual: %v", err)
			}

			successor := onlyMemoryWithContent(t, s, merged)
			got := getOne(t, s, successor)
			if got.Retention != tc.want {
				t.Errorf("successor %s retention = %q, want %q — a merge inherits the longest life its sources named and never lowers it", successor, got.Retention, tc.want)
			}
			if tc.want == RetentionSession {
				wantFreshSessionExpiry(t, successor, got.ExpiresAt, from, time.Now().UTC())
			} else if got.ExpiresAt != nil {
				t.Errorf("successor %s expires_at = %q on a %s row: only a session row is scheduled for expiry", successor, *got.ExpiresAt, got.Retention)
			}
			if rowExists(t, s, sources[0]) {
				t.Errorf("source %s survived a merge that named it", sources[0])
			}
			// A keep-forever source is in no replaceable set, so inheriting its tier
			// must not have taken the source with it.
			if tc.tiers[1] == RetentionPersistent && !rowExists(t, s, sources[1]) {
				t.Errorf("the keep-forever source %s was deleted: it is exempt from every set this function builds", sources[1])
			}
		})
	}
}

// TestAConsolidationEmissionWithNoSourcesArrivesAsTheTierItStated pins the
// fallback, so the inheritance above cannot quietly become "an emission with no
// sources is a session row": there is nothing to inherit from, and the emission's
// own value — or the default, which is what reflection always sends — is what the
// row gets.
func TestAConsolidationEmissionWithNoSourcesArrivesAsTheTierItStated(t *testing.T) {
	from := time.Now().UTC()
	for _, tc := range []struct {
		name  string
		tier  string
		want  string
		hasEx bool
	}{
		{name: "states nothing", tier: "", want: RetentionProject, hasEx: false},
		{name: "states session", tier: RetentionSession, want: RetentionSession, hasEx: true},
		{name: "states keep-forever", tier: RetentionPersistent, want: RetentionPersistent, hasEx: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			content := "a consolidated note that stands on its own: " + tc.name

			if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
				Category: "fact", Content: content, Importance: 0.5, Retention: tc.tier,
			}}, ""); err != nil {
				t.Fatalf("ReplaceNonManual: %v", err)
			}
			id := onlyMemoryWithContent(t, s, content)
			got := getOne(t, s, id)
			if got.Retention != tc.want {
				t.Errorf("retention = %q, want %q", got.Retention, tc.want)
			}
			if tc.hasEx {
				wantFreshSessionExpiry(t, id, got.ExpiresAt, from, time.Now().UTC())
			} else if got.ExpiresAt != nil {
				t.Errorf("expires_at = %q on a row with no sources to inherit a session life from", *got.ExpiresAt)
			}
		})
	}
}

// TestAReflectionMergeLeavesItsSessionSuccessorPrunable is the two issues meeting.
// A merge of two session rows now arrives as a session row with a derived expiry,
// so the tier the user asked for still has a way to take it back; without it the
// promotion #773 fixes would have been permanent, which is the whole cost of it.
func TestAReflectionMergeLeavesItsSessionSuccessorPrunable(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	bodies := [2]string{
		"the review bot re-runs on pull requests that touch the migrator",
		"the migrator refuses a down migration with no recorded reason",
	}
	var sources []string
	for _, body := range bodies {
		id, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", body, "mcp", 0.5, nil,
			UpsertOptions{Retention: RetentionSession})
		if err != nil {
			t.Fatalf("save the session source: %v", err)
		}
		sources = append(sources, id)
	}

	const merged = "the migrator is the one path a review has to re-run, and it refuses a down without a reason"
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category: "fact", Content: merged, Importance: 0.5, ReplacesIDs: sources,
	}}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	successor := onlyMemoryWithContent(t, s, merged)
	if got := getOne(t, s, successor); got.Retention != RetentionSession {
		t.Fatalf("the merged successor is %s; this test is about the prunable one", got.Retention)
	}

	// Aged the way a conversation that has moved on ages, and then pruned. A
	// project-tier successor takes neither branch, which is the whole of what the
	// merge used to do to it.
	aged := stamp(-30 * 24 * time.Hour)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE memories SET created_at = ?, updated_at = ?, expires_at = ? WHERE id = ?`,
		aged, aged, aged, successor); err != nil {
		t.Fatalf("age the successor: %v", err)
	}

	report, err := s.PruneSessionMemories(ctx, PruneOptions{Apply: true})
	if err != nil {
		t.Fatalf("PruneSessionMemories: %v", err)
	}
	if len(report.RemovedIDs) != 1 || report.RemovedIDs[0] != successor {
		t.Errorf("prune removed %v, want exactly the merged session successor %s: a merge of session rows was promoted to the tier that never expires",
			report.RemovedIDs, successor)
	}
}
