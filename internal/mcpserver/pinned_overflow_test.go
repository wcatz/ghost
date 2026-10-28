package mcpserver

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
)

func seedPinned(t *testing.T, s *memory.Store, project string, n int, contentLen int, importance float32) []string {
	t.Helper()
	ctx := context.Background()
	// Ensure project exists for _global tests (testStore only creates abc123)
	_ = s.EnsureProject(ctx, project, "/tmp/"+project, project)
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		content := strings.Repeat("x", contentLen)
		if contentLen == 0 {
			content = fmt.Sprintf("content %03d", i)
		}
		id, err := s.Create(ctx, project, memory.Memory{Category: "fact", Content: content, Source: "manual", Importance: importance})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := s.TogglePinInProject(ctx, project, id, true); err != nil {
			t.Fatalf("TogglePin: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

func TestPinOverflowAtCapReportsNone(t *testing.T) {
	store := testStore(t)
	srv := New(store, nil, "test")
	session := connectedClient(t, srv)
	ctx := context.Background()

	// Fill to cap-1, then pin via tool to reach exactly cap -> should report none
	for i := 0; i < memory.PinnedProjectCap-1; i++ {
		id, _ := store.Create(ctx, "abc123", memory.Memory{Category: "fact", Content: fmt.Sprintf("pin %03d", i), Source: "manual", Importance: 0.7})
		_ = store.TogglePinInProject(ctx, "abc123", id, true)
	}
	id, _ := store.Create(ctx, "abc123", memory.Memory{Category: "fact", Content: "extra", Source: "manual", Importance: 0.7})
	res := callTool(t, session, "ghost_memory_pin", map[string]any{"project_id": "test-project", "memory_id": id, "pinned": true})
	txt := pinnedToolText(t, res)
	if strings.Contains(txt, "exceed the session-start bucket") || strings.Contains(txt, "at risk of being cut") {
		t.Fatalf("pin at cap should not report overflow, got %q", txt)
	}
	report := pinnedHealthText(t, srv)
	if strings.Contains(report, "exceed the session-start bucket") {
		t.Fatalf("health at cap should report none, got:\n%s", report)
	}
}

func TestPinOverflowCapPlusOneReportsOverflow(t *testing.T) {
	store := testStore(t)
	srv := New(store, nil, "test")
	session := connectedClient(t, srv)
	ctx := context.Background()
	// Create cap pinned
	for i := 0; i < memory.PinnedProjectCap; i++ {
		id, _ := store.Create(ctx, "abc123", memory.Memory{Category: "fact", Content: fmt.Sprintf("c %03d", i), Source: "manual", Importance: 0.7})
		_ = store.TogglePinInProject(ctx, "abc123", id, true)
	}
	// Create one more and pin via tool
	id, _ := store.Create(ctx, "abc123", memory.Memory{Category: "fact", Content: "overflow", Source: "manual", Importance: 0.1})
	res := callTool(t, session, "ghost_memory_pin", map[string]any{"project_id": "test-project", "memory_id": id, "pinned": true})
	txt := pinnedToolText(t, res)
	if !strings.Contains(txt, "pinned") {
		t.Fatalf("pin result missing pinned: %q", txt)
	}
	if !strings.Contains(txt, "lowest-ranked by importance") {
		t.Fatalf("pin overflow should name lowest-ranked by importance, got %q", txt)
	}
	if !strings.Contains(txt, "most at risk of being cut") {
		t.Fatalf("pin overflow should name most at risk of being cut, got %q", txt)
	}
	if !strings.Contains(txt, fmt.Sprintf("cap %d", memory.PinnedProjectCap)) {
		t.Fatalf("pin overflow should name cap %d, got %q", memory.PinnedProjectCap, txt)
	}
	// The notice must name exactly one id after the risk phrase
	if !strings.Contains(txt, "most at risk of being cut") {
		t.Fatalf("pin overflow should name risk phrase, got %q", txt)
	}
	// Find a quoted 32-hex id
	re := regexp.MustCompile(`"([0-9a-fA-F]{32})"`)
	matches := re.FindAllStringSubmatch(txt, -1)
	if len(matches) != 1 {
		t.Fatalf("cap+1 should name exactly one 32-hex quoted id, got %d in %q", len(matches), txt)
	}
	if strings.Contains(txt, " and ") {
		t.Fatalf("cap+1 should name exactly one id, got %q", txt)
	}
	if strings.Contains(txt, "bytes vs") {
		t.Fatalf("pin overflow should not name bytes vs share, got %q", txt)
	}
	// Also test save with pin=true
	store2 := testStore(t)
	srv2 := New(store2, nil, "test")
	session2 := connectedClient(t, srv2)
	for i := 0; i < memory.PinnedProjectCap; i++ {
		seedPinned(t, store2, "abc123", 1, 10, 0.7)
	}
	res2, err := session2.CallTool(ctx, &mcp.CallToolParams{Name: "ghost_memory_save", Arguments: map[string]any{"project_id": "test-project", "content": "new pin via save", "pin": true}})
	if err != nil {
		t.Fatalf("save pin: %v", err)
	}
	txt2 := pinnedToolText(t, res2)
	if !strings.Contains(txt2, "lowest-ranked by importance") || !strings.Contains(txt2, "most at risk of being cut") {
		t.Fatalf("save pin overflow should report risk with new wording, got %q", txt2)
	}
	// Health lists the same project
	report := pinnedHealthText(t, srv)
	if !strings.Contains(report, "exceed the session-start bucket") {
		t.Fatalf("health should list overflow, got:\n%s", report)
	}
	if !strings.Contains(report, fmt.Sprintf("cap %d", memory.PinnedProjectCap)) {
		t.Fatalf("health should name cap, got:\n%s", report)
	}
	// Check exact cap reports none for health
	store3 := testStore(t)
	srv3 := New(store3, nil, "test")
	for i := 0; i < memory.PinnedProjectCap; i++ {
		seedPinned(t, store3, "abc123", 1, 10, 0.7)
	}
	report3 := pinnedHealthText(t, srv3)
	if strings.Contains(report3, "exceed the session-start bucket") {
		t.Fatalf("health at exact cap should report none, got:\n%s", report3)
	}
	// _global at cap
	store4 := testStore(t)
	srv4 := New(store4, nil, "test")
	for i := 0; i < memory.PinnedGlobalCap; i++ {
		seedPinned(t, store4, "_global", 1, 10, 0.7)
	}
	report4 := pinnedHealthText(t, srv4)
	if strings.Contains(report4, "exceed the session-start bucket") {
		t.Fatalf("health for _global at cap should report none, got:\n%s", report4)
	}
	store5 := testStore(t)
	srv5 := New(store5, nil, "test")
	for i := 0; i < memory.PinnedGlobalCap+1; i++ {
		seedPinned(t, store5, "_global", 1, 10, 0.7)
	}
	report5 := pinnedHealthText(t, srv5)
	if !strings.Contains(report5, "exceed the session-start bucket") {
		t.Fatalf("health for _global cap+1 should report overflow, got:\n%s", report5)
	}
}

func TestPinByteOverflowWithFewLongPins(t *testing.T) {
	store := testStore(t)
	srv := New(store, nil, "test")
	session := connectedClient(t, srv)
	ctx := context.Background()
	// 3 long pins each 1500 bytes under cap; byte-only overflow not reported
	ids := seedPinned(t, store, "abc123", 3, 1500, 0.7)
	report := pinnedHealthText(t, srv)
	if strings.Contains(report, "exceed the session-start bucket") {
		t.Fatalf("byte-only overflow should not report, got:\n%s", report)
	}
	// Pin result for a new long pin under cap
	id, _ := store.Create(ctx, "abc123", memory.Memory{Category: "fact", Content: strings.Repeat("y", 1500), Source: "manual", Importance: 0.1})
	res := callTool(t, session, "ghost_memory_pin", map[string]any{"project_id": "test-project", "memory_id": id, "pinned": true})
	txt := pinnedToolText(t, res)
	if strings.Contains(txt, "lowest-ranked by importance") || strings.Contains(txt, "most at risk of being cut") {
		t.Fatalf("byte-only overflow should not report cut, got %q", txt)
	}
	_ = ids
}

func pinnedHealthText(t *testing.T, srv *Server) string {
	t.Helper()
	ctx := context.Background()
	session := connectedClient(t, srv)
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ghost_health", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("ghost_health: %v", err)
	}
	var txt string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			txt += tc.Text
		}
	}
	return txt
}

func pinnedToolText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var txt string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			txt += tc.Text
		}
	}
	return txt
}
