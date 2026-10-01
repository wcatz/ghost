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
// Create and UpsertWithOptions were guarded first (secret_guard_tags_test.go).
// The four subtests below are the rest of the set, and they are guarded for the
// same reason rather than because a reviewer counted: a decision's tags reach
// BOTH the decisions row and a companion memory row, and that companion is an
// ordinary memory — returned by search like any other.
//
// The four are named here rather than totalled, because a total is the thing that
// drifted: the sentence this replaced said "all five" across the package while
// this table held three of the six, and the member it missed was the one that
// mattered. They are RecordDecision, ImportMemory, UpdateMemory and
// ImportDecision.
//
// The ImportDecision case is #835, and it was the one member of the set this table
// never had: `RecordDecision` has refused a credential-shaped tag since #656, while
// `CheckImportedDecision` judged a decision's id, project, required fields, status,
// text and alternatives and never its tags — so the artifact's tags column, which
// arrives from a FILE, was written raw into the same table the write path guards.
// One predicate, one set: a decision's tags are guarded on the write path and on the
// import path or on neither, and the difference between the two is which route the
// credential arrived on rather than whether it is one.
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

	// #835: the one member of the set that was missing. `CheckImportedDecision` is
	// the exporter's own predicate too (portable.Export calls it), so a decision
	// whose tags column carries a token was BOTH written raw from an artifact and
	// re-emitted into the next one.
	//
	// The assertions are ImportMemory's, deliberately: the two must not drift, and
	// a drift here is a decision that is guarded on one route and not the other.
	// What makes it a decision is the field name and nothing else — the sentence
	// names the field, and the value is in no field of the error.
	t.Run("ImportDecision", func(t *testing.T) {
		s := testStore(t)
		if err := s.EnsureProject(context.Background(), testProject, "/tmp/"+testProject, testProject); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		d := Decision{
			ID: "tagrefusaldecision0000AA1", ProjectID: testProject,
			Title: "rotate the pool keys", Decision: "rotate them quarterly",
			Rationale: "quarterly is enough", Status: "active",
			Tags: []string{"ops", credential},
		}
		_, err := s.ImportDecision(context.Background(), d, true)
		if err == nil {
			t.Fatal("ImportDecision accepted a credential as a tag — the artifact's tags " +
				"column is written raw, and the decision row is re-emitted by every export")
		}
		if !strings.Contains(err.Error(), "tags[1]") {
			t.Errorf("the refusal does not name the tag it refused: %v", err)
		}
		if strings.Contains(err.Error(), credential) {
			t.Errorf("the refusal printed the value: %v", err)
		}
		// Nothing was written, so a re-run of the same artifact is still a fresh
		// write rather than a skip over a row holding the token.
		rows, err := s.ListDecisions(context.Background(), testProject, "", 100)
		if err != nil {
			t.Fatalf("ListDecisions: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("a refused import stored %d decision(s), want 0", len(rows))
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
		if err := s.EnsureProject(context.Background(), testProject, "/tmp/"+testProject, testProject); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		if _, err := s.ImportDecision(context.Background(), Decision{
			ID: "tagrefusaldecision0000AA1", ProjectID: testProject,
			Title: "rotate the pool keys", Decision: "rotate them quarterly",
			Rationale: "quarterly is enough", Status: "active", Tags: tags,
		}, true); err != nil {
			t.Fatalf("ImportDecision refused ordinary tags: %v", err)
		}
	})
}
