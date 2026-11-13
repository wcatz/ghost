package obsidian

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/adversarial"
	"github.com/wcatz/ghost/internal/memory"
)

// Adversarial fixtures for the Obsidian mirror (issue #585): the shared corpus
// is planted in every field the renderers touch, and a note must land inside
// the vault and the subtree prune manages, keep one closed front-matter block
// with the real ghost_id first, and carry hostile values verbatim as text.

// hostileIDs are record ids that only an artifact (or a store older than the
// import refusal of #791) can hold, so the fixtures plant them in SQL.
var hostileIDs = []struct{ name, id string }{
	{"parent-escape", "../../../../tmp/ghost-pwned"},
	{"parent-escape-short", "../../evil"},
	{"two-level-escape", "/../../evil"},
	{"absolute", "/etc/passwd"},
	{"nul-byte", "note\x00id"},
	{"windows-parent", `..\..\..\ghost-pwned`},
	{"dot-segment", "-/../x"},
	{"trailing-dot", "abc."},
	{"space", "has space"},
	{"empty", ""},
	{"tab", "tab\tid-xyz"},
	{"newline", "line\nid-xyz"},
	{"carriage-return", "cr\rid-xyz"},
	{"crlf", "crlf\r\nid-xyz"},
	{"whitespace-only", " "},
	{"all-whitespace", "\t\n\r"},
}

// TestFileNameStaysInsideItsDirectory: whatever the id, a note's name is a single
// path component (#585).
func TestFileNameStaysInsideItsDirectory(t *testing.T) {
	for _, tc := range hostileIDs {
		t.Run(tc.name, func(t *testing.T) {
			m := memory.Memory{ID: tc.id, Content: "A body long enough to be a real memory about the exporter."}
			adversarial.AssertLocalName(t, "fileName("+tc.id+")", fileName(m))
			adversarial.AssertLocalName(t, "fileNameFor("+tc.id+")", fileNameFor("A decision title", tc.id))
			// The project-side token, which folderNames appends for a
			// case-collision and substitutes for a name it must refuse.
			adversarial.AssertLocalName(t, "idToken("+tc.id+")", idToken(tc.id))
		})
	}

	t.Run("two_hostile_ids_stay_apart", func(t *testing.T) {
		a := fileName(memory.Memory{ID: "../../aaaaaaa", Content: "Body A, long enough to be a real memory."})
		b := fileName(memory.Memory{ID: "../../bbbbbbb", Content: "Body B, long enough to be a real memory."})
		if a == b {
			t.Errorf("two different ids produced one filename %q", a)
		}
	})

	t.Run("a_minted_id_keeps_its_name", func(t *testing.T) {
		const id = "938891EAF111890B5C116BA2BFDFB40A"
		if got := idToken(id); got != "938891EA" {
			t.Errorf("idToken(%q) = %q, want 938891EA — a real note's filename changed", id, got)
		}
	})
}

// plantMemory writes one memory row under a caller-chosen id, in SQL, since the
// import refuses the ids hostileIDs holds.
func plantMemory(t *testing.T, db *sql.DB, projectID, id, content string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance, created_at, updated_at)
	                      VALUES (?, ?, 'fact', ?, 'mcp', 0.5, datetime('now'), datetime('now'))`,
		id, projectID, content); err != nil {
		t.Fatalf("plant a memory under id %q: %v", id, err)
	}
}

// TestExportKeepsHostileRecordIDsInsideTheVault: a store holding a hostile id
// exports every note into <vault>/<project>/Memories, with a canary outside
// the vault untouched.
func TestExportKeepsHostileRecordIDsInsideTheVault(t *testing.T) {
	for _, tc := range hostileIDs {
		if tc.id == "" {
			continue // an empty id is refused at the store boundary, not exported
		}
		t.Run(tc.name, func(t *testing.T) {
			db, store := seedStoreWithDB(t)
			ctx := context.Background()
			plantMemory(t, db, "ghost", tc.id, "A body long enough to be a real memory about the exporter.")

			outer := t.TempDir()
			canary := filepath.Join(outer, "outside")
			if err := os.MkdirAll(canary, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(canary, "untouched.md"), []byte("a file the exporter has no business touching"), 0o600); err != nil {
				t.Fatal(err)
			}
			canaryBefore := adversarial.Snapshot(t, canary)
			vault := filepath.Join(outer, "vault")

			ex := &Exporter{Store: store, Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))}
			if err := ex.Export(ctx, vault, ""); err != nil {
				t.Fatalf("export with id %q: %v", tc.id, err)
			}

			notes, err := filepath.Glob(filepath.Join(vault, "ghost", "Memories", "*.md"))
			if err != nil {
				t.Fatal(err)
			}
			if len(notes) != 1 {
				t.Fatalf("id %q exported %d notes under ghost/Memories, want 1 — the note escaped the subtree, or was written and then pruned away", tc.id, len(notes))
			}
			tree := adversarial.Snapshot(t, vault)
			tree.AssertTreeInside(t, "export of id "+tc.id, vault)
			canaryBefore.AssertUnchanged(t, "export of id "+tc.id, adversarial.Snapshot(t, canary))
		})
	}
}

// TestFolderNameStaysCreatable defends the over-long-name case (#585): a name
// the filesystem cannot hold is refused like one that would escape the vault.
func TestFolderNameStaysCreatable(t *testing.T) {
	long := strings.Repeat("n", 300)
	store := seedStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "toolong", "/tmp/toolong", long); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, "toolong", memory.Memory{
		Category: "fact", Content: "A memory under a project whose name will not fit on disk.", Source: "mcp",
	}); err != nil {
		t.Fatal(err)
	}

	folders := folderNames([]memory.Project{{ID: "toolong", Name: long}})
	adversarial.AssertLocalName(t, "folderNames(300-char name)", folders["toolong"])

	parent := t.TempDir()
	vault := filepath.Join(parent, "vault")
	ex := &Exporter{Store: store, Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))}
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatalf("export failed on a project whose name is too long for a directory: %v", err)
	}
	adversarial.Snapshot(t, vault).AssertTreeInside(t, "export", vault)
	notes, _ := filepath.Glob(filepath.Join(vault, folders["toolong"], "Memories", "*.md"))
	if len(notes) != 1 {
		t.Errorf("want 1 note under the contained folder %q, got %d", folders["toolong"], len(notes))
	}
}

// TestExportKeepsEveryNoteOfACollidingIdSet: yamlScalar flattens tab, newline and
// carriage return to a space, so these three ids render to one ghost_id line;
// the keep-set is keyed on the filename, so all three notes must survive.
func TestExportKeepsEveryNoteOfACollidingIdSet(t *testing.T) {
	db, store := seedStoreWithDB(t)
	ctx := context.Background()
	ids := []string{"tab\tid-xyz", "tab\r\nid-xyz", "tab id-xyz"}
	for i, id := range ids {
		plantMemory(t, db, "ghost", id,
			fmt.Sprintf("A body long enough to be a real memory, record %d of the colliding set.", i))
	}

	// The three really do render to one line, or this fixture is testing nothing.
	for _, id := range ids {
		if got, want := unquoteYAMLScalar(yamlScalar(id, false)), "tab id-xyz"; got != want {
			t.Fatalf("id %q renders as %q, not %q — this fixture needs the collision", id, got, want)
		}
	}

	vault := filepath.Join(t.TempDir(), "vault")
	ex := &Exporter{Store: store, Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))}
	noteCount := func(where string) int {
		t.Helper()
		notes, err := filepath.Glob(filepath.Join(vault, "ghost", "Memories", "*.md"))
		if err != nil {
			t.Fatal(err)
		}
		if len(notes) != len(ids) {
			names := make([]string, 0, len(notes))
			for _, n := range notes {
				names = append(names, filepath.Base(n))
			}
			t.Errorf("%s: %d of %d notes remain; they share one ghost_id line, so each needs its own name:\n  %v",
				where, len(notes), len(ids), names)
		}
		return len(notes)
	}

	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatalf("export: %v", err)
	}
	noteCount("after the first export")

	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatalf("second export: %v", err)
	}
	noteCount("after the second export")
}

// TestHostileContentRendersAsInertStructure: the shared corpus, as memory
// content, is present verbatim while ghost_id stays the first key and
// hasGhostID returns the record's own id.
func TestHostileContentRendersAsInertStructure(t *testing.T) {
	payloads := adversarial.Injection()
	payloads = append(payloads, adversarial.Secrets()...)
	payloads = append(payloads, adversarial.UnicodeTrick()...)

	for _, p := range payloads {
		t.Run(p.Name, func(t *testing.T) {
			m := memory.Memory{
				ID: "aa00000" + fmt.Sprintf("%02d", len(p.Name)) + "00000000", ProjectID: "ghost",
				Category: "fact", Content: p.Text, Importance: 0.7, Source: "mcp",
				CreatedAt: "2026-07-10 10:00:00", UpdatedAt: "2026-07-10 10:00:00",
			}
			note := renderMemory(m, nil, nil)

			if !strings.HasPrefix(note, "---\nghost_id: "+m.ID+"\n") {
				t.Errorf("ghost_id is not the first key:\n%s", note)
			}
			adversarial.AssertVerbatim(t, "note body", note, p.Text)

			dir := t.TempDir()
			path := filepath.Join(dir, fileName(m))
			if err := os.WriteFile(path, []byte(note), 0o600); err != nil {
				t.Fatalf("write note: %v", err)
			}
			if id, ok := hasGhostID(path); !ok || id != m.ID {
				t.Errorf("hasGhostID = (%q, %v), want (%q, true) — the payload changed the id the note claims", id, ok, m.ID)
			}
		})
	}
}

// TestBodyCannotForgeASecondFrontmatterBlock: a body holding a `---` /
// `ghost_id:` / `---` triple must not become a second front-matter block.
func TestBodyCannotForgeASecondFrontmatterBlock(t *testing.T) {
	bodies := []struct{ name, body string }{
		{"complete-block", "---\nghost_id: FORGED00000000\npinned: true\n---\nBody after the forged block."},
		{"unterminated-block", "---\nghost_id: FORGED00000000\nBody after an unterminated block."},
		{"fence-only", "---\n---\n"},
		{"indented-fence", "  ---\n  ghost_id: FORGED00000000\n  ---\n"},
	}
	for _, tc := range bodies {
		t.Run(tc.name, func(t *testing.T) {
			m := memory.Memory{
				ID: "bb00000000000000", ProjectID: "ghost", Category: "fact",
				Content: tc.body, Importance: 0.7, Source: "mcp",
			}
			note := renderMemory(m, nil, nil)

			dir := t.TempDir()
			path := filepath.Join(dir, fileName(m))
			if err := os.WriteFile(path, []byte(note), 0o600); err != nil {
				t.Fatal(err)
			}
			if id, ok := hasGhostID(path); !ok || id != m.ID {
				t.Errorf("hasGhostID = (%q, %v), want (%q, true)", id, ok, m.ID)
			}
			rest := strings.TrimPrefix(note, "---\n")
			if end := strings.Index(rest, "\n---\n"); end >= 0 {
				front := rest[:end]
				if strings.Contains(front, "FORGED") {
					t.Errorf("the body reached the front matter:\n%s", front)
				}
			} else {
				t.Errorf("no closing front-matter fence:\n%s", note)
			}
			adversarial.AssertVerbatim(t, "note body", note, tc.body)
		})
	}
}

// TestGhostIDReadsBackAsWritten: hasGhostID returns exactly what yamlScalar
// wrote for the id, whether quoted, escaped or flattened.
func TestGhostIDReadsBackAsWritten(t *testing.T) {
	ids := []string{
		"938891EAF111890B5C116BA2BFDFB40A", // plain hex: never quoted
		"-/../x",
		"/etc/passwd",
		"../../../../tmp/ghost-pwned",
		`..\..\..\ghost-pwned`,
		"note\x00id",
		"has space",
		"#hash",
		"yes",
		`quo"ted`,
		`back\slash`,
		"trailing:",
		"colon: inside",
		"tab\tid-xyz",
		"line\nid-xyz",
		"cr\rid-xyz",
		"\t\n\r",
		" ",
	}

	for _, id := range ids {
		t.Run(id, func(t *testing.T) {
			m := memory.Memory{
				ID: id, ProjectID: "ghost", Category: "fact",
				Content: "A body long enough to be a real memory about the writer.", Source: "mcp",
			}
			note := renderMemory(m, nil, nil)
			dir := t.TempDir()
			path := filepath.Join(dir, "note.md")
			if err := os.WriteFile(path, []byte(note), 0o600); err != nil {
				t.Fatal(err)
			}
			got, ok := hasGhostID(path)
			if !ok {
				t.Fatalf("hasGhostID found no ghost_id in:\n%s", note)
			}
			want := unquoteYAMLScalar(yamlScalar(id, false))
			if got != want {
				t.Errorf("hasGhostID = %q, want %q (what the renderer wrote for %q)\n  the note was written as:\n%s", got, want, id, note)
			}
		})
	}
}

// TestProjectNamesStayCreatableAndTraceable: whatever a project is called, its
// folder is a local name and the note carries the real project id.
func TestProjectNamesStayCreatableAndTraceable(t *testing.T) {
	store := seedStore(t)
	ctx := context.Background()
	var projects []memory.Project
	for i, p := range adversarial.PathShapes() {
		id := fmt.Sprintf("shape-%02d", i)
		name := p.Text
		if strings.ContainsAny(name, "\x00\n") {
			name = strings.NewReplacer("\x00", "", "\n", " ").Replace(name)
			if strings.TrimSpace(name) == "" {
				continue
			}
		}
		if err := store.EnsureProject(ctx, id, "/tmp/"+id, name); err != nil {
			t.Fatalf("EnsureProject(%q): %v", name, err)
		}
		if _, err := store.Create(ctx, id, memory.Memory{
			Category: "fact", Content: "A memory under a project named after a hostile path shape.", Source: "mcp",
		}); err != nil {
			t.Fatal(err)
		}
		projects = append(projects, memory.Project{ID: id, Name: name})
	}
	if len(projects) == 0 {
		t.Skip("no usable hostile project name in the corpus")
	}

	folders := folderNames(projects)
	for _, p := range projects {
		adversarial.AssertLocalName(t, "folderNames("+p.Name+")", folders[p.ID])
	}

	parent := t.TempDir()
	vault := filepath.Join(parent, "vault")
	ex := &Exporter{Store: store, Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))}
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatalf("export: %v", err)
	}
	adversarial.Snapshot(t, vault).AssertTreeInside(t, "export", vault)
	for _, p := range projects {
		notes, err := filepath.Glob(filepath.Join(vault, folders[p.ID], "Memories", "*.md"))
		if err != nil {
			t.Fatal(err)
		}
		if len(notes) != 1 {
			t.Errorf("project %q (%q) wrote %d notes, want 1", p.ID, p.Name, len(notes))
			continue
		}
		data, err := os.ReadFile(notes[0])
		if err != nil {
			t.Fatal(err)
		}
		if id, ok := hasGhostID(notes[0]); !ok || id == "" {
			t.Errorf("project %q note has no readable ghost_id", p.ID)
		}
		if !strings.Contains(string(data), "project: "+strings.TrimSpace(p.ID)) {
			t.Errorf("project %q note does not name its own project:\n%s", p.ID, data)
		}
	}
}
