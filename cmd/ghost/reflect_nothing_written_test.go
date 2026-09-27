package main

import (
	"context"
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
// without calling the store. Three things then had to be true, and none of them
// were:
//
//   - the report must not say "Applied", because nothing was applied;
//   - the skip fingerprint must not be recorded, because the corpus is
//     byte-identical to what it was — recording it makes --skip-unchanged skip
//     this project forever, so a stored credential is never revisited;
//   - the stored row must still be there, because no replace ran.
//
// The second one is why this test runs `ghost reflect` TWICE with
// --skip-unchanged: the assertion is not about a message, it is about whether
// the next scheduled run would look at this project again.
//
// A store-only test cannot see any of it, because all three are in the caller's
// branch on the `applied` flag.
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

	run := func(extra ...string) string {
		t.Helper()
		orig := os.Args
		os.Args = append([]string{orig[0], "reflect", project, "--tier", "sqlite", "--apply"}, extra...)
		defer func() { os.Args = orig }()
		// Both streams: the proposal listing and the "Applied" summary are
		// stdout, the diagnostics are stderr, and the guarantee is about the
		// whole report.
		var out strings.Builder
		captureOutput(t, &out, &out, func() { runReflect() })
		return out.String()
	}

	first := run()
	if strings.Contains(first, "Applied: 1 memories consolidated") ||
		strings.Contains(first, "(use --restore to undo)") {
		t.Errorf("a round that wrote nothing reported itself as applied:\n%s", first)
	}
	if !strings.Contains(first, "Applied: nothing") {
		t.Errorf("the run did not say that nothing was applied:\n%s", first)
	}
	if !strings.Contains(first, "still there") {
		t.Errorf("the run did not tell the operator the stored row survives:\n%s", first)
	}
	if strings.Contains(first, credential) {
		t.Errorf("the run printed the credential:\n%s", first)
	}
	if !strings.Contains(first, "no skip fingerprint is recorded") {
		t.Errorf("the run did not say it is recording no fingerprint:\n%s", first)
	}

	// The second run is the assertion that matters: with --skip-unchanged, a
	// recorded fingerprint would skip the project and this output would instead
	// say it was skipping. Nothing being fixed means nothing is skipped.
	second := run("--skip-unchanged")
	if strings.Contains(second, "unchanged, skipping") || strings.Contains(second, "--skip-unchanged") {
		t.Errorf("the second run skipped the project, so the fingerprint WAS recorded over an unchanged corpus:\n%s", second)
	}
	if !strings.Contains(second, "Applied: nothing") {
		t.Errorf("the second run did not reconsider the project:\n%s", second)
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
	if err := after.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}
