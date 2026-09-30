package memory

import (
	"context"
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
