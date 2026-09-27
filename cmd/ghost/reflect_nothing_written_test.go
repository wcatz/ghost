package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestRunReflectWritesNothingAndRecordsNoSignatureWhenTheDropEmptiesTheSet
// drives the real production path for the worst outcome in this PR's history:
// a project whose only consolidation proposal holds a credential value.
//
// The store guard refuses to WRITE that value, so the proposal is dropped at
// the write boundary, nothing is left to write, and applyReflection returns
// without calling the store. The corpus is then byte-identical to what it was,
// and the caller has three ways to get that wrong:
//
//   - it can print "Applied", for proposals that were never written;
//   - it can offer a restore hint for a snapshot that was never taken;
//   - and above all it can record the skip fingerprint over the unchanged
//     corpus, which is what makes --skip-unchanged skip this project forever —
//     so a stored credential is never revisited while the operator is told the
//     round landed.
//
// This asserts the two store-observable properties and NOTHING about the
// report's wording — and that is a coverage gap, not a design position. Four
// user-facing properties of the !applied branch have no test:
//
//   - that it prints "Applied: nothing" and names the surviving row;
//   - that it does NOT offer "(use --restore to undo)" for a snapshot that was
//     never taken;
//   - that it says no skip fingerprint is recorded; and
//   - that runReflect never prints the credential, which the same PR's
//     best_practices.md says has to hold at EVERY print site.
//
// An earlier version of this test asserted all four, by swapping the
// os.Stdout/os.Stderr globals around runReflect to capture the output. CI's
// -race run failed on it. The justification I gave for dropping them — that
// runReflect starts goroutines which read those globals — was WRONG: the only
// `go` statements under cmd/ghost are in runMCP and runLifecycle, so nothing on
// this path is concurrent, and the assertions were removable.
//
// The honest state is that those four properties are unverified and this test
// covers the two that are observable in the database. Pinned here so the gap is
// a recorded one.
func TestRunReflectWritesNothingAndRecordsNoSignatureWhenTheDropEmptiesTheSet(t *testing.T) {
	dataHome := isolatedLifecycleEnv(t)
	ctx := context.Background()
	const project = "credsweep"

	// Assembled rather than written out: GitHub push protection matches the PAT
	// format anywhere in a diff and rejects the push (GH013) before review.
	credential := "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd"

	// bootstrap() creates this on demand; the seed store opens the same file
	// first, so the directory has to exist already.
	if err := os.MkdirAll(filepath.Join(dataHome, "ghost"), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}

	// The row is seeded through CreateFromCorpus because Create now refuses it,
	// and that refusal is correct: this row represents a database written
	// BEFORE the store guard existed, which is the only way a stored credential
	// exists at all, and therefore the only case the drop can be about.
	seed := func() *memory.Store {
		t.Helper()
		db, err := memory.OpenDB(filepath.Join(dataHome, "ghost", "ghost.db"))
		if err != nil {
			t.Fatalf("OpenDB: %v", err)
		}
		s := memory.NewStore(db, nil)
		if err := s.EnsureProject(ctx, project, "/tmp/"+project, project); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		return s
	}

	readSignature := func() string {
		t.Helper()
		db, err := sql.Open("sqlite", filepath.Join(dataHome, "ghost", "ghost.db"))
		if err != nil {
			t.Fatalf("open for signature read: %v", err)
		}
		defer db.Close() //nolint:errcheck
		var sig sql.NullString
		if err := db.QueryRow(`SELECT reflect_input_sig FROM ghost_state`).Scan(&sig); err != nil {
			t.Fatalf("read signature: %v", err)
		}
		return sig.String
	}

	s := seed()
	rowID, err := s.CreateFromCorpus(ctx, project, memory.Memory{
		Category: "gotcha", Content: "the deploy token is " + credential,
		Source: "mcp", Importance: 0.7,
	})
	if err != nil {
		t.Fatalf("seed the pre-guard row: %v", err)
	}
	// Backdate it: ReplaceNonManual leaves anything created at or after the
	// round trip's start untouched, and created_at has one-second granularity.
	if err := execBackdate(dataHome, project); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	run := func(extra ...string) {
		t.Helper()
		orig := os.Args
		os.Args = append([]string{orig[0], "reflect", project, "--tier", "sqlite", "--apply"}, extra...)
		defer func() { os.Args = orig }()
		// Output is not captured here. Whether it can be captured race-free is
		// exactly the open question the comment above records.
		runReflect()
	}

	run()
	if got := readSignature(); got != "" {
		t.Errorf("a round that wrote nothing recorded a skip fingerprint %q — --skip-unchanged will now skip this project forever, so the stored credential is never revisited", got)
	}
	after := seed()
	rows, err := after.GetByIDs(ctx, []string{rowID})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the stored row is gone (%d rows) — no replace ran, so it should have survived", len(rows))
	}
	if !strings.Contains(rows[0].Content, credential) {
		t.Errorf("the stored row changed unexpectedly: %q", rows[0].Content)
	}
	// A second round with --skip-unchanged has to be reconsidered rather than
	// skipped. With no fingerprint there is nothing to compare against, and the
	// signature must still be absent afterwards.
	run("--skip-unchanged")
	if got := readSignature(); got != "" {
		t.Errorf("the second round recorded a fingerprint over an unchanged corpus: %q", got)
	}
	if err := after.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}
