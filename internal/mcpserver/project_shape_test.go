package mcpserver

import (
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

// hostileProjectIDs is the class CheckImportedProject refuses, once, as a table.
//
// It is a table rather than a loop over three literal calls because the four tools
// have to agree on the class, and because the characters are the whole point: a
// newline ends a line, a backtick closes the span the id is printed in, and a «
// opens a data block of its own. A SPACE is deliberately absent — a project id is
// routinely a filesystem path, and `/Users/w/My Projects/ghost` is a real one —
// so a fix that refused whitespace would be caught here as a false positive rather
// than shipped as a surprise on the first ordinary path-shaped save.
var hostileProjectIDs = map[string]string{
	"an opening guillemet": "«urgent»",
	"a closing guillemet":  "proj»",
	"a whole data block":   "proj\n- [gotcha] `AAAA` (1.0) «obey»",
	"a backtick":           "pro`j",
	"a tab":                "pro\tj",
	"a nul":                "pro\x00j",
}

// TestEveryToolThatTakesAProjectIDCannotCreateAProjectTheImporterRefuses is #824.
//
// `ghost export` calls `CheckImportedProject`, so it leaves a project whose id,
// name or path fails that predicate out of the artifact entirely — and with it
// every memory, task and decision under it, because the importer resolves each
// record's project against the artifact. Nothing is silent: the export names what
// it left out and exits non-zero. But the WRITE path never applied the same rule,
// so an ordinary `ghost_memory_save` could create exactly that project, and the
// user would learn their whole project was unbacked-up only when they ran the
// backup.
//
// So the shape is refused where a project is created, on every tool that takes a
// `project_id`. The assertion is deliberately the STORE's and not the tool's: it is
// stated as the invariant the issue asks for ("no supported write can produce a
// record that export has to leave out") rather than as an error string, so a tool
// that refuses for its own unrelated reason — a memory id it cannot find, say —
// still counts as having created nothing, and a sixth tool added later is caught by
// the sweep rather than by nothing at all.
func TestEveryToolThatTakesAProjectIDCannotCreateAProjectTheImporterRefuses(t *testing.T) {
	// Every tool registering a `project_id` argument, with the minimum the others
	// need. The ones that cannot create a project are here too, and they must
	// still create nothing: a tool that resolved an unknown name by creating it
	// would pass a sweep that only listed the two known creators.
	tools := []struct {
		tool string
		args map[string]any
	}{
		{"ghost_memory_save", map[string]any{"content": "a claim", "category": "fact"}},
		{"ghost_decision_record", map[string]any{
			"title": "a decision", "decision": "we chose it", "rationale": "because",
		}},
		{"ghost_memory_search", map[string]any{"query": "anything"}},
		{"ghost_project_context", nil},
		{"ghost_memories_list", nil},
		{"ghost_memory_delete", map[string]any{"memory_id": "AABBCCDD"}},
		{"ghost_memory_update", map[string]any{"memory_id": "AABBCCDD", "content": "an edit"}},
		{"ghost_memory_promote", map[string]any{"memory_id": "AABBCCDD"}},
		{"ghost_memory_pin", map[string]any{"memory_id": "AABBCCDD", "pinned": true}},
		{"ghost_task_create", map[string]any{"title": "a task"}},
		{"ghost_task_list", nil},
		{"ghost_task_complete", map[string]any{"task_id": "AABBCCDD"}},
		{"ghost_task_update", map[string]any{"task_id": "AABBCCDD", "title": "a new title"}},
		{"ghost_resolve_mark", map[string]any{"memory_ids": []string{"AABBCCDD"}}},
		{"ghost_link_withdraw", map[string]any{"source_id": "AABBCCDD", "target_id": "EEFF0011"}},
		{"ghost_decisions_list", nil},
	}

	for _, tc := range tools {
		for shape, hostile := range hostileProjectIDs {
			t.Run(tc.tool+"/"+shape, func(t *testing.T) {
				_, srv, session := projectShapeSession(t)
				args := map[string]any{}
				for k, v := range tc.args {
					args[k] = v
				}
				args["project_id"] = hostile
				callTool(t, session, tc.tool, args)

				// The invariant, read off the rows rather than off the answer.
				for _, p := range projectsOf(t, srv) {
					if err := memory.CheckImportedProject(p); err != nil {
						t.Errorf("%s created a project %q that `ghost export` must leave out (%v); the write "+
							"boundary and the importer must agree on the shape", tc.tool, p.ID, err)
					}
				}
				// And nothing was opened under the hostile name at all, which is what
				// a refusal on one of the resolving tools looks like from here. A
				// tool that wrote the id but also made it importable would still be
				// wrong, just differently — and a tool that wrote it as the NAME of
				// some other row would be wronger still, so the sweep above checks
				// every field of every row rather than only ids.
				if projectExists(t, srv, hostile) {
					t.Errorf("%s opened a project under the refused id", tc.tool)
				}
			})
		}
	}
}

// TestTheProjectShapeRefusalNamesTheValueThroughTheSafeRenderer is the half of the
// refusal that is easy to get wrong and worse than having no message.
//
// The rejected value is caller-supplied text that reaches the answer, and an error
// that interpolates it raw is a second injection on the very surface the refusal
// exists to close — the same argument `validateTags` makes about the tag it names.
// So the value goes through `assemble.Token`, which is the renderer the row itself
// uses, and the sentence carries NONE of the three characters it refuses, not even
// in its own prose — which is why the predicate's own messages say "backtick or data
// delimiter" rather than spelling the characters out, and that wording is part of
// what this test holds. The predicate deliberately does NOT show the value (its
// message is also the export report's, where quoting the caller's text is noise and
// a hazard); the tool boundary is a different surface with a caller who can fix it,
// so it names the value — safely.
func TestTheProjectShapeRefusalNamesTheValueThroughTheSafeRenderer(t *testing.T) {
	// The tool that OPENS a project, and it is `ghost_memory_save` alone rather than
	// the tools that merely take a `project_id`. `ghost_decision_record` refuses an
	// unknown project before it can create one — `ResolveProject` answers "", and
	// the handler says so — so the creation check never fires there and asserting
	// this message of it would be asserting a different tool's refusal. The sweep
	// above is what holds that one to creating nothing.
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"ghost_memory_save", map[string]any{"content": "a claim", "category": "fact"}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			_, _, session := projectShapeSession(t)
			args := map[string]any{"project_id": "«urgent»"}
			for k, v := range tc.args {
				args[k] = v
			}
			out := resultText(callTool(t, session, tc.tool, args))
			if !strings.Contains(out, "must hold no") {
				t.Errorf("%s did not refuse the project shape (answer: %s)", tc.tool, out)
			}
			for _, forbidden := range []string{"«", "»", "`"} {
				if strings.Contains(out, forbidden) {
					t.Errorf("%s: the refusal carries %q into the answer:\n%s", tc.tool, forbidden, out)
				}
			}
			if strings.ContainsAny(out, "\n\r") {
				t.Errorf("%s: the refusal carries a line break into the answer:\n%q", tc.tool, out)
			}
			// Identifiable, and in the SAME form the row would print it.
			if !strings.Contains(out, assemble.Token("«urgent»")) {
				t.Errorf("%s: the refusal does not name the project_id in the form the renderer would print it "+
					"(want %q):\n%s", tc.tool, assemble.Token("«urgent»"), out)
			}
			// And it must not tell the caller not to look for the value it has just
			// printed. The predicate's own sentence is the export report's, where the
			// value is deliberately absent, so it cannot also say "not shown" here —
			// the two surfaces share one message and it has to be true on both.
			if strings.Contains(out, "not shown") {
				t.Errorf("%s: the refusal says the value is not shown and then shows it:\n%s", tc.tool, out)
			}
			// And it says which FIELD, so an agent that passed a path-shaped id
			// knows the project_id is what to change.
			if !strings.Contains(out, "project id") {
				t.Errorf("%s: the refusal does not name the field:\n%s", tc.tool, out)
			}
		})
	}
}

// TestACredentialShapedProjectIDIsNeverEchoedBack is the limit of naming the
// refused value, and the reason `ensureProjectFor` asks the credential guard before
// it asks the shape predicate rather than branching on what comes back.
//
// Naming the value is what makes the shape refusal actionable — an agent that passed
// `project_id` can fix it — and it is only safe because the value it names is a
// value the SHAPE rule refused, which the renderer neutralises. That reasoning does
// not survive a credential. `CheckImportedProject` ends in `rejectSecretFields`, so
// for a `project_id` holding a token it returns a `*SecretContentError` whose whole
// contract is that it names the field and the format and never the value: the
// sentence reaches the log file, this agent's context, and a reflection prompt sent
// to a third-party model. Appending the refused value would put the token back into
// the one answer that says Ghost never stores credentials. And a path-shaped
// project_id carrying a token is not a contrived input — it is what an agent that
// pasted a clone URL with embedded auth produces, which is exactly the class the
// guard exists for.
//
// The SECOND case is the one that decides where the question is asked. A value that
// is both hostile and credential-shaped comes back as a SHAPE error, because the
// predicate judges shape before credentials, so `errors.Is(err, ErrSecretContent)`
// is false and the secret is hiding behind an error that says nothing about it. A
// caller that branched on the returned error would print the token. So the guard is
// asked first, and the assertion below holds both cases against one rule: whatever
// comes back, the answer must not contain the secret.
func TestACredentialShapedProjectIDIsNeverEchoedBack(t *testing.T) {
	// The token, and the two shapes it arrives in. The first is the ordinary
	// mistake: a remote URL with inline credentials, path-shaped, so `ghost_memory_save`
	// would otherwise treat it as a checkout. The second is the one that defeats a
	// check placed after the predicate: the SAME URL wrapped in the guillemet the
	// shape rule refuses, so the shape error wins the race and hides the credential.
	const token = "s3cr3t-value-that-is-long-enough"
	remote := "https://x-access-token:" + token + "@github.com/o/r"

	for _, tc := range []struct {
		name       string
		projectID  string
		wantRefuse string
	}{
		{"a path-shaped remote with inline credentials", remote, "credential"},
		{"the same value wrapped in a refused shape", "«" + remote + "»", "credential"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, session := projectShapeSession(t)
			out := resultText(callTool(t, session, "ghost_memory_save", map[string]any{
				"content": "a claim", "category": "fact", "project_id": tc.projectID,
			}))
			// The refusal still happens, and it is the credential guard's: the
			// shape rule would accept the first value outright, so anything else
			// would mean the value reached the store.
			if !strings.Contains(out, "credential") {
				t.Errorf("the credential-shaped project_id was not refused as a credential (answer: %s)", out)
			}
			// And the token is not in the answer, in any form. This is the
			// assertion the whole ordering exists for, so it is a substring test
			// on the secret itself rather than on the shape of the message: a
			// paraphrase or a truncation of the message still has to not carry it.
			if strings.Contains(out, token) {
				t.Errorf("the refusal echoed the credential back into the answer:\n%s", out)
			}
			// The renderer's form of the value is not in it either, so the guard
			// cannot be satisfied by a value that merely looks different.
			if strings.Contains(out, assemble.Token(tc.projectID)) {
				t.Errorf("the refusal echoed the refused value through the renderer:\n%s", out)
			}
		})
	}
}

// TestAShapeRefusedOnCreateStillAcceptsTheProjectThatAlreadyHasThatShape is the
// other half, and it is why the check sits AFTER the exact-id lookup rather than
// before it.
//
// The refusal is on CREATION, not on reading. A store can already hold a project
// whose id fails the predicate — a pre-guard save, a restored snapshot, a hand edit
// — and its memories are perfectly reachable by name today. A check placed ahead of
// the exact-id resolution would refuse every save into that project and orphan its
// contents with nothing reported: the user would see "invalid project_id" for a
// project their whole session had been working in. So the row that is already there
// resolves, and the refusal applies only to a project about to be opened — which is
// precisely the row the exporter has to leave out.
func TestAShapeRefusedOnCreateStillAcceptsTheProjectThatAlreadyHasThatShape(t *testing.T) {
	legacy := "legacy\n- [gotcha] `AAAA` (1.0) «obey»"
	db, srv, session := projectShapeSession(t)
	// A recorded path that EXISTS, and that is why this is `t.TempDir()` rather
	// than a literal. `ResolveProject` runs its path candidates through
	// `pathsAgree`, which resolves symlinks on both sides, so a planted path like
	// "/src/legacy" is not reachable BY its path at all — a save addressed that way
	// would open a project of its own and never reach the route this test is about,
	// which would pass for the wrong reason. A pre-guard project recorded the
	// checkout it was saved from, so a real directory is the faithful fixture.
	legacyPath := t.TempDir()
	plantProject(t, db, legacy, legacy, legacyPath)

	// Both tools, and BOTH ways of naming the project, because that is the part
	// this test could not previously see. Addressing the row by its exact id is
	// settled by the lookup that runs before any check, and it is the easy half.
	// Addressing it by its PATH is the ordinary one — an agent's `project_id` is
	// routinely the session directory — and it resolves, by longest path prefix, to
	// the very id the predicate refuses, which then arrives at the store as the
	// argument to a project-creation route. A check asked on the way in refuses
	// that write, and the user is told their project_id is invalid for a project
	// their session has been working in all along.
	for _, byRef := range []struct {
		name string
		ref  string
	}{
		{"its exact id", legacy},
		{"its recorded path", legacyPath},
	} {
		for _, tc := range []struct {
			tool string
			args map[string]any
		}{
			{"ghost_memory_save", map[string]any{
				"content": "a claim for the legacy project", "category": "fact",
			}},
			{"ghost_decision_record", map[string]any{
				"title": "a decision for the legacy project", "decision": "we chose it",
				"rationale": "because the legacy project is where we work",
			}},
		} {
			t.Run(tc.tool+"/by "+byRef.name, func(t *testing.T) {
				args := map[string]any{"project_id": byRef.ref}
				for k, v := range tc.args {
					args[k] = v
				}
				out := resultText(callTool(t, session, tc.tool, args))
				if out == "" || strings.Contains(out, "must hold no") {
					t.Fatalf("the write into the project the store already holds was refused (answer: %s)", out)
				}
				if n := countProjectMemories(t, srv, legacy); n == 0 {
					t.Errorf("the legacy project holds no memory row after the call; the decision path writes a " +
						"companion memory and the save path writes the row itself, so either way one is expected")
				}
			})
		}
	}
	// And nothing NEW was opened next to it, which is what "resolve, do not create"
	// means for a name that resolves — and the strongest form of that, because the
	// path address is the one that would open a second project if the resolution
	// failed.
	for _, p := range projectsOf(t, srv) {
		if err := memory.CheckImportedProject(p); err != nil {
			if p.ID == legacy {
				continue // the planted row is the store's own history, not a call's doing
			}
			t.Errorf("a call opened a project the exporter must leave out: %q (%v)", p.ID, err)
		}
	}
	if n := len(projectsOf(t, srv)); n != 4 {
		t.Errorf("the store holds %d project(s), want the two fixtures, _global and the planted legacy row", n)
	}
}

// TestABoundPathThePredicateRefusesStillAcceptsASaveAddressedByThatPath is the half
// of the creation rule that the exact-id lookup alone cannot settle, and it is why
// `ensureProjectFor`'s two guards sit behind the resolution rather than in front of
// it.
//
// A project the store already holds can carry a refused character in a field the
// write boundary does not own. `ghost project bind` writes `projects.path` through
// `storedPathIsUsable`, which asks only whether the path is absolute and is not a
// bare root — it says nothing about «, » or a backtick, and all three are legal in a
// POSIX directory name. So a checkout called `«ghost»` binds to an ordinary project
// with a perfectly clean id, and the guard reads the CALLER's argument, which is
// that directory, because an agent's `project_id` is routinely the session
// directory. `ResolveExactProjectID` misses (the argument is not an id), the path
// resolves by longest prefix to the project's clean id, and the write lands in the
// project the session has been working in all along. Asked before the resolution, the
// guard refuses it — "project id must hold no data delimiter" for a project whose
// memories are all still there, which is the exact failure the creation-only rule
// exists to prevent.
//
// The credential case is the same gate on the other guard, and it is why the gate is
// on both rather than one: a bound path carrying a token would otherwise make the
// project unwritable by the only address a session uses. Nothing prints the value on
// a route that resolves, so nothing leaks by skipping the check here.
func TestABoundPathThePredicateRefusesStillAcceptsASaveAddressedByThatPath(t *testing.T) {
	// Two shapes in the LEAF of the recorded path, each one the predicate (or the
	// credential guard) refuses on its own. The directory is created for real:
	// `ResolveProject` runs path candidates through `pathsAgree`, which resolves
	// symlinks on both sides, so a path that does not exist is unreachable BY its
	// path and the save would open a project of its own — passing for the wrong
	// reason.
	for _, tc := range []struct {
		name string
		leaf string
	}{
		{"a refused data delimiter", "«ghost»"},
		{"a credential-shaped leaf", "ghp_" + strings.Repeat("a", 36)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, srv, session := projectShapeSession(t)
			dir := filepath.Join(t.TempDir(), tc.leaf)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("create the checkout directory: %v", err)
			}
			// A CLEAN id and name, which is the whole point: only the recorded path
			// fails, so nothing here looks like a row a guard should have refused.
			plantProject(t, db, "boundid", "boundid", dir)

			out := resultText(callTool(t, session, "ghost_memory_save", map[string]any{
				"content": "a claim saved from the bound checkout", "category": "fact", "project_id": dir,
			}))
			if out == "" || strings.Contains(out, "must hold no") || strings.Contains(out, "credential") {
				t.Fatalf("a save addressed by a bound path the predicate refuses was refused (answer: %s)", out)
			}
			if n := countProjectMemories(t, srv, "boundid"); n == 0 {
				t.Errorf("the bound project holds no memory row after the call; the write should have landed " +
					"in the project the path resolves to")
			}
			// And nothing was opened beside it, which is what "resolve, do not
			// create" has to mean for a path that resolves.
			if n := len(projectsOf(t, srv)); n != 4 {
				t.Errorf("the store holds %d project(s), want the two fixtures, _global and the planted bound row", n)
			}
			if projectExists(t, srv, dir) {
				t.Errorf("the save opened a project under the directory it should have resolved to")
			}
		})
	}
}

// TestACredentialShapedProjectIDIsNeverEchoedBackByTheResolveItself is the leak the
// gate on the guards would otherwise open, and the reason the credential guard is
// asked on the resolve's error path as well as on the creation path.
//
// Hoisting the resolution above the guards means a credential-shaped `project_id`
// now reaches `Store.ResolveProject` before anything has judged it, and two of that
// function's refusals interpolate the caller's own argument — `"%q matches multiple
// projects"` and `"%q has tied path-prefix matches"`. Those sentences travel on into
// the tool's answer, so a token that happened to be ambiguous would land in the one
// answer that says Ghost never stores credentials. The precondition is narrow — two
// projects have to share the value as a name — and it is exactly the invariant the
// guard was written for.
//
// So the guard is asked twice, at the two points where the argument can become an
// answer, and both refusals are credential refusals that name the field and never
// the value. The repository refusal needs no such treatment: it names a
// `NormalizeRepoRemote`d remote, which has no userinfo to strip back out.
func TestACredentialShapedProjectIDIsNeverEchoedBackByTheResolveItself(t *testing.T) {
	// Two projects sharing the token as their NAME, which is what
	// `basenameCandidates` matches on and what makes the reference ambiguous.
	const token = "ghp_" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	db, _, session := projectShapeSession(t)
	plantProject(t, db, "twin-a", token, filepath.Join(t.TempDir(), "a"))
	plantProject(t, db, "twin-b", token, filepath.Join(t.TempDir(), "b"))

	out := resultText(callTool(t, session, "ghost_memory_save", map[string]any{
		"content": "a claim", "category": "fact", "project_id": token,
	}))
	if strings.Contains(out, token) {
		t.Errorf("the ambiguity refusal echoed the credential back into the answer:\n%s", out)
	}
	if strings.Contains(out, assemble.Token(token)) {
		t.Errorf("the ambiguity refusal echoed the refused value through the renderer:\n%s", out)
	}
	// And it is still refused — as a credential, which is what names the reason a
	// caller can act on. A resolve error that merely happened to avoid printing the
	// value would not satisfy this.
	if !strings.Contains(out, "credential") {
		t.Errorf("an ambiguous credential-shaped project_id was not refused as a credential (answer: %s)", out)
	}
}

// projectShapeSession is the fixture the three tests above share: a store holding
// the projects every other test in this package creates, over a live MCP
// connection. The database handle comes back too, because the legacy test has to
// plant a row no write boundary will create.
func projectShapeSession(t *testing.T) (*sql.DB, *Server, *mcp.ClientSession) {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store := memory.NewStore(db, logger)
	ctx := context.Background()
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`); err != nil {
		t.Fatalf("insert _global project: %v", err)
	}
	for _, p := range []struct{ id, path, name string }{
		{"vproj", t.TempDir(), "vproj"},
		{"bare", t.TempDir(), "bare"},
	} {
		if err := store.EnsureProject(ctx, p.id, p.path, p.name); err != nil {
			t.Fatalf("EnsureProject %s: %v", p.id, err)
		}
	}
	srv := New(store, logger, "test")
	return db, srv, connectedClient(t, srv)
}

// projectsOf reads every project row as the importer would see it, so the sweep's
// assertion is the same predicate rather than a second shape check.
func projectsOf(t *testing.T, srv *Server) []memory.PortableProject {
	t.Helper()
	store, ok := srv.store.(interface {
		PortableProjects(context.Context) ([]memory.PortableProject, error)
	})
	if !ok {
		t.Fatal("the server's store cannot list projects the importer would see")
	}
	projects, err := store.PortableProjects(context.Background())
	if err != nil {
		t.Fatalf("PortableProjects: %v", err)
	}
	return projects
}

// projectExists reports whether the store holds a project under this exact id,
// which is the raw form of the assertion above and needs no renderer to say it.
func projectExists(t *testing.T, srv *Server, id string) bool {
	t.Helper()
	for _, p := range projectsOf(t, srv) {
		if p.ID == id {
			return true
		}
	}
	return false
}
