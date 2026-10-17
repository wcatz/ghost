package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// guardReaddRound seeds a project with two rows, runs `ghost reflect --apply`
// against a fake harness whose only operation merges them into mergeText, and
// returns the seeded ids and the command's stderr. mergeText is chosen by each
// test so the drop guard flags the first row and not the second.
func guardReaddRound(t *testing.T, project, mergeText string, seed func(*testing.T, context.Context, *memory.Store) (first, second string), extraArgs ...string) (dataHome, first, second, stderr string) {
	t.Helper()
	dataHome, ids, stderr := reflectOpsRound(t, project,
		func(t *testing.T, ctx context.Context, s *memory.Store) []string {
			a, b := seed(t, ctx, s)
			return []string{a, b}
		},
		func(ids []string) []string { return []string{"merge " + ids[0] + "," + ids[1] + " -> " + mergeText} },
		extraArgs...)
	return dataHome, ids[0], ids[1], stderr
}

// reflectOpsRound seeds a project, runs `ghost reflect --apply` once against a
// fake harness answering with the operations ops builds from the seeded ids, and
// returns the ids and the command's stderr.
func reflectOpsRound(t *testing.T, project string, seed func(*testing.T, context.Context, *memory.Store) []string, ops func(ids []string) []string, extraArgs ...string) (dataHome string, ids []string, stderr string) {
	t.Helper()
	dataHome = isolatedLifecycleEnv(t)
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(dataHome, "ghost"), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	s := openReflectStore(t, dataHome, project)
	ids = seed(t, ctx, s)
	if err := s.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
	// Older than the round trip, so the replace does not treat the seed as a
	// concurrent save and leave it alone.
	if err := execBackdateAges(dataHome, project); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	calls := filepath.Join(t.TempDir(), "calls")
	t.Setenv("GHOST_CLI_CLAUDE_BINARY", opsReflectHarness(t, calls, ops(ids)))
	t.Setenv("GHOST_SCRATCH_DIR", t.TempDir())
	args := append([]string{"reflect", project, "--tier", "cli", "--apply"}, extraArgs...)
	stderr = captureStderr(t, func() {
		_ = captureStdout(t, func() { runReflectArgs(t, args...) })
	})
	if n := countLines(t, calls); n != 1 {
		t.Fatalf("the harness answered %d rounds, want 1", n)
	}
	return dataHome, ids, stderr
}

// opsReflectHarness is fakeReflectHarness with the operation list given whole,
// so a round can mix a merge with a rewrite.
func opsReflectHarness(t *testing.T, calls string, ops []string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"ops": ops})
	if err != nil {
		t.Fatalf("build harness response: %v", err)
	}
	if strings.ContainsAny(string(payload), "'") {
		t.Fatalf("the harness payload would need shell quoting to survive: %s", payload)
	}
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--help\" ]; then\n" +
		"  printf '%s\\n' '--safe-mode' '--restricted' '--strict-mcp-config' '--disable-slash-commands' '--tools' '--disallowedTools' '--setting-sources'\n" +
		"  exit 0\n" +
		"fi\n" +
		"echo x >> " + calls + "\n" +
		"printf '%s' '" + string(payload) + "'\n"
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec // an executable this test runs
		t.Fatalf("write fake harness: %v", err)
	}
	return path
}

// seedCompanionAndFact records a decision (its companion is the first row) and
// an ordinary fact the merge text restates (the second).
func seedCompanionAndFact(project string, decID *string) func(*testing.T, context.Context, *memory.Store) (string, string) {
	return func(t *testing.T, ctx context.Context, s *memory.Store) (string, string) {
		t.Helper()
		dec, comp, _, err := s.RecordDecision(ctx, project, "queue", "we use nats for the event queue", "it was cheapest", nil, nil)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		*decID = dec
		fact, err := s.Create(ctx, project, memory.Memory{Category: "fact", Source: "mcp", Importance: 0.5,
			Content: "the event queue consumer restarts in place"})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		return comp, fact
	}
}

const guardReaddMergeText = "the event queue consumer restarts in place, and the queue choice is recorded"

// liveLinkCarriers lists the live rows of the project whose source_ref names
// the decision, and the source of each.
func liveLinkCarriers(t *testing.T, dataHome, project, decisionID string) map[string]string {
	t.Helper()
	db, err := openRawDB(dataHome)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close() //nolint:errcheck
	rows, err := db.Query(`SELECT id, source FROM memories WHERE project_id = ? AND resolved_at IS NULL AND source_ref LIKE ?`,
		project, "%decision:"+decisionID+"%")
	if err != nil {
		t.Fatalf("read carriers: %v", err)
	}
	defer rows.Close() //nolint:errcheck
	out := map[string]string{}
	for rows.Next() {
		var id, source string
		if err := rows.Scan(&id, &source); err != nil {
			t.Fatalf("scan carrier: %v", err)
		}
		out[id] = source
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read carriers: %v", err)
	}
	return out
}

func supersedeAfterRound(t *testing.T, dataHome, project, decID string) memory.DecisionRetirement {
	t.Helper()
	ctx := context.Background()
	s := openReflectStore(t, dataHome, project)
	defer s.Close() //nolint:errcheck
	newDec, _, _, err := s.RecordDecision(ctx, project, "queue v2", "we use kafka for the event queue", "scale", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision (new): %v", err)
	}
	rep, err := s.SupersedeDecisionReport(ctx, project, decID, newDec)
	if err != nil {
		t.Fatalf("SupersedeDecisionReport: %v", err)
	}
	return rep
}

// assertNoCleanRetirementWhileRestated is the property every decision round
// below must keep: a supersede that answered clean (nothing ambiguous, declined
// or shared) leaves no live row in the project that still states the reversed
// decision, linked or not.
func assertNoCleanRetirementWhileRestated(t *testing.T, dataHome, project string, rep memory.DecisionRetirement, reversed string) {
	t.Helper()
	if len(rep.Ambiguous) > 0 || len(rep.Declined) > 0 || len(rep.Shared) > 0 {
		return
	}
	if got := readColumn(t, dataHome,
		`SELECT count(*) FROM memories WHERE project_id = ? AND resolved_at IS NULL AND lower(content) LIKE ?`,
		project, "%"+strings.ToLower(reversed)+"%"); got != "0" {
		t.Errorf("supersede report %+v answered clean while %s live row(s) still state %q", rep, got, reversed)
	}
}

// `ghost reflect --apply` with default flags, a merge whose text does not carry
// a decision companion: the drop guard re-adds the companion verbatim and its
// row is reused. The merge keeps the companion's link (the store cannot tell
// whether its text still restates the decision) but takes none of its
// evidence. Two live rows carry the link, so the supersede reports both as
// ambiguous; it used to retire the companion alone and answer clean while the
// merge (category decision, importance from the companion) stayed live.
func TestRunReflectGuardReaddedCompanionKeepsItsLinkOnTheMerge(t *testing.T) {
	const project = "guardreaddz"
	var decID string
	dataHome, comp, fact, stderr := guardReaddRound(t, project, guardReaddMergeText, seedCompanionAndFact(project, &decID))
	if !strings.Contains(stderr, "retained 1 verbatim") {
		t.Fatalf("the drop guard did not re-add the companion; stderr:\n%s", stderr)
	}
	successor := readColumn(t, dataHome, `SELECT id FROM memories WHERE project_id = ? AND content = ?`, project, guardReaddMergeText)
	carriers := liveLinkCarriers(t, dataHome, project, decID)
	if len(carriers) != 2 || carriers[comp] != "decision_log" || carriers[successor] != "reflection" {
		t.Fatalf("live rows carrying the decision link = %v, want the companion %s and the merge %s", carriers, comp, successor)
	}
	carried := readColumn(t, dataHome,
		`SELECT COALESCE(group_concat(carried_from), '') FROM memory_provenance WHERE memory_id = ?`, successor)
	if carried != fact {
		t.Errorf("the merge carries evidence from %q, want only the absorbed fact %s", carried, fact)
	}

	rep := supersedeAfterRound(t, dataHome, project, decID)
	if len(rep.Retired) != 0 || len(rep.Ambiguous) != 2 {
		t.Errorf("supersede report = %+v, want nothing retired and both carriers (%s, %s) ambiguous", rep, comp, successor)
	}
	assertNoCleanRetirementWhileRestated(t, dataHome, project, rep, "nats")
}

// A reuse that is NOT a guard re-add: a rewrite of a near-copy decision row
// lands on the companion's exact text, so it reuses the companion's row, while
// a merge of the companion and a fact restates the decision word for word. The
// merge must keep the link; without it, the supersede retired the companion,
// answered clean, and the merge went on stating "we use nats" with no link any
// later supersede could follow.
func TestRunReflectAMergeBesideAReuseOfItsCompanionKeepsTheLink(t *testing.T) {
	const project = "reuseb1z"
	var decID, compText string
	dataHome, ids, _ := reflectOpsRound(t, project,
		func(t *testing.T, ctx context.Context, s *memory.Store) []string {
			t.Helper()
			companionAndFact := seedCompanionAndFact(project, &decID)
			comp, fact := companionAndFact(t, ctx, s)
			got, err := s.GetByIDs(ctx, []string{comp})
			if err != nil || len(got) != 1 {
				t.Fatalf("read the companion: %v (%d rows)", err, len(got))
			}
			compText = got[0].Content
			near, err := s.Create(ctx, project, memory.Memory{Category: "decision", Source: "mcp", Importance: 0.6,
				Content: "queue: we use nats for the event queue, rationale: it was the cheapest"})
			if err != nil {
				t.Fatalf("seed the near copy: %v", err)
			}
			return []string{comp, fact, near}
		},
		func(ids []string) []string {
			return []string{
				"merge " + ids[0] + "," + ids[1] + " -> " + compText + ". The event queue consumer restarts in place",
				"rewrite " + ids[2] + " -> " + compText,
			}
		})
	comp := ids[0]
	successor := readColumn(t, dataHome, `SELECT id FROM memories WHERE project_id = ? AND content = ?`,
		project, compText+". The event queue consumer restarts in place")
	carriers := liveLinkCarriers(t, dataHome, project, decID)
	if carriers[comp] == "" || carriers[successor] == "" {
		t.Fatalf("live rows carrying the decision link = %v, want the reused companion %s and the merge %s", carriers, comp, successor)
	}
	rep := supersedeAfterRound(t, dataHome, project, decID)
	if len(rep.Retired) != 0 || len(rep.Ambiguous) != 2 {
		t.Errorf("supersede report = %+v, want nothing retired and both carriers ambiguous", rep)
	}
	assertNoCleanRetirementWhileRestated(t, dataHome, project, rep, "nats")
}

// The guard path with a paraphrase: the merge restates the decision in other
// words, the guard scores the companion below its bar and re-adds it. The merge
// still asserts NATS, so it must keep the link and the supersede must not
// answer clean.
func TestRunReflectAParaphrasingMergeBesideAGuardReaddKeepsTheLink(t *testing.T) {
	const project = "reuseb2z"
	const mergeText = "NATS is our event bus, and its consumer restarts in place"
	var decID string
	dataHome, comp, _, stderr := guardReaddRound(t, project, mergeText, seedCompanionAndFact(project, &decID))
	if !strings.Contains(stderr, "retained 1 verbatim") {
		t.Fatalf("the drop guard did not re-add the companion; stderr:\n%s", stderr)
	}
	successor := readColumn(t, dataHome, `SELECT id FROM memories WHERE project_id = ? AND content = ?`, project, mergeText)
	carriers := liveLinkCarriers(t, dataHome, project, decID)
	if carriers[comp] == "" || carriers[successor] == "" {
		t.Fatalf("live rows carrying the decision link = %v, want the companion %s and the merge %s", carriers, comp, successor)
	}
	rep := supersedeAfterRound(t, dataHome, project, decID)
	if len(rep.Retired) != 0 || len(rep.Ambiguous) != 2 {
		t.Errorf("supersede report = %+v, want nothing retired and both carriers ambiguous", rep)
	}
	assertNoCleanRetirementWhileRestated(t, dataHome, project, rep, "nats")
}

// --allow-drops keeps its meaning: the guard's flagged companion is deleted,
// so the merge IS its successor, inherits the link, and the supersede retires
// the merge.
func TestRunReflectAllowDropsHandsTheCompanionLinkToTheMerge(t *testing.T) {
	const project = "guardallowz"
	var decID string
	dataHome, comp, _, stderr := guardReaddRound(t, project, guardReaddMergeText, seedCompanionAndFact(project, &decID), "--allow-drops")
	if !strings.Contains(stderr, "--allow-drops set: these 1 memories will be DELETED") {
		t.Fatalf("the drop guard did not flag the companion; stderr:\n%s", stderr)
	}
	successor := readColumn(t, dataHome, `SELECT id FROM memories WHERE project_id = ? AND content = ?`, project, guardReaddMergeText)
	carriers := liveLinkCarriers(t, dataHome, project, decID)
	if len(carriers) != 1 || carriers[successor] != "reflection" {
		t.Fatalf("live rows carrying the decision link = %v, want only the merge %s (companion %s deleted)", carriers, successor, comp)
	}
	rep := supersedeAfterRound(t, dataHome, project, decID)
	if len(rep.Retired) != 1 || rep.Retired[0] != successor {
		t.Errorf("supersede report = %+v, want the merge %s retired", rep, successor)
	}
}

// The control: an ordinary re-add with no decision link. The guard's warning
// and its verbatim reuse are unchanged — the row keeps its id, age, source and
// evidence — and the merge still absorbs the other source and its evidence.
func TestRunReflectGuardReaddedFactKeepsItsRowBesideTheMerge(t *testing.T) {
	const project = "guardfactz"
	const mergeText = "the bastion host is the only ssh entry point for every repo"
	dataHome, kept, other, stderr := guardReaddRound(t, project, mergeText,
		func(t *testing.T, ctx context.Context, s *memory.Store) (string, string) {
			t.Helper()
			a, err := s.Create(ctx, project, memory.Memory{Category: "gotcha", Source: "mcp", Importance: 0.7,
				Content: "deploys freeze on friday afternoons after the release cut"})
			if err != nil {
				t.Fatalf("seed: %v", err)
			}
			b, err := s.Create(ctx, project, memory.Memory{Category: "fact", Source: "mcp", Importance: 0.5,
				Content: "the bastion host is the only ssh entry point"})
			if err != nil {
				t.Fatalf("seed: %v", err)
			}
			return a, b
		})
	for _, want := range []string{"WARNING: 1 memory(ies) had no surviving merge target:", "retained 1 verbatim — nothing dropped"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if got := readColumn(t, dataHome, `SELECT source || '|' || (created_at < datetime('now', '-30 minutes')) FROM memories WHERE id = ?`, kept); got != "mcp|1" {
		t.Errorf("the re-added row reads %q, want its own source and its backdated age (mcp|1)", got)
	}
	if got := readColumn(t, dataHome, `SELECT count(*) FROM memory_provenance WHERE memory_id = ? AND carried_from IS NULL`, kept); got == "0" {
		t.Error("the re-added row lost its own evidence")
	}
	if got := readColumn(t, dataHome, `SELECT count(*) FROM memories WHERE id = ?`, other); got != "0" {
		t.Error("the absorbed row survived the merge")
	}
	successor := readColumn(t, dataHome, `SELECT id FROM memories WHERE project_id = ? AND content = ?`, project, mergeText)
	if got := readColumn(t, dataHome,
		`SELECT COALESCE(group_concat(carried_from), '') FROM memory_provenance WHERE memory_id = ?`, successor); got != other {
		t.Errorf("the merge carries evidence from %q, want only the absorbed row %s", got, other)
	}
	if got := readColumn(t, dataHome,
		`SELECT COALESCE(related_id, '') FROM memory_history WHERE memory_id = ? AND phase = 'delete'`, other); got != successor {
		t.Errorf("the absorbed row's delete history names %q, want the merge %s", got, successor)
	}
}
