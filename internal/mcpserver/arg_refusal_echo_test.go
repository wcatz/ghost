package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/provider"
)

const refusalBigValue = 200 * 1024

func assertNoRefusalEcho(t *testing.T, out, v string) {
	t.Helper()
	if strings.Contains(out, echoToken) || (len(v) > 64 && strings.Contains(out, v[:64])) {
		t.Errorf("refusal echoed value: %.200s", out)
	}
	if len(out) > 2048 {
		t.Errorf("refusal unbounded: %d bytes", len(out))
	}
}

func TestArgumentRefusalsNeverEchoATokenOrAnUnboundedValue(t *testing.T) {
	big := strings.Repeat("z", refusalBigValue)
	values := map[string]string{"token": echoToken, "big": big}
	save := func(extra map[string]any) map[string]any {
		a := map[string]any{"project_id": "p", "content": "a fact worth keeping"}
		for k, v := range extra {
			a[k] = v
		}
		return a
	}
	cases := []struct {
		tool string
		args func(v string) map[string]any
	}{
		{"ghost_memory_save", func(v string) map[string]any { return save(map[string]any{"tags": v}) }},
		{"ghost_memory_save", func(v string) map[string]any { return save(map[string]any{"tags": map[string]any{v: v}}) }},
		{"ghost_memory_save", func(v string) map[string]any { return save(map[string]any{"tags": []any{map[string]any{v: v}}}) }},
		{"ghost_memory_save", func(v string) map[string]any { return save(map[string]any{"importance": v}) }},
		{"ghost_memory_save", func(v string) map[string]any { return save(map[string]any{"importance": map[string]any{v: v}}) }},
		{"ghost_memory_save", func(v string) map[string]any { return save(map[string]any{"category": v}) }},
		{"ghost_memory_update", func(v string) map[string]any {
			return map[string]any{"project_id": "p", "memory_id": "m", "category": v}
		}},
		{"ghost_task_update", func(v string) map[string]any { return map[string]any{"task_id": "t", "status": v} }},
		{"ghost_task_list", func(v string) map[string]any { return map[string]any{"project_id": "p", "status": v} }},
		{"ghost_decisions_list", func(v string) map[string]any { return map[string]any{"project_id": "p", "status": v} }},
		{"ghost_memories_list", func(v string) map[string]any { return map[string]any{"project_id": "p", "category": v} }},
		{"ghost_memories_list", func(v string) map[string]any { return map[string]any{"project_id": "p", "retention": v} }},
		{"ghost_link_withdraw", func(v string) map[string]any {
			return map[string]any{"project_id": "p", "source_id": "s", "target_id": "t", "relation": v}
		}},
		{"ghost_memory_search", func(v string) map[string]any { return map[string]any{"project_id": "p", "query": "q", "as_of": v} }},
		{"ghost_memory_save", func(v string) map[string]any { return save(map[string]any{"valid_from": v}) }},
		{"ghost_memory_save", func(v string) map[string]any { return save(map[string]any{"valid_from": map[string]any{v: v}}) }},
	}
	for i, tc := range cases {
		for label, v := range values {
			t.Run(fmt.Sprintf("%d/%s", i, label), func(t *testing.T) {
				_, session, _ := echoLogSession(t)
				assertNoRefusalEcho(t, callToolErr(t, session, tc.tool, tc.args(v)), v)
			})
		}
	}
}

func TestStoreBackedRefusalsNeverEchoATokenOrAnUnboundedValue(t *testing.T) {
	big := strings.Repeat("z", refusalBigValue)
	for label, v := range map[string]string{"token": echoToken, "big": big} {
		t.Run("memory ops/"+label, func(t *testing.T) {
			db, session, _ := echoLogSession(t)
			callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "p", "content": "seed fact for the project"})
			var seed string
			_ = db.QueryRow(`SELECT id FROM memories WHERE project_id = 'p' LIMIT 1`).Scan(&seed)
			for _, tc := range []struct {
				tool string
				args map[string]any
			}{
				{"ghost_memory_update", map[string]any{"project_id": "p", "memory_id": v, "content": "c"}},
				{"ghost_memory_delete", map[string]any{"project_id": "p", "memory_id": v}},
				{"ghost_memory_promote", map[string]any{"project_id": "p", "memory_id": v}},
				{"ghost_memory_pin", map[string]any{"project_id": "p", "memory_id": v, "pinned": true}},
				{"ghost_memory_flag", map[string]any{"project_id": "p", "memory_id": v, "kind": "wrong", "reason": "r"}},
				{"ghost_memory_delete", map[string]any{"project_id": "p", "memory_id": v, "purge_history": true}},
				{"ghost_link_withdraw", map[string]any{"project_id": "p", "source_id": seed, "target_id": v}},
				{"ghost_link_withdraw", map[string]any{"project_id": "p", "source_id": v, "target_id": seed}},
				{"ghost_resolve_mark", map[string]any{"project_id": "p", "memory_ids": []any{v}}},
			} {
				assertNoRefusalEcho(t, callToolErr(t, session, tc.tool, tc.args), v)
			}
		})
		t.Run("task/tag/project/schema/"+label, func(t *testing.T) {
			db, session, _ := echoLogSession(t)
			callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "p", "content": "seed fact for the project"})
			callTool(t, session, "ghost_task_create", map[string]any{"project_id": "p", "title": "seed task"})
			var seed string
			_ = db.QueryRow(`SELECT id FROM memories WHERE project_id = 'p' LIMIT 1`).Scan(&seed)
			assertNoRefusalEcho(t, callToolErr(t, session, "ghost_task_complete", map[string]any{"task_id": v}), v)
			assertNoRefusalEcho(t, callToolErr(t, session, "ghost_task_update", map[string]any{"task_id": v, "priority": 1}), v)
			tag := "`" + v
			for _, tc := range []struct {
				tool string
				args map[string]any
			}{
				{"ghost_memory_save", map[string]any{"project_id": "p", "content": "a fact worth keeping", "tags": []any{"ordinary", tag}}},
				{"ghost_save_global", map[string]any{"content": "a fact worth keeping", "tags": []any{"ordinary", tag}}},
				{"ghost_memory_update", map[string]any{"project_id": "p", "memory_id": seed, "tags": []any{"ordinary", tag}}},
				{"ghost_decision_record", map[string]any{"project_id": "p", "title": "a decision", "decision": "we chose", "rationale": "because", "tags": []any{"ordinary", tag}}},
			} {
				assertNoRefusalEcho(t, callToolErr(t, session, tc.tool, tc.args), tag)
			}
			assertNoRefusalEcho(t, callToolErr(t, session, "ghost_memory_flag", map[string]any{"project_id": "p", "memory_id": seed, "kind": v, "reason": "r"}), v)
			assertNoRefusalEcho(t, callToolErr(t, session, "ghost_save_global", map[string]any{"content": "a fact worth keeping", "category": v}), v)
			assertNoRefusalEcho(t, callToolErr(t, session, "ghost_memory_pin", map[string]any{"project_id": v, "memory_id": "m", "pinned": true}), v)
			assertNoRefusalEcho(t, resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": v, "as_of": "2026-01-01T00:00:00Z"})), v)
			for _, args := range []map[string]any{{"project_id": "p", "category": v}, {"project_id": "p", "retention": v}, {"project_id": "p", "category": v, "retention": v}} {
				assertNoRefusalEcho(t, resultText(callTool(t, session, "ghost_memories_list", args)), v)
			}
			assertNoRefusalEcho(t, callToolErr(t, session, "ghost_memory_save", map[string]any{"project_id": "\u00ab" + v, "content": "a fact worth keeping"}), v)
		})
	}
	t.Run("subscribe and client name", func(t *testing.T) {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		db, err := memory.OpenDB(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		srv := New(memory.NewStore(db, logger), logger, "test")
		err = srv.handleSubscribe(context.Background(), &mcp.SubscribeRequest{Params: &mcp.SubscribeParams{URI: "http://" + echoToken}})
		if err == nil {
			t.Fatal("subscribe was not refused")
		}
		assertNoRefusalEcho(t, err.Error(), echoToken)
		old := detectCallingSource
		detectCallingSource = func() string { return "" }
		t.Cleanup(func() { detectCallingSource = old })
		session := connectedClientNamed(t, srv, echoToken)
		callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "p", "content": "seed fact for the project"})
		assertNoRefusalEcho(t, callToolErr(t, session, "ghost_resolve", map[string]any{"project": "p"}), echoToken)
	})
	t.Run("purge on store without history", func(t *testing.T) {
		db, err := memory.OpenDB(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		session := connectedClient(t, New(struct{ provider.MemoryStore }{memory.NewStore(db, logger)}, logger, "test"))
		callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "p", "content": "seed fact for the project"})
		assertNoRefusalEcho(t, callToolErr(t, session, "ghost_memory_delete", map[string]any{"project_id": "p", "memory_id": echoToken, "purge_history": true}), echoToken)
	})
}

func schemaRefusalArgs(extra map[string]any) map[string]any {
	args := map[string]any{"project_id": "p", "content": "a fact worth keeping"}
	for k, v := range extra {
		args[k] = v
	}
	return args
}

func TestSchemaRefusalsNeverEchoATokenOrAnUnboundedValue(t *testing.T) {
	big := strings.Repeat("z", refusalBigValue)
	for label, v := range map[string]string{"token": echoToken, "big": big} {
		t.Run("wrong type/"+label, func(t *testing.T) {
			_, session, _ := echoLogSession(t)
			out := callToolErr(t, session, "ghost_memory_save", schemaRefusalArgs(map[string]any{"pin": v}))
			assertNoRefusalEcho(t, out, v)
		})
		t.Run("unexpected property/"+label, func(t *testing.T) {
			_, session, _ := echoLogSession(t)
			out := callToolErr(t, session, "ghost_memory_save", schemaRefusalArgs(map[string]any{v: "x"}))
			assertNoRefusalEcho(t, out, v)
		})
		t.Run("nested object/"+label, func(t *testing.T) {
			_, session, _ := echoLogSession(t)
			out := callToolErr(t, session, "ghost_memory_save", schemaRefusalArgs(map[string]any{"pin": map[string]any{"held": v}}))
			assertNoRefusalEcho(t, out, v)
		})
	}
}

func TestSchemaRefusalStillNamesAnOrdinaryValueAndProperty(t *testing.T) {
	_, session, _ := echoLogSession(t)
	out := callToolErr(t, session, "ghost_memory_save", schemaRefusalArgs(map[string]any{"colour": "blue"}))
	if !strings.Contains(out, "colour") || !strings.Contains(out, "unexpected additional properties") {
		t.Errorf("unexpected refusal: %.300s", out)
	}
	_, session2, _ := echoLogSession(t)
	out2 := callToolErr(t, session2, "ghost_memory_save", schemaRefusalArgs(map[string]any{"pin": "yes"}))
	if !strings.Contains(out2, "yes") || !strings.Contains(out2, `has type "string", want "boolean"`) {
		t.Errorf("unexpected refusal: %.300s", out2)
	}
}

func TestSchemaRefusalWithManyUnexpectedPropertiesIsBounded(t *testing.T) {
	extra := map[string]any{}
	for i := range 200 {
		extra[fmt.Sprintf("unexpected_property_%d", i)] = "v"
	}
	_, session, _ := echoLogSession(t)
	out := callToolErr(t, session, "ghost_memory_save", schemaRefusalArgs(extra))
	if len(out) > 2048 || !strings.Contains(out, "unexpected additional properties") {
		t.Errorf("unexpected refusal: len=%d %.300s", len(out), out)
	}
}

const sdkSchemaRefusalPrefix = `validating "arguments": `

func TestTheSDKSchemaRefusalStillStartsWithThePrefixTheGuardKeysOn(t *testing.T) {
	_, session, _ := echoLogSession(t)
	out := callToolErr(t, session, "ghost_memory_save", schemaRefusalArgs(map[string]any{"pin": "not a boolean"}))
	if !strings.HasPrefix(out, sdkSchemaRefusalPrefix) {
		t.Errorf("prefix changed: %.300s", out)
	}
}

type toolSchema struct {
	name         string
	refusesExtra bool
	wrongTyped   []string
}

func listToolSchemas(t *testing.T, session *mcp.ClientSession) []toolSchema {
	t.Helper()
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]toolSchema, 0, len(res.Tools))
	for _, tool := range res.Tools {
		schema, _ := tool.InputSchema.(map[string]any)
		listed := toolSchema{name: tool.Name}
		if extra, ok := schema["additionalProperties"].(bool); ok && !extra {
			listed.refusesExtra = true
		}
		properties, _ := schema["properties"].(map[string]any)
		for name, raw := range properties {
			property, _ := raw.(map[string]any)
			if !acceptsString(property) {
				listed.wrongTyped = append(listed.wrongTyped, name)
			}
		}
		sort.Strings(listed.wrongTyped)
		out = append(out, listed)
	}
	return out
}

func acceptsString(property map[string]any) bool {
	switch declared := property["type"].(type) {
	case string:
		return declared == "" || declared == "string"
	case []any:
		for _, t := range declared {
			if name, ok := t.(string); ok && name == "string" {
				return true
			}
		}
		return len(declared) == 0
	}
	return true
}

func TestEveryToolRefusesUnexpectedTokenAndWrongTypedToken(t *testing.T) {
	_, session, _ := echoLogSession(t)
	tools := listToolSchemas(t, session)
	for _, tool := range tools {
		if tool.refusesExtra {
			out := callToolErr(t, session, tool.name, map[string]any{echoToken: "x"})
			assertNoRefusalEcho(t, out, echoToken)
		}
		for _, property := range tool.wrongTyped {
			out := callToolErr(t, session, tool.name, map[string]any{property: echoToken})
			assertNoRefusalEcho(t, out, echoToken)
		}
	}
}

func TestArgumentRefusalsStillNameTheArgumentAndTheShape(t *testing.T) {
	for _, tc := range []struct {
		call func() error
		want []string
	}{
		{func() error { _, err := optStringSlice("not json", "tags"); return err }, []string{"tags", "array of strings"}},
		{func() error { _, err := optStringSlice([]any{"ok", 7.0}, "tags"); return err }, []string{"tags", "element 2"}},
		{func() error { _, err := toFloat64("high", "importance"); return err }, []string{"importance", "number"}},
		{func() error { _, err := parseStampArg("valid_from", "soon", false); return err }, []string{"valid_from", "RFC 3339"}},
		{func() error { _, err := parseStampArg("valid_from", 5.0, false); return err }, []string{"valid_from", "string timestamp"}},
		{func() error { _, err := parseAsOf("yesterday"); return err }, []string{"as_of", "RFC 3339"}},
		{func() error { _, err := toFloat64(json.Number("1.2.3"), "importance"); return err }, []string{"importance", "number"}},
	} {
		err := tc.call()
		if err == nil {
			t.Fatal("not refused")
		}
		for _, w := range tc.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("refusal %q missing %q", err.Error(), w)
			}
		}
	}
}

func TestPromptsRenderALongProjectPathWholeAndRefusalsDoNot(t *testing.T) {
	long := "/srv/" + strings.Repeat("d", 300) + "/ghost"
	_, session, _ := echoLogSession(t)
	for _, name := range []string{"recall_project", "record_decision"} {
		args := map[string]string{"project_id": long}
		if name == "record_decision" {
			args["topic"] = "caching"
		}
		res, err := session.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		var text string
		for _, msg := range res.Messages {
			if c, ok := msg.Content.(*mcp.TextContent); ok {
				text += c.Text
			}
		}
		if !strings.Contains(text, long) {
			t.Errorf("%s did not render long id whole", name)
		}
	}
}

func TestAFractionalPriorityRefusalDoesNotRepeatTheNumber(t *testing.T) {
	_, session, _ := echoLogSession(t)
	callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "p", "content": "seed fact for the project"})
	digits := "2." + strings.Repeat("5", 2000)
	for tool, args := range map[string]map[string]any{
		"ghost_task_create": {"project_id": "p", "title": "t", "priority": digits},
		"ghost_task_update": {"task_id": "t", "priority": digits},
	} {
		out := callToolErr(t, session, tool, args)
		if !strings.Contains(out, "must be an integer") || len(out) > 2048 {
			t.Errorf("%s unexpected refusal: len=%d %q", tool, len(out), out)
		}
	}
}
