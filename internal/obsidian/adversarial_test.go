package obsidian

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/adversarial"
	"github.com/wcatz/ghost/internal/memory"
)

// Adversarial fixtures for the Obsidian mirror (issue #585).
//
// A vault is the one place Ghost writes Markdown a human and other tools read,
// and everything in it comes from the store — which a portable artifact can
// seed with ids, names, tags and bodies ghost never generated. So the fixtures
// here plant the shared corpus in every field the renderers touch and assert
// three things: the note lands INSIDE the vault and inside the subtree prune
// manages, the front matter stays one key per line with the real ghost_id first
// (prune's whole identity check reads that block), and a hostile value is
// present verbatim as text rather than having become structure.
//
// Nothing is stripped. A payload that cannot be found is a payload the user
// cannot see, and an Obsidian note that silently lost its injection is not a
// defence.

// hostileIDs are record ids that only an artifact can produce: ghost mints hex,
// and Store.ImportMemory writes an artifact's ids verbatim.
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
}

// TestFileNameStaysInsideItsDirectory defends the containment half for the
// filename every note is published under (#585).
//
// A note's name is slug(content) + "-" + the record's id token + ".md", and the
// id token used to be a bare prefix of the id. Ghost mints hex ids, so that was
// safe for everything the store wrote itself — but a portable artifact carries
// its own ids and `ghost import` writes them verbatim, so a hostile id reached
// this function unchanged. A separator in the fragment let the note land outside
// the Memories/ subtree prune watches, and the keep-set then deleted it on the
// same pass; a NUL made the write fail with EINVAL, failing every export and
// every retry. So the rule is the one the whole package is built on: whatever
// else happens, the name is a single path component.
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
		// Replacing the offending bytes would have collapsed these onto one
		// filename, so one memory's note would have overwritten the other's and
		// then been pruned. Hashing is what keeps them distinct.
		a := fileName(memory.Memory{ID: "../../aaaaaaa", Content: "Body A, long enough to be a real memory."})
		b := fileName(memory.Memory{ID: "../../bbbbbbb", Content: "Body B, long enough to be a real memory."})
		if a == b {
			t.Errorf("two different ids produced one filename %q", a)
		}
	})

	t.Run("a_minted_id_keeps_its_name", func(t *testing.T) {
		// The plain path must be byte-identical to the old prefix rule, or every
		// existing vault renames every note on the next export.
		const id = "938891EAF111890B5C116BA2BFDFB40A"
		if got := idToken(id); got != "938891EA" {
			t.Errorf("idToken(%q) = %q, want 938891EA — a real note's filename changed", id, got)
		}
	})
}

// TestExportKeepsHostileRecordIDsInsideTheVault is the end-to-end half of the
// same rule: a store seeded through the documented artifact path must export
// every note into <vault>/<project>/Memories, every pass, with the tree itself
// inside the vault and a canary outside it untouched.
func TestExportKeepsHostileRecordIDsInsideTheVault(t *testing.T) {
	for _, tc := range hostileIDs {
		if tc.id == "" {
			continue // an empty id is refused at the store boundary, not exported
		}
		t.Run(tc.name, func(t *testing.T) {
			store := seedStore(t)
			ctx := context.Background()
			if _, _, _, err := store.ImportMemory(ctx, memory.PortableMemory{
				ID: tc.id, ProjectID: "ghost", Category: "fact",
				Content: "A body long enough to be a real memory about the exporter.",
				Source:  "mcp",
			}, memory.ImportOptions{Apply: true}); err != nil {
				t.Fatalf("import memory with id %q: %v", tc.id, err)
			}

			// The canary lives in a SIBLING of the vault, not in the vault's
			// parent: the vault is supposed to appear under its parent, so a
			// snapshot of the parent would report the vault itself as a write
			// outside the target and the assertion would be meaningless.
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

// TestExportSurvivesEveryHostileRecordAtOnce pins the failure the per-id cases
// could each miss: the export is one pass over every project, so a single id the
// filesystem cannot name took down every OTHER project's notes too, on every run
// and every retry, and no error said which record did it.
func TestExportSurvivesEveryHostileRecordAtOnce(t *testing.T) {
	store := seedStore(t)
	ctx := context.Background()
	for i, tc := range hostileIDs {
		if tc.id == "" {
			continue
		}
		if _, _, _, err := store.ImportMemory(ctx, memory.PortableMemory{
			ID: fmt.Sprintf("%s%d", tc.id, i), ProjectID: "ghost", Category: "fact",
			Content: fmt.Sprintf("A body long enough to be a real memory, case %d.", i),
			Source:  "mcp",
		}, memory.ImportOptions{Apply: true}); err != nil {
			t.Fatalf("import %q: %v", tc.id, err)
		}
	}
	// An ordinary project alongside the hostile ones, because the point is that
	// it is not dragged down with them.
	if err := store.EnsureProject(ctx, "bystander", "/tmp/bystander", "bystander"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, "bystander", memory.Memory{
		Category: "fact", Content: "An ordinary memory that must still be exported.", Source: "mcp",
	}); err != nil {
		t.Fatal(err)
	}

	parent := t.TempDir()
	vault := filepath.Join(parent, "vault")
	ex := &Exporter{Store: store, Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))}
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatalf("export failed: %v", err)
	}
	adversarial.Snapshot(t, vault).AssertTreeInside(t, "export", vault)

	notes, err := filepath.Glob(filepath.Join(vault, "bystander", "Memories", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 {
		t.Errorf("the bystander project exported %d notes, want 1", len(notes))
	}
	hostile, err := filepath.Glob(filepath.Join(vault, "ghost", "Memories", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(hostile) != len(hostileIDs)-1 {
		t.Errorf("ghost exported %d notes, want %d — a note escaped Memories/ or was pruned away", len(hostile), len(hostileIDs)-1)
	}
}

// TestFolderNameStaysCreatable defends the over-long-name case (#585).
//
// A project name is whatever a caller sent: EnsureProject stores it verbatim and
// an artifact carries its own. A 300-character name is therefore reachable, and
// MkdirAll answered ENAMETOOLONG for it — so the note was never written, the
// export returned an error, and the auto-sync retried forever. folderNames
// already refused a name that would ESCAPE the vault; a name the filesystem
// cannot hold is refused the same way, because it fails just as completely.
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

// TestHostileContentRendersAsInertStructure is the inertness half for the
// renderers: the shared corpus, planted as memory content, must be present in
// the note verbatim while the front matter around it stays exactly as parseable
// as it is for an ordinary note.
//
// Two things can go wrong and the fixtures separate them. prune identifies a
// note by the ghost_id in its front matter, so a body that looks like front
// matter must not become one — that is TestBodyCannotForgeASecondBlock. And a
// front-matter VALUE carrying a newline, a colon, a quote or an invisible
// character must not change the shape of the line it sits on, which is what this
// test drives: ghost_id stays the first key, every value stays on its own line,
// and hasGhostID — the real reader, the one prune calls — returns the record's
// own id.
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

			// ghost_id first, and it is the record's own — prune's identity check
			// reads exactly this line and nothing else.
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
				t.Errorf("hasGhostID = (%q, %v), want (%q, true) — the payload changed the note's identity for prune", id, ok, m.ID)
			}
		})
	}
}

// TestBodyCannotForgeASecondFrontmatterBlock pins the one property the whole
// prune design rests on: exactly one closed front-matter block per note, and it
// is the one Ghost wrote.
//
// A body is user-authored text that a hand edit or an imported artifact can put
// anything in, including a complete `---` / `ghost_id:` / `---` triple. If that
// reached the front matter, prune would read the body's id, find it absent from
// the keep-set, and delete a live note.
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
			// One block only: the note's front matter ends at the first closing
			// fence, and everything after it is body.
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

// TestTagsCannotImpersonateFrontmatterKeys pins the tags half of the same rule.
//
// A tag is a free string the store does not constrain, and the tags line is a
// flow sequence — so a tag carrying a comma, a bracket or a `key: value` shape
// could end a list, start a new line, or add a key the note never had. Ghost
// quotes any tag that would, which keeps the line one line; what the fixture
// adds is the consequence: hasGhostID must still be the record's own id, and the
// planted `ghost_id` must not be readable as one.
func TestTagsCannotImpersonateFrontmatterKeys(t *testing.T) {
	m := memory.Memory{
		ID: "cc00000000000000", ProjectID: "ghost", Category: "fact",
		Content: "A memory whose tags impersonate Ghost's own front-matter keys.", Importance: 0.7, Source: "mcp",
		Tags: []string{
			"ghost_id: dd00000000000000",
			"pinned: true",
			"category: decision",
			"source: manual",
			"a,b",
			"x[0]",
			`quo"te`,
			"]], type: memory",
		},
	}
	note := renderMemory(m, nil, nil)

	var tagsLine string
	for _, line := range strings.Split(note, "\n") {
		if strings.HasPrefix(line, "tags: ") {
			tagsLine = line
		}
		if strings.HasPrefix(line, "ghost_id: ") && line != "ghost_id: "+m.ID {
			t.Errorf("a second ghost_id line reached the front matter: %q", line)
		}
	}
	if tagsLine == "" {
		t.Fatalf("no tags line rendered:\n%s", note)
	}
	if strings.ContainsAny(tagsLine[len("tags: ["):len(tagsLine)-1], "\n") {
		t.Errorf("the tags list spans more than one line:\n%s", tagsLine)
	}
	for _, planted := range []string{"ghost_id: dd00000000000000", "pinned: true", "category: decision", "source: manual"} {
		adversarial.AssertVerbatim(t, "tags line", tagsLine, planted)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, fileName(m))
	if err := os.WriteFile(path, []byte(note), 0o600); err != nil {
		t.Fatal(err)
	}
	if id, ok := hasGhostID(path); !ok || id != m.ID {
		t.Errorf("hasGhostID = (%q, %v), want (%q, true) — an impersonating tag changed the note's identity", id, ok, m.ID)
	}
}

// TestGhostIDSurvivesItsOwnQuoting pins the agreement the whole prune design
// rests on: the reader must return the id the writer wrote.
//
// fm writes ghost_id through yamlScalar, which quotes any value a YAML reader
// would not take as a plain scalar — and a separator, a backslash or a NUL in an
// id all need quoting, so every id an artifact can bring that is not plain hex
// was written quoted and read back quoted. The mismatch was silent in the worst
// direction: prune looked the quoted text up in a keep-set keyed by the real id,
// did not find it, and deleted the note it had just written, on every export,
// for good. The note existed for exactly as long as the pass that created it.
func TestGhostIDSurvivesItsOwnQuoting(t *testing.T) {
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
		// Quoted because of a character anywhere in the value, not only at its
		// start — these are the cases where fm emits an ESCAPE, so the reader
		// has to undo one rather than just peel a pair of quotes.
		`quo"ted`,
		`back\slash`,
		"trailing:",
		"colon: inside",
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
			if got != id {
				t.Errorf("hasGhostID = %q, want %q\n  the note was written as:\n%s", got, id, note)
			}
		})
	}

	t.Run("a_single_quoted_value_stays_as_written", func(t *testing.T) {
		// This function decides which files prune may DELETE, so it must not
		// start matching a hand-written quoting form it never emitted. A note
		// written that way keeps reading as its own text and stays untouchable.
		dir := t.TempDir()
		path := filepath.Join(dir, "handwritten.md")
		if err := os.WriteFile(path, []byte("---\nghost_id: 'abc'\n---\nbody\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, ok := hasGhostID(path); !ok || got != "'abc'" {
			t.Errorf("hasGhostID = (%q, %v), want ('abc', true)", got, ok)
		}
	})
}

// TestProjectNamesStayCreatableAndTraceable covers the name half of the folder
// rule over the corpus: whatever a project is called, the folder it lands in is
// a local name the filesystem accepts, the note carries the real project id in
// its front matter, and the note is written where prune will look for it.
func TestProjectNamesStayCreatableAndTraceable(t *testing.T) {
	store := seedStore(t)
	ctx := context.Background()
	var projects []memory.Project
	for i, p := range adversarial.PathShapes() {
		id := fmt.Sprintf("shape-%02d", i)
		name := p.Text
		if strings.ContainsAny(name, "\x00\n") {
			// A NUL or a newline in a project name is legal in the column and
			// the renderer quotes it; the folder is what has to survive.
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
