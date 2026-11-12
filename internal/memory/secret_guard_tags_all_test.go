package memory

import (
	"context"
	"strings"
	"testing"
)

// TestEveryTagBearingWriterIsGuarded covers tag-bearing writers (#656, #835).
func TestEveryTagBearingWriterIsGuarded(t *testing.T) {
	credential := "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd"

	t.Run("RecordDecision", func(t *testing.T) {
		s := testStore(t)
		_, _, _, err := s.RecordDecision(context.Background(), testProject, "rotate the pool keys", "rotate them quarterly", "quarterly is enough", nil, []string{credential})
		if err == nil || !strings.Contains(err.Error(), "tags[0]") || strings.Contains(err.Error(), credential) {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("ImportMemory", func(t *testing.T) {
		s := testStore(t)
		if err := s.EnsureProject(context.Background(), testProject, "/tmp/"+testProject, testProject); err != nil {
			t.Fatal(err)
		}
		_, _, _, err := s.ImportMemory(context.Background(), PortableMemory{ID: "tagrefusalmemory0000000AA1", ProjectID: testProject, Category: "fact", Content: "an ordinary imported fact", Tags: []string{credential}, Source: "onboarding"}, ImportOptions{Apply: true})
		if err == nil || !strings.Contains(err.Error(), "tags[0]") || strings.Contains(err.Error(), credential) {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("UpdateMemory", func(t *testing.T) {
		s := testStore(t)
		id, err := s.Create(context.Background(), testProject, Memory{Category: "fact", Content: "an ordinary fact", Source: "mcp", Importance: 0.7})
		if err != nil {
			t.Fatal(err)
		}
		err = s.UpdateMemory(context.Background(), testProject, id, nil, nil, nil, []string{credential})
		if err == nil || !strings.Contains(err.Error(), "tags[0]") {
			t.Fatalf("err=%v", err)
		}
	})

	// #835
	t.Run("ImportDecision", func(t *testing.T) {
		s := testStore(t)
		if err := s.EnsureProject(context.Background(), testProject, "/tmp/"+testProject, testProject); err != nil {
			t.Fatal(err)
		}
		_, _, err := s.ImportDecision(context.Background(), Decision{ID: "tagrefusaldecision0000AA1", ProjectID: testProject, Title: "rotate the pool keys", Decision: "rotate them quarterly", Rationale: "quarterly is enough", Status: "active", Tags: []string{"ops", credential}}, true)
		if err == nil || !strings.Contains(err.Error(), "tags[1]") || strings.Contains(err.Error(), credential) {
			t.Fatalf("err=%v", err)
		}
		rows, err := s.ListDecisions(context.Background(), testProject, "", 100)
		if err != nil || len(rows) != 0 {
			t.Fatalf("rows=%d err=%v", len(rows), err)
		}
	})

	t.Run("ordinary tags still save on every path", func(t *testing.T) {
		s := testStore(t)
		tags := []string{"production", "db", "cardano", "relay-1"}
		if _, err := s.Create(context.Background(), testProject, Memory{Category: "fact", Content: "an ordinary fact", Source: "mcp", Importance: 0.7, Tags: tags}); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := s.RecordDecision(context.Background(), testProject, "rotate the pool keys", "rotate them quarterly", "quarterly is enough", nil, tags); err != nil {
			t.Fatal(err)
		}
		if err := s.EnsureProject(context.Background(), testProject, "/tmp/"+testProject, testProject); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.ImportDecision(context.Background(), Decision{ID: "tagrefusaldecision0000AA1", ProjectID: testProject, Title: "rotate the pool keys", Decision: "rotate them quarterly", Rationale: "quarterly is enough", Status: "active", Tags: tags}, true); err != nil {
			t.Fatal(err)
		}
	})
}
