package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
)

// #960: repo-fact advisory must not fire on conventions/rules with reasons, but must fire on bare restatements.
func TestRepoFactHintExemptionsAndFlags(t *testing.T) {
	for _, tc := range []struct {
		name, category, content string
		want                    bool
	}{
		{"convention with reason", "convention", "Convention: the schema version is declared in internal/memory/schema.go, because an older build's store is migrated on open rather than read as it stands.", false},
		{"decision rule no connector", "decision", "Decision: the repository-fact hint always reads the same two patterns, and the pair is defined in internal/mcpserver/durable.go beside the save path it judges.", false},
		{"gotcha deliberate", "gotcha", "Gotcha: the sync check is defined in internal/mcpserver/ensure_project.go on purpose, because a bound path is the only address a session has.", false},
		{"reason in later sentence", "convention", "The rule half is defined in internal/mcpserver/durable.go, and the advisory is appended to the save response. It reads the whole note, because a reason can sit in a sentence other than the one naming the file.", false},
		{"issue bad example", "fact", "foo.go contains HandleFoo()", true},
		{"bare defines", "fact", "internal/reflection/prompt.go defines the drop guard.", true},
		{"bare lives in", "fact", "The restore helper is in internal/memory/backup.go.", true},
		{"bare restatement as convention", "convention", "The restore helper is in internal/memory/backup.go.", true},
		{"rule word not exempt in fact", "fact", "Decision: the repository-fact hint always reads the same two patterns, and the pair is defined in internal/mcpserver/durable.go beside the save path it judges.", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if (repoFactHint(tc.content, tc.category) != "") != tc.want {
				t.Fatalf("repoFactHint(%q,%q) want=%v", tc.content, tc.category, tc.want)
			}
		})
	}
	for _, category := range []string{"convention", "decision", "gotcha"} {
		if repoFactHint("Decision: the repository-fact hint always reads the same two patterns, and the pair is defined in internal/mcpserver/durable.go beside the save path it judges.", category) != "" {
			t.Fatalf("rule word should exempt in %s", category)
		}
	}
}

func TestASavedConventionThatNamesAFileDrawsNoAdvisory(t *testing.T) {
	srv, session := newCapSession(t)
	for _, tc := range []struct {
		name, category, content string
		wantAdvisory            bool
	}{
		{"convention with reason", "convention", "Convention: the schema version is declared in internal/memory/schema.go, because an older build's store is migrated on open rather than read as it stands.", false},
		{"fact with reason connector", "fact", "Gotcha: the sync check is defined in internal/mcpserver/ensure_project.go on purpose, because a bound path is the only address a session has.", false},
		{"bare restatement", "fact", "The restore helper is in internal/memory/backup.go.", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "test-project", "content": tc.content, "category": tc.category})
			resp := resultText(res)
			if !strings.Contains(resp, "Memory saved (id: ") || strings.Contains(resp, "ADVISORY") != tc.wantAdvisory {
				t.Fatalf("unexpected response: %q", resp)
			}
			id, _ := extractID(resp)
			mems, err := srv.store.GetByIDs(context.Background(), []string{id})
			if err != nil || len(mems) != 1 || mems[0].Content != tc.content {
				t.Fatalf("stored row mismatch: err=%v mems=%v", err, mems)
			}
		})
	}
}

// #674 durable guidance golden contract.
var durableGuidanceGolden = []string{
	"durable knowledge", "survives the conversation",
	"Production schema changes require explicit approval.",
	"Deployment keeps database migrations separate from application rollout",
	"foo.go contains HandleFoo()",
}

func saveToolDescription(t *testing.T, session *mcp.ClientSession, tool string) string {
	t.Helper()
	tools, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools.Tools {
		if tl.Name == tool {
			return tl.Description
		}
	}
	t.Fatalf("%s missing", tool)
	return ""
}

func TestDurableGuidanceStatesTheRuleOnEverySaveSurface(t *testing.T) {
	_, session := newCapSession(t)
	surfaces := map[string]string{
		"mcpInstructions":   mcpInstructions,
		"ghost_memory_save": saveToolDescription(t, session, "ghost_memory_save"),
		"ghost_save_global": saveToolDescription(t, session, "ghost_save_global"),
	}
	for name, text := range surfaces {
		for _, want := range durableGuidanceGolden {
			if !strings.Contains(text, want) {
				t.Errorf("%s missing %q", name, want)
			}
		}
	}
}

func TestSaveGuidanceSaysGhostOnlyGuides(t *testing.T) {
	_, session := newCapSession(t)
	for name, text := range map[string]string{
		"mcpInstructions": mcpInstructions, "ghost_memory_save": saveToolDescription(t, session, "ghost_memory_save"), "ghost_save_global": saveToolDescription(t, session, "ghost_save_global"),
	} {
		low := strings.ToLower(text)
		if !strings.Contains(low, "never refuse") && !strings.Contains(low, "never rejects") {
			t.Errorf("%s does not state guidance-only", name)
		}
	}
}

func TestRepoFactHintRecognisesOnlyContainmentClaims(t *testing.T) {
	for _, content := range []string{
		"foo.go contains HandleFoo()", "internal/reflection/prompt.go defines the drop guard.",
		"The Restore path in backup.go is where every copy of the store is written.",
		"`Store.UpsertWithOptions` is where the fold target is decided.",
		"HandleFoo() lives in internal/foo/bar.go. The rule it enforces is that deploys need sign-off.",
	} {
		if repoFactHint(content, "fact") == "" {
			t.Errorf("expected hint for %q", content)
		}
	}
	for _, content := range []string{
		"Production schema changes require explicit approval.",
		"Deployment keeps database migrations separate from application rollout, on purpose.",
		"The session-start digest contains the scope label. It is printed by internal/mcpinit/hook.go.",
		"The fold target is decided by the store. `Store.UpsertWithOptions` owns the transaction that decides it.",
		"cmd/ghost/lifecycle.go calls os.Exit, so the tier switch is unreachable from a test and has to be pinned at the seam.",
		"The linker imports nothing from the harness package.",
		"SSH to the bastion goes through port 2222, not 22.",
		"This rule is defined by U.S. regulators for production compliance.",
		"Bob.Smith is where escalations are routed after hours.",
		"Node.js is where the build tooling lives for the frontend, per team convention.",
		"The release manager is named per quarter, and Terraform.State is where locks are held.",
		"Escalation contacts are defined in the on-call rotation, not in Jane.Doe's calendar.",
		"",
	} {
		if repoFactHint(content, "fact") != "" {
			t.Errorf("unexpected hint for %q", content)
		}
	}
}

func TestSaveAdvisesAndStillStoresARepositoryFact(t *testing.T) {
	srv, session := newCapSession(t)
	content := "internal/mcpserver/mcpserver.go contains HandleFoo()"
	resp, stored := saveAndFetchContent(t, srv, session, content)
	if !strings.Contains(resp, "ADVISORY") || !strings.Contains(stored, content) || !strings.Contains(resp, "Memory saved (id: ") || !strings.Contains(resp, "Stored anyway") {
		t.Fatalf("unexpected response/storage: %q %q", resp, stored)
	}
}

func TestGlobalSaveAdvisesToo(t *testing.T) {
	_, session := newCapSession(t)
	for _, tc := range []struct {
		content      string
		wantAdvisory bool
	}{
		{"foo.go contains HandleFoo()", true},
		{"Always use 2-space YAML indentation", false},
	} {
		res := callTool(t, session, "ghost_save_global", map[string]any{"content": tc.content, "category": "fact"})
		resp := resultText(res)
		if !strings.Contains(resp, "Global memory saved (id: ") || strings.Contains(resp, "ADVISORY") != tc.wantAdvisory {
			t.Fatalf("unexpected response: %q", resp)
		}
		found := callTool(t, session, "ghost_memory_search", map[string]any{"project_id": "test-project", "query": tc.content, "limit": 5})
		if !strings.Contains(resultText(found), "«"+tc.content+"»") {
			t.Fatalf("not stored/found: %q", resultText(found))
		}
	}
}

func TestUpdateAdvisesOnAContentRewrite(t *testing.T) {
	srv, session := newCapSession(t)
	id, err := srv.store.Create(context.Background(), "abc123", memory.Memory{Category: "fact", Content: "an older note about the same subject", Source: "mcp", Importance: 0.7, Tags: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name         string
		args         map[string]any
		wantAdvisory bool
	}{
		{"repo fact", map[string]any{"content": "foo.go contains HandleFoo()"}, true},
		{"durable rule", map[string]any{"content": "Production schema changes require explicit approval."}, false},
		{"tag only", map[string]any{"tags": []any{"a", "b"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.args["project_id"], tc.args["memory_id"] = "test-project", id
			resp := resultText(callTool(t, session, "ghost_memory_update", tc.args))
			if !strings.Contains(resp, "Memory updated (id: "+id+")") || strings.Contains(resp, "ADVISORY") != tc.wantAdvisory {
				t.Fatalf("unexpected response: %q", resp)
			}
		})
	}
}

func TestDurableSavesCarryNoAdvisory(t *testing.T) {
	srv, session := newCapSession(t)
	for _, content := range []string{
		"Production schema changes require explicit approval.",
		"Deployment keeps database migrations separate from application rollout, on purpose.",
		"cmd/ghost/lifecycle.go calls os.Exit, so the tier switch is unreachable from a test and has to be pinned at the seam.",
		"The session-start digest contains the scope label. It is printed by internal/mcpinit/hook.go.",
	} {
		resp, stored := saveAndFetchContent(t, srv, session, content)
		if strings.Contains(resp, "ADVISORY") || stored != content {
			t.Fatalf("unexpected: %q %q", resp, stored)
		}
	}
}

func TestUpdateJudgesAnOmittedCategoryAgainstTheStoredRow(t *testing.T) {
	srv, session := newCapSession(t)
	const content = "The hint is always defined in internal/mcpserver/durable.go."
	for _, tc := range []struct {
		name, stored string
		wantAdvisory bool
	}{
		{"stored convention", "convention", false},
		{"stored fact", "fact", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, err := srv.store.Create(context.Background(), "abc123", memory.Memory{Category: tc.stored, Content: "an older note about the same subject", Source: "mcp", Importance: 0.7, Tags: []string{}})
			if err != nil {
				t.Fatal(err)
			}
			resp := resultText(callTool(t, session, "ghost_memory_update", map[string]any{"project_id": "test-project", "memory_id": id, "content": content}))
			if !strings.Contains(resp, "Memory updated (id: "+id+")") || strings.Contains(resp, "ADVISORY") != tc.wantAdvisory {
				t.Fatalf("unexpected response: %q", resp)
			}
		})
	}
}

// #1953 dated global status heuristics.
func TestGlobalStatusHintMatchesDateInsideFirstSentence(t *testing.T) {
	srv, session := newCapSession(t)
	for _, tc := range []struct{ name, content string }{
		{"on date", "On 2026-10-16, the staging cluster was rotated to new credentials."},
		{"embedded date", "ghost v1.4.0 (released 2026-10-16, includes the migration fix)"},
		{"colon form", "ghost (2026-10-16): is now running the patched scheduler in production."},
		{"as of", "The production indexer is healthy again as of 2026-10-16."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := callTool(t, session, "ghost_save_global", map[string]any{"content": tc.content, "category": "fact"})
			resp := resultText(res)
			if !strings.Contains(resp, "Global memory saved (id: ") || !strings.Contains(resp, "ADVISORY") || !strings.Contains(resp, "session start") {
				t.Fatalf("unexpected response: %q", resp)
			}
			id, _ := extractID(resp)
			mems, err := srv.store.GetByIDs(context.Background(), []string{id})
			if err != nil || len(mems) != 1 || mems[0].Content != tc.content {
				t.Fatalf("stored mismatch")
			}
		})
	}
}

func TestGlobalStatusHintKeepsAttributedPreferencesQuiet(t *testing.T) {
	_, session := newCapSession(t)
	for _, content := range []string{
		"Wayne, 2026-10-16: prefer tabs over spaces",
		"Wayne (2026-10-16): always run targeted package tests first",
		"Wayne (2026-10-16): never push without review",
		"Wayne (2026-10-16): is now running the patched scheduler in production.",
	} {
		res := callTool(t, session, "ghost_save_global", map[string]any{"content": content, "category": "preference"})
		resp := resultText(res)
		if !strings.Contains(resp, "Global memory saved (id: ") {
			t.Fatalf("unexpected response: %q", resp)
		}
	}
	res := callTool(t, session, "ghost_save_global", map[string]any{"content": "Wayne (2026-10-16): is now running the patched scheduler in production.", "category": "fact"})
	if !strings.Contains(resultText(res), "ADVISORY") {
		t.Fatalf("expected advisory for attributed plain status")
	}
}

func TestGlobalStatusHintBoundaries(t *testing.T) {
	_, session := newCapSession(t)
	content := strings.Repeat("durable guidance note ", 10) + "recorded for reference 2026-10-16 only"
	resp := resultText(callTool(t, session, "ghost_save_global", map[string]any{"content": content, "category": "fact"}))
	if strings.Contains(resp, "ADVISORY") {
		t.Fatalf("date outside scan window must not advise: %q", resp)
	}
	at115 := "On " + strings.Repeat("x", 111) + " 2026-10-16: status update"
	if globalStatusHint(at115, nil) == "" {
		t.Fatal("date at 115 should advise")
	}
	at121 := "On " + strings.Repeat("x", 117) + " 2026-10-16: status update"
	if globalStatusHint(at121, nil) != "" {
		t.Fatal("date at 121 should not advise")
	}
}

// #1832 long memory advisories and compositions.
func TestLongMemoryHintAndCompositions(t *testing.T) {
	srv, session := newCapSession(t)
	at := strings.Repeat("a", 800)
	resp, stored := saveAndFetchContent(t, srv, session, at)
	if strings.Contains(resp, "ADVISORY") || stored != at {
		t.Fatalf("at threshold unexpected: %q", resp)
	}
	over := strings.Repeat("a", 801)
	resp, stored = saveAndFetchContent(t, srv, session, over)
	if !strings.Contains(resp, "ADVISORY") || !strings.Contains(resp, "801 bytes") || stored != over {
		t.Fatalf("over threshold unexpected: %q", resp)
	}
	longRepoFact := strings.Repeat("x", 500) + " foo.go contains HandleFoo() " + strings.Repeat("y", 400)
	resp = resultText(callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "test-project", "content": longRepoFact, "category": "fact"}))
	if !strings.Contains(resp, "bytes") || !strings.Contains(resp, "repository fact") {
		t.Fatalf("missing composed advisories: %q", resp)
	}
	resp = resultText(callTool(t, session, "ghost_save_global", map[string]any{"content": "2026-10-16: System is down for maintenance", "category": "fact"}))
	if !strings.Contains(resp, "ADVISORY") {
		t.Fatalf("dated global should advise: %q", resp)
	}
	resp = resultText(callTool(t, session, "ghost_save_global", map[string]any{"content": "2026-11-01: Deployment window opens", "category": "fact", "valid_until": "2026-11-02T00:00:00Z"}))
	if strings.Contains(resp, "ADVISORY") {
		t.Fatalf("dated global with valid_until should not advise: %q", resp)
	}
	res := callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "test-project", "content": strings.Repeat("x", 9000), "category": "fact"})
	resp = resultText(res)
	if !strings.Contains(resp, "ADVISORY") || !strings.Contains(resp, "8000 bytes") || !strings.Contains(resp, "WARNING") || strings.Index(resp, "ADVISORY") > strings.Index(resp, "WARNING") {
		t.Fatalf("clamped advisory/warning issue: %q", resp)
	}
	id, _ := extractID(resp)
	mems, err := srv.store.GetByIDs(context.Background(), []string{id})
	if err != nil || len(mems) != 1 || !strings.Contains(mems[0].Content, "truncated at 8000 bytes") {
		t.Fatalf("clamped stored content unexpected")
	}
	_ = srv
}

func TestPromoteAdvisories(t *testing.T) {
	srv, session := newCapSession(t)
	ctx := context.Background()
	// #1953 embedded date on promote
	id1, _ := srv.store.Create(ctx, "abc123", memory.Memory{Category: "fact", Content: "On 2026-10-16, deployments were paused for the release freeze.", Source: "mcp", Importance: 0.7, Tags: []string{}})
	resp := resultText(callTool(t, session, "ghost_memory_promote", map[string]any{"project_id": "test-project", "memory_id": id1}))
	if !strings.Contains(resp, "ADVISORY") || !strings.Contains(resp, "session start") {
		t.Fatalf("promote embedded-date should advise: %q", resp)
	}
	id2, _ := srv.store.Create(ctx, "abc123", memory.Memory{Category: "fact", Content: "foo.go contains HandleFoo()", Source: "mcp", Importance: 0.7, Tags: []string{}})
	resp = resultText(callTool(t, session, "ghost_memory_promote", map[string]any{"project_id": "test-project", "memory_id": id2}))
	if !strings.Contains(resp, "repository fact") {
		t.Fatalf("promote repo fact should advise: %q", resp)
	}
	longRepoFact := strings.Repeat("x", 500) + " foo.go contains HandleFoo() " + strings.Repeat("y", 400)
	id3, _ := srv.store.Create(ctx, "abc123", memory.Memory{Category: "fact", Content: longRepoFact, Source: "mcp", Importance: 0.7, Tags: []string{}})
	resp = resultText(callTool(t, session, "ghost_memory_promote", map[string]any{"project_id": "test-project", "memory_id": id3}))
	if !strings.Contains(resp, "bytes") || !strings.Contains(resp, "repository fact") {
		t.Fatalf("promote composed advisories missing: %q", resp)
	}
}
