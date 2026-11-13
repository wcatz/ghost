package main

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// A credential-shaped row, and the print sites that would copy it.
//
// #656 refuses a credential on every write path and `ghost reflect` withholds one
// at every site that prints a proposal, so the argument for the rest of cmd/ghost
// is narrow: 70 characters is more than a GitHub PAT needs, and in the autonomous
// path stdout is the append-only lifecycle.log. The defence the lifecycle listings
// had was the write-boundary guard alone, and that guard is not retroactive — a
// database written before it existed can still hold a value, and `ghost resolve`
// prints the first 70 characters of it.
//
// So the fixture below is a pre-guard database, built the only way one can be now:
// CreateFromCorpus is the documented unguarded writer (the corpus path, reachable
// only from the bench seeders) and every path that refuses a credential refuses
// this content too. Each test then renders the site the command renders and holds
// two things: the value is absent, and what replaced it says so.

// preGuardProject is the project the pre-guard rows live in.
const preGuardProject = "preguard"

// preGuardStore opens a store holding one credential-shaped memory, written the
// way a build from before the write-boundary guard existed wrote it, and returns
// the row as the command's own read hands it back. The store is closed when the
// test ends.
func preGuardStore(t *testing.T) (*memory.Store, *sql.DB, memory.Memory) {
	t.Helper()
	ctx := context.Background()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := memory.NewStore(db, nil)
	t.Cleanup(func() { _ = s.Close() })
	if err := s.EnsureProject(ctx, preGuardProject, "/tmp/"+preGuardProject, preGuardProject); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	id, err := s.CreateFromCorpus(ctx, preGuardProject, memory.Memory{
		Category: "gotcha", Content: preGuardContent, Source: "mcp", Importance: 0.7,
	})
	if err != nil {
		t.Fatalf("CreateFromCorpus: %v", err)
	}
	rows, err := s.GetByIDs(ctx, []string{id})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the pre-guard row is not readable (%d rows) — the fixture is the state under test", len(rows))
	}
	// The premise, asserted rather than assumed: the value really is on disk, so
	// a site that withholds it is withholding something and not printing nothing.
	if !strings.Contains(rows[0].Content, preGuardCredential) {
		t.Fatalf("the seeded row does not hold the credential: %q", rows[0].Content)
	}
	return s, db, rows[0]
}

// preGuardContent is a memory holding a GitHub PAT. The token is assembled rather
// than written out because GitHub push protection matches that format anywhere in
// a diff and rejects the push (GH013) before review starts.
var preGuardCredential = "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd"

var preGuardContent = "the deploy token is " + preGuardCredential

// assertWithheld holds the shape every site has to produce: the value is not in
// the output, and a reader can tell that something WAS withheld rather than
// reading a short memory as a whole one. The format is named because an operator
// deciding whether to purge needs to know what shape of value was in the row.
func assertWithheld(t *testing.T, site, out string) {
	t.Helper()
	if strings.Contains(out, preGuardCredential) {
		t.Errorf("%s printed the stored credential:\n%s", site, out)
	}
	if !strings.Contains(out, "<withheld:") {
		t.Errorf("%s did not say it withheld anything:\n%s", site, out)
	}
	if !strings.Contains(out, "GitHub personal access token") {
		t.Errorf("%s did not name the format it withheld:\n%s", site, out)
	}
}

// TestPruneReportWithholdsACredentialInAPreGuardRow: `ghost prune` prints the
// stored text of every row it is about to remove — the report exists so an
// operator can recognise each row before approving its deletion, which is
// precisely the moment a stale, credential-shaped row is most likely to be
// sitting in the corpus. The report reads the stored text, and the write-boundary
// guard is not retroactive, so on a pre-guard database that text can be a
// credential and the listing's 160 bytes is more than enough to print one whole.
// The render goes through the same displayStored substitution every other
// stored-text listing uses, so the marker names the format instead of the value.
func TestPruneReportWithholdsACredentialInAPreGuardRow(t *testing.T) {
	_, _, row := preGuardStore(t)

	var out bytes.Buffer
	if err := printPrune(&out, pruneView{
		Grace: memory.DefaultPruneGrace,
		Now:   time.Now().UTC(),
		Candidates: []memory.PruneCandidate{{
			ID:         row.ID,
			ProjectID:  preGuardProject,
			Category:   row.Category,
			Content:    row.Content,
			Retention:  memory.RetentionSession,
			ExpiresAt:  time.Now().UTC().Add(-24 * time.Hour).Format(memory.StoredStampLayout),
			ActivityAt: time.Now().UTC().Add(-48 * time.Hour).Format(memory.StoredStampLayout),
		}},
	}); err != nil {
		t.Fatalf("printPrune: %v", err)
	}
	assertWithheld(t, "ghost prune", out.String())
}
