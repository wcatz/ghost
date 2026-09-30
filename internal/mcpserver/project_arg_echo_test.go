package mcpserver

// #839, at the surfaces: no answer may quote a credential-shaped project
// argument.
//
// The leak is not one place. `Store.ResolveProject` interpolated the caller's own
// argument into three of its refusals, nineteen handlers handed those sentences
// straight back as tool text, and a set of boundary sentences built their own
// `%q` beside them — including two on the `recall_project` prompt that quote the
// argument into the USER message, and a third prompt that never resolves anything
// at all and so had nothing upstream to stop it. An agent reaches every one of
// them the same way, by pasting the session's clone URL — inline credentials
// included — into `project_id`. Fixing the store's strings closes the store's;
// the rest are listed here so a handler added later is caught rather than
// trusted.
//
// Two shapes are walked, because they are different sentences produced by
// different code:
//
//   - a reference that is AMBIGUOUS, which is the store's own refusal and is
//     rendered by `ambiguousProject`, so it names a project IDENTIFIER and not a
//     field;
//   - a reference to a project Ghost has never heard of, which is the boundary's
//     refusal, and which therefore names the field the caller actually used — the
//     only shape that can tell `ghost_resolve`'s `project` from everyone else's
//     `project_id`.
//
// The precondition for the first is narrow, so every case plants an ambiguity
// deliberately, in both the forms the resolver has: a name that two projects
// answer to, and a directory path two spellings tie on.

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

// echoToken is obviously fake and shaped like the class an agent produces by
// pasting a remote URL. It is a GitHub personal access token by the detector's own
// rule (`ghp_` plus 36 characters), which is also a legal POSIX directory name —
// the ambiguous-by-path shape needs a real directory, and naming it after the token
// is what makes the session's reported directory credential-shaped too.
const echoToken = "ghp_" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// storeAmbiguityLabel is the name the store's ambiguity refusal carries, and it is
// deliberately NOT a field name.
//
// `ResolveProject` is handed one string and cannot know what the caller called it:
// `ghost_resolve` passes `project`, every other tool passes `project_id`, the
// resource templates pass a URI segment and the prompt an argument map. So the
// store's own sentence says "project identifier", and the assertions below hold it
// to that — a store refusal that named a field would be telling the agent to fix an
// argument it never passed. The boundary sentences, which do know, are held to the
// caller's own name by the unknown-project shape instead, and that split is the
// rule: whoever knows the field names it, and nobody names one they cannot see.
const storeAmbiguityLabel = memory.ProjectIdentifierLabel

// A surface whose refusal is the STORE's own — because the store built it rather
// than the handler — is held to the neutral label even where the handler knows the
// field. `ghost_project_delete` is that case, and it is here because it is the one
// place the two rules meet: the tool's argument is `project`, and the sentence that
// reaches the agent comes out of `Store.DeleteProject`, which cannot see a tool
// argument and is equally reached by `ghost project delete <name-or-id>`. So it
// renders through `memory.ProjectIdentifierLabel` and the tool name in the wrapping
// error is what tells the agent which argument to change.
//
// This is asserted rather than assumed, because the failure it guards is a sentence
// that looks correct and is not: `ghost_project_delete: project "<project_id
// withheld: …>" not found` withholds the value perfectly and still tells the agent
// to look for a `project_id` argument this tool does not have.

// projectResolvingSurfaces is every surface that takes a project argument before it
// can do anything, with the minimum other arguments each needs. It is the same table
// TestEveryToolThatTakesAProjectIDCannotCreateAProjectTheImporterRefuses walks, plus
// the three resource templates and the two prompts, which carry the argument through
// a URI or an argument map rather than through tool args and so cannot be swept as
// tools.
//
// The list is exhaustive BY CONSTRUCTION rather than by inspection: every entry
// states the argument's key and the field name a refusal should use, because a
// sweep that assumed `project_id` would silently drop whichever surface did not use
// it — and `ghost_resolve` does not.
//
// Three entries are deliberately qualified rather than plain members of the set,
// and each qualification is the honest reason the surface cannot satisfy the whole
// sweep:
//
//   - Two tools are ABSENT. `ghost_task_complete` and `ghost_task_update` take no
//     project argument at all — a task id identifies one task whatever project asked
//     about it — and passing them one is rejected by the tool's own schema before any
//     handler runs. They appear in the sweep
//     TestEveryToolThatTakesAProjectIDCannotCreateAProjectTheImporterRefuses walks,
//     where that rejection happens to satisfy "creates nothing" for the wrong reason;
//     here it would satisfy every assertion for the wrong reason, which is worse than
//     being missing.
//
//   - `ghost_memory_save` is absent for the opposite reason: `ensureProjectFor`
//     asks the credential guard on its own resolve's error path and hands the
//     guard's refusal back untouched — `refusing to store credential-shaped content
//     in project_id: …` — so it produces neither the placeholder these assertions
//     look for nor any of the sentences this change touched. It is held instead by
//     TestACredentialShapedProjectIDIsNeverEchoedBackByTheResolveItself and
//     TestACredentialShapedProjectIDIsNeverEchoedBack, which is where a regression of
//     #836 would show; putting it here would mean one table asserting two different
//     shapes of the same guarantee.
//
//   - `record_decision` RESOLVES nothing. It hands its `project_id` straight to
//     `ghost_decision_record` in the text it asks the model to write, so the
//     argument reaches the agent's prompt with no resolver between them and no
//     possibility of being withheld upstream — which is why it is here at all
//     rather than being a leak nobody had looked for. It cannot be ambiguous
//     because nothing asks, so the ambiguity shapes skip it.
var projectResolvingSurfaces = []struct {
	name string
	// argKey is the tool argument, URI segment or prompt argument that carries the
	// project, and argName is the name a refusal should use when it has to name the
	// field.
	argKey, argName string
	// read runs the surface and returns everything the caller can see: the tool
	// answer, the resource contents, or the prompt's description and messages. A
	// refusal surfaces as text in all three, because the SDK turns a handler's
	// returned error into tool-error content and a resource or prompt read fails the
	// same way — and a prompt's DESCRIPTION is returned alongside its messages, so
	// it is read too: two of these sentences used to put the argument there.
	read func(*testing.T, *mcp.ClientSession, string) string
	// neverResolves marks a surface that hands its project argument on without
	// resolving it, so the ambiguity shapes — which need a resolver to be ambiguous
	// — skip it.
	neverResolves bool
	// silentWhenUnknown marks a surface whose answer for a project Ghost has never
	// heard of is an ordinary empty result rather than a sentence about the project.
	// `ghost_memory_search` and the three listing surfaces report "nothing found" and
	// never mention the argument, so there is nothing there to withhold and nothing
	// to name: the unknown-project shape asserts only that nothing is echoed on
	// them, which is the whole of what is true.
	silentWhenUnknown bool
	// storeRefusal marks a surface whose unknown-project sentence is built by the
	// STORE rather than by the handler, so the field name the handler knows is not
	// available to it and the refusal must carry the neutral label instead. Only
	// `ghost_project_delete` sets it — see the note above storeAmbiguityLabel.
	storeRefusal bool
}{
	{
		name: "ghost_memory_search", argKey: "project_id", argName: "project_id",
		read:              toolReader("ghost_memory_search", "project_id", map[string]any{"query": "anything"}),
		silentWhenUnknown: true,
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
		name: "ghost_decisions_list", argKey: "project_id", argName: "project_id",
		read:              toolReader("ghost_decisions_list", "project_id", nil),
		silentWhenUnknown: true,
	},
	{
		name: "ghost_decision_record", argKey: "project_id", argName: "project_id",
		read: toolReader("ghost_decision_record", "project_id", map[string]any{
			"title": "a decision", "decision": "we chose it", "rationale": "because",
		}),
	},
	{
		name: "ghost_task_create", argKey: "project_id", argName: "project_id",
		read: toolReader("ghost_task_create", "project_id", map[string]any{"title": "a task"}),
	},
	{
		name: "ghost_task_list", argKey: "project_id", argName: "project_id",
		read:              toolReader("ghost_task_list", "project_id", nil),
		silentWhenUnknown: true,
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
		// The only surface whose argument is not called `project_id`. It is here
		// so the sweep cannot quietly assume otherwise, and so its refusal names
		// `project` rather than borrowing the name of an argument the caller never
		// passed — which is the assertion that fails if the boundary ever starts
		// rendering every project argument through one hard-coded label.
		name: "ghost_resolve", argKey: "project", argName: "project",
		read: toolReader("ghost_resolve", "project", nil),
	},
	{
		// The surface review-sweeper found, and the reason `storeRefusal` exists.
		// Its argument is `project` like `ghost_resolve`'s, but unlike that tool the
		// not-found sentence is not written here: `ghost_project_delete` hands
		// `args.Project` to `Store.DeleteProject`, and the store is also reached by
		// the CLI's `ghost project delete <name-or-id>`. So the store cannot name a
		// field, and the neutral label is the honest answer.
		name: "ghost_project_delete", argKey: "project", argName: "project",
		read:         toolReader("ghost_project_delete", "project", nil),
		storeRefusal: true,
	},
	{name: "ghost://project/{id}/context", argKey: "project_id", argName: "project_id", read: resourceReader("context")},
	{name: "ghost://project/{id}/tasks", argKey: "project_id", argName: "project_id", read: resourceReader("tasks"), silentWhenUnknown: true},
	{name: "ghost://project/{id}/decisions", argKey: "project_id", argName: "project_id", read: resourceReader("decisions"), silentWhenUnknown: true},
	{name: "recall_project", argKey: "project_id", argName: "project_id", read: promptReader("recall_project", nil)},
	{
		// The one surface with no resolver above it at all, and the reason the
		// prompt read includes the description: `recall_project` quotes the
		// argument into the USER message it sends to the model, and both prompts
		// quote it into the description the client shows.
		name: "record_decision", argKey: "project_id", argName: "project_id",
		read:          promptReader("record_decision", map[string]string{"topic": "a topic"}),
		neverResolves: true,
	},
}

// toolReader returns a reader for one tool: its own minimum arguments, plus the
// project argument the entry names.
// echoLogSession is projectShapeSession with the server's slog output captured
// instead of discarded, because #839 has two destinations and not one: the answer
// reaches the agent's context, and the same sentence is written to whatever the
// operator's log is. A sweep that only read the answer would pass while a handler
// logged the raw argument beside it, so the buffer comes back and is asserted on
// alongside.
//
// The buffer is emptied once the fixture has connected, so what a test reads is
// the log of ITS OWN call and not the connection chatter every session logs.
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

// resourceReader reads one `ghost://project/{id}/…` template. The URI carries the
// argument, so a token in it is a token a client put in a URL — and the read is
// where the resource handler wraps its own sentence around whatever the resolve
// returned.
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

// promptReader is the prompt surface, whose argument arrives as `project_id` in an
// argument MAP rather than as a tool arg or a URI segment — the third way a project
// identifier reaches Ghost, and the one a `%q` at a call site would have missed if
// the sweep only knew about tools.
//
// It reads the DESCRIPTION as well as the messages, and that is not thoroughness:
// a prompt's description is returned to the client and shown to the user next to
// the prompt's name, so `Ghost's accumulated knowledge for <the argument>` was a
// third interpolation of the same value on the same surface, and one no resolve
// error ever wrapped.
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
//
// The rule it holds: whatever a caller passes as its project argument, no part of
// a credential in that argument reaches the answer — as text, or through the
// renderer a row would use to print it. It is stated on the VALUE rather than on
// the shape of the message, because the failure mode is a paraphrase or a
// truncation of the message that still carries it, and a substring test on the
// secret is the only one that cannot be satisfied by re-wording.
func TestACredentialShapedProjectIDIsNeverEchoedBackByAResolvingSurface(t *testing.T) {
	for _, shape := range []struct {
		name string
		// argument builds the session's ambiguous reference and returns the value
		// the caller passes. They are not the same string for the path shape,
		// because the ambiguity lives in a subdirectory of a directory named
		// after the token while the argument is that subdirectory.
		argument func(*testing.T, *sql.DB) string
		// label is the name the refusal has to carry. The ambiguity refusal is the
		// store's own, so it names a project identifier rather than a field; see
		// storeAmbiguityLabel for why that is the rule and not an evasion.
		label string
		// uriAddressable reports whether this reference can reach a
		// `ghost://project/{project_id}/…` template at all. It cannot for the
		// path shape, because a URI template's parameter is one path SEGMENT and
		// the tied-prefix argument is a directory path — the read fails as an
		// unmatched template before a handler sees it, which is an MCP limitation
		// rather than a gap in this rule, and the name shape covers the same code
		// on those surfaces.
		uriAddressable bool
		// needsPlanting reports whether the shape has to put rows in the database
		// before the reference can be refused. The unknown-project shape does not:
		// a project Ghost has never heard of is refused BY ITS ABSENCE, which is
		// the ordinary case and the one an agent hits by pasting a URL.
		needsPlanting bool
	}{
		{name: "ambiguous by name", argument: ambiguousByName, label: storeAmbiguityLabel, uriAddressable: true, needsPlanting: true},
		{name: "ambiguous by tied path prefix", argument: ambiguousByPathPrefix, label: storeAmbiguityLabel, needsPlanting: true},
		{name: "unknown project", argument: unknownProject, needsPlanting: false},
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
					// The second destination. A handler that answers safely and logs
					// the raw argument has leaked just as surely as one that quotes
					// it, and it is the worse of the two: the answer is read once by
					// the agent that asked, while the log is read by whoever debugs
					// the store next. The renderer the log would have used is
					// checked too, for the same reason it is above — a value that
					// merely looks different once escaped is still the value.
					if logged := logs.String(); logged != "" {
						assertNoCredentialEcho(t, surface.name+" (log)", argument, logged)
					}

					// A surface that reports an ordinary empty result for a project
					// it has never heard of says nothing about the argument at all —
					// it never quoted it, so there is nothing to withhold and nothing
					// to name. That is the whole of what is true on it, and the
					// echo assertion above is the whole of what can be checked.
					if surface.silentWhenUnknown && !shape.needsPlanting {
						return
					}

					// The FORMAT is named, which is what makes the refusal
					// actionable: an agent that pasted a clone URL with inline auth
					// is told what to take out, not only that something was taken
					// out. A guard that reported nothing but "withheld" would leave it
					// guessing which token had leaked.
					if !strings.Contains(out, "GitHub personal access token") {
						t.Errorf("%s withheld the value without naming what it found (answer: %s)", surface.name, out)
					}

					// And the refusal names the thing it is refusing, which is a
					// different name on each shape and is asserted separately for
					// each: the store cannot see the caller's field, the boundary can.
					// A boundary that DELEGATED its sentence to the store cannot see it
					// either, so it is held to the neutral label — the one case where
					// knowing the field is not enough to be allowed to use it.
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

// assertNoCredentialEcho is the invariant, stated once so both halves of it are
// applied identically everywhere: the value itself, and the form the renderer a row
// would use would print it in. The second is not redundant with the first — a guard
// satisfied by a value that merely LOOKS different once quoted, escaped or
// truncated, is not a guard.
func assertNoCredentialEcho(t *testing.T, surface, argument, out string) {
	t.Helper()
	if strings.Contains(out, echoToken) {
		t.Errorf("%s echoed the credential back into the answer:\n%s", surface, out)
	}
	if rendered := assemble.Token(argument); strings.Contains(out, rendered) {
		t.Errorf("%s echoed the refused value through the renderer %q:\n%s", surface, rendered, out)
	}
}

// ambiguousByName plants two projects whose NAME is the token, which is what
// `basenameCandidates` matches on and the first resolver step that can report
// ambiguity.
func ambiguousByName(t *testing.T, db *sql.DB) string {
	t.Helper()
	for _, id := range []string{"twin-a", "twin-b"} {
		plantProject(t, db, id, echoToken, filepath.Join(t.TempDir(), id))
	}
	return echoToken
}

// ambiguousByPathPrefix plants two projects on ONE directory spelled two ways, and
// returns a subdirectory of it. The directory exists and is named after the token,
// because `ResolveProject` runs path candidates through `pathsAgree`, which
// resolves symlinks on both sides — a path that does not exist is unreachable BY
// its path, and the tie would never be reached. The two spellings are the same
// length, so the longest-prefix ranking ties and the resolver refuses to choose.
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

// unknownProject plants nothing and returns the token. An empty session is the
// hardest case for the rule and the most ordinary one: nothing here is ambiguous
// and nothing is planted, so the refusal is whatever the boundary says about a
// project it has never heard of — which is the sentence that fires for a project
// id an agent pasted a clone URL into, every time, rather than the exotic one.
func unknownProject(t *testing.T, db *sql.DB) string {
	t.Helper()
	_ = db
	return echoToken
}

// TestAResolvingSurfaceStillNamesAnOrdinaryProjectArgument is the other half, and
// the reason the fix is a renderer rather than a removal.
//
// Every refusal this change touched used to name its argument, and that name is the
// diagnostic: an agent whose `project_id` is misspelled cannot find its own typo in
// `project "…" not found`, and an operator who has two projects called `infra`
// cannot find the duplicate in a refusal that says only "ambiguous". So a value the
// guard does not recognise must come back quoted, byte for byte.
//
// The fixture is the ambiguous pair from above with an ordinary name, and the
// assertion is that the name is in the answer — on a surface that quotes it, which
// is every surface here once the value is not a credential.
// TestTheEchoSweepActuallyReadsTheServersLog is the control that keeps the log
// half of the sweep honest, and it exists because that half would otherwise be
// vacuous.
//
// Every surface the sweep reads REFUSES, and a refusal is returned to the caller
// rather than logged — the MCP SDK does not log a handler's error, and Ghost does
// not either. So the log buffer the sweep reads is EMPTY on every case above, and
// "the log contains no part of the secret" would be satisfied by a capture that
// never captured anything: a handler could start logging the raw argument tomorrow
// and every one of those assertions would still pass.
//
// This proves the capture is live instead of assuming it. It saves a memory, which
// DOES write to the log (the resource-updated notification the store pushes on a
// write), and requires that the buffer received it. A capture wired to the wrong
// logger, a buffer reset after the call, a level filter set above the line — each
// of those turns this red, which is what makes the sweep's log assertion mean
// something on the day a handler starts logging.
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
