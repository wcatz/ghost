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

// expiryText renders a nullable stamp for a failure message. %v on a *string
// prints an address, so a comparison that went wrong would report where two
// strings live rather than what they say.
func expiryText(v *string) string {
	if v == nil {
		return "NULL"
	}
	return *v
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

// TestAReusedRowInheritsTheLongestTierOfItsOtherSources is #773 through the REUSE
// path, and it is the same defect the fresh-INSERT fix removed arriving by the
// other door. The reuse branches update a row in place, so the reused row keeps
// its own tier — correct when it is the row the consolidator re-emitted and
// nothing else. It is not correct when the emission's ReplacesIDs names OTHER
// sources beside it: those rows are deleted into this one, and their tier is the
// life the merged knowledge then has.
//
// The repro: A saved as session, B saved as project, and one emission whose
// content byte-matches A with ReplacesIDs [A, B]. A is reused and stays session
// with a live expiry, B is deleted, and a `ghost prune` run a month later takes
// the project's knowledge with it. The successor must be the LONGEST life any
// source named, exactly as the fresh insert's is.
//
// The two cases that must NOT move are here too, because the fix is a raise and
// both are ways to write too much. A verbatim re-emission — reuseChangesNothing,
// the branch that writes nothing at all — is not a new assertion of the fact, so
// it must not extend a session row's life on every applied reflect; and a reuse
// that names no other source is the row restating itself, which is the case
// keeping the tier was always right for.
func TestAReusedRowInheritsTheLongestTierOfItsOtherSources(t *testing.T) {
	from := time.Now().UTC()
	for _, tc := range []struct {
		name  string
		tiers [2]string
		want  string
		hasEx bool
	}{
		{name: "session reused beside project", tiers: [2]string{RetentionSession, RetentionProject}, want: RetentionProject, hasEx: false},
		{name: "session reused beside keep-forever", tiers: [2]string{RetentionSession, RetentionPersistent}, want: RetentionPersistent, hasEx: false},
		{name: "project reused beside session", tiers: [2]string{RetentionProject, RetentionSession}, want: RetentionProject, hasEx: false},
		{name: "session reused beside session", tiers: [2]string{RetentionSession, RetentionSession}, want: RetentionSession, hasEx: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()

			// Byte-identical content for the reused row, because reuse is matched on
			// content alone; unrelated text for the other source, so the emission
			// cannot reuse it instead.
			const reused = "the staging cluster answers on port 8443"
			const other = "deploys go out through the bastion tunnel on port 2222"
			a, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", reused, "mcp", 0.5, nil,
				UpsertOptions{Retention: tc.tiers[0]})
			if err != nil {
				t.Fatalf("save the reused row as %s: %v", tc.tiers[0], err)
			}
			b, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", other, "mcp", 0.5, nil,
				UpsertOptions{Retention: tc.tiers[1]})
			if err != nil {
				t.Fatalf("save the other source as %s: %v", tc.tiers[1], err)
			}
			before := getOne(t, s, a)

			// Importance 0.5 with no tags and no scope, so the emission also matches
			// reuseChangesNothing — the branch that writes nothing. The tier raise is
			// not optional there, so the test runs against the strictest branch.
			if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
				Category: "fact", Content: reused, Importance: 0.5,
				ReplacesIDs: []string{a, b},
			}}, ""); err != nil {
				t.Fatalf("ReplaceNonManual: %v", err)
			}

			if !rowExists(t, s, a) {
				t.Fatalf("the reused row %s is gone; the fixture did not exercise a reuse", a)
			}
			// A keep-forever source is exempt from every set this replace builds, so
			// it is never deleted and the reused row does not absorb it. The raise
			// still applies — ReplacesIDs naming a row is the emission asserting it
			// stands for that row, and the rule does not consult the delete set —
			// which is asserted above and is why this case is here.
			if tc.tiers[1] == RetentionPersistent {
				if !rowExists(t, s, b) {
					t.Errorf("the keep-forever source %s was deleted: it is exempt from every set this replace builds", b)
				}
			} else if rowExists(t, s, b) {
				t.Errorf("the other source %s survived; the fixture is not exercising the merge this is about", b)
			}
			got := getOne(t, s, a)
			if got.Retention != tc.want {
				t.Errorf("reused row %s retention = %q, want %q — the row that absorbs %s inherits the longest life any source named",
					a, got.Retention, tc.want, b)
			}
			if tc.hasEx {
				wantFreshSessionExpiry(t, a, got.ExpiresAt, from, time.Now().UTC())
			} else if got.ExpiresAt != nil {
				t.Errorf("reused row %s expires_at = %q on a %s row: only a session row is scheduled for expiry", a, *got.ExpiresAt, got.Retention)
			}
			// A verbatim re-emission is not a re-save, so the raise must not move
			// the stamp reuseChangesNothing exists to leave alone.
			if got.UpdatedAt != before.UpdatedAt {
				t.Errorf("updated_at moved from %q to %q: a verbatim re-emission is not a write (#727), and this pass wrote no other column either",
					before.UpdatedAt, got.UpdatedAt)
			}
		})
	}
}

// TestAReusedSessionRowIsNotRefreshedByAVerbatimReEmission is the other half of
// the raise, and the reason it is a separate test: a session row that a reflect
// run re-emits byte-for-byte has not been re-asserted by anybody. Refreshing its
// expiry on every applied reflect would give a conversation-scoped memory an
// unbounded life, which is the opposite of what the tier is for — the fix raises
// a tier when the SOURCES demand one and never invents an extension. A FOLD is the
// contrast and already has its own test: a fold is a new assertion of the fact and
// does refresh (#772), so the two behaviours are deliberately different and this
// is the half that would otherwise go unstated.
func TestAReusedSessionRowIsNotRefreshedByAVerbatimReEmission(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	reEmitted := pruneRow(t, s, testProject, "a session fact a reflect run re-emits every pass", RetentionSession)
	agePruneRow(t, s, reEmitted, stamp(-30*24*time.Hour), stamp(-29*24*time.Hour))
	wasReEmitted := getOne(t, s, reEmitted)

	// The control, and it is a `manual` source for a reason the fixture needs
	// rather than for realism: consolidation DELETES every replaceable row the
	// emission does not account for, so a peer session row written here would be
	// gone before the prune ever saw it. `manual` is in the exclusion list, so this
	// row survives the replace and is a candidate on its own terms — an aged,
	// expired session row the prune must reach, which is what makes the row above
	// removed because of ITS expiry and nothing else.
	control := pruneRow(t, s, testProject, "a session fact the user wrote by hand", RetentionSession)
	if _, err := s.db.ExecContext(ctx, `UPDATE memories SET source = 'manual' WHERE id = ?`, control); err != nil {
		t.Fatalf("mark the control manual: %v", err)
	}
	agePruneRow(t, s, control, stamp(-30*24*time.Hour), stamp(-29*24*time.Hour))

	// The reflect run: re-emit the row byte-for-byte, naming only itself.
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category: "fact", Content: "a session fact a reflect run re-emits every pass", Importance: 0.6,
		ReplacesIDs: []string{reEmitted},
	}}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	if !rowExists(t, s, reEmitted) {
		t.Fatalf("the re-emitted row %s is gone; the fixture did not exercise a reuse", reEmitted)
	}
	if !rowExists(t, s, control) {
		t.Fatalf("the control %s was deleted by the replace; a `manual` row is exempt from it", control)
	}
	// Compared through the sameStrPtr helper, so a mutation that DROPS the expiry
	// reports as a finding rather than as a nil dereference: "no expiry" and "a
	// different expiry" are both the defect, and a test that panics on one of them
	// does not say which.
	if got := getOne(t, s, reEmitted); !sameStrPtr(got.ExpiresAt, wasReEmitted.ExpiresAt) {
		t.Errorf("the re-emitted row's expiry moved from %s to %s: a reflect run that re-states a row is not a new assertion of the fact, and refreshing it here would give a session memory an unbounded life",
			expiryText(wasReEmitted.ExpiresAt), expiryText(got.ExpiresAt))
	}

	// The outcome, which is the tier's own. Both rows are expired, aged and past
	// the grace, and both are removed: the re-emitted one is not spared, because a
	// reflect run is not something anybody re-asserted.
	report, err := s.PruneSessionMemories(ctx, PruneOptions{Apply: true})
	if err != nil {
		t.Fatalf("PruneSessionMemories: %v", err)
	}
	removed := map[string]bool{}
	for _, id := range report.RemovedIDs {
		removed[id] = true
	}
	if !removed[control] {
		t.Errorf("the control %s survived; nothing is prunable at this instant, so the row above proves nothing", control)
	}
	if !removed[reEmitted] {
		t.Errorf("prune spared %s: its expiry was refreshed by a re-emission, so a reflect run has become a way to keep a session memory forever", reEmitted)
	}
}

// TestAnEmissionWithNoOtherSourcesLeavesTheReusedRowAlone is the boundary the
// raise must not cross: a reuse that names nothing but the reused row is that row
// restating itself, and it has no claim on a longer life. This is the case the
// reuse path was always right about, and it is what a raise written as "fold the
// ReplacesIDs together" would break — with no other source there is nothing to
// fold, and a rule that fell back to the emission's silence would read that
// silence as the project default and quietly make every session memory durable.
func TestAnEmissionWithNoOtherSourcesLeavesTheReusedRowAlone(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	from := time.Now().UTC()

	reused, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"a session fact reflection keeps, without naming anything else", "mcp", 0.5, nil,
		UpsertOptions{Retention: RetentionSession})
	if err != nil {
		t.Fatalf("save the session row: %v", err)
	}
	// A second session row the emission does NOT account for, so the replace has a
	// real delete to perform and is not a no-op.
	dropped, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"a session fact the consolidator decided was stale", "mcp", 0.5, nil,
		UpsertOptions{Retention: RetentionSession})
	if err != nil {
		t.Fatalf("save the dropped row: %v", err)
	}
	was := getOne(t, s, reused)

	// ReplacesIDs names only the reused row. Reflection does this on every `keep`.
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category: "fact", Content: "a session fact reflection keeps, without naming anything else", Importance: 0.5,
		ReplacesIDs: []string{reused},
	}}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	if rowExists(t, s, dropped) {
		t.Errorf("the unaccounted row %s survived; the fixture is not exercising a replace", dropped)
	}
	got := getOne(t, s, reused)
	if got.Retention != RetentionSession {
		t.Errorf("retention = %q, want %q: an emission naming no other source has no claim on a longer life, and reading its silence as the project default would make every session memory durable",
			got.Retention, RetentionSession)
	}
	wantFreshSessionExpiry(t, reused, got.ExpiresAt, from, time.Now().UTC())
	if got.ExpiresAt != nil && *got.ExpiresAt != *was.ExpiresAt {
		t.Errorf("expires_at moved from %q to %q: nothing re-asserted this row", *was.ExpiresAt, *got.ExpiresAt)
	}
}

// TestPruneReportsTheGraceBasisSeparatelyFromActivity is the store half of the
// renderer fix, and it exists because the two columns are projected by one
// statement and a renderer cannot tell a projection that was never made from one
// it chose to ignore.
//
// The shape it pins: a session row saved and never written to since. Its expiry
// is the save's own value derived forward, so it is the NEWEST of the three
// stamps the grace can be measured from — which means the two fields are equal
// here, and a report that printed the grace basis under the name "last touched"
// would be naming an event at an instant nothing happened. A row edited after its
// save is the other direction: the write is later than the expiry, so the basis
// and the activity are again the same value but neither is the expiry, which is
// the case where printing only one of the two hides why the row is eligible now.
//
// The control is a project row, which is never a candidate at all: if it reached
// the list, the projection rather than this test's arithmetic would be wrong.
func TestPruneReportsTheGraceBasisSeparatelyFromActivity(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	untouched := pruneRow(t, s, testProject, "a session note nobody has written to since its save", RetentionSession)
	edited := pruneRow(t, s, testProject, "a session note edited after the expiry its save derived", RetentionSession)
	pruneFixture(t, s, "a durable note that is just as old", RetentionProject,
		stamp(-30*24*time.Hour), stamp(-29*24*time.Hour))
	// Thirty days ago with no recorded read, which is the shape a real store
	// holds and the shape the fallback is for: last_accessed stays NULL, the
	// write stamps go back to the save, and the expiry is that save's own
	// SessionTTL — a month past the write, not on it.
	for _, id := range []string{untouched, edited} {
		backdateWrite(t, s, id, stamp(-30*24*time.Hour))
		agePruneRow(t, s, id, stamp(-30*24*time.Hour+SessionTTL), "")
	}
	// The edit: later than the expiry, so the grace has to run from it.
	if _, err := s.db.ExecContext(ctx, `UPDATE memories SET updated_at = ? WHERE id = ?`,
		stamp(-20*24*time.Hour), edited); err != nil {
		t.Fatalf("edit %s: %v", edited, err)
	}

	report, err := s.PruneSessionMemories(ctx, PruneOptions{})
	if err != nil {
		t.Fatalf("PruneSessionMemories: %v", err)
	}
	got := map[string]PruneCandidate{}
	for _, c := range report.Candidates {
		got[c.ID] = c
		if c.Retention != RetentionSession {
			t.Errorf("a %s row is a prune candidate: %s", c.Retention, c.ID)
		}
	}
	if len(got) != 2 {
		t.Fatalf("the preview selected %d row(s), want the 2 session rows: %+v", len(got), report.Candidates)
	}

	// Untouched: the expiry is the save's own forward-derived value and is the
	// newest stamp, so the basis IS the expiry while the activity is the save.
	// Equality here is the fact the renderer has to be able to see.
	u := got[untouched]
	if u.GraceFrom != u.ExpiresAt {
		t.Errorf("%s: GraceFrom = %q, want the expiry %q — nothing has written to this row since its save, so the expiry is the newest stamp the grace can be measured from",
			untouched, u.GraceFrom, u.ExpiresAt)
	}
	if u.ActivityAt == u.ExpiresAt {
		t.Errorf("%s: ActivityAt = %q, which is the expiry: nothing touched this row at the instant it stopped being wanted, and a report saying so is claiming an event",
			untouched, u.ActivityAt)
	}
	if u.ActivityAt == "" || u.GraceFrom == "" {
		t.Errorf("%s: both readings must be populated: %+v", untouched, u)
	}

	// Edited: the write is later than the expiry, so the basis follows the write
	// and the two readings agree on something that HAPPENED.
	e := got[edited]
	if e.GraceFrom != e.ActivityAt {
		t.Errorf("%s: GraceFrom = %q and ActivityAt = %q, want the same — the row's last write is the newest stamp, so the grace ran from the event",
			edited, e.GraceFrom, e.ActivityAt)
	}
	if e.GraceFrom == e.ExpiresAt {
		t.Errorf("%s: GraceFrom = %q is the expiry, but this row was edited after it: the basis is the write",
			edited, e.GraceFrom)
	}

	// And the case the two comments disagreed about, pinned rather than argued: a
	// FOLD is a write, and #772 made it the write that most refreshes a session
	// row — but raiseRetentionTx sets expires_at and deliberately leaves
	// updated_at alone, so on a row with no recorded read the expiry is STILL the
	// newest stamp. The basis therefore equals the expiry here and the renderer
	// omits the label, even though something did write to the row. The rule is not
	// "nothing has been written to the row"; it is "no stamp on the row is newer
	// than the expiry".
	// The fold happens BEFORE the ageing, which is the order the case needs: the
	// fold refreshes the expiry to now+SessionTTL, so a row aged first and folded
	// second is not a candidate at all — which is #772 working, and would make this
	// the re-emission test's subject rather than this one's.
	folded := pruneRow(t, s, testProject, "a session note a later save folds into", RetentionSession)
	backdateWrite(t, s, folded, stamp(-30*24*time.Hour))
	if _, dup, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"a session note a later save folds into, at some length", "mcp", 0.6, nil,
		UpsertOptions{Retention: RetentionSession}); err != nil {
		t.Fatalf("fold into %s: %v", folded, err)
	} else if dup != folded {
		t.Fatalf("the restatement folded into %q, want %q", dup, folded)
	}
	agePruneRow(t, s, folded, stamp(-30*24*time.Hour+SessionTTL), "")
	// The fold must leave updated_at alone, or this row is not the case the test
	// claims to be. Asserted rather than assumed, because the whole point of the
	// case is that the fold's write lands on a DIFFERENT column than an edit's.
	if got := getOne(t, s, folded); got.UpdatedAt != stamp(-30*24*time.Hour) {
		t.Errorf("updated_at = %q after the fold, want the backdated %q: #772 refreshes expires_at and must not move this",
			got.UpdatedAt, stamp(-30*24*time.Hour))
	}
	report, err = s.PruneSessionMemories(ctx, PruneOptions{})
	if err != nil {
		t.Fatalf("PruneSessionMemories: %v", err)
	}
	got = map[string]PruneCandidate{}
	for _, c := range report.Candidates {
		got[c.ID] = c
	}
	fc, ok := got[folded]
	if !ok {
		t.Fatalf("the folded row %s is not a candidate; the fixture did not age a prunable row", folded)
	}
	if fc.GraceFrom != fc.ExpiresAt {
		t.Errorf("%s: GraceFrom = %q, want the expiry %q — a fold refreshes expires_at and leaves updated_at alone, so the expiry is still the newest stamp and the basis is not a separate event",
			folded, fc.GraceFrom, fc.ExpiresAt)
	}
}

// TestARestoreDoesNotRevertATierOrAnExpiry is the fixture for the bullet's
// claim about RestoreSnapshot's two paths, which had been asserted in prose with
// nothing behind it. The two are different answers and only one of them is a
// DEFAULT: the UPDATE omits both columns, and an UPDATE that omits a column
// leaves the live row's value alone, while the fresh INSERT takes the column
// DEFAULT because it mints a row with no value of its own.
//
// A restore that REVERTED would be a destructive act nobody asked for, and it
// would be invisible: memory_snapshots records no tier and no expiry, so the
// statement could only revert one by writing a literal, and a literal is exactly
// what a future edit adding `retention = ?, expires_at = ?` to the column list
// would do. Each case below is a shape a restore is actually asked to undo.
//
// The persistent case is deliberately NOT here: a row that became keep-forever
// is outside the UPDATE altogether (retentionExemptSQL), which is a different
// rule with its own fixture in retention_exempt_test.go. The two here stay
// INSIDE the statement, so what is under test is the omitted columns rather
// than the exclusion.
//
// ONE SUBTEST CARRIES THE WEIGHT, and the split is not arbitrary. A session row
// can only be raised to project or keep-forever, keep-forever puts it outside
// the statement, and project IS the column DEFAULT — so for any row still in
// scope, "the UPDATE preserved the raise" and "the UPDATE reset it to the
// default" are the SAME value, and the first subtest cannot tell them apart. It
// is kept for the one thing it does pin: that a restore hands a durable row no
// expiry. The subtest that discriminates is the session one, where preserving and
// resetting differ in BOTH columns at once.
func TestARestoreDoesNotRevertATierOrAnExpiry(t *testing.T) {
	t.Run("a raised durable row gains no expiry (weak, see above)", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		const body = "the staging cluster answers on port 8443"

		live, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", body, "mcp", 0.5, nil,
			UpsertOptions{Retention: RetentionSession})
		if err != nil {
			t.Fatalf("save the session row: %v", err)
		}
		// The snapshot, and a byte-identical re-emission so the row is REUSED and
		// keeps the id the snapshot records. Without the reuse the snapshot names no
		// live row and the restore would take the INSERT path instead.
		if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
			Category: "fact", Content: body, Importance: 0.5, ReplacesIDs: []string{live},
		}}, ""); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
		if !rowExists(t, s, live) {
			t.Fatal("the row was not reused, so the snapshot does not name its id")
		}
		if got := getOne(t, s, live); got.Retention != RetentionSession {
			t.Fatalf("the snapshot row is %s, want %q", got.Retention, RetentionSession)
		}

		// The edit the restore needs: a restore only rewrites a row whose text has
		// moved, and without it the UPDATE would be a no-op and the assertions below
		// would pass for the wrong reason. A PROJECT save, which raises the tier and
		// clears the expiry — the two facts the restore must not undo.
		edited := "the staging cluster answers on port 8443, kept for the record"
		if err := s.UpdateMemory(ctx, testProject, live, &edited, nil, nil, nil); err != nil {
			t.Fatalf("edit the row after the snapshot: %v", err)
		}
		if _, dup, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
			edited+" and restated as durable", "mcp", 0.5, nil,
			UpsertOptions{Retention: RetentionProject}); err != nil {
			t.Fatalf("raise the row: %v", err)
		} else if dup != live {
			t.Fatalf("the restatement folded into %q, want %q", dup, live)
		}
		raised := getOne(t, s, live)
		if raised.Retention != RetentionProject {
			t.Fatalf("the fixture did not raise the row (retention %q); the restore would have nothing to undo", raised.Retention)
		}

		if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
			t.Fatalf("RestoreSnapshot: %v", err)
		}
		// The revert of the TEXT is the proof the UPDATE fired: the restore's job is
		// to put the row back, so the content coming back to the snapshot's is what
		// distinguishes a real restore from a no-op one. Asserting the edit SURVIVED
		// would be asserting the restore failed.
		after := getOne(t, s, live)
		if after.Content != body {
			t.Fatalf("the restore did not rewrite the row (content %q, want the snapshot's %q): the fixture's UPDATE path did not fire", after.Content, body)
		}
		if after.Retention != RetentionProject {
			t.Errorf("after the restore the row is %q, want %q: the UPDATE omits retention, so it must leave the tier a later raise established rather than reverting it",
				after.Retention, RetentionProject)
		}
		if after.ExpiresAt != nil {
			t.Errorf("after the restore the row carries expires_at %q; a durable row carrying an expiry is a claim nobody made, and the restore must not have written one", *after.ExpiresAt)
		}
	})

	t.Run("a session row keeps the tier and the expiry a fold gave it", func(t *testing.T) {
		// The discriminating case. The row is still session, so preserving and
		// resetting differ in BOTH columns at once: a restore that grew the column
		// list would make it project and expiry-less, and both assertions below fail
		// together. The expiry is the fold's and not the save's, because
		// raiseRetentionTx refreshes one and deliberately leaves updated_at alone —
		// so this is also the only shape in which a session row's expiry is newer
		// than its last write.
		s := testStore(t)
		ctx := context.Background()
		from := time.Now().UTC()
		const body = "a session fact the consolidator keeps"

		live, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", body, "mcp", 0.5, nil,
			UpsertOptions{Retention: RetentionSession})
		if err != nil {
			t.Fatalf("save the session row: %v", err)
		}
		if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
			Category: "fact", Content: body, Importance: 0.5, ReplacesIDs: []string{live},
		}}, ""); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
		edited := "a session fact the consolidator keeps, with more of it"
		if err := s.UpdateMemory(ctx, testProject, live, &edited, nil, nil, nil); err != nil {
			t.Fatalf("edit the row after the snapshot: %v", err)
		}
		// The fold, which refreshes the expiry and leaves the tier alone.
		if _, dup, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
			edited+" and restated", "mcp", 0.5, nil,
			UpsertOptions{Retention: RetentionSession}); err != nil {
			t.Fatalf("fold into the row: %v", err)
		} else if dup != live {
			t.Fatalf("the restatement folded into %q, want %q", dup, live)
		}
		refreshed := getOne(t, s, live)

		if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
			t.Fatalf("RestoreSnapshot: %v", err)
		}
		after := getOne(t, s, live)
		if after.Content != body {
			t.Fatalf("the restore did not rewrite the row (content %q, want the snapshot's %q): the fixture's UPDATE path did not fire", after.Content, body)
		}
		if after.Retention != RetentionSession {
			t.Errorf("after the restore the row is %q, want %q", after.Retention, RetentionSession)
		}
		wantFreshSessionExpiry(t, live, after.ExpiresAt, from, time.Now().UTC())
		if !sameStrPtr(after.ExpiresAt, refreshed.ExpiresAt) {
			t.Errorf("after the restore the expiry is %v, want the one the fold refreshed (%v): the UPDATE omits expires_at, so it must not have cleared it",
				expiryText(after.ExpiresAt), expiryText(refreshed.ExpiresAt))
		}
	})

	t.Run("the fresh INSERT takes the column DEFAULT", func(t *testing.T) {
		// A row that really is gone comes back through the INSERT ... SELECT, which
		// names neither column, so it arrives as project with no expiry: kept
		// longer, never pruned. The opposite of the two cases above, and stated here
		// because the bullet's whole point is that the two paths differ.
		s := testStore(t)
		ctx := context.Background()
		const body = "a session fact that gets deleted before the restore"

		live, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", body, "mcp", 0.5, nil,
			UpsertOptions{Retention: RetentionSession})
		if err != nil {
			t.Fatalf("save the session row: %v", err)
		}
		if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
			Category: "fact", Content: body, Importance: 0.5, ReplacesIDs: []string{live},
		}}, ""); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM memories WHERE id = ?`, live); err != nil {
			t.Fatalf("delete the row the snapshot names: %v", err)
		}

		if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
			t.Fatalf("RestoreSnapshot: %v", err)
		}
		if !rowExists(t, s, live) {
			t.Fatalf("the row was not brought back by its snapshot")
		}
		back := getOne(t, s, live)
		if back.Retention != RetentionProject {
			t.Errorf("the re-created row is %q, want %q: the INSERT names no tier, so it takes the column DEFAULT",
				back.Retention, RetentionProject)
		}
		if back.ExpiresAt != nil {
			t.Errorf("the re-created row carries expires_at %q, want NULL: nothing may schedule an expiry for a row that had none", *back.ExpiresAt)
		}
	})
}
