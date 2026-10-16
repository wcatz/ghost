package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The project resources and the recall_project and record_decision templates
// used to return plain Go errors, which the SDK puts on the wire with JSON-RPC
// code 0 — outside every defined error class, so a client could not tell a
// missing project or a bad argument from an internal failure. These tests read
// the error back through a real client session and assert the code, the
// message and the data a client would see.

// wireErrorOf extracts the JSON-RPC error a client received, failing the test
// when the call did not fail or failed with something that is not one.
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

// dataURI decodes the "uri" member of a resource-not-found error's data, and
// reports whether the data carried one.
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

func TestProjectResources_UnknownProjectIsResourceNotFound(t *testing.T) {
	_, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)

	for _, tail := range []string{"tasks", "decisions"} {
		t.Run(tail, func(t *testing.T) {
			uri := "ghost://project/neverseen/" + tail
			_, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri})
			wire := wireErrorOf(t, uri, err)
			if wire.Code != jsonrpc.CodeInvalidParams {
				t.Errorf("code = %d, want %d (resource not found): %q", wire.Code, jsonrpc.CodeInvalidParams, wire.Message)
			}
			if !strings.HasPrefix(wire.Message, "Resource not found") {
				t.Errorf("message %q does not lead with the SDK's %q", wire.Message, "Resource not found")
			}
			if !strings.Contains(wire.Message, `project "neverseen" not found`) {
				t.Errorf("message %q no longer names the project", wire.Message)
			}
			got, ok := dataURI(t, wire)
			if !ok || got != uri {
				t.Errorf("data.uri = %q (present %v), want %q; data %s", got, ok, uri, wire.Data)
			}
		})
	}
}

func TestProjectResources_MissingProjectIDIsInvalidParams(t *testing.T) {
	_, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)

	for _, tail := range []string{"context", "tasks", "decisions"} {
		t.Run(tail, func(t *testing.T) {
			uri := "ghost://project//" + tail
			_, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri})
			wire := wireErrorOf(t, uri, err)
			if wire.Code != jsonrpc.CodeInvalidParams {
				t.Errorf("code = %d, want %d (invalid params): %q", wire.Code, jsonrpc.CodeInvalidParams, wire.Message)
			}
			if !strings.Contains(wire.Message, "resource URI missing project_id") {
				t.Errorf("message %q no longer says what is wrong with the URI", wire.Message)
			}
			// A malformed URI is a bad argument, not a missing resource, so it
			// carries the invalid-params shape and no resource data.
			if strings.HasPrefix(wire.Message, "Resource not found") {
				t.Errorf("a malformed URI was answered as a missing resource: %q", wire.Message)
			}
			if got, ok := dataURI(t, wire); ok {
				t.Errorf("a malformed URI carries resource data uri %q", got)
			}
		})
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
		if wire.Code != jsonrpc.CodeInvalidParams {
			t.Errorf("%s %v: code = %d, want %d (invalid params): %q", tc.name, tc.args, wire.Code, jsonrpc.CodeInvalidParams, wire.Message)
		}
		if !strings.Contains(wire.Message, tc.want) {
			t.Errorf("%s %v: message %q does not say %q", tc.name, tc.args, wire.Message, tc.want)
		}
	}
}

// The controls: every success path reads exactly as it did, and an unknown
// project on the context resource and on recall_project is still an answer —
// its `## Global` half does not depend on the project, and it is the block a
// client pins at the first session in a new project.
func TestProjectResourcesAndTemplates_SuccessPathsUnchanged(t *testing.T) {
	db, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)
	plantTask(t, db, "abc123", "T1", "a real task", "d", "pending")
	plantDecision(t, db, "abc123", "D1", "a real decision", "we chose A", "for a reason", "active")

	read := func(uri string) string {
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
	if got := read("ghost://project/test-project/tasks"); !strings.Contains(got, "a real task") {
		t.Errorf("tasks resource stopped listing:\n%s", got)
	}
	if got := read("ghost://project/test-project/decisions"); !strings.Contains(got, "a real decision") {
		t.Errorf("decisions resource stopped listing:\n%s", got)
	}
	if got := read("ghost://project/test-project/context"); !strings.Contains(got, "a real decision") {
		t.Errorf("context resource lost its Recent Decisions:\n%s", got)
	}
	if got := read("ghost://project/neverseen/context"); !strings.Contains(got, "neverseen") || !strings.Contains(got, "not registered") {
		t.Errorf("an unknown project on the context resource is no longer the not-registered answer:\n%s", got)
	}

	prompt := func(name string, args map[string]string) string {
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
	if got := prompt("recall_project", map[string]string{"project_id": "test-project"}); !strings.Contains(got, "a real decision") {
		t.Errorf("recall_project lost the project block:\n%s", got)
	}
	if got := prompt("recall_project", map[string]string{"project_id": "neverseen"}); !strings.Contains(got, "not registered") {
		t.Errorf("recall_project on an unknown project is no longer the not-registered answer:\n%s", got)
	}
	if got := prompt("record_decision", map[string]string{"project_id": "test-project", "topic": "storage"}); !strings.Contains(got, "ghost_decision_record") {
		t.Errorf("record_decision lost its guidance:\n%s", got)
	}
}

// The resource-not-found data carries the URI, and the URI carries the project
// argument — which is routinely a clone URL with a token in it. The data must
// withhold it exactly as the message does.
func TestProjectResources_NotFoundDataWithholdsATokenShapedID(t *testing.T) {
	_, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)

	const token = "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"
	for _, id := range []string{
		token,
		"https%3A%2F%2Fx-oauth%3A" + token + "%40github.com%2Fo%2Fr",
	} {
		uri := "ghost://project/" + id + "/tasks"
		_, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri})
		wire := wireErrorOf(t, uri, err)
		if strings.Contains(wire.Message, token) {
			t.Errorf("message echoes the token: %q", wire.Message)
		}
		if strings.Contains(string(wire.Data), token) {
			t.Errorf("data echoes the token: %s", wire.Data)
		}
		if _, ok := dataURI(t, wire); !ok {
			t.Errorf("data lost its uri member when the value was withheld: %s", wire.Data)
		}
	}
}
