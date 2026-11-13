package memory

import (
	"context"
	"testing"
)

var skipExact = UpsertOptions{SkipExactContent: true}

func TestSkipExactContentWritesOnce(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const content = "the build host is not the laptop; use the ci runner"
	first, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", content, "onboarding", 0.5, []string{"claude-code"}, skipExact)
	if err != nil || first == "" {
		t.Fatalf("first: id=%q err=%v", first, err)
	}
	again, dupOf, _, err := s.UpsertWithOptions(ctx, testProject, "fact", content, "onboarding", 0.5, []string{"claude-code"}, skipExact)
	if err != nil || again != "" || dupOf != first {
		t.Fatalf("again=%q dup=%q err=%v", again, dupOf, err)
	}
	if n := projectMemoryCount(t, s); n != 1 {
		t.Fatalf("count=%d", n)
	}
	if links, err := s.GetLinks(ctx, first); err != nil || len(links) != 0 {
		t.Fatalf("links=%v err=%v", links, err)
	}
}

func TestSkipExactContentStillFoldsANearDuplicate(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	stored, _, _, err := s.Upsert(ctx, testProject, "fact", "the release branch is cut from main on the deploy day", "onboarding", 0.5, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, dupOf, _, err := s.UpsertWithOptions(ctx, testProject, "fact", "the release branch is cut from main on every deploy day", "onboarding", 0.5, nil, skipExact)
	if err != nil || id == "" || dupOf != stored {
		t.Fatalf("id=%q dup=%q err=%v", id, dupOf, err)
	}
}

func TestSkipExactContentIsScopedToTheProject(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const other = "other-project"
	if err := s.EnsureProject(ctx, other, "/srv/other", other); err != nil {
		t.Fatal(err)
	}
	const content = "the release branch is cut from main on the deploy day"
	if id, _, _, err := s.UpsertWithOptions(ctx, other, "fact", content, "onboarding", 0.5, nil, skipExact); err != nil || id == "" {
		t.Fatalf("other: id=%q err=%v", id, err)
	}
	if id, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", content, "onboarding", 0.5, nil, skipExact); err != nil || id == "" {
		t.Fatalf("proj: id=%q err=%v", id, err)
	}
}

func TestSkipExactContentKeepsTheTokenGuard(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", "an imported note holding "+secretFixture, "onboarding", 0.5, nil, skipExact)
	assertRefusal(t, err, "content")
	if n := projectMemoryCount(t, s); n != 0 {
		t.Fatalf("count=%d", n)
	}
}
