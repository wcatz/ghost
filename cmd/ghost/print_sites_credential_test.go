package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/resolve"
	"github.com/wcatz/ghost/internal/supersede"
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

// TestResolveMarkReportWithholdsACredentialInAPreGuardRow: `ghost resolve --mark`
// names every memory it stamped, with the row's own text, so an operator can
// confirm it buried the note they meant. The row it reads is the stored corpus,
// so on a pre-guard database that text can be a credential — and 70 characters of
// a PAT is the whole of it.
//
// Mark() itself is driven here rather than the report being handed a literal, so
// what the report is given is what the command is given.
func TestResolveMarkReportWithholdsACredentialInAPreGuardRow(t *testing.T) {
	s, _, row := preGuardStore(t)

	res, err := resolve.Mark(context.Background(), s, resolve.MarkRequest{
		ProjectID: preGuardProject,
		Refs:      []string{row.ID},
	}, nil)
	if err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if res.Resolved != 1 || len(res.Memories) != 1 {
		t.Fatalf("Mark resolved %d memories, want the one seeded row", res.Resolved)
	}
	if res.Memories[0].Content != preGuardContent {
		t.Errorf("the mark result carries %q, want the stored content — the fixture has to be the pre-guard state", res.Memories[0].Content)
	}
	assertWithheld(t, "resolve --mark", resolveMarkReport(preGuardProject, res, false))
}

// TestResolveListingsWithholdACredentialInAPreGuardRow: the two listings that
// share one renderer — the memories `ghost resolve` confirmed and the ones
// `ghost resolve --reassess` kept — each print the stored row so a caller can
// see what the pass decided about it. Both are one function, which is the whole
// of both sites.
func TestResolveListingsWithholdACredentialInAPreGuardRow(t *testing.T) {
	_, _, row := preGuardStore(t)
	kept := []memory.Memory{row}
	for _, tc := range []struct{ command string }{
		{command: "resolve"},
		{command: "resolve --reassess"},
	} {
		t.Run(tc.command, func(t *testing.T) {
			assertWithheld(t, tc.command, memoryLines(kept))
		})
	}
}

// TestResolveListingsStillRenderACleanRowInFull is the other half of the change:
// withholding a credential must not cost the listing its shape. The first-line
// cut and the 70-rune width are what these lines are, and a row holding an
// ordinary sentence still has to be readable and still identifiable.
func TestResolveListingsStillRenderACleanRowInFull(t *testing.T) {
	const body = "the staging relay port is 2222 and nothing else"
	rows := []memory.Memory{{
		ID: "A1B2C3D4E5F60718293A4B5C6D7E8F90", Category: "changelog", Content: body + "\nand a second line the listing never showed",
	}}
	out := memoryLines(rows)
	if !strings.Contains(out, body) {
		t.Errorf("a clean row is not rendered: %q", out)
	}
	// The first-line cut is unchanged: a multi-line memory took one line before
	// this and takes one now, so a listing cannot start printing paragraphs.
	if strings.Contains(out, "second line") {
		t.Errorf("the listing printed a second line of a stored memory: %q", out)
	}
	if !strings.Contains(out, "[changelog]") {
		t.Errorf("the listing lost the row's category: %q", out)
	}
	// And the width: 70 runes of a long row, still marked as cut.
	long := strings.Repeat("x", 200)
	cut := memoryLines([]memory.Memory{{ID: "B", Category: "fact", Content: long}})
	if !strings.Contains(cut, "…") {
		t.Errorf("a long row is not marked as truncated: %q", cut)
	}
	if strings.Count(cut, "x") != 70 {
		t.Errorf("a long row printed %d characters, want the listing's 70", strings.Count(cut, "x"))
	}
}

// TestSupersedeWithdrawReportWithholdsACredentialInAPreGuardRow: the withdrawal
// report prints the target's own first line so an operator withdrawing an edge can
// check from the output that it was the right edge. The target is a stored memory,
// and `ghost supersede --withdraw` is the repair path an operator reaches for when
// an edge is wrong — so it is read on exactly the databases where a stale,
// credential-shaped row is most likely to still be sitting there.
//
// The edge is created and withdrawn for real, so the text in the report is the
// text the store read.
func TestSupersedeWithdrawReportWithholdsACredentialInAPreGuardRow(t *testing.T) {
	s, _, row := preGuardStore(t)
	ctx := context.Background()

	// The superseding endpoint: an ordinary note, so the credential is only ever in
	// the target's text — which is the field the report prints.
	newer, err := s.Create(ctx, preGuardProject, memory.Memory{
		Category: "fact", Content: "the relay now reads its token from the runner env", Source: "mcp", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("Create the superseding memory: %v", err)
	}
	if err := s.CreateLink(ctx, newer, row.ID, "supersedes", 0.9, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	res, err := supersede.Withdraw(ctx, s, preGuardProject,
		[]supersede.WithdrawPair{{Source: newer, Target: row.ID}}, false, nil)
	if err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if len(res.Links) != 1 {
		t.Fatalf("the withdrawal resolved %d edges, want the one just created", len(res.Links))
	}
	if res.Links[0].TargetText != preGuardContent {
		t.Errorf("the withdrawal carries %q, want the stored target text", res.Links[0].TargetText)
	}
	assertWithheld(t, "supersede --withdraw", supersedeWithdrawReport(preGuardProject, res, false))
}

// TestHistoryPrintersWithholdCredentialsInPreGuardRows: `ghost history` prints the
// text a memory used to hold, which is the one surface where a credential
// outlives the row it was removed from. The write-time filter
// (`ghost_history_content`) redacts every row appended after it was installed, and
// that is the right layer for it — it is the only one that also covers the reads
// Ghost makes itself, an as_of search answering from a recorded version. It is not
// retroactive, though, and it does not cover `merged_content` at all, so the print
// site carries the substitution too: this is a database whose history rows were
// written before the filter existed, which is the state `ghost history` is asked
// about on exactly the store that needs purging.
func TestHistoryPrintersWithholdCredentialsInPreGuardRows(t *testing.T) {
	s, db, row := preGuardStore(t)
	ctx := context.Background()

	// Two rows, because printHistoryEntry prints two text fields and one of them is
	// not the write-time filter's: `content` goes through
	// `ghost_history_content`, and `merged_content` is a plain column beside it —
	// so this second row is the field the filter never saw. It is also the only
	// field here that reaches the marker with no category, which is what makes the
	// marker's two shapes observable rather than theoretical.
	insertPreGuardHistory(t, db, row.ID, preGuardContent, "", "gotcha")
	insertPreGuardHistory(t, db, row.ID, "the relay now reads its token from the runner env",
		"the older wording held "+preGuardCredential, "gotcha")

	entries, err := s.MemoryHistory(ctx, row.ID, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	// The corpus write left a history row of its own, and the write-time filter
	// redacted it — so the two rows inserted above are the only ones still holding
	// a value, which is the whole difference between the two layers.
	var unfiltered int
	for _, e := range entries {
		if strings.Contains(e.Content, preGuardCredential) || strings.Contains(e.MergedContent, preGuardCredential) {
			unfiltered++
		}
	}
	if unfiltered != 2 {
		t.Fatalf("%d of %d history rows hold the value, want the two inserted past the filter", unfiltered, len(entries))
	}

	t.Run("the human form", func(t *testing.T) {
		var out strings.Builder
		view := historyView{MemoryID: row.ID, Entries: entries, Live: &row}
		if err := printMemoryHistory(&out, view); err != nil {
			t.Fatalf("printMemoryHistory: %v", err)
		}
		if strings.Contains(out.String(), preGuardCredential) {
			t.Errorf("`ghost history` printed a stored credential:\n%s", out.String())
		}
		if strings.Count(out.String(), "<withheld:") != 2 {
			// Two fields hold a value and both say so, and a reader can count what
			// was withheld: the first row's content, and the second row's folded-in
			// text. The second row's own content is clean and is printed in full.
			t.Errorf("`ghost history` withheld %d field(s), want 2:\n%s",
				strings.Count(out.String(), "<withheld:"), out.String())
		}
		// The two shapes, because the marker reports what the caller knows: the
		// entry's own content names its category, and the folded-in text does not
		// carry one — a history row records no category for a fold's discarded
		// wording — so it prints the short form rather than an empty `category=`.
		if !strings.Contains(out.String(), "category=gotcha") {
			t.Errorf("`ghost history` did not name the category it holds:\n%s", out.String())
		}
		if strings.Contains(out.String(), "category=,") || strings.Contains(out.String(), "category=>") {
			t.Errorf("`ghost history` printed an empty category in a marker:\n%s", out.String())
		}
		// The rest of the entry is still there. The history answers "what did this
		// memory say, when, and who wrote it", and a withheld value must not cost
		// the reader the timestamp, the performer or the category.
		for _, want := range []string{"save", "preguard", "gotcha"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("`ghost history` lost %q:\n%s", want, out.String())
			}
		}
		// A clean version is still printed in full: the history exists to answer
		// "what did this hold", and a cut is not an answer.
		if !strings.Contains(out.String(), "reads its token from the runner env") {
			t.Errorf("`ghost history` elided a clean entry's text:\n%s", out.String())
		}
		// And the write-time filter's own notice survives. It is the other layer,
		// and "this earlier version held a value of that shape" says something a
		// `<withheld: …>` marker alone does not: that the row was redacted on the
		// way in, which is what `ghost history purge` is for.
		if !strings.Contains(out.String(), "<redacted:") {
			t.Errorf("`ghost history` lost the write-time redaction notice:\n%s", out.String())
		}
	})

	t.Run("the json form", func(t *testing.T) {
		var out strings.Builder
		if err := printHistory(&out, historyView{MemoryID: row.ID, Entries: entries}, true); err != nil {
			t.Fatalf("printHistory --json: %v", err)
		}
		// Decoded rather than grepped, because the encoder escapes the marker
		// (`<` is \u003c) and a consumer reads the decoded value: a test that
		// matched the marker textually would be testing the encoder.
		dec := json.NewDecoder(strings.NewReader(out.String()))
		var got []memory.HistoryEntry
		for {
			var e memory.HistoryEntry
			if err := dec.Decode(&e); err == io.EOF {
				break
			} else if err != nil {
				t.Fatalf("decode the --json stream: %v", err)
			}
			got = append(got, e)
		}
		if len(got) != len(entries) {
			t.Fatalf("the --json stream holds %d entries, want the %d read", len(got), len(entries))
		}
		var withheldContent, withheldMerged int
		for i, e := range got {
			if strings.Contains(e.Content, preGuardCredential) || strings.Contains(e.MergedContent, preGuardCredential) {
				t.Errorf("`ghost history --json` printed a stored credential: %+v", e)
			}
			if strings.HasPrefix(e.Content, "<withheld:") {
				withheldContent++
			} else if e.Content != entries[i].Content {
				// The schema is the entry's own and only the VALUE changes: a
				// clean row decodes back to exactly what the store returned, so
				// nothing downstream of this command has to learn a second shape.
				t.Errorf("entry %d's clean content changed: %q", i, e.Content)
			}
			if strings.HasPrefix(e.MergedContent, "<withheld:") {
				withheldMerged++
			}
			// The same two shapes in the machine form, so a consumer branching on
			// the marker sees what a person reading it sees.
			if strings.HasPrefix(e.MergedContent, "<withheld:") && strings.Contains(e.MergedContent, "category=") {
				t.Errorf("the folded-in text's marker names a category it does not have: %q", e.MergedContent)
			}
		}
		if withheldContent != 1 || withheldMerged != 1 {
			t.Errorf("the --json stream withheld %d content and %d folded-in field(s), want 1 and 1: %s",
				withheldContent, withheldMerged, out.String())
		}
	})
}

// insertPreGuardHistory writes one history row the way the build that predates
// `ghost_history_content` wrote it: the text goes into the column with no filter,
// because there was none. Nothing through the store's own writers can produce this
// state, which is the point — a test that reached it through a writer would be
// testing the guard rather than the reader.
func insertPreGuardHistory(t *testing.T, db *sql.DB, memoryID, content, merged, category string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO memory_history
		(memory_id, project_id, phase, agent, session_id, related_id, merged_content,
		 content, category, importance, resolved_at, source, recorded_at)
		VALUES (?, ?, 'save', 'opencode', '', '', ?, ?, ?, 0.7, NULL, 'mcp',
			'2020-01-01T00:00:00Z')`,
		memoryID, preGuardProject, merged, content, category); err != nil {
		t.Fatalf("insert the pre-guard history row: %v", err)
	}
}
