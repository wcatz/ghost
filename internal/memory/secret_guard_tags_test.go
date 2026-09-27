package memory

import (
	"context"
	"strings"
	"testing"
)

// TestTagsAreGuardedBecauseTheyTravel pins a claim I had backwards.
//
// I recorded in secret_guard.go that a memory's tags were not worth guarding,
// on the grounds that they are "not returned by search, not embedded, and are a
// handful of short words chosen by the caller rather than pasted from a log". The
// first two halves are false in this checkout: assemble.Item.Line marshals the
// tag list into the row ghost_memory_search returns, and BuildReflectionPrompt
// writes `, tags:[…]` into the prompt sent to a CLI harness talking to a
// third-party model. So a token pasted as a tag is embedded, returned and quoted
// into a model exactly as one in the body would be — and validateTags permits ten
// tags of 64 characters, which is room for every provider token format.
//
// The test asserts the three paths separately, because they fail separately.
func TestTagsAreGuardedBecauseTheyTravel(t *testing.T) {
	// Assembled rather than written out: GitHub push protection matches the PAT
	// format anywhere in a diff and rejects the push (GH013) before review.
	credential := "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd"

	t.Run("upsert", func(t *testing.T) {
		s := testStore(t)
		_, _, _, err := s.UpsertWithOptions(context.Background(), testProject, "fact",
			"an ordinary fact", "mcp", 0.7, []string{"production", credential}, UpsertOptions{})
		if err == nil {
			t.Fatal("UpsertWithOptions accepted a credential as a tag — the tag is " +
				"returned by search and quoted into the reflect prompt")
		}
		if !strings.Contains(err.Error(), "tags[1]") {
			t.Errorf("the refusal does not name the tag it refused: %v", err)
		}
		if strings.Contains(err.Error(), credential) {
			t.Errorf("the refusal printed the value: %v", err)
		}
		if n := projectMemoryCount(t, s); n != 0 {
			t.Errorf("a refused upsert stored %d memories, want 0", n)
		}
	})

	t.Run("create", func(t *testing.T) {
		s := testStore(t)
		_, err := s.Create(context.Background(), testProject, Memory{
			Category: "fact", Content: "an ordinary fact", Source: "mcp", Importance: 0.7,
			Tags: []string{credential},
		})
		if err == nil {
			t.Fatal("Create accepted a credential as a tag")
		}
		if !strings.Contains(err.Error(), "tags[0]") {
			t.Errorf("the refusal does not name the tag it refused: %v", err)
		}
	})

	t.Run("ordinary tags still save", func(t *testing.T) {
		s := testStore(t)
		tags := []string{"production", "db", "infrastructure", "cardano", "relay-1"}
		if _, err := s.Create(context.Background(), testProject, Memory{
			Category: "fact", Content: "an ordinary fact", Source: "mcp", Importance: 0.7,
			Tags: tags,
		}); err != nil {
			t.Fatalf("Create refused ordinary tags: %v", err)
		}
		if n := projectMemoryCount(t, s); n != 1 {
			t.Errorf("store holds %d memories, want 1", n)
		}
	})
}
