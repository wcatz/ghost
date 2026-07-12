package main

import (
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

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
