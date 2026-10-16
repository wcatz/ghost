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

// extractID pulls the hex ID out of "… (id: ABC123…)" result text.
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

func TestOptFloat32(t *testing.T) {
	cases := []struct {
		name    string
		in      any
		want    *float32
		wantErr bool
	}{
		{"nil", nil, nil, false},
		{"native", 0.7, ptr32(0.7), false},
		{"int native", 1.0, ptr32(1.0), false},
		{"stringified", "0.4", ptr32(0.4), false},
		{"padded", " 2 ", ptr32(2), false},
		{"garbage", "abc", nil, true},
		{"bool", true, nil, true},
		{"json.Number", json.Number("0.25"), ptr32(0.25), false},
		{"string NaN", "NaN", nil, true},
		{"string +Inf", "+Inf", nil, true},
		{"string -Inf", "-Inf", nil, true},
		{"json.Number NaN", json.Number("NaN"), nil, true},
		{"json.Number Inf", json.Number("Infinity"), nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := optFloat32(tc.in, "importance")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "importance") {
				t.Errorf("error must name the field: %v", err)
			}
			if got == nil || tc.want == nil {
				if (got == nil) != (tc.want == nil) {
					t.Errorf("got %v, want %v", got, tc.want)
				}
				return
			}
			if math.Abs(float64(*got)-float64(*tc.want)) > 1e-6 {
				t.Errorf("got %v, want %v", *got, *tc.want)
			}
		})
	}
}

func TestOptInt(t *testing.T) {
	cases := []struct {
		name    string
		in      any
		want    *int
		wantErr bool
	}{
		{"nil", nil, nil, false},
		{"native", 3.0, ptr(3), false},
		{"stringified", "2", ptr(2), false},
		{"fractional", 1.5, nil, true},
		{"garbage", "low", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := optInt(tc.in, "priority")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if got == nil || tc.want == nil {
				if (got == nil) != (tc.want == nil) {
					t.Errorf("got %v, want %v", got, tc.want)
				}
				return
			}
			if *got != *tc.want {
				t.Errorf("got %v, want %v", *got, *tc.want)
			}
		})
	}
}

func TestOptStringSlice(t *testing.T) {
	cases := []struct {
		name    string
		in      any
		want    []string
		nilOut  bool
		wantErr bool
	}{
		{"nil", nil, nil, true, false},
		{"any array", []any{"a", "b"}, []string{"a", "b"}, false, false},
		{"typed array", []string{"a"}, []string{"a"}, false, false},
		{"json string", `["a","b"]`, []string{"a", "b"}, false, false},
		{"empty json string", `[]`, []string{}, false, false},
		{"empty any array", []any{}, []string{}, false, false},
		{"number element", []any{1}, nil, false, true},
		{"bare string", "a,b", nil, false, true},
		{"malformed json", "[a]", nil, false, true},
		{"empty string", "", nil, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := optStringSlice(tc.in, "tags")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "tags") {
				t.Errorf("error must name the field: %v", err)
			}
			if tc.nilOut {
				if got != nil {
					t.Errorf("got %v, want nil", got)
				}
				return
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func ptr32(f float32) *float32 { return &f }
func ptr(i int) *int           { return &i }

// End-to-end: stringified scalars/arrays (as sent by clients that coerce
// union-typed schema fields to strings) must be accepted and persisted.

func TestGhostMemorySave_AcceptsStringifiedArgs(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)

	result := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "abc123",
		"content":    "stringified importance and tags regression probe",
		"importance": "0.4",
		"tags":       `["coerce","regression"]`,
	})
	if result.IsError {
		t.Fatalf("save rejected stringified args: %s", resultText(result))
	}
	id, ok := extractID(resultText(result))
	if !ok {
		t.Fatalf("parse id from %q", resultText(result))
	}

	mem, err := store.GetByIDs(context.Background(), []string{id})
	if err != nil || len(mem) != 1 {
		t.Fatalf("GetByIDs: %v, %d rows", err, len(mem))
	}
	if math.Abs(float64(mem[0].Importance)-0.4) > 1e-6 {
		t.Errorf("importance = %v, want 0.4", mem[0].Importance)
	}
	if len(mem[0].Tags) != 2 || mem[0].Tags[0] != "coerce" {
		t.Errorf("tags = %v, want [coerce regression]", mem[0].Tags)
	}
}

func TestGhostMemorySave_RejectsNonNumericImportance(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)

	result := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "abc123",
		"content":    "bad importance",
		"importance": "very",
	})
	if !result.IsError {
		t.Fatalf("expected rejection of importance=\"very\", got: %s", resultText(result))
	}
	if !strings.Contains(resultText(result), "importance") {
		t.Errorf("error must name the field: %s", resultText(result))
	}
}

// TestSaveToolsStoreTheirDocumentedImportanceDefaults pins the two defaults the
// tool schemas state: ghost_memory_save stores 0.7 when the caller omits an
// importance, and ghost_save_global stores 0.8. The writers that bind no
// default (`Store.Create` and the corpus seeders that share its insert) are not
// reached by a production save path, so neither tool may leave a saved row at
// 0. A stated 0 is a different case the caller asked for, and the schemas say
// so; the artifact's round trip is what depends on this distinction, so a
// default that drifted here would make the docs wrong about every row the tools
// write.
func TestSaveToolsStoreTheirDocumentedImportanceDefaults(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)

	projectResult := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "abc123",
		"content":    "a project memory saved with no importance",
	})
	if projectResult.IsError {
		t.Fatalf("ghost_memory_save: %s", resultText(projectResult))
	}
	projectID, ok := extractID(resultText(projectResult))
	if !ok {
		t.Fatalf("parse id from %q", resultText(projectResult))
	}

	globalResult := callTool(t, session, "ghost_save_global", map[string]any{
		"content":  "a cross-project memory saved with no importance",
		"category": "fact",
	})
	if globalResult.IsError {
		t.Fatalf("ghost_save_global: %s", resultText(globalResult))
	}
	globalID, ok := extractID(resultText(globalResult))
	if !ok {
		t.Fatalf("parse id from %q", resultText(globalResult))
	}

	mem, err := store.GetByIDs(context.Background(), []string{projectID, globalID})
	if err != nil || len(mem) != 2 {
		t.Fatalf("GetByIDs: %v, %d rows", err, len(mem))
	}
	got := map[string]float32{}
	for _, m := range mem {
		got[m.ID] = m.Importance
	}
	if math.Abs(float64(got[projectID])-0.7) > 1e-6 {
		t.Errorf("ghost_memory_save stored importance %v, want its documented 0.7 default", got[projectID])
	}
	if math.Abs(float64(got[globalID])-0.8) > 1e-6 {
		t.Errorf("ghost_save_global stored importance %v, want its documented 0.8 default", got[globalID])
	}
}

func TestGhostTaskCreateAndUpdate_AcceptStringifiedPriority(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)

	result := callTool(t, session, "ghost_task_create", map[string]any{
		"project_id": "abc123",
		"title":      "stringified priority",
		"priority":   "3",
	})
	if result.IsError {
		t.Fatalf("create rejected stringified priority: %s", resultText(result))
	}
	id, ok := extractID(resultText(result))
	if !ok {
		t.Fatalf("parse id from %q", resultText(result))
	}

	tasks, err := store.ListTasks(context.Background(), "abc123", "", 10)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("ListTasks: %v, %d rows", err, len(tasks))
	}
	if tasks[0].Priority != 3 {
		t.Errorf("priority = %d, want 3", tasks[0].Priority)
	}

	result = callTool(t, session, "ghost_task_update", map[string]any{
		"task_id":  id,
		"priority": 0.0,
	})
	if result.IsError {
		t.Fatalf("update rejected native-numeric priority: %s", resultText(result))
	}
	tasks, _ = store.ListTasks(context.Background(), "abc123", "", 10)
	if len(tasks) != 1 || tasks[0].Priority != 0 {
		t.Fatalf("priority after update = %+v, want 0", tasks)
	}

	result = callTool(t, session, "ghost_task_update", map[string]any{
		"task_id":  id,
		"priority": "critical",
	})
	if !result.IsError || !strings.Contains(resultText(result), "priority") {
		t.Fatalf("expected named rejection, got: %s", resultText(result))
	}
}

func TestGhostDecisionRecord_AcceptsStringifiedArrays(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)

	result := callTool(t, session, "ghost_decision_record", map[string]any{
		"project_id":   "abc123",
		"title":        "stringified arrays",
		"decision":     "d",
		"rationale":    "r",
		"alternatives": `["PostgreSQL","Redis"]`,
		"tags":         `["coerce"]`,
	})
	if result.IsError {
		t.Fatalf("decision record rejected stringified arrays: %s", resultText(result))
	}

	decisions, err := store.ListDecisions(context.Background(), "abc123", "", 10)
	if err != nil || len(decisions) != 1 {
		t.Fatalf("ListDecisions: %v, %d rows", err, len(decisions))
	}
	if fmt.Sprint(decisions[0].Alternatives) != "[PostgreSQL Redis]" {
		t.Errorf("alternatives = %v", decisions[0].Alternatives)
	}
}

// A scope value that is not a string is refused by the coercion before the
// scope guard runs. The refusal must never carry the offending key: a key is
// caller text, so quoting it would echo a token-shaped or arbitrarily long
// string back in the answer.
func TestOptScope_NonStringValueRefusalOmitsKey(t *testing.T) {
	keys := map[string]string{
		"token":    "ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"oversize": strings.Repeat("k", 200*1024),
	}
	for name, key := range keys {
		t.Run(name, func(t *testing.T) {
			_, err := optScope(map[string]any{key: 5}, "scope")
			if err == nil {
				t.Fatal("expected a refusal for a non-string scope value")
			}
			if strings.Contains(err.Error(), key[:16]) {
				t.Errorf("refusal echoes the key: %.120s", err.Error())
			}
			if len(err.Error()) > 200 {
				t.Errorf("refusal is unbounded: %d bytes", len(err.Error()))
			}
			if !strings.Contains(err.Error(), "string") {
				t.Errorf("refusal must say a string is required: %v", err)
			}
		})
	}
}

func TestGhostMemorySave_NonStringScopeValueRefusalOmitsKey(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)

	for _, key := range []string{"ghp_abcdefghijklmnopqrstuvwxyz0123456789", strings.Repeat("k", 200*1024)} {
		result := callTool(t, session, "ghost_memory_save", map[string]any{
			"project_id": "abc123",
			"content":    "scope with a non-string value",
			"scope":      map[string]any{key: 5},
		})
		if !result.IsError {
			t.Fatalf("expected a refusal, got: %.200s", resultText(result))
		}
		if txt := resultText(result); strings.Contains(txt, key[:16]) || len(txt) > 1024 {
			t.Errorf("refusal carries the key or is unbounded (%d bytes): %.200s", len(txt), txt)
		}
	}
}
