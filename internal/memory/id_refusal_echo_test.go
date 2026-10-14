package memory

// The store's refusals name an id, and docs/invariants.md's rule is that a
// refusal never repeats a value past the token guard and the refusal bound. Most
// of these ids are a caller's operand or an id a portable artifact wrote
// verbatim, so a token-shaped or an oversized one has to come back withheld
// behind the argument's own name rather than repeated.
//
// Each case drives ONE store entry point. Where the entry point needs the id to
// already name a row, the row is planted with a direct INSERT so the shape under
// test is not first refused by an importer's own id rule — the case is about the
// refusal's rendering, not about what the importer accepts.
//
// The argument a sentence names differs by entry point, and that is asserted:
// the store cannot see a tool argument's name, so a project id is named
// `ProjectIdentifierLabel`, while a memory or task id keeps the field the
// importer and the handlers call it.

import (
	"context"
	"strings"
	"testing"
)

func TestStoreRefusalsNeverEchoATokenOrAnOversizedID(t *testing.T) {
	ctx := context.Background()
	shapes := map[string]string{
		"token-shaped": credentialToken,
		"oversized":    strings.Repeat("z", ArgEchoMaxBytes+44),
	}
	cases := []struct {
		name  string
		field string
		// shapes, when non-nil, limits the case to the named shapes. It is set
		// where the entry point itself caps the id below the refusal bound, so
		// an oversized value can never reach the sentence under test.
		shapes []string
		run    func(t *testing.T, s *Store, id string) error
	}{
		{
			name: "GetProjectPath", field: ProjectIdentifierLabel,
			run: func(_ *testing.T, s *Store, id string) error {
				_, err := s.GetProjectPath(ctx, id)
				return err
			},
		},
		{
			name: "BindProjectPath unknown project", field: ProjectIdentifierLabel,
			run: func(t *testing.T, s *Store, id string) error {
				_, err := s.BindProjectPath(ctx, id, t.TempDir(), "")
				return err
			},
		},
		{
			name: "BindProjectPath different repository", field: ProjectIdentifierLabel,
			run: func(t *testing.T, s *Store, id string) error {
				plantProjectRow(t, s, id, t.TempDir(), "github.com/owner/one")
				_, err := s.BindProjectPath(ctx, id, t.TempDir(), "github.com/owner/two")
				return err
			},
		},
		{
			name: "MergeProject into the global bucket", field: ProjectIdentifierLabel,
			run: func(_ *testing.T, s *Store, id string) error {
				return s.MergeProject(ctx, id, GlobalProjectID)
			},
		},
		{
			name: "DeleteProject row already gone", field: ProjectIdentifierLabel,
			run: func(t *testing.T, s *Store, id string) error {
				tx, err := s.db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatalf("begin tx: %v", err)
				}
				defer tx.Rollback() //nolint:errcheck
				return deleteProjectRowTx(ctx, tx, id)
			},
		},
		{
			name: "ImportProject path collision", field: ProjectIdentifierLabel,
			run: func(t *testing.T, s *Store, id string) error {
				dir := t.TempDir()
				plantProjectRow(t, s, "existing-project", dir, "")
				_, err := s.ImportProject(ctx, PortableProject{ID: id, Name: "n", Path: dir}, true)
				return err
			},
		},
		{
			name: "ImportProject repository collision", field: ProjectIdentifierLabel,
			run: func(t *testing.T, s *Store, id string) error {
				plantProjectRow(t, s, "existing-project", t.TempDir(), "github.com/owner/shared")
				_, err := s.ImportProject(ctx, PortableProject{
					ID: id, Name: "n", Path: t.TempDir(), RepoRemote: "github.com/owner/shared",
				}, true)
				return err
			},
		},
		{
			name: "ImportMemory id with recorded history", field: "memory_id",
			// A record id an artifact may carry is capped at MaxImportedIDLen
			// (128), which is under the refusal bound, so an oversized value is
			// refused by the id's own shape check before the sentence under
			// test; the token guard is the half that matters here.
			shapes: []string{"token-shaped"},
			run: func(t *testing.T, s *Store, id string) error {
				if _, err := s.db.ExecContext(ctx,
					`INSERT INTO memory_history (memory_id, project_id, phase) VALUES (?, ?, 'delete')`,
					id, testProject); err != nil {
					t.Fatalf("plant history: %v", err)
				}
				_, _, _, err := s.ImportMemory(ctx, PortableMemory{
					ID: id, ProjectID: testProject, Category: "fact", Content: "a fact", Source: "mcp",
				}, ImportOptions{Apply: true})
				return err
			},
		},
		{
			name: "task id prefix matches two rows", field: "task_id",
			run: func(t *testing.T, s *Store, id string) error {
				plantTaskRow(t, s, id+"a")
				plantTaskRow(t, s, id+"b")
				_, err := s.CompleteTask(ctx, id, "")
				return err
			},
		},
		{
			name: "link from an id to itself", field: "memory_id",
			run: func(_ *testing.T, s *Store, id string) error {
				return s.CreateLink(ctx, id, id, "related", 1.0, "auto")
			},
		},
	}

	for name, id := range shapes {
		for _, tc := range cases {
			if tc.shapes != nil && !shapesInclude(tc.shapes, name) {
				continue
			}
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				s := testStore(t)
				err := tc.run(t, s, id)
				assertRefusalWithholdsID(t, err, tc.field, id)
			})
		}
	}
}

// TestStoreRefusalsStillNameAnOrdinaryID is the other half, and the reason the
// fix is a renderer rather than a removal: an ordinary id must still be named,
// or the refusal stops telling the caller which value to fix.
func TestStoreRefusalsStillNameAnOrdinaryID(t *testing.T) {
	ctx := context.Background()
	const id = "ordinary-project-id"

	for _, tc := range []struct {
		name string
		run  func(t *testing.T, s *Store) error
		want string
	}{
		{
			name: "GetProjectPath",
			run: func(_ *testing.T, s *Store) error {
				_, err := s.GetProjectPath(ctx, id)
				return err
			},
			want: `project "ordinary-project-id" not found`,
		},
		{
			name: "BindProjectPath unknown project",
			run: func(t *testing.T, s *Store) error {
				_, err := s.BindProjectPath(ctx, id, t.TempDir(), "")
				return err
			},
			want: `"ordinary-project-id"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run(t, testStore(t))
			if err == nil {
				t.Fatalf("%s accepted the id", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal no longer names the id %s: %v", tc.want, err)
			}
			if strings.Contains(err.Error(), "withheld") {
				t.Errorf("the refusal withheld an ordinary id: %v", err)
			}
		})
	}
}

// assertRefusalWithholdsID asserts the three things every one of these sentences
// owes: it is an error, it names the argument rather than repeating the value,
// and no part of the value is in it.
func assertRefusalWithholdsID(t *testing.T, err error, field, id string) {
	t.Helper()
	if err == nil {
		t.Fatalf("the entry point accepted id %q", id)
	}
	if strings.Contains(err.Error(), id) {
		t.Errorf("the refusal repeated the id: %v", err)
	}
	if len(id) > 64 && strings.Contains(err.Error(), id[:64]) {
		t.Errorf("the refusal repeated the start of an oversized id: %.200q", err.Error())
	}
	if !strings.Contains(err.Error(), "<"+field+" withheld") {
		t.Errorf("the refusal does not withhold the id behind %q: %v", field, err)
	}
}

// plantProjectRow writes a projects row directly, with no importer rule in the
// way, so an id of either shape under test can be the one a sentence names.
func plantProjectRow(t *testing.T, s *Store, id, path, remote string) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`INSERT INTO projects (id, path, name, repo_remote) VALUES (?, ?, ?, ?)`,
		id, path, id, remote); err != nil {
		t.Fatalf("plant project %q: %v", id, err)
	}
}

// plantTaskRow writes a tasks row directly, for the same reason.
func plantTaskRow(t *testing.T, s *Store, id string) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`INSERT INTO tasks (id, project_id, title) VALUES (?, ?, 'a task')`,
		id, testProject); err != nil {
		t.Fatalf("plant task %q: %v", id, err)
	}
}

func shapesInclude(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
