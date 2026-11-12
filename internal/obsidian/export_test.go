package obsidian

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func seedStore(t *testing.T) *memory.Store {
	t.Helper()
	_, store := seedStoreWithDB(t)
	return store
}

// seedStoreWithDB is seedStore plus the *sql.DB behind it, for the adversarial
// fixtures that have to plant a row under an id no writer accepts.
//
// `Store.ImportMemory` REFUSES an id carrying a control character, whitespace, a
// backtick or a «» since #791 — a newline in an id forges a second memory line
// on every assembled surface, so it is not written any more. The rows the
// exporter has to survive are still reachable, and still the exporter's problem:
// a store written before the refusal landed, one restored from a snapshot an
// older Ghost took, a hand-edited database. So the fixture plants them in SQL.
func seedStoreWithDB(t *testing.T) (*sql.DB, *memory.Store) {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "ghost", "/tmp/ghost", "ghost"); err != nil {
		t.Fatal(err)
	}
	return db, store
}

func TestExport(t *testing.T) {
	store := seedStore(t)
	ctx := context.Background()
	id1, _ := store.Create(ctx, "ghost", memory.Memory{Category: "gotcha", Content: "First fact about embedding", Importance: 0.8, Source: "mcp"})
	id2, _ := store.Create(ctx, "ghost", memory.Memory{Category: "fact", Content: "Second fact about linking", Importance: 0.7, Source: "mcp"})
	if err := store.CreateLink(ctx, id1, id2, "related", 0.83, "auto"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.RecordDecision(ctx, "ghost", "Use SQLite", "SQLite it is", "zero infra", []string{"Postgres"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateTask(ctx, "ghost", "Ship mirror", "Build it", 2); err != nil {
		t.Fatal(err)
	}
	// A registered project with no entities must not produce a vault folder.
	if err := store.EnsureProject(ctx, "empty", "/tmp/empty", "empty"); err != nil {
		t.Fatal(err)
	}

	vault := filepath.Join(t.TempDir(), "vault")
	ex := &Exporter{Store: store, Logger: slog.Default()}
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatalf("export: %v", err)
	}

	// RecordDecision also saves a companion memory (decisions.go: "creates a
	// decision and also saves it as a memory"), so 2 explicit Creates + 1
	// companion = 3 memory notes.
	mems, _ := filepath.Glob(filepath.Join(vault, "ghost", "Memories", "*.md"))
	if len(mems) != 3 {
		t.Fatalf("want 3 memory notes (2 created + 1 decision companion), got %d", len(mems))
	}
	// Wikilink present between the two linked memories.
	var all strings.Builder
	for _, f := range mems {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		all.Write(data)
	}
	if !strings.Contains(all.String(), "[[") {
		t.Error("expected a wikilink in Related section")
	}
	decs, _ := filepath.Glob(filepath.Join(vault, "ghost", "Decisions", "*.md"))
	tasks, _ := filepath.Glob(filepath.Join(vault, "ghost", "Tasks", "*.md"))
	if len(decs) != 1 || len(tasks) != 1 {
		t.Fatalf("want 1 decision + 1 task, got %d + %d", len(decs), len(tasks))
	}
	if _, err := os.Stat(filepath.Join(vault, "empty")); !os.IsNotExist(err) {
		t.Errorf("project with no entities should not get a folder: %v", err)
	}

	// Idempotence: second export rewrites nothing (mtimes unchanged).
	before, _ := os.Stat(mems[0])
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(mems[0])
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("unchanged note was rewritten")
	}

	// Deletion prunes.
	if err := store.Delete(ctx, id2); err != nil {
		t.Fatal(err)
	}
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatal(err)
	}
	mems2, _ := filepath.Glob(filepath.Join(vault, "ghost", "Memories", "*.md"))
	if len(mems2) >= len(mems) {
		t.Errorf("deleted memory's note should be pruned: %d -> %d", len(mems), len(mems2))
	}
}

// TestExportPrunesRenamedSlug: Upsert strengthens a memory by rewriting its
// content in place (same ID) — when the leading words change, the next export
// writes the note under a new slug. The old-slug file carries the same
// ghost_id, so a keep-set of bare IDs would retain it forever; prune must
// also match the canonical basename.
func TestExportPrunesRenamedSlug(t *testing.T) {
	db, store := seedStoreWithDB(t)
	ctx := context.Background()
	id, err := store.Create(ctx, "ghost", memory.Memory{Category: "fact", Content: "original opening words drive the slug", Importance: 0.8, Source: "mcp"})
	if err != nil {
		t.Fatal(err)
	}

	vault := filepath.Join(t.TempDir(), "vault")
	ex := &Exporter{Store: store, Logger: slog.Default()}
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatalf("export: %v", err)
	}
	old := filepath.Join(vault, "ghost", "Memories", "original-opening-words-drive-the-slug-"+id[:8]+".md")
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("expected note at old slug: %v", err)
	}

	// In-place content rewrite, same ID — the direct-SQL equivalent of
	// Upsert's strengthen path (store.go: UPDATE memories SET content ...).
	if _, err := db.ExecContext(ctx, `UPDATE memories SET content = ? WHERE id = ?`, "different phrasing appears here", id); err != nil {
		t.Fatal(err)
	}
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatalf("second export: %v", err)
	}

	notes, _ := filepath.Glob(filepath.Join(vault, "ghost", "Memories", "*.md"))
	if len(notes) != 1 {
		t.Fatalf("renamed memory must leave exactly 1 note, got %d: %v", len(notes), notes)
	}
	want := "different-phrasing-appears-here-" + id[:8] + ".md"
	if filepath.Base(notes[0]) != want {
		t.Errorf("surviving note = %s, want %s", filepath.Base(notes[0]), want)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("old-slug note must be pruned after rename: %v", err)
	}
}

func TestExportProjectFilter(t *testing.T) {
	store := seedStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "widget", "/tmp/widget", "widget"); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatal(err)
	}
	ghostID, _ := store.Create(ctx, "ghost", memory.Memory{Category: "fact", Content: "Ghost project fact", Importance: 0.8, Source: "mcp"})
	widgetID, _ := store.Create(ctx, "widget", memory.Memory{Category: "fact", Content: "Widget project fact", Importance: 0.8, Source: "mcp"})
	if _, err := store.Create(ctx, "_global", memory.Memory{Category: "preference", Content: "Global preference fact", Importance: 0.9, Source: "mcp"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateLink(ctx, ghostID, widgetID, "related", 0.9, "auto"); err != nil {
		t.Fatal(err)
	}

	vault := filepath.Join(t.TempDir(), "vault")
	ex := &Exporter{Store: store, Logger: slog.Default()}
	if err := ex.Export(ctx, vault, "ghost"); err != nil {
		t.Fatalf("export: %v", err)
	}

	// Filtered project exported; _global rides along under "Global".
	ghostMems, _ := filepath.Glob(filepath.Join(vault, "ghost", "Memories", "*.md"))
	if len(ghostMems) != 1 {
		t.Fatalf("want 1 ghost memory note, got %d", len(ghostMems))
	}
	globalMems, _ := filepath.Glob(filepath.Join(vault, "Global", "Memories", "*.md"))
	if len(globalMems) != 1 {
		t.Fatalf("want 1 global memory note under Global/, got %d", len(globalMems))
	}
	// Filtered-out project gets no folder at all.
	if _, err := os.Stat(filepath.Join(vault, "widget")); !os.IsNotExist(err) {
		t.Errorf("filtered-out project should not get a folder: %v", err)
	}
	// Link to a filtered-out memory degrades to a plain short ID, not a wikilink.
	data, err := os.ReadFile(ghostMems[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "[[") {
		t.Error("link to filtered-out project should not be a wikilink")
	}
	if !strings.Contains(string(data), widgetID[:8]) {
		t.Error("link to filtered-out project should degrade to its short ID")
	}

	// _global alone never satisfies a filter.
	if err := ex.Export(ctx, vault, "no-such-project"); err == nil {
		t.Error("expected error for unmatched project filter")
	}
}

// TestExportOrphanCleanupKeepsUserFiles: an orphaned project folder is cleaned
// one Ghost note at a time. User-authored files and folders inside it — notes
// without a ghost_id, attachments, a user subfolder — survive, and so does the
// folder that still holds them. A symlink inside it is never followed.
func TestExportOrphanCleanupKeepsUserFiles(t *testing.T) {
	store := seedStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "doomed", "/tmp/doomed", "doomed"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, "doomed", memory.Memory{Category: "fact", Content: "Doomed fact", Importance: 0.8, Source: "mcp"}); err != nil {
		t.Fatal(err)
	}
	vault := filepath.Join(t.TempDir(), "vault")
	ex := &Exporter{Store: store, Logger: slog.Default()}
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatal(err)
	}

	userNote := filepath.Join(vault, "doomed", "Memories", "my-own-note.md")
	mustWrite(t, userNote, "# mine\nno frontmatter here\n")
	attachment := filepath.Join(vault, "doomed", "attachments", "diagram.png")
	outside := filepath.Join(t.TempDir(), "outside")
	for _, d := range []string{filepath.Dir(attachment), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, attachment, "png")
	mustWrite(t, filepath.Join(outside, "keep.md"), "---\nghost_id: not-yours\n---\n")
	if err := os.Symlink(outside, filepath.Join(vault, "doomed", "linked")); err != nil {
		t.Fatal(err)
	}

	if _, err := store.DeleteProject(ctx, "doomed", true); err != nil {
		t.Fatal(err)
	}
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatal(err)
	}

	notes, _ := filepath.Glob(filepath.Join(vault, "doomed", "Memories", "*.md"))
	if len(notes) != 1 || notes[0] != userNote {
		t.Errorf("only the user's note should remain in the orphan folder, got %v", notes)
	}
	for _, p := range []string{userNote, attachment, filepath.Join(outside, "keep.md")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("user file %s must survive orphan cleanup: %v", p, err)
		}
	}
	if fi, err := os.Lstat(filepath.Join(vault, "doomed", "linked")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("symlink inside the orphan folder must be left alone: %v", err)
	}
}

// TestHasGhostIDNeedsClosedFrontmatter: a note that opens with a --- rule and
// mentions ghost_id later in its body, with no closing ---, is not Ghost's.
func TestHasGhostIDNeedsClosedFrontmatter(t *testing.T) {
	dir := t.TempDir()
	open := filepath.Join(dir, "hr.md")
	mustWrite(t, open, "---\nMy notes after a rule\nghost_id: mentioned in body\nmore text\n")
	if _, ok := hasGhostID(open); ok {
		t.Error("unclosed frontmatter must not count as a ghost_id")
	}
	closed := filepath.Join(dir, "note.md")
	mustWrite(t, closed, "---\nghost_id: abc\n---\nbody\n")
	if id, ok := hasGhostID(closed); !ok || id != "abc" {
		t.Errorf("closed frontmatter ghost_id = %q, %v; want abc, true", id, ok)
	}
}

// TestVaultWritesArePrivate: directories and files Ghost creates are 0700 and
// 0600; a folder the user created keeps its own mode.
func TestVaultWritesArePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits")
	}
	store := seedStore(t)
	ctx := context.Background()
	if _, err := store.Create(ctx, "ghost", memory.Memory{Category: "fact", Content: "Ghost fact", Importance: 0.8, Source: "mcp"}); err != nil {
		t.Fatal(err)
	}
	vault := filepath.Join(t.TempDir(), "vault")
	ex := &Exporter{Store: store, Logger: slog.Default()}
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatal(err)
	}
	userDir := filepath.Join(vault, "UserStuff")
	if err := os.Mkdir(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatal(err)
	}
	notes, _ := filepath.Glob(filepath.Join(vault, "ghost", "Memories", "*.md"))
	if len(notes) != 1 {
		t.Fatalf("want 1 note, got %d", len(notes))
	}
	checks := map[string]os.FileMode{
		vault:                            0o700,
		filepath.Join(vault, "ghost"):    0o700,
		filepath.Join(vault, markerName): 0o600,
		notes[0]:                         0o600,
		userDir:                          0o755,
	}
	for p, want := range checks {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s: mode %o, want %o", p, got, want)
		}
	}
}

func TestExportSkipsPruneOnTruncatedList(t *testing.T) {
	store := seedStore(t)
	ctx := context.Background()
	for _, m := range []memory.Memory{
		{Category: "fact", Content: "Highest importance fact", Importance: 0.9, Source: "mcp"},
		{Category: "fact", Content: "Middle importance fact", Importance: 0.8, Source: "mcp"},
		{Category: "fact", Content: "Lowest importance fact", Importance: 0.7, Source: "mcp"},
	} {
		if _, err := store.Create(ctx, "ghost", m); err != nil {
			t.Fatal(err)
		}
	}

	vault := filepath.Join(t.TempDir(), "vault")
	ex := &Exporter{Store: store, Logger: slog.Default()}
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatalf("export: %v", err)
	}
	notes, _ := filepath.Glob(filepath.Join(vault, "ghost", "Memories", "*.md"))
	if len(notes) != 3 {
		t.Fatalf("want 3 notes after full export, got %d", len(notes))
	}

	// With the list limit squeezed below the row count, GetAll returns
	// exactly limit rows — the export must treat the project as possibly
	// truncated and skip pruning its folder: the third note survives even
	// though it is absent from the keep-set (stale beats deleted).
	limited := &Exporter{Store: store, Logger: slog.Default(), listLimit: 2}
	if err := limited.Export(ctx, vault, ""); err != nil {
		t.Fatalf("limited export: %v", err)
	}
	notes, _ = filepath.Glob(filepath.Join(vault, "ghost", "Memories", "*.md"))
	if len(notes) != 3 {
		t.Fatalf("truncated export must not prune: want 3 surviving notes, got %d", len(notes))
	}
}

// TestExportRefusesAmbiguousProjectName: a project's name is not unique, so a
// --project filter two projects answer to must be refused, naming both ids,
// before the vault is touched, so a fresh vault path is not even created.
func TestExportRefusesAmbiguousProjectName(t *testing.T) {
	store := seedStore(t)
	ctx := context.Background()
	for _, id := range []string{"dup-a", "dup-b"} {
		if err := store.EnsureProject(ctx, id, "/tmp/"+id, "dup"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Create(ctx, id, memory.Memory{Category: "fact", Content: "Fact under " + id, Importance: 0.8, Source: "mcp"}); err != nil {
			t.Fatal(err)
		}
	}
	ex := &Exporter{Store: store, Logger: slog.Default()}

	fresh := filepath.Join(t.TempDir(), "vault")
	err := ex.Export(ctx, fresh, "dup")
	if err == nil {
		t.Fatal("an export of a name two projects answer to succeeded")
	}
	if !strings.Contains(err.Error(), "multiple projects") {
		t.Errorf("error = %v, want it to say the name is ambiguous", err)
	}
	for _, id := range []string{"dup-a", "dup-b"} {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("error = %v, want it to name %s so the operator can pick one", err, id)
		}
	}
	if _, statErr := os.Stat(fresh); !os.IsNotExist(statErr) {
		t.Errorf("a refused export created the vault: %v", statErr)
	}
}

// tokenShapedProjectID is a project id an imported or older store can hold:
// the import check refuses control characters, backticks and delimiters but
// never asks the token guard, so a GitHub token-shaped value is storable
// verbatim and the exporter has to withhold it on the way out.
func tokenShapedProjectID() string {
	return "ghp_" + strings.Repeat("a", 36)
}

func seedTruncationProject(t *testing.T, store *memory.Store, projectID, name string) {
	t.Helper()
	ctx := context.Background()
	if err := store.EnsureProject(ctx, projectID, "/tmp/"+name, name); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"First fact", "Second fact", "Third fact"} {
		if _, err := store.Create(ctx, projectID, memory.Memory{Category: "fact", Content: content, Importance: 0.8, Source: "mcp"}); err != nil {
			t.Fatal(err)
		}
	}
	for i, title := range []string{"First decision", "Second decision"} {
		if _, _, _, err := store.RecordDecision(ctx, projectID, title, "Use A", "Because A", []string{"B"}, nil); err != nil {
			t.Fatalf("record decision %d: %v", i, err)
		}
	}
	for i, title := range []string{"First task", "Second task"} {
		if _, err := store.CreateTask(ctx, projectID, title, "Do it", 2); err != nil {
			t.Fatalf("create task %d: %v", i, err)
		}
	}
}

// TestExportWithholdsTokenShapedProjectIDFromTruncationWarnings pins the
// warn half of the stored-id rule: a stored project id that is token-shaped
// must not reach the log file raw, while the warning still names the kind and
// the fact that the list was cut.
func TestExportWithholdsTokenShapedProjectIDFromTruncationWarnings(t *testing.T) {
	store := seedStore(t)
	ctx := context.Background()
	tokenID := tokenShapedProjectID()
	seedTruncationProject(t, store, tokenID, "tokenwarn")

	var buf logBuffer
	ex := &Exporter{Store: store, Logger: slog.New(slog.NewTextHandler(&buf, nil)), listLimit: 2}
	vault := filepath.Join(t.TempDir(), "vault")
	if err := ex.Export(ctx, vault, tokenID); err != nil {
		t.Fatalf("export: %v", err)
	}
	logged := buf.String()
	if strings.Contains(logged, tokenID) {
		t.Errorf("truncation warning echoed a token-shaped stored project id: %s", logged)
	}
	if !strings.Contains(logged, "withheld") || !strings.Contains(logged, "GitHub personal access token") {
		t.Errorf("truncation warning did not withhold the token-shaped id by its format: %s", logged)
	}
	for _, kind := range []string{"memories", "decisions", "tasks"} {
		if !strings.Contains(logged, "list truncated at limit") || !strings.Contains(logged, "kind="+kind) {
			t.Errorf("truncation warning for %s missing from log: %s", kind, logged)
		}
	}
}

// TestExportNamesOrdinaryProjectIDInTruncationWarnings is the other half:
// an ordinary stored id stays readable so the warning still says which
// project was cut. Without it a renderer that withheld every id would pass
// the test above.
func TestExportNamesOrdinaryProjectIDInTruncationWarnings(t *testing.T) {
	store := seedStore(t)
	ctx := context.Background()
	const ordinaryID = "ordinary-project-id"
	seedTruncationProject(t, store, ordinaryID, "ordinarywarn")

	var buf logBuffer
	ex := &Exporter{Store: store, Logger: slog.New(slog.NewTextHandler(&buf, nil)), listLimit: 2}
	vault := filepath.Join(t.TempDir(), "vault")
	if err := ex.Export(ctx, vault, ordinaryID); err != nil {
		t.Fatalf("export: %v", err)
	}
	logged := buf.String()
	if !strings.Contains(logged, ordinaryID) {
		t.Errorf("truncation warning withheld an ordinary stored id: %s", logged)
	}
	if strings.Contains(logged, "withheld") {
		t.Errorf("truncation warning withheld an ordinary stored id: %s", logged)
	}
}

// TestExportWithholdsOverLimitProjectIDFromTruncationWarnings pins the bound
// half: a stored id longer than one refusal repeats is withheld with its
// length, not treated as ordinary.
func TestExportWithholdsOverLimitProjectIDFromTruncationWarnings(t *testing.T) {
	store := seedStore(t)
	ctx := context.Background()
	longID := strings.Repeat("a", 300)
	seedTruncationProject(t, store, longID, "longwarn")

	var buf logBuffer
	ex := &Exporter{Store: store, Logger: slog.New(slog.NewTextHandler(&buf, nil)), listLimit: 2}
	vault := filepath.Join(t.TempDir(), "vault")
	if err := ex.Export(ctx, vault, longID); err != nil {
		t.Fatalf("export: %v", err)
	}
	logged := buf.String()
	if strings.Contains(logged, longID) {
		t.Errorf("truncation warning echoed an over-limit stored project id")
	}
	if !strings.Contains(logged, "withheld") || !strings.Contains(logged, "bytes") {
		t.Errorf("truncation warning did not withhold the over-limit id by its length: %s", logged)
	}
}

func TestExportWithholdsTokenShapedStoredIDsInErrorPrefixes(t *testing.T) {
	tokenID := tokenShapedProjectID()
	withheld := func(kind string) string {
		return `"<` + kind + ` withheld: it holds a GitHub personal access token>"`
	}
	for _, tc := range []struct {
		name, table, filter, want string
		plantMemoryID             bool
	}{
		{"load", "memories", tokenID, "load memories for " + withheld("project_id"), false},
		{"links", "memory_links", "ghost", "links for " + withheld("memory_id"), true},
		{"decisions", "decisions", tokenID, "decisions for " + withheld("project_id"), false},
		{"tasks", "tasks", tokenID, "tasks for " + withheld("project_id"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, store := seedStoreWithDB(t)
			ctx := context.Background()
			if tc.plantMemoryID {
				plantMemory(t, db, "ghost", tokenID, "A body long enough to be a real memory about the exporter.")
			} else if err := store.EnsureProject(ctx, tokenID, "/tmp/token"+tc.name, "token"+tc.name); err != nil {
				t.Fatal(err)
			}
			if tc.table != "memories" && !tc.plantMemoryID {
				if _, err := store.Create(ctx, tokenID, memory.Memory{Category: "fact", Content: "A fact", Importance: 0.8, Source: "mcp"}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.ExecContext(ctx, `DROP TABLE `+tc.table); err != nil {
				t.Fatal(err)
			}
			ex := &Exporter{Store: store, Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))}
			err := ex.Export(ctx, filepath.Join(t.TempDir(), "vault"), tc.filter)
			if err == nil {
				t.Fatalf("want the %s failure, got nil", tc.name)
			}
			if strings.Contains(err.Error(), tokenID) {
				t.Errorf("%s failure echoed a token-shaped stored id: %v", tc.name, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s failure did not withhold the token-shaped id by its format: %v", tc.name, err)
			}
		})
	}
}

func TestExportNamesOrdinaryStoredIDsInErrorPrefixes(t *testing.T) {
	const ordinaryID = "ordinary-project-id"
	db, store := seedStoreWithDB(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, ordinaryID, "/tmp/ordinaryload", "ordinaryload"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE memories`); err != nil {
		t.Fatal(err)
	}
	vault := filepath.Join(t.TempDir(), "vault")
	ex := &Exporter{Store: store, Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))}
	err := ex.Export(ctx, vault, ordinaryID)
	if err == nil {
		t.Fatal("want the load failure, got nil")
	}
	if want := `load memories for "` + ordinaryID + `"`; !strings.Contains(err.Error(), want) {
		t.Errorf("load failure did not name an ordinary stored id quoted: %v", err)
	}
}
