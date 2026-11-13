package mcpserver

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/claudeimport"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/provider"
)

func newValiditySession(t *testing.T) (*Server, *mcp.ClientSession) {
	t.Helper()
	return validityServerFor(t, newValidityStore(t))
}

func newValidityStore(t *testing.T) *memory.Store {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	st := memory.NewStore(db, logger)
	ctx := context.Background()
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`); err != nil {
		t.Fatalf("insert _global project: %v", err)
	}
	if err := st.EnsureProject(ctx, "vproj", t.TempDir(), "vproj"); err != nil {
		t.Fatalf("EnsureProject vproj: %v", err)
	}
	if err := st.EnsureProject(ctx, "bare", t.TempDir(), "bare"); err != nil {
		t.Fatalf("EnsureProject bare: %v", err)
	}
	return st
}

func saveValidityRow(t *testing.T, session *mcp.ClientSession, content string, extra map[string]any) string {
	t.Helper()
	args := map[string]any{"project_id": "vproj", "content": content, "category": "fact"}
	for k, v := range extra {
		args[k] = v
	}
	res := callTool(t, session, "ghost_memory_save", args)
	if res.IsError {
		t.Fatalf("save %q: %s", content, resultText(res))
	}
	id, ok := extractID(resultText(res))
	if !ok {
		t.Fatalf("save %q carried no memory id: %q", content, resultText(res))
	}
	return id
}

func TestTheProjectContextDropsRowsWhoseWindowHasClosed(t *testing.T) {
	_, session := newValiditySession(t)

	live := saveValidityRow(t, session, "vproj: a durable fact about the project", nil)
	openWindow := saveValidityRow(t, session, "vproj: a policy with an open window",
		map[string]any{"valid_from": "2020-01-01", "valid_until": "2999-12-31", "verified": true})
	unverified := saveValidityRow(t, session, "vproj: a claim with a window nobody checked",
		map[string]any{"valid_from": "2020-01-01", "valid_until": "2999-12-31"})
	expired := saveValidityRow(t, session, "vproj: a migration that has been rolled back",
		map[string]any{"valid_from": "2020-01-01", "valid_until": "2021-01-01"})
	future := saveValidityRow(t, session, "vproj: a policy that starts next year",
		map[string]any{"valid_from": "2999-01-01", "valid_until": "2999-12-31"})

	out := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "vproj"}))

	for _, present := range []string{live, openWindow, unverified} {
		if !strings.Contains(out, present) {
			t.Errorf("%s is missing from the block; a row inside its window must survive:\n%s", present, out)
		}
	}
	if !strings.Contains(out, "unverified") {
		t.Errorf("a live row nobody re-verified lost its [unverified] marker, so the state is no longer reported:\n%s", out)
	}
	for _, absent := range []string{expired, future} {
		if strings.Contains(out, absent) {
			t.Errorf("%s reached the block; a row whose validity window has closed, or has not opened, is not a current claim:\n%s", absent, out)
		}
	}
	for _, marker := range []string{"expired", "not yet valid"} {
		if strings.Contains(out, marker) {
			t.Errorf("the block still carries the %q verdict for a row stage 2 dropped, so the row reached the renderer:\n%s", marker, out)
		}
	}
}

func TestTheProjectContextEmptyBlockSaysRowsWereExcludedRatherThanThatNothingWasSaved(t *testing.T) {
	_, session := newValiditySession(t)

	saveValidityRow(t, session, "vproj: the only memory, and it is retired",
		map[string]any{"valid_until": "2021-01-01"})

	withheld := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "vproj"}))
	if strings.Contains(withheld, "nothing has been saved") {
		t.Errorf("a project whose every memory has retired is reported as never having been saved for:\n%s", withheld)
	}
	if !strings.Contains(withheld, "out of date") {
		t.Errorf("the empty block does not say WHY it is empty, so a reader cannot tell a retired project from an empty one:\n%s", withheld)
	}

	empty := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "bare"}))
	if !strings.Contains(empty, "nothing has been saved") {
		t.Errorf("a registered project with no memories no longer says so; an empty window is the one case that may describe absence:\n%s", empty)
	}
}

func strPtr(s string) *string { return &s }

func TestTheProjectContextServesTheGlobalProjectItself(t *testing.T) {
	_, session := newValiditySession(t)
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ghost_save_global",
		Arguments: map[string]any{"content": "a cross-project preference", "category": "preference"},
	}); err != nil {
		t.Fatalf("save_global: %v", err)
	}
	out := resultText(callTool(t, session, "ghost_project_context", map[string]any{
		"project_id": memory.GlobalProjectID,
	}))
	if strings.Contains(out, "does not support candidate retrieval") {
		t.Fatalf("a project-context read of _global was refused outright:\n%s", out)
	}
	if !strings.Contains(out, "a cross-project preference") {
		t.Errorf("a project-context read of _global did not list the global rows:\n%s", out)
	}
}

func TestTheProjectContextKeepsTheOriginLabelOnAGlobalRowInAMixedBlock(t *testing.T) {
	st := newValidityStore(t)
	if _, err := st.CreateWithIDFromCorpus(context.Background(), memory.GlobalProjectID, "gorigin", memory.Memory{
		Category: "preference", Content: "a preference the user typed once", Source: "mcp", Importance: 0.9,
	}); err != nil {
		t.Fatalf("seed global: %v", err)
	}
	seedText := shippedSeedText(t, st)
	if got := memory.CanonicalOriginSourceForProject(memory.GlobalProjectID, "manual", seedText); got != "builtin" {
		t.Fatalf("fixture precondition: the store no longer rewrites the shipped seed text for a global row (got %q), "+
			"so this test would pass without exercising the scoping at all", got)
	}
	if got := memory.CanonicalOriginSourceForProject("vproj", "manual", seedText); got != "manual" {
		t.Fatalf("fixture precondition: a project row holding the shipped words is no longer left alone (got %q)", got)
	}
	for _, r := range []struct{ id, project string }{
		{"gseed", memory.GlobalProjectID},
		{"pseed", "vproj"},
	} {
		if _, err := st.CreateWithIDFromCorpus(context.Background(), r.project, r.id, memory.Memory{
			Category: "preference", Content: seedText, Source: "manual", Importance: 0.9,
		}); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}
	_, session := validityServerFor(t, st)

	byID := map[string]string{}
	var replaced []memory.Memory
	for _, project := range []string{"vproj", memory.GlobalProjectID} {
		rows, err := st.ListMemories(context.Background(), project, "", "", 10)
		if err != nil {
			t.Fatalf("ListMemories %s: %v", project, err)
		}
		replaced = append(replaced, rows...)
	}
	for _, line := range strings.Split(formatMemories(replaced), "\n") {
		if i := strings.Index(line, "`"); i >= 0 {
			if j := strings.Index(line[i+1:], "`"); j >= 0 {
				byID[line[i+1:i+1+j]] = strings.TrimSpace(line)
			}
		}
	}
	for _, id := range []string{"gorigin", "gseed", "pseed"} {
		if _, ok := byID[id]; !ok {
			t.Fatalf("fixture precondition: the replaced renderer printed no line for %s:\n%s", id, formatMemories(replaced))
		}
	}

	got := resultText(callTool(t, session, "ghost_project_context", map[string]any{
		"project_id": "vproj",
	}))
	for _, id := range []string{"gorigin", "gseed", "pseed"} {
		want, ok := byID[id]
		if !ok {
			t.Fatalf("fixture precondition: the replaced renderer printed no line for %s", id)
		}
		if !strings.Contains(got, want) {
			t.Errorf("a row rendered differently than the renderer this migration replaced.\n"+
				"%s: the replaced renderer ---\n%s\n--- the assembled block ---\n%s", id, want, got)
		}
	}
	if !strings.Contains(got, "`gseed` (0.9 source=builtin)") {
		t.Errorf("the global seed row is not attributed to the builtin; its OWN project is what decides that, and a "+
			"global row read through a project bucket is where it can be lost:\n%s", got)
	}
	if !strings.Contains(got, "`pseed` (0.9)") || strings.Contains(got, "`pseed` (0.9 source=builtin)") {
		t.Errorf("a PROJECT row holding the shipped words was attributed to the builtin; it is the user's own material:\n%s", got)
	}
}

func shippedSeedText(t *testing.T, st *memory.Store) string {
	t.Helper()
	if err := st.SeedGlobalMemories(context.Background()); err != nil {
		t.Fatalf("SeedGlobalMemories: %v", err)
	}
	rows, err := st.ListMemories(context.Background(), memory.GlobalProjectID, "", "", 10)
	if err != nil {
		t.Fatalf("ListMemories: %v", err)
	}
	for _, m := range rows {
		if m.Source == "builtin" {
			return m.Content
		}
	}
	t.Fatal("the store seeded no builtin global row, so there is no shipped seed text to copy")
	return ""
}

func TestEveryProjectContextSurfaceAnswersAnUnresolvedProject(t *testing.T) {
	srv, session := newValiditySession(t)
	const wanted = "no-such-project-anywhere"
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ghost_save_global",
		Arguments: map[string]any{"content": "a cross-project preference", "category": "preference"},
	}); err != nil {
		t.Fatalf("save_global: %v", err)
	}

	assertAnswered := func(surface, out string) {
		t.Helper()
		if !strings.Contains(out, "is not registered with Ghost yet") {
			t.Errorf("%s did not answer the not-registered sentence:\n%s", surface, out)
		}
		if !strings.Contains(out, wanted) {
			t.Errorf("%s did not NAME the project the caller asked for (want %q); quoting the resolved id would say "+
				`Project "" is not registered, which names nothing they can act on:`+"\n%s", surface, wanted, out)
		}
		if strings.Contains(out, "## Memories") {
			t.Errorf("%s rendered a memory listing for a project that does not exist; those rows belong to a "+
				"different project:\n%s", surface, out)
		}
		if !strings.Contains(out, "## Global (applies to all projects)") {
			t.Errorf("%s dropped the cross-project section for an unknown project; it does not depend on one, and a "+
				"first session in a project Ghost has never seen is when it matters most:\n%s", surface, out)
		}
		if !strings.Contains(out, "a cross-project preference") {
			t.Errorf("%s did not deliver the cross-project row:\n%s", surface, out)
		}
	}

	assertAnswered("ghost_project_context", resultText(callTool(t, session, "ghost_project_context",
		map[string]any{"project_id": wanted})))

	rr, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{
		URI: "ghost://project/" + wanted + "/context",
	})
	if err != nil {
		t.Fatalf("ReadResource context: %v", err)
	}
	if len(rr.Contents) != 1 {
		t.Fatalf("expected 1 resource content, got %d", len(rr.Contents))
	}
	assertAnswered("the context RESOURCE", rr.Contents[0].Text)

	pr, err := session.GetPrompt(context.Background(), &mcp.GetPromptParams{
		Name:      "recall_project",
		Arguments: map[string]string{"project_id": wanted},
	})
	if err != nil {
		t.Fatalf("GetPrompt recall_project: %v", err)
	}
	if len(pr.Messages) != 1 {
		t.Fatalf("expected 1 prompt message, got %d", len(pr.Messages))
	}
	tc, ok := pr.Messages[0].Content.(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", pr.Messages[0].Content)
	}
	assertAnswered("the recall_project PROMPT", tc.Text)
	_ = srv
}

func TestTheToolRefusesAnAsOfRequestForAnUnresolvedProject(t *testing.T) {
	_, session := newValiditySession(t)
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ghost_save_global",
		Arguments: map[string]any{"content": "a cross-project preference", "category": "preference"},
	}); err != nil {
		t.Fatalf("save_global: %v", err)
	}
	const wanted = "no-such-project-as-of"

	current := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": wanted}))
	historical := resultText(callTool(t, session, "ghost_project_context", map[string]any{
		"project_id": wanted, "as_of": asOfToolFuture,
	}))

	if !strings.Contains(current, "## Global (applies to all projects)") ||
		!strings.Contains(current, "a cross-project preference") {
		t.Errorf("the present-tense read dropped the cross-project section:\n%s", current)
	}
	if strings.Contains(current, "## Memories") {
		t.Errorf("the present-tense read rendered a memory listing for a project that does not exist:\n%s", current)
	}

	if !strings.Contains(historical, "is not registered with Ghost yet") {
		t.Errorf("the as_of read did not answer the not-registered sentence:\n%s", historical)
	}
	if !strings.Contains(historical, asOfToolFuture) {
		t.Errorf("the as_of read does not name the instant, so the reader cannot tell the requested instant was never "+
			"consulted:\n%s", historical)
	}
	if strings.Contains(historical, "a cross-project preference") {
		t.Errorf("the as_of read returned a present-tense row; a caller who asked for an instant cannot be handed "+
			"today's rows with nothing in the payload saying so:\n%s", historical)
	}
	if strings.Contains(historical, "## Memories") {
		t.Errorf("the as_of read rendered a memory listing for a project that does not exist:\n%s", historical)
	}
	if current == historical {
		t.Errorf("the two branches answer identically, which was the first fix's mistake: an as_of caller must not be "+
			"handed a present-tense block.\n%s", current)
	}
	for _, c := range []struct{ name, out string }{
		{"the current read", current}, {"the as_of read", historical},
	} {
		if !strings.Contains(c.out, wanted) {
			t.Errorf("%s did not name the project the caller asked for:\n%s", c.name, c.out)
		}
	}
}

func TestAnUnresolvedProjectNameDoesNotHideABrokenStore(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	blind := connectedClient(t, New(notHistoryCapable{newValidityStore(t)}, logger, "test"))

	res := callTool(t, blind, "ghost_project_context", map[string]any{
		"project_id": "vproj", "as_of": asOfToolFuture,
	})
	if !res.IsError || !strings.Contains(resultText(res), "cannot read its own history") {
		t.Fatalf("fixture precondition: a store that cannot read history must say so for a resolved project, got %q",
			resultText(res))
	}

	unres := callTool(t, blind, "ghost_project_context", map[string]any{
		"project_id": "no-such-project-broken-store", "as_of": asOfToolFuture,
	})
	if !unres.IsError || !strings.Contains(resultText(unres), "cannot read its own history") {
		t.Errorf("an unresolved project name hid the store's missing-history diagnostic and reported the "+
			"not-registered sentence instead: %q", resultText(unres))
	}
}

type notHistoryCapable struct{ provider.MemoryStore }

func TestProjectContextLoadDoesNotScaleWithStoreSize(t *testing.T) {
	if testing.Short() {
		t.Skip("latency measurement: skipped in -short")
	}
	run := func(n int) (time.Duration, int) {
		srv := perfProjectContextStore(t, n)
		ctx := context.Background()
		for i := 0; i < 2; i++ {
			if _, err := srv.buildProjectContext(ctx, "pcperf", "", searchResponseMaxBytes); err != nil {
				t.Fatalf("buildProjectContext: %v", err)
			}
		}
		var total time.Duration
		admitted := 0
		for i := 0; i < 20; i++ {
			start := time.Now()
			text, err := srv.buildProjectContext(ctx, "pcperf", "", searchResponseMaxBytes)
			total += time.Since(start)
			if err != nil {
				t.Fatalf("buildProjectContext: %v", err)
			}
			admitted = strings.Count(text, "\n- [")
		}
		return total / 20, admitted
	}
	small, smallAdmitted := run(100)
	large, largeAdmitted := run(1000)
	t.Logf("project context: 100 memories %s (%d lines), 1000 memories %s (%d lines), ratio %.2fx for 10x the rows",
		small, smallAdmitted, large, largeAdmitted, float64(large)/float64(small))
	if small <= 0 {
		t.Fatalf("implausible measurement at 100 memories: %s", small)
	}
	const caps = 20 + 15
	if smallAdmitted > caps || largeAdmitted > caps {
		t.Errorf("the block carried %d (100 memories) and %d (1000) lines; the caps bound it at %d, so the "+
			"window is no longer bounded and the read is a store scan", smallAdmitted, largeAdmitted, caps)
	}
	if ratio := float64(large) / float64(small); ratio > 5 {
		t.Errorf("the project-context load grew %.2fx for a 10x larger store, which is the whole-store read this "+
			"path must not do (100 memories %s, 1000 memories %s)", ratio, small, large)
	}
}

func perfProjectContextStore(t *testing.T, n int) *Server {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	st := memory.NewStore(db, logger)
	ctx := context.Background()
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`); err != nil {
		t.Fatalf("insert _global project: %v", err)
	}
	if err := st.EnsureProject(ctx, "pcperf", t.TempDir(), "pcperf"); err != nil {
		t.Fatalf("EnsureProject pcperf: %v", err)
	}
	for i := 0; i < n+120; i++ {
		project := "pcperf"
		if i >= n {
			project = memory.GlobalProjectID
		}
		id := "perf" + twoDigits((i/100)%100) + twoDigits((i/10)%10) + twoDigits(i%10)
		if _, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance, tags, created_at, updated_at)
		                       VALUES (?, ?, 'preference', ?, 'manual', 0.7, '[]', ?, ?)`,
			id, project, "performance row "+id, "2026-01-01 00:00:00", "2026-01-01 00:00:00"); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	return New(st, logger, "test")
}

// TestAProjectWhoseOwnRowsAreAllWithheldIsToldSoBesideItsLearnedContext is #788,

// same union bug #788 is about and it predates this PR.

func TestAResolvedRowIsNotAPermanentlyEmptyProject(t *testing.T) {
	st := newValidityStore(t)
	srv, session := validityServerFor(t, st)
	ctx := context.Background()
	if _, err := st.CreateWithIDFromCorpus(ctx, "vproj", "withdrawn", memory.Memory{
		Category: "fact", Content: "a row ghost resolve withdrew", Source: "manual", Importance: 0.9,
	}); err != nil {
		t.Fatalf("seed the row: %v", err)
	}
	stampedIDs, err := st.MarkResolved(ctx, "vproj", []string{"withdrawn"}, memory.Provenance{})
	if err != nil {
		t.Fatalf("MarkResolved: %v", err)
	}
	if len(stampedIDs) != 1 || stampedIDs[0] != "withdrawn" {
		t.Fatalf("fixture: MarkResolved stamped %v, so this is not the state the passive fetch filters on", stampedIDs)
	}
	stamped, err := st.ListMemories(ctx, "vproj", "", "", 10)
	if err != nil {
		t.Fatalf("ListMemories: %v", err)
	}
	if len(stamped) != 1 || stamped[0].ResolvedAt == nil {
		t.Fatalf("fixture: MarkResolved declined the row (%d rows, resolved_at %v), so this is not the state the "+
			"passive fetch filters on", len(stamped), func() any {
			if len(stamped) == 1 {
				return stamped[0].ResolvedAt
			}
			return nil
		}())
	}

	res, err := srv.projectContextMemories(ctx, "vproj", projectContextMemoriesCap)
	if err != nil {
		t.Fatalf("projectContextMemories vproj: %v", err)
	}
	if len(res.Items) != 0 {
		t.Fatalf("fixture: %d rows were admitted, so the fetch did not filter this row", len(res.Items))
	}
	if res.Outcome != assemble.OutcomeEmpty || res.Reason != assemble.ReasonNoMemories {
		t.Fatalf("fixture: got outcome %q reason %q, want empty/%q — a resolved row is filtered by the FETCH, so no "+
			"stage withheld anything and the reason is an empty window", res.Outcome, res.Reason, assemble.ReasonNoMemories)
	}
	if n, err := st.CountMemories(ctx, "vproj"); err != nil || n != 1 {
		t.Fatalf("fixture: CountMemories says %d (err %v); the finding is that it and the window disagree", n, err)
	}
	if rows, err := st.ListMemories(ctx, "vproj", "", "", 10); err != nil || len(rows) != 1 {
		t.Fatalf("fixture: ghost_memories_list returns %d rows (err %v), so a sentence pointing there is pointing at "+
			"nothing", len(rows), err)
	}

	for name, block := range map[string]string{
		"ghost_project_context":        resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "vproj"})),
		"the project-context resource": mustProjectContext(t, srv, "vproj"),
	} {
		if strings.Contains(block, "nothing has been saved for it") ||
			strings.Contains(block, "No memories found for this project.") {
			t.Errorf("%s told a caller that nothing was ever saved for a project holding a row, which "+
				"ghost_memories_list returns:\n%s", name, block)
		}
		if !strings.Contains(block, "Ghost holds 1 memory for this project") {
			t.Errorf("%s reported an empty project without saying the row exists; the count already read for the gate "+
				"is what makes that sentence, and it was discarded:\n%s", name, block)
		}
		if n := strings.Count(block, "Ghost holds 1 memory for this project"); n > 1 {
			t.Errorf("%s gave the own-rows note %d times; an emptied block says it once:\n%s", name, n, block)
		}
	}
}

func TestAResolvedOwnRowIsNotBlamedForAnExpiredGlobalsRow(t *testing.T) {
	st := newValidityStore(t)
	srv, session := validityServerFor(t, st)
	ctx := context.Background()
	if _, err := st.CreateWithIDFromCorpus(ctx, "vproj", "withdrawn", memory.Memory{
		Category: "fact", Content: "a row ghost resolve withdrew", Source: "manual", Importance: 0.9,
	}); err != nil {
		t.Fatalf("seed the row: %v", err)
	}
	if stamped, err := st.MarkResolved(ctx, "vproj", []string{"withdrawn"}, memory.Provenance{}); err != nil ||
		len(stamped) != 1 {
		t.Fatalf("MarkResolved: %v (stamped %v), so the fixture is not the state the fetch filters on", err, stamped)
	}
	for i := 0; i < 2; i++ {
		if _, err := st.CreateWithIDFromCorpus(ctx, memory.GlobalProjectID, "aged"+twoDigits(i), memory.Memory{
			Category: "preference", Content: "an aged-out cross-project preference", Source: "manual",
			Importance: 0.9, ValidFrom: strPtr("2020-01-01"), ValidUntil: strPtr("2021-01-01"),
		}); err != nil {
			t.Fatalf("seed the aged-out global: %v", err)
		}
	}

	res, err := srv.projectContextMemories(ctx, "vproj", projectContextMemoriesCap)
	if err != nil {
		t.Fatalf("projectContextMemories vproj: %v", err)
	}
	if len(res.Items) != 0 {
		t.Fatalf("fixture: %d rows were admitted", len(res.Items))
	}
	if res.Outcome != assemble.OutcomeEmpty || res.Reason != "all_invalid" {
		t.Fatalf("fixture: got outcome %q reason %q, want empty/all_invalid — the aged-out globals are in the "+
			"window and stage 2 drops them, which is what makes the abstention reachable", res.Outcome, res.Reason)
	}
	if n, err := st.CountMemories(ctx, "vproj"); err != nil || n != 1 {
		t.Fatalf("fixture: CountMemories says %d (err %v), want 1 — the count has no resolved_at predicate", n, err)
	}

	for name, block := range map[string]string{
		"ghost_project_context":        resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "vproj"})),
		"the project-context resource": mustProjectContext(t, srv, "vproj"),
	} {
		for _, blame := range []string{"withheld as out of date", "nothing has been saved for it",
			"No memories found for this project."} {
			if strings.Contains(block, blame) {
				t.Errorf("%s told a caller %q, and the project's own row was withdrawn by ghost resolve rather than "+
					"retired — the expired rows in the window are _global's:\n%s", name, blame, block)
			}
		}
		if !strings.Contains(block, "Ghost holds 1 memory for this project") {
			t.Errorf("%s said nothing at all about a project holding a row:\n%s", name, block)
		}
	}
}

func mustProjectContext(t *testing.T, srv *Server, projectID string) string {
	t.Helper()
	body, err := srv.buildProjectContext(context.Background(), projectID, "", searchResponseMaxBytes)
	if err != nil {
		t.Fatalf("buildProjectContext %s: %v", projectID, err)
	}
	return body
}

// the silent first-contact refusal (#1235): a Claude memory file the guard
func TestTheProjectContextNamesARefusedImportFileOnce(t *testing.T) {
	ctx := context.Background()
	projPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projPath, 0o755); err != nil {
		t.Fatal(err)
	}

	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store := memory.NewStore(db, logger)
	if err := store.EnsureProject(ctx, "refproj", projPath, "refproj"); err != nil {
		t.Fatal(err)
	}

	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	memDir := filepath.Join(fakeHome, ".claude", "projects", claudeimport.EncodeProjectPath(projPath), "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memDir, "clean-note.md"), []byte("A plain project note with nothing sensitive in it."), 0o600); err != nil {
		t.Fatal(err)
	}
	const refusedName = "refused-note.md"
	const refusedText = "The service account value is ghp_0123456789abcdefghijklmnopqrstuvwxyzAB for CI"
	if err := os.WriteFile(filepath.Join(memDir, refusedName), []byte("Imported from a hostile file.\n\n"+refusedText), 0o600); err != nil {
		t.Fatal(err)
	}

	_, session := validityServerFor(t, store)
	answer := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "refproj"}))

	if got := strings.Count(answer, refusedName); got != 1 {
		t.Errorf("the refused file is named %d times, want exactly once:\n%s", got, answer)
	}
	if strings.Contains(answer, refusedText) {
		t.Errorf("the answer echoes the refused file's text:\n%s", answer)
	}
	if !strings.Contains(answer, "clean project note") && !strings.Contains(answer, "plain project note") {
		t.Errorf("the clean file did not import alongside the refusal:\n%s", answer)
	}
}

// regression test for the restored first-contact gate (#1235): the import runs
func TestTheProjectContextDoesNotReImportOnAProjectThatAlreadyHasMemories(t *testing.T) {
	ctx := context.Background()
	projPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projPath, 0o755); err != nil {
		t.Fatal(err)
	}

	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store := memory.NewStore(db, logger)
	if err := store.EnsureProject(ctx, "gateproj", projPath, "gateproj"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.Upsert(ctx, "gateproj", "fact", "a pre-existing memory", "mcp", 0.5, nil); err != nil {
		t.Fatal(err)
	}

	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	memDir := filepath.Join(fakeHome, ".claude", "projects", claudeimport.EncodeProjectPath(projPath), "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const fileName = "secret-note.md"
	if err := os.WriteFile(filepath.Join(memDir, fileName), []byte("Imported from a hostile file.\n\nAWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE and the deploy user is admin"), 0o600); err != nil {
		t.Fatal(err)
	}

	srv, session := validityServerFor(t, store)
	_ = srv
	answer := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "gateproj"}))

	if strings.Contains(answer, fileName) {
		t.Errorf("the gate was open on a non-empty project and the file was reported:\n%s", answer)
	}
	if got, _ := store.CountMemories(ctx, "gateproj"); got != 1 {
		t.Errorf("after the (gated-out) import: count = %d, want 1 (no new rows)", got)
	}
}

func TestProjectContextFirstContactImportNotifiesEmbeddingWorker(t *testing.T) {
	ctx := context.Background()
	projPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projPath, 0o755); err != nil {
		t.Fatal(err)
	}

	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store := memory.NewStore(db, logger)
	if err := store.EnsureProject(ctx, "importproj", projPath, "importproj"); err != nil {
		t.Fatal(err)
	}

	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	memDir := filepath.Join(fakeHome, ".claude", "projects", claudeimport.EncodeProjectPath(projPath), "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memDir, "clean-note.md"), []byte("A plain project note with nothing sensitive in it."), 0o600); err != nil {
		t.Fatal(err)
	}

	srv, session := validityServerFor(t, store)
	ch := make(chan string, 4)
	srv.SetEmbedder(&mockEmbedder{}, ch)

	answer := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "importproj"}))
	if !strings.Contains(answer, "plain project note") {
		t.Fatalf("the first-contact import did not store the row:\n%s", answer)
	}

	select {
	case got := <-ch:
		if got != "importproj" {
			t.Errorf("projectCh notified with %q, want %q", got, "importproj")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the first-contact import did not notify the embedding worker")
	}
}

func TestEveryProjectContextSurfaceAnswersAnUnregisteredProjectWithEmptyGlobalBucket(t *testing.T) {
	srv, session := newValiditySession(t)
	const wanted = "no-such-project-anywhere"

	assertSentenceOnly := func(surface, out string) {
		t.Helper()
		if !strings.Contains(out, "is not registered with Ghost yet") {
			t.Errorf("%s did not answer the not-registered sentence:\n%s", surface, out)
		}
		if !strings.Contains(out, wanted) {
			t.Errorf("%s did not NAME the project the caller asked for (want %q):\n%s", surface, wanted, out)
		}
		if strings.Contains(out, "No memories found for this project.") {
			t.Errorf("%s carried the empty census; an unresolved project with no global rows must be sentence-only:\n%s", surface, out)
		}
		if strings.Contains(out, "## Global (applies to all projects)") {
			t.Errorf("%s rendered a Global section for an empty bucket:\n%s", surface, out)
		}
		if strings.Contains(out, "## Memories") {
			t.Errorf("%s rendered a project memory listing for a project that does not exist:\n%s", surface, out)
		}
		if strings.Contains(out, "Ghost holds") {
			t.Errorf("%s carried the withheld-rows note, which is about a project that exists:\n%s", surface, out)
		}
	}

	assertSentenceOnly("ghost_project_context", resultText(callTool(t, session, "ghost_project_context",
		map[string]any{"project_id": wanted})))

	rr, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{
		URI: "ghost://project/" + wanted + "/context",
	})
	if err != nil {
		t.Fatalf("ReadResource context: %v", err)
	}
	if len(rr.Contents) != 1 {
		t.Fatalf("expected 1 resource content, got %d", len(rr.Contents))
	}
	assertSentenceOnly("the context RESOURCE", rr.Contents[0].Text)

	pr, err := session.GetPrompt(context.Background(), &mcp.GetPromptParams{
		Name:      "recall_project",
		Arguments: map[string]string{"project_id": wanted},
	})
	if err != nil {
		t.Fatalf("GetPrompt recall_project: %v", err)
	}
	if len(pr.Messages) != 1 {
		t.Fatalf("expected 1 prompt message, got %d", len(pr.Messages))
	}
	tc, ok := pr.Messages[0].Content.(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", pr.Messages[0].Content)
	}
	assertSentenceOnly("the recall_project PROMPT", tc.Text)
	_ = srv
}
