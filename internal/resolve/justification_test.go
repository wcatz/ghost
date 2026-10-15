package resolve

// #1389: a deterministic demotion is stamped on the strength of a signal the
// pass read BEFORE its classify call — a live supersedes edge (mechanism 1) or
// a correction memory (mechanism 2). A repair that lands in that window — a
// withdrawal, or the correction being deleted or rewritten — removes the
// signal, and the write must notice, exactly as #1052 does for a rewritten
// target. The refused row is reported in Stale and stays live.

import (
	"context"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// duringClassify runs fn once, inside the classify call, and answers KEEP for
// everything — the classifier standing in for a concurrent writer.
type duringClassify struct {
	fn    func(ctx context.Context) error
	calls int
}

func (c *duringClassify) IsResolvedBatch(ctx context.Context, contents []string) ([]Verdict, error) {
	c.calls++
	if c.fn != nil {
		if err := c.fn(ctx); err != nil {
			return nil, err
		}
	}
	out := make([]Verdict, len(contents))
	for i := range out {
		out[i] = VerdictKeep
	}
	return out, nil
}

func mustCreate(t *testing.T, s *memory.Store, ctx context.Context, project, content string) string {
	t.Helper()
	id, err := s.Create(ctx, project, memory.Memory{Category: "fact", Content: content, Importance: 0.5, Source: "mcp"})
	if err != nil {
		t.Fatalf("Create %q: %v", content, err)
	}
	return id
}

func resolvedAt(t *testing.T, s *memory.Store, ctx context.Context, id string) *string {
	t.Helper()
	rows, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(rows) != 1 {
		t.Fatalf("GetByIDs: %v (%d rows)", err, len(rows))
	}
	return rows[0].ResolvedAt
}

const bystander = "the migration was shipped in v2 and is closed"

func TestAWithdrawnSupersedesEdgeDoesNotBuryItsTarget(t *testing.T) {
	s, ctx, project := newFreshnessStore(t)
	older := mustCreate(t, s, ctx, project, "the cache layer uses redis for sessions")
	newer := mustCreate(t, s, ctx, project, "the cache layer now uses memcached for sessions")
	mustCreate(t, s, ctx, project, bystander)
	if err := s.CreateLink(ctx, newer, older, "supersedes", 0.9, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	cls := &duringClassify{fn: func(ctx context.Context) error {
		n, err := s.InvalidateLink(ctx, newer, older, "supersedes")
		if err == nil && n != 1 {
			t.Errorf("withdrew %d edges, want 1", n)
		}
		return err
	}}

	res, _, err := Run(ctx, s, cls, project, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cls.calls != 1 {
		t.Fatalf("classifier calls = %d, want 1 (the window under test)", cls.calls)
	}
	if res.Resolved != 0 || res.Stale != 1 {
		t.Errorf("Resolved=%d Stale=%d, want 0 and 1: the edge was withdrawn before the write", res.Resolved, res.Stale)
	}
	if ra := resolvedAt(t, s, ctx, older); ra != nil {
		t.Errorf("older resolved_at = %q, want NULL: nothing holds it down any more", *ra)
	}
}

// The positive half: an edge that is still live at write time still stamps, and
// a target with two live edges survives the withdrawal of one of them.
func TestALiveSupersedesEdgeStillStamps(t *testing.T) {
	s, ctx, project := newFreshnessStore(t)
	older := mustCreate(t, s, ctx, project, "the cache layer uses redis for sessions")
	newer := mustCreate(t, s, ctx, project, "the cache layer now uses memcached for sessions")
	newest := mustCreate(t, s, ctx, project, "sessions moved to valkey, replacing memcached")
	mustCreate(t, s, ctx, project, bystander)
	for _, src := range []string{newer, newest} {
		if err := s.CreateLink(ctx, src, older, "supersedes", 0.9, "llm"); err != nil {
			t.Fatalf("CreateLink: %v", err)
		}
	}
	cls := &duringClassify{fn: func(ctx context.Context) error {
		_, err := s.InvalidateLink(ctx, newer, older, "supersedes")
		return err
	}}
	res, _, err := Run(ctx, s, cls, project, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Resolved != 1 || res.Stale != 0 {
		t.Errorf("Resolved=%d Stale=%d, want 1 and 0: a second live edge still justifies the stamp", res.Resolved, res.Stale)
	}
	if ra := resolvedAt(t, s, ctx, older); ra == nil {
		t.Error("older resolved_at is NULL, want stamped")
	}
}

const correctionText = "CORRECTION/RESOLUTION: the sessions cache fix zephyrone zephyrtwo zephyrthree is already fixed on main"
const claimText = "sessions cache problem zephyrone zephyrtwo zephyrthree was shipped with a bug"

func correctionFixture(t *testing.T) (*memory.Store, context.Context, string, string, string) {
	s, ctx, project := newFreshnessStore(t)
	// Same-second creates tie on updated_at and fall back to id order, so the ids
	// are chosen: the correction must be the newer of the pair for it to pair.
	const claim, correction = "m-claim-a", "m-claim-b"
	for id, content := range map[string]string{claim: claimText, correction: correctionText} {
		if _, err := s.CreateWithID(ctx, project, id, memory.Memory{Category: "fact", Content: content, Importance: 0.5, Source: "mcp"}); err != nil {
			t.Fatalf("CreateWithID %s: %v", id, err)
		}
	}
	mustCreate(t, s, ctx, project, bystander)
	return s, ctx, project, claim, correction
}

func TestACorrectionDeletedDuringClassifyDoesNotBuryItsTarget(t *testing.T) {
	s, ctx, project, claim, correction := correctionFixture(t)
	cls := &duringClassify{fn: func(ctx context.Context) error { return s.Delete(ctx, correction) }}
	res, _, err := Run(ctx, s, cls, project, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Corrected != 1 {
		t.Fatalf("Corrected = %d, want 1 (the fixture must pair)", res.Corrected)
	}
	if res.Resolved != 0 || res.Stale != 1 {
		t.Errorf("Resolved=%d Stale=%d, want 0 and 1: the correction was deleted before the write", res.Resolved, res.Stale)
	}
	if ra := resolvedAt(t, s, ctx, claim); ra != nil {
		t.Errorf("claim resolved_at = %q, want NULL", *ra)
	}
}

func TestACorrectionRewrittenDuringClassifyDoesNotBuryItsTarget(t *testing.T) {
	s, ctx, project, claim, correction := correctionFixture(t)
	rewrite := "the sessions cache fix is NOT done; keep the patch plan live"
	cls := &duringClassify{fn: func(ctx context.Context) error {
		return s.UpdateMemory(ctx, project, correction, &rewrite, nil, nil, nil)
	}}
	res, _, err := Run(ctx, s, cls, project, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Corrected != 1 {
		t.Fatalf("Corrected = %d, want 1 (the fixture must pair)", res.Corrected)
	}
	if res.Resolved != 0 || res.Stale != 1 {
		t.Errorf("Resolved=%d Stale=%d, want 0 and 1: the correction was rewritten before the write", res.Resolved, res.Stale)
	}
	if ra := resolvedAt(t, s, ctx, claim); ra != nil {
		t.Errorf("claim resolved_at = %q, want NULL", *ra)
	}
}

func TestAnUntouchedCorrectionStillStamps(t *testing.T) {
	s, ctx, project, claim, _ := correctionFixture(t)
	res, _, err := Run(ctx, s, &duringClassify{}, project, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Corrected != 1 || res.Resolved != 1 || res.Stale != 0 {
		t.Errorf("Corrected=%d Resolved=%d Stale=%d, want 1, 1, 0", res.Corrected, res.Resolved, res.Stale)
	}
	if ra := resolvedAt(t, s, ctx, claim); ra == nil {
		t.Error("claim resolved_at is NULL, want stamped")
	}
}
