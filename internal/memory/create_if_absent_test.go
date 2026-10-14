package memory

import (
	"context"
	"testing"
)

// TestCreateIfAbsentWritesOnce covers the write the Claude auto-memory import
// makes: the first call inserts, and a second call carrying the same content
// writes nothing and says so. The store's ordinary Create would insert a second
// row here, which is the doubling the import must not have.
func TestCreateIfAbsentWritesOnce(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const content = "the build host is not the laptop; use the ci runner"
	created, err := s.CreateIfAbsent(ctx, testProject, "fact", content, "onboarding", 0.5, []string{"claude-code"})
	if err != nil {
		t.Fatalf("CreateIfAbsent: %v", err)
	}
	if !created {
		t.Fatal("first CreateIfAbsent reported no write, want one")
	}

	again, err := s.CreateIfAbsent(ctx, testProject, "fact", content, "onboarding", 0.5, []string{"claude-code"})
	if err != nil {
		t.Fatalf("second CreateIfAbsent: %v", err)
	}
	if again {
		t.Error("second CreateIfAbsent wrote a row for content the project already held")
	}

	rows, err := s.GetAll(ctx, testProject, 1000)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	var matches int
	for _, m := range rows {
		if m.Content == content {
			matches++
		}
	}
	if matches != 1 {
		t.Errorf("project holds %d rows with the imported content, want 1", matches)
	}
}

// TestCreateIfAbsentScopesTheProbeToTheProject pins the predicate: another
// project holding the same text does not make this project's import a skip.
// The import's guard has always been per-project, and a global probe would
// silently drop a file from the second checkout that shares its wording.
func TestCreateIfAbsentScopesTheProbeToTheProject(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const other = "other-project"
	if err := s.EnsureProject(ctx, other, "/srv/other", other); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	const content = "the release branch is cut from main on the deploy day"

	if created, err := s.CreateIfAbsent(ctx, other, "fact", content, "onboarding", 0.5, nil); err != nil || !created {
		t.Fatalf("CreateIfAbsent into the other project: created=%v err=%v", created, err)
	}
	created, err := s.CreateIfAbsent(ctx, testProject, "fact", content, "onboarding", 0.5, nil)
	if err != nil {
		t.Fatalf("CreateIfAbsent: %v", err)
	}
	if !created {
		t.Error("a row in another project made this project's import skip its own file")
	}
}

// TestCreateIfAbsentKeepsTheCredentialGuard is the same refusal Create makes.
// The import is a path where the content is a file that arrived from somewhere,
// so a token in one must be refused here exactly as it is on any other write —
// and refused before the transaction, storing nothing.
func TestCreateIfAbsentKeepsTheCredentialGuard(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	_, err := s.CreateIfAbsent(ctx, testProject, "fact",
		"an imported note holding "+secretFixture, "onboarding", 0.5, nil)
	assertRefusal(t, err, "content")

	if n := projectMemoryCount(t, s); n != 0 {
		t.Errorf("a refused conditional save stored %d memories, want 0", n)
	}
}
