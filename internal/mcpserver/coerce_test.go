package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func callTool(t *testing.T, session *mcp.ClientSession, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", tool, err)
	}
	return result
}

func extractID(text string) (string, bool) {
	start := strings.Index(text, "(id: ")
	if start < 0 {
		return "", false
	}
	rest := text[start+len("(id: "):]
	end := strings.IndexByte(rest, ')')
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}

func resultText(result *mcp.CallToolResult) string {
	var parts []string
	for _, c := range result.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, " ")
}

func ptr32(f float32) *float32 { return &f }
func ptr(i int) *int           { return &i }

func TestOptFloat32(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      any
		want    *float32
		wantErr bool
	}{
		{"nil", nil, nil, false}, {"native", 0.7, ptr32(0.7), false}, {"stringified", "0.4", ptr32(0.4), false}, {"padded", " 2 ", ptr32(2), false},
		{"garbage", "abc", nil, true}, {"bool", true, nil, true}, {"json.Number", json.Number("0.25"), ptr32(0.25), false},
		{"string NaN", "NaN", nil, true}, {"string +Inf", "+Inf", nil, true}, {"json.Number NaN", json.Number("NaN"), nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := optFloat32(tc.in, "importance")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if err != nil {
				if !strings.Contains(err.Error(), "importance") {
					t.Fatalf("error missing field: %v", err)
				}
				return
			}
			if (got == nil) != (tc.want == nil) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
			if got != nil && math.Abs(float64(*got)-float64(*tc.want)) > 1e-6 {
				t.Fatalf("got %v want %v", *got, *tc.want)
			}
		})
	}
}

func TestOptInt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      any
		want    *int
		wantErr bool
	}{
		{"nil", nil, nil, false}, {"native", 3.0, ptr(3), false}, {"stringified", "2", ptr(2), false}, {"fractional", 1.5, nil, true}, {"garbage", "low", nil, true},
	} {
		got, err := optInt(tc.in, "priority")
		if (err != nil) != tc.wantErr {
			t.Fatalf("%s: err=%v wantErr=%v", tc.name, err, tc.wantErr)
		}
		if got == nil || tc.want == nil {
			if (got == nil) != (tc.want == nil) {
				t.Fatalf("%s: got %v want %v", tc.name, got, tc.want)
			}
		} else if *got != *tc.want {
			t.Fatalf("%s: got %v want %v", tc.name, *got, *tc.want)
		}
	}
}

func TestOptStringSlice(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      any
		want    []string
		nilOut  bool
		wantErr bool
	}{
		{"nil", nil, nil, true, false}, {"any array", []any{"a", "b"}, []string{"a", "b"}, false, false}, {"typed array", []string{"a"}, []string{"a"}, false, false},
		{"json string", `["a","b"]`, []string{"a", "b"}, false, false}, {"empty json string", `[]`, []string{}, false, false}, {"empty any array", []any{}, []string{}, false, false},
		{"number element", []any{1}, nil, false, true}, {"bare string", "a,b", nil, false, true}, {"malformed json", "[a]", nil, false, true}, {"empty string", "", nil, false, true},
	} {
		got, err := optStringSlice(tc.in, "tags")
		if (err != nil) != tc.wantErr {
			t.Fatalf("%s: err=%v wantErr=%v", tc.name, err, tc.wantErr)
		}
		if err != nil {
			if !strings.Contains(err.Error(), "tags") {
				t.Fatalf("%s: missing field in %v", tc.name, err)
			}
			continue
		}
		if tc.nilOut {
			if got != nil {
				t.Fatalf("%s: got %v want nil", tc.name, got)
			}
			continue
		}
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Fatalf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestGhostMemorySave_AcceptsStringifiedArgs(t *testing.T) {
	store := testStore(t)
	srv := New(store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test")
	session := connectedClient(t, srv)
	result := callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "abc123", "content": "stringified importance and tags regression probe", "importance": "0.4", "tags": `["coerce","regression"]`})
	if result.IsError {
		t.Fatalf("rejected: %s", resultText(result))
	}
	id, _ := extractID(resultText(result))
	mem, err := store.GetByIDs(context.Background(), []string{id})
	if err != nil || len(mem) != 1 || math.Abs(float64(mem[0].Importance)-0.4) > 1e-6 || len(mem[0].Tags) != 2 || mem[0].Tags[0] != "coerce" {
		t.Fatalf("unexpected mem: %+v err=%v", mem, err)
	}
}

func TestGhostMemorySave_RejectsNonNumericImportance(t *testing.T) {
	srv := New(testStore(t), slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test")
	result := callTool(t, connectedClient(t, srv), "ghost_memory_save", map[string]any{"project_id": "abc123", "content": "bad importance", "importance": "very"})
	if !result.IsError || !strings.Contains(resultText(result), "importance") {
		t.Fatalf("expected rejection: %s", resultText(result))
	}
}

func TestSaveToolsStoreTheirDocumentedImportanceDefaults(t *testing.T) {
	store := testStore(t)
	session := connectedClient(t, New(store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test"))
	projectResult := callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "abc123", "content": "a project memory saved with no importance"})
	globalResult := callTool(t, session, "ghost_save_global", map[string]any{"content": "a cross-project memory saved with no importance", "category": "fact"})
	projectID, _ := extractID(resultText(projectResult))
	globalID, _ := extractID(resultText(globalResult))
	mem, err := store.GetByIDs(context.Background(), []string{projectID, globalID})
	if err != nil || len(mem) != 2 {
		t.Fatalf("GetByIDs: %v %d", err, len(mem))
	}
	got := map[string]float32{}
	for _, m := range mem {
		got[m.ID] = m.Importance
	}
	if math.Abs(float64(got[projectID])-0.7) > 1e-6 || math.Abs(float64(got[globalID])-0.8) > 1e-6 {
		t.Fatalf("defaults wrong: %v %v", got[projectID], got[globalID])
	}
}

func TestGhostTaskCreateAndUpdate_AcceptStringifiedPriority(t *testing.T) {
	store := testStore(t)
	session := connectedClient(t, New(store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test"))
	result := callTool(t, session, "ghost_task_create", map[string]any{"project_id": "abc123", "title": "stringified priority", "priority": "3"})
	if result.IsError {
		t.Fatalf("create rejected: %s", resultText(result))
	}
	id, _ := extractID(resultText(result))
	tasks, _ := store.ListTasks(context.Background(), "abc123", "", 10)
	if len(tasks) != 1 || tasks[0].Priority != 3 {
		t.Fatalf("priority wrong: %+v", tasks)
	}
	callTool(t, session, "ghost_task_update", map[string]any{"task_id": id, "priority": 0.0})
	tasks, _ = store.ListTasks(context.Background(), "abc123", "", 10)
	if len(tasks) != 1 || tasks[0].Priority != 0 {
		t.Fatalf("priority after update: %+v", tasks)
	}
	result = callTool(t, session, "ghost_task_update", map[string]any{"task_id": id, "priority": "critical"})
	if !result.IsError || !strings.Contains(resultText(result), "priority") {
		t.Fatalf("expected rejection: %s", resultText(result))
	}
}

func TestGhostDecisionRecord_AcceptsStringifiedArrays(t *testing.T) {
	store := testStore(t)
	session := connectedClient(t, New(store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test"))
	result := callTool(t, session, "ghost_decision_record", map[string]any{"project_id": "abc123", "title": "stringified arrays", "decision": "d", "rationale": "r", "alternatives": `["PostgreSQL","Redis"]`, "tags": `["coerce"]`})
	if result.IsError {
		t.Fatalf("rejected: %s", resultText(result))
	}
	decisions, _ := store.ListDecisions(context.Background(), "abc123", "", 10)
	if len(decisions) != 1 || fmt.Sprint(decisions[0].Alternatives) != "[PostgreSQL Redis]" {
		t.Fatalf("alternatives: %v", decisions[0].Alternatives)
	}
}

func TestOptScopeAndSave_NonStringScopeValueRefusalOmitsKey(t *testing.T) {
	for _, key := range []string{"ghp_abcdefghijklmnopqrstuvwxyz0123456789", strings.Repeat("k", 200*1024)} {
		_, err := optScope(map[string]any{key: 5}, "scope")
		if err == nil || strings.Contains(err.Error(), key[:16]) || len(err.Error()) > 200 || !strings.Contains(err.Error(), "string") {
			t.Fatalf("bad refusal: %v", err)
		}
	}
	store := testStore(t)
	session := connectedClient(t, New(store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test"))
	for _, key := range []string{"ghp_abcdefghijklmnopqrstuvwxyz0123456789", strings.Repeat("k", 200*1024)} {
		result := callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "abc123", "content": "scope with a non-string value", "scope": map[string]any{key: 5}})
		if !result.IsError {
			t.Fatalf("expected refusal: %s", resultText(result))
		}
		if txt := resultText(result); strings.Contains(txt, key[:16]) || len(txt) > 1024 {
			t.Fatalf("bad refusal text: %q", txt)
		}
	}
}
