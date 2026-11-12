package mcpserver

// #839, at the surfaces: no answer may quote a credential-shaped project

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/memory"
)

const echoToken = "ghp_" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

const storeAmbiguityLabel = memory.ProjectIdentifierLabel

// #836 would show; putting it here would mean one table asserting two different
// and since #956 the reason is the same MECHANISM: the tool resolves its project
var projectResolvingSurfaces = []struct {
	name            string
	argKey, argName string
	read            func(*testing.T, *mcp.ClientSession, string) string
	neverResolves   bool
	storeRefusal    bool
}{
	{
		name: "ghost_memory_search", argKey: "project_id", argName: "project_id",
		read: toolReader("ghost_memory_search", "project_id", map[string]any{"query": "anything"}),
	},
	{
		name: "ghost_project_context", argKey: "project_id", argName: "project_id",
		read: toolReader("ghost_project_context", "project_id", nil),
	},
	{
		name: "ghost_memories_list", argKey: "project_id", argName: "project_id",
		read: toolReader("ghost_memories_list", "project_id", nil),
	},
	{
		name: "ghost_memory_delete", argKey: "project_id", argName: "project_id",
		read: toolReader("ghost_memory_delete", "project_id", map[string]any{"memory_id": "AABBCCDD"}),
	},
	{
		name: "ghost_memory_update", argKey: "project_id", argName: "project_id",
		read: toolReader("ghost_memory_update", "project_id", map[string]any{"memory_id": "AABBCCDD", "content": "an edit"}),
	},
	{
		name: "ghost_memory_promote", argKey: "project_id", argName: "project_id",
		read: toolReader("ghost_memory_promote", "project_id", map[string]any{"memory_id": "AABBCCDD"}),
	},
	{
		name: "ghost_memory_pin", argKey: "project_id", argName: "project_id",
		read: toolReader("ghost_memory_pin", "project_id", map[string]any{"memory_id": "AABBCCDD", "pinned": true}),
	},
	{
		name: "ghost_memory_flag", argKey: "project_id", argName: "project_id",
		read: toolReader("ghost_memory_flag", "project_id", map[string]any{
			"memory_id": "AABBCCDD", "kind": "wrong", "reason": "it is wrong",
		}),
	},
	{
		name: "ghost_decisions_list", argKey: "project_id", argName: "project_id",
		read: toolReader("ghost_decisions_list", "project_id", nil),
	},
	{
		name: "ghost_task_create", argKey: "project_id", argName: "project_id",
		read: toolReader("ghost_task_create", "project_id", map[string]any{"title": "a task"}),
	},
	{
		name: "ghost_task_list", argKey: "project_id", argName: "project_id",
		read: toolReader("ghost_task_list", "project_id", nil),
	},
	{
		name: "ghost_resolve_mark", argKey: "project_id", argName: "project_id",
		read: toolReader("ghost_resolve_mark", "project_id", map[string]any{"memory_ids": []string{"AABBCCDD"}}),
	},
	{
		name: "ghost_link_withdraw", argKey: "project_id", argName: "project_id",
		read: toolReader("ghost_link_withdraw", "project_id", map[string]any{
			"source_id": "AABBCCDD", "target_id": "EEFF0011",
		}),
	},
	{
		name: "ghost_resolve", argKey: "project", argName: "project",
		read: toolReader("ghost_resolve", "project", nil),
	},
	{
		name: "ghost_project_delete", argKey: "project", argName: "project",
		read:         toolReader("ghost_project_delete", "project", nil),
		storeRefusal: true,
	},
	{name: "ghost://project/{id}/context", argKey: "project_id", argName: "project_id", read: resourceReader("context")},
	{name: "ghost://project/{id}/tasks", argKey: "project_id", argName: "project_id", read: resourceReader("tasks")},
	{name: "ghost://project/{id}/decisions", argKey: "project_id", argName: "project_id", read: resourceReader("decisions")},
	{name: "recall_project", argKey: "project_id", argName: "project_id", read: promptReader("recall_project", nil)},
	{
		name: "record_decision", argKey: "project_id", argName: "project_id",
		read:          promptReader("record_decision", map[string]string{"topic": "a topic"}),
		neverResolves: true,
	},
}

// instead of discarded, because #839 has two destinations and not one: the answer
func echoLogSession(t *testing.T) (*sql.DB, *mcp.ClientSession, *bytes.Buffer) {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	store := memory.NewStore(db, logger)
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`); err != nil {
		t.Fatalf("insert _global project: %v", err)
	}
	session := connectedClient(t, New(store, logger, "test"))
	logs.Reset()
	return db, session, logs
}

func toolReader(tool, argKey string, base map[string]any) func(*testing.T, *mcp.ClientSession, string) string {
	return func(t *testing.T, session *mcp.ClientSession, project string) string {
		t.Helper()
		args := map[string]any{}
		for k, v := range base {
			args[k] = v
		}
		args[argKey] = project
		return resultText(callTool(t, session, tool, args))
	}
}

func resourceReader(tail string) func(*testing.T, *mcp.ClientSession, string) string {
	return func(t *testing.T, session *mcp.ClientSession, project string) string {
		t.Helper()
		rr, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{
			URI: "ghost://project/" + project + "/" + tail,
		})
		if err != nil {
			return err.Error()
		}
		var parts []string
		for _, c := range rr.Contents {
			parts = append(parts, c.Text)
		}
		return strings.Join(parts, "\n")
	}
}

func promptReader(name string, extra map[string]string) func(*testing.T, *mcp.ClientSession, string) string {
	return func(t *testing.T, session *mcp.ClientSession, project string) string {
		t.Helper()
		args := map[string]string{"project_id": project}
		for k, v := range extra {
			args[k] = v
		}
		res, err := session.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: name, Arguments: args})
		if err != nil {
			return err.Error()
		}
		parts := []string{res.Description}
		for _, msg := range res.Messages {
			if c, ok := msg.Content.(*mcp.TextContent); ok {
				parts = append(parts, c.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
}

// TestACredentialShapedProjectIDIsNeverEchoedBackByAResolvingSurface is #839.
func TestACredentialShapedProjectIDIsNeverEchoedBackByAResolvingSurface(t *testing.T) {
	for _, shape := range []struct {
		name           string
		argument       func(*testing.T, *sql.DB) string
		label          string
		uriAddressable bool
		needsPlanting  bool
	}{
		{name: "ambiguous by name", argument: ambiguousByName, label: storeAmbiguityLabel, uriAddressable: true, needsPlanting: true},
		{name: "ambiguous by tied path prefix", argument: ambiguousByPathPrefix, label: storeAmbiguityLabel, needsPlanting: true},
		{name: "unknown project", argument: unknownProject, uriAddressable: true, needsPlanting: false},
	} {
		t.Run(shape.name, func(t *testing.T) {
			for _, surface := range projectResolvingSurfaces {
				t.Run(surface.name, func(t *testing.T) {
					if shape.needsPlanting && surface.neverResolves {
						t.Skip("this surface never resolves its project argument, so no reference can be " +
							"ambiguous for it; the unknown-project shape covers what it does with the value")
					}
					if strings.HasPrefix(surface.name, "ghost://") && !shape.uriAddressable {
						t.Skip("a tied path-prefix reference is a directory path, which no URI template's " +
							"single-segment project_id can carry; the name shape reaches this surface's resolver")
					}
					db, session, logs := echoLogSession(t)
					argument := shape.argument(t, db)

					out := surface.read(t, session, argument)

					assertNoCredentialEcho(t, surface.name, argument, out)
					if logged := logs.String(); logged != "" {
						assertNoCredentialEcho(t, surface.name+" (log)", argument, logged)
					}

					if !strings.Contains(out, "GitHub personal access token") {
						t.Errorf("%s withheld the value without naming what it found (answer: %s)", surface.name, out)
					}

					want := shape.label
					if want == "" {
						want = surface.argName
						if surface.storeRefusal {
							want = storeAmbiguityLabel
						}
					}
					placeholder := `"<` + want + ` withheld: it holds a `
					if !strings.Contains(out, placeholder) {
						t.Errorf("%s did not withhold the value naming it %q "+
							"(want a sentence containing %s…, answer: %s)",
							surface.name, want, placeholder, out)
					}
				})
			}
		})
	}
}

func assertNoCredentialEcho(t *testing.T, surface, argument, out string) {
	t.Helper()
	if strings.Contains(out, echoToken) {
		t.Errorf("%s echoed the credential back into the answer:\n%s", surface, out)
	}
	if rendered := assemble.Token(argument); strings.Contains(out, rendered) {
		t.Errorf("%s echoed the refused value through the renderer %q:\n%s", surface, rendered, out)
	}
}

func ambiguousByName(t *testing.T, db *sql.DB) string {
	t.Helper()
	for _, id := range []string{"twin-a", "twin-b"} {
		plantProject(t, db, id, echoToken, filepath.Join(t.TempDir(), id))
	}
	return echoToken
}

func ambiguousByPathPrefix(t *testing.T, db *sql.DB) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), echoToken)
	argument := filepath.Join(dir, "sub")
	if err := os.MkdirAll(argument, 0o755); err != nil {
		t.Fatalf("mkdir the session's directory: %v", err)
	}
	plantProject(t, db, "path-one", "infra", dir)
	plantProject(t, db, "path-two", "infra", strings.ReplaceAll(dir, string(filepath.Separator), `\`))
	return argument
}

func unknownProject(t *testing.T, db *sql.DB) string {
	t.Helper()
	_ = db
	return echoToken
}

func TestTheEchoSweepActuallyReadsTheServersLog(t *testing.T) {
	_, session, logs := echoLogSession(t)

	callTool(t, session, "ghost_memory_save", map[string]any{
		"content": "a claim that makes the store log something", "category": "fact", "project_id": "vproj",
	})

	if logs.Len() == 0 {
		t.Fatal("the log capture is dead: a write that does reach the server's logger produced nothing, " +
			"so the sweep's assertion that the log holds no part of a credential would pass on an empty string")
	}
}

func TestAResolvingSurfaceStillNamesAnOrdinaryProjectArgument(t *testing.T) {
	const dup = "ambiguous-project-name"
	for _, surface := range projectResolvingSurfaces {
		t.Run(surface.name, func(t *testing.T) {
			db, _, session := projectShapeSession(t)
			for _, id := range []string{"twin-a", "twin-b"} {
				plantProject(t, db, id, dup, filepath.Join(t.TempDir(), id))
			}

			out := surface.read(t, session, dup)

			if !strings.Contains(out, dup) {
				t.Errorf("%s no longer names an ordinary ambiguous project argument, so the refusal has "+
					"nothing to act on (answer: %s)", surface.name, out)
			}
			if !strings.Contains(out, `"`+dup+`"`) {
				t.Errorf("%s names the argument without quoting it, so it is not the sentence it used to be "+
					"(answer: %s)", surface.name, out)
			}
		})
	}
}
