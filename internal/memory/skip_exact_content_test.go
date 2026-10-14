package memory

import (
	"context"
	"testing"
)

var skipExact = UpsertOptions{SkipExactContent: true}

// TestSkipExactContentWritesOnce covers the save the Claude auto-memory import
// makes: the first call inserts, and a second call carrying the same content
// writes nothing and reports the skip as an empty id naming the row it found.
// A plain Upsert would insert a linked copy here, which is the doubling the
// import must not have.
func TestSkipExactContentWritesOnce(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const content = "the build host is not the laptop; use the ci runner"
	first, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", content, "onboarding", 0.5, []string{"claude-code"}, skipExact)
	if err != nil {
		t.Fatalf("first save: %v", err)
	}
	if first == "" {
		t.Fatal("first save reported a skip, want a new row")
	}

	again, dupOf, _, err := s.UpsertWithOptions(ctx, testProject, "fact", content, "onboarding", 0.5, []string{"claude-code"}, skipExact)
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if again != "" {
		t.Errorf("second save wrote row %s for content the project already held", again)
	}
	if dupOf != first {
		t.Errorf("skip named %q as the present row, want %q", dupOf, first)
	}
	if n := projectMemoryCount(t, s); n != 1 {
		t.Errorf("project holds %d rows, want 1", n)
	}
	links, err := s.GetLinks(ctx, first)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	if len(links) != 0 {
		t.Errorf("a skipped save left %d links on the present row, want none", len(links))
	}
}

// TestSkipExactContentStillFoldsANearDuplicate pins that the option narrows
// nothing but the byte-identical case: a near-duplicate still takes Upsert's
// fold, inserted as its own row and linked to the row it folded into.
func TestSkipExactContentStillFoldsANearDuplicate(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	stored, _, _, err := s.Upsert(ctx, testProject, "fact", "the release branch is cut from main on the deploy day", "onboarding", 0.5, nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	id, dupOf, _, err := s.UpsertWithOptions(ctx, testProject, "fact", "the release branch is cut from main on every deploy day", "onboarding", 0.5, nil, skipExact)
	if err != nil {
		t.Fatalf("near-duplicate save: %v", err)
	}
	if id == "" {
		t.Fatal("a near-duplicate was skipped, want it stored and folded")
	}
	if dupOf != stored {
		t.Errorf("near-duplicate folded into %q, want %q", dupOf, stored)
	}
}

// TestSkipExactContentIsScopedToTheProject pins the lookup: another project
// holding the same text does not make this project's save a skip. The
// import's guard has always been per-project, and a store-wide lookup would
// silently drop a file from the second checkout that shares its wording.
func TestSkipExactContentIsScopedToTheProject(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const other = "other-project"
	if err := s.EnsureProject(ctx, other, "/srv/other", other); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	const content = "the release branch is cut from main on the deploy day"

	if id, _, _, err := s.UpsertWithOptions(ctx, other, "fact", content, "onboarding", 0.5, nil, skipExact); err != nil || id == "" {
		t.Fatalf("save into the other project: id=%q err=%v", id, err)
	}
	id, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", content, "onboarding", 0.5, nil, skipExact)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if id == "" {
		t.Error("a row in another project made this project's save skip")
	}
}

// TestSkipExactContentKeepsTheTokenGuard is the same refusal every Upsert
// makes, before the transaction and storing nothing: a Claude auto-memory file
// is text that arrived from somewhere.
func TestSkipExactContentKeepsTheTokenGuard(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	_, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"an imported note holding "+secretFixture, "onboarding", 0.5, nil, skipExact)
	assertRefusal(t, err, "content")

	if n := projectMemoryCount(t, s); n != 0 {
		t.Errorf("a refused save stored %d memories, want 0", n)
	}
}
