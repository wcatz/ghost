package mcpserver

// An argument refusal names the argument and the shape it expected, and renders
// the offending value only through a bounded, token-guarded renderer or not at
// all. The refusal goes back to the caller as tool text, so a token-shaped
// argument or a 200 KB one must come back as neither.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/provider"
)

const refusalBigValue = 200 * 1024

func TestArgumentRefusalsNeverEchoATokenOrAnUnboundedValue(t *testing.T) {
	big := strings.Repeat("z", refusalBigValue)
	values := map[string]string{"token": echoToken, "big": big}

	// Each case refuses one argument. The argument is built from the offending
	// value, so the same shape is walked with a token and with a 200 KB string.
	type refusal struct {
		name string
		tool string
		args func(v string) map[string]any
	}
	save := func(extra map[string]any) map[string]any {
		a := map[string]any{"project_id": "p", "content": "a fact worth keeping"}
		for k, v := range extra {
			a[k] = v
		}
		return a
	}
	cases := []refusal{
		{"tags string", "ghost_memory_save", func(v string) map[string]any { return save(map[string]any{"tags": v}) }},
		{"tags object", "ghost_memory_save", func(v string) map[string]any { return save(map[string]any{"tags": map[string]any{v: v}}) }},
		{"tags element", "ghost_memory_save", func(v string) map[string]any {
			return save(map[string]any{"tags": []any{map[string]any{v: v}}})
		}},
		{"tags bool", "ghost_memory_save", func(v string) map[string]any { return save(map[string]any{"tags": true}) }},
		{"importance string", "ghost_memory_save", func(v string) map[string]any { return save(map[string]any{"importance": v}) }},
		{"importance object", "ghost_memory_save", func(v string) map[string]any {
			return save(map[string]any{"importance": map[string]any{v: v}})
		}},
		{"importance array", "ghost_memory_save", func(v string) map[string]any {
			return save(map[string]any{"importance": []any{v}})
		}},
		{"category save", "ghost_memory_save", func(v string) map[string]any { return save(map[string]any{"category": v}) }},
		{"category update", "ghost_memory_update", func(v string) map[string]any {
			return map[string]any{"project_id": "p", "memory_id": "m", "category": v}
		}},
		{"status", "ghost_task_update", func(v string) map[string]any {
			return map[string]any{"task_id": "t", "status": v}
		}},
		{"relation", "ghost_link_withdraw", func(v string) map[string]any {
			return map[string]any{"project_id": "p", "source_id": "s", "target_id": "t", "relation": v}
		}},
		{"as_of", "ghost_memory_search", func(v string) map[string]any {
			return map[string]any{"project_id": "p", "query": "q", "as_of": v}
		}},
		{"valid_from string", "ghost_memory_save", func(v string) map[string]any { return save(map[string]any{"valid_from": v}) }},
		{"valid_from object", "ghost_memory_save", func(v string) map[string]any {
			return save(map[string]any{"valid_from": map[string]any{v: v}})
		}},
	}
	for _, tc := range cases {
		for label, v := range values {
			t.Run(tc.name+"/"+label, func(t *testing.T) {
				_, session, _ := echoLogSession(t)
				out := callToolErr(t, session, tc.tool, tc.args(v))
				assertNoRefusalEcho(t, out, v)
			})
		}
	}
}

// The refusals that need a registered project (a memory that is not there, a
// filtered browse that matches nothing) and the ones outside a tool's arguments.
func TestStoreBackedRefusalsNeverEchoATokenOrAnUnboundedValue(t *testing.T) {
	big := strings.Repeat("z", refusalBigValue)
	for label, v := range map[string]string{"token": echoToken, "big": big} {
		t.Run("memory_id/"+label, func(t *testing.T) {
			_, session, _ := echoLogSession(t)
			callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "p", "content": "seed fact for the project"})
			out := callToolErr(t, session, "ghost_memory_update", map[string]any{"project_id": "p", "memory_id": v, "content": "c"})
			assertNoRefusalEcho(t, out, v)
		})
		t.Run("memory_id delete/"+label, func(t *testing.T) {
			_, session, _ := echoLogSession(t)
			callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "p", "content": "seed fact for the project"})
			out := callToolErr(t, session, "ghost_memory_delete", map[string]any{"project_id": "p", "memory_id": v})
			assertNoRefusalEcho(t, out, v)
		})
		for tool, extra := range map[string]map[string]any{
			"ghost_memory_promote": {},
			"ghost_memory_pin":     {"pinned": true},
			"ghost_memory_flag":    {"kind": "wrong", "reason": "r"},
		} {
			t.Run(tool+"/"+label, func(t *testing.T) {
				_, session, _ := echoLogSession(t)
				callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "p", "content": "seed fact for the project"})
				args := map[string]any{"project_id": "p", "memory_id": v}
				for k, x := range extra {
					args[k] = x
				}
				out := callToolErr(t, session, tool, args)
				if !strings.Contains(out, "not found") && !strings.Contains(out, "no memory with that id") {
					t.Fatalf("%s did not reach its not-found refusal: %.200s", tool, out)
				}
				assertNoRefusalEcho(t, out, v)
			})
		}
		t.Run("memory_id purge/"+label, func(t *testing.T) {
			_, session, _ := echoLogSession(t)
			callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "p", "content": "seed fact for the project"})
			out := callToolErr(t, session, "ghost_memory_delete", map[string]any{"project_id": "p", "memory_id": v, "purge_history": true})
			if !strings.Contains(out, "no recorded history") {
				t.Fatalf("purge did not reach its no-history refusal: %.200s", out)
			}
			assertNoRefusalEcho(t, out, v)
		})
		t.Run("memory_id purge on a store without history/"+label, func(t *testing.T) {
			db, err := memory.OpenDB(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			session := connectedClient(t, New(struct{ provider.MemoryStore }{memory.NewStore(db, logger)}, logger, "test"))
			callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "p", "content": "seed fact for the project"})
			out := callToolErr(t, session, "ghost_memory_delete", map[string]any{"project_id": "p", "memory_id": v, "purge_history": true})
			if !strings.Contains(out, "cannot reach") {
				t.Fatalf("purge did not reach the no-history-store refusal: %.200s", out)
			}
			assertNoRefusalEcho(t, out, v)
		})
		t.Run("category save_global/"+label, func(t *testing.T) {
			_, session, _ := echoLogSession(t)
			out := callToolErr(t, session, "ghost_save_global", map[string]any{"content": "a fact worth keeping", "category": v})
			assertNoRefusalEcho(t, out, v)
		})
		t.Run("unknown project/"+label, func(t *testing.T) {
			_, session, _ := echoLogSession(t)
			out := callToolErr(t, session, "ghost_memory_pin", map[string]any{"project_id": v, "memory_id": "m", "pinned": true})
			if !strings.Contains(out, "not found") {
				t.Fatalf("did not reach the unknown-project refusal: %.200s", out)
			}
			assertNoRefusalEcho(t, out, v)
		})
		t.Run("list filters/"+label, func(t *testing.T) {
			_, session, _ := echoLogSession(t)
			callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "p", "content": "seed fact for the project"})
			for _, args := range []map[string]any{
				{"project_id": "p", "category": v},
				{"project_id": "p", "retention": v},
				{"project_id": "p", "category": v, "retention": v},
			} {
				assertNoRefusalEcho(t, resultText(callTool(t, session, "ghost_memories_list", args)), v)
			}
		})
		t.Run("project shape/"+label, func(t *testing.T) {
			_, session, _ := echoLogSession(t)
			out := callToolErr(t, session, "ghost_memory_save", map[string]any{"project_id": "\u00ab" + v, "content": "a fact worth keeping"})
			assertNoRefusalEcho(t, out, v)
		})
		t.Run("subscribe URI/"+label, func(t *testing.T) {
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			db, err := memory.OpenDB(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			srv := New(memory.NewStore(db, logger), logger, "test")
			err = srv.handleSubscribe(context.Background(), &mcp.SubscribeRequest{Params: &mcp.SubscribeParams{URI: "http://" + v}})
			if err == nil {
				t.Fatal("subscribe was not refused")
			}
			assertNoRefusalEcho(t, err.Error(), v)
		})
		t.Run("client name/"+label, func(t *testing.T) {
			old := detectCallingSource
			detectCallingSource = func() string { return "" }
			t.Cleanup(func() { detectCallingSource = old })
			db, err := memory.OpenDB(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			session := connectedClientNamed(t, New(memory.NewStore(db, logger), logger, "test"), v)
			callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "p", "content": "seed fact for the project"})
			out := callToolErr(t, session, "ghost_resolve", map[string]any{"project": "p"})
			if !strings.Contains(out, "cannot determine the calling harness") {
				t.Fatalf("ghost_resolve did not reach the undetectable-harness refusal: %.200s", out)
			}
			assertNoRefusalEcho(t, out, v)
		})
	}
}

func assertNoRefusalEcho(t *testing.T, out, v string) {
	t.Helper()
	if strings.Contains(out, echoToken) {
		t.Errorf("refusal echoed the token: %.300s", out)
	}
	if strings.Contains(out, v[:min(len(v), 64)]) && len(v) > 64 {
		t.Errorf("refusal echoed the start of an oversized value: %.300s", out)
	}
	if len(out) > 2048 {
		t.Errorf("refusal is %d bytes, want a bounded sentence: %.200s", len(out), out)
	}
}

func TestArgumentRefusalsStillNameTheArgumentAndTheShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func() error
		want []string
	}{
		{"tags string", func() error { _, err := optStringSlice("not json", "tags"); return err }, []string{"tags", "array of strings", "not json"}},
		{"tags object", func() error { _, err := optStringSlice(map[string]any{"a": "b"}, "tags"); return err }, []string{"tags", "array of strings", "map"}},
		{"tags element", func() error { _, err := optStringSlice([]any{"ok", 7.0}, "tags"); return err }, []string{"tags", "element 2", "float64"}},
		{"number string", func() error { _, err := toFloat64("high", "importance"); return err }, []string{"importance", "number", "high"}},
		{"number object", func() error { _, err := toFloat64(map[string]any{}, "importance"); return err }, []string{"importance", "number", "map"}},
		{"stamp string", func() error { _, err := parseStampArg("valid_from", "soon", false); return err }, []string{"valid_from", "RFC 3339", "soon"}},
		{"stamp object", func() error { _, err := parseStampArg("valid_from", 5.0, false); return err }, []string{"valid_from", "string timestamp", "float64"}},
		{"number json", func() error { _, err := toFloat64(json.Number("1.2.3"), "importance"); return err }, []string{"importance", "number", "1.2.3"}},
		{"number json big", func() error {
			_, err := toFloat64(json.Number(strings.Repeat("9", refusalBigValue)+"x"), "importance")
			if err != nil && len(err.Error()) > 2048 {
				t.Errorf("json.Number refusal is %d bytes", len(err.Error()))
			}
			return err
		}, []string{"importance", "number"}},
		{"as_of", func() error { _, err := parseAsOf("yesterday"); return err }, []string{"as_of", "RFC 3339", "yesterday"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("not refused")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("refusal %q does not contain %q", err.Error(), w)
				}
			}
		})
	}
}

// ProjectArg is also how the prompts and the not-registered answers name a
// project, and there the value has to come back whole: record_decision tells the
// agent to call a tool with that id. Only a refusal is bounded.
func TestPromptsRenderALongProjectPathWholeAndRefusalsDoNot(t *testing.T) {
	long := "/srv/" + strings.Repeat("d", 300) + "/ghost"
	if len(long) <= memory.ArgEchoMaxBytes {
		t.Fatalf("fixture is %d bytes, want over the refusal bound", len(long))
	}
	_, session, _ := echoLogSession(t)
	for _, name := range []string{"recall_project", "record_decision"} {
		args := map[string]string{"project_id": long}
		if name == "record_decision" {
			args["topic"] = "caching"
		}
		res, err := session.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("GetPrompt %s: %v", name, err)
		}
		var text string
		for _, msg := range res.Messages {
			if c, ok := msg.Content.(*mcp.TextContent); ok {
				text += c.Text
			}
		}
		if !strings.Contains(text, long) || strings.Contains(text, "withheld") {
			t.Errorf("%s did not render the %d-byte project id whole: %.300s", name, len(long), text)
		}
	}
}

func TestAFractionalPriorityRefusalDoesNotRepeatTheNumber(t *testing.T) {
	_, session, _ := echoLogSession(t)
	callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "p", "content": "seed fact for the project"})
	digits := "2." + strings.Repeat("5", refusalBigValue)
	for tool, args := range map[string]map[string]any{
		"ghost_task_create": {"project_id": "p", "title": "t", "priority": digits},
		"ghost_task_update": {"task_id": "t", "priority": digits},
	} {
		out := callToolErr(t, session, tool, args)
		if !strings.Contains(out, "must be an integer") {
			t.Fatalf("%s did not reach the integer refusal: %.200s", tool, out)
		}
		if len(out) > 2048 {
			t.Errorf("%s repeated the number: %d bytes", tool, len(out))
		}
	}
}
