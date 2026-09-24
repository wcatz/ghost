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

// truncationMarkerLiteral pins the exact marker ClampContent appends to
// stored content that was cut at the cap. Kept as a literal (not derived
// from memory.MaxContentLen) so a cap or marker change that alters stored
// bytes fails here instead of silently redefining "correct".
const truncationMarkerLiteral = " …[truncated at 8000 bytes]"

// truncationWarningLiteral pins the caller-facing warning that must appear
// in the save/update response when content was cut, so the saving agent
// learns the full text did NOT land.
const truncationWarningLiteral = "WARNING: content was truncated at 8000 bytes"

// newCapSession wires a fully-registered server (tools, schema and all) to
// an in-memory client session, returning both the session and the store so
// tests can read back what was actually persisted.
func newCapSession(t *testing.T) (*Server, *mcp.ClientSession) {
	t.Helper()
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	return srv, connectedClient(t, srv)
}

// saveAndFetchContent saves content through the live ghost_memory_save tool
// and returns the tool's response text plus the content actually stored for
// the memory id the response reports.
func saveAndFetchContent(t *testing.T, srv *Server, session *mcp.ClientSession, content string) (resp, stored string) {
	t.Helper()
	ctx := context.Background()
	res := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project",
		"content":    content,
		"category":   "fact",
	})
	resp = resultText(res)
	id, ok := extractID(resp)
	if !ok || id == "" {
		t.Fatalf("save response carries no memory id: %q", resp)
	}
	mems, err := srv.store.GetByIDs(ctx, []string{id})
	if err != nil || len(mems) != 1 {
		t.Fatalf("GetByIDs(%q): err=%v n=%d", id, err, len(mems))
	}
	return resp, mems[0].Content
}

// TestSave_StoresContentUpTo8000: content below the cap must be stored
// byte-for-byte. Before the cap rose this was the silent-loss path: a
// 5000-char incident record came back "Memory saved" while only its first
// 2000 chars reached the store.
func TestSave_StoresContentUpTo8000(t *testing.T) {
	srv, session := newCapSession(t)

	content := strings.Repeat("incident timeline: every byte must survive. ", 114)[:5000]
	resp, stored := saveAndFetchContent(t, srv, session, content)

	if stored != content {
		t.Errorf("stored content must be byte-equal to the 5000-char input; got len=%d (old silent 2000-char cut = %v), want len=5000",
			len(stored), len(stored) == 2000)
	}
	if strings.Contains(stored, truncationMarkerLiteral) {
		t.Errorf("content under the cap must not carry the truncation marker; stored tail %q", tail(stored, 60))
	}
	if strings.Contains(resp, "truncated") {
		t.Errorf("save of sub-cap content must not report truncation; got %q", resp)
	}
}

// TestSave_TruncationIsExplicitAtCap: content over the cap is stored cut at
// the cap with the marker naming the limit, and the response warns the
// caller — truncation must be visible in both the stored row and the reply.
func TestSave_TruncationIsExplicitAtCap(t *testing.T) {
	srv, session := newCapSession(t)

	input := strings.Repeat("z", 12000)
	resp, stored := saveAndFetchContent(t, srv, session, input)

	want := strings.Repeat("z", 8000) + truncationMarkerLiteral
	if stored != want {
		t.Errorf("stored content = len %d ending %q, want exactly 8000 bytes + %q (len %d)",
			len(stored), tail(stored, 60), truncationMarkerLiteral, len(want))
	}
	if !strings.HasSuffix(stored, truncationMarkerLiteral) {
		t.Errorf("stored content must end with the truncation marker; tail %q", tail(stored, 60))
	}
	if !strings.Contains(resp, truncationWarningLiteral) {
		t.Errorf("save response must warn the caller about the cut; got %q", resp)
	}
}

// TestSave_ContentCapBoundary: the marker appears only on an actual cut —
// exactly-at-cap content is stored untouched, one byte over is cut + marked.
func TestSave_ContentCapBoundary(t *testing.T) {
	cases := []struct {
		name    string
		n       int
		wantCut bool
	}{
		{"exactly at cap is stored as-is", 8000, false},
		{"one over the cap is cut and marked", 8001, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, session := newCapSession(t)

			input := strings.Repeat("z", tc.n)
			resp, stored := saveAndFetchContent(t, srv, session, input)

			if !tc.wantCut {
				if stored != input {
					t.Errorf("exactly-at-cap content must be stored byte-equal; got len=%d, want %d", len(stored), tc.n)
				}
				if strings.Contains(stored, truncationMarkerLiteral) {
					t.Errorf("exactly-at-cap content must carry no marker; tail %q", tail(stored, 60))
				}
				if strings.Contains(resp, "truncated") {
					t.Errorf("exactly-at-cap save must not report truncation; got %q", resp)
				}
				return
			}
			want := strings.Repeat("z", 8000) + truncationMarkerLiteral
			if stored != want {
				t.Errorf("8001-char input stored as len %d ending %q, want len %d ending %q",
					len(stored), tail(stored, 60), len(want), truncationMarkerLiteral)
			}
			if !strings.Contains(resp, truncationWarningLiteral) {
				t.Errorf("over-cap save response must carry the truncation warning; got %q", resp)
			}
		})
	}
}

// TestUpdate_TruncationIsExplicitAtCap: ghost_memory_update obeys the same
// cap with the same explicit marker and caller warning — one cap, one
// contract for both write paths.
func TestUpdate_TruncationIsExplicitAtCap(t *testing.T) {
	srv, session := newCapSession(t)
	ctx := context.Background()

	id, err := srv.store.Create(ctx, "abc123", memory.Memory{
		Category: "fact", Content: "original", Source: "mcp", Importance: 0.7, Tags: []string{},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	input := strings.Repeat("u", 9000)
	res := callTool(t, session, "ghost_memory_update", map[string]any{
		"project_id": "test-project",
		"memory_id":  id,
		"content":    input,
	})
	resp := resultText(res)
	mems, err := srv.store.GetByIDs(ctx, []string{id})
	if err != nil || len(mems) != 1 {
		t.Fatalf("GetByIDs(%q): err=%v n=%d", id, err, len(mems))
	}
	stored := mems[0].Content

	want := strings.Repeat("u", 8000) + truncationMarkerLiteral
	if stored != want {
		t.Errorf("updated content = len %d ending %q, want len %d ending %q",
			len(stored), tail(stored, 60), len(want), truncationMarkerLiteral)
	}
	if !strings.Contains(resp, truncationWarningLiteral) {
		t.Errorf("update response must warn the caller about the cut; got %q", resp)
	}
}

// TestSchemas_NeverRejectContentUpToCap guards the advertised maxLength
// contract of the save/update input schemas. The go-sdk validates every
// tool call against the input schema BEFORE the handler runs, so a schema
// maxLength below (or even at) the byte cap would make validating clients
// and the SDK itself reject content outright — no truncation, no marker,
// no save — instead of letting the server cut it explicitly. The schema may
// therefore carry no maxLength at all, or one no smaller than the server's
// own cap, never anything that rejects content the server would accept.
func TestSchemas_NeverRejectContentUpToCap(t *testing.T) {
	srv, session := newCapSession(t)
	_ = srv

	ctx := context.Background()
	tools, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	byName := map[string]*mcp.Tool{}
	for _, tool := range tools.Tools {
		byName[tool.Name] = tool
	}

	// Every field this PR clamps must also be free of a schema maxLength
	// below the cap: the SDK validates against the schema before the
	// handler runs, so a sub-cap maxLength there would hard-reject instead
	// of letting the server cut explicitly.
	guards := map[string][]string{
		"ghost_memory_save":     {"content"},
		"ghost_memory_update":   {"content"},
		"ghost_save_global":     {"content"},
		"ghost_task_create":     {"description"},
		"ghost_task_update":     {"description"},
		"ghost_decision_record": {"decision", "rationale"},
	}
	for name, props := range guards {
		tool, ok := byName[name]
		if !ok {
			t.Fatalf("tool %s missing from tools/list", name)
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshal %s input schema: %v", name, err)
		}
		var schema struct {
			Properties map[string]struct {
				MaxLength *int `json:"maxLength"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("unmarshal %s input schema: %v", name, err)
		}
		for _, prop := range props {
			field, ok := schema.Properties[prop]
			if !ok {
				t.Fatalf("%s input schema has no %s property: %s", name, prop, raw)
			}
			if field.MaxLength != nil && *field.MaxLength < memory.MaxContentLen {
				t.Errorf("%s input schema property %q declares maxLength=%d, below the %d-byte server cap — "+
					"validating clients and the SDK would reject content the server would accept",
					name, prop, *field.MaxLength, memory.MaxContentLen)
			}
		}
	}
}

// tail returns the last n bytes of s for failure messages (bounded so a
// pathological input can't flood the log).
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// TestTruncationWarnings_NameTheirKind pins the warning text emitted by
// each call-site kind: memory tools advise splitting into memories, task
// tools into tasks, and the decision tool into decision-appropriate
// recovery — one shared "split it into focused memories" line reused for a
// task or a decision is the sweeper finding this guards against. Every
// case also asserts the OTHER kinds' advice is absent, so swapping two
// call sites' kinds (or reverting to the shared wording) fails a test.
func TestTruncationWarnings_NameTheirKind(t *testing.T) {
	cases := []struct {
		name    string
		call    func(t *testing.T, srv *Server, session *mcp.ClientSession) string
		want    string
		notWant []string
	}{
		{
			name: "memory save advises memories",
			call: func(t *testing.T, srv *Server, session *mcp.ClientSession) string {
				resp, _ := saveAndFetchContent(t, srv, session, strings.Repeat("z", 9000))
				return resp
			},
			want:    "WARNING: content was truncated at 8000 bytes; the stored text is incomplete — split it into focused memories or shorten it deliberately.",
			notWant: []string{"focused tasks", "rationale context"},
		},
		{
			name: "task create advises tasks",
			call: func(t *testing.T, srv *Server, session *mcp.ClientSession) string {
				res := callTool(t, session, "ghost_task_create", map[string]any{
					"project_id":  "test-project",
					"title":       "cap test task",
					"description": strings.Repeat("t", 9000),
				})
				return resultText(res)
			},
			want:    "WARNING: task description was truncated at 8000 bytes; the stored text is incomplete — split it into focused tasks or shorten it deliberately.",
			notWant: []string{"focused memories", "rationale context"},
		},
		{
			name: "decision record advises decision recovery",
			call: func(t *testing.T, srv *Server, session *mcp.ClientSession) string {
				res := callTool(t, session, "ghost_decision_record", map[string]any{
					"project_id": "test-project",
					"title":      "cap test decision",
					"decision":   strings.Repeat("d", 9000),
					"rationale":  "short rationale",
				})
				return resultText(res)
			},
			want:    "WARNING: decision text was truncated at 8000 bytes; the stored text is incomplete — shorten it, or move detail into the decision's rationale context.",
			notWant: []string{"focused memories", "focused tasks"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, session := newCapSession(t)
			resp := tc.call(t, srv, session)
			t.Logf("captured response: %s", resp)

			if !strings.Contains(resp, tc.want) {
				t.Errorf("response must carry the kind-correct warning\n  want substring: %q\n  got: %q", tc.want, resp)
			}
			for _, bad := range tc.notWant {
				if strings.Contains(resp, bad) {
					t.Errorf("response carries another writer's advice %q: %q", bad, resp)
				}
			}
		})
	}
}

// memoryIDFromDecisionResp pulls the companion memory id out of a
// ghost_decision_record response ("… (memory_id: ABC…)").
func memoryIDFromDecisionResp(t *testing.T, resp string) string {
	t.Helper()
	const marker = "(memory_id: "
	i := strings.Index(resp, marker)
	if i < 0 {
		t.Fatalf("response carries no memory id: %q", resp)
	}
	rest := resp[i+len(marker):]
	j := strings.IndexByte(rest, ')')
	if j < 0 {
		t.Fatalf("unterminated memory id in response: %q", resp)
	}
	return rest[:j]
}

// TestDecisionCompanionMemoryCapped guards the composition path:
// RecordDecision builds the companion memory as "title: decision.
// Rationale: rationale", so two INDIVIDUALLY sub-cap fields can sum over
// the cap — 7000B decision + 2000B rationale passes both field clamps with
// no warning, yet the composed row lands at ~9034 bytes > 8000. The
// composition must be clamped at the canonical site inside RecordDecision,
// and the handler must warn the caller even though no field was cut.
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
		{"sub-cap fields summing over the cap", strings.Repeat("d", 7000), strings.Repeat("r", 2000), true},
		{"both fields at the cap", strings.Repeat("e", 8000), strings.Repeat("f", 8000), true},
		{"sub-cap sum stays byte-identical", "Use SQLite", "No external deps", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, session := newCapSession(t)
			res := callTool(t, session, "ghost_decision_record", map[string]any{
				"project_id": "test-project",
				"title":      title,
				"decision":   tc.decision,
				"rationale":  tc.rationale,
			})
			resp := resultText(res)
			t.Logf("captured response: %s", resp)
			memID := memoryIDFromDecisionResp(t, resp)

			mems, err := srv.store.GetByIDs(context.Background(), []string{memID})
			if err != nil || len(mems) != 1 {
				t.Fatalf("GetByIDs(%q): err=%v n=%d", memID, err, len(mems))
			}
			stored := mems[0].Content

			if !tc.wantCut {
				want := fmt.Sprintf("%s: %s. Rationale: %s", title, tc.decision, tc.rationale)
				if stored != want {
					t.Errorf("companion memory rewritten despite sub-cap inputs: len %d, want byte-identical len %d", len(stored), len(want))
				}
				if strings.Contains(stored, truncationMarkerLiteral) || strings.Contains(resp, "WARNING") {
					t.Errorf("sub-cap composition must carry no marker or warning; stored tail %q, resp %q", tail(stored, 40), resp)
				}
				return
			}

			// Cut: exactly the cap's worth of composed text plus the pinned
			// marker (ASCII inputs → rune boundary is the byte boundary).
			if !strings.HasSuffix(stored, truncationMarkerLiteral) {
				t.Errorf("companion memory must end with the truncation marker; len=%d tail %q", len(stored), tail(stored, 60))
			}
			if len(stored) > memory.MaxContentLen+markerLen {
				t.Errorf("companion memory stored at %d bytes > cap+marker (%d) — composition bypassed the cap", len(stored), memory.MaxContentLen+markerLen)
			}
			if !strings.Contains(resp, companionWarning) {
				t.Errorf("response must warn that the companion composition was cut even though no field was; got %q", resp)
			}
		})
	}
}
