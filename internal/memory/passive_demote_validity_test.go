package memory

import (
	"context"
	"testing"
	"time"
)

// passiveDemoteValidityFixture creates the rows #893 reproduces with. A row
// whose validity window has closed is the winner of the edge under test; the
// rows it would demote are live.
func passiveDemoteValidityFixture(t *testing.T) (*Store, context.Context, time.Time) {
	t.Helper()
	s, ctx := newDedupStore(t)
	if err := s.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject _global: %v", err)
	}
	return s, ctx, time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
}

func createPassiveRow(t *testing.T, s *Store, ctx context.Context, project, content string, importance float32) string {
	t.Helper()
	id, err := s.Create(ctx, project, Memory{Category: "fact", Content: content, Source: "manual", Importance: importance})
	if err != nil {
		t.Fatalf("Create(%s): %v", content, err)
	}
	return id
}

// TestPassiveNearDuplicateWinnerOutOfWindowDemotesNothing is the first case of
// #893. Two _global rows are linked as near-duplicates and the higher-ranked
// winner is past its valid_until. The _global policy drops near-duplicate
// losers, so judging the pair before validity removes the live loser while the
// expired winner is dropped as expired afterwards: neither reaches the block.
// The control (no edge) pins that the live row is otherwise admitted.
func TestPassiveNearDuplicateWinnerOutOfWindowDemotesNothing(t *testing.T) {
	for _, linked := range []bool{false, true} {
		name := "control-no-edge"
		if linked {
			name = "edge-to-expired-winner"
		}
		t.Run(name, func(t *testing.T) {
			s, ctx, now := passiveDemoteValidityFixture(t)
			past := now.Add(-24 * time.Hour).Format(StoredStampLayout)
			winner := createPassiveRow(t, s, ctx, "_global", "expired winner", 0.99)
			live := createPassiveRow(t, s, ctx, "_global", "live restatement", 0.5)
			setRawValidity(t, s, ctx, winner, strPtr(past), strPtr(past), nil)
			if linked {
				if err := s.CreateLink(ctx, live, winner, "related", 0.95, "manual"); err != nil {
					t.Fatalf("CreateLink: %v", err)
				}
			}
			req := passiveRequest(testProject, globalPassivePolicy())
			req.Now = now
			set, err := s.Candidates(ctx, req)
			if err != nil {
				t.Fatalf("Candidates: %v", err)
			}
			ids := passiveIDs(set)
			if !containsStr(ids, live) {
				t.Errorf("the live row was removed by a near-duplicate edge to an expired row: %v", ids)
			}
			if containsStr(ids, winner) {
				t.Errorf("the expired row was offered to stage 2: %v", ids)
			}
		})
	}
}

// TestPassiveSupersedeByOutOfWindowRowDemotesNothing is the second case of
// #893: a live row superseded by a row whose window has closed must keep its
// rank. Both the order and the control are asserted.
func TestPassiveSupersedeByOutOfWindowRowDemotesNothing(t *testing.T) {
	for _, linked := range []bool{false, true} {
		name := "control-no-edge"
		if linked {
			name = "supersede-by-expired"
		}
		t.Run(name, func(t *testing.T) {
			s, ctx, now := passiveDemoteValidityFixture(t)
			past := now.Add(-24 * time.Hour).Format(StoredStampLayout)
			live := createPassiveRow(t, s, ctx, "_global", "live and important", 0.99)
			other := createPassiveRow(t, s, ctx, "_global", "live and modest", 0.6)
			expired := createPassiveRow(t, s, ctx, "_global", "expired superseder", 0.3)
			setRawValidity(t, s, ctx, expired, strPtr(past), strPtr(past), nil)
			if linked {
				if err := s.CreateLink(ctx, expired, live, "supersedes", 1, "manual"); err != nil {
					t.Fatalf("CreateLink: %v", err)
				}
			}
			pol := globalPassivePolicy()
			pol.DropDemotedLosers = false
			req := passiveRequest(testProject, pol)
			req.Now = now
			set, err := s.Candidates(ctx, req)
			if err != nil {
				t.Fatalf("Candidates: %v", err)
			}
			ids := passiveIDs(set)
			if len(ids) != 2 || ids[0] != live || ids[1] != other {
				t.Errorf("order = %v, want [%s %s]: a superseder outside its window must not demote a live row", ids, live, other)
			}
		})
	}
}

// TestSelectPassiveDemotionsIgnoreRowsOutsideTheirWindow pins the order inside
// the selection itself, with the SQL predicate out of the picture. The fetch
// states the window in SQL, but the demotions must not depend on that: a row
// that is out of window may arrive here (a fetch that predates the predicate, a
// future caller), and it must demote and remove nothing, nor fill a slot of the
// selection pool. Stage 2 stays the authority, so the row is still returned,
// behind everything eligible, for it to drop.
func TestSelectPassiveDemotionsIgnoreRowsOutsideTheirWindow(t *testing.T) {
	s, ctx, now := passiveDemoteValidityFixture(t)
	past := now.Add(-24 * time.Hour).Format(StoredStampLayout)

	winner := createPassiveRow(t, s, ctx, "_global", "expired winner", 0.99)
	live := createPassiveRow(t, s, ctx, "_global", "live restatement", 0.5)
	if err := s.CreateLink(ctx, live, winner, "related", 0.95, "manual"); err != nil {
		t.Fatalf("CreateLink related: %v", err)
	}
	superseded := createPassiveRow(t, s, ctx, "_global", "live superseded", 0.9)
	expiredSuperseder := createPassiveRow(t, s, ctx, "_global", "expired superseder", 0.3)
	if err := s.CreateLink(ctx, expiredSuperseder, superseded, "supersedes", 1, "manual"); err != nil {
		t.Fatalf("CreateLink supersedes: %v", err)
	}
	for _, id := range []string{winner, expiredSuperseder} {
		setRawValidity(t, s, ctx, id, strPtr(past), strPtr(past), nil)
	}

	created := now.Add(-time.Hour).Format(StoredStampLayout)
	mk := func(id string, imp float32, until *string) Memory {
		return Memory{ID: id, Category: "fact", Importance: imp, CreatedAt: created, ValidFrom: until, ValidUntil: until}
	}
	mems := []Memory{
		mk(winner, 0.99, strPtr(past)),
		mk(superseded, 0.9, nil),
		mk(live, 0.5, nil),
		mk(expiredSuperseder, 0.3, strPtr(past)),
	}
	pol := globalPassivePolicy()
	got, _, err := s.selectPassive(ctx, mems, pol, now, pol.Bucket)
	if err != nil {
		t.Fatalf("selectPassive: %v", err)
	}
	var ids []string
	for _, c := range got {
		ids = append(ids, c.ID)
	}
	want := []string{superseded, live, winner, expiredSuperseder}
	if len(ids) != len(want) {
		t.Fatalf("selectPassive = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("selectPassive = %v, want %v (eligible rows keep their rank; out-of-window rows trail for stage 2)", ids, want)
		}
	}
}

// TestPassiveNearDuplicateWinnerExpiredWithinTheSameSecond reaches the demotion
// through Store.Candidates on the one route where the SQL predicate and stage 2
// disagree. The SQL window compares at whole-second precision, so it is a
// superset of ValidityState: a winner whose valid_until is the second the
// request clock is already part-way through is kept by SQL and expired by Go.
// Without the validity-first order that winner removes the live loser on the
// _global bucket and is then dropped itself.
func TestPassiveNearDuplicateWinnerExpiredWithinTheSameSecond(t *testing.T) {
	s, ctx, now := passiveDemoteValidityFixture(t)
	until := now.Format(StoredStampLayout)
	winner := createPassiveRow(t, s, ctx, "_global", "winner expiring this second", 0.99)
	live := createPassiveRow(t, s, ctx, "_global", "live restatement", 0.5)
	setRawValidity(t, s, ctx, winner, nil, strPtr(until), nil)
	if err := s.CreateLink(ctx, live, winner, "related", 0.95, "manual"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	req := passiveRequest(testProject, globalPassivePolicy())
	req.Now = now.Add(500 * time.Millisecond)
	set, err := s.Candidates(ctx, req)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	ids := passiveIDs(set)
	if !containsStr(ids, live) {
		t.Errorf("the live row was removed by an edge to a winner that is expired at the request clock: %v", ids)
	}
}
