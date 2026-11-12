package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
)

func wireErrorOf(t *testing.T, what string, err error) *jsonrpc.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: got success, want a JSON-RPC error", what)
	}
	var wire *jsonrpc.Error
	if !errors.As(err, &wire) {
		t.Fatalf("%s: error %v (%T) is not a JSON-RPC error", what, err, err)
	}
	return wire
}

func dataURI(t *testing.T, wire *jsonrpc.Error) (string, bool) {
	t.Helper()
	if len(wire.Data) == 0 {
		return "", false
	}
	var data struct {
		URI *string `json:"uri"`
	}
	if err := json.Unmarshal(wire.Data, &data); err != nil {
		t.Fatalf("error data %s is not a JSON object: %v", wire.Data, err)
	}
	if data.URI == nil {
		return "", false
	}
	return *data.URI, true
}

func readResourceURI(t *testing.T, session *mcp.ClientSession, uri string) string {
	t.Helper()
	rr, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		t.Fatalf("ReadResource(%q): %v", uri, err)
	}
	var sb strings.Builder
	for _, c := range rr.Contents {
		sb.WriteString(c.Text)
	}
	return sb.String()
}

func promptText(t *testing.T, session *mcp.ClientSession, name string, args map[string]string) string {
	t.Helper()
	res, err := session.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("GetPrompt %s: %v", name, err)
	}
	var sb strings.Builder
	for _, m := range res.Messages {
		if c, ok := m.Content.(*mcp.TextContent); ok {
			sb.WriteString(c.Text)
		}
	}
	return sb.String()
}

func TestProjectResources_UnknownProjectIsResourceNotFound(t *testing.T) {
	_, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)
	for _, tail := range []string{"tasks", "decisions"} {
		uri := "ghost://project/neverseen/" + tail
		wire := wireErrorOf(t, uri, func() error {
			_, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri})
			return err
		}())
		if wire.Code != jsonrpc.CodeInvalidParams || !strings.HasPrefix(wire.Message, "Resource not found") || !strings.Contains(wire.Message, `project "neverseen" not found`) {
			t.Errorf("unexpected error: code %d %q", wire.Code, wire.Message)
		}
		if got, ok := dataURI(t, wire); !ok || got != uri {
			t.Errorf("data.uri = %q (present %v), want %q", got, ok, uri)
		}
	}
}

func TestProjectResources_MissingProjectIDIsInvalidParams(t *testing.T) {
	_, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)
	for _, tail := range []string{"context", "tasks", "decisions"} {
		uri := "ghost://project//" + tail
		wire := wireErrorOf(t, uri, func() error {
			_, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri})
			return err
		}())
		if wire.Code != jsonrpc.CodeInvalidParams || !strings.Contains(wire.Message, "resource URI missing project_id") || strings.HasPrefix(wire.Message, "Resource not found") {
			t.Errorf("unexpected error: code %d %q data=%s", wire.Code, wire.Message, wire.Data)
		}
		if _, ok := dataURI(t, wire); ok {
			t.Errorf("malformed URI carries resource data: %s", wire.Data)
		}
	}
}

func TestTemplates_MissingArgumentsAreInvalidParams(t *testing.T) {
	_, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)
	for _, tc := range []struct {
		name, want string
		args       map[string]string
	}{
		{"recall_project", "project_id argument is required", nil},
		{"recall_project", "project_id argument is required", map[string]string{"project_id": ""}},
		{"record_decision", "project_id and topic arguments are required", map[string]string{"project_id": "test-project"}},
		{"record_decision", "project_id and topic arguments are required", map[string]string{"topic": "storage"}},
	} {
		_, err := session.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: tc.name, Arguments: tc.args})
		wire := wireErrorOf(t, tc.name, err)
		if wire.Code != jsonrpc.CodeInvalidParams || !strings.Contains(wire.Message, tc.want) {
			t.Errorf("%s %v: code=%d msg=%q", tc.name, tc.args, wire.Code, wire.Message)
		}
	}
}

func TestProjectResourcesAndTemplates_SuccessPathsUnchanged(t *testing.T) {
	db, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)
	plantTask(t, db, "abc123", "T1", "a real task", "d", "pending")
	plantDecision(t, db, "abc123", "D1", "a real decision", "we chose A", "for a reason", "active")

	if got := readResourceURI(t, session, "ghost://project/test-project/tasks"); !strings.Contains(got, "a real task") {
		t.Errorf("tasks resource stopped listing:\n%s", got)
	}
	if got := readResourceURI(t, session, "ghost://project/test-project/decisions"); !strings.Contains(got, "a real decision") {
		t.Errorf("decisions resource stopped listing:\n%s", got)
	}
	if got := readResourceURI(t, session, "ghost://project/test-project/context"); !strings.Contains(got, "a real decision") {
		t.Errorf("context resource lost its Recent Decisions:\n%s", got)
	}
	if got := readResourceURI(t, session, "ghost://project/neverseen/context"); !strings.Contains(got, "neverseen") || !strings.Contains(got, "not registered") {
		t.Errorf("unknown project context answer changed:\n%s", got)
	}
	if got := promptText(t, session, "recall_project", map[string]string{"project_id": "test-project"}); !strings.Contains(got, "a real decision") {
		t.Errorf("recall_project lost the project block:\n%s", got)
	}
	if got := promptText(t, session, "recall_project", map[string]string{"project_id": "neverseen"}); !strings.Contains(got, "not registered") {
		t.Errorf("recall_project unknown project answer changed:\n%s", got)
	}
	if got := promptText(t, session, "record_decision", map[string]string{"project_id": "test-project", "topic": "storage"}); !strings.Contains(got, "ghost_decision_record") {
		t.Errorf("record_decision lost its guidance:\n%s", got)
	}
}

func TestProjectResources_NotFoundDataWithholdsATokenShapedID(t *testing.T) {
	_, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)
	const token = "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"
	for _, tc := range []struct{ name, id string }{
		{"bare token", token},
		{"clone URL with a token", "https%3A%2F%2Fx-oauth%3A" + token + "%40github.com%2Fo%2Fr"},
		{"over the echo bound", strings.Repeat("a", memory.ArgEchoMaxBytes+1)},
	} {
		uri := "ghost://project/" + tc.id + "/tasks"
		wire := wireErrorOf(t, tc.name, func() error {
			_, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri})
			return err
		}())
		if wire.Code != jsonrpc.CodeInvalidParams || !strings.HasPrefix(wire.Message, "Resource not found") {
			t.Errorf("%s: not the resource-not-found error: code %d %q", tc.name, wire.Code, wire.Message)
		}
		if strings.Contains(wire.Message, token) || strings.Contains(wire.Message, tc.id) || strings.Contains(string(wire.Data), token) || strings.Contains(string(wire.Data), tc.id) || len(wire.Data) != 0 {
			t.Errorf("%s: withheld id leaked: %q %s", tc.name, wire.Message, wire.Data)
		}
	}
}

func TestParseProjectIDFromURI_BadEncodingIsInvalidParams(t *testing.T) {
	_, err := parseProjectIDFromURI("ghost://project/%zz/tasks")
	var wire *jsonrpc.Error
	if !errors.As(err, &wire) || wire.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("error %v (%T) is not invalid-params", err, err)
	}
}
