package memory

import (
	"context"
	"strings"
	"testing"
)

// TestEveryTagBearingWriterIsGuarded covers the last of the write paths that
// take a tag list.
//
// A memory's tags are not inert. assemble.Item.Line marshals the tag list into
// the row ghost_memory_search returns, and BuildReflectionPrompt writes
// `, tags:[…]` into the prompt sent to a CLI harness talking to a third-party
// model — so a token pasted as a tag is embedded, returned and quoted exactly as
// one in the body would be. validateTags permits ten tags of 64 characters,
// which is more than room for every provider token format.
//
// Create and UpsertWithOptions were guarded first. These are the other two, and
// they are guarded for the same reason rather than because a reviewer counted:
// a decision's tags reach BOTH the decisions row and a companion memory row,
// and that companion is an ordinary memory — returned by search like any other.
func TestEveryTagBearingWriterIsGuarded(t *testing.T) {
	// Assembled rather than written out: GitHub push protection matches the PAT
	// format anywhere in a diff and rejects the push (GH013) before review.
	credential := "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd"

	t.Run("RecordDecision", func(t *testing.T) {
		s := testStore(t)
		_, _, _, err := s.RecordDecision(context.Background(), testProject,
			"rotate the pool keys", "rotate them quarterly", "quarterly is enough",
			nil, []string{credential})
		if err == nil {
			t.Fatal("RecordDecision accepted a credential as a tag — it is written to " +
				"the decisions row AND to a companion memory row that search returns " +
				"and the next reflect prompt quotes")
		}
		if !strings.Contains(err.Error(), "tags[0]") {
			t.Errorf("the refusal does not name the tag: %v", err)
		}
		if strings.Contains(err.Error(), credential) {
			t.Errorf("the refusal printed the value: %v", err)
		}
	})

	t.Run("ImportMemory", func(t *testing.T) {
		s := testStore(t)
		if err := s.EnsureProject(context.Background(), testProject, "/tmp/"+testProject, testProject); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		_, _, _, err := s.ImportMemory(context.Background(), PortableMemory{
			ID: "tagrefusalmemory0000000AA1", ProjectID: testProject,
			Category: "fact", Content: "an ordinary imported fact",
			Tags: []string{credential}, Source: "onboarding",
		}, ImportOptions{Apply: true})
		if err == nil {
			t.Fatal("ImportMemory accepted a credential as a tag — the artifact's " +
				"tags column is written raw, and the record is an ordinary memory")
		}
		if !strings.Contains(err.Error(), "tags[0]") {
			t.Errorf("the refusal does not name the tag: %v", err)
		}
		if strings.Contains(err.Error(), credential) {
			t.Errorf("the refusal printed the value: %v", err)
		}
	})

	t.Run("UpdateMemory", func(t *testing.T) {
		s := testStore(t)
		id, err := s.Create(context.Background(), testProject, Memory{
			Category: "fact", Content: "an ordinary fact", Source: "mcp", Importance: 0.7,
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		err = s.UpdateMemory(context.Background(), testProject, id, nil, nil, nil,
			[]string{credential})
		if err == nil {
			t.Fatal("UpdateMemory accepted a credential as a tag — this is the store " +
				"method behind ghost_memory_update, and validateTags passes the " +
				"tool's tags straight through")
		}
		if !strings.Contains(err.Error(), "tags[0]") {
			t.Errorf("the refusal does not name the tag: %v", err)
		}
	})

	t.Run("ordinary tags still save on every path", func(t *testing.T) {
		s := testStore(t)
		tags := []string{"production", "db", "cardano", "relay-1"}
		if _, err := s.Create(context.Background(), testProject, Memory{
			Category: "fact", Content: "an ordinary fact", Source: "mcp",
			Importance: 0.7, Tags: tags,
		}); err != nil {
			t.Fatalf("Create refused ordinary tags: %v", err)
		}
		if _, _, _, err := s.RecordDecision(context.Background(), testProject,
			"rotate the pool keys", "rotate them quarterly", "quarterly is enough",
			nil, tags); err != nil {
			t.Fatalf("RecordDecision refused ordinary tags: %v", err)
		}
	})
}
