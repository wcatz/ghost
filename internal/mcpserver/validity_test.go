package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
)

func pinAgent(t *testing.T, agent string) {
	t.Helper()
	old := detectCallingSource
	detectCallingSource = func() string { return agent }
	t.Cleanup(func() { detectCallingSource = old })
}

func savedMemory(t *testing.T, srv *Server, session *mcp.ClientSession, tool string, args map[string]any) memory.Memory {
	t.Helper()
	res := callTool(t, session, tool, args)
	if res.IsError {
		t.Fatalf("%s returned an error: %s", tool, resultText(res))
	}
	id, ok := extractID(resultText(res))
	if !ok || id == "" {
		t.Fatalf("%s response carries no memory id: %q", tool, resultText(res))
	}
	mems, err := srv.store.GetByIDs(context.Background(), []string{id})
	if err != nil || len(mems) != 1 {
		t.Fatalf("GetByIDs(%q): err=%v n=%d", id, err, len(mems))
	}
	return mems[0]
}

func stamp(t *testing.T, m memory.Memory, field string, got *string) string {
	t.Helper()
	if got == nil {
		t.Fatalf("%s.%s is NULL, want a recorded value", m.ID, field)
	}
	return *got
}

func datePtr(s string) *string { return &s }

func parseStampLikeReader(s string) (time.Time, bool) {
	for _, layout := range memory.StampLayouts {
		if at, err := time.Parse(layout, s); err == nil {
			return at, true
		}
	}
	return time.Time{}, false
}

func effectiveWindow(m memory.Memory) (string, bool) {
	if m.ValidFrom == nil || m.ValidUntil == nil {
		return "", false
	}
	from, fromOK := parseStampLikeReader(*m.ValidFrom)
	until, untilOK := parseStampLikeReader(*m.ValidUntil)
	if !fromOK || !untilOK {
		return "", false
	}
	return *m.ValidFrom + " \u2192 " + *m.ValidUntil, !until.After(from)
}

func TestSaveRecordsEveryValidityAndProvenanceField(t *testing.T) {
	srv, session := newCapSession(t)
	pinAgent(t, "opencode")
	m := savedMemory(t, srv, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project", "content": "the ledger cutover lands on 2026-12-01", "category": "fact",
		"valid_from": "2026-01-15", "valid_until": "2026-12-01", "verified_at": "2026-09-20",
		"confidence": 0.8, "source_ref": "docs/roadmap.md#L42",
	})
	for _, tc := range []struct {
		field string
		got   *string
		want  time.Time
	}{
		{"valid_from", m.ValidFrom, time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)},
		{"valid_until", m.ValidUntil, time.Date(2026, 12, 1, 23, 59, 59, 0, time.UTC)},
		{"verified_at", m.VerifiedAt, time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)},
	} {
		raw := stamp(t, m, tc.field, tc.got)
		at, err := time.Parse("2006-01-02 15:04:05", raw)
		if err != nil {
			t.Fatalf("%s = %q: %v", tc.field, raw, err)
		}
		if !at.Equal(tc.want) {
			t.Errorf("%s = %v, want %v", tc.field, at, tc.want)
		}
	}
	if m.Confidence == nil || *m.Confidence != 0.8 {
		t.Errorf("Confidence = %v, want 0.8", m.Confidence)
	}
	if m.SourceRef != "docs/roadmap.md#L42" {
		t.Errorf("SourceRef = %q", m.SourceRef)
	}
	if m.Agent != "opencode" {
		t.Errorf("Agent = %q, want opencode", m.Agent)
	}
}

func TestSaveAcceptsRFC3339AndVerifiedShortcut(t *testing.T) {
	srv, session := newCapSession(t)
	pinAgent(t, "opencode")
	m := savedMemory(t, srv, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project", "content": "the maintenance window opens at nine", "category": "fact",
		"valid_until": "2027-03-31T09:00:00Z",
	})
	raw := stamp(t, m, "valid_until", m.ValidUntil)
	at, err := time.Parse("2006-01-02 15:04:05", raw)
	if err != nil || !at.Equal(time.Date(2027, 3, 31, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("valid_until = %q (%v), want 2027-03-31 09:00:00Z", raw, err)
	}
	before := time.Now().UTC().Add(-time.Minute)
	v := savedMemory(t, srv, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project", "content": "the second gateway was re-checked today", "category": "fact", "verified": true,
	})
	after := time.Now().UTC().Add(time.Minute)
	vr := stamp(t, v, "verified_at", v.VerifiedAt)
	vat, err := time.Parse("2006-01-02 15:04:05", vr)
	if err != nil || vat.Before(before) || vat.After(after) {
		t.Fatalf("verified_at = %q %v", vr, err)
	}
	if v.ValidFrom != nil || v.ValidUntil != nil {
		t.Errorf("verified=true invented a window: from=%v until=%v", v.ValidFrom, v.ValidUntil)
	}
}

func TestSaveGlobalAndUpdateRecordFields(t *testing.T) {
	srv, session := newCapSession(t)
	pinAgent(t, "opencode")
	g := savedMemory(t, srv, session, "ghost_save_global", map[string]any{
		"content": "prefer tabs over spaces in Go", "category": "convention",
		"valid_from": "2026-01-01", "valid_until": "2030-01-01", "confidence": 0.95, "source_ref": "AGENTS.md",
	})
	if g.ProjectID != "_global" || g.SourceRef != "AGENTS.md" || g.Confidence == nil || *g.Confidence != 0.95 {
		t.Fatalf("global row not recorded as expected: %+v", g)
	}
	m := savedMemory(t, srv, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project", "content": "the port mapping moves to 8444", "category": "fact",
		"valid_from": "2026-01-01", "valid_until": "2031-01-01", "confidence": 0.7, "source_ref": "docs/policy.md",
	})
	res := callTool(t, session, "ghost_memory_update", map[string]any{
		"project_id": "test-project", "memory_id": m.ID, "content": "the retention window is fourteen days",
		"valid_until": "2027-02-01", "verified_at": "2026-10-05", "confidence": 0.4, "source_ref": "PR #123",
	})
	if res.IsError {
		t.Fatalf("update error: %s", resultText(res))
	}
	got, err := srv.store.GetByIDs(context.Background(), []string{m.ID})
	if err != nil || len(got) != 1 {
		t.Fatalf("GetByIDs: err=%v n=%d", err, len(got))
	}
	after := got[0]
	if after.Content != "the retention window is fourteen days" || after.SourceRef != "PR #123" || after.Agent != "opencode" {
		t.Errorf("unexpected after: %+v", after)
	}
	if after.ValidFrom == nil || *after.ValidFrom != *m.ValidFrom {
		t.Errorf("valid_from changed: got %v want %v", after.ValidFrom, m.ValidFrom)
	}
	if after.Confidence == nil || *after.Confidence != 0.4 || after.ValidUntil == nil || after.VerifiedAt == nil {
		t.Errorf("updated fields missing: %+v", after)
	}
}

func TestEveryWriterRejectsBadValidityAndConfidence(t *testing.T) {
	for _, tool := range []string{"ghost_memory_save", "ghost_save_global", "ghost_memory_update"} {
		t.Run(tool, func(t *testing.T) {
			srv, session := newCapSession(t)
			base := map[string]any{"content": "a claim", "category": "fact"}
			if tool != "ghost_save_global" {
				base["project_id"] = "test-project"
			}
			if tool == "ghost_memory_update" {
				base["memory_id"] = savedMemory(t, srv, session, "ghost_memory_save", map[string]any{
					"project_id": "test-project", "content": "seed", "category": "fact",
				}).ID
			}
			for name, extra := range map[string]map[string]any{
				"window":     {"valid_from": "2026-10-01", "valid_until": "2026-09-01"},
				"confidence": {"confidence": 1.5},
				"stamp":      {"valid_from": "next tuesday"},
			} {
				args := map[string]any{}
				for k, v := range base {
					args[k] = v
				}
				for k, v := range extra {
					args[k] = v
				}
				res := callTool(t, session, tool, args)
				if !res.IsError {
					t.Fatalf("%s %s accepted: %q", tool, name, resultText(res))
				}
			}
		})
	}
}

func TestUpdateRefusesWindowContradictingStored(t *testing.T) {
	for _, tc := range []struct {
		name       string
		first      map[string]any
		storeFirst memory.Validity
		update     map[string]any
		wantReject bool
	}{
		{name: "end before stored start", first: map[string]any{"valid_from": "2026-01-01"}, update: map[string]any{"valid_until": "2020-01-01"}, wantReject: true},
		{name: "start after stored end", first: map[string]any{"valid_until": "2026-06-01"}, update: map[string]any{"valid_from": "2027-01-01"}, wantReject: true},
		{name: "end before stored bare-date start", storeFirst: memory.Validity{ValidFrom: datePtr("2026-01-01")}, update: map[string]any{"valid_until": "2025-01-01"}, wantReject: true},
		{name: "valid end beside stored bare-date start", storeFirst: memory.Validity{ValidFrom: datePtr("2026-01-01")}, update: map[string]any{"valid_until": "2027-01-01"}, wantReject: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, session := newCapSession(t)
			var m memory.Memory
			if tc.storeFirst.ValidFrom != nil || tc.storeFirst.ValidUntil != nil {
				ctx := context.Background()
				project, _, _ := srv.store.ResolveProject(ctx, "test-project")
				id, err := srv.store.Create(ctx, project, memory.Memory{
					Category: "fact", Content: "a claim whose stored boundary is a bare date", Source: "mcp",
					ValidFrom: tc.storeFirst.ValidFrom, ValidUntil: tc.storeFirst.ValidUntil,
				})
				if err != nil {
					t.Fatal(err)
				}
				rows, _ := srv.store.GetByIDs(ctx, []string{id})
				m = rows[0]
			} else {
				first := map[string]any{"project_id": "test-project", "content": "a claim with half a window", "category": "fact"}
				for k, v := range tc.first {
					first[k] = v
				}
				m = savedMemory(t, srv, session, "ghost_memory_save", first)
			}
			edit := map[string]any{"project_id": "test-project", "memory_id": m.ID}
			for k, v := range tc.update {
				edit[k] = v
			}
			res := callTool(t, session, "ghost_memory_update", edit)
			if res.IsError != tc.wantReject {
				t.Fatalf("rejected=%v want %v: %q", res.IsError, tc.wantReject, resultText(res))
			}
			after, _ := srv.store.GetByIDs(context.Background(), []string{m.ID})
			if len(after) == 1 {
				if window, contradictory := effectiveWindow(after[0]); contradictory {
					t.Errorf("stored contradictory window: %s", window)
				}
			}
		})
	}
}

func TestSaveRejectsZeroWindowConfidenceEndsAndBoundsSourceRef(t *testing.T) {
	srv, session := newCapSession(t)
	pinAgent(t, "opencode")
	for _, args := range []map[string]any{
		{"project_id": "test-project", "content": "a", "category": "fact", "valid_from": "2026-10-01T00:00:00Z", "valid_until": "2026-10-01T00:00:00Z"},
		{"project_id": "test-project", "content": "b", "category": "fact", "source_ref": strings.Repeat("docs/", 400)},
	} {
		if res := callTool(t, session, "ghost_memory_save", args); !res.IsError {
			t.Fatalf("accepted bad args %#v", args)
		}
	}
	for _, want := range []float64{0, 1} {
		m := savedMemory(t, srv, session, "ghost_memory_save", map[string]any{
			"project_id": "test-project", "content": fmt.Sprintf("confidence-%v", want), "category": "fact", "confidence": want,
		})
		if m.Confidence == nil || *m.Confidence != want {
			t.Fatalf("confidence=%v got %v", want, m.Confidence)
		}
	}
	const token = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcdefghij"
	for _, tool := range []string{"ghost_memory_save", "ghost_save_global", "ghost_memory_update"} {
		args := map[string]any{"content": "c", "category": "fact", "source_ref": "https://example.com/" + token}
		if tool != "ghost_save_global" {
			args["project_id"] = "test-project"
		}
		if tool == "ghost_memory_update" {
			args["memory_id"] = savedMemory(t, srv, session, "ghost_memory_save", map[string]any{
				"project_id": "test-project", "content": "seed", "category": "fact",
			}).ID
		}
		if res := callTool(t, session, tool, args); !res.IsError {
			t.Fatalf("%s stored credential-shaped source_ref", tool)
		}
	}
}

func TestValidityRenderingAndSearchMarks(t *testing.T) {
	srv, session := newCapSession(t)
	pinAgent(t, "opencode")
	today := time.Now().UTC().Format("2006-01-02")
	m := savedMemory(t, srv, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project", "content": "the freeze holds for the rest of today", "category": "fact", "valid_until": today,
	})
	raw := stamp(t, m, "valid_until", m.ValidUntil)
	at, err := time.Parse(memory.StoredStampLayout, raw)
	if err != nil {
		t.Fatal(err)
	}
	end := time.Date(at.Year(), at.Month(), at.Day(), 23, 59, 59, 0, time.UTC)
	if !at.Equal(end) {
		t.Fatalf("valid_until = %v, want end of day", at)
	}
	if out := searchText(t, session, "freeze rest of today"); !strings.Contains(out, m.ID) {
		t.Fatalf("row withheld on its last day: %q", out)
	}
	expired := savedMemory(t, srv, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project", "content": "the legacy ingest endpoint is still the write path", "category": "fact",
		"valid_until": time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02"),
	})
	out := resultText(callTool(t, session, "ghost_memories_list", map[string]any{"project_id": "test-project"}))
	if !strings.Contains(out, expired.ID) || !strings.Contains(out, "expired") {
		t.Fatalf("browsing did not mark expired row: %q", out)
	}
}

func TestSaveStoresOnlyWhatCallerStated(t *testing.T) {
	srv, session := newCapSession(t)
	pinAgent(t, "opencode")
	bare := savedMemory(t, srv, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project", "content": "an ordinary fact with nothing dated about it", "category": "fact",
	})
	if bare.ValidFrom != nil || bare.ValidUntil != nil || bare.VerifiedAt != nil || bare.Confidence != nil || bare.SourceRef != "" || bare.SessionID != "" {
		t.Fatalf("bare save invented fields: %+v", bare)
	}
}
