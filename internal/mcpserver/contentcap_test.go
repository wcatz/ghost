package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
)

const (
	truncationMarkerLiteral  = " …[truncated at 8000 bytes]"
	truncationWarningLiteral = "WARNING: content was truncated at 8000 bytes"
)

func newCapSession(t *testing.T) (*Server, *mcp.ClientSession) {
	t.Helper()
	store := testStore(t)
	srv := New(store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test")
	return srv, connectedClient(t, srv)
}

func saveAndFetchContent(t *testing.T, srv *Server, session *mcp.ClientSession, content string) (string, string) {
	t.Helper()
	res := callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "test-project", "content": content, "category": "fact"})
	resp := resultText(res)
	id, ok := extractID(resp)
	if !ok {
		t.Fatalf("no id in %q", resp)
	}
	mems, err := srv.store.GetByIDs(context.Background(), []string{id})
	if err != nil || len(mems) != 1 {
		t.Fatalf("GetByIDs: %v %d", err, len(mems))
	}
	return resp, mems[0].Content
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func TestSaveContentCap(t *testing.T) {
	srv, session := newCapSession(t)
	content := strings.Repeat("incident timeline: every byte must survive. ", 114)[:5000]
	resp, stored := saveAndFetchContent(t, srv, session, content)
	if stored != content || strings.Contains(stored, truncationMarkerLiteral) || strings.Contains(resp, "truncated") {
		t.Fatalf("under cap failed: %q %q", resp, tail(stored, 40))
	}
	input := strings.Repeat("z", 12000)
	resp, stored = saveAndFetchContent(t, srv, session, input)
	want := strings.Repeat("z", 8000) + truncationMarkerLiteral
	if stored != want || !strings.Contains(resp, truncationWarningLiteral) {
		t.Fatalf("over cap failed: %q %q", resp, tail(stored, 40))
	}
	for _, tc := range []struct {
		n       int
		wantCut bool
	}{
		{8000, false}, {8001, true},
	} {
		srv2, session2 := newCapSession(t)
		resp, stored = saveAndFetchContent(t, srv2, session2, strings.Repeat("z", tc.n))
		if tc.wantCut {
			if stored != strings.Repeat("z", 8000)+truncationMarkerLiteral || !strings.Contains(resp, truncationWarningLiteral) {
				t.Fatalf("boundary cut failed")
			}
		} else if stored != strings.Repeat("z", 8000) || strings.Contains(resp, "truncated") {
			t.Fatalf("boundary exact failed")
		}
	}
	_ = srv
}

func TestUpdateTruncationIsExplicitAtCap(t *testing.T) {
	srv, session := newCapSession(t)
	id, err := srv.store.Create(context.Background(), "abc123", memory.Memory{Category: "fact", Content: "original", Source: "mcp", Importance: 0.7, Tags: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	resp := resultText(callTool(t, session, "ghost_memory_update", map[string]any{"project_id": "test-project", "memory_id": id, "content": strings.Repeat("u", 9000)}))
	mems, _ := srv.store.GetByIDs(context.Background(), []string{id})
	if mems[0].Content != strings.Repeat("u", 8000)+truncationMarkerLiteral || !strings.Contains(resp, truncationWarningLiteral) {
		t.Fatalf("update truncation failed: %q %q", resp, tail(mems[0].Content, 40))
	}
}

func TestSchemasNeverRejectContentUpToCap(t *testing.T) {
	_, session := newCapSession(t)
	tools, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*mcp.Tool{}
	for _, tool := range tools.Tools {
		byName[tool.Name] = tool
	}
	guards := map[string][]string{
		"ghost_memory_save": {"content"}, "ghost_memory_update": {"content"}, "ghost_save_global": {"content"},
		"ghost_task_create": {"description"}, "ghost_task_update": {"description"}, "ghost_decision_record": {"decision", "rationale"},
	}
	for name, props := range guards {
		raw, _ := json.Marshal(byName[name].InputSchema)
		var schema struct {
			Properties map[string]struct {
				MaxLength *int `json:"maxLength"`
			} `json:"properties"`
		}
		_ = json.Unmarshal(raw, &schema)
		for _, prop := range props {
			if f := schema.Properties[prop].MaxLength; f != nil && *f < memory.MaxContentLen {
				t.Fatalf("%s.%s maxLength %d < %d", name, prop, *f, memory.MaxContentLen)
			}
		}
	}
}

func TestGlobalToolDescriptionsMatchProvenanceContract(t *testing.T) {
	_, session := newCapSession(t)
	tools, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]string{}
	for _, tool := range tools.Tools {
		byName[tool.Name] = tool.Description
	}
	for _, name := range []string{"ghost_memory_promote", "ghost_save_global"} {
		d := byName[name]
		if strings.Contains(d, "replayed as trusted context") || !strings.Contains(d, "source") || !strings.Contains(d, "verify") {
			t.Fatalf("%s description unexpected: %q", name, d)
		}
	}
	if !strings.Contains(byName["ghost_save_global"], "source=mcp") {
		t.Fatalf("save_global missing source=mcp: %q", byName["ghost_save_global"])
	}
}

func memoryIDFromDecisionResp(t *testing.T, resp string) string {
	t.Helper()
	const marker = "(memory_id: "
	i := strings.Index(resp, marker)
	if i < 0 {
		t.Fatalf("no memory id: %q", resp)
	}
	rest := resp[i+len(marker):]
	j := strings.IndexByte(rest, ')')
	if j < 0 {
		t.Fatalf("unterminated memory id: %q", resp)
	}
	return rest[:j]
}

func TestTruncationWarningsNameTheirKind(t *testing.T) {
	for _, tc := range []struct {
		name    string
		call    func(*Server, *mcp.ClientSession) string
		want    string
		notWant []string
	}{
		{"memory save", func(srv *Server, s *mcp.ClientSession) string {
			resp, _ := saveAndFetchContent(t, srv, s, strings.Repeat("z", 9000))
			return resp
		},
			"WARNING: content was truncated at 8000 bytes; the stored text is incomplete — split it into focused memories or shorten it deliberately.", []string{"focused tasks", "rationale context"}},
		{"task create", func(_ *Server, s *mcp.ClientSession) string {
			return resultText(callTool(t, s, "ghost_task_create", map[string]any{"project_id": "test-project", "title": "cap test task", "description": strings.Repeat("t", 9000)}))
		}, "WARNING: task description was truncated at 8000 bytes; the stored text is incomplete — split it into focused tasks or shorten it deliberately.", []string{"focused memories", "rationale context"}},
		{"task complete", func(_ *Server, s *mcp.ClientSession) string {
			id := parseToolID(t, callTool(t, s, "ghost_task_create", map[string]any{"project_id": "test-project", "title": "cap test completion"}))
			return resultText(callTool(t, s, "ghost_task_complete", map[string]any{"task_id": id, "notes": strings.Repeat("n", 9000)}))
		}, "WARNING: task notes was truncated at 8000 bytes; the stored text is incomplete — split it into focused tasks or shorten it deliberately.", []string{"focused memories", "rationale context"}},
		{"decision record", func(_ *Server, s *mcp.ClientSession) string {
			return resultText(callTool(t, s, "ghost_decision_record", map[string]any{"project_id": "test-project", "title": "cap test decision", "decision": strings.Repeat("d", 9000), "rationale": "short rationale"}))
		}, "WARNING: decision text was truncated at 8000 bytes; the stored text is incomplete — shorten it, or move detail into the decision's rationale context.", []string{"focused memories", "focused tasks"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, session := newCapSession(t)
			resp := tc.call(srv, session)
			if !strings.Contains(resp, tc.want) {
				t.Fatalf("missing %q in %q", tc.want, resp)
			}
			for _, bad := range tc.notWant {
				if strings.Contains(resp, bad) {
					t.Fatalf("unexpected %q in %q", bad, resp)
				}
			}
		})
	}
}

func TestDecisionCompanionMemoryCapped(t *testing.T) {
	const title = "CapGuard"
	markerLen := len(truncationMarkerLiteral)
	companionWarning := "WARNING: decision companion memory was truncated at 8000 bytes"
	cases := []struct {
		name      string
		decision  string
		rationale string
		wantCut   bool
	}{
		{"sum over cap", strings.Repeat("d", 7000), strings.Repeat("r", 2000), true},
		{"both at cap", strings.Repeat("e", 8000), strings.Repeat("f", 8000), true},
		{"sub cap", "Use SQLite", "No external deps", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, session := newCapSession(t)
			resp := resultText(callTool(t, session, "ghost_decision_record", map[string]any{"project_id": "test-project", "title": title, "decision": tc.decision, "rationale": tc.rationale}))
			memID := memoryIDFromDecisionResp(t, resp)
			mems, err := srv.store.GetByIDs(context.Background(), []string{memID})
			if err != nil || len(mems) != 1 {
				t.Fatalf("GetByIDs: %v %d", err, len(mems))
			}
			stored := mems[0].Content
			if !tc.wantCut {
				want := fmt.Sprintf("%s: %s. Rationale: %s", title, tc.decision, tc.rationale)
				if stored != want || strings.Contains(stored, truncationMarkerLiteral) || strings.Contains(resp, "WARNING") {
					t.Fatalf("unexpected no-cut result")
				}
				return
			}
			if !strings.HasSuffix(stored, truncationMarkerLiteral) || len(stored) > memory.MaxContentLen+markerLen || !strings.Contains(resp, companionWarning) {
				t.Fatalf("unexpected cut result: %q %q", tail(stored, 40), resp)
			}
		})
	}
}
