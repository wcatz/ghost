package memory

import (
	"context"
	"strings"
	"testing"
)

func TestTagsAreGuardedBecauseTheyTravel(t *testing.T) {
	credential := "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd"

	t.Run("upsert", func(t *testing.T) {
		s := testStore(t)
		_, _, _, err := s.UpsertWithOptions(context.Background(), testProject, "fact", "an ordinary fact", "mcp", 0.7, []string{"production", credential}, UpsertOptions{})
		if err == nil {
			t.Fatal("accepted credential tag")
		}
		if !strings.Contains(err.Error(), "tags[1]") || strings.Contains(err.Error(), credential) {
			t.Errorf("bad refusal: %v", err)
		}
		if n := projectMemoryCount(t, s); n != 0 {
			t.Errorf("stored %d memories", n)
		}
	})

	t.Run("create", func(t *testing.T) {
		s := testStore(t)
		_, err := s.Create(context.Background(), testProject, Memory{Category: "fact", Content: "an ordinary fact", Source: "mcp", Importance: 0.7, Tags: []string{credential}})
		if err == nil || !strings.Contains(err.Error(), "tags[0]") {
			t.Fatalf("bad refusal: %v", err)
		}
	})

	t.Run("ordinary", func(t *testing.T) {
		s := testStore(t)
		if _, err := s.Create(context.Background(), testProject, Memory{Category: "fact", Content: "an ordinary fact", Source: "mcp", Importance: 0.7, Tags: []string{"production", "db"}}); err != nil {
			t.Fatal(err)
		}
		if n := projectMemoryCount(t, s); n != 1 {
			t.Errorf("got %d memories", n)
		}
	})
}
