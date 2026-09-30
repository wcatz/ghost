package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// TestEveryProjectCreationRouteRefusesAShapeTheImporterRefuses is #824 at the
// seam every caller passes through, MCP or not.
//
// `ghost export` calls `CheckImportedProject`, so a project whose id, name or path
// that predicate refuses leaves the artifact entirely — and with it every memory,
// task and decision under it, because the importer resolves each record's project
// against the artifact. The write path never applied the same rule, so a project
// the importer would refuse could be created by an ordinary save, and the operator
// learns their project is unbacked-up only when they run a backup.
//
// There are two creation routes and both are here, because a fix on one is a fix on
// one: `ensureProjectLocked` (behind EnsureProject and EnsureProjectWithRepo) and
// `ResolveOrCreateRepoProject` → `createRepoProjectTx`, which a path-shaped
// `project_id` with a detectable remote takes INSTEAD of the first. Enumerating them
// is the point — the second was the more likely miss, since it does not share a
// function with the first.
func TestEveryProjectCreationRouteRefusesAShapeTheImporterRefuses(t *testing.T) {
	// The class, as one table, because both routes have to agree on it: a control
	// character ends a line, a backtick closes the span the value is printed in, and
	// a « opens a data block of its own. A SPACE is absent and so is a length,
	// because a project id is routinely a filesystem path — and a refusal that took
	// either would break every ordinary path-shaped save.
	refused := map[string]struct{ id, name, path string }{
		"a newline in the id":   {"p\nid", "p", "/src/p"},
		"a backtick in the id":  {"p`id", "p", "/src/p"},
		"a guillemet in the id": {"p«id", "p", "/src/p"},
		"a nul in the id":       {"p\x00id", "p", "/src/p"},
		// An ordinary id with the NAME hostile, and an ordinary id with the PATH
		// hostile: `ensureProjectFor` stores the caller's argument as all three, but
		// the store's routes are also called directly with distinct values (the
		// bench seeders are), and a fix that checked only the id would miss these.
		"a newline in the name":   {"pn", "bad\nname", "/src/pn"},
		"a guillemet in the name": {"pg", "«urgent»", "/src/pg"},
		"a backtick in the path":  {"pp", "p", "/src/p`p"},
		"a guillemet in the path": {"ppg", "p", "/src/p«p"},
	}
	routes := []struct {
		name string
		call func(ctx context.Context, s *Store, id, name, path string) error
	}{
		{"EnsureProject", func(ctx context.Context, s *Store, id, name, path string) error {
			return s.EnsureProject(ctx, id, path, name)
		}},
		{"EnsureProjectWithRepo", func(ctx context.Context, s *Store, id, name, path string) error {
			return s.EnsureProjectWithRepo(ctx, id, path, name, "")
		}},
		{"ResolveOrCreateRepoProject", func(ctx context.Context, s *Store, id, name, path string) error {
			_, _, err := s.ResolveOrCreateRepoProject(ctx, id, "repo", id, path, name,
				"git@github.com:wcatz/repo.git")
			return err
		}},
	}

	for _, r := range routes {
		for shape, tc := range refused {
			t.Run(r.name+"/"+shape, func(t *testing.T) {
				s := testStore(t)
				ctx := context.Background()
				err := r.call(ctx, s, tc.id, tc.name, tc.path)
				if err == nil {
					t.Fatalf("%s created the project the importer refuses (%+v)", r.name, tc)
				}
				// The refusal has to be the IMPORTER's, so the same row is judged
				// the same way everywhere it is judged. A copy of the rule here
				// would be a second rule, and this is the assertion that says so.
				refusedRec := createdProject(tc.id, tc.path, tc.name)
				if err := CheckImportedProject(refusedRec); err == nil {
					t.Errorf("%s refused the project, but CheckImportedProject accepts it: the write boundary "+
						"and the importer disagree", r.name)
				}
				// And nothing was written: a refusal that still created the row is
				// the exact defect, since the row is what export leaves out.
				projects, err := s.PortableProjects(ctx)
				if err != nil {
					t.Fatalf("PortableProjects: %v", err)
				}
				for _, p := range projects {
					if p.ID == tc.id {
						t.Errorf("%s created the refused project anyway (id %q)", r.name, p.ID)
					}
				}
				// A ghost_state row would be the same failure one step behind, since
				// it is written beside the project.
				var states int
				if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ghost_state WHERE project_id = ?`, tc.id).Scan(&states); err != nil {
					t.Fatalf("count ghost_state: %v", err)
				}
				if states != 0 {
					t.Errorf("%s left %d ghost_state row(s) for the refused project", r.name, states)
				}
			})
		}
	}
}

// TestProjectCreationStillAcceptsAPathShapedIDIs the other half of the class table
// above, and it is the one that would catch an over-eager fix. `ensureProjectFor`
// stores the caller's `project_id` as the id, the name AND the path, and that
// argument is routinely a filesystem path — a space belongs in it, and so does a
// deep, long directory. `CheckImportedProjectID` documents that it is deliberately
// weaker than `CheckImportedID` for exactly this reason, so a write boundary that
// reached for the stricter rule would refuse every ordinary save made from a
// checkout with a space in its name.
func TestProjectCreationStillAcceptsAPathShapedID(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, id := range []string{
		"/Users/w/My Projects/ghost",
		strings.Repeat("/deep/", 60) + "checkout",
		"ghost",
	} {
		if err := s.EnsureProject(ctx, id, "", id); err != nil {
			t.Errorf("EnsureProject refused the project id %q: %v", id, err)
		}
		if _, ok, err := s.ResolveExactProjectID(ctx, id); err != nil || !ok {
			t.Errorf("the project %q was not created (ok=%v, err=%v)", id, ok, err)
		}
	}
}

// TestAProjectTheStoreAlreadyHoldsIsNotJudged is #824's other half, on the road the
// MCP layer actually takes to reach a project that is already there.
//
// A store can hold a project the predicate refuses — a pre-guard save, a restored
// snapshot, a hand edit, or a pre-guard save later bound with `ghost project bind`
// — and refusing writes into it would orphan the user's memories with nothing
// reported: the session they are working in names the project, and the answer says
// the project_id is invalid. The refusal therefore belongs to the CREATE, and
// honouring that is harder than asking the predicate on the way in, because neither
// store route is reached with the caller's own argument.
//
// `mcpserver.ensureProjectFor` looks that argument up by exact id, then basename
// name, then longest path prefix, and hands the RESOLVED id down. So a legacy
// project whose id is hostile and whose recorded PATH is ordinary is addressed
// every day by its path, arrives here carrying the hostile id, and a check placed
// above the lookup would refuse a write into a project that exists and holds the
// user's memories. Both routes are here because they are reached differently: the
// upsert by a caller that resolved the reference first, the repository route by a
// caller that passes the reference through and lets `resolveRepoProjectTx` match it
// on `id = ? OR path = ?`. A fix on one of them is a fix on one of them.
func TestAProjectTheStoreAlreadyHoldsIsNotJudged(t *testing.T) {
	// The recorded path has to be a directory that EXISTS. `ResolveProject` runs
	// its path candidates through `pathsAgree`, which resolves symlinks on both
	// sides, so a planted path like "/src/legacy" is not reachable by its path at
	// all — the test would pass for the wrong reason, because the save would open
	// a project of its own and the hostile id would never reach the route under
	// test. A pre-guard project records the checkout it was saved from, so a real
	// directory is also the faithful fixture.
	legacyPath := t.TempDir()
	legacy := "legacy\n- [gotcha] `AAAA` (1.0) «obey»"

	for _, tc := range []struct {
		route string
		// call is what the store is asked to do, and it returns the project the
		// write was routed to.
		call func(ctx context.Context, s *Store) (string, error)
	}{
		{"EnsureProjectWithRepo", func(ctx context.Context, s *Store) (string, error) {
			// The MCP layer's own sequence: resolve the caller's reference, then
			// ensure the project the resolution named. Reproducing both halves is
			// the point — resolving first and planting the resolved id is what
			// turns a clean reference into a hostile one on this route.
			id, _, err := s.ResolveProject(ctx, legacyPath)
			if err != nil {
				return "", err
			}
			if id == "" {
				return "", fmt.Errorf("the planted project is not reachable by its own recorded path %q, "+
					"so this case would never reach the route it claims to test", legacyPath)
			}
			if err := s.EnsureProjectWithRepo(ctx, id, "", id, ""); err != nil {
				return "", err
			}
			return id, nil
		}},
		{"ResolveOrCreateRepoProject", func(ctx context.Context, s *Store) (string, error) {
			// The reference is the hostile id rather than the path, and that is the
			// discriminating choice: `resolveExplicitProjectRepoTx` matches on
			// `id = ? OR path = ?`, so a path reference would leave the create arm
			// holding a clean record that the predicate accepts whatever the gate
			// says. A caller that knows this project by the id it was stored under
			// is the case the old placement refused.
			canonical, _, err := s.ResolveOrCreateRepoProject(ctx, legacy, "legacy", legacy,
				legacyPath, legacy, "git@github.com:wcatz/legacy.git")
			return canonical, err
		}},
	} {
		t.Run(tc.route, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			// Planted in SQL, because no write boundary will create it any more —
			// which is the state this test is about.
			if _, err := s.db.Exec(`INSERT INTO projects (id, path, name) VALUES (?, ?, ?)`,
				legacy, legacyPath, legacy); err != nil {
				t.Fatalf("plant the legacy project: %v", err)
			}
			before, err := s.PortableProjects(ctx)
			if err != nil {
				t.Fatalf("PortableProjects: %v", err)
			}

			routed, err := tc.call(ctx, s)
			if err != nil {
				t.Fatalf("%s refused a write into the project the store already holds: %v", tc.route, err)
			}
			if routed != legacy {
				t.Errorf("%s routed the write to %q rather than to the project it already held (%q); a refusal "+
					"here opens a second project instead of reaching the user's own", tc.route, routed, legacy)
			}
			// And it reached the row rather than a new one: the refusal is on
			// creation, so the project count is the observable of it.
			after, err := s.PortableProjects(ctx)
			if err != nil {
				t.Fatalf("PortableProjects: %v", err)
			}
			if len(after) != len(before) {
				t.Errorf("%s left %d project(s), want the %d the store already held", tc.route, len(after), len(before))
			}
			// And the point of the whole arrangement: a memory still lands in it.
			// An orphan is not a project row that renders safely, it is a project
			// whose contents the user can no longer write to.
			if _, _, _, err := s.Upsert(ctx, legacy, "fact", "a claim in the legacy project",
				"mcp", 0.5, nil); err != nil {
				t.Errorf("%s left the held project unwritable: %v", tc.route, err)
			}
		})
	}
}

// TestARefusedShapeIsRefusedOnCreationAndAcceptedOnTheHeldRow is the negative half
// of the gate above, in the one place it could be got wrong. A check gated on "the
// row is not already there" is a check that can be skipped, and the skip is a silent
// hole unless something holds both halves to the same condition.
//
// Nothing above distinguishes an empty store from one holding the row except that
// row's existence, so this asks for the refusal, then plants the very project it
// refused, then asks again with the identical value on both routes. The second call
// has to be accepted — that is the legacy store #824 is careful not to break — and
// the first has to be refused, and neither can hold without the other: a gate
// dropped entirely fails the first, and a gate that ignores the row fails the
// second. The creation half is also asserted by the sweep above, so what is new here
// is that the two halves are one condition rather than two rules.
func TestARefusedShapeIsRefusedOnCreationAndAcceptedOnTheHeldRow(t *testing.T) {
	const hostile = "«urgent»"
	remote := "git@github.com:wcatz/gate.git"

	for _, route := range []string{"EnsureProjectWithRepo", "ResolveOrCreateRepoProject"} {
		t.Run(route, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			call := func() error {
				if route == "EnsureProjectWithRepo" {
					return s.EnsureProjectWithRepo(ctx, hostile, "", hostile, "")
				}
				_, _, err := s.ResolveOrCreateRepoProject(ctx, hostile, "gate", hostile, hostile, hostile, remote)
				return err
			}

			if err := call(); err == nil {
				t.Fatalf("%s opened the project %q the importer refuses, with the store empty", route, hostile)
			}
			projects, err := s.PortableProjects(ctx)
			if err != nil {
				t.Fatalf("PortableProjects: %v", err)
			}
			if len(projects) != 1 {
				// One, and it is the fixture `testStore` seeds: nothing was opened.
				for _, p := range projects {
					if p.ID == hostile {
						t.Fatalf("%s created the refused project %q anyway", route, hostile)
					}
				}
				t.Fatalf("%s left %d project(s) behind a refusal, want only the fixture project", route, len(projects))
			}
			// Now the same value with the row already there, which is the state a
			// pre-guard store is in.
			if _, err := s.db.Exec(`INSERT INTO projects (id, path, name) VALUES (?, ?, ?)`,
				hostile, "/src/gate", "gate"); err != nil {
				t.Fatalf("plant the project: %v", err)
			}
			if err := call(); err != nil {
				t.Errorf("%s refused a project the store already holds: %v", route, err)
			}
		})
	}
}
