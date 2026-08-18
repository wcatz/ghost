package main

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// testDeleteStore returns a real in-memory Store with one project ("proj",
// name "test-project") already created, for exercising runProjectDeleteCore
// against real DeleteProject behavior rather than a mock.
func testDeleteStore(t *testing.T) *memory.Store {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := memory.NewStore(db, logger)

	if err := s.EnsureProject(context.Background(), "proj", "/tmp/proj", "test-project"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return s
}

func TestParseObsidianFlags(t *testing.T) {
	t.Run("both forms and all flags", func(t *testing.T) {
		out, project, interval, err := parseObsidianFlags([]string{"--out", "/v", "--project=ghost", "--interval", "5s"})
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if out != "/v" || project != "ghost" || interval != "5s" {
			t.Fatalf("got out=%q project=%q interval=%q", out, project, interval)
		}
	})
	t.Run("empty is fine", func(t *testing.T) {
		if _, _, _, err := parseObsidianFlags(nil); err != nil {
			t.Fatalf("no flags must not error: %v", err)
		}
	})
	t.Run("unknown flag errors", func(t *testing.T) {
		if _, _, _, err := parseObsidianFlags([]string{"--intervl", "5s"}); err == nil {
			t.Fatal("misspelled flag must error, not silently fall back")
		}
	})
	t.Run("missing value errors", func(t *testing.T) {
		if _, _, _, err := parseObsidianFlags([]string{"--interval"}); err == nil {
			t.Fatal("value flag with no argument must error")
		}
	})
	t.Run("bare positional errors", func(t *testing.T) {
		if _, _, _, err := parseObsidianFlags([]string{"oops"}); err == nil {
			t.Fatal("unexpected positional arg must error")
		}
	})
}

// TestParseMCPClient verifies --client parsing rejects a missing or empty
// value (which previously silently fell back to Claude) and accepts both
// "--client NAME" and "--client=NAME" forms.
func TestParseMCPClient(t *testing.T) {
	t.Run("separate argument", func(t *testing.T) {
		client, err := parseMCPClient([]string{"--client", "opencode"})
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if client != "opencode" {
			t.Fatalf("got %q, want opencode", client)
		}
	})
	t.Run("equals form", func(t *testing.T) {
		client, err := parseMCPClient([]string{"--client=opencode"})
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if client != "opencode" {
			t.Fatalf("got %q, want opencode", client)
		}
	})
	t.Run("absent defaults to empty", func(t *testing.T) {
		client, err := parseMCPClient(nil)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if client != "" {
			t.Fatalf("got %q, want empty (caller supplies default)", client)
		}
	})
	t.Run("missing value errors", func(t *testing.T) {
		if _, err := parseMCPClient([]string{"--client"}); err == nil {
			t.Fatal("--client with no value must error, not silently default")
		}
	})
	t.Run("empty equals value errors", func(t *testing.T) {
		if _, err := parseMCPClient([]string{"--client="}); err == nil {
			t.Fatal("--client= with empty value must error, not silently default")
		}
	})
}

// TestRODSNIsReadOnly guards the obsidian commands' read-only guarantee:
// modernc.org/sqlite honors mode=ro only on file: URI DSNs — with a bare
// path the connection opens silently read-write (verified empirically
// against v1.53.0), which is exactly the regression this test would catch.
func TestRODSNIsReadOnly(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	seed, err := memory.OpenDB(dbPath) // creates the schema read-write
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", roDSN(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck

	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('x', '/x', 'x')`); err == nil {
		t.Fatal("write through the read-only DSN must fail")
	}
	// Reads must still work.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM projects`).Scan(&n); err != nil {
		t.Fatalf("read through the read-only DSN must work: %v", err)
	}
}

// TestRODSNEscapesPath guards against a '?' or '#' in the data-dir path being
// parsed as the query separator or a fragment — which would drop mode=ro
// (opening read-write) or open a different file. It also confirms the DSN
// keeps working end-to-end for such paths.
func TestRODSNEscapesPath(t *testing.T) {
	for _, name := range []string{"plain", "with space", "we?rd", "ha#sh"} {
		t.Run(name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), name, "ghost.db")
			if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
				t.Fatal(err)
			}
			// Seed a DB at exactly dbPath via a correctly-escaped writable URI.
			// (memory.OpenDB uses a bare-path DSN that itself misparses a '?'
			// in the path, so it can't seed these cases — a separate concern.)
			seedDSN := (&url.URL{Scheme: "file", Opaque: (&url.URL{Path: dbPath}).EscapedPath()}).String()
			seed, err := sql.Open("sqlite", seedDSN)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := seed.Exec(`CREATE TABLE canary (x)`); err != nil {
				t.Fatalf("seed at %q: %v", dbPath, err)
			}
			if err := seed.Close(); err != nil {
				t.Fatal(err)
			}

			dsn := roDSN(dbPath)
			if !strings.HasPrefix(dsn, "file:") {
				t.Fatalf("DSN must be a file: URI, got %q", dsn)
			}
			// The raw path separators must not leak into the query: the only
			// '?' in the DSN is the query separator introduced by roDSN, and
			// there must be no unescaped '#'.
			if strings.Count(dsn, "?") != 1 || strings.Contains(dsn, "#") {
				t.Fatalf("path special chars not escaped in DSN: %q", dsn)
			}
			if !strings.Contains(dsn, "mode=ro") {
				t.Fatalf("mode=ro missing from DSN: %q", dsn)
			}

			db, err := sql.Open("sqlite", dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close() //nolint:errcheck
			// Reading the canary proves roDSN resolved to the intended file
			// (not a '?'-truncated or '#'-fragmented wrong path).
			var n int
			if err := db.QueryRow(`SELECT COUNT(*) FROM canary`).Scan(&n); err != nil {
				t.Fatalf("read through DSN for path %q must work: %v", name, err)
			}
			if _, err := db.Exec(`INSERT INTO canary VALUES (1)`); err == nil {
				t.Fatalf("write through read-only DSN for path %q must fail", name)
			}
		})
	}
}

func TestConfirmProjectDeleteName_MatchesExactly(t *testing.T) {
	if !confirmProjectDeleteName("my-project\n", "my-project") {
		t.Error("expected trimmed exact match to confirm")
	}
	if !confirmProjectDeleteName("  my-project  ", "my-project") {
		t.Error("expected surrounding whitespace to be trimmed before comparing")
	}
}

func TestConfirmProjectDeleteName_RejectsMismatch(t *testing.T) {
	if confirmProjectDeleteName("my-projec", "my-project") {
		t.Error("expected a partial/typo'd name not to confirm")
	}
	if confirmProjectDeleteName("", "my-project") {
		t.Error("expected an empty input not to confirm")
	}
}

// TestPrintDeleteSummary_FieldsNotTransposed pins the label-to-value mapping
// with six distinct values (one per field) and a whole-string comparison, so
// swapping any two summary.X arguments inside printDeleteSummary — the same
// bug class the memory-layer fixture in store_test.go was hardened against —
// fails this test instead of passing silently.
func TestPrintDeleteSummary_FieldsNotTransposed(t *testing.T) {
	var out bytes.Buffer
	if err := printDeleteSummary(&out, memory.DeleteProjectSummary{
		ProjectID:   "proj",
		ProjectName: "test-project",
		Memories:    1,
		MemoryLinks: 2,
		Tasks:       3,
		Decisions:   4,
		TokenUsage:  5,
		AuditLog:    6,
	}, "Would delete"); err != nil {
		t.Fatalf("printDeleteSummary: %v", err)
	}

	want := `Would delete "test-project" (proj):
  memories:     1
  memory_links: 2
  tasks:        3
  decisions:    4
  token_usage:  5
  audit_log:    6
`
	if out.String() != want {
		t.Errorf("printDeleteSummary output mismatch:\ngot:\n%s\nwant:\n%s", out.String(), want)
	}
}

// TestRunProjectDeleteCore_MismatchAborts exercises the full confirmation
// gate against a real store: apply=true with a wrong re-typed name at the
// prompt must return an error and leave the project (and its memory)
// completely untouched. This is the spec's "wrong re-typed name aborts
// without deleting" requirement.
func TestRunProjectDeleteCore_MismatchAborts(t *testing.T) {
	store := testDeleteStore(t)
	ctx := context.Background()

	if _, err := store.Create(ctx, "proj", memory.Memory{
		Category: "fact", Content: "must survive an aborted delete", Source: "manual", Importance: 0.5, Tags: []string{},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	var out bytes.Buffer
	err := runProjectDeleteCore(ctx, store, &out, strings.NewReader("wrong-name\n"), "test-project", true)
	if err == nil {
		t.Fatal("expected an error for a mismatched confirmation, got nil")
	}
	if !strings.Contains(err.Error(), "did not match") {
		t.Errorf("expected mismatch error, got: %v", err)
	}

	id, _, rErr := store.ResolveProject(ctx, "test-project")
	if rErr != nil || id == "" {
		t.Errorf("expected project to still exist after aborted delete: id=%q err=%v", id, rErr)
	}
	mems, gErr := store.GetAll(ctx, "proj", 100)
	if gErr != nil {
		t.Fatalf("GetAll: %v", gErr)
	}
	if len(mems) != 1 {
		t.Errorf("expected the seeded memory to survive an aborted delete, got %d memories", len(mems))
	}
	if !strings.Contains(out.String(), "Would delete") {
		t.Errorf("expected dry-run summary in output, got %q", out.String())
	}
	if strings.Contains(out.String(), "\nDeleted ") {
		t.Errorf("did not expect the post-apply 'Deleted' report after an aborted confirmation, got %q", out.String())
	}
}

// TestRunProjectDeleteCore_MatchProceeds is the positive counterpart: the
// correct re-typed name must let the delete proceed and actually remove the
// project and its memories.
func TestRunProjectDeleteCore_MatchProceeds(t *testing.T) {
	store := testDeleteStore(t)
	ctx := context.Background()

	if _, err := store.Create(ctx, "proj", memory.Memory{
		Category: "fact", Content: "should be deleted", Source: "manual", Importance: 0.5, Tags: []string{},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	var out bytes.Buffer
	err := runProjectDeleteCore(ctx, store, &out, strings.NewReader("test-project\n"), "test-project", true)
	if err != nil {
		t.Fatalf("expected a correctly re-typed name to proceed, got error: %v", err)
	}

	if !strings.Contains(out.String(), "Deleted ") {
		t.Errorf("expected the post-apply 'Deleted' report, got %q", out.String())
	}
	id, _, rErr := store.ResolveProject(ctx, "test-project")
	if rErr != nil {
		t.Fatalf("ResolveProject: %v", rErr)
	}
	if id != "" {
		t.Error("expected project to be gone after a correctly confirmed delete")
	}
}

// TestRunProjectDeleteCore_DryRunNeverPrompts confirms apply=false stops
// after the preview and never reads from in or deletes anything, regardless
// of what stdin contains.
func TestRunProjectDeleteCore_DryRunNeverPrompts(t *testing.T) {
	store := testDeleteStore(t)
	ctx := context.Background()

	var out bytes.Buffer
	err := runProjectDeleteCore(ctx, store, &out, strings.NewReader(""), "test-project", false)
	if err != nil {
		t.Fatalf("dry-run should not error: %v", err)
	}
	if !strings.Contains(out.String(), "Would delete") || !strings.Contains(out.String(), "Re-run with --apply") {
		t.Errorf("expected dry-run preview and re-run hint, got %q", out.String())
	}
	id, _, rErr := store.ResolveProject(ctx, "test-project")
	if rErr != nil || id == "" {
		t.Errorf("expected project to still exist after dry-run: id=%q err=%v", id, rErr)
	}
}
