package memory

import (
	"context"
	"testing"
)

// TestFoldOnlyEquivalentIsEqualityOnly is the final architect ruling, case by
// case. FoldOnly DROPS the incoming text, so anything the comparison tolerates
// is a way for the opposite of what was promoted to be silently discarded. The
// rule is therefore equality after normalization, and nothing fuzzy — every pair
// below that differs in a word must be stored as its own row.
//
// The opposites are the ones that matter: all of them are the same sentence
// with the meaning inverted, which is the shape a similarity score cannot see.
func TestFoldOnlyEquivalentIsEqualityOnly(t *testing.T) {
	mustNotFold := []struct{ name, a, b string }{
		{
			"long instruction: required versus optional",
			"before the release can be cut, the API token is required for every client that talks to the staging gateway, and the team should confirm the rotation policy in writing",
			"before the release can be cut, the API token is optional for every client that talks to the staging gateway, and the team should confirm the rotation policy in writing",
		},
		{
			"long instruction: fine versus forbidden",
			"the internal audit log is fine to retain indefinitely because it is how we answer questions about past incidents after the fact",
			"the internal audit log is forbidden to retain indefinitely because it is how we answer questions about past incidents after the fact",
		},
		{
			"long instruction: negated versus not",
			"Don't run migrations against the production database without first taking a verified snapshot and confirming the restore procedure with the on-call engineer",
			"Run migrations against the production database without first taking a verified snapshot and confirming the restore procedure with the on-call engineer",
		},
		{
			"short: required versus optional",
			"the API token is required",
			"the API token is optional",
		},
		{
			"curly apostrophe present versus absent is a difference in the WORD, not the mark",
			"Don't commit directly to the main branch",
			"Do not commit directly to the main branch",
		},
		{
			"reordered",
			"use sqlite not postgres for storage",
			"use postgres not sqlite for storage",
		},
		{
			"one word differs",
			"prefer tabs over spaces",
			"prefer spaces over tabs",
		},
	}
	for _, c := range mustNotFold {
		t.Run("not fold/"+c.name, func(t *testing.T) {
			if foldOnlyEquivalent(c.a, c.b) {
				t.Errorf("foldOnlyEquivalent(%q, %q) = true, want false — the texts state different claims", c.a, c.b)
			}
		})
	}

	// The positive control, without which a rule that refuses everything would
	// pass every case above while reintroducing the redundancy this option
	// exists to remove. Every pair here differs only in a way that cannot change
	// what the sentence says.
	mustFold := []struct{ name, a, b string }{
		{
			"identical",
			"run the linter before pushing",
			"run the linter before pushing",
		},
		{
			"case",
			"Run The Linter Before Pushing",
			"run the linter before pushing",
		},
		{
			"whitespace",
			"run  the linter   before pushing",
			"run the linter before pushing",
		},
		{
			"punctuation",
			"run the linter, before pushing.",
			"run the linter before pushing",
		},
		{
			"curly apostrophe",
			"don’t run the linter before pushing",
			"don't run the linter before pushing",
		},
		{
			"modifier letter apostrophe",
			"donʼt run the linter before pushing",
			"don't run the linter before pushing",
		},
		{
			"NFKC: fullwidth letters",
			"ｒｕｎ　ｔｈｅ ｌｉｎｔｅｒ",
			"run the linter",
		},
	}
	for _, c := range mustFold {
		t.Run("fold/"+c.name, func(t *testing.T) {
			if !foldOnlyEquivalent(c.a, c.b) {
				t.Errorf("foldOnlyEquivalent(%q, %q) = false, want true — the same sentence, differing only in presentation", c.a, c.b)
			}
		})
	}
}

// TestFoldOnlyNeverDropsTheIncomingMemory is the property the whole design
// turns on: when the texts are not equal, the incoming memory is stored as its
// own row. Asserted against a real store, because the failure this prevents is
// "reported as promoted, stored nowhere".
func TestFoldOnlyNeverDropsTheIncomingMemory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("ensure _global: %v", err)
	}
	const original = "the API token is required for every client that talks to the staging gateway"
	if _, err := s.Create(ctx, "_global", Memory{
		Category: "fact", Content: original, Source: "reflection", Importance: 0.5,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// One word different. Similar enough to any sane threshold, and the exact
	// opposite claim.
	const inverted = "the API token is optional for every client that talks to the staging gateway"
	if _, _, _, err := s.UpsertWithOptions(ctx, "_global", "fact", inverted, "reflection", 0.6, nil,
		UpsertOptions{FoldOnly: true}); err != nil {
		t.Fatalf("FoldOnly upsert: %v", err)
	}
	rows, err := s.GetAll(ctx, "_global", 10)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	var found bool
	for _, m := range rows {
		if m.Content == inverted {
			found = true
		}
	}
	if !found {
		t.Errorf("the inverted instruction was folded away and stored nowhere: %+v", rows)
	}
}
