package memory

import (
	"context"
	"strings"
	"testing"
)

func TestSeedGlobalMemoriesUsesNonUserSource(t *testing.T) {
	s := testStore(t)
	if err := s.SeedGlobalMemories(context.Background()); err != nil {
		t.Fatalf("SeedGlobalMemories: %v", err)
	}
	global, err := s.GetAll(context.Background(), "_global", -1)
	if err != nil {
		t.Fatalf("GetAll global: %v", err)
	}
	var seed *Memory
	for i := range global {
		if strings.Contains(global[i].Content, "Co-Authored-By") {
			seed = &global[i]
			break
		}
	}
	if seed == nil {
		t.Fatal("built-in seed memory not found")
	}
	if seed.Source != "builtin" {
		t.Errorf("built-in seed source = %q, want builtin", seed.Source)
	}
	if own, label := OriginClass(seed.Source); own || label != "builtin" {
		t.Errorf("OriginClass(%q) = (%v, %q), want (false, builtin)", seed.Source, own, label)
	}
}
